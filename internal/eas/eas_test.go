package eas

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
	"github.com/linchange/linchange/internal/webmail"
)

// The command tests use a fixed authenticator that accepts one principal,
// which is enough for the questions they ask -- what a command answers and
// what state it advances -- while the authentication contract is tested
// separately, against an authenticator that refuses.
const testOwner = "alice@example.test"

// fixedAuthenticator accepts any credentials and answers with one principal.
type fixedAuthenticator struct{ who policy.Principal }

func (f fixedAuthenticator) Authenticate(ctx context.Context, login, pw string, action policy.Action) (policy.Principal, error) {
	if f.who.Address == "" {
		return policy.Principal{}, fmt.Errorf("no such account")
	}
	return f.who, nil
}

// refusingAuthenticator rejects every login, as a verifier would for a wrong
// password.
type refusingAuthenticator struct{}

func (refusingAuthenticator) Authenticate(ctx context.Context, login, pw string, action policy.Action) (policy.Principal, error) {
	return policy.Principal{}, fmt.Errorf("rejected")
}

func principal() policy.Principal {
	return policy.Principal{Address: testOwner, MailboxEnabled: true}
}

// newTransportServer builds a service without a database, for the tests that
// ask about the gates and the dispatch, which never touch one.
func newTransportServer() *EAS {
	return New(Options{
		Config: config.New(),
		Policy: policy.Policy{},
		Auth:   fixedAuthenticator{who: principal()},
	})
}

// testService is a full service against its own migrated database and blob
// store, with the account provisioned.
type testService struct {
	eas  *EAS
	db   *sqlx.DB
	blob *blobstore.Store
	mail *webmail.Service
}

func newTestService(t *testing.T) *testService {
	t.Helper()
	db := testdb.Migrated(t)

	blob, err := blobstore.Open(blobstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("opening the blob store: %v", err)
	}
	mailService, err := webmail.New(webmail.Options{
		DB:       db,
		Blobs:    blob,
		Hostname: "linchange.test",
	})
	if err != nil {
		t.Fatalf("building the mail service: %v", err)
	}

	e := New(Options{
		DB:       db,
		Mail:     mailService,
		Messages: imapd.NewMessageRepo(),
		Auth:     fixedAuthenticator{who: principal()},
		Policy:   policy.Policy{},
	})

	ctx := context.Background()
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: testOwner,
		Source:  "local",
	}); err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	if err := imapd.NewMailboxRepo().EnsureFolders(ctx, db, testOwner); err != nil {
		t.Fatalf("provisioning the folders: %v", err)
	}

	return &testService{eas: e, db: db, blob: blob, mail: mailService}
}

// folderID looks up one of the owner's folders by name.
func (ts *testService) folderID(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	if err := ts.db.GetContext(context.Background(), &id,
		`SELECT id FROM mailboxes WHERE owner = $1 AND name = $2`, testOwner, name); err != nil {
		t.Fatalf("looking up %s: %v", name, err)
	}
	return id
}

// deliver stores one real message and files it in the given folder, so a
// command that reads it back finds a body in the blob store.
func (ts *testService) deliver(t *testing.T, mailboxID int64, subject, body, messageID string, flags []string) {
	t.Helper()

	raw := fmt.Sprintf("From: Alice <alice@example.com>\r\n"+
		"To: bob@example.test\r\n"+
		"Subject: %s\r\n"+
		"Message-ID: <%s>\r\n"+
		"Date: Mon, 02 Jan 2006 15:04:05 +0000\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\n"+
		"%s\r\n", subject, messageID, body)

	meta, err := mail.Describe([]byte(raw))
	if err != nil {
		t.Fatalf("describing %q: %v", subject, err)
	}
	digest, size, err := ts.blob.Put([]byte(raw))
	if err != nil {
		t.Fatalf("storing %q: %v", subject, err)
	}
	if _, err := imapd.NewMessageRepo().Insert(context.Background(), ts.db, mailboxID,
		digest[:], size, meta.Date, flags, meta); err != nil {
		t.Fatalf("filing %q: %v", subject, err)
	}
}

// post sends one command, with credentials the fixed authenticator accepts and
// the version header a modern client speaks: 14.1, where a failure carries a
// Status element in a 200 rather than an HTTP error.
func (ts *testService) post(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/eas/Microsoft-Server-ActiveSync",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/vnd.ms-sync.wbxml")
	req.Header.Set("MS-ASProtocolVersion", "14.1")
	req.SetBasicAuth(testOwner, "correct-horse")
	rr := httptest.NewRecorder()
	ts.eas.Handler().ServeHTTP(rr, req)
	return rr
}

// root decodes the WBXML body of a successful answer.
func decodeRoot(t *testing.T, rr *httptest.ResponseRecorder) *WBXMLNode {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %q", rr.Code, rr.Body.String())
	}
	dec := NewWBXMLDecoder(rr.Body.Bytes())
	root, err := dec.Decode()
	if err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	return root
}

// folderSyncRequest is a FolderSync, with the key of the previous round.
func folderSyncRequest(syncKey string) []byte {
	w := NewWBXMLWriter()
	_ = w.Start(nsFolderHierarchy, "FolderSync")
	_ = w.Leaf(nsFolderHierarchy, "SyncKey", syncKey)
	w.End()
	return w.Bytes()
}

// syncRequest is a Sync of one folder, with the key of the previous round.
func syncRequest(folderID, syncKey string) []byte {
	w := NewWBXMLWriter()
	_ = w.Start(nsAirSync, "Sync")
	_ = w.Leaf(nsAirSync, "ClientId", "test-client")
	_ = w.Start(nsAirSync, "Collections")
	_ = w.Start(nsAirSync, "Collection")
	_ = w.Leaf(nsAirSync, "CollectionId", folderID)
	_ = w.Leaf(nsAirSync, "SyncKey", syncKey)
	w.End()
	w.End()
	w.End()
	return w.Bytes()
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// A body that is not announced as WBXML is not a command and is answered
// before it is read, which is also before any credential is asked for.
func TestNonWBXMLContentTypeIsRejected(t *testing.T) {
	e := newTransportServer()
	req := httptest.NewRequest(http.MethodPost, "/eas", strings.NewReader(`<Sync/>`))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(testOwner, "correct-horse")
	rr := httptest.NewRecorder()
	e.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// Without credentials the answer is a challenge, so a client asks for a
// password instead of treating the account as absent.
func TestMissingCredentialsAreChallenged(t *testing.T) {
	e := newTransportServer()
	req := httptest.NewRequest(http.MethodPost, "/eas", bytes.NewReader(folderSyncRequest("")))
	req.Header.Set("Content-Type", "application/vnd.ms-sync.wbxml")
	rr := httptest.NewRecorder()
	e.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
	}
}

// A refused login is the same challenge: the answer must not say whether the
// address exists, because that is how an account list is built.
func TestRefusedCredentialsAreChallenged(t *testing.T) {
	e := New(Options{
		Config: config.New(),
		Policy: policy.Policy{},
		Auth:   refusingAuthenticator{},
	})
	req := httptest.NewRequest(http.MethodPost, "/eas", bytes.NewReader(folderSyncRequest("")))
	req.Header.Set("Content-Type", "application/vnd.ms-sync.wbxml")
	req.SetBasicAuth(testOwner, "wrong-password")
	rr := httptest.NewRecorder()
	e.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", rr.Header().Get("WWW-Authenticate"))
	}
}

// The OPTIONS request negotiates the protocol before any command is sent, and
// the answer names the versions and the commands the server speaks.
func TestOptionsNegotiatesTheProtocol(t *testing.T) {
	e := newTransportServer()
	req := httptest.NewRequest(http.MethodOptions, "/eas", nil)
	req.Header.Set("MS-ASProtocolVersion", "14.0")
	rr := httptest.NewRecorder()
	e.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	versions := rr.Header().Get("MS-ASProtocolVersions")
	if !strings.Contains(versions, "12.1") {
		t.Errorf("MS-ASProtocolVersions = %q, want it to include 12.1", versions)
	}
	// A request above everything the server has is answered with the newest
	// the server has, not the requested version.
	if got := rr.Header().Get("MS-ASProtocolVersion"); got != "12.1" {
		t.Errorf("MS-ASProtocolVersion = %q, want 12.1 for a request of 14.0", got)
	}
	commands := rr.Header().Get("MS-ASProtocolCommands")
	for _, want := range []string{"Sync", "SendMail", "FolderSync", "Provision"} {
		if !strings.Contains(commands, want) {
			t.Errorf("MS-ASProtocolCommands = %q, want it to include %s", commands, want)
		}
	}
}

// A GET of the mount point is answered, because a client that probes before it
// commits reads a 404 as "there is no ActiveSync here".
func TestServiceRootAnswersAGet(t *testing.T) {
	e := newTransportServer()
	req := httptest.NewRequest(http.MethodGet, "/eas", nil)
	rr := httptest.NewRecorder()
	e.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("MS-Server-ActiveSync"); got == "" {
		t.Error("the service root should carry the MS-Server-ActiveSync header")
	}
}

// A stream that decodes to an element that is not a command is refused, and a
// stream that is not WBXML at all is refused before it is read.
func TestUnreadableStreamsAreRejected(t *testing.T) {
	e := newTransportServer()

	// Valid WBXML, naming an element this service does not implement.
	unknown := NewWBXMLWriter()
	_ = unknown.Start(nsAirSync, "Responses")
	unknown.End()
	rr := tsPost(e, unknown.Bytes())
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("unknown command = %d, want 501", rr.Code)
	}

	// Not WBXML at all.
	rr = tsPost(e, []byte{0x01, 0x02, 0x03})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad header = %d, want 400", rr.Code)
	}

	// A WBXML header with no command behind it.
	rr = tsPost(e, []byte{0x03, 0x00, 0x6A, 0x00, 0x00})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty document = %d, want 400", rr.Code)
	}
}

// tsPost posts to a service built outside testService.
func tsPost(e *EAS, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/eas", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/vnd.ms-sync.wbxml")
	req.SetBasicAuth(testOwner, "correct-horse")
	rr := httptest.NewRecorder()
	e.Handler().ServeHTTP(rr, req)
	return rr
}

// The version a client asks for is answered with the newest version the server
// has that is at or under it, the oldest when it asks for less than the server
// has, and the newest when it has not asked at all.
func TestSelectProtocolVersion(t *testing.T) {
	cases := map[string]string{
		"":     "12.1",
		"14.1": "12.1",
		"14.0": "12.1",
		"12.1": "12.1",
		"12.0": "12.0",
		"2.5":  "2.5",
		"2.0":  "2.0",
		"1.5":  "2.0",
	}
	for in, want := range cases {
		if got := selectProtocolVersion(in); got != want {
			t.Errorf("selectProtocolVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Codec
// ---------------------------------------------------------------------------

// The writer and the decoder are the contract with a real client, so a
// document the writer produces must decode back to the same elements, in the
// same code pages, with the same text and the same opaque payloads. The header
// and the content bits are checked here too: they are what a real client reads
// first, and a document that gets them wrong is a document no client can read
// however well the rest of it is formed.
func TestWBXMLRoundTrip(t *testing.T) {
	w := NewWBXMLWriter()
	_ = w.Start(nsAirSync, "Sync")
	_ = w.Leaf(nsAirSync, "CollectionId", "42")
	w.Start(nsAirSync, "ApplicationData")
	_ = w.Leaf(nsEmail, "Subject", "héllo wörld")
	w.Start(nsAirSyncBase, "Body")
	_ = w.Leaf(nsAirSyncBase, "Type", "1")
	w.Opaque([]byte{0x00, 0x01, 0xFF, 0x7F})
	w.End()
	w.End()
	w.End()

	raw := w.Bytes()
	// The header ActiveSync clients write and read: WBXML 1.3, the public
	// identifier 0x01, UTF-8 (MIBEnum 106) and no string table.
	if want := []byte{0x03, 0x01, 0x6A, 0x00}; !bytes.Equal(raw[:4], want) {
		t.Fatalf("header = % x, want % x", raw[:4], want)
	}
	// Every element with content carries bit 6 of its token and an END token
	// after it; the root element Sync has content, so its token is 0x45.
	if raw[4] != byte(0x05|tokenHasContent) {
		t.Errorf("the root element token = 0x%02x, want 0x45 (content bit set)", raw[4])
	}

	dec := NewWBXMLDecoder(raw)
	root, err := dec.Decode()
	if err != nil {
		t.Fatalf("decoding the stream: %v", err)
	}

	if root.Name != "Sync" || root.NS != "AirSync" {
		t.Fatalf("root = %s:%s, want AirSync:Sync", root.NS, root.Name)
	}
	if got := root.TextOf("CollectionId"); got != "42" {
		t.Errorf("CollectionId = %q, want 42", got)
	}
	data := root.FindChild("ApplicationData")
	if data == nil || data.NS != "AirSync" {
		t.Fatalf("the ApplicationData child is missing or in the wrong code page: %+v", data)
	}
	if got := data.TextOf("Subject"); got != "héllo wörld" {
		t.Errorf("Subject = %q, want %q", got, "héllo wörld")
	}
	body := data.FindChild("Body")
	if body == nil || body.NS != "AirSyncBase" {
		t.Fatalf("the Body child is missing or in the wrong code page: %+v", body)
	}
	if !bytes.Equal(body.Opaque, []byte{0x00, 0x01, 0xFF, 0x7F}) {
		t.Errorf("the opaque body did not survive the round trip: %+v", body)
	}
}

// A document written the way a real client writes it -- content bits set, an
// END token after every element with content, and none after an empty element
// -- decodes the same document the other way round. This is the shape on the
// wire that the round trip above cannot produce by itself, because writer and
// decoder share a file.
func TestSpecShapedDocumentDecodes(t *testing.T) {
	// Written out byte by byte: the stream is the thing under test.
	//
	// <Sync><Collections><Collection><SyncKey>7</SyncKey><CollectionId>1</>
	// <MoreAvailable/></Collection></Collections></Sync>
	document := []byte{0x03, 0x01, 0x6A, 0x00,
		0x45,                        //   Sync, has content
		0x5C,                        //     Collections, has content (0x1C|0x40)
		0x4F,                        //       Collection, has content (0x0F|0x40)
		0x4B, 0x03, '7', 0x00, 0x01, //         SyncKey "7"
		0x52, 0x03, '1', 0x00, 0x01, //         CollectionId "1"
		0x14, //                                MoreAvailable, empty: no content bit, no END
		0x01, //       end Collection
		0x01, //     end Collections
		0x01, //   end Sync
	}

	root, err := NewWBXMLDecoder(document).Decode()
	if err != nil {
		t.Fatalf("decoding a spec-shaped document: %v", err)
	}
	if root.Name != "Sync" {
		t.Fatalf("root = %s, want Sync", root.Name)
	}
	coll := root.FindDescendant("Collection")
	if coll == nil {
		t.Fatal("the Collection element did not survive")
	}
	if got := coll.TextOf("SyncKey"); got != "7" {
		t.Errorf("SyncKey = %q, want 7", got)
	}
	if coll.FindChild("MoreAvailable") == nil {
		t.Error("the empty MoreAvailable element did not survive")
	}
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// The first FolderSync adds every folder the account has, and the one after it
// carries the key it returned and reports nothing, which is what a client
// reads as "the tree has not moved".
func TestFolderSyncListsEveryFolderAndThenReportsNoChange(t *testing.T) {
	ts := newTestService(t)
	inboxID := strconv.FormatInt(ts.folderID(t, "INBOX"), 10)

	rr := ts.post(t, folderSyncRequest(""))
	root := decodeRoot(t, rr)
	if root.Name != "FolderSync" {
		t.Fatalf("root = %s, want FolderSync", root.Name)
	}
	if got := root.TextOf("Status"); got != "1" {
		t.Fatalf("Status = %s, want 1", got)
	}
	key := root.TextOf("SyncKey")
	if key == "" {
		t.Fatal("the first FolderSync must return a SyncKey")
	}

	adds := folderAdds(root)
	byName := map[string]*WBXMLNode{}
	for _, f := range adds {
		byName[f.TextOf("DisplayName")] = f
	}
	for name, typ := range map[string]string{
		// MS-ASCMD 2.2.3.186.3: 2 Inbox, 3 Drafts, 4 Deleted (Junk and Trash
		// both), 5 Sent, 1 any other user-created folder.
		"INBOX":   "2",
		"Sent":    "5",
		"Drafts":  "3",
		"Junk":    "4",
		"Trash":   "4",
		"Archive": "1",
	} {
		f, ok := byName[name]
		if !ok {
			t.Errorf("the first FolderSync does not add %s; adds: %v", name, namesOf(adds))
			continue
		}
		if got := f.TextOf("Type"); got != typ {
			t.Errorf("%s Type = %s, want %s", name, got, typ)
		}
		if got := f.TextOf("ParentId"); got != "0" {
			t.Errorf("%s ParentId = %s, want 0 (top level)", name, got)
		}
	}
	if got := byName["INBOX"].TextOf("ServerId"); got != inboxID {
		t.Errorf("INBOX ServerId = %s, want the mailbox row id %s", got, inboxID)
	}

	// A calendar is in the tree, under its own kind of id.
	calendars := 0
	for _, f := range adds {
		if f.TextOf("Type") == "8" && strings.HasPrefix(f.TextOf("ServerId"), "cal") {
			calendars++
		}
	}
	if calendars == 0 {
		t.Error("no calendar folder in the first FolderSync")
	}

	// The next round, with the returned key, reports no change.
	rr = ts.post(t, folderSyncRequest(key))
	root = decodeRoot(t, rr)
	if got := root.TextOf("Status"); got != "1" {
		t.Fatalf("incremental Status = %s, want 1", got)
	}
	changes := root.FindChild("Changes")
	if changes == nil {
		t.Fatal("the incremental FolderSync has no Changes element")
	}
	if got := changes.TextOf("Count"); got != "0" {
		t.Errorf("incremental Count = %s, want 0", got)
	}
	if len(changes.FindChildren("Add"))+len(changes.FindChildren("Delete"))+len(changes.FindChildren("Update")) != 0 {
		t.Errorf("the incremental FolderSync reports changes: %v", namesOf(folderAdds(root)))
	}
}

// folderAdds collects the Add elements of a folder sync, keyed by ServerId.
func folderAdds(root *WBXMLNode) map[string]*WBXMLNode {
	out := map[string]*WBXMLNode{}
	changes := root.FindChild("Changes")
	if changes == nil {
		return out
	}
	for _, add := range changes.FindChildren("Add") {
		out[add.TextOf("ServerId")] = add
	}
	return out
}

func namesOf(folders map[string]*WBXMLNode) []string {
	out := make([]string, 0, len(folders))
	for _, f := range folders {
		out = append(out, f.TextOf("DisplayName"))
	}
	return out
}

// The first Sync of a folder adds every message, with the properties a client
// shows; the round after it, with the returned key, reports nothing; and a
// round after a delivery and a flag change reports the new message as an add
// and the flagged one as a replace.
func TestSyncAddsThenReportsOnlyTheDiff(t *testing.T) {
	ts := newTestService(t)
	inboxID := strconv.FormatInt(ts.folderID(t, "INBOX"), 10)

	ts.deliver(t, ts.folderID(t, "INBOX"), "first message", "the first body", "first@test", []string{`\Seen`})
	ts.deliver(t, ts.folderID(t, "INBOX"), "second message", "the second body", "second@test", nil)

	// First round: both messages are adds.
	rr := ts.post(t, syncRequest(inboxID, ""))
	root := decodeRoot(t, rr)
	if root.Name != "Sync" {
		t.Fatalf("root = %s, want Sync", root.Name)
	}
	coll := syncCollection(t, root)
	if got := coll.TextOf("CollectionId"); got != inboxID {
		t.Fatalf("CollectionId = %s, want %s", got, inboxID)
	}
	key := coll.TextOf("SyncKey")
	if key == "" {
		t.Fatal("the first Sync must return a SyncKey")
	}
	if coll.FindChild("MoreAvailable") != nil {
		t.Error("MoreAvailable is set for a folder this small")
	}

	adds := syncAdds(coll)
	if len(adds) != 2 {
		t.Fatalf("first Sync added %d items, want 2: %v", len(adds), serverIDsOf(adds))
	}
	bySubject := map[string]*WBXMLNode{}
	for _, email := range adds {
		bySubject[email.TextOf("Subject")] = email
	}
	first, ok := bySubject["first message"]
	if !ok {
		t.Fatalf("the first message is not an add; subjects: %v", subjectsOf(bySubject))
	}
	if got := first.TextOf("Read"); got != "1" {
		t.Errorf("a read message has Read = %s, want 1", got)
	}
	second, ok := bySubject["second message"]
	if !ok {
		t.Fatalf("the second message is not an add; subjects: %v", subjectsOf(bySubject))
	}
	if got := second.TextOf("Read"); got != "0" {
		t.Errorf("an unread message has Read = %s, want 0", got)
	}
	// The body a client will show is in the add, not in a follow-up it must
	// make separately.
	body := first.FindChild("Body")
	if body == nil || body.NS != "AirSyncBase" {
		t.Fatalf("the add carries no AirSyncBase Body: %+v", body)
	}
	if !strings.Contains(body.TextOf("Data"), "the first body") {
		t.Errorf("the body is not in the add's Body/Data: %+v", body)
	}
	if got := first.TextOf("DateReceived"); got == "" || len(got) != 15 {
		t.Errorf("DateReceived = %q, want the yyyymmddThhmmss form", got)
	}

	// Second round, with the key: nothing has changed, so nothing is reported.
	rr = ts.post(t, syncRequest(inboxID, key))
	coll = syncCollection(t, decodeRoot(t, rr))
	if len(syncAdds(coll))+len(syncReplaces(coll))+len(syncDeletes(coll)) != 0 {
		t.Errorf("an unchanged Sync reports changes: adds %v replaces %v deletes %v",
			serverIDsOf(syncAdds(coll)), serverIDsOf(syncReplaces(coll)), serverIDsOf(syncDeletes(coll)))
	}
	key = coll.TextOf("SyncKey")
	if key == "" {
		t.Fatal("the unchanged Sync must still return a SyncKey")
	}

	// A delivery and a flag change: the new message is an add, the flagged one
	// a replace, and the untouched one is absent from the answer.
	ts.deliver(t, ts.folderID(t, "INBOX"), "third message", "the third body", "third@test", nil)
	firstID := mustParseID(t, serverIDOfSubject(t, adds, "first message"))
	if err := ts.mail.MarkRead(context.Background(), testOwner, firstID, false); err != nil {
		t.Fatalf("marking the first message unread: %v", err)
	}

	rr = ts.post(t, syncRequest(inboxID, key))
	coll = syncCollection(t, decodeRoot(t, rr))
	adds = syncAdds(coll)
	replaces := syncReplaces(coll)
	if len(adds) != 1 || len(replaces) != 1 || len(syncDeletes(coll)) != 0 {
		t.Fatalf("the diff reports adds %v replaces %v, want one of each and no deletes",
			serverIDsOf(adds), serverIDsOf(replaces))
	}
	if got := onlyValue(adds).TextOf("Subject"); got != "third message" {
		t.Errorf("the add is %q, want the third message", got)
	}
	if got := onlyValue(replaces).TextOf("Subject"); got != "first message" {
		t.Errorf("the replace is %q, want the first message", got)
	}
	if got := onlyValue(replaces).TextOf("Read"); got != "0" {
		t.Errorf("the replaced message has Read = %s, want 0 (now unread)", got)
	}
}

// syncCollection is the Collection element of a Sync answer.
func syncCollection(t *testing.T, root *WBXMLNode) *WBXMLNode {
	t.Helper()
	cols := root.FindChild("Collections")
	if cols == nil {
		t.Fatal("the Sync answer has no Collections element")
	}
	coll := cols.FindChild("Collection")
	if coll == nil {
		t.Fatal("the Sync answer has no Collection element")
	}
	// The status is the collection's, not the whole answer's: a client
	// syncing two collections in one request reads one status per collection.
	if got := coll.TextOf("Status"); got != "1" {
		t.Fatalf("Collection Status = %s, want 1", got)
	}
	return coll
}

// syncAdds maps each Add's ServerId to its item properties.
func syncAdds(coll *WBXMLNode) map[string]*WBXMLNode {
	out := map[string]*WBXMLNode{}
	if commands := coll.FindChild("Commands"); commands != nil {
		for _, add := range commands.FindChildren("Add") {
			out[add.TextOf("ServerId")] = add.FindChild("ApplicationData")
		}
	}
	return out
}

// syncReplaces maps each Change's ServerId to its item properties.
func syncReplaces(coll *WBXMLNode) map[string]*WBXMLNode {
	out := map[string]*WBXMLNode{}
	if commands := coll.FindChild("Commands"); commands != nil {
		for _, change := range commands.FindChildren("Change") {
			out[change.TextOf("ServerId")] = change.FindChild("ApplicationData")
		}
	}
	return out
}

func syncDeletes(coll *WBXMLNode) map[string]*WBXMLNode {
	out := map[string]*WBXMLNode{}
	if commands := coll.FindChild("Commands"); commands != nil {
		for _, d := range commands.FindChildren("Delete") {
			out[d.TextOf("ServerId")] = d
		}
	}
	return out
}

func serverIDsOf(items map[string]*WBXMLNode) []string {
	out := make([]string, 0, len(items))
	for id := range items {
		out = append(out, id)
	}
	return out
}

// onlyValue is the single value of a map that the caller has just checked holds one.
func onlyValue(items map[string]*WBXMLNode) *WBXMLNode {
	var v *WBXMLNode
	for _, item := range items {
		v = item
	}
	return v
}

func subjectsOf(items map[string]*WBXMLNode) []string {
	out := make([]string, 0, len(items))
	for _, email := range items {
		if email != nil {
			out = append(out, email.TextOf("Subject"))
		}
	}
	return out
}

func serverIDOfSubject(t *testing.T, byServerID map[string]*WBXMLNode, subject string) string {
	t.Helper()
	for id, email := range byServerID {
		if email != nil && email.TextOf("Subject") == subject {
			return id
		}
	}
	t.Fatalf("no add with subject %q", subject)
	return ""
}

func mustParseID(t *testing.T, s string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("parsing server id %q: %v", s, err)
	}
	return id
}

// A SyncKey is the state of one folder, and a state that belongs to another
// folder is refused rather than diffed, which is where the diff would be
// nonsense. The refusal is in the body of a 200, so the client retries fresh
// instead of treating the endpoint as down.
func TestSyncRefusesAStateFromAnotherFolder(t *testing.T) {
	ts := newTestService(t)
	inboxID := strconv.FormatInt(ts.folderID(t, "INBOX"), 10)
	sentID := strconv.FormatInt(ts.folderID(t, "Sent"), 10)

	rr := ts.post(t, syncRequest(inboxID, ""))
	key := syncCollection(t, decodeRoot(t, rr)).TextOf("SyncKey")
	if key == "" {
		t.Fatal("the INBOX Sync did not return a key")
	}

	rr = ts.post(t, syncRequest(sentID, key))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an in-body status", rr.Code)
	}
	root := decodeRoot(t, rr)
	if got := root.TextOf("Status"); got != "1201" {
		t.Errorf("Status = %s, want 1201 for a state from another folder", got)
	}

	// The retry without the state is a fresh sync of the folder asked about.
	rr = ts.post(t, syncRequest(sentID, ""))
	coll := syncCollection(t, decodeRoot(t, rr))
	if got := coll.TextOf("CollectionId"); got != sentID {
		t.Errorf("CollectionId = %s, want %s", got, sentID)
	}
}

// SendMail sends through the same path the web client uses, so the message is
// in the Sent folder when the answer comes back, and the answer carries the
// Message-ID a client files against its own outbox.
// sendMailRequest builds a SendMail: one MIME document in the Mime element,
// which is the shape MS-ASCMD specifies.
func sendMailRequest(rawMIME string) []byte {
	w := NewWBXMLWriter()
	_ = w.Start(nsComposeMail, "SendMail")
	_ = w.Start(nsComposeMail, "SaveInSentItems")
	w.End()
	w.Start(nsComposeMail, "Mime")
	w.Opaque([]byte(rawMIME))
	w.End()
	w.End()
	return w.Bytes()
}

// A SendMail carries the message as one MIME document and is answered with an
// empty body: MS-ASCMD says a successful SendMail has no response content, and
// a client reads anything else as a failure to send.
func TestSendMailFilesIntoSent(t *testing.T) {
	ts := newTestService(t)

	mimeMessage := "From: " + testOwner + "\r\n" +
		"To: Bob <bob@example.test>\r\n" +
		"Subject: hello from eas\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"the sent body\r\n"

	rr := ts.post(t, sendMailRequest(mimeMessage))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %q", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("a successful SendMail must answer with an empty body, got %q",
			rr.Body.String())
	}

	var n int
	if err := ts.db.GetContext(context.Background(), &n, `
		SELECT count(*) FROM messages m
		JOIN mailboxes b ON b.id = m.mailbox_id
		WHERE b.owner = $1 AND b.name = 'Sent' AND m.subject = $2`,
		testOwner, "hello from eas"); err != nil {
		t.Fatalf("counting the sent copy: %v", err)
	}
	if n != 1 {
		t.Errorf("the Sent folder holds %d copies, want 1", n)
	}
}

// A SendMail whose MIME document names no recipient is understood but fails,
// and the failure is a status inside the command on a 200: an empty body is
// the success answer, so a failure has to say something a client can read.
func TestSendMailWithoutRecipientsIsRefused(t *testing.T) {
	ts := newTestService(t)

	mimeMessage := "Subject: nowhere to go\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"body\r\n"

	rr := ts.post(t, sendMailRequest(mimeMessage))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an in-body status", rr.Code)
	}
	root := decodeRoot(t, rr)
	// 119 is MessageHasNoRecipient, the code MS-ASCMD 2.2.2 names for this.
	if got := root.TextOf("Status"); got != "119" {
		t.Errorf("Status = %s, want 119 (MessageHasNoRecipient)", got)
	}
}

// Ping answers the heartbeat a push client waits on: the status, and the
// interval it should sleep for before asking again.
func TestPingAnswersTheHeartbeat(t *testing.T) {
	ts := newTestService(t)

	w := NewWBXMLWriter()
	_ = w.Start(nsPing, "Ping")
	_ = w.Leaf(nsPing, "HeartbeatInterval", "300")
	w.End()

	rr := ts.post(t, w.Bytes())
	root := decodeRoot(t, rr)
	if root.Name != "Ping" {
		t.Fatalf("root = %s, want Ping", root.Name)
	}
	if got := root.TextOf("Status"); got != "1" {
		t.Errorf("Status = %s, want 1", got)
	}
	if got := root.TextOf("HeartbeatInterval"); got != strconv.Itoa(heartbeatInterval) {
		t.Errorf("HeartbeatInterval = %s, want %d", got, heartbeatInterval)
	}
}

// Provision runs in two requests, MS-ASPROV's phases 2 and 3: the first asks
// for the policy and gets the policy document under a temporary key, the
// second acknowledges that key and gets the final one. The two keys differ, so
// a client can tell which half it is in, and the policy is a real document of
// elements rather than an empty shell, because a client reads its values to
// decide what it may do.
func TestProvisionAnswersAPolicy(t *testing.T) {
	ts := newTestService(t)

	// Phase 2: ask for the policy. The request names the policy type a
	// client on 12.0 or newer uses and carries its device information.
	request := func(policyKey string) []byte {
		w := NewWBXMLWriter()
		_ = w.Start(nsProvision, "Provision")
		w.Start(nsSettings, "DeviceInformation")
		w.Start(nsSettings, "Set")
		_ = w.Leaf(nsSettings, "Model", "test device")
		w.End()
		w.End()
		w.Start(nsProvision, "Policies")
		w.Start(nsProvision, "Policy")
		_ = w.Leaf(nsProvision, "PolicyType", "MS-EAS-Provisioning-WBXML")
		if policyKey != "" {
			_ = w.Leaf(nsProvision, "PolicyKey", policyKey)
			_ = w.Leaf(nsProvision, "Status", "1")
		}
		w.End()
		w.End()
		w.End()
		return w.Bytes()
	}

	root := decodeRoot(t, ts.post(t, request("")))
	if got := root.TextOf("Status"); got != "1" {
		t.Fatalf("Status = %s, want 1", got)
	}
	if info := root.FindChild("DeviceInformation"); info == nil || info.TextOf("Status") != "1" {
		t.Errorf("the answer does not acknowledge the device information: %+v", info)
	}
	policyEl := root.FindChild("Policies").FindChild("Policy")
	if policyEl == nil {
		t.Fatal("the Provision answer has no Policy element")
	}
	if got := policyEl.TextOf("PolicyType"); got != "MS-EAS-Provisioning-WBXML" {
		t.Errorf("PolicyType = %q, want MS-EAS-Provisioning-WBXML", got)
	}
	if got := policyEl.TextOf("Status"); got != "1" {
		t.Errorf("Policy Status = %s, want 1", got)
	}
	tempKey := policyEl.TextOf("PolicyKey")
	if tempKey == "" {
		t.Fatal("the policy download carries no PolicyKey")
	}
	doc := policyEl.FindDescendant("EASProvisionDoc")
	if doc == nil {
		t.Fatal("the policy Data carries no EASProvisionDoc")
	}
	if doc.TextOf("DevicePasswordEnabled") == "" {
		t.Errorf("the EASProvisionDoc carries no policy fields: %+v", doc.Children)
	}

	// Phase 3: acknowledge the temporary key and get the final one.
	root2 := decodeRoot(t, ts.post(t, request(tempKey)))
	if got := root2.TextOf("Status"); got != "1" {
		t.Fatalf("acknowledgement Status = %s, want 1", got)
	}
	policy2 := root2.FindChild("Policies").FindChild("Policy")
	finalKey := policy2.TextOf("PolicyKey")
	if finalKey == "" {
		t.Fatal("the acknowledgement carries no final PolicyKey")
	}
	if finalKey == tempKey {
		t.Errorf("the final PolicyKey equals the temporary one (%s); the two are distinct", finalKey)
	}
	if policy2.FindChild("Data") != nil {
		t.Error("the acknowledgement repeats the policy document; only the download carries it")
	}
}

// ItemOperations fetches one item in full: the same properties a Sync add
// carries, under Properties, which is what a client reads when it opens a
// message whose body a Sync truncated.
func TestItemOperationsFetchesAnItem(t *testing.T) {
	ts := newTestService(t)
	inboxID := ts.folderID(t, "INBOX")

	ts.deliver(t, inboxID, "fetch me", "the whole body", "fetchme@test", nil)
	var id int64
	if err := ts.db.GetContext(context.Background(), &id,
		`SELECT m.id FROM messages m
		 JOIN mailboxes b ON b.id = m.mailbox_id
		 WHERE b.owner = $1 AND m.subject = $2`, testOwner, "fetch me"); err != nil {
		t.Fatalf("looking up the message: %v", err)
	}

	w := NewWBXMLWriter()
	_ = w.Start(nsItemOperations, "ItemOperations")
	w.Start(nsItemOperations, "Fetch")
	_ = w.Leaf(nsItemOperations, "Store", "Mailbox")
	_ = w.Leaf(nsAirSync, "CollectionId", strconv.FormatInt(inboxID, 10))
	_ = w.Leaf(nsAirSync, "ServerId", strconv.FormatInt(id, 10))
	w.Start(nsItemOperations, "Options")
	w.Start(nsAirSyncBase, "BodyPreference")
	_ = w.Leaf(nsAirSyncBase, "Type", "1")
	w.End()
	w.End()
	w.End()
	w.End()

	rr := ts.post(t, w.Bytes())
	root := decodeRoot(t, rr)
	if got := root.TextOf("Status"); got != "1" {
		t.Fatalf("Status = %s, want 1", got)
	}
	fetch := root.FindDescendant("Fetch")
	if fetch == nil {
		t.Fatal("the ItemOperations answer has no Response/Fetch")
	}
	if got := fetch.TextOf("Status"); got != "1" {
		t.Fatalf("Fetch Status = %s, want 1", got)
	}
	properties := fetch.FindChild("Properties")
	if properties == nil {
		t.Fatal("the fetch carries no Properties")
	}
	if got := properties.TextOf("Subject"); got != "fetch me" {
		t.Errorf("Subject = %q, want %q", got, "fetch me")
	}
	body := properties.FindChild("Body")
	if body == nil || body.NS != "AirSyncBase" {
		t.Fatalf("the fetch carries no AirSyncBase Body: %+v", body)
	}
	if !strings.Contains(body.TextOf("Data"), "the whole body") {
		t.Errorf("the fetched body is missing: %q", body.TextOf("Data"))
	}
}

// A read flag the client sends in a Sync request is applied through the same
// service the web client uses, so a message marked read on a phone is marked
// read in the web client too. Marking read is a Sync Change per MS-ASCMD, not
// an ItemOperations operation.
func TestSyncAppliesAReadFlag(t *testing.T) {
	ts := newTestService(t)
	inboxID := ts.folderID(t, "INBOX")

	ts.deliver(t, inboxID, "flag me", "body", "flagme@test", nil)
	var id int64
	if err := ts.db.GetContext(context.Background(), &id,
		`SELECT m.id FROM messages m
		 JOIN mailboxes b ON b.id = m.mailbox_id
		 WHERE b.owner = $1 AND m.subject = $2`, testOwner, "flag me"); err != nil {
		t.Fatalf("looking up the message: %v", err)
	}

	// A sync request carrying the client's own change: the message was read
	// on the device.
	w := NewWBXMLWriter()
	_ = w.Start(nsAirSync, "Sync")
	w.Start(nsAirSync, "Collections")
	w.Start(nsAirSync, "Collection")
	_ = w.Leaf(nsAirSync, "SyncKey", "1")
	_ = w.Leaf(nsAirSync, "CollectionId", strconv.FormatInt(inboxID, 10))
	_ = w.Leaf(nsAirSync, "GetChanges", "1")
	w.Start(nsAirSync, "Commands")
	w.Start(nsAirSync, "Change")
	_ = w.Leaf(nsAirSync, "ServerId", strconv.FormatInt(id, 10))
	w.Start(nsAirSync, "ApplicationData")
	_ = w.Leaf(nsEmail, "Read", "1")
	w.End()
	w.End()
	w.End()
	w.End()
	w.End()
	w.End()
	w.End()

	rr := ts.post(t, w.Bytes())
	decodeRoot(t, rr)

	var flags []string
	if err := ts.db.SelectContext(context.Background(), &flags,
		`SELECT unnest(flags) FROM messages WHERE id = $1`, id); err != nil {
		t.Fatalf("reading the flags: %v", err)
	}
	hasSeen := false
	for _, f := range flags {
		if f == `\Seen` {
			hasSeen = true
		}
	}
	if !hasSeen {
		t.Errorf("the message is not marked read: flags %v", flags)
	}
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// searchRequest is a Search of one folder (or of the whole mailbox when no
// folder is named) for a free-text term.
func searchRequest(collectionID, query string) []byte {
	w := NewWBXMLWriter()
	_ = w.Start(nsSearch, "Search")
	_ = w.Start(nsSearch, "Store")
	_ = w.Leaf(nsSearch, "Name", "Mailbox")
	_ = w.Start(nsSearch, "Query")
	_ = w.Start(nsSearch, "And")
	if collectionID != "" {
		_ = w.Leaf(nsAirSync, "CollectionId", collectionID)
	}
	_ = w.Leaf(nsSearch, "FreeText", query)
	w.End() // And
	w.End() // Query
	_ = w.Start(nsSearch, "Options")
	_ = w.Leaf(nsSearch, "Range", "0-9")
	w.End() // Options
	w.End() // Store
	w.End() // Search
	return w.Bytes()
}

// A Search finds the messages the query matches, reports the total, and
// renders each result with the properties a Sync item carries, so a client
// that finds a message here opens it the way it opens one it synced.
func TestSearchFindsMatchingMessages(t *testing.T) {
	ts := newTestService(t)
	inbox := strconv.FormatInt(ts.folderID(t, "INBOX"), 10)

	ts.deliver(t, ts.folderID(t, "INBOX"), "Quarterly presentation deck", "figures for the quarter", "deck-1", nil)
	ts.deliver(t, ts.folderID(t, "INBOX"), "Lunch in the kitchen", "sandwiches at noon", "lunch-1", nil)
	ts.deliver(t, ts.folderID(t, "Archive"), "Presentation archive", "old slides", "arch-1", nil)

	rr := ts.post(t, searchRequest(inbox, "presentation"))
	root := decodeRoot(t, rr)
	if root.Name != "Search" {
		t.Fatalf("root = %s, want Search", root.Name)
	}
	if got := root.TextDescendant("Status"); got != "1" {
		t.Fatalf("Status = %s, want 1", got)
	}
	if got := root.TextDescendant("Total"); got != "1" {
		t.Fatalf("Total = %s, want 1: the Inbox holds one presentation", got)
	}
	results := root.FindDescendant("Store").FindChildren("Result")
	if len(results) != 1 {
		t.Fatalf("got %d Result elements, want 1", len(results))
	}
	if got := results[0].TextDescendant("Subject"); got != "Quarterly presentation deck" {
		t.Errorf("result Subject = %q, want the deck", got)
	}

	// A query that names no folder searches every folder the owner has.
	rr = ts.post(t, searchRequest("", "presentation"))
	root = decodeRoot(t, rr)
	if got := root.TextDescendant("Total"); got != "2" {
		t.Fatalf("Total = %s, want 2: the archive holds the other presentation", got)
	}

	// A term nothing contains matches nothing, and says so rather than failing.
	rr = ts.post(t, searchRequest(inbox, "zzzz-no-such-term"))
	root = decodeRoot(t, rr)
	if got := root.TextDescendant("Total"); got != "0" {
		t.Fatalf("Total = %s, want 0", got)
	}
	if len(root.FindDescendant("Store").FindChildren("Result")) != 0 {
		t.Error("a query that matched nothing must carry no Result elements")
	}
}

// ---------------------------------------------------------------------------
// Sync uploads: Delete, calendar Add, MeetingResponse
// ---------------------------------------------------------------------------

// syncDeleteRequest deletes one item from one folder.
func syncDeleteRequest(folderID, syncKey, serverID string) []byte {
	w := NewWBXMLWriter()
	_ = w.Start(nsAirSync, "Sync")
	_ = w.Leaf(nsAirSync, "ClientId", "test-client")
	_ = w.Start(nsAirSync, "Collections")
	_ = w.Start(nsAirSync, "Collection")
	_ = w.Leaf(nsAirSync, "CollectionId", folderID)
	_ = w.Leaf(nsAirSync, "SyncKey", syncKey)
	_ = w.Start(nsAirSync, "Commands")
	_ = w.Start(nsAirSync, "Delete")
	_ = w.Leaf(nsAirSync, "ServerId", serverID)
	w.End() // Delete
	w.End() // Commands
	w.End() // Collection
	w.End() // Collections
	w.End() // Sync
	return w.Bytes()
}

// folderCreateRequest creates a folder of the given type.
func folderCreateRequest(name, typ string) []byte {
	w := NewWBXMLWriter()
	_ = w.Start(nsFolderHierarchy, "FolderCreate")
	_ = w.Leaf(nsFolderHierarchy, "DisplayName", name)
	_ = w.Leaf(nsFolderHierarchy, "Type", typ)
	w.End()
	return w.Bytes()
}

// calendarAddRequest adds an appointment to a calendar folder.
func calendarAddRequest(calendarID, syncKey, subject, start, end string) []byte {
	w := NewWBXMLWriter()
	_ = w.Start(nsAirSync, "Sync")
	_ = w.Leaf(nsAirSync, "ClientId", "test-client")
	_ = w.Start(nsAirSync, "Collections")
	_ = w.Start(nsAirSync, "Collection")
	_ = w.Leaf(nsAirSync, "CollectionId", calendarID)
	_ = w.Leaf(nsAirSync, "SyncKey", syncKey)
	_ = w.Start(nsAirSync, "Commands")
	_ = w.Start(nsAirSync, "Add")
	_ = w.Start(nsAirSync, "ApplicationData")
	_ = w.Leaf(nsCalendar, "Subject", subject)
	_ = w.Leaf(nsCalendar, "StartTime", start)
	_ = w.Leaf(nsCalendar, "EndTime", end)
	w.End() // ApplicationData
	w.End() // Add
	w.End() // Commands
	w.End() // Collection
	w.End() // Collections
	w.End() // Sync
	return w.Bytes()
}

// meetingResponseRequest answers one meeting with the UserResponse code the
// spec defines: 1 accept, 2 decline, 3 tentative.
func meetingResponseRequest(calendarID string, answer int) []byte {
	w := NewWBXMLWriter()
	_ = w.Start(nsMeetingResponse, "MeetingResponse")
	_ = w.Start(nsMeetingResponse, "Request")
	_ = w.Leaf(nsMeetingResponse, "CalendarId", calendarID)
	_ = w.Leaf(nsMeetingResponse, "UserResponse", strconv.Itoa(answer))
	w.End() // Request
	w.End() // MeetingResponse
	return w.Bytes()
}

// A Sync Delete moves a message to Trash, and a Delete of a message already in
// Trash destroys it: that is what a client means by delete and by empty, and
// the folder's next sync reports the item as gone either way.
func TestSyncDeleteMovesMailToTrashAndPurgesFromTrash(t *testing.T) {
	ts := newTestService(t)
	inbox := ts.folderID(t, "INBOX")
	trash := ts.folderID(t, "Trash")
	ts.deliver(t, inbox, "A message to delete", "the body", "del-1", nil)

	rr := ts.post(t, syncRequest(strconv.FormatInt(inbox, 10), ""))
	coll := decodeRoot(t, rr).FindDescendant("Collection")
	key := coll.TextOf("SyncKey")
	serverID := serverIDOfSubject(t, syncAdds(coll), "A message to delete")

	rr = ts.post(t, syncDeleteRequest(strconv.FormatInt(inbox, 10), key, serverID))
	coll = decodeRoot(t, rr).FindDescendant("Collection")
	if got := coll.TextOf("Status"); got != "1" {
		t.Fatalf("the delete came back with Status %s, want 1", got)
	}

	var folderID int64
	if err := ts.db.GetContext(context.Background(), &folderID,
		`SELECT mailbox_id FROM messages WHERE id = $1`, serverID); err != nil {
		t.Fatalf("finding the message after the delete: %v", err)
	}
	if folderID != trash {
		t.Fatalf("the message is in folder %d, want Trash (%d)", folderID, trash)
	}

	// The Inbox's next sync reports the item as deleted.
	rr = ts.post(t, syncRequest(strconv.FormatInt(inbox, 10), key))
	coll = decodeRoot(t, rr).FindDescendant("Collection")
	if _, ok := syncDeletes(coll)[serverID]; !ok {
		t.Errorf("the Inbox sync does not report the deletion of %s", serverID)
	}

	// Deleting it again, from Trash, destroys the row.
	rr = ts.post(t, syncRequest(strconv.FormatInt(trash, 10), ""))
	trashKey := decodeRoot(t, rr).FindDescendant("Collection").TextOf("SyncKey")
	rr = ts.post(t, syncDeleteRequest(strconv.FormatInt(trash, 10), trashKey, serverID))
	decodeRoot(t, rr)

	var count int
	if err := ts.db.GetContext(context.Background(), &count,
		`SELECT count(*) FROM messages WHERE id = $1`, serverID); err != nil {
		t.Fatalf("counting the message after purging: %v", err)
	}
	if count != 0 {
		t.Errorf("the message still exists after a delete in Trash")
	}
}

// A Sync Add on a calendar folder creates a real event, and a MeetingResponse
// records the answer on it. The spec's answer field is UserResponse, so that
// is what the server reads, and the answer echoes back with the meeting's id.
func TestCalendarSyncAddCreatesAnEventAndMeetingResponseAnswersIt(t *testing.T) {
	ts := newTestService(t)

	rr := ts.post(t, folderCreateRequest("Team calendar", "13"))
	root := decodeRoot(t, rr)
	if root.TextOf("Status") != "1" {
		t.Fatalf("FolderCreate Status = %s, want 1", root.TextOf("Status"))
	}
	calID := root.TextOf("ServerId")
	if !strings.HasPrefix(calID, "cal") {
		t.Fatalf("the calendar ServerId is %q, want it to start with cal", calID)
	}

	rr = ts.post(t, syncRequest(calID, ""))
	coll := decodeRoot(t, rr).FindDescendant("Collection")
	key := coll.TextOf("SyncKey")

	rr = ts.post(t, calendarAddRequest(calID, key, "Kickoff, phase 2", "20260115T090000", "20260115T100000"))
	coll = decodeRoot(t, rr).FindDescendant("Collection")
	adds := syncAdds(coll)
	if len(adds) != 1 {
		t.Fatalf("the sync created %d events, want 1", len(adds))
	}
	eventID := serverIDOfSubject(t, adds, "Kickoff, phase 2")

	rr = ts.post(t, meetingResponseRequest(eventID, 1))
	root = decodeRoot(t, rr)
	if root.Name != "MeetingResponse" {
		t.Fatalf("root = %s, want MeetingResponse", root.Name)
	}
	result := root.FindChild("Result")
	if result == nil {
		t.Fatal("the MeetingResponse carries no Result")
	}
	if got := result.TextOf("Status"); got != "1" {
		t.Errorf("Result Status = %s, want 1", got)
	}
	if got := result.TextOf("CalendarId"); got != eventID {
		t.Errorf("Result CalendarId = %q, want %q", got, eventID)
	}

	var partstat string
	if err := ts.db.GetContext(context.Background(), &partstat,
		`SELECT a.partstat FROM attendees a JOIN events e ON e.id = a.event_id
		 WHERE e.summary = $1 AND a.address = $2`, "Kickoff, phase 2", testOwner); err != nil {
		t.Fatalf("reading the attendance: %v", err)
	}
	if partstat != "ACCEPTED" {
		t.Errorf("the organizer's partstat is %q, want ACCEPTED", partstat)
	}
}

// A calendar that belongs to somebody else is not synced: a ServerId is a
// row id, and knowing it is not owning it.
func TestSyncRefusesAnotherOwnersCalendar(t *testing.T) {
	ts := newTestService(t)

	ctx := context.Background()
	other := "mallory@example.test"
	if err := store.NewUsers().Create(ctx, ts.db, store.CreateUserRequest{Address: other, Source: "local"}); err != nil {
		t.Fatalf("creating the other account: %v", err)
	}
	cal, err := store.NewCalendars().Create(ctx, ts.db, store.CreateCalendarRequest{Owner: &other, Name: "Mallory's calendar"})
	if err != nil {
		t.Fatalf("creating the other calendar: %v", err)
	}

	rr := ts.post(t, syncRequest("cal"+strconv.FormatInt(cal.ID, 10), ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a status in the body", rr.Code)
	}
	dec := NewWBXMLDecoder(rr.Body.Bytes())
	root, err := dec.Decode()
	if err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	if got := root.TextDescendant("Status"); got == "1" {
		t.Error("a Sync of another owner's calendar succeeded; it must not")
	}
}

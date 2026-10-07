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

// post sends one command, with credentials the fixed authenticator accepts.
func (ts *testService) post(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/eas/Microsoft-Server-ActiveSync",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/vnd.ms-sync.wbxml")
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
	if err := dec.DecodeHeader(); err != nil {
		t.Fatalf("the answer is not a WBXML stream: %v", err)
	}
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
// same code pages, with the same text and the same opaque payloads.
func TestWBXMLRoundTrip(t *testing.T) {
	w := NewWBXMLWriter()
	_ = w.Start(nsAirSync, "Sync")
	_ = w.Leaf(nsAirSync, "CollectionId", "42")
	_ = w.Start(nsEmail, "Email")
	_ = w.Leaf(nsEmail, "Subject", "héllo wörld")
	_ = w.Start(nsEmail, "Body")
	w.Opaque([]byte{0x00, 0x01, 0xFF, 0x7F})
	w.End()
	w.End()
	w.End()

	dec := NewWBXMLDecoder(w.Bytes())
	if err := dec.DecodeHeader(); err != nil {
		t.Fatalf("decoding the header: %v", err)
	}
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
	email := root.FindChild("Email")
	if email == nil || email.NS != "Email" {
		t.Fatalf("the Email child is missing or in the wrong code page: %+v", email)
	}
	if got := email.TextOf("Subject"); got != "héllo wörld" {
		t.Errorf("Subject = %q, want %q", got, "héllo wörld")
	}
	body := email.FindChild("Body")
	if body == nil || !bytes.Equal(body.Opaque, []byte{0x00, 0x01, 0xFF, 0x7F}) {
		t.Errorf("the opaque body did not survive the round trip: %+v", body)
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
		"INBOX":   "2",
		"Sent":    "32",
		"Drafts":  "16",
		"Junk":    "8",
		"Trash":   "8",
		"Archive": "32768",
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
		if f.TextOf("Type") == "64" && strings.HasPrefix(f.TextOf("ServerId"), "cal") {
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
	if got := coll.TextOf("MoreAvailable"); got != "0" {
		t.Errorf("MoreAvailable = %s, want 0 for a folder this small", got)
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
	if got := first.TextOf("Flag"); got != "0" {
		t.Errorf("a read message has Flag = %s, want 0 (no unread bit)", got)
	}
	second, ok := bySubject["second message"]
	if !ok {
		t.Fatalf("the second message is not an add; subjects: %v", subjectsOf(bySubject))
	}
	if got := second.TextOf("Flag"); got != "1" {
		t.Errorf("an unread message has Flag = %s, want 1 (unread bit)", got)
	}
	// The body a client will show is in the add, not in a follow-up it must
	// make separately.
	body := first.FindChild("Body")
	if body == nil || !bytes.Contains(body.Opaque, []byte("the first body")) {
		t.Errorf("the add carries no body: %+v", body)
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
	if got := onlyValue(replaces).TextOf("Flag"); got != "1" {
		t.Errorf("the replaced message has Flag = %s, want 1 (now unread)", got)
	}
}

// syncCollection is the Collection element of a Sync answer.
func syncCollection(t *testing.T, root *WBXMLNode) *WBXMLNode {
	t.Helper()
	if got := root.TextOf("Status"); got != "1" {
		t.Fatalf("Status = %s, want 1", got)
	}
	cols := root.FindChild("Collections")
	if cols == nil {
		t.Fatal("the Sync answer has no Collections element")
	}
	coll := cols.FindChild("Collection")
	if coll == nil {
		t.Fatal("the Sync answer has no Collection element")
	}
	return coll
}

// syncAdds maps each Add's ServerId to its Email element.
func syncAdds(coll *WBXMLNode) map[string]*WBXMLNode {
	out := map[string]*WBXMLNode{}
	if adds := coll.FindChild("Adds"); adds != nil {
		for _, add := range adds.FindChildren("Add") {
			out[add.TextOf("ServerId")] = add.FindChild("Email")
		}
	}
	return out
}

// syncReplaces maps each Replace's ServerId to its Email element.
func syncReplaces(coll *WBXMLNode) map[string]*WBXMLNode {
	out := map[string]*WBXMLNode{}
	if reps := coll.FindChild("Replaces"); reps != nil {
		for _, r := range reps.FindChildren("Replace") {
			out[r.TextOf("ServerId")] = r.FindChild("Email")
		}
	}
	return out
}

func syncDeletes(coll *WBXMLNode) map[string]*WBXMLNode {
	out := map[string]*WBXMLNode{}
	if dels := coll.FindChild("Deletes"); dels != nil {
		for _, d := range dels.FindChildren("Delete") {
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
func TestSendMailFilesIntoSent(t *testing.T) {
	ts := newTestService(t)

	w := NewWBXMLWriter()
	_ = w.Start(nsEmail, "SendMail")
	_ = w.Start(nsEmail, "Email")
	_ = w.Start(nsEmail, "To")
	_ = w.Start(nsEmail, "Email")
	_ = w.Leaf(nsEmail, "Address", "bob@example.test")
	_ = w.Leaf(nsEmail, "Name", "Bob")
	w.End()
	w.End()
	_ = w.Leaf(nsEmail, "Subject", "hello from eas")
	_ = w.Start(nsEmail, "Body")
	_ = w.Leaf(nsEmail, "BodyType", "1")
	w.Text("the sent body")
	w.End()
	w.End()
	w.End()

	rr := ts.post(t, w.Bytes())
	root := decodeRoot(t, rr)
	if root.Name != "SendMail" {
		t.Fatalf("root = %s, want SendMail", root.Name)
	}
	if got := root.TextOf("Status"); got != "1" {
		t.Fatalf("Status = %s, want 1", got)
	}
	if root.TextOf("MessageId") == "" {
		t.Error("the SendMail answer carries no MessageId")
	}
	if rep := root.FindChild("SentReplica"); rep == nil || rep.FindChild("Email") == nil {
		t.Error("the SendMail answer carries no SentReplica")
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

// A client that sends no recipients gets the refusal in the body of a 200,
// because the command was understood and the message is the fault.
func TestSendMailWithoutRecipientsIsRefused(t *testing.T) {
	ts := newTestService(t)

	w := NewWBXMLWriter()
	_ = w.Start(nsEmail, "SendMail")
	_ = w.Start(nsEmail, "Email")
	_ = w.Leaf(nsEmail, "Subject", "nowhere to go")
	_ = w.Start(nsEmail, "Body")
	_ = w.Leaf(nsEmail, "BodyType", "1")
	w.Text("body")
	w.End()
	w.End()
	w.End()

	rr := ts.post(t, w.Bytes())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an in-body status", rr.Code)
	}
	root := decodeRoot(t, rr)
	if got := root.TextOf("Status"); got == "" || got == "1" {
		t.Errorf("Status = %s, want a non-1 status for a message with no recipients", got)
	}
}

// Ping answers the heartbeat a push client waits on: the status, and the
// interval it should sleep for before asking again.
func TestPingAnswersTheHeartbeat(t *testing.T) {
	ts := newTestService(t)

	w := NewWBXMLWriter()
	_ = w.Start(nsPing, "Ping")
	_ = w.Leaf(nsPing, "Wait", "300")
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

// Provision answers with a policy, not an empty shell: the client reads the
// values to decide what it may do, and the key is stable for the account.
func TestProvisionAnswersAPolicy(t *testing.T) {
	ts := newTestService(t)

	w := NewWBXMLWriter()
	_ = w.Start(nsProvision, "Provision")
	w.End()

	rr := ts.post(t, w.Bytes())
	root := decodeRoot(t, rr)
	if got := root.TextOf("Status"); got != "1" {
		t.Fatalf("Status = %s, want 1", got)
	}
	policies := root.FindChild("Policies")
	if policies == nil {
		t.Fatal("the Provision answer has no Policies element")
	}
	policyEl := policies.FindChild("Policy")
	if policyEl == nil {
		t.Fatal("the Provision answer has no Policy element")
	}
	if got := policyEl.TextOf("PolicyKey"); got == "" {
		t.Error("the policy has no PolicyKey")
	}
	data := policyEl.FindChild("Data")
	if data == nil || len(data.Opaque) == 0 {
		t.Fatal("the policy carries no data")
	}
	if !strings.Contains(string(data.Opaque), "<Policy>") {
		t.Errorf("the policy data is not a policy document: %q", string(data.Opaque[:min(len(data.Opaque), 40)]))
	}

	// The key is a function of the account, so the next provision of the same
	// account gets the same key.
	w2 := NewWBXMLWriter()
	_ = w2.Start(nsProvision, "Provision")
	w2.End()
	root2 := decodeRoot(t, ts.post(t, w2.Bytes()))
	key2 := root2.FindChild("Policies").FindChild("Policy").TextOf("PolicyKey")
	if key2 != policyEl.TextOf("PolicyKey") {
		t.Errorf("the PolicyKey changed between provisions of the same account: %s and %s",
			policyEl.TextOf("PolicyKey"), key2)
	}
}

// ItemOperations applies the flag changes through the same service the web
// client uses, so a flag set here is the flag the web client shows.
func TestItemOperationsMarksAMessageRead(t *testing.T) {
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

	w := NewWBXMLWriter()
	_ = w.Start(nsItemOperations, "ItemOperations")
	_ = w.Start(nsItemOperations, "Operations")
	_ = w.Start(nsItemOperations, "Operation")
	_ = w.Leaf(nsItemOperations, "ServerId", strconv.FormatInt(id, 10))
	_ = w.Start(nsItemOperations, "MarkAsRead")
	w.End()
	w.End()
	w.End()
	w.End()

	rr := ts.post(t, w.Bytes())
	root := decodeRoot(t, rr)
	if got := root.TextOf("Status"); got != "1" {
		t.Fatalf("Status = %s, want 1", got)
	}

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

package carddav_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-vcard"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/mailauth"
	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/carddav"
	"github.com/linchange/linchange/internal/contacts"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

const passwordText = "correct horse battery staple"

var testArgon = config.Argon2Params{
	MemoryKib: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
}

func seedUser(t *testing.T, db *sqlx.DB, address string) {
	t.Helper()
	ctx := context.Background()
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: address, Source: policy.SourceLocal, DisplayName: address,
	}); err != nil {
		t.Fatalf("seeding %s: %v", address, err)
	}
	hash, err := password.Hash(passwordText, testArgon)
	if err != nil {
		t.Fatalf("hashing %s: %v", address, err)
	}
	if err := store.NewCredentials().Set(ctx, db, address, hash); err != nil {
		t.Fatalf("storing the credential for %s: %v", address, err)
	}
}

func call(t *testing.T, client *http.Client, base, method, path, body, user string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	req.SetBasicAuth(user, passwordText)
	if method == "PUT" {
		req.Header.Set("Content-Type", "text/vcard; charset=utf-8")
	} else {
		req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	}
	if method == "PROPFIND" || method == "REPORT" {
		req.Header.Set("Depth", "1")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

const card = `BEGIN:VCARD
VERSION:3.0
UID:bob-card
FN:Bob Example
N:Example;Bob;;;
EMAIL;TYPE=work:bob@example.com
TEL;TYPE=work:+39 02 1234
END:VCARD`

func TestCardDAVLifecycle(t *testing.T) {
	db := testdb.Migrated(t)
	seedUser(t, db, "alice@example.com")
	seedUser(t, db, "bob@example.com")
	svc := contacts.New(db, nil)
	verifier := mailauth.New(db, nil, nil)
	srv := carddav.NewServer("/carddav", svc, db, verifier)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := &http.Client{Timeout: 10 * time.Second}

	// Discovery: the root reports the principal for the authenticated user.
	code, body := call(t, client, httpSrv.URL, "PROPFIND", "/carddav/", `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`, "alice@example.com")
	if code != http.StatusMultiStatus || !strings.Contains(body, "/carddav/alice@example.com") {
		t.Fatalf("root PROPFIND = %d %s", code, body)
	}

	// The home lists the personal book the user has by default.
	code, body = call(t, client, httpSrv.URL, "PROPFIND", "/carddav/alice@example.com/contacts/", `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>`, "alice@example.com")
	if code != http.StatusMultiStatus || !strings.Contains(body, "/carddav/alice@example.com/contacts/personal/") {
		t.Fatalf("home PROPFIND = %d %s", code, body)
	}

	// PUT a card, then read it back.
	code, body = call(t, client, httpSrv.URL, "PUT",
		"/carddav/alice@example.com/contacts/personal/bob-card.vcf", card, "alice@example.com")
	if code != http.StatusCreated && code != http.StatusNoContent {
		t.Fatalf("PUT = %d %s", code, body)
	}
	code, body = call(t, client, httpSrv.URL, "GET",
		"/carddav/alice@example.com/contacts/personal/bob-card.vcf", "", "alice@example.com")
	if code != http.StatusOK || !strings.Contains(body, "FN:Bob Example") {
		t.Fatalf("GET = %d %s", code, body)
	}

	// addressbook-query finds it by a property filter.
	code, body = call(t, client, httpSrv.URL, "REPORT", "/carddav/alice@example.com/contacts/personal/", `<?xml version="1.0"?>
<c:addressbook-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
  <d:prop><d:getetag/><c:address-data/></d:prop>
  <c:filter><c:prop-filter name="FN"><c:text-match>Bob</c:text-match></c:prop-filter></c:filter>
</c:addressbook-query>`, "alice@example.com")
	if code != http.StatusMultiStatus || !strings.Contains(body, "bob@example.com") {
		t.Fatalf("REPORT = %d %s", code, body)
	}

	// A new book can be made over CardDAV.
	code, body = call(t, client, httpSrv.URL, "MKCOL", "/carddav/alice@example.com/contacts/team", `<?xml version="1.0"?>
<d:mkcol xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
  <d:set><d:prop><d:resourcetype><d:collection/><c:addressbook/></d:resourcetype>
  <d:displayname>Team</d:displayname></d:prop></d:set>
</d:mkcol>`, "alice@example.com")
	if code != http.StatusCreated {
		t.Fatalf("MKCOL = %d %s", code, body)
	}
	code, body = call(t, client, httpSrv.URL, "PROPFIND", "/carddav/alice@example.com/contacts/", `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:displayname/></d:prop></d:propfind>`, "alice@example.com")
	if code != http.StatusMultiStatus || !strings.Contains(body, "/carddav/alice@example.com/contacts/team/") {
		t.Fatalf("home after MKCOL = %d %s", code, body)
	}

	// Share the new book with bob at read level; after he accepts it appears
	// under his own home, and he can read the card but not write one.
	book, err := svc.CreateBook(context.Background(), "alice@example.com", "Team contacts", "the team")
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	share, err := svc.Share(context.Background(), "alice@example.com", book.ID,
		store.ShareSubjectUser, "bob@example.com", 0, store.AddressBookShareRead)
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if _, err := svc.Decide(context.Background(), "bob@example.com", share.ID, "accepted"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := svc.PutCard(context.Background(), "alice@example.com", book.ID, "shared-card",
		mustCard(t)); err != nil {
		t.Fatalf("PutCard: %v", err)
	}

	code, body = call(t, client, httpSrv.URL, "PROPFIND", "/carddav/bob@example.com/contacts/", `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:displayname/></d:prop></d:propfind>`, "bob@example.com")
	if code != http.StatusMultiStatus || !strings.Contains(body, "~book-") {
		t.Fatalf("bob's home = %d %s", code, body)
	}
	alias := "/carddav/bob@example.com/contacts/~book-" + strconv.FormatInt(book.ID, 10) + "/"
	code, body = call(t, client, httpSrv.URL, "GET", alias+"shared-card.vcf", "", "bob@example.com")
	if code != http.StatusOK || !strings.Contains(body, "FN:Bob Example") {
		t.Fatalf("bob GET shared card = %d %s", code, body)
	}
	code, body = call(t, client, httpSrv.URL, "PUT", alias+"other.vcf", card, "bob@example.com")
	if code != http.StatusForbidden {
		t.Fatalf("bob PUT to a read-only share = %d %s, want 403", code, body)
	}
}

func mustCard(t *testing.T) vcard.Card {
	t.Helper()
	decoded, err := vcard.NewDecoder(strings.NewReader(card)).Decode()
	if err != nil {
		t.Fatalf("decoding the test card: %v", err)
	}
	return decoded
}

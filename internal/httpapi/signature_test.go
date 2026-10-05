package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/httpapi"
	"github.com/linchange/linchange/internal/mail"
)

// A one pixel PNG: the smallest thing that is genuinely an image, and so the
// smallest thing worth standing in for a logo in a signature.
var logoPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
	0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05,
	0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00,
	0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42,
	0x60, 0x82,
}

func logoDataURI() string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(logoPNG)
}

// getSignature reads the caller's stored signature.
func getSignature(t *testing.T, srv *httpapi.Server, headers map[string]string) string {
	t.Helper()
	code, body := do(t, srv, http.MethodGet, "/api/v1/mail/signature", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("GET signature = %d %v, want 200", code, body)
	}
	return str(body, "html")
}

// putSignature stores a signature and insists that it was accepted.
func putSignature(t *testing.T, srv *httpapi.Server, headers map[string]string, html string) map[string]any {
	t.Helper()
	code, body := do(t, srv, http.MethodPut, "/api/v1/mail/signature",
		map[string]any{"html": html}, headers)
	if code != http.StatusOK {
		t.Fatalf("PUT signature = %d %v, want 200", code, body)
	}
	return body
}

// A user who has never set a signature gets an empty one rather than an error, so
// the editor can open on a blank box without treating the ordinary case as a
// failure to load.
func TestSignatureDefaultsToEmpty(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	if got := getSignature(t, srv, headers); got != "" {
		t.Errorf("the signature of a user who has set none = %q, want empty", got)
	}
}

func TestSignatureRoundTrips(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	const sig = "<p>Alice Example</p><p>Director, Example Ltd</p>"
	putSignature(t, srv, headers, sig)

	if got := getSignature(t, srv, headers); got != sig {
		t.Errorf("the signature read back = %q, want %q", got, sig)
	}

	// And it is replaced rather than appended to, because a second save that
	// appended would be indistinguishable from two signatures.
	const shorter = "<p>Alice</p>"
	putSignature(t, srv, headers, shorter)
	if got := getSignature(t, srv, headers); got != shorter {
		t.Errorf("after replacing it, the signature = %q, want %q", got, shorter)
	}
}

func TestSignatureCanBeCleared(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	putSignature(t, srv, headers, "<p>Alice</p>")
	code, body := do(t, srv, http.MethodDelete, "/api/v1/mail/signature", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("DELETE signature = %d %v, want 200", code, body)
	}
	if got := getSignature(t, srv, headers); got != "" {
		t.Errorf("the signature after clearing = %q, want empty", got)
	}
}

// One user's signature is not another's, and the owner is taken from the token
// rather than from the body: a body that named somebody else would otherwise
// write into their mailbox.
func TestSignatureIsTheCallersOwn(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	alice := mailUser(t, srv, db, "alice@example.com")
	bob := mailUser(t, srv, db, "bob@example.com")

	putSignature(t, srv, alice, "<p>Alice</p>")
	putSignature(t, srv, bob, "<p>Bob</p>")

	if got := getSignature(t, srv, alice); got != "<p>Alice</p>" {
		t.Errorf("alice reads %q, want her own", got)
	}
	if got := getSignature(t, srv, bob); got != "<p>Bob</p>" {
		t.Errorf("bob reads %q, want his own", got)
	}
}

// A signature belongs to the mailbox it was set for, and it is a personal thing.
// Deleting the user has to take it with them, or a reused address would send
// under the previous holder's name.
func TestDeletingAUserTakesTheirSignature(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	putSignature(t, srv, headers, "<p>Alice</p>")
	if _, err := db.ExecContext(context.Background(),
		`DELETE FROM users WHERE address = 'alice@example.com'`); err != nil {
		t.Fatalf("deleting the user: %v", err)
	}

	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT count(*) FROM user_signatures WHERE owner = 'alice@example.com'`); err != nil {
		t.Fatalf("counting signatures: %v", err)
	}
	if n != 0 {
		t.Errorf("the signature outlived the user it belonged to")
	}
}

// The signature is part of what is sent, not part of what the user wrote, so it
// belongs in the bytes that go out and in the copy the sender reads back.
func TestSendingAppendsTheSignature(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	putSignature(t, srv, headers, "<p>Alice Example</p><p>Director</p>")

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/send", map[string]any{
		"to":        []map[string]any{{"address": "bob@example.com"}},
		"subject":   "Hello",
		"text_body": "the message itself",
		"html_body": "<p>the message itself</p>",
	}, headers)
	if code != http.StatusAccepted {
		t.Fatalf("send = %d %v, want 202", code, body)
	}

	detail := sentMessage(t, srv, db, headers, "alice@example.com")

	html := str(detail, "html_body")
	if !strings.Contains(html, "the message itself") {
		t.Errorf("the html body lost the message: %q", html)
	}
	if !strings.Contains(html, "Alice Example") {
		t.Errorf("the html body has no signature: %q", html)
	}
	if !strings.Contains(html, "Director") {
		t.Errorf("the html body has only part of the signature: %q", html)
	}
	// The signature has to be after the message, not before it.
	if strings.Index(html, "Alice Example") < strings.Index(html, "the message itself") {
		t.Errorf("the signature was put above the message: %q", html)
	}

	text := str(detail, "text_body")
	if !strings.Contains(text, "the message itself") || !strings.Contains(text, "Alice Example") {
		t.Errorf("the plain text part = %q, want the message and the signature", text)
	}
	// The conventional separator, so a reader can tell the signature from the
	// last line the sender wrote.
	if !strings.Contains(text, "-- ") {
		t.Errorf("the plain text part has no signature separator: %q", text)
	}
}

// A message composed in plain text gets a plain-text signature, and no HTML part
// appears out of nowhere.
func TestSendingPlainTextAppendsAPlainTextSignature(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	putSignature(t, srv, headers, "<p>Alice Example</p>")

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/send", map[string]any{
		"to":        []map[string]any{{"address": "bob@example.com"}},
		"subject":   "Hello",
		"text_body": "the message itself",
	}, headers)
	if code != http.StatusAccepted {
		t.Fatalf("send = %d %v, want 202", code, body)
	}

	detail := sentMessage(t, srv, db, headers, "alice@example.com")
	if got := str(detail, "html_body"); got != "" {
		t.Errorf("a plain-text message grew an html part: %q", got)
	}
	if text := str(detail, "text_body"); !strings.Contains(text, "Alice Example") {
		t.Errorf("the plain text body = %q, want the signature", text)
	}
}

// A plain-text alternative that is empty is worse than one holding the
// signature: a reader who cannot render HTML would otherwise be left with
// nothing at all and no idea who wrote to them.
func TestSendingHTMLGivesThePlainTextReaderTheSignature(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	putSignature(t, srv, headers, "<p>Alice Example</p>")

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/send", map[string]any{
		"to":        []map[string]any{{"address": "bob@example.com"}},
		"subject":   "Hello",
		"html_body": "<p>the message itself</p>",
	}, headers)
	if code != http.StatusAccepted {
		t.Fatalf("send = %d %v, want 202", code, body)
	}

	detail := sentMessage(t, srv, db, headers, "alice@example.com")
	text := str(detail, "text_body")
	if strings.TrimSpace(text) == "" {
		t.Errorf("the plain text alternative is empty, so a reader who cannot render HTML gets nothing")
	}
	if !strings.Contains(text, "Alice Example") {
		t.Errorf("the plain text alternative = %q, want the signature", text)
	}
}

// This is the requirement that makes a signature usable at all: the logo in it
// has to arrive as a part the recipient's client can resolve, not as a data: URI
// that Outlook refuses to draw and webmail clients strip.
func TestSendingCarriesTheSignatureImageAsAPart(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	putSignature(t, srv, headers, `<p>Alice Example</p><img src="`+logoDataURI()+`" alt="logo">`)

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/send", map[string]any{
		"to":        []map[string]any{{"address": "bob@example.com"}},
		"subject":   "Hello",
		"text_body": "the message itself",
		"html_body": "<p>the message itself</p>",
	}, headers)
	if code != http.StatusAccepted {
		t.Fatalf("send = %d %v, want 202", code, body)
	}

	parsed := parsedSent(t, srv, db, headers, "alice@example.com")

	// The body refers to a part rather than to inline bytes, which is the whole
	// point: a data: URI is not rendered by Outlook and is stripped by several
	// webmail clients.
	if strings.Contains(parsed.HTML, "data:image") {
		t.Errorf("the sent body still carries a data URI:\n%s", parsed.HTML)
	}
	if !strings.Contains(parsed.HTML, `src="cid:`) {
		t.Errorf("the sent body does not refer to an inline part:\n%s", parsed.HTML)
	}

	// And the part it refers to is really there, with the bytes in it. This is
	// read back through the same parser the reader uses, so a message that
	// looked right as bytes but could not be resolved does not pass.
	if len(parsed.Inline) != 1 {
		t.Fatalf("the message has %d inline parts, want 1: %+v", len(parsed.Inline), parsed.Inline)
	}
	img := parsed.Inline[0]
	if got := img.ContentType; !strings.HasPrefix(got, "image/png") {
		t.Errorf("the inline part is %q, want image/png", got)
	}
	if !strings.Contains(parsed.HTML, "cid:"+img.ContentID) {
		t.Errorf("the body does not refer to the part that is there, %q", img.ContentID)
	}
	if !bytes.Equal(img.Content, logoPNG) {
		t.Errorf("the inline part carries %d bytes, want the %d of the logo", len(img.Content), len(logoPNG))
	}
	// An image is not offered to the reader as a file to save, because the body
	// is what refers to it.
	if len(parsed.Attachments) != 0 {
		t.Errorf("the logo was sent as a downloadable attachment: %+v", parsed.Attachments)
	}
}

// A draft is the message the user is still writing. A client that put the
// signature in at save time would put another in every time it was saved, and the
// sent copy would carry all of them.
func TestDraftsDoNotCarryTheSignature(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	putSignature(t, srv, headers, "<p>Alice Example</p>")

	for i := range 3 {
		code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/draft", map[string]any{
			"subject":   "notes",
			"text_body": "half a thought",
		}, headers)
		if code != http.StatusCreated {
			t.Fatalf("saving draft %d = %d %v, want 201", i, code, body)
		}
	}

	var body string
	if err := db.GetContext(context.Background(), &body, `
		SELECT m.snippet FROM messages m
		JOIN mailboxes b ON b.id = m.mailbox_id
		WHERE b.owner = 'alice@example.com' AND b.name = 'Drafts'
		LIMIT 1`); err != nil {
		t.Fatalf("reading the draft: %v", err)
	}
	if strings.Contains(body, "Alice Example") {
		t.Errorf("the draft carries the signature: %q", body)
	}
}

// A user with no signature sends exactly what they wrote. The signature path runs
// on every send, so the case where there is nothing to append is the common one
// and has to leave the message alone.
func TestSendingWithNoSignatureAddsNothing(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/send", map[string]any{
		"to":        []map[string]any{{"address": "bob@example.com"}},
		"subject":   "Hello",
		"text_body": "the message itself",
		"html_body": "<p>the message itself</p>",
	}, headers)
	if code != http.StatusAccepted {
		t.Fatalf("send = %d %v, want 202", code, body)
	}

	detail := sentMessage(t, srv, db, headers, "alice@example.com")
	if html := str(detail, "html_body"); strings.Contains(html, "border-top") {
		t.Errorf("a separator was added to a message with no signature: %q", html)
	}
	if text := str(detail, "text_body"); strings.Contains(text, "-- ") {
		t.Errorf("a signature separator was added to a message with no signature: %q", text)
	}
}

// The signature is pasted markup that goes out inside somebody else's message, so
// what can execute in it does not go out with it.
func TestSendingStripsBehaviourFromTheSignature(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	putSignature(t, srv, headers,
		`<p onclick="steal()">Alice</p><script>fetch('x')</script><a href="javascript:alert(1)">site</a>`)

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/send", map[string]any{
		"to":        []map[string]any{{"address": "bob@example.com"}},
		"subject":   "Hello",
		"text_body": "the message itself",
		"html_body": "<p>the message itself</p>",
	}, headers)
	if code != http.StatusAccepted {
		t.Fatalf("send = %d %v, want 202", code, body)
	}

	raw := sentRaw(t, srv, db, headers, "alice@example.com")
	for _, gone := range []string{"steal(", "<script", "javascript:"} {
		if strings.Contains(strings.ToLower(raw), gone) {
			t.Errorf("the sent message still carries %q:\n%s", gone, raw)
		}
	}
	if !strings.Contains(raw, "Alice") {
		t.Errorf("cleaning took the text with it:\n%s", raw)
	}
}

// The size limit has to be reported as a limit rather than surfacing as a
// constraint violation, because the client is the only place that can say
// something useful about it.
func TestAnOversizedSignatureIsRefused(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	code, body := do(t, srv, http.MethodPut, "/api/v1/mail/signature", map[string]any{
		"html": "<p>" + strings.Repeat("x", 300_000) + "</p>",
	}, headers)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT signature = %d %v, want 400", code, body)
	}
	// And it was not half-stored on the way to being refused.
	if got := getSignature(t, srv, headers); got != "" {
		t.Errorf("a refused signature was stored anyway")
	}
}

// sentMessage reads the single message in an owner's Sent folder.
func sentMessage(t *testing.T, srv *httpapi.Server, db *sqlx.DB, headers map[string]string, owner string) map[string]any {
	t.Helper()
	id := onlyMessageID(t, db, owner, "Sent")
	code, body := do(t, srv, http.MethodGet, "/api/v1/mail/messages/"+strconv.FormatInt(id, 10), nil, headers)
	if code != http.StatusOK {
		t.Fatalf("reading the sent message = %d %v, want 200", code, body)
	}
	return body
}

// sentRaw reads a message as the bytes that would be transmitted.
func sentRaw(t *testing.T, srv *httpapi.Server, db *sqlx.DB, headers map[string]string, owner string) string {
	t.Helper()
	id := onlyMessageID(t, db, owner, "Sent")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/mail/messages/"+strconv.FormatInt(id, 10)+"/raw", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reading the raw message = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// parsedSent reads the sent message back through the same parser the reader
// uses, so a message whose bytes look plausible but do not resolve does not pass.
func parsedSent(t *testing.T, srv *httpapi.Server, db *sqlx.DB, headers map[string]string, owner string) *mail.Parsed {
	t.Helper()
	raw := sentRaw(t, srv, db, headers, owner)
	parsed, err := mail.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("the sent message does not parse, so no client can read it: %v\n%s", err, raw)
	}
	return parsed
}

// onlyMessageID is the id of the one message in a folder, which every test here
// arranges to be exactly one.
func onlyMessageID(t *testing.T, db *sqlx.DB, owner, folder string) int64 {
	t.Helper()
	var id int64
	err := db.GetContext(context.Background(), &id, `
		SELECT m.id FROM messages m
		JOIN mailboxes b ON b.id = m.mailbox_id
		WHERE b.owner = $1 AND b.name = $2
		ORDER BY m.id LIMIT 1`, owner, folder)
	if err != nil {
		t.Fatalf("reading the id of the message in %s: %v", folder, err)
	}
	return id
}

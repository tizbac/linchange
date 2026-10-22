package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/httpapi"
	"github.com/linchange/linchange/internal/imapd"
)

// mailUser provisions a local user with a password and returns an authorization
// header for them. It is the same shape as seedAdmin, but for somebody who
// receives mail rather than administers it.
func mailUser(t *testing.T, srv *httpapi.Server, db *sqlx.DB, address string) map[string]string {
	t.Helper()
	seedUser(t, db, address, "local", true, true)
	setPassword(t, db, address, "correct horse battery staple")
	if err := imapd.NewMailboxRepo().EnsureFolders(context.Background(), db, address); err != nil {
		t.Fatalf("provisioning folders for %s: %v", address, err)
	}
	return adminToken(t, srv, address)
}

// seedMessage puts one message into a folder directly.
//
// The questions below are about counting and labelling, so the rows are written
// rather than delivered: a delivery would bring its own folder provisioning, its
// own threading and a whole outbox round trip along with the flag under test.
func seedMessage(t *testing.T, db *sqlx.DB, owner, folder string, uid int, flags []string) {
	t.Helper()
	var mailboxID int64
	if err := db.GetContext(context.Background(), &mailboxID,
		`SELECT id FROM mailboxes WHERE owner = $1 AND name = $2`, owner, folder); err != nil {
		t.Fatalf("reading the id of %s: %v", folder, err)
	}
	msgID := fmt.Sprintf("%s-%d", strings.ToLower(folder), uid)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO messages (mailbox_id, uid, blob_sha256, size, internal_date,
		                      flags, subject, from_address, message_id, thread_key)
		VALUES ($1, $2, sha256($3::bytea), 42, now(), $4, 'a subject',
		        'someone@example.com', $3, $3)`,
		mailboxID, uid, msgID, textArray(flags)); err != nil {
		t.Fatalf("seeding a message into %s: %v", folder, err)
	}
}

// num reads an integer field out of a decoded JSON body, which is a float64.
func num(m map[string]any, k string) int64 {
	f, _ := m[k].(float64)
	return int64(f)
}

// textArray renders a slice as a PostgreSQL text[] literal, so a test can pass
// flags or none without hand-writing the braces.
//
// Backslashes are doubled because the backslash is the escape character in an
// array literal: IMAP's \Seen written as {\Seen} would be read back as "Seen"
// and would then match no flag at all, which fails as a missing count rather
// than as a quoting mistake.
func textArray(v []string) string {
	if len(v) == 0 {
		return "{}"
	}
	escaped := make([]string, len(v))
	for i, s := range v {
		escaped[i] = strings.ReplaceAll(s, `\`, `\\`)
	}
	return "{" + strings.Join(escaped, ",") + "}"
}

// foldersByName reads the folder list into a map keyed on the folder name, which
// is how the assertions below address folders: by what a user calls them rather
// than by the order the server happened to return them in.
func foldersByName(t *testing.T, srv *httpapi.Server, headers map[string]string) map[string]map[string]any {
	t.Helper()
	code, body := do(t, srv, http.MethodGet, "/api/v1/mail/folders", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/mail/folders = %d %v, want 200", code, body)
	}
	list, _ := body["folders"].([]any)
	out := make(map[string]map[string]any, len(list))
	for _, raw := range list {
		f, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("folder entry is %T, want an object", raw)
		}
		out[str(f, "name")] = f
	}
	return out
}

// TestFolderCountsBelongToTheirOwnFolder pins the counts the web client shows
// next to each folder name.
//
// They used to be read from a slice that was index-aligned with the database's
// own ordering, while the folders themselves were sorted for display. INBOX
// sorts first and sorts last alphabetically, so the pairing was off by two
// positions and every folder reported its neighbour's numbers: an unread message
// in INBOX was counted as unread in Drafts. The counts are now looked up by
// mailbox id, which a sort cannot detach from the folder it belongs to, and this
// test is here to keep the two from being joined by position again.
func TestFolderCountsBelongToTheirOwnFolder(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	for _, f := range foldersByName(t, srv, headers) {
		if got := f["unread"]; got != float64(0) {
			t.Errorf("%s unread = %v, want 0 for an empty mailbox", str(f, "name"), got)
		}
		if got := f["total"]; got != float64(0) {
			t.Errorf("%s total = %v, want 0 for an empty mailbox", str(f, "name"), got)
		}
	}

	seedMessage(t, db, "alice@example.com", "INBOX", 1, nil)
	seedMessage(t, db, "alice@example.com", "INBOX", 2, nil)
	seedMessage(t, db, "alice@example.com", "INBOX", 3, []string{`\Seen`})
	seedMessage(t, db, "alice@example.com", "Sent", 1, []string{`\Seen`})

	folders := foldersByName(t, srv, headers)
	want := map[string]struct{ total, unread float64 }{
		"INBOX": {3, 2},
		"Sent":  {1, 0},
	}
	for name, exp := range want {
		f, ok := folders[name]
		if !ok {
			t.Errorf("no folder called %s in the folder list", name)
			continue
		}
		if got := f["total"]; got != exp.total {
			t.Errorf("%s total = %v, want %v", name, got, exp.total)
		}
		if got := f["unread"]; got != exp.unread {
			t.Errorf("%s unread = %v, want %v", name, got, exp.unread)
		}
	}
	// The reported symptom specifically: INBOX's unread count appearing on a
	// folder that has nothing in it.
	for _, name := range []string{"Drafts", "Junk", "Trash", "Archive"} {
		f, ok := folders[name]
		if !ok {
			continue
		}
		if got := f["unread"]; got != float64(0) {
			t.Errorf("%s unread = %v, want 0; a count from another folder has leaked onto it", name, got)
		}
		if got := f["total"]; got != float64(0) {
			t.Errorf("%s total = %v, want 0; a count from another folder has leaked onto it", name, got)
		}
	}
}

// A draft is a partly written message. The two things a sent message must have
// are the two things a draft most often does not, and refusing one for missing
// either throws away the user's own work for not being finished yet.
func TestSubfoldersCanBeCreatedAndStandardFoldersStayFixed(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	folders := foldersByName(t, srv, headers)
	inboxID := int64(folders["INBOX"]["id"].(float64))
	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/folders",
		map[string]any{"name": "Projects", "parent_id": inboxID}, headers)
	if code != http.StatusCreated {
		t.Fatalf("creating a subfolder = %d %v, want 201", code, body)
	}
	if str(body, "name") != "INBOX/Projects" {
		t.Errorf("subfolder name = %q, want INBOX/Projects", str(body, "name"))
	}
	if got := num(body, "parent_id"); got != inboxID {
		t.Errorf("subfolder parent = %d, want %d", got, inboxID)
	}
	projectsID := num(body, "id")

	// A grandchild proves the tree is more than one level deep.
	code, body = do(t, srv, http.MethodPost, "/api/v1/mail/folders",
		map[string]any{"name": "2026", "parent_id": projectsID}, headers)
	if code != http.StatusCreated {
		t.Fatalf("creating a nested subfolder = %d %v, want 201", code, body)
	}
	if str(body, "name") != "INBOX/Projects/2026" {
		t.Errorf("nested name = %q, want INBOX/Projects/2026", str(body, "name"))
	}

	// Renaming the parent types only the leaf; the parent path and the child
	// paths have to be carried along.
	code, body = do(t, srv, http.MethodPatch, fmt.Sprintf("/api/v1/mail/folders/%d", projectsID),
		map[string]any{"name": "Events"}, headers)
	if code != http.StatusOK {
		t.Fatalf("renaming a subfolder = %d %v, want 200", code, body)
	}
	if str(body, "name") != "INBOX/Events" {
		t.Errorf("renamed subfolder = %q, want INBOX/Events", str(body, "name"))
	}
	folders = foldersByName(t, srv, headers)
	if _, ok := folders["INBOX/Events/2026"]; !ok {
		t.Errorf("the child kept the old path; folders are %v", folders)
	}
	if _, ok := folders["INBOX/Projects"]; ok {
		t.Error("the old parent path is still listed")
	}

	// The standard folders are named by protocol role and cannot be renamed.
	code, _ = do(t, srv, http.MethodPatch, fmt.Sprintf("/api/v1/mail/folders/%d", inboxID),
		map[string]any{"name": "Inbox renamed"}, headers)
	if code != http.StatusBadRequest {
		t.Errorf("renaming a standard folder = %d, want 400", code)
	}
}

func TestDraftsSaveWithoutARecipientOrABody(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"a recipient and no body", map[string]any{
			"to": []map[string]any{{"address": "bob@example.com"}},
		}},
		{"a subject and no recipient", map[string]any{
			"subject": "notes for later",
		}},
		{"a body and no recipient", map[string]any{
			"text_body": "half a thought",
		}},
		{"a recipient in cc only", map[string]any{
			"cc": []map[string]any{{"address": "bob@example.com"}},
		}},
		{"an html body and no recipient", map[string]any{
			"html_body": "<p>bold <b>idea</b></p>",
		}},
		{"an attachment and nothing else", map[string]any{
			"attachments": []map[string]any{{
				"filename": "a.txt", "mime_type": "text/plain",
				"content": "data:text/plain;base64,aGk=",
			}},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testMigratedDB(t)
			srv := newServer(t, db)
			headers := mailUser(t, srv, db, "alice@example.com")

			code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/draft", tc.body, headers)
			if code != http.StatusCreated {
				t.Fatalf("saving a draft with %s = %d %v, want 201", tc.name, code, body)
			}
			if num(body, "message_id") == 0 {
				t.Errorf("saving a draft with %s returned no message_id: %v", tc.name, body)
			}

			// It has to be a draft where Drafts belongs, with the flag a client
			// filters on, not merely accepted and dropped.
			var flags []string
			if err := db.GetContext(context.Background(), &flags, `
				SELECT flags FROM messages
				WHERE mailbox_id = (
					SELECT id FROM mailboxes WHERE owner = 'alice@example.com' AND name = 'Drafts')
				ORDER BY id DESC LIMIT 1`); err != nil {
				t.Fatalf("reading the stored draft: %v", err)
			}
			if len(flags) != 1 || flags[0] != `\Draft` {
				t.Errorf("stored draft flags = %v, want exactly [\\Draft]", flags)
			}
		})
	}
}

// The rules that are about being well-formed rather than being finished still
// apply to a draft: a mistyped address is wrong, not unfinished, and the user
// wants to hear about it while the draft is still open.
func TestDraftsStillRefuseWhatIsMalformed(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/draft", map[string]any{
		"to":        []map[string]any{{"address": "not an address"}},
		"subject":   "x",
		"text_body": "y",
	}, headers)
	if code != http.StatusBadRequest {
		t.Errorf("saving a draft with a malformed address = %d %v, want 400", code, body)
	}

	// A subject that cannot be sent later either is refused now rather than at
	// send time, when the user is no longer in the message.
	long := strings.Repeat("x", 1000)
	code, body = do(t, srv, http.MethodPost, "/api/v1/mail/compose/draft", map[string]any{
		"subject": long,
	}, headers)
	if code != http.StatusBadRequest {
		t.Errorf("saving a draft with a 1000-character subject = %d %v, want 400", code, body)
	}

	// A draft with nothing in it at all still has nothing to save.
	code, body = do(t, srv, http.MethodPost, "/api/v1/mail/compose/draft", map[string]any{}, headers)
	if code != http.StatusBadRequest {
		t.Errorf("saving a completely empty draft = %d %v, want 400", code, body)
	}
}

// Saving a draft repeatedly replaces it instead of accumulating copies, and the
// grant of a draft id is how the client says which one it is replacing.
func TestSavingADraftTwiceReplacesIt(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/draft", map[string]any{
		"subject": "first",
	}, headers)
	if code != http.StatusCreated {
		t.Fatalf("first save = %d %v, want 201", code, body)
	}
	first := num(body, "message_id")

	code, body = do(t, srv, http.MethodPost, "/api/v1/mail/compose/draft", map[string]any{
		"subject": "second", "draft_id": first,
	}, headers)
	if code != http.StatusCreated {
		t.Fatalf("second save = %d %v, want 201", code, body)
	}

	var n int
	if err := db.GetContext(context.Background(), &n, `
		SELECT count(*) FROM messages
		WHERE mailbox_id = (
			SELECT id FROM mailboxes WHERE owner = 'alice@example.com' AND name = 'Drafts')`); err != nil {
		t.Fatalf("counting drafts: %v", err)
	}
	if n != 1 {
		t.Errorf("Drafts holds %d messages after saving the same draft twice, want 1", n)
	}
}

// Sending is the other half of the rule: a draft may be incomplete, but the
// message that goes out may not be.
func TestSendingStillNeedsARecipientAndABody(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/send", map[string]any{
		"text_body": "now it is finished",
	}, headers)
	if code != http.StatusBadRequest {
		t.Errorf("sending with no recipient = %d %v, want 400", code, body)
	}

	code, body = do(t, srv, http.MethodPost, "/api/v1/mail/compose/send", map[string]any{
		"to": []map[string]any{{"address": "bob@example.com"}},
	}, headers)
	if code != http.StatusBadRequest {
		t.Errorf("sending with no body = %d %v, want 400", code, body)
	}
}

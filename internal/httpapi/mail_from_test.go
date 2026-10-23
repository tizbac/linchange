package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/linchange/linchange/internal/mail"
)

// The From header must name the account, not only its address. A recipient
// seeing `alice@example.com` with no name is what made webmail mail look
// different from mail sent by every other client.
func TestSentMessageUsesTheSendersDisplayName(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")
	exec(t, db, `UPDATE users SET display_name = 'Alice Example' WHERE address = 'alice@example.com'`)

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/send", map[string]any{
		"to":        []map[string]any{{"address": "bob@example.com"}},
		"subject":   "with a name",
		"text_body": "hello",
	}, headers)
	if code != http.StatusAccepted {
		t.Fatalf("send = %d %v, want 202", code, body)
	}

	raw := sentRaw(t, srv, db, headers, "alice@example.com")
	meta, err := mail.Describe([]byte(raw))
	if err != nil {
		t.Fatalf("the sent message does not parse: %v\n%s", err, raw)
	}
	if meta.FromAddress != "alice@example.com" || meta.FromName != "Alice Example" {
		t.Errorf("From = %q <%q>, want Alice Example <alice@example.com>", meta.FromName, meta.FromAddress)
	}
}

// Sending as a shared mailbox must use the team name, not the name of the
// person who happened to press Send.
func TestSentMessageUsesTheSharedMailboxName(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")
	exec(t, db, `UPDATE users SET display_name = 'Alice Example' WHERE address = 'alice@example.com'`)

	seedUser(t, db, "support@example.com", "shared", true, true)
	exec(t, db, `UPDATE users SET display_name = 'Support Team' WHERE address = 'support@example.com'`)
	exec(t, db, `INSERT INTO shared_mailboxes (address, display_name) VALUES ('support@example.com', 'Support Team')`)
	exec(t, db, `INSERT INTO shared_mailbox_grants (shared_address, member_address, level)
		VALUES ('support@example.com', 'alice@example.com', 'contribute')`)

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/compose/send", map[string]any{
		"from":      "support@example.com",
		"to":        []map[string]any{{"address": "bob@example.com"}},
		"subject":   "from the team",
		"text_body": "hello",
	}, headers)
	if code != http.StatusAccepted {
		t.Fatalf("send as shared = %d %v, want 202", code, body)
	}

	raw := sentRaw(t, srv, db, headers, "alice@example.com")
	meta, err := mail.Describe([]byte(raw))
	if err != nil {
		t.Fatalf("the sent message does not parse: %v\n%s", err, raw)
	}
	if meta.FromAddress != "support@example.com" || meta.FromName != "Support Team" {
		t.Errorf("From = %q <%q>, want Support Team <support@example.com>", meta.FromName, meta.FromAddress)
	}
}

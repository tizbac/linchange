package httpapi_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/linchange/linchange/internal/store"
)

func pstSHA() string { return strings.Repeat("ab", 32) }

func TestPSTImportBatchIsIdempotent(t *testing.T) {
	srv, db := newAdminServer(t)
	seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	bob := adminToken(t, srv, "bob@example.com")

	ctx := context.Background()
	calendar, err := store.NewCalendars().EnsureDefault(ctx, db, "bob@example.com", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	book, err := store.NewAddressBooks().EnsurePersonal(ctx, db, "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/pst-import", map[string]any{
		"file_name":       "archive.pst",
		"sha256":          pstSHA(),
		"calendar_id":     calendar.ID,
		"address_book_id": book.ID,
	}, bob)
	if code != http.StatusAccepted {
		t.Fatalf("creating a PST import = %d %v, want 202", code, body)
	}
	if str(body, "owner") != "bob@example.com" {
		t.Errorf("owner = %q", str(body, "owner"))
	}
	sourceID := int64(body["id"].(float64))

	message := "From: Alice <alice@example.com>\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: Imported from PST\r\n" +
		"Date: Mon, 01 Jan 2024 10:00:00 +0000\r\n" +
		"Message-ID: <pst-1@example.com>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Hello from the archive.\r\n"

	batch := map[string]any{"items": []map[string]any{
		{
			"key":     "m:1",
			"kind":    "message",
			"target":  "PST Import/Inbox",
			"content": base64.StdEncoding.EncodeToString([]byte(message)),
			"date":    "2024-01-01T10:00:00Z",
			"seen":    true,
		},
		{
			"key":     "e:2",
			"kind":    "event",
			"uid":     "pst-event-2@linchange.import",
			"summary": "Weekly meeting",
			"dtstart": "2024-01-02T09:00:00Z",
			"dtend":   "2024-01-02T10:00:00Z",
			"content": base64.StdEncoding.EncodeToString([]byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")),
		},
		{
			"key":          "c:3",
			"kind":         "contact",
			"uid":          "pst-contact-3@linchange.import",
			"display_name": "Carol Example",
			"email":        "carol@example.com",
		},
	}}

	code, body = do(t, srv, http.MethodPost, "/api/v1/mail/pst-import/"+itoa(sourceID)+"/batch", batch, bob)
	if code != http.StatusOK {
		t.Fatalf("uploading a batch = %d %v, want 200", code, body)
	}
	stats, _ := body["stats"].(map[string]any)
	if stats["messages"].(float64) != 1 || stats["events"].(float64) != 1 || stats["contacts"].(float64) != 1 {
		t.Fatalf("first batch stats = %v", stats)
	}

	// The same keys again: the ledger recognises them.
	code, body = do(t, srv, http.MethodPost, "/api/v1/mail/pst-import/"+itoa(sourceID)+"/batch", batch, bob)
	if code != http.StatusOK {
		t.Fatalf("re-uploading a batch = %d %v", code, body)
	}
	stats, _ = body["stats"].(map[string]any)
	if stats["messages"].(float64) != 0 || stats["events"].(float64) != 0 || stats["contacts"].(float64) != 0 {
		t.Fatalf("second batch imported %v, want nothing", stats)
	}
	if stats["skipped"].(float64) != 3 {
		t.Fatalf("second batch skipped = %v, want 3", stats["skipped"])
	}

	var messages, events, contacts int
	if err := db.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM messages),
		       (SELECT count(*) FROM events),
		       (SELECT count(*) FROM contacts)`).Scan(&messages, &events, &contacts); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || events != 1 || contacts != 1 {
		t.Fatalf("stored %d messages, %d events, %d contacts", messages, events, contacts)
	}

	code, body = do(t, srv, http.MethodPost, "/api/v1/mail/pst-import/"+itoa(sourceID)+"/finish",
		map[string]any{"status": "completed"}, bob)
	if code != http.StatusOK || str(body, "status") != "completed" {
		t.Fatalf("finishing = %d %v", code, body)
	}
}

func TestPSTImportRefusesAnotherUsersSource(t *testing.T) {
	srv, db := newAdminServer(t)
	seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	seedUser(t, db, "carol@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	setPassword(t, db, "carol@example.com", "correct horse battery staple")
	bob := adminToken(t, srv, "bob@example.com")
	carol := adminToken(t, srv, "carol@example.com")

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/pst-import", map[string]any{
		"file_name": "archive.pst", "sha256": pstSHA(),
	}, bob)
	if code != http.StatusAccepted {
		t.Fatalf("creating = %d %v", code, body)
	}
	sourceID := itoa(int64(body["id"].(float64)))

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/mail/pst-import/" + sourceID, nil},
		{http.MethodPost, "/api/v1/mail/pst-import/" + sourceID + "/batch", map[string]any{"items": []any{}}},
		{http.MethodPost, "/api/v1/mail/pst-import/" + sourceID + "/finish", map[string]any{"status": "completed"}},
	} {
		code, _ := do(t, srv, tc.method, tc.path, tc.body, carol)
		if code != http.StatusNotFound {
			t.Errorf("%s %s as another user = %d, want 404", tc.method, tc.path, code)
		}
	}
}

func TestAdminPSTImportRoutesRefuseANonAdmin(t *testing.T) {
	srv, db := newAdminServer(t)
	seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	bob := adminToken(t, srv, "bob@example.com")

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/pst-import"},
		{http.MethodPost, "/api/v1/admin/pst-import"},
		{http.MethodGet, "/api/v1/admin/pst-import/1"},
		{http.MethodPost, "/api/v1/admin/pst-import/1/batch"},
		{http.MethodPost, "/api/v1/admin/pst-import/1/finish"},
	} {
		code, _ := do(t, srv, tc.method, tc.path, map[string]any{}, bob)
		if code != http.StatusForbidden {
			t.Errorf("%s %s as non-admin = %d, want 403", tc.method, tc.path, code)
		}
	}
}

func TestAdminPSTImportForAnyMailbox(t *testing.T) {
	srv, db := newAdminServer(t)
	adminAddr, headers := seedAdmin(t, srv, db)
	seedUser(t, db, "team@example.com", "shared", true, true)

	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/pst-import", map[string]any{
		"owner": "team@example.com", "file_name": "archive.pst", "sha256": pstSHA(),
	}, headers)
	if code != http.StatusAccepted {
		t.Fatalf("admin creating a PST import = %d %v, want 202", code, body)
	}
	if str(body, "owner") != "team@example.com" {
		t.Errorf("owner = %q", str(body, "owner"))
	}
	if str(body, "started_by") != adminAddr {
		t.Errorf("started_by = %q, want %q", str(body, "started_by"), adminAddr)
	}
	sourceID := itoa(int64(body["id"].(float64)))

	// The admin page cannot list another user's calendars, so the server
	// resolves the target mailbox's defaults when the request names none.
	code, body = do(t, srv, http.MethodPost, "/api/v1/admin/pst-import/"+sourceID+"/batch", map[string]any{
		"items": []map[string]any{
			{
				"key": "e:1", "kind": "event", "uid": "pst-admin-event@linchange.import",
				"summary": "Team meeting", "dtstart": "2024-02-01T09:00:00Z",
			},
			{
				"key": "c:2", "kind": "contact", "display_name": "Team Contact",
				"email": "contact@example.com",
			},
		},
	}, headers)
	if code != http.StatusOK {
		t.Fatalf("admin batch = %d %v", code, body)
	}
	stats, _ := body["stats"].(map[string]any)
	if stats["events"].(float64) != 1 || stats["contacts"].(float64) != 1 {
		t.Fatalf("admin batch stats = %v", stats)
	}

	code, body = do(t, srv, http.MethodGet, "/api/v1/admin/pst-import?owner=team%40example.com", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("admin listing = %d %v", code, body)
	}
	imports, _ := body["imports"].([]any)
	if len(imports) != 1 {
		t.Fatalf("admin listed %d imports, want 1", len(imports))
	}
}

func TestPSTImportCreateValidation(t *testing.T) {
	srv, db := newAdminServer(t)
	seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	bob := adminToken(t, srv, "bob@example.com")

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"missing digest", map[string]any{"file_name": "x.pst"}},
		{"short digest", map[string]any{"file_name": "x.pst", "sha256": "abcd"}},
		{"not hex", map[string]any{"file_name": "x.pst", "sha256": strings.Repeat("zz", 32)}},
	} {
		code, _ := do(t, srv, http.MethodPost, "/api/v1/mail/pst-import", tc.body, bob)
		if code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", tc.name, code)
		}
	}
}

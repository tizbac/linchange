package httpapi_test

import (
	"net/http"
	"strconv"
	"testing"
)

// Shared mailboxes cannot sign in with a password (the auth layer refuses the
// "shared" source), so a shared principal never reaches the user import route
// in practice. The route-level refuseSharedPrincipal is defense in depth. This
// test pins the login refusal, which is the real gate.
func TestSharedPrincipalCannotSignIn(t *testing.T) {
	srv, db := newAdminServer(t)
	seedAdmin(t, srv, db)
	seedUser(t, db, "team@example.com", "shared", true, true)
	setPassword(t, db, "team@example.com", "correct horse battery staple")

	code, _ := do(t, srv, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"login": "team@example.com", "password": "correct horse battery staple"}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("a shared mailbox signing in = %d, want 401", code)
	}
}

// An ordinary local user may create an import for their own mailbox. The job
// is accepted (202) and appears in their list.
func TestUserImportCreateAndList(t *testing.T) {
	srv, db := newAdminServer(t)
	seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	bob := adminToken(t, srv, "bob@example.com")

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/imports", map[string]any{
		"remote_host":     "imap.old.example.com",
		"remote_username": "bob@old.example.com",
		"remote_password": "old-secret",
	}, bob)
	if code != http.StatusAccepted {
		t.Fatalf("a local user creating an import = %d %v, want 202", code, body)
	}
	if str(body, "owner") != "bob@example.com" {
		t.Errorf("job owner = %q, want bob@example.com", str(body, "owner"))
	}
	if str(body, "status") != "queued" {
		t.Errorf("job status = %q, want queued", str(body, "status"))
	}

	code, body = do(t, srv, http.MethodGet, "/api/v1/mail/imports", nil, bob)
	if code != http.StatusOK {
		t.Fatalf("listing imports = %d %v, want 200", code, body)
	}
	imports, _ := body["imports"].([]any)
	if len(imports) != 1 {
		t.Fatalf("listed %d imports, want 1", len(imports))
	}
}

// A user cannot read or cancel someone else's import.
func TestUserImportCannotSeeAnotherUsersJob(t *testing.T) {
	srv, db := newAdminServer(t)
	seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	seedUser(t, db, "carol@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	setPassword(t, db, "carol@example.com", "correct horse battery staple")
	bob := adminToken(t, srv, "bob@example.com")
	carol := adminToken(t, srv, "carol@example.com")

	code, body := do(t, srv, http.MethodPost, "/api/v1/mail/imports", map[string]any{
		"remote_host":     "imap.old.example.com",
		"remote_username": "bob@old.example.com",
		"remote_password": "old-secret",
	}, bob)
	if code != http.StatusAccepted {
		t.Fatalf("creating an import = %d %v, want 202", code, body)
	}
	jobID := int64(body["id"].(float64))

	code, body = do(t, srv, http.MethodGet, "/api/v1/mail/imports/"+itoa(jobID), nil, carol)
	if code != http.StatusNotFound {
		t.Fatalf("another user reading bob's job = %d %v, want 404", code, body)
	}

	code, body = do(t, srv, http.MethodDelete, "/api/v1/mail/imports/"+itoa(jobID), nil, carol)
	if code != http.StatusNotFound {
		t.Fatalf("another user cancelling bob's job = %d %v, want 404", code, body)
	}
}

// The admin import routes refuse a non-admin.
func TestAdminImportRoutesRefuseANonAdmin(t *testing.T) {
	srv, db := newAdminServer(t)
	seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	bob := adminToken(t, srv, "bob@example.com")

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/imports"},
		{http.MethodPost, "/api/v1/admin/imports"},
		{http.MethodPost, "/api/v1/admin/imports/remote-folders"},
		{http.MethodGet, "/api/v1/admin/imports/1"},
		{http.MethodDelete, "/api/v1/admin/imports/1"},
	} {
		code, _ := do(t, srv, tc.method, tc.path, map[string]any{}, bob)
		if code != http.StatusForbidden {
			t.Errorf("%s %s as non-admin = %d, want 403", tc.method, tc.path, code)
		}
	}
}

// An admin may create an import for any mailbox, including a shared one.
func TestAdminImportForAnyMailbox(t *testing.T) {
	srv, db := newAdminServer(t)
	adminAddr, headers := seedAdmin(t, srv, db)
	seedUser(t, db, "team@example.com", "shared", true, true)

	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/imports", map[string]any{
		"owner":           "team@example.com",
		"remote_host":     "imap.old.example.com",
		"remote_username": "team@old.example.com",
		"remote_password": "old-secret",
	}, headers)
	if code != http.StatusAccepted {
		t.Fatalf("admin creating an import = %d %v, want 202", code, body)
	}
	if str(body, "owner") != "team@example.com" {
		t.Errorf("job owner = %q, want team@example.com", str(body, "owner"))
	}
	if str(body, "started_by") != adminAddr {
		t.Errorf("job started_by = %q, want %q", str(body, "started_by"), adminAddr)
	}

	code, body = do(t, srv, http.MethodGet, "/api/v1/admin/imports?owner=team%40example.com", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("admin listing imports = %d %v, want 200", code, body)
	}
	imports, _ := body["imports"].([]any)
	if len(imports) != 1 {
		t.Fatalf("admin listed %d imports for team, want 1", len(imports))
	}
}

// An admin may cancel any job.
func TestAdminImportCancel(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)

	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/imports", map[string]any{
		"owner":           "bob@example.com",
		"remote_host":     "imap.old.example.com",
		"remote_username": "bob@old.example.com",
		"remote_password": "old-secret",
	}, headers)
	if code != http.StatusAccepted {
		t.Fatalf("creating an import = %d %v, want 202", code, body)
	}
	jobID := int64(body["id"].(float64))

	code, body = do(t, srv, http.MethodDelete, "/api/v1/admin/imports/"+itoa(jobID), nil, headers)
	if code != http.StatusOK {
		t.Fatalf("cancelling an import = %d %v, want 200", code, body)
	}
	if str(body, "status") != "cancelled" {
		t.Errorf("job status after cancel = %q, want cancelled", str(body, "status"))
	}
}

// Creating an import with missing fields is a 400, not a 500.
func TestImportCreateValidation(t *testing.T) {
	srv, db := newAdminServer(t)
	seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	bob := adminToken(t, srv, "bob@example.com")

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"missing host", map[string]any{"remote_username": "u", "remote_password": "p"}},
		{"missing username", map[string]any{"remote_host": "h", "remote_password": "p"}},
		{"missing password", map[string]any{"remote_host": "h", "remote_username": "u"}},
	} {
		code, _ := do(t, srv, http.MethodPost, "/api/v1/mail/imports", tc.body, bob)
		if code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", tc.name, code)
		}
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

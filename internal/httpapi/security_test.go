package httpapi_test

import (
	"net/http"
	"strconv"
	"testing"
)

// The theme catalogue is administration, not a per-user preference. A normal
// mailbox user must not be able to list, upload or delete deployment-wide
// themes.
func TestThemeAdminRoutesRefuseNonAdmins(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	_, admin := seedAdmin(t, srv, db)
	alice := mailUser(t, srv, db, "alice@example.com")

	if code, body := do(t, srv, http.MethodGet, "/api/v1/admin/themes/", nil, admin); code != http.StatusOK {
		t.Fatalf("admin listing themes = %d %v, want 200", code, body)
	}
	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/admin/themes/", nil},
		{http.MethodPost, "/api/v1/admin/themes/", map[string]any{}},
		{http.MethodDelete, "/api/v1/admin/themes/some-uid", nil},
	} {
		code, body := do(t, srv, tc.method, tc.path, tc.body, alice)
		if code != http.StatusForbidden {
			t.Errorf("%s %s as a normal user = %d %v, want 403", tc.method, tc.path, code, body)
		}
	}
}

// Creating a book inside an organization is an administration action. A
// normal member must not be able to inject one into somebody else's org.
func TestWorkgroupAddressBookCreationRequiresAdmin(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	_, admin := seedAdmin(t, srv, db)
	alice := mailUser(t, srv, db, "alice@example.com")

	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/workgroups", map[string]any{
		"name":   "acme",
		"domain": "example.com",
	}, admin)
	if code != http.StatusCreated {
		t.Fatalf("creating a workgroup = %d %v", code, body)
	}
	id := num(body, "id")
	if id == 0 {
		t.Fatalf("workgroup response has no id: %v", body)
	}

	path := "/api/v1/admin/workgroups/" + strconv.FormatInt(id, 10) + "/address-books"
	if code, body := do(t, srv, http.MethodPost, path, map[string]any{"name": "Injected"}, alice); code != http.StatusForbidden {
		t.Fatalf("non-admin creating an organization book = %d %v, want 403", code, body)
	}
}

func TestMustChangePasswordBlocksMailAPI(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	exec(t, db, `UPDATE users SET must_change_password = true WHERE address = 'alice@example.com'`)
	srv := newServer(t, db)
	headers := adminToken(t, srv, "alice@example.com")

	if code, body := do(t, srv, http.MethodGet, "/api/v1/mail/folders", nil, headers); code != http.StatusForbidden {
		t.Fatalf("mail folders with a forced password change = %d %v, want 403", code, body)
	}
	if code, body := do(t, srv, http.MethodGet, "/api/v1/auth/me", nil, headers); code != http.StatusOK {
		t.Fatalf("auth/me with a forced password change = %d %v, want 200", code, body)
	}
}

// A refresh token is single-use. The old one stops working as soon as the new
// pair is issued, which is what makes a stolen refresh token detectable.
func TestRefreshRotatesAndBurnsTheOldToken(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	srv := newServer(t, db)

	code, body := do(t, srv, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"login": "alice@example.com", "password": "correct horse battery staple"}, nil)
	if code != http.StatusOK {
		t.Fatalf("login = %d %v", code, body)
	}
	old := str(body, "refresh_token")

	code, body = do(t, srv, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": old}, nil)
	if code != http.StatusOK {
		t.Fatalf("first refresh = %d %v", code, body)
	}
	rotated := str(body, "refresh_token")
	if rotated == "" || rotated == old {
		t.Fatalf("refresh did not rotate the token: %q -> %q", old, rotated)
	}

	// The rotated token works once more and rotates again.
	code, body = do(t, srv, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": rotated}, nil)
	if code != http.StatusOK {
		t.Fatalf("rotated refresh = %d %v", code, body)
	}
	// Reusing the burned token is treated as theft and revokes the session.
	code, body = do(t, srv, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": old}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("reused refresh = %d %v, want 401", code, body)
	}
}

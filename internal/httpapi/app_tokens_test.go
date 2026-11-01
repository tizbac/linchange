package httpapi_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestApplicationPasswordDeviceFlow is the whole approval: a client starts it,
// the signed-in person approves in the browser, and the client polls once and
// receives a password that works in a mail client.
func TestApplicationPasswordDeviceFlow(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	code, started := do(t, srv, http.MethodPost, "/api/v1/auth/app-token",
		map[string]any{"description": "MailCraft on the test laptop"}, nil)
	if code != http.StatusOK {
		t.Fatalf("starting an approval = %d %v", code, started)
	}
	device := str(started, "device_code")
	userCode := str(started, "user_code")
	verify := str(started, "verification_url")
	if device == "" || userCode == "" || !strings.Contains(verify, "/app-auth?code=") {
		t.Fatalf("incomplete approval: %v", started)
	}
	// The browser is sent to the origin the client reached, not to a configured
	// hostname that may be a different address or port.
	if !strings.HasPrefix(verify, "http://example.com/app-auth?code=") {
		t.Fatalf("the verification address = %q, want the request's origin", verify)
	}

	// The browser page names what is asking, only to a signed-in person.
	if code, body := do(t, srv, http.MethodGet, "/api/v1/profile/application-tokens/request/"+userCode, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("reading a request without signing in = %d %v, want 401", code, body)
	}
	code, request := do(t, srv, http.MethodGet, "/api/v1/profile/application-tokens/request/"+userCode, nil, headers)
	if code != http.StatusOK || str(request, "description") != "MailCraft on the test laptop" {
		t.Fatalf("reading a request = %d %v", code, request)
	}

	// Before approval the client is told to wait.
	code, pending := do(t, srv, http.MethodPost, "/api/v1/auth/app-token/token",
		map[string]any{"device_code": device}, nil)
	if code != http.StatusBadRequest || str(pending, "error") != "authorization_pending" {
		t.Fatalf("polling before approval = %d %v", code, pending)
	}

	// Approval creates no password by itself: the client's next poll does.
	code, decided := do(t, srv, http.MethodPost, "/api/v1/profile/application-tokens/request/"+userCode,
		map[string]any{"approve": true}, headers)
	if code != http.StatusOK || str(decided, "status") != "approved" {
		t.Fatalf("approving = %d %v", code, decided)
	}
	code, tokenBody := do(t, srv, http.MethodPost, "/api/v1/auth/app-token/token",
		map[string]any{"device_code": device}, nil)
	if code != http.StatusOK {
		t.Fatalf("redeeming the approval = %d %v", code, tokenBody)
	}
	password := str(tokenBody, "token")
	if !strings.HasPrefix(password, "lcapp_") || str(tokenBody, "username") != "alice@example.com" {
		t.Fatalf("the issued password = %v", tokenBody)
	}

	// The same device code cannot be redeemed twice.
	if code, body := do(t, srv, http.MethodPost, "/api/v1/auth/app-token/token",
		map[string]any{"device_code": device}, nil); code != http.StatusBadRequest || str(body, "error") != "invalid_grant" {
		t.Fatalf("second poll = %d %v, want invalid_grant", code, body)
	}

	// The owner sees the new password and can revoke it.
	code, listed := do(t, srv, http.MethodGet, "/api/v1/profile/application-tokens", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("listing application passwords = %d %v", code, listed)
	}
	tokens, _ := listed["tokens"].([]any)
	if len(tokens) != 1 {
		t.Fatalf("listed application passwords = %v", listed)
	}
	one, _ := tokens[0].(map[string]any)
	id := fmt.Sprint(one["id"])
	if id == "" || one["description"] != "MailCraft on the test laptop" {
		t.Fatalf("the listed password = %v", one)
	}
	// The password and its digest are never part of a listing: only the name,
	// prefix and times are.
	if _, has := one["token"]; has {
		t.Fatalf("the listing exposes the password: %v", one)
	}
	if _, has := one["token_hash"]; has {
		t.Fatalf("the listing exposes the password digest: %v", one)
	}
	if code, body := do(t, srv, http.MethodDelete, "/api/v1/profile/application-tokens/"+id, nil, headers); code != http.StatusNoContent {
		t.Fatalf("revoking = %d %v", code, body)
	}
	code, listed = do(t, srv, http.MethodGet, "/api/v1/profile/application-tokens", nil, headers)
	tokens, _ = listed["tokens"].([]any)
	if code != http.StatusOK || len(tokens) != 0 {
		t.Fatalf("after revoking = %d %v", code, listed)
	}
}

// TestApplicationPasswordDescriptionIsOneLine keeps a page from showing an
// approval whose name is a paragraph or carries control characters.
func TestApplicationPasswordDescriptionIsOneLine(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	code, body := do(t, srv, http.MethodPost, "/api/v1/auth/app-token",
		map[string]any{"description": "  MailCraft\non\tthis computer  "}, nil)
	if code != http.StatusOK {
		t.Fatalf("starting = %d %v", code, body)
	}
	userCode := str(body, "user_code")
	code, request := do(t, srv, http.MethodGet, "/api/v1/profile/application-tokens/request/"+userCode, nil, headers)
	if code != http.StatusOK || str(request, "description") != "MailCraft on this computer" {
		t.Fatalf("the request's description = %d %v, want one line", code, request)
	}
}

// TestWorkgroupLegacyMailAuthIsForAdministrators keeps the organization switch
// where the requirement puts it: an administrator sets it, and a normal member
// cannot.
func TestWorkgroupLegacyMailAuthIsForAdministrators(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	_, admin := seedAdmin(t, srv, db)
	alice := mailUser(t, srv, db, "alice@example.com")

	code, created := do(t, srv, http.MethodPost, "/api/v1/admin/workgroups",
		map[string]any{"name": "example", "domain": "example.com"}, admin)
	if code != http.StatusCreated {
		t.Fatalf("creating a workgroup = %d %v", code, created)
	}
	id := fmt.Sprint(num(created, "id"))
	if id == "0" {
		t.Fatalf("workgroup has no id: %v", created)
	}
	if created["legacy_mail_auth"] != false {
		t.Fatalf("a new organization allows the account password in mail clients by default: %v", created)
	}

	path := "/api/v1/admin/workgroups/" + id + "/"
	code, updated := do(t, srv, http.MethodPatch, path, map[string]any{
		"name": "example", "display_name": "Example", "description": "",
		"legacy_mail_auth": true,
	}, admin)
	if code != http.StatusOK || updated["legacy_mail_auth"] != true {
		t.Fatalf("enabling legacy auth = %d %v", code, updated)
	}
	code, fetched := do(t, srv, http.MethodGet, path, nil, admin)
	if code != http.StatusOK || fetched["legacy_mail_auth"] != true {
		t.Fatalf("reading the organization = %d %v", code, fetched)
	}
	if code, body := do(t, srv, http.MethodPatch, path, map[string]any{
		"name": "example", "legacy_mail_auth": false,
	}, alice); code != http.StatusForbidden {
		t.Fatalf("a member changing legacy auth = %d %v, want 403", code, body)
	}
}

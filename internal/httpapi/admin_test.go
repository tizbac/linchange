package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/httpapi"
	"github.com/linchange/linchange/internal/store"
)

// adminToken signs in as a domain administrator and returns a bearer header
// map ready to pass to do.
//
// The sign-in goes through the real login endpoint rather than minting a token
// directly, so the test exercises the same path a browser would and the token
// is bound to a real live session. A domain admin has no mailbox, so the sign-in
// carries no second factor: the enrolment requirement belongs on accounts that
// receive mail.
func adminToken(t *testing.T, srv *httpapi.Server, address string) map[string]string {
	t.Helper()

	code, body := do(t, srv, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"login": address, "password": "correct horse battery staple"}, nil)
	if code != http.StatusOK {
		t.Fatalf("signing in as %s = %d %v, want 200", address, code, body)
	}
	if body["requires_second_factor"] == true {
		t.Fatalf("%s was asked for a second factor: %v", address, body)
	}
	token := str(body, "access_token")
	if token == "" {
		t.Fatalf("no access token for %s: %v", address, body)
	}
	return map[string]string{"Authorization": "Bearer " + token}
}

// seedAdmin provisions a domain and an administrator, and returns the
// authorization header for that administrator.
func seedAdmin(t *testing.T, srv *httpapi.Server, db *sqlx.DB) (address string, headers map[string]string) {
	t.Helper()
	address = "root@example.com"
	seedUser(t, db, address, "domain_admin", false, true)
	setPassword(t, db, address, "correct horse battery staple")
	return address, adminToken(t, srv, address)
}

func newAdminServer(t *testing.T) (*httpapi.Server, *sqlx.DB) {
	t.Helper()
	db := testMigratedDB(t)
	return newServer(t, db), db
}

// --- the authorization boundary ---

// The administration API must not be reachable without a token. This is the
// outer wall; the service's own check is the inner one.
func TestAdminRoutesRequireAToken(t *testing.T) {
	srv, db := newAdminServer(t)
	seedAdmin(t, srv, db)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/domains"},
		{http.MethodGet, "/api/v1/admin/users"},
		{http.MethodGet, "/api/v1/admin/aliases"},
		{http.MethodGet, "/api/v1/admin/shared-mailboxes"},
		{http.MethodGet, "/api/v1/admin/audit"},
		{http.MethodGet, "/api/v1/admin/impersonation/grants"},
		{http.MethodPost, "/api/v1/admin/domains"},
		{http.MethodPost, "/api/v1/admin/users"},
	} {
		code, _ := do(t, srv, tc.method, tc.path, map[string]any{}, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", tc.method, tc.path, code)
		}
	}
}

// A valid token belonging to an ordinary user is not enough. This is the
// distinction that matters most: authenticated and permitted are different
// things, and the router must not conflate them.
func TestAdminRoutesRefuseANonAdmin(t *testing.T) {
	srv, db := newAdminServer(t)
	seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	bob := adminToken(t, srv, "bob@example.com")

	code, body := do(t, srv, http.MethodGet, "/api/v1/admin/users", nil, bob)
	if code != http.StatusForbidden {
		t.Fatalf("an ordinary user listing users = %d %v, want 403", code, body)
	}

	// And a write is refused too, not just a read.
	code, body = do(t, srv, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"address": "intruder@example.com", "source": "local", "password": "correct horse battery staple",
	}, bob)
	if code != http.StatusForbidden {
		t.Fatalf("an ordinary user creating a user = %d %v, want 403", code, body)
	}

	var n int
	if err := db.GetContext(t.Context(), &n,
		`SELECT count(*) FROM users WHERE address = 'intruder@example.com'`); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if n != 0 {
		t.Error("a refused request still created a user")
	}
}

// --- the routes ---

func TestAdminDomainLifecycle(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)
	// seedAdmin has already created example.com, so this uses a second name
	// rather than fighting the fixture.
	const name = "other.com"

	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/domains", map[string]any{
		"name": name, "is_local": true, "description": "a second domain",
	}, headers)
	if code != http.StatusCreated {
		t.Fatalf("creating a domain = %d %v, want 201", code, body)
	}
	if body["name"] != name {
		t.Errorf("name = %v, want %s", body["name"], name)
	}
	if body["status"] != "active" {
		t.Errorf("status = %v, want active", body["status"])
	}

	// A duplicate is a conflict, not a 500: it is a rule refusing the write.
	code, body = do(t, srv, http.MethodPost, "/api/v1/admin/domains",
		map[string]any{"name": name}, headers)
	if code != http.StatusConflict {
		t.Fatalf("a duplicate domain = %d %v, want 409", code, body)
	}

	code, body = do(t, srv, http.MethodGet, "/api/v1/admin/domains", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("listing domains = %d %v, want 200", code, body)
	}
	if domains, _ := body["domains"].([]any); len(domains) != 2 {
		t.Fatalf("got %v domains, want the fixture's and the new one", len(domains))
	}

	// A PATCH that names only the description must not zero the quota. This is
	// the merge behaviour the full-row store contract makes easy to get wrong.
	code, body = do(t, srv, http.MethodPatch, "/api/v1/admin/domains/"+name,
		map[string]any{"description": "renamed"}, headers)
	if code != http.StatusOK {
		t.Fatalf("patching a domain = %d %v, want 200", code, body)
	}
	if body["description"] != "renamed" {
		t.Errorf("description = %v, want the new value", body["description"])
	}
	if body["is_local"] != true {
		t.Errorf("is_local = %v, want a patch to leave it alone", body["is_local"])
	}
	if quota, _ := body["default_quota_bytes"].(float64); quota != float64(store.DefaultDomainQuotaBytes) {
		t.Errorf("default_quota_bytes = %v, want the schema default to survive the patch", body["default_quota_bytes"])
	}

	code, _ = do(t, srv, http.MethodPut, "/api/v1/admin/domains/"+name+"/status",
		map[string]any{"status": "suspended"}, headers)
	if code != http.StatusNoContent {
		t.Fatalf("suspending a domain = %d, want 204", code)
	}

	// An unknown status is rejected by name rather than reaching the database.
	code, body = do(t, srv, http.MethodPut, "/api/v1/admin/domains/"+name+"/status",
		map[string]any{"status": "paused"}, headers)
	if code != http.StatusBadRequest {
		t.Errorf("an unknown status = %d %v, want 400", code, body)
	}

	code, _ = do(t, srv, http.MethodGet, "/api/v1/admin/domains/nope.com", nil, headers)
	if code != http.StatusNotFound {
		t.Errorf("an unknown domain = %d, want 404", code)
	}
}

func TestAdminUserLifecycle(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)

	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"address": "bob@example.com", "source": "local",
		"display_name": "Bob", "password": "correct horse battery staple",
	}, headers)
	if code != http.StatusCreated {
		t.Fatalf("creating a user = %d %v, want 201", code, body)
	}
	if body["address"] != "bob@example.com" {
		t.Errorf("address = %v, want bob@example.com", body["address"])
	}
	// The response must not carry the password back.
	if strings.Contains(mustJSON(t, body), "correct horse") {
		t.Error("the response echoed the password")
	}

	code, body = do(t, srv, http.MethodGet, "/api/v1/admin/users", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("listing users = %d %v, want 200", code, body)
	}
	users, _ := body["users"].([]any)
	if len(users) != 2 {
		t.Fatalf("got %d users, want the administrator and Bob: %v", len(users), body)
	}
	// Each user reports its usage so a list page needs no request per row.
	first, _ := users[0].(map[string]any)
	if _, ok := first["quota_used_bytes"]; !ok {
		t.Error("a listed user does not report quota_used_bytes")
	}

	// Too short a password is the caller's mistake and says so.
	code, body = do(t, srv, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"address": "carol@example.com", "source": "local", "password": "short",
	}, headers)
	if code != http.StatusBadRequest {
		t.Fatalf("a short password = %d %v, want 400", code, body)
	}

	// A malformed address likewise.
	code, body = do(t, srv, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"address": "not-an-address", "source": "local", "password": "correct horse battery staple",
	}, headers)
	if code != http.StatusBadRequest {
		t.Fatalf("a malformed address = %d %v, want 400", code, body)
	}

	code, _ = do(t, srv, http.MethodPut, "/api/v1/admin/users/bob@example.com/enabled",
		map[string]any{"enabled": false}, headers)
	if code != http.StatusNoContent {
		t.Fatalf("disabling a user = %d, want 204", code)
	}
	code, _ = do(t, srv, http.MethodDelete, "/api/v1/admin/users/bob@example.com", nil, headers)
	if code != http.StatusNoContent {
		t.Fatalf("deleting a user = %d, want 204", code)
	}
	code, _ = do(t, srv, http.MethodGet, "/api/v1/admin/users/bob@example.com", nil, headers)
	if code != http.StatusNotFound {
		t.Errorf("a deleted user = %d, want 404", code)
	}
}

// An address in a path is escaped by any client that follows the usual rule
// for path segments — Angular's HttpClient does it for every address it puts in
// a URL — and chi reports the segment as it arrived in the route path. Read
// raw, the account the path names is "bob%40example.com", which exists for
// nobody, so a password change on the account sitting on screen came back 404
// "no such thing". Every string parameter goes through pathParam now, which
// leaves an unescaped value alone and reaches the same row either way.
func TestAdminPathsDecodeEscapedAddresses(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")

	const user = "/api/v1/admin/users/bob%40example.com"

	code, body := do(t, srv, http.MethodGet, user, nil, headers)
	if code != http.StatusOK {
		t.Fatalf("reading a user through an escaped address = %d %v, want 200", code, body)
	}
	if got := str(body, "address"); got != "bob@example.com" {
		t.Fatalf("address = %q, want bob@example.com", got)
	}

	// The reported request: a password change on an account that exists.
	code, body = do(t, srv, http.MethodPut, user+"/password",
		map[string]any{"password": "a different horse battery staple"}, headers)
	if code != http.StatusNoContent {
		t.Fatalf("setting a password through an escaped address = %d %v, want 204", code, body)
	}

	// The password is the account's afterwards, so the change changed
	// something rather than being a request that went nowhere.
	code, body = do(t, srv, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"login": "bob@example.com", "password": "a different horse battery staple"}, nil)
	if code != http.StatusOK {
		t.Fatalf("signing in after the password change = %d %v, want 200", code, body)
	}

	// Enabled is the same path shape and the same decode.
	code, body = do(t, srv, http.MethodPut, user+"/enabled",
		map[string]any{"enabled": false}, headers)
	if code != http.StatusNoContent {
		t.Fatalf("disabling through an escaped address = %d %v, want 204", code, body)
	}

	// An alias is the other address-bearing family of routes.
	code, body = do(t, srv, http.MethodPost, "/api/v1/admin/aliases", map[string]any{
		"address":      "b.bob@example.com",
		"targets":      []map[string]any{{"target_kind": "user", "target_address": "bob@example.com", "position": 0}},
		"display_name": "Bob's alias",
	}, headers)
	if code != http.StatusCreated {
		t.Fatalf("creating an alias = %d %v, want 201", code, body)
	}
	code, body = do(t, srv, http.MethodPut, "/api/v1/admin/aliases/b.bob%40example.com/enabled",
		map[string]any{"enabled": false}, headers)
	if code != http.StatusNoContent {
		t.Fatalf("disabling an alias through an escaped address = %d %v, want 204", code, body)
	}
}

func TestAdminSharedMailboxAndMembers(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	seedUser(t, db, "support@example.com", "shared", true, true)

	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/shared-mailboxes", map[string]any{
		"address": "support@example.com", "display_name": "Support", "colour": "#0b7285",
	}, headers)
	if code != http.StatusCreated {
		t.Fatalf("declaring a shared mailbox = %d %v, want 201", code, body)
	}

	// A colour that is not #rrggbb is refused with a message naming the shape.
	code, body = do(t, srv, http.MethodPost, "/api/v1/admin/shared-mailboxes", map[string]any{
		"address": "sales@example.com", "colour": "teal",
	}, headers)
	if code == http.StatusCreated {
		t.Fatalf("a malformed colour was accepted: %v", body)
	}

	code, _ = do(t, srv, http.MethodPost,
		"/api/v1/admin/shared-mailboxes/support@example.com/members",
		map[string]any{"member": "bob@example.com", "level": "editor"}, headers)
	if code != http.StatusNoContent {
		t.Fatalf("granting access = %d, want 204", code)
	}

	code, body = do(t, srv, http.MethodGet, "/api/v1/admin/shared-mailboxes", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("listing shared mailboxes = %d %v, want 200", code, body)
	}
	boxes, _ := body["shared_mailboxes"].([]any)
	if len(boxes) != 1 {
		t.Fatalf("got %d shared mailboxes, want 1: %v", len(boxes), body)
	}
	box, _ := boxes[0].(map[string]any)
	members, _ := box["members"].([]any)
	if len(members) != 1 {
		t.Fatalf("the listed mailbox has %d members, want 1: %v", len(members), box)
	}

	// A grant to itself is refused, since the owner already has every right.
	code, body = do(t, srv, http.MethodPost,
		"/api/v1/admin/shared-mailboxes/support@example.com/members",
		map[string]any{"member": "support@example.com", "level": "owner"}, headers)
	if code != http.StatusBadRequest {
		t.Fatalf("a self-grant = %d %v, want 400", code, body)
	}

	code, _ = do(t, srv, http.MethodDelete,
		"/api/v1/admin/shared-mailboxes/support@example.com/members",
		map[string]any{"member": "bob@example.com"}, headers)
	if code != http.StatusNoContent {
		t.Fatalf("revoking access = %d, want 204", code)
	}
	code, _ = do(t, srv, http.MethodDelete,
		"/api/v1/admin/shared-mailboxes/support@example.com/members",
		map[string]any{"member": "bob@example.com"}, headers)
	if code != http.StatusNotFound {
		t.Errorf("revoking twice = %d, want 404", code)
	}
}

// declareSharedBox provisions a shared principal and declares it, which is the
// fixture every member-grant test needs.
func declareSharedBox(t *testing.T, srv *httpapi.Server, db *sqlx.DB, headers map[string]string, address string) {
	t.Helper()
	seedUser(t, db, address, "shared", true, true)
	if code, body := do(t, srv, http.MethodPost, "/api/v1/admin/shared-mailboxes",
		map[string]any{"address": address, "display_name": "Support"}, headers); code != http.StatusCreated {
		t.Fatalf("declaring %s = %d %v, want 201", address, code, body)
	}
}

// Granting a member twice must change the level, not fail. The second grant is
// an ON CONFLICT DO UPDATE, and a member guard that also fired for UPDATE used
// to refuse it as a 409 conflict - the reported bug.
func TestAdminSharedMailboxMemberGrantIsRepeatable(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	seedUser(t, db, "carol@example.com", "local", true, true)
	declareSharedBox(t, srv, db, headers, "support@example.com")

	const members = "/api/v1/admin/shared-mailboxes/support@example.com/members"

	if code, body := do(t, srv, http.MethodPost, members,
		map[string]any{"member": "bob@example.com", "level": "read"}, headers); code != http.StatusNoContent {
		t.Fatalf("granting read = %d %v, want 204", code, body)
	}
	if code, body := do(t, srv, http.MethodPost, members,
		map[string]any{"member": "bob@example.com", "level": "editor"}, headers); code != http.StatusNoContent {
		t.Fatalf("re-granting as editor = %d %v, want 204", code, body)
	}
	if code, body := do(t, srv, http.MethodPost, members,
		map[string]any{"member": "carol@example.com", "level": "editor"}, headers); code != http.StatusNoContent {
		t.Fatalf("granting a second member = %d %v, want 204", code, body)
	}

	code, body := do(t, srv, http.MethodGet, "/api/v1/admin/shared-mailboxes", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("listing shared mailboxes = %d %v, want 200", code, body)
	}
	boxes, _ := body["shared_mailboxes"].([]any)
	if len(boxes) != 1 {
		t.Fatalf("got %d shared mailboxes, want 1: %v", len(boxes), body)
	}
	box, _ := boxes[0].(map[string]any)
	members2, _ := box["members"].([]any)
	if len(members2) != 2 {
		t.Fatalf("got %d members, want 2: %v", len(members2), box)
	}
	levels := map[string]string{}
	for _, m := range members2 {
		row, _ := m.(map[string]any)
		levels[str(row, "member_address")] = str(row, "level")
	}
	if levels["bob@example.com"] != "editor" {
		t.Errorf("bob's level = %q, want editor after the re-grant", levels["bob@example.com"])
	}
	if levels["carol@example.com"] != "editor" {
		t.Errorf("carol's level = %q, want editor", levels["carol@example.com"])
	}
}

// Every refusal a member grant can hit is a request the administrator can fix,
// so each is a 400 naming what is wrong. None of them may come back as the
// generic 409 "conflicts with something that already exists".
func TestAdminSharedMailboxMemberRefusalsAreNotConflicts(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	declareSharedBox(t, srv, db, headers, "support@example.com")

	for _, tc := range []struct{ name, shared, member, wantMsg string }{
		{"a domain administrator has no mailbox", "support@example.com",
			"root@example.com", "cannot hold mail"},
		{"a member that does not exist", "support@example.com",
			"ghost@example.com", "does not exist"},
		{"a shared mailbox that was never declared", "undeclared@example.com",
			"bob@example.com", "not a declared shared mailbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, srv, http.MethodPost,
				"/api/v1/admin/shared-mailboxes/"+tc.shared+"/members",
				map[string]any{"member": tc.member, "level": "editor"}, headers)
			if code != http.StatusBadRequest {
				t.Fatalf("granting = %d %v, want 400", code, body)
			}
			got := str(body, "error")
			if !strings.Contains(got, tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", got, tc.wantMsg)
			}
			if strings.Contains(got, "conflicts with something that already exists") {
				t.Errorf("error = %q, want a specific reason rather than a conflict", got)
			}
		})
	}
}

func TestAdminAliasLifecycle(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)

	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/aliases", map[string]any{
		"address":      "b.bob@example.com",
		"targets":      []map[string]any{{"target_kind": "user", "target_address": "bob@example.com", "position": 0}},
		"display_name": "Bob's alias",
		"description":  "Test alias",
	}, headers)
	if code != http.StatusCreated {
		t.Fatalf("creating an alias = %d %v, want 201", code, body)
	}

	// An alias pointing at somebody who does not exist is a conflict.
	code, body = do(t, srv, http.MethodPost, "/api/v1/admin/aliases", map[string]any{
		"address":      "b.nobody@example.com",
		"targets":      []map[string]any{{"target_kind": "user", "target_address": "nobody@example.com", "position": 0}},
		"display_name": "Nobody's alias",
		"description":  "Test alias to unknown user",
	}, headers)
	if code != http.StatusConflict {
		t.Fatalf("an alias to an unknown user = %d %v, want 409", code, body)
	}

	code, _ = do(t, srv, http.MethodPut, "/api/v1/admin/aliases/b.bob@example.com/enabled",
		map[string]any{"enabled": false}, headers)
	if code != http.StatusNoContent {
		t.Fatalf("disabling an alias = %d, want 204", code)
	}
	code, _ = do(t, srv, http.MethodDelete, "/api/v1/admin/aliases/b.bob@example.com", nil, headers)
	if code != http.StatusNoContent {
		t.Fatalf("deleting an alias = %d, want 204", code)
	}
}

// A request with a field the handler does not know is rejected outright rather
// than half-applied, so a typo in a client cannot silently do nothing.
func TestAdminRejectsUnknownFields(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)

	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/domains", map[string]any{
		"name": "example.com", "is_local": true, "colour": "teal",
	}, headers)
	if code != http.StatusBadRequest {
		t.Fatalf("an unknown field = %d %v, want 400", code, body)
	}
}

func TestAdminAuditIsQueryable(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)

	if code, body := do(t, srv, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"address": "bob@example.com", "source": "local", "password": "correct horse battery staple",
	}, headers); code != http.StatusCreated {
		t.Fatalf("creating a user = %d %v", code, body)
	}

	code, body := do(t, srv, http.MethodGet, "/api/v1/admin/audit?action=user.create&target=bob@example.com", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("querying the audit log = %d %v, want 200", code, body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: %v", len(entries), body)
	}
	entry, _ := entries[0].(map[string]any)
	if entry["action"] != "user.create" || entry["target_id"] != "bob@example.com" {
		t.Errorf("entry = %v, want the creation of Bob", entry)
	}

	// A filter that is not a number is refused rather than ignored.
	code, _ = do(t, srv, http.MethodGet, "/api/v1/admin/audit?limit=lots", nil, headers)
	if code != http.StatusBadRequest {
		t.Errorf("a non-numeric limit = %d, want 400", code)
	}
}

func TestAdminAuditSearchPaginationAndCSV(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)

	for _, address := range []string{"bob@example.com", "carol@example.com"} {
		if code, body := do(t, srv, http.MethodPost, "/api/v1/admin/users", map[string]any{
			"address": address, "source": "local", "password": "correct horse battery staple",
		}, headers); code != http.StatusCreated {
			t.Fatalf("creating %s = %d %v", address, code, body)
		}
	}

	// The single search box matches a value in any field, not only the actor
	// or target columns the old API exposed separately.
	code, body := do(t, srv, http.MethodGet,
		"/api/v1/admin/audit?search=bob&limit=1&offset=0", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("searching the audit log = %d %v", code, body)
	}
	if total, _ := body["total"].(float64); total != 1 {
		t.Errorf("total for search=bob = %v, want 1: %v", body["total"], body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("search=bob returned %d entries, want 1: %v", len(entries), body)
	}

	// The same term matches the action column.
	code, body = do(t, srv, http.MethodGet, "/api/v1/admin/audit?search=user.create", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("searching by action = %d %v", code, body)
	}
	total, _ := body["total"].(float64)
	if total < 2 {
		t.Errorf("total for search=user.create = %v, want at least 2", total)
	}
	all, _ := body["entries"].([]any)
	if len(all) != int(total) {
		t.Errorf("one page without limit returned %d of %v entries", len(all), total)
	}

	// Offset pages through the same filtered set.
	_, first := do(t, srv, http.MethodGet,
		"/api/v1/admin/audit?search=user.create&limit=1&offset=0", nil, headers)
	_, second := do(t, srv, http.MethodGet,
		"/api/v1/admin/audit?search=user.create&limit=1&offset=1", nil, headers)
	firstEntries, _ := first["entries"].([]any)
	secondEntries, _ := second["entries"].([]any)
	if len(firstEntries) != 1 || len(secondEntries) != 1 {
		t.Fatalf("paging returned %d and %d entries, want 1 each", len(firstEntries), len(secondEntries))
	}
	firstID := firstEntries[0].(map[string]any)["id"]
	secondID := secondEntries[0].(map[string]any)["id"]
	if firstID == secondID {
		t.Errorf("offset=1 returned the same entry as offset=0: %v", firstID)
	}

	// The CSV export applies the same filter but no page: both Bob and Carol's
	// actions fan out from the action term, while search=bob leaves only Bob.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit.csv?search=bob", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("exporting the audit log = %d %q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("CSV content type = %q, want text/csv", ct)
	}
	csvBody := rr.Body.String()
	lines := strings.Split(strings.TrimSpace(csvBody), "\n")
	if len(lines) != 2 {
		t.Errorf("CSV with search=bob has %d lines, want a header and one entry: %q", len(lines), csvBody)
	}
	if !strings.Contains(csvBody, "bob@example.com") {
		t.Errorf("CSV does not contain Bob: %q", csvBody)
	}
	if strings.Contains(csvBody, "carol@example.com") {
		t.Errorf("CSV contains an entry the filter excluded: %q", csvBody)
	}
}

func TestAdminImpersonationGrantsOverHTTP(t *testing.T) {
	srv, db := newAdminServer(t)
	adminAddr, headers := seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)

	// A grant with no reason is refused, because the reason is the record of
	// why the power was handed over.
	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/impersonation/grants",
		map[string]any{"actor": adminAddr, "target": "bob@example.com"}, headers)
	if code != http.StatusBadRequest {
		t.Fatalf("a grant with no reason = %d %v, want 400", code, body)
	}

	code, _ = do(t, srv, http.MethodPost, "/api/v1/admin/impersonation/grants", map[string]any{
		"actor": adminAddr, "target": "bob@example.com",
		"reason": "a support incident", "ttl_seconds": 3600,
	}, headers)
	if code != http.StatusNoContent {
		t.Fatalf("granting = %d, want 204", code)
	}

	code, body = do(t, srv, http.MethodGet, "/api/v1/admin/impersonation/who/bob@example.com", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("who can impersonate = %d %v, want 200", code, body)
	}
	grants, _ := body["grants"].([]any)
	if len(grants) != 1 {
		t.Fatalf("got %d grants, want 1: %v", len(grants), body)
	}
	grant, _ := grants[0].(map[string]any)
	if grant["active"] != true {
		t.Errorf("grant = %v, want it reported active", grant)
	}

	// The default listing is the caller's own grants, so a supervisor does not
	// have to name themselves.
	code, body = do(t, srv, http.MethodGet, "/api/v1/admin/impersonation/grants", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("listing grants = %d %v, want 200", code, body)
	}
	if grants, _ := body["grants"].([]any); len(grants) != 1 {
		t.Errorf("got %d grants for the caller, want 1: %v", len(grants), body)
	}

	code, _ = do(t, srv, http.MethodDelete,
		"/api/v1/admin/impersonation/grants/"+adminAddr+"/bob@example.com", nil, headers)
	if code != http.StatusNoContent {
		t.Fatalf("revoking = %d, want 204", code)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return string(blob)
}

// A PATCH to a user names only the descriptive fields, so it must not be able
// to disable the account or take away its mailbox. The store's update takes
// both as plain bools, which makes this the easiest mistake in the whole API to
// make and the most damaging.
func TestAdminUserPatchCannotDisable(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)

	code, body := do(t, srv, http.MethodPatch, "/api/v1/admin/users/bob@example.com", map[string]any{
		"display_name": "Robert",
	}, headers)
	if code != http.StatusOK {
		t.Fatalf("patching a user = %d %v, want 200", code, body)
	}
	if body["display_name"] != "Robert" {
		t.Errorf("display_name = %v, want the new value", body["display_name"])
	}
	if body["enabled"] != true {
		t.Error("enabled = false after a patch that did not mention it")
	}
	if body["mailbox_enabled"] != true {
		t.Error("mailbox_enabled = false after a patch that did not mention it")
	}

	// And the row agrees, rather than the response merely claiming so.
	var flags struct {
		Enabled        bool `db:"enabled"`
		MailboxEnabled bool `db:"mailbox_enabled"`
	}
	if err := db.GetContext(t.Context(), &flags,
		`SELECT enabled, mailbox_enabled FROM users WHERE address = 'bob@example.com'`); err != nil {
		t.Fatalf("reading the user row: %v", err)
	}
	if !flags.Enabled || !flags.MailboxEnabled {
		t.Errorf("the row says enabled=%v mailbox_enabled=%v, want both true",
			flags.Enabled, flags.MailboxEnabled)
	}
}

// newServerWithImpersonationGrants builds a server that requires a per-user
// grant, which is the configuration where the grant table is load-bearing.
func newServerWithImpersonationGrants(t *testing.T, db *sqlx.DB) *httpapi.Server {
	t.Helper()
	cfg := testConfig()
	cfg.Auth.Impersonation.RequireGrant = true
	srv, err := httpapi.New(httpapi.Options{DB: db, Config: cfg, Blobs: testBlobs(t)})
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	return srv
}

// The whole point of the grant is that it decides whether a sign-in may act as
// somebody else. This drives that through the public endpoints rather than
// calling the repository directly, because the two halves can be individually
// correct and still disagree about the spelling of the address.
func TestImpersonationFlowEndToEnd(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServerWithImpersonationGrants(t, db)
	adminAddr, headers := seedAdmin(t, srv, db)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")

	signIn := func(imp string) (int, map[string]any) {
		t.Helper()
		return do(t, srv, http.MethodPost, "/api/v1/auth/login", map[string]any{
			"login": adminAddr, "password": "correct horse battery staple",
			"impersonating": imp, "impersonation_reason": "a support incident",
		}, nil)
	}

	// No grant yet, so the configuration alone must not be enough.
	if code, body := signIn("bob@example.com"); code != http.StatusUnauthorized {
		t.Fatalf("impersonation without a grant = %d %v, want 401", code, body)
	}

	code, _ := do(t, srv, http.MethodPost, "/api/v1/admin/impersonation/grants", map[string]any{
		"actor": adminAddr, "target": "bob@example.com", "reason": "a support incident",
	}, headers)
	if code != http.StatusNoContent {
		t.Fatalf("granting = %d, want 204", code)
	}

	code, body := signIn("bob@example.com")
	if code != http.StatusOK {
		t.Fatalf("impersonation with a grant = %d %v, want 200", code, body)
	}
	access, _ := body["access_token"].(string)
	if access == "" {
		t.Fatal("want an access token from the impersonating sign-in")
	}
	as := map[string]string{"Authorization": "Bearer " + access}

	// The client has to be able to see that it is impersonating, or it cannot
	// draw the banner that tells a support agent whose mail they are reading.
	code, body = do(t, srv, http.MethodGet, "/api/v1/auth/me", nil, as)
	if code != http.StatusOK {
		t.Fatalf("reading /me = %d %v, want 200", code, body)
	}
	if body["address"] != adminAddr {
		t.Errorf("/me address = %v, want the admin: the session acts as, it does not become", body["address"])
	}
	if body["impersonating"] != "bob@example.com" {
		t.Errorf("/me impersonating = %v, want bob", body["impersonating"])
	}
	if body["is_domain_admin"] != true {
		t.Errorf("/me is_domain_admin = %v, want true", body["is_domain_admin"])
	}

	// And the sign-in is on the record, which is the whole reason the audit
	// entry is written inside the session's transaction.
	code, body = do(t, srv, http.MethodGet, "/api/v1/admin/audit?action=impersonation.start", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("reading the audit log = %d %v, want 200", code, body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("got %d impersonation.start entries, want 1: %v", len(entries), body)
	}
	entry, _ := entries[0].(map[string]any)
	if entry["actor_address"] != adminAddr || entry["target_address"] != "bob@example.com" {
		t.Errorf("entry = %v, want the admin acting on bob", entry)
	}

	// Revoking the grant takes the ability away on the next sign-in rather than
	// when the outstanding session happens to expire.
	if code, _ = do(t, srv, http.MethodDelete,
		"/api/v1/admin/impersonation/grants/"+adminAddr+"/bob@example.com", nil, headers); code != http.StatusNoContent {
		t.Fatalf("revoking = %d, want 204", code)
	}
	if code, body := signIn("bob@example.com"); code != http.StatusUnauthorized {
		t.Errorf("impersonation after revocation = %d %v, want 401", code, body)
	}

	// Ending the impersonation revokes the session, so the token the client is
	// holding stops working rather than silently becoming an admin token.
	code, body = do(t, srv, http.MethodPost, "/api/v1/auth/stop-impersonating", nil, as)
	if code != http.StatusNoContent {
		t.Fatalf("stopping the impersonation = %d %v, want 204", code, body)
	}
	if code, _ := do(t, srv, http.MethodGet, "/api/v1/auth/me", nil, as); code != http.StatusUnauthorized {
		t.Errorf("the revoked token = %d, want 401", code)
	}
	// And there is nothing left to stop on a fresh sign-in.
	fresh := map[string]string{"Authorization": "Bearer " + mustField(t, signInToken(t, srv, adminAddr, "correct horse battery staple"), "access_token")}
	if code, _ := do(t, srv, http.MethodPost, "/api/v1/auth/stop-impersonating", nil, fresh); code != http.StatusConflict {
		t.Errorf("stopping a plain sign-in = %d, want 409", code)
	}
}

func signInToken(t *testing.T, srv *httpapi.Server, address, password string) map[string]any {
	t.Helper()
	code, body := do(t, srv, http.MethodPost, "/api/v1/auth/login",
		map[string]any{"login": address, "password": password}, nil)
	if code != http.StatusOK {
		t.Fatalf("signing in as %s = %d %v", address, code, body)
	}
	return body
}

func mustField(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	v, _ := body[key].(string)
	if v == "" {
		t.Fatalf("response has no %q: %v", key, body)
	}
	return v
}

// An organization's calendar is shared from the administration page, whose
// picker offers groups by name. A share the response can describe only as
// "group 7" leaves the administrator reading a number where they chose a name,
// so the group is named back in both directions: on the row that is created
// and in the list the page shows afterwards.
func TestAdminCalendarSharesNameTheGroup(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)

	code, wg := do(t, srv, http.MethodPost, "/api/v1/admin/workgroups",
		map[string]any{"name": "Example", "domain": "example.com"}, headers)
	if code != http.StatusCreated {
		t.Fatalf("creating the organization = %d %v, want 201", code, wg)
	}
	wgID := int(wg["id"].(float64))

	code, group := do(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/admin/workgroups/%d/groups", wgID),
		map[string]any{"name": "Sales"}, headers)
	if code != http.StatusCreated {
		t.Fatalf("creating the group = %d %v, want 201", code, group)
	}
	groupID := int(group["id"].(float64))

	code, cal := do(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/admin/workgroups/%d/calendars", wgID),
		map[string]any{"name": "Team"}, headers)
	if code != http.StatusCreated {
		t.Fatalf("creating the organization's calendar = %d %v, want 201", code, cal)
	}
	calID := int(cal["id"].(float64))

	shares := fmt.Sprintf("/api/v1/admin/workgroups/%d/calendars/%d/shares", wgID, calID)
	code, created := do(t, srv, http.MethodPost, shares, map[string]any{
		"subject_kind": "group", "subject_group_id": groupID, "level": "write",
	}, headers)
	if code != http.StatusCreated {
		t.Fatalf("sharing the calendar = %d %v, want 201", code, created)
	}
	if got := str(created, "subject_name"); got != "Sales" {
		t.Errorf("subject_name = %q, want Sales", got)
	}
	if got := str(created, "level"); got != "write" {
		t.Errorf("level = %q, want write", got)
	}

	code, listed := do(t, srv, http.MethodGet, shares, nil, headers)
	if code != http.StatusOK {
		t.Fatalf("listing the shares = %d %v, want 200", code, listed)
	}
	rows, _ := listed["shares"].([]any)
	if len(rows) != 1 {
		t.Fatalf("got %d shares, want 1: %v", len(rows), listed)
	}
	row, _ := rows[0].(map[string]any)
	if got := str(row, "subject_name"); got != "Sales" {
		t.Errorf("the listed share is named %q, want Sales", got)
	}
	if int(row["subject_group_id"].(float64)) != groupID {
		t.Errorf("subject_group_id = %v, want %d", row["subject_group_id"], groupID)
	}

	// The share the page shows is the share that governs: taking it away takes
	// the write access with it, and the row goes.
	shareID := int(row["id"].(float64))
	code, body := do(t, srv, http.MethodDelete,
		fmt.Sprintf("%s/%d", shares, shareID), nil, headers)
	if code != http.StatusNoContent {
		t.Fatalf("removing the share = %d %v, want 204", code, body)
	}
	code, listed = do(t, srv, http.MethodGet, shares, nil, headers)
	if code != http.StatusOK || len(listed["shares"].([]any)) != 0 {
		t.Errorf("after removing the share, the list is %d %v, want an empty 200", code, listed)
	}
}

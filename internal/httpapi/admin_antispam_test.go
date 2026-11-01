package httpapi_test

import (
	"net/http"
	"testing"
)

// The allowed sources of a domain are the address list its inbound listener
// accepts mail from. They come back in the form the listener matches against,
// so "203.0.113.5" and "203.0.113.5/32" are one entry rather than two.
func TestAdminDomainAntispamNetworks(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)

	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/domains", map[string]any{
		"name":              "filtered.example",
		"is_local":          true,
		"antispam_networks": []string{"203.0.113.5", "203.0.113.5/32", "192.0.2.0/24"},
	}, headers)
	if code != http.StatusCreated {
		t.Fatalf("creating a domain = %d %v, want 201", code, body)
	}
	networks, _ := body["antispam_networks"].([]any)
	if len(networks) != 2 {
		t.Fatalf("antispam_networks = %v, want the duplicate folded into one entry", body["antispam_networks"])
	}
	if networks[0] != "203.0.113.5/32" {
		t.Errorf("antispam_networks[0] = %v, want 203.0.113.5/32", networks[0])
	}

	// A patch that says nothing about the list leaves it alone.
	code, body = do(t, srv, http.MethodPatch, "/api/v1/admin/domains/filtered.example",
		map[string]any{"description": "fronted by a filter"}, headers)
	if code != http.StatusOK {
		t.Fatalf("patching a domain = %d %v, want 200", code, body)
	}
	if networks, _ := body["antispam_networks"].([]any); len(networks) != 2 {
		t.Errorf("antispam_networks = %v after an unrelated patch, want it unchanged", body["antispam_networks"])
	}

	// An entry that is not an address or a network is refused at the API with a
	// message naming it, rather than stored and later read as "allow nothing".
	code, body = do(t, srv, http.MethodPatch, "/api/v1/admin/domains/filtered.example",
		map[string]any{"antispam_networks": []string{"192.0.2.0/33"}}, headers)
	if code != http.StatusBadRequest {
		t.Fatalf("an unparseable network = %d %v, want 400", code, body)
	}

	// Null clears the list, which means the domain is open to any source again.
	code, body = do(t, srv, http.MethodPatch, "/api/v1/admin/domains/filtered.example",
		map[string]any{"antispam_networks": nil}, headers)
	if code != http.StatusOK {
		t.Fatalf("clearing the networks = %d %v, want 200", code, body)
	}
	if networks, _ := body["antispam_networks"].([]any); len(networks) != 0 {
		t.Errorf("antispam_networks = %v, want empty", body["antispam_networks"])
	}
}

// The header rules of a domain are its own collection: they are added one at a
// time, edited, switched off, and removed.
func TestAdminDomainHeaderRuleLifecycle(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)

	const domain = "example.com"

	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/domains/"+domain+"/header-rules",
		map[string]any{"header": "X-Spam", "value": "YES", "action": "junk"}, headers)
	if code != http.StatusCreated {
		t.Fatalf("creating a rule = %d %v, want 201", code, body)
	}
	id := num(body, "id")
	if id == 0 {
		t.Fatal("the created rule has no id")
	}
	if body["match"] != "contains" {
		t.Errorf("match = %v, want the contains default", body["match"])
	}
	if body["enabled"] != true {
		t.Errorf("enabled = %v, want a rule to start enabled", body["enabled"])
	}
	if body["case_sensitive"] != false {
		t.Errorf("case_sensitive = %v, want the insensitive default", body["case_sensitive"])
	}

	code, body = do(t, srv, http.MethodGet, "/api/v1/admin/domains/"+domain+"/header-rules", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("listing rules = %d %v, want 200", code, body)
	}
	if rules, _ := body["rules"].([]any); len(rules) != 1 {
		t.Fatalf("rules = %v, want one", body["rules"])
	}

	code, body = do(t, srv, http.MethodPatch, "/api/v1/admin/domains/"+domain+"/header-rules/"+itoa(id),
		map[string]any{"action": "reject", "enabled": false}, headers)
	if code != http.StatusOK {
		t.Fatalf("patching a rule = %d %v, want 200", code, body)
	}
	if body["action"] != "reject" || body["enabled"] != false {
		t.Errorf("rule after patch = %v %v, want reject and disabled", body["action"], body["enabled"])
	}

	// A rule of one domain cannot be edited through another domain's URL.
	code, body = do(t, srv, http.MethodPatch, "/api/v1/admin/domains/other.example/header-rules/"+itoa(id),
		map[string]any{"action": "discard"}, headers)
	if code != http.StatusNotFound {
		t.Fatalf("editing through the wrong domain = %d %v, want 404", code, body)
	}

	code, _ = do(t, srv, http.MethodDelete, "/api/v1/admin/domains/"+domain+"/header-rules/"+itoa(id), nil, headers)
	if code != http.StatusNoContent {
		t.Fatalf("deleting a rule = %d, want 204", code)
	}
	code, body = do(t, srv, http.MethodDelete, "/api/v1/admin/domains/"+domain+"/header-rules/"+itoa(id), nil, headers)
	if code != http.StatusNotFound {
		t.Fatalf("deleting a rule twice = %d %v, want 404", code, body)
	}
}

// An outcome the delivery path cannot carry out is refused by name at the API,
// so the settings page never stores a rule that silently does nothing.
func TestAdminDomainHeaderRuleValidation(t *testing.T) {
	srv, db := newAdminServer(t)
	_, headers := seedAdmin(t, srv, db)

	const domain = "example.com"

	for _, bad := range []map[string]any{
		{"header": "", "value": "YES"},
		{"header": "X-Spam", "value": ""},
		{"header": "X-Spam", "value": "YES", "action": "quarantine"},
		{"header": "X-Spam", "value": "YES", "match": "sounds_like"},
		{"header": "X-Spam", "value": "   ", "match": "is"},
	} {
		code, body := do(t, srv, http.MethodPost, "/api/v1/admin/domains/"+domain+"/header-rules", bad, headers)
		if code != http.StatusBadRequest {
			t.Errorf("rule %v = %d %v, want 400", bad, code, body)
		}
	}

	// "exists" is the one match that needs no value: it asks only whether the
	// header is present.
	code, body := do(t, srv, http.MethodPost, "/api/v1/admin/domains/"+domain+"/header-rules",
		map[string]any{"header": "X-Spam-Flag", "match": "exists", "action": "discard"}, headers)
	if code != http.StatusCreated {
		t.Fatalf("an exists rule = %d %v, want 201", code, body)
	}

	// A non-numeric id is a bad request, not a missing rule.
	code, _ = do(t, srv, http.MethodPatch, "/api/v1/admin/domains/"+domain+"/header-rules/abc",
		map[string]any{"action": "junk"}, headers)
	if code != http.StatusBadRequest {
		t.Errorf("a non-numeric rule id = %d, want 400", code)
	}
}

// A rule is not something a signed-in user of a domain can change: header rules
// decide what mail reaches every mailbox in it, which is an administrator's call.
func TestAdminDomainHeaderRulesRequireAnAdministrator(t *testing.T) {
	srv, db := newAdminServer(t)
	seedUser(t, db, "user@example.com", "local", true, true)
	setPassword(t, db, "user@example.com", "correct horse battery staple")
	headers := adminToken(t, srv, "user@example.com")

	code, body := do(t, srv, http.MethodGet, "/api/v1/admin/domains/example.com/header-rules", nil, headers)
	if code != http.StatusForbidden {
		t.Fatalf("a non-admin listing header rules = %d %v, want 403", code, body)
	}
}

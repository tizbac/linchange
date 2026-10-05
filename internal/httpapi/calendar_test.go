package httpapi_test

import (
	"net/http"
	"strconv"
	"testing"
)

// The web interface reads these calendars by field name, and the model file says
// it mirrors what the server emits. Nothing checks that claim from the Go side, so
// the store types were serialised with their Go field names -- "Name", "ID" -- and
// the calendar page silently rendered nothing: no dropdown, and event fetches to
// /api/v1/calendars/undefined/events. A field that is merely absent is a field
// nothing complains about, which is what makes this worth a test.
func TestTheCalendarListMatchesTheWebInterfaceModel(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	srv := newServer(t, db)

	code, body := do(t, srv, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"login": "alice@example.com", "password": "correct horse battery staple"}, nil)
	if code != http.StatusOK {
		t.Fatalf("login = %d %v, want 200", code, body)
	}
	access, _ := body["access_token"].(string)
	auth := map[string]string{"Authorization": "Bearer " + access}

	code, created := do(t, srv, http.MethodPost, "/api/v1/calendars",
		map[string]any{"name": "Work"}, auth)
	if code != http.StatusCreated {
		t.Fatalf("creating a calendar = %d %v, want 201", code, created)
	}
	for _, field := range []string{"id", "name", "color", "timezone", "is_default", "show_free_busy"} {
		if _, present := created[field]; !present {
			t.Errorf("the created calendar has no %q: %v", field, created)
		}
	}

	code, listed := do(t, srv, http.MethodGet, "/api/v1/calendars", nil, auth)
	if code != http.StatusOK {
		t.Fatalf("listing calendars = %d %v, want 200", code, listed)
	}
	calendars, _ := listed["calendars"].([]any)
	if len(calendars) != 1 {
		t.Fatalf("got %d calendars, want 1: %v", len(calendars), listed)
	}
	cal, _ := calendars[0].(map[string]any)
	if cal["name"] != "Work" {
		t.Errorf("name = %v, want Work", cal["name"])
	}
	if cal["is_default"] != false {
		t.Errorf("is_default = %v, want false", cal["is_default"])
	}
}

// A calendar renamed from the web interface moves its label with it, so the same
// calendar does not read as one name here and another in a CalDAV client.
func TestRenamingACalendarMovesItsLabel(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	srv := newServer(t, db)

	code, body := do(t, srv, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"login": "alice@example.com", "password": "correct horse battery staple"}, nil)
	if code != http.StatusOK {
		t.Fatalf("login = %d %v, want 200", code, body)
	}
	access, _ := body["access_token"].(string)
	auth := map[string]string{"Authorization": "Bearer " + access}

	code, created := do(t, srv, http.MethodPost, "/api/v1/calendars",
		map[string]any{"name": "Work"}, auth)
	if code != http.StatusCreated {
		t.Fatalf("creating a calendar = %d %v, want 201", code, created)
	}
	id := int(created["id"].(float64))

	code, renamed := do(t, srv, http.MethodPatch, "/api/v1/calendars/"+strconv.Itoa(id),
		map[string]any{"name": "Team"}, auth)
	if code != http.StatusOK {
		t.Fatalf("renaming = %d %v, want 200", code, renamed)
	}
	if renamed["name"] != "Team" {
		t.Errorf("name = %v, want Team", renamed["name"])
	}
	if renamed["display_name"] != "Team" {
		t.Errorf("display_name = %v, want it to follow the rename", renamed["display_name"])
	}
}

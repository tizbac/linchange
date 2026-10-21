package httpapi_test

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/linchange/linchange/internal/httpapi"
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

// A personal calendar is made by the person it belongs to and then handed to
// the organization: a colleague, or a group. The group case is the one that
// matters most, because the picker offers groups by name — a share the response
// can only answer with "group 7" puts a number where the administrator is
// reading a name, and the person holding the calendar cannot tell which set of
// people they just let in.
func TestAPersonalCalendarIsSharedWithTheOrganization(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	_, adminHeaders := seedAdmin(t, srv, db)

	// The organization and the group inside it that will be shared with.
	code, wg := do(t, srv, http.MethodPost, "/api/v1/admin/workgroups",
		map[string]any{"name": "Example", "domain": "example.com"}, adminHeaders)
	if code != http.StatusCreated {
		t.Fatalf("creating the organization = %d %v, want 201", code, wg)
	}
	wgID := int(wg["id"].(float64))

	code, group := do(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/admin/workgroups/%d/groups", wgID),
		map[string]any{"name": "Sales"}, adminHeaders)
	if code != http.StatusCreated {
		t.Fatalf("creating the group = %d %v, want 201", code, group)
	}
	groupID := int(group["id"].(float64))

	alice := map[string]string{"Authorization": "Bearer " + mustField(t,
		signInToken(t, srv, "alice@example.com", "correct horse battery staple"), "access_token")}

	// The calendar itself: made by its owner through the ordinary API, which is
	// where the button on the calendar page posts.
	code, cal := do(t, srv, http.MethodPost, "/api/v1/calendars",
		map[string]any{"name": "Trips", "color": "#2563eb"}, alice)
	if code != http.StatusCreated {
		t.Fatalf("creating a personal calendar = %d %v, want 201", code, cal)
	}
	calID := int(cal["id"].(float64))
	shares := "/api/v1/calendars/" + strconv.Itoa(calID) + "/shares"

	// A colleague may be given the view of it...
	code, personShare := do(t, srv, http.MethodPost, shares, map[string]any{
		"subject_kind": "user", "subject_address": "bob@example.com", "level": "read",
	}, alice)
	if code != http.StatusCreated {
		t.Fatalf("sharing with a person = %d %v, want 201", code, personShare)
	}

	// ...and a group the right to write, named back the way it was picked.
	code, groupShare := do(t, srv, http.MethodPost, shares, map[string]any{
		"subject_kind": "group", "subject_group_id": groupID, "level": "write",
	}, alice)
	if code != http.StatusCreated {
		t.Fatalf("sharing with the group = %d %v, want 201", code, groupShare)
	}
	if got := str(groupShare, "subject_name"); got != "Sales" {
		t.Errorf("subject_name = %q, want Sales", got)
	}
	if got := str(groupShare, "level"); got != "write" {
		t.Errorf("level = %q, want write", got)
	}
	// Both shares were made by the calendar's own side, so both are offers:
	// the row exists, it is listed, and it grants nothing until the person it
	// names has said yes to it.
	if got := str(groupShare, "status"); got != "pending" {
		t.Errorf("status = %q, want pending", got)
	}

	// The owner's list says the same thing back...
	code, listed := do(t, srv, http.MethodGet, shares, nil, alice)
	if code != http.StatusOK {
		t.Fatalf("listing the shares = %d %v, want 200", code, listed)
	}
	rows, _ := listed["shares"].([]any)
	if len(rows) != 2 {
		t.Fatalf("got %d shares, want 2: %v", len(rows), listed)
	}
	named := false
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if str(row, "subject_kind") == "group" {
			named = true
			if str(row, "subject_name") != "Sales" {
				t.Errorf("the listed group share is named %q, want Sales", str(row, "subject_name"))
			}
		}
	}
	if !named {
		t.Errorf("the group share is missing from the list: %v", listed)
	}

	// ...and nothing of it is in bob's hands yet: a share made this way is an
	// offer, and it grants nothing until the person it names answers it.
	bob := map[string]string{"Authorization": "Bearer " + mustField(t,
		signInToken(t, srv, "bob@example.com", "correct horse battery staple"), "access_token")}

	offersURL := "/api/v1/calendars/offers"
	code, offers := do(t, srv, http.MethodGet, offersURL, nil, bob)
	if code != http.StatusOK {
		t.Fatalf("bob's offers = %d %v, want 200", code, offers)
	}
	offerList, _ := offers["offers"].([]any)
	if len(offerList) != 1 {
		t.Fatalf("bob was offered %d calendars, want 1: %v", len(offerList), offers)
	}
	offer, _ := offerList[0].(map[string]any)
	if got := str(offer, "calendar_name"); got != "Trips" {
		t.Errorf("the offer names the calendar %q, want Trips", got)
	}
	if got := str(offer, "calendar_owner"); got != "alice@example.com" {
		t.Errorf("the offer says it comes from %q, want alice@example.com", got)
	}
	offerID := int64(offer["id"].(float64))

	// The offer also arrived as a message in bob's own inbox. Without it the
	// owner would be left to mention in person that something on a screen
	// somewhere is waiting, which is not what sharing is supposed to ask of
	// them.
	if subjects := inboxSubjects(t, srv, bob); !hasLine(subjects,
		"Calendar shared with you: Trips") {
		t.Errorf("bob's inbox does not mention the offer: %v", subjects)
	}

	// Before the answer, the calendar is not in bob's list: there is an offer
	// about it, which is not the same as having it.
	code, bobCalendars := do(t, srv, http.MethodGet, "/api/v1/calendars", nil, bob)
	if code != http.StatusOK {
		t.Fatalf("bob's calendar list = %d %v, want 200", code, bobCalendars)
	}
	if calendarListed(bobCalendars, calID) {
		t.Errorf("the calendar bob has not accepted is already in his list: %v", bobCalendars)
	}

	// Accepting is the whole of what the offer asks: from here on the share
	// grants, and the offer is not an offer any more.
	code, accepted := do(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/calendars/offers/%d/accept", offerID), nil, bob)
	if code != http.StatusOK {
		t.Fatalf("accepting the offer = %d %v, want 200", code, accepted)
	}
	if got := str(accepted, "decision"); got != "accepted" {
		t.Errorf("decision = %q, want accepted", got)
	}
	code, offers = do(t, srv, http.MethodGet, offersURL, nil, bob)
	if code != http.StatusOK {
		t.Fatalf("bob's offers after accepting = %d %v, want 200", code, offers)
	}
	if left, _ := offers["offers"].([]any); len(left) != 0 {
		t.Errorf("the accepted offer is still waiting: %v", offers)
	}

	// And the person it names has the calendar, which is what the grant was
	// for rather than a row in a table.
	code, bobCalendars = do(t, srv, http.MethodGet, "/api/v1/calendars", nil, bob)
	if code != http.StatusOK {
		t.Fatalf("bob's calendar list = %d %v, want 200", code, bobCalendars)
	}
	found := false
	for _, raw := range bobCalendars["calendars"].([]any) {
		entry, _ := raw.(map[string]any)
		if int(entry["id"].(float64)) != calID {
			continue
		}
		found = true
		if got := str(entry, "level"); got != "read" {
			t.Errorf("bob's level on the calendar = %q, want read", got)
		}
	}
	if !found {
		t.Errorf("the shared calendar is not in bob's list: %v", bobCalendars)
	}

	// And somebody who merely holds it cannot pass it on: sharing is the
	// owner's side of the calendar.
	code, body := do(t, srv, http.MethodPost, shares, map[string]any{
		"subject_kind": "user", "subject_address": "alice@example.com", "level": "read",
	}, bob)
	if code != http.StatusForbidden {
		t.Errorf("a reader re-sharing the calendar = %d %v, want 403", code, body)
	}
}

// inboxSubjects lists the subjects sitting in the caller's INBOX.
//
// This server files its own notices there directly instead of mailing them in
// from outside, so the inbox is where a person is told that something is
// waiting for them to answer — and where a missing notice would otherwise go
// unnoticed until somebody asked in person.
func inboxSubjects(t *testing.T, srv *httpapi.Server, headers map[string]string) []string {
	t.Helper()
	folders := foldersByName(t, srv, headers)
	inbox, ok := folders["INBOX"]
	if !ok {
		t.Fatalf("the caller has no INBOX: %v", folders)
	}
	id, _ := inbox["id"].(float64)
	code, page := do(t, srv, http.MethodGet,
		"/api/v1/mail/folders/"+strconv.FormatInt(int64(id), 10)+"/messages", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("listing the inbox = %d %v, want 200", code, page)
	}
	rows, _ := page["messages"].([]any)
	out := make([]string, 0, len(rows))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		out = append(out, str(row, "subject"))
	}
	return out
}

// hasLine reports whether a list holds an exact line.
func hasLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// calendarListed reports whether a calendar list holds a calendar id.
func calendarListed(body map[string]any, id int) bool {
	rows, _ := body["calendars"].([]any)
	for _, raw := range rows {
		entry, _ := raw.(map[string]any)
		if int(entry["id"].(float64)) == id {
			return true
		}
	}
	return false
}

// bearer signs an address in and returns the header every request below sends.
func bearer(t *testing.T, srv *httpapi.Server, address string) map[string]string {
	t.Helper()
	return map[string]string{"Authorization": "Bearer " + mustField(t,
		signInToken(t, srv, address, "correct horse battery staple"), "access_token")}
}

// A calendar offered to somebody may be turned down, and turning it down is
// not a verdict for all time: they can still take it afterwards. Either way it
// grants nothing until they do, and the person it was not addressed to cannot
// answer it at all.
func TestACalendarOfferCanBeDeclinedAndAcceptedLater(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	for _, address := range []string{"alice@example.com", "bob@example.com", "carol@example.com"} {
		seedUser(t, db, address, "local", true, true)
		setPassword(t, db, address, "correct horse battery staple")
	}

	alice := bearer(t, srv, "alice@example.com")
	bob := bearer(t, srv, "bob@example.com")
	carol := bearer(t, srv, "carol@example.com")

	code, cal := do(t, srv, http.MethodPost, "/api/v1/calendars",
		map[string]any{"name": "Trips", "color": "#2563eb"}, alice)
	if code != http.StatusCreated {
		t.Fatalf("creating a personal calendar = %d %v, want 201", code, cal)
	}
	shares := "/api/v1/calendars/" + strconv.Itoa(int(cal["id"].(float64))) + "/shares"

	code, share := do(t, srv, http.MethodPost, shares, map[string]any{
		"subject_kind": "user", "subject_address": "bob@example.com", "level": "read",
	}, alice)
	if code != http.StatusCreated {
		t.Fatalf("sharing with bob = %d %v, want 201", code, share)
	}
	offerID := int64(share["id"].(float64))

	// Somebody else's offer is not Carol's to answer. The id reads as absent
	// rather than as forbidden, so that guessing ids cannot be used to find
	// out which shares exist on the server.
	code, body := do(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/calendars/offers/%d/decline", offerID), nil, carol)
	if code != http.StatusNotFound {
		t.Fatalf("declining an offer made to somebody else = %d %v, want 404", code, body)
	}

	code, declined := do(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/calendars/offers/%d/decline", offerID), nil, bob)
	if code != http.StatusOK {
		t.Fatalf("declining the offer = %d %v, want 200", code, declined)
	}
	if got := str(declined, "decision"); got != "declined" {
		t.Errorf("decision = %q, want declined", got)
	}

	// The offer stops asking, and it grants nothing.
	code, offers := do(t, srv, http.MethodGet, "/api/v1/calendars/offers", nil, bob)
	if code != http.StatusOK {
		t.Fatalf("bob's offers = %d %v, want 200", code, offers)
	}
	if left, _ := offers["offers"].([]any); len(left) != 0 {
		t.Errorf("the declined offer is still asking: %v", offers)
	}
	code, bobCalendars := do(t, srv, http.MethodGet, "/api/v1/calendars", nil, bob)
	if code != http.StatusOK {
		t.Fatalf("bob's calendar list = %d %v, want 200", code, bobCalendars)
	}
	if calendarListed(bobCalendars, int(cal["id"].(float64))) {
		t.Errorf("a declined calendar is in bob's list: %v", bobCalendars)
	}

	// On the owner's side the share is still there and still an offer: being
	// declined says nothing about whether it may be offered again.
	code, listed := do(t, srv, http.MethodGet, shares, nil, alice)
	if code != http.StatusOK {
		t.Fatalf("listing the shares = %d %v, want 200", code, listed)
	}
	rows, _ := listed["shares"].([]any)
	if len(rows) != 1 {
		t.Fatalf("the owner sees %d shares, want 1: %v", len(rows), listed)
	}
	if got := str(rows[0].(map[string]any), "status"); got != "pending" {
		t.Errorf("the declined share's status = %q, want pending", got)
	}

	// Changing their mind works, and is a decision like any other.
	code, accepted := do(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/calendars/offers/%d/accept", offerID), nil, bob)
	if code != http.StatusOK {
		t.Fatalf("accepting after declining = %d %v, want 200", code, accepted)
	}
	code, bobCalendars = do(t, srv, http.MethodGet, "/api/v1/calendars", nil, bob)
	if code != http.StatusOK {
		t.Fatalf("bob's calendar list = %d %v, want 200", code, bobCalendars)
	}
	if !calendarListed(bobCalendars, int(cal["id"].(float64))) {
		t.Errorf("the accepted calendar is not in bob's list: %v", bobCalendars)
	}
}

// A personal calendar cannot be offered to everyone, and a group belonging to
// somebody else's organization is not this calendar's to reach. The first is
// an administrator's answer; the second reads as absent rather than as
// forbidden, so that group ids cannot be probed from one organization into
// another.
func TestAPersonalCalendarRefusesTheGroupsItCannotOfferTo(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	_, adminHeaders := seedAdmin(t, srv, db)
	alice := bearer(t, srv, "alice@example.com")

	code, wg := do(t, srv, http.MethodPost, "/api/v1/admin/workgroups",
		map[string]any{"name": "Example", "domain": "example.com"}, adminHeaders)
	if code != http.StatusCreated {
		t.Fatalf("creating the organization = %d %v, want 201", code, wg)
	}
	wgID := int(wg["id"].(float64))

	// The Everyone group the schema gave the organization.
	code, groups := do(t, srv, http.MethodGet, "/api/v1/organization/groups", nil, alice)
	if code != http.StatusOK {
		t.Fatalf("the caller's groups = %d %v, want 200", code, groups)
	}
	everyoneID := int64(0)
	for _, raw := range groups["groups"].([]any) {
		group, _ := raw.(map[string]any)
		if everyone, _ := group["everyone"].(bool); everyone {
			everyoneID = int64(group["id"].(float64))
		}
	}
	if everyoneID == 0 {
		t.Fatalf("the organization has no Everyone group: %v", groups)
	}

	code, cal := do(t, srv, http.MethodPost, "/api/v1/calendars",
		map[string]any{"name": "Trips", "color": "#2563eb"}, alice)
	if code != http.StatusCreated {
		t.Fatalf("creating a personal calendar = %d %v, want 201", code, cal)
	}
	shares := "/api/v1/calendars/" + strconv.Itoa(int(cal["id"].(float64))) + "/shares"

	code, body := do(t, srv, http.MethodPost, shares, map[string]any{
		"subject_kind": "group", "subject_group_id": everyoneID, "level": "read",
	}, alice)
	if code != http.StatusBadRequest {
		t.Fatalf("sharing a personal calendar with everyone = %d %v, want 400", code, body)
	}

	// Another organization, with a group of its own that this offer would
	// have to name to reach it.
	code, other := do(t, srv, http.MethodPost, "/api/v1/admin/workgroups",
		map[string]any{"name": "Other", "domain": "other.example"}, adminHeaders)
	if code != http.StatusCreated {
		t.Fatalf("creating the second organization = %d %v, want 201", code, other)
	}
	code, otherGroups := do(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/admin/workgroups/%d/groups", int(other["id"].(float64))), nil, adminHeaders)
	if code != http.StatusOK {
		t.Fatalf("the second organization's groups = %d %v, want 200", code, otherGroups)
	}
	foreign := int64(0)
	for _, raw := range otherGroups["groups"].([]any) {
		group, _ := raw.(map[string]any)
		foreign = int64(group["id"].(float64))
		break
	}
	if foreign == 0 {
		t.Fatalf("the second organization has no group: %v", otherGroups)
	}

	code, body = do(t, srv, http.MethodPost, shares, map[string]any{
		"subject_kind": "group", "subject_group_id": foreign, "level": "read",
	}, alice)
	if code != http.StatusNotFound {
		t.Fatalf("sharing with another organization's group = %d %v, want 404", code, body)
	}

	// A group of her own organization's is offered the ordinary way, and that
	// one goes through: it is only these two that are refused.
	code, own := do(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/admin/workgroups/%d/groups", wgID),
		map[string]any{"name": "Sales"}, adminHeaders)
	if code != http.StatusCreated {
		t.Fatalf("creating a group = %d %v, want 201", code, own)
	}
	code, created := do(t, srv, http.MethodPost, shares, map[string]any{
		"subject_kind": "group", "subject_group_id": int(own["id"].(float64)), "level": "read",
	}, alice)
	if code != http.StatusCreated {
		t.Fatalf("sharing with a group of the same organization = %d %v, want 201", code, created)
	}
}

// A group share is offered to each member of the group on their own: the one
// person in the group answers for herself, and until she does, the calendar
// she is a member of the group for is not in her list.
func TestAGroupCalendarOfferReachesThePeopleInTheGroup(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	_, adminHeaders := seedAdmin(t, srv, db)
	alice := bearer(t, srv, "alice@example.com")
	bob := bearer(t, srv, "bob@example.com")

	code, wg := do(t, srv, http.MethodPost, "/api/v1/admin/workgroups",
		map[string]any{"name": "Example", "domain": "example.com"}, adminHeaders)
	if code != http.StatusCreated {
		t.Fatalf("creating the organization = %d %v, want 201", code, wg)
	}
	wgID := int(wg["id"].(float64))
	code, group := do(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/admin/workgroups/%d/groups", wgID),
		map[string]any{"name": "Sales"}, adminHeaders)
	if code != http.StatusCreated {
		t.Fatalf("creating the group = %d %v, want 201", code, group)
	}
	groupID := int(group["id"].(float64))
	code, body := do(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/admin/workgroups/%d/groups/%d/members", wgID, groupID),
		map[string]any{"address": "bob@example.com"}, adminHeaders)
	if code != http.StatusNoContent {
		t.Fatalf("adding bob to the group = %d %v, want 204", code, body)
	}

	code, cal := do(t, srv, http.MethodPost, "/api/v1/calendars",
		map[string]any{"name": "Trips", "color": "#2563eb"}, alice)
	if code != http.StatusCreated {
		t.Fatalf("creating a personal calendar = %d %v, want 201", code, cal)
	}
	calID := int(cal["id"].(float64))
	shares := "/api/v1/calendars/" + strconv.Itoa(calID) + "/shares"

	code, share := do(t, srv, http.MethodPost, shares, map[string]any{
		"subject_kind": "group", "subject_group_id": groupID, "level": "write",
	}, alice)
	if code != http.StatusCreated {
		t.Fatalf("sharing with the group = %d %v, want 201", code, share)
	}

	// The offer reaches Bob as his own, named back the way he would say the
	// group out loud rather than by its id.
	code, offers := do(t, srv, http.MethodGet, "/api/v1/calendars/offers", nil, bob)
	if code != http.StatusOK {
		t.Fatalf("bob's offers = %d %v, want 200", code, offers)
	}
	list, _ := offers["offers"].([]any)
	if len(list) != 1 {
		t.Fatalf("bob was offered %d calendars, want 1: %v", len(list), offers)
	}
	offer, _ := list[0].(map[string]any)
	if got := str(offer, "subject_name"); got != "Sales" {
		t.Errorf("the offer names the group %q, want Sales", got)
	}
	if got := str(offer, "level"); got != "write" {
		t.Errorf("the offer's level = %q, want write", got)
	}
	if got := str(offer, "calendar_name"); got != "Trips" {
		t.Errorf("the offer names the calendar %q, want Trips", got)
	}
	if subjects := inboxSubjects(t, srv, bob); !hasLine(subjects, "Calendar shared with you: Trips") {
		t.Errorf("bob's inbox does not mention the offer: %v", subjects)
	}

	code, accepted := do(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/calendars/offers/%d/accept",
			int64(offer["id"].(float64))), nil, bob)
	if code != http.StatusOK {
		t.Fatalf("accepting the group offer = %d %v, want 200", code, accepted)
	}

	code, bobCalendars := do(t, srv, http.MethodGet, "/api/v1/calendars", nil, bob)
	if code != http.StatusOK {
		t.Fatalf("bob's calendar list = %d %v, want 200", code, bobCalendars)
	}
	found := false
	for _, raw := range bobCalendars["calendars"].([]any) {
		entry, _ := raw.(map[string]any)
		if int(entry["id"].(float64)) != calID {
			continue
		}
		found = true
		if got := str(entry, "level"); got != "write" {
			t.Errorf("bob's level on the calendar = %q, want write", got)
		}
	}
	if !found {
		t.Errorf("the group-shared calendar is not in bob's list: %v", bobCalendars)
	}
}

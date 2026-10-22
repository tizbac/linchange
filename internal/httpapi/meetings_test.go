package httpapi_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/httpapi"
	"github.com/linchange/linchange/internal/imapimport"
)

// newMeetingsServer builds a server whose blob store the test also holds, so
// an invitation queued by the API can be read back as the bytes a mail server
// would have received.
func newMeetingsServer(t *testing.T, db *sqlx.DB) (*httpapi.Server, *blobstore.Store) {
	t.Helper()
	blobs := testBlobs(t)
	cfg := testConfig()
	srv, err := httpapi.New(httpapi.Options{
		DB: db, Config: cfg, Blobs: blobs,
		Imports: imapimport.New(db, blobs, nil, cfg.Auth.EncryptionKey),
	})
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	return srv, blobs
}

// outboxMessage is one queued message with the bytes it was queued as.
type outboxMessage struct {
	addresses []string
	raw       string
}

// queuedMessages reads the outbox back as raw messages. The delivery pool is
// not running in a test, so everything Send queued is still there - which is
// exactly what makes the outbox the place to inspect what would have gone
// out.
func queuedMessages(t *testing.T, db *sqlx.DB, blobs *blobstore.Store) []outboxMessage {
	t.Helper()
	rows := []struct {
		SHA  []byte `db:"blob_sha256"`
		Addr string `db:"address"`
	}{}
	err := db.Select(&rows, `
		SELECT o.blob_sha256, r.address
		FROM outbox o
		JOIN outbox_recipients r ON r.outbox_id = o.id
		ORDER BY o.id, r.position`)
	if err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}

	bySHA := map[string][]string{}
	var order []string
	for _, row := range rows {
		sha := string(row.SHA)
		if _, seen := bySHA[sha]; !seen {
			order = append(order, sha)
		}
		bySHA[sha] = append(bySHA[sha], row.Addr)
	}

	var out []outboxMessage
	for _, sha := range order {
		if len(sha) != 32 {
			t.Fatalf("blob_sha256 is %d bytes, want a 32-byte digest", len(sha))
		}
		var digest blobstore.Digest
		copy(digest[:], sha)
		raw, err := blobs.Get(digest)
		if err != nil {
			t.Fatalf("reading blob: %v", err)
		}
		out = append(out, outboxMessage{addresses: bySHA[sha], raw: string(raw)})
	}
	return out
}

// decodeQP undoes quoted-printable encoding well enough to search a message:
// soft line wrapping is rejoined, and the escaped equals signs that keep a URL
// like "to=..." from looking like an escape are unescaped. The result is what
// a mail client would show the reader.
func decodeQP(s string) string {
	r := strings.NewReplacer("=\r\n", "", "=3D", "=")
	return r.Replace(s)
}

var inviteLinkRE = regexp.MustCompile(`meetings/[0-9a-f]{32}@linchange\?to=[^&\s]+&token=([0-9a-f]{32})`)

// TestMeetingsLifecycle drives one meeting end to end through the REST API:
// creation queues one invitation per guest with the right link, the external
// guest answers through their token without a session, the local guest answers
// through theirs with one, a resend queues the same links again, and a
// cancellation queues a CANCEL.
func TestMeetingsLifecycle(t *testing.T) {
	db := testMigratedDB(t)
	srv, blobs := newMeetingsServer(t, db)

	seedUser(t, db, "alice@example.com", "local", true, true)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	auth := adminToken(t, srv, "alice@example.com")

	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	code, created := do(t, srv, "POST", "/api/v1/meetings", map[string]any{
		"title":       "Design review",
		"description": "Bring the mocks",
		"start":       start,
		"end":         start.Add(time.Hour),
		"timezone":    "Europe/Berlin",
		"attendees": []map[string]string{
			{"address": "Bob <bob@example.com>"},
			{"address": "carol@outside.example"},
		},
	}, auth)
	if code != 201 {
		t.Fatalf("create = %d %v, want 201", code, created)
	}

	uid := mustField(t, created, "uid")
	if !regexp.MustCompile(`^[0-9a-f]{32}@linchange$`).MatchString(uid) {
		t.Fatalf("uid = %q, want 32 hex digits and the suffix", uid)
	}
	if !strings.HasSuffix(str(created, "joinUrl"), "/meetings/"+uid) {
		t.Errorf("joinUrl = %q, want it to end in the meeting's page", str(created, "joinUrl"))
	}
	if got := str(created, "title"); got != "Design review" {
		t.Errorf("title = %q", got)
	}
	if got := str(created, "timezone"); got != "Europe/Berlin" {
		t.Errorf("timezone = %q", got)
	}
	if inviteFailures, _ := created["inviteFailures"].([]any); len(inviteFailures) != 0 {
		t.Errorf("inviteFailures = %v, want none", created["inviteFailures"])
	}

	// The guest list is the event's guest list: two guests, the organiser not
	// among them, everybody asked to answer, and only the local guest carrying
	// a chat identity.
	attendees := attendeesOf(t, created)
	if len(attendees) != 2 {
		t.Fatalf("attendees = %d, want 2: %v", len(attendees), created["attendees"])
	}
	bob := attendeeNamed(t, attendees, "bob@example.com")
	if bob["partStat"] != "NEEDS-ACTION" || bob["rsvp"] != true || bob["local"] != true {
		t.Errorf("bob = %v, want NEEDS-ACTION, rsvp, local", bob)
	}
	if bob["displayName"] != "Bob" {
		t.Errorf("bob's name = %v, want the name from the address", bob["displayName"])
	}
	carol := attendeeNamed(t, attendees, "carol@outside.example")
	if carol["local"] != false {
		t.Errorf("carol = %v, want no chat identity for an external address", carol)
	}
	for _, a := range attendees {
		if a["address"] == "alice@example.com" {
			t.Errorf("the organiser is on their own guest list: %v", attendees)
		}
	}

	// One message per guest: the reply link is personal, so one message for
	// the list would give every guest everybody else's link.
	messages := queuedMessages(t, db, blobs)
	if len(messages) != 2 {
		t.Fatalf("queued messages = %d, want one per guest", len(messages))
	}

	var carolMessage string
	for _, m := range messages {
		if len(m.addresses) != 1 {
			t.Fatalf("a message has %d recipients, want 1: %v", len(m.addresses), m.addresses)
		}
		body := decodeQP(m.raw)
		// The two declarations of the iTIP method: the part's parameter,
		// which is what a receiving client reads to decide whether to show
		// an answer prompt, and the METHOD in the document itself.
		if !strings.Contains(body, "method=REQUEST") {
			t.Errorf("invitation for %v does not declare method=REQUEST:\n%s",
				m.addresses, firstLines(body, 40))
		}
		if !strings.Contains(body, "METHOD:REQUEST") {
			t.Errorf("invitation for %v carries no iTIP REQUEST document", m.addresses)
		}
		if !strings.Contains(body, "Subject: Invitation: Design review") {
			t.Errorf("invitation for %v has the wrong subject:\n%s", m.addresses, firstLines(body, 8))
		}
		if !strings.Contains(body, "invitation.ics") {
			t.Errorf("invitation for %v carries no invitation file:\n%s",
				m.addresses, firstLines(body, 80))
		}
		if strings.Contains(body, "\r\nBcc:") {
			t.Errorf("invitation for %v leaks a Bcc header", m.addresses)
		}
		if m.addresses[0] == "carol@outside.example" {
			carolMessage = body
		}
	}
	if carolMessage == "" {
		t.Fatal("no message was queued for the external guest")
	}

	// The link a guest follows answers two questions at once - which meeting,
	// and who is arriving - so the token and the address travel together.
	match := inviteLinkRE.FindStringSubmatch(carolMessage)
	if match == nil {
		t.Fatalf("carol's invitation carries no reply link:\n%s", firstLines(carolMessage, 60))
	}
	token := match[1]

	// An invitee with no session reads their meeting through the token.
	code, view := do(t, srv, "GET",
		"/api/v1/meeting-invites/"+uid+"?to=carol%40outside.example&token="+token, nil, nil)
	if code != 200 {
		t.Fatalf("invite view = %d %v, want 200", code, view)
	}
	if str(view, "title") != "Design review" {
		t.Errorf("invite view title = %q", str(view, "title"))
	}
	if str(view, "chatConversation") == "" {
		t.Errorf("invite view did not include the chat room id for a token holder")
	}

	// The same token unlocks the guest's chat history and ICE configuration.
	// This is what lets an accountless invitee join the call and the room
	// without signing in.
	code, history := do(t, srv, "GET",
		"/api/v1/meeting-invites/"+uid+"/chat?to=carol%40outside.example&token="+token, nil, nil)
	if code != 200 {
		t.Errorf("guest chat history = %d %v, want 200", code, history)
	}
	code, ice := do(t, srv, "GET",
		"/api/v1/meeting-invites/"+uid+"/ice?to=carol%40outside.example&token="+token, nil, nil)
	if code != 200 {
		t.Errorf("guest ICE configuration = %d %v, want 200", code, ice)
	}

	// The same link with the wrong answer to either half is a 404, not a
	// lesson about which half was wrong.
	if code, _ := do(t, srv, "GET",
		"/api/v1/meeting-invites/"+uid+"?to=carol%40outside.example&token="+strings.Repeat("0", 32), nil, nil); code != 404 {
		t.Errorf("wrong token = %d, want 404", code)
	}
	if code, _ := do(t, srv, "GET",
		"/api/v1/meeting-invites/"+uid+"?to=eve%40outside.example&token="+token, nil, nil); code != 404 {
		t.Errorf("wrong guest = %d, want 404", code)
	}
	if code, _ := do(t, srv, "GET",
		"/api/v1/meeting-invites/"+uid+"?token="+token, nil, nil); code != 404 {
		t.Errorf("missing guest = %d, want 404", code)
	}

	// The external guest answers with the token and nothing else.
	code, answer := do(t, srv, "POST", "/api/v1/meeting-invites/"+uid+"/rsvp", map[string]string{
		"to":       "carol@outside.example",
		"token":    token,
		"partStat": "ACCEPTED",
	}, nil)
	if code != 200 {
		t.Fatalf("invite rsvp = %d %v, want 200", code, answer)
	}
	if str(answer, "partStat") != "ACCEPTED" {
		t.Errorf("invite rsvp answered %q", str(answer, "partStat"))
	}
	if got := partStatOf(t, db, uid, "carol@outside.example"); got != "ACCEPTED" {
		t.Errorf("carol's answer = %q, want ACCEPTED", got)
	}

	// A token does not travel with the body: a wrong pair changes nothing.
	code, _ = do(t, srv, "POST", "/api/v1/meeting-invites/"+uid+"/rsvp", map[string]string{
		"to":       "carol@outside.example",
		"token":    strings.Repeat("0", 32),
		"partStat": "DECLINED",
	}, nil)
	if code != 404 {
		t.Errorf("rsvp with a wrong token = %d, want 404", code)
	}
	if got := partStatOf(t, db, uid, "carol@outside.example"); got != "ACCEPTED" {
		t.Errorf("carol's answer changed to %q on a failed token", got)
	}

	// A local guest answers through their session.
	bobAuth := adminToken(t, srv, "bob@example.com")
	code, _ = do(t, srv, "POST", "/api/v1/meetings/"+uid+"/rsvp",
		map[string]string{"partStat": "DECLINED"}, bobAuth)
	if code != 200 {
		t.Fatalf("bob's rsvp = %d, want 200", code)
	}
	if got := partStatOf(t, db, uid, "bob@example.com"); got != "DECLINED" {
		t.Errorf("bob's answer = %q, want DECLINED", got)
	}

	// Somebody who is not on the guest list cannot answer at all - not even
	// the organiser, who is not on their own list.
	if code, body := do(t, srv, "POST", "/api/v1/meetings/"+uid+"/rsvp",
		map[string]string{"partStat": "ACCEPTED"}, auth); code != 403 {
		t.Errorf("organiser rsvp = %d %v, want 403", code, body)
	}

	// The list shows the meeting once, with the answer tallies.
	code, listing := do(t, srv, "GET", "/api/v1/meetings/", nil, auth)
	if code != 200 {
		t.Fatalf("list = %d %v, want 200", code, listing)
	}
	items, _ := listing["meetings"].([]any)
	if len(items) != 1 {
		t.Fatalf("meetings = %v, want one", listing["meetings"])
	}
	item, _ := items[0].(map[string]any)
	if str(item, "role") != "organizer" {
		t.Errorf("role = %q, want organizer", str(item, "role"))
	}
	if got, _ := item["attendeeCount"].(float64); got != 2 {
		t.Errorf("attendeeCount = %v, want 2", item["attendeeCount"])
	}
	if got, _ := item["acceptedCount"].(float64); got != 1 {
		t.Errorf("acceptedCount = %v, want 1 (carol)", item["acceptedCount"])
	}
	if str(item, "myPartStat") != "" {
		t.Errorf("myPartStat for an organiser = %q, want empty", str(item, "myPartStat"))
	}

	// A resend queues the same links again: the guest who already answered
	// keeps the identity they answered with.
	code, resent := do(t, srv, "POST", "/api/v1/meetings/"+uid+"/resend", nil, auth)
	if code != 200 {
		t.Fatalf("resend = %d %v, want 200", code, resent)
	}
	if got, _ := resent["sent"].(float64); got != 2 {
		t.Errorf("resent = %v, want 2", resent["sent"])
	}
	messages = queuedMessages(t, db, blobs)
	if len(messages) != 4 {
		t.Fatalf("queued after resend = %d, want 4", len(messages))
	}
	for _, m := range messages[2:] {
		if !inviteLinkRE.MatchString(decodeQP(m.raw)) {
			t.Errorf("the resent message for %v carries a different link", m.addresses)
		}
	}

	// Chat is native, so there is no per-deployment chat session to mint:
	// the room is opened by the meeting itself and reached over the chat
	// socket.

	// Cancellation keeps the row, mails everybody one CANCEL, and closes the
	// meeting against further work.
	code, cancelled := do(t, srv, "POST", "/api/v1/meetings/"+uid+"/cancel", nil, auth)
	if code != 200 {
		t.Fatalf("cancel = %d %v, want 200", code, cancelled)
	}
	if cancelled["cancelled"] != true {
		t.Errorf("cancelled = %v, want true", cancelled["cancelled"])
	}
	messages = queuedMessages(t, db, blobs)
	if len(messages) != 5 {
		t.Fatalf("queued after cancel = %d, want 5 (one cancellation to two guests)", len(messages))
	}
	cancel := decodeQP(messages[4].raw)
	if !strings.Contains(cancel, "method=CANCEL") || !strings.Contains(cancel, "METHOD:CANCEL") {
		t.Errorf("the cancellation does not declare METHOD:CANCEL:\n%s", firstLines(cancel, 40))
	}
	if !strings.Contains(cancel, "Subject: ") || !strings.Contains(strings.ToLower(cancel), "cancelled") {
		t.Errorf("the cancellation message does not read like one:\n%s", firstLines(cancel, 20))
	}
	if len(messages[4].addresses) != 2 {
		t.Errorf("the cancellation went to %v, want both guests", messages[4].addresses)
	}

	if code, body := do(t, srv, "POST", "/api/v1/meetings/"+uid+"/resend", nil, auth); code != 400 {
		t.Errorf("resend of a cancelled meeting = %d %v, want 400", code, body)
	}
}

// TestMeetingUIDEncoding: a meeting UID is an event UID, so it carries an
// "@", and a client that encodes its path segments sends that as %40. Both
// spellings must reach the same meeting, or the browser - which builds URLs
// out of its own path parameters - 404s on rooms it created seconds ago.
// TestMeetingExpiryAndListPagination pins the two rules the meeting list
// depends on: an expired room is hidden unless asked for, and the list is a
// page whose total describes the whole filtered set.
func TestMeetingExpiryAndListPagination(t *testing.T) {
	db := testMigratedDB(t)
	srv, _ := newMeetingsServer(t, db)

	seedUser(t, db, "alice@example.com", "local", true, true)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	alice := adminToken(t, srv, "alice@example.com")
	bob := adminToken(t, srv, "bob@example.com")

	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	create := func(title string, expiry int) map[string]any {
		body := map[string]any{
			"title":    title,
			"start":    start,
			"end":      start.Add(time.Hour),
			"timezone": "UTC",
			"attendees": []map[string]string{
				{"address": "bob@example.com"},
			},
		}
		if expiry != 0 {
			body["expiryHours"] = expiry
		}
		code, created := do(t, srv, "POST", "/api/v1/meetings", body, alice)
		if code != 201 {
			t.Fatalf("creating %s = %d %v", title, code, created)
		}
		return created
	}

	first := create("Expiring", 2)
	if first["expired"] == true {
		t.Errorf("a new meeting is already expired: %v", first)
	}
	if str(first, "expiresAt") == "" {
		t.Errorf("created meeting has no expiresAt: %v", first)
	}
	uid := mustField(t, first, "uid")
	create("Still live", 0)

	// A lifetime beyond the maximum is a client error, not a silently clamped
	// room. The default (zero) is valid and was exercised above.
	code, body := do(t, srv, "POST", "/api/v1/meetings", map[string]any{
		"title":       "Too long",
		"start":       start,
		"end":         start.Add(time.Hour),
		"expiryHours": 25,
		"attendees": []map[string]string{
			{"address": "bob@example.com"},
		},
	}, alice)
	if code != 400 {
		t.Fatalf("expiryHours=25 = %d %v, want 400", code, body)
	}

	// Move the first meeting's expiry into the past. The row stays; only the
	// capability is gone.
	if _, err := db.Exec(`UPDATE meetings SET expires_at = now() - interval '1 hour' WHERE uid = $1`, uid); err != nil {
		t.Fatalf("expiring the meeting: %v", err)
	}

	code, list := do(t, srv, "GET", "/api/v1/meetings", nil, alice)
	if code != 200 {
		t.Fatalf("listing meetings = %d %v", code, list)
	}
	if total, _ := list["total"].(float64); total != 1 {
		t.Errorf("default list total = %v, want 1 (expired hidden): %v", list["total"], list)
	}
	for _, raw := range list["meetings"].([]any) {
		item := raw.(map[string]any)
		if item["uid"] == uid {
			t.Errorf("an expired meeting appeared in the default list: %v", item)
		}
	}

	code, list = do(t, srv, "GET", "/api/v1/meetings?include_expired=1", nil, alice)
	if code != 200 {
		t.Fatalf("listing with expired = %d %v", code, list)
	}
	if total, _ := list["total"].(float64); total != 2 {
		t.Errorf("include_expired total = %v, want 2: %v", list["total"], list)
	}

	// The same filter drifts into pages without changing the total or
	// repeating a row.
	code, page1 := do(t, srv, "GET", "/api/v1/meetings?include_expired=1&limit=1&offset=0", nil, alice)
	if code != 200 {
		t.Fatalf("first page = %d %v", code, page1)
	}
	code, page2 := do(t, srv, "GET", "/api/v1/meetings?include_expired=1&limit=1&offset=1", nil, alice)
	if code != 200 {
		t.Fatalf("second page = %d %v", code, page2)
	}
	p1 := page1["meetings"].([]any)
	p2 := page2["meetings"].([]any)
	if len(p1) != 1 || len(p2) != 1 {
		t.Fatalf("pages returned %d and %d rows, want 1 each", len(p1), len(p2))
	}
	if p1[0].(map[string]any)["uid"] == p2[0].(map[string]any)["uid"] {
		t.Errorf("offset=1 repeated the first page's row")
	}
	if total, _ := page2["total"].(float64); total != 2 {
		t.Errorf("paged total = %v, want 2", page2["total"])
	}

	// The organizer still sees the expired record, marked expired.
	code, view := do(t, srv, "GET", "/api/v1/meetings/"+uid, nil, alice)
	if code != 200 {
		t.Fatalf("reading an expired meeting = %d %v", code, view)
	}
	if view["expired"] != true {
		t.Errorf("expired view = %v, want expired:true", view)
	}

	// Answering and opening the invitation link are refused with 410 once the
	// lifetime has passed.
	code, _ = do(t, srv, "POST", "/api/v1/meetings/"+uid+"/rsvp",
		map[string]any{"partStat": "ACCEPTED"}, bob)
	if code != 410 {
		t.Errorf("RSVP on an expired meeting = %d, want 410", code)
	}
	var token string
	if err := db.Get(&token, `
		SELECT i.rsvp_token FROM meeting_invites i
		JOIN meetings m ON m.id = i.meeting_id
		WHERE m.uid = $1 AND i.address = 'bob@example.com'`, uid); err != nil {
		t.Fatalf("reading the invitation token: %v", err)
	}
	code, _ = do(t, srv, "GET",
		"/api/v1/meeting-invites/"+uid+"?to=bob@example.com&token="+token, nil, nil)
	if code != 410 {
		t.Errorf("expired invitation view = %d, want 410", code)
	}
}

// TestAcceptedMeetingAppearsOnGuestCalendar is the promise that accepting an
// invitation puts it on your own calendar. The organiser's event is not the
// guest's event: without a copy in the guest's default calendar, a signed-in
// guest sees the meeting in the meetings list but nowhere in their calendar.
func TestAcceptedMeetingAppearsOnGuestCalendar(t *testing.T) {
	db := testMigratedDB(t)
	srv, _ := newMeetingsServer(t, db)

	seedUser(t, db, "alice@example.com", "local", true, true)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	alice := adminToken(t, srv, "alice@example.com")
	bob := adminToken(t, srv, "bob@example.com")

	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	code, created := do(t, srv, "POST", "/api/v1/meetings", map[string]any{
		"title":    "Calendar copy",
		"start":    start,
		"end":      start.Add(time.Hour),
		"timezone": "UTC",
		"attendees": []map[string]string{
			{"address": "bob@example.com"},
		},
	}, alice)
	if code != 201 {
		t.Fatalf("create = %d %v", code, created)
	}
	uid := mustField(t, created, "uid")

	count := func() int {
		t.Helper()
		var n int
		if err := db.Get(&n, `
			SELECT count(*) FROM events e
			JOIN calendars c ON c.id = e.calendar_id
			WHERE c.owner = 'bob@example.com' AND e.uid = $1`, uid); err != nil {
			t.Fatalf("counting the guest's calendar: %v", err)
		}
		return n
	}
	if n := count(); n != 0 {
		t.Fatalf("guest calendar already has %d copies before answering", n)
	}

	code, _ = do(t, srv, "POST", "/api/v1/meetings/"+uid+"/rsvp",
		map[string]any{"partStat": "ACCEPTED"}, bob)
	if code != 200 {
		t.Fatalf("accepting = %d, want 200", code)
	}
	if n := count(); n != 1 {
		t.Errorf("accepted meeting produced %d calendar copies, want 1", n)
	}

	// Declining removes it again: an invitation you said no to is not a
	// booking on your calendar.
	code, _ = do(t, srv, "POST", "/api/v1/meetings/"+uid+"/rsvp",
		map[string]any{"partStat": "DECLINED"}, bob)
	if code != 200 {
		t.Fatalf("declining = %d, want 200", code)
	}
	if n := count(); n != 0 {
		t.Errorf("declined meeting left %d calendar copies, want 0", n)
	}
}

func TestMeetingUIDEncoding(t *testing.T) {
	db := testMigratedDB(t)
	srv, _ := newMeetingsServer(t, db)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	auth := adminToken(t, srv, "alice@example.com")

	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	code, created := do(t, srv, "POST", "/api/v1/meetings", map[string]any{
		"title":     "Encoded uid",
		"start":     start,
		"end":       start.Add(time.Hour),
		"attendees": []map[string]string{{"address": "carol@outside.example"}},
	}, auth)
	if code != 201 {
		t.Fatalf("create = %d %v, want 201", code, created)
	}
	uid := mustField(t, created, "uid")

	for _, path := range []string{
		"/api/v1/meetings/" + uid,
		"/api/v1/meetings/" + strings.Replace(uid, "@", "%40", 1),
	} {
		status, view := do(t, srv, "GET", path, nil, auth)
		if status != 200 {
			t.Fatalf("GET %s = %d %v, want 200", path, status, view)
		}
		if mustField(t, view, "title") != "Encoded uid" {
			t.Errorf("GET %s returned the wrong meeting", path)
		}
	}
}

// TestMeetingCreateValidation checks the refusals that must happen before
// anything is written.
func TestMeetingCreateValidation(t *testing.T) {
	db := testMigratedDB(t)
	srv, _ := newMeetingsServer(t, db)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	auth := adminToken(t, srv, "alice@example.com")

	start := time.Now().UTC().Add(time.Hour)
	cases := []struct {
		name  string
		draft map[string]any
	}{
		{"no guests", map[string]any{
			"title": "Solo", "start": start, "end": start.Add(time.Hour),
			"attendees": []map[string]string{},
		}},
		{"no title", map[string]any{
			"start": start, "end": start.Add(time.Hour),
			"attendees": []map[string]string{{"address": "bob@example.com"}},
		}},
		{"ends before it starts", map[string]any{
			"title": "Backwards", "start": start, "end": start.Add(-time.Hour),
			"attendees": []map[string]string{{"address": "bob@example.com"}},
		}},
		{"unparsable guest", map[string]any{
			"title": "Nope", "start": start, "end": start.Add(time.Hour),
			"attendees": []map[string]string{{"address": "not even close"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := do(t, srv, "POST", "/api/v1/meetings", tc.draft, auth); code != 400 {
				t.Errorf("create = %d %v, want 400", code, body)
			}
		})
	}

	// Nothing was queued for any of them: a refused meeting costs no mail.
	var count int
	if err := db.Get(&count, `SELECT count(*) FROM outbox`); err != nil {
		t.Fatalf("counting the outbox: %v", err)
	}
	if count != 0 {
		t.Errorf("queued messages = %d, want 0", count)
	}

	// An unknown field is a typo the server must not silently drop: a meeting
	// created from a half-read request is the wrong meeting.
	if code, body := do(t, srv, "POST", "/api/v1/meetings", map[string]any{
		"title": "Typo", "start": start, "end": start.Add(time.Hour),
		"attendees": []map[string]string{{"address": "bob@example.com"}},
		"atendees":  []string{"typo@example.com"},
	}, auth); code != 400 {
		t.Errorf("create with a misspelled field = %d %v, want 400", code, body)
	}
}

// attendeesOf reads the attendee list out of a meeting response.
func attendeesOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["attendees"].([]any)
	if !ok {
		t.Fatalf("response has no attendees: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, a := range raw {
		m, ok := a.(map[string]any)
		if !ok {
			t.Fatalf("attendee = %v, want an object", a)
		}
		out = append(out, m)
	}
	return out
}

func attendeeNamed(t *testing.T, attendees []map[string]any, address string) map[string]any {
	t.Helper()
	for _, a := range attendees {
		if str(a, "address") == address {
			return a
		}
	}
	t.Fatalf("no attendee %q in %v", address, attendees)
	return nil
}

// partStatOf reads one guest's answer straight from the guest list table.
func partStatOf(t *testing.T, db *sqlx.DB, uid, address string) string {
	t.Helper()
	var partstat string
	err := db.Get(&partstat, `
		SELECT a.partstat
		FROM attendees a
		JOIN events e ON e.id = a.event_id
		WHERE e.uid = $1 AND a.address = $2`, uid, address)
	if err != nil {
		t.Fatalf("reading %s's answer: %v", address, err)
	}
	return partstat
}

// firstLines returns the first n lines of a message, for failure output that
// shows the part that matters instead of the whole multipart document.
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n") + "\n..."
}

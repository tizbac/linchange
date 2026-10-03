package calendar_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/recurrence"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

func db(t *testing.T) *sqlx.DB { return testdb.Migrated(t) }

// principals is the cast of a calendar test: two colleagues, a closed group
// between them and an outsider.
type cast struct {
	alice    policy.Principal
	bob      policy.Principal
	carol    policy.Principal
	aliceCal int64
	bobCal   int64
	group    int64
	svc      *calendar.Service
}

func principal(address, name string) policy.Principal {
	return policy.Principal{
		Address: address, Source: policy.SourceLocal, Enabled: true,
		MailboxEnabled: true, Authenticated: true,
	}
}

func seed(t *testing.T, database *sqlx.DB) cast {
	t.Helper()
	ctx := context.Background()

	if err := store.NewDomains().Create(ctx, database, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("seeding domain: %v", err)
	}
	users := store.NewUsers()
	for _, p := range []policy.Principal{
		principal("alice@example.com", "Alice"),
		principal("bob@example.com", "Bob"),
		principal("carol@example.com", "Carol"),
	} {
		if err := users.Create(ctx, database, store.CreateUserRequest{
			Address: p.Address, Source: p.Source,
		}); err != nil {
			t.Fatalf("seeding %s: %v", p.Address, err)
		}
	}

	calendars := store.NewCalendars()
	aliceCal, err := calendars.Create(ctx, database, store.CreateCalendarRequest{
		Owner: ptr("alice@example.com"), Name: "Calendar", Timezone: "Europe/London", IsDefault: true,
	})
	if err != nil {
		t.Fatalf("seeding alice's calendar: %v", err)
	}
	bobCal, err := calendars.Create(ctx, database, store.CreateCalendarRequest{
		Owner: ptr("bob@example.com"), Name: "Calendar", IsDefault: true,
	})
	if err != nil {
		t.Fatalf("seeding bob's calendar: %v", err)
	}

	group, err := store.NewWorkgroups().Create(ctx, database, store.CreateWorkgroupRequest{
		Name: "support", Domain: "example.com", DisplayName: "Support",
		OpenBooking: ptr(false),
		Members:     []store.WorkgroupMember{{Address: "alice@example.com", Role: store.WorkgroupRoleAdmin}},
	})
	if err != nil {
		t.Fatalf("seeding workgroup: %v", err)
	}

	return cast{
		alice: principal("alice@example.com", "Alice"),
		bob:   principal("bob@example.com", "Bob"),
		carol: principal("carol@example.com", "Carol"),
		// The workgroup is closed, so booking it tests the refusal path.
		aliceCal: aliceCal.ID, bobCal: bobCal.ID, group: group.ID,
		svc: calendar.NewService(calendars, store.NewEvents(), store.NewWorkgroups()),
	}
}

func ptr[T any](v T) *T { return &v }

// createMeeting is the ordinary local creation: no scheduling message is sent,
// because a meeting saved without inviting anybody is a legitimate thing to want.
func createMeeting(t *testing.T, s *calendar.Service, database *sqlx.DB, who policy.Principal, calID int64, ev ical.Event) store.Event {
	t.Helper()
	got, err := s.Create(context.Background(), database, who, calendar.Draft{
		CalendarID: calID, Event: ev,
	})
	if err != nil {
		t.Fatalf("creating event: %v", err)
	}
	return got
}

func baseEvent(summary string, start time.Time, attendees ...string) ical.Event {
	ev := ical.Event{
		Summary: summary,
		Start:   start, End: start.Add(time.Hour),
		DTStamp: start.Add(-24 * time.Hour),
	}
	for _, a := range attendees {
		ev.Attendees = append(ev.Attendees, ical.Attendee{
			Address: a, Role: ical.RoleRequired,
			PartStat: ical.PartStatNeedsAction, RSVP: true,
		})
	}
	return ev
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// The server fills in what a client must not choose for itself: a UID nobody can
// collide, an organizer, a timestamp and a revision. An invitation missing any of
// these cannot be cancelled by anyone.
func TestCreateFillsInWhatTheClientMustNotChoose(t *testing.T) {
	database := db(t)
	c := seed(t, database)

	got := createMeeting(t, c.svc, database, c.alice, c.aliceCal, baseEvent("Standup", utc(2026, 3, 10, 9, 0)))

	if got.UID == "" {
		t.Error("no UID was generated")
	}
	if got.OrganizerAddress != c.alice.Address {
		t.Errorf("organizer = %q, want the creator", got.OrganizerAddress)
	}
	if got.DTStamp.IsZero() {
		t.Error("no DTSTAMP was set")
	}
	if got.Sequence != 1 {
		t.Errorf("sequence = %d, want a first revision of 1", got.Sequence)
	}
	if got.Status != ical.StatusConfirmed {
		t.Errorf("status = %q, want a confirmed default", got.Status)
	}
	if got.ShowAs != ical.ShowAsBusy {
		t.Errorf("show_as = %q, want busy", got.ShowAs)
	}
	// The calendar's zone becomes the event's, so a London user's afternoon
	// meeting is not filed as the same instant in UTC.
	if got.Timezone != "Europe/London" {
		t.Errorf("timezone = %q, want the calendar's", got.Timezone)
	}
	if len(got.ICalBlob) == 0 {
		t.Error("no document was stored")
	}
}

// Two events must not share a UID, or they are the same meeting as far as every
// client and every scheduling message is concerned.
func TestGeneratedUIDsAreUnique(t *testing.T) {
	database := db(t)
	c := seed(t, database)

	var seen = map[string]bool{}
	for i := 0; i < 20; i++ {
		got := createMeeting(t, c.svc, database, c.alice, c.aliceCal,
			baseEvent("Meeting", utc(2026, 3, 10, 9, 0)))
		if seen[got.UID] {
			t.Fatalf("UID %q was generated twice", got.UID)
		}
		seen[got.UID] = true
	}
}

// A draft with no calendar lands on the default one, which is provisioned on
// demand so a brand-new account can make a booking without a setup step.
func TestCreateWithoutACalendarUsesTheDefault(t *testing.T) {
	database := db(t)
	c := seed(t, database)

	// A fourth person with no calendars at all.
	if err := store.NewUsers().Create(context.Background(), database, store.CreateUserRequest{
		Address: "dave@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("seeding dave: %v", err)
	}
	dave := principal("dave@example.com", "Dave")

	got := createMeeting(t, c.svc, database, dave, 0, baseEvent("First", utc(2026, 3, 10, 9, 0)))
	if got.CalendarID == 0 {
		t.Fatal("no calendar was chosen")
	}
	cal, err := store.NewCalendars().Get(context.Background(), database, got.CalendarID)
	if err != nil {
		t.Fatalf("loading the chosen calendar: %v", err)
	}
	if cal.Owner == nil || *cal.Owner != dave.Address {
		t.Errorf("calendar owner = %v, want dave", cal.Owner)
	}
	if !cal.IsDefault {
		t.Error("the provisioned calendar should be the default one")
	}
}

// Somebody else's calendar is refused rather than quietly used, because writing
// to it would hand over the meeting's ownership.
func TestCreateOnSomebodyElsesCalendarIsDenied(t *testing.T) {
	database := db(t)
	c := seed(t, database)

	_, err := c.svc.Create(context.Background(), database, c.bob, calendar.Draft{
		CalendarID: c.aliceCal, Event: baseEvent("Trespass", utc(2026, 3, 10, 9, 0)),
	})
	if !errors.Is(err, calendar.ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
	if policy.CodeOf(err) != policy.CodeNotCalendarOwner {
		t.Errorf("code = %q, want %q", policy.CodeOf(err), policy.CodeNotCalendarOwner)
	}
}

// Booking rights and administrative rights are different, and the difference is
// visible here: an outsider may put a meeting in an open team calendar but may
// not touch the personal calendar next to it.
func TestBookingAWorkgroupCalendarVersusWritingToOne(t *testing.T) {
	database := db(t)
	c := seed(t, database)
	ctx := context.Background()

	groupCal, err := store.NewCalendars().Create(ctx, database, store.CreateCalendarRequest{
		WorkgroupID: &c.group, Name: "Support rota",
	})
	if err != nil {
		t.Fatalf("creating the team calendar: %v", err)
	}

	// The group was seeded closed, so an outsider is refused.
	if _, err := c.svc.Create(ctx, database, c.carol, calendar.Draft{
		CalendarID: groupCal.ID, Event: baseEvent("Booking", utc(2026, 3, 10, 9, 0)),
	}); policy.CodeOf(err) != policy.CodeWorkgroupClosed {
		t.Fatalf("closed group: code = %q, want %q", policy.CodeOf(err), policy.CodeWorkgroupClosed)
	}

	// A member may book it.
	if _, err := c.svc.Create(ctx, database, c.alice, calendar.Draft{
		CalendarID: groupCal.ID, Event: baseEvent("Member booking", utc(2026, 3, 10, 9, 0)),
	}); err != nil {
		t.Errorf("a member should be able to book: %v", err)
	}

	// Opening it lets an outsider book, but not administer it.
	if err := store.NewWorkgroups().SetOpenBooking(ctx, database, c.group, true); err != nil {
		t.Fatalf("opening booking: %v", err)
	}
	if _, err := c.svc.Create(ctx, database, c.carol, calendar.Draft{
		CalendarID: groupCal.ID, Event: baseEvent("Outsider booking", utc(2026, 3, 11, 9, 0)),
	}); err != nil {
		t.Errorf("an open group should be bookable by anyone: %v", err)
	}

	// Reading it is open regardless, because a team calendar nobody can see
	// cannot be booked.
	result, err := c.svc.Query(ctx, database, c.carol, calendar.Selector{
		CalendarIDs: []int64{groupCal.ID},
	}, march())
	if err != nil {
		t.Errorf("an outsider should be able to read a team calendar: %v", err)
	}
	if len(result.Occurrences) != 2 {
		t.Errorf("got %d occurrences, want both bookings", len(result.Occurrences))
	}
}

// ---------------------------------------------------------------------------
// Update and Cancel
// ---------------------------------------------------------------------------

// Two people editing the same meeting is normal, and the loser of the race has
// to be told rather than allowed to overwrite the other silently.
func TestUpdateRefusesAStaleRevision(t *testing.T) {
	database := db(t)
	c := seed(t, database)
	ctx := context.Background()

	created := createMeeting(t, c.svc, database, c.alice, c.aliceCal,
		baseEvent("Standup", utc(2026, 3, 10, 9, 0)))

	// Alice edits it, which advances the sequence.
	edited := baseEvent("Standup (moved)", utc(2026, 3, 10, 11, 0))
	edited.UID = created.UID
	updated, err := c.svc.Update(ctx, database, c.alice, created.ID, calendar.Draft{
		CalendarID: c.aliceCal, Event: edited,
	}, created.Sequence)
	if err != nil {
		t.Fatalf("updating: %v", err)
	}
	if updated.Sequence != created.Sequence+1 {
		t.Errorf("sequence = %d, want it advanced", updated.Sequence)
	}
	if updated.Summary != "Standup (moved)" {
		t.Errorf("summary = %q, want the edit", updated.Summary)
	}

	// Bob's copy is now stale, and saying so is the point.
	stale := baseEvent("Standup (bob's)", utc(2026, 3, 10, 14, 0))
	stale.UID = created.UID
	if _, err := c.svc.Update(ctx, database, c.alice, created.ID, calendar.Draft{
		CalendarID: c.aliceCal, Event: stale,
	}, created.Sequence); !errors.Is(err, calendar.ErrStale) {
		t.Errorf("err = %v, want ErrStale", err)
	}

	// The current revision still works.
	if _, err := c.svc.Update(ctx, database, c.alice, updated.ID, calendar.Draft{
		CalendarID: c.aliceCal, Event: stale,
	}, updated.Sequence); err != nil {
		t.Errorf("the current revision should be editable: %v", err)
	}
}

// A client cannot turn one occurrence of a series into the whole series, or move
// it onto a different slot, by editing it and echoing back the wrong fields.
func TestUpdateKeepsTheSeriesIdentityFixed(t *testing.T) {
	database := db(t)
	c := seed(t, database)
	ctx := context.Background()

	series := baseEvent("Weekly", utc(2026, 3, 3, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	created := createMeeting(t, c.svc, database, c.alice, c.aliceCal, series)

	edit := baseEvent("Weekly (retitled)", utc(2026, 3, 3, 14, 0))
	edit.UID = "attacker-chosen@example.com"
	edit.RRULE = "FREQ=DAILY"
	slot := utc(2026, 6, 1, 9, 0)
	edit.RecurrenceID = &slot

	got, err := c.svc.Update(ctx, database, c.alice, created.ID, calendar.Draft{
		CalendarID: c.aliceCal, Event: edit,
	}, created.Sequence)
	if err != nil {
		t.Fatalf("updating: %v", err)
	}
	if got.UID != created.UID {
		t.Errorf("uid = %q, want the series' own", got.UID)
	}
	if got.RecurrenceID != nil {
		t.Errorf("recurrence_id = %v, the master must stay the master", got.RecurrenceID)
	}
	if got.Recurs() != created.Recurs() {
		t.Errorf("the rule changed from %q to %q", created.RecurrenceText(), got.RecurrenceText())
	}
}

// Moving an event to a calendar the caller happens to write on is not a thing
// the API offers, so a draft cannot redirect a meeting somewhere else.
func TestUpdateCannotMoveAnEventToAnotherCalendar(t *testing.T) {
	database := db(t)
	c := seed(t, database)

	created := createMeeting(t, c.svc, database, c.alice, c.aliceCal,
		baseEvent("Standup", utc(2026, 3, 10, 9, 0)))

	// Alice also runs the team calendar, so the refusal is about the move rather
	// than about permission.
	groupCal, err := store.NewCalendars().Create(context.Background(), database,
		store.CreateCalendarRequest{WorkgroupID: &c.group, Name: "Rota"})
	if err != nil {
		t.Fatalf("creating the team calendar: %v", err)
	}

	_, err = c.svc.Update(context.Background(), database, c.alice, created.ID, calendar.Draft{
		CalendarID: groupCal.ID, Event: baseEvent("Standup", utc(2026, 3, 10, 9, 0)),
	}, created.Sequence)
	if !errors.Is(err, calendar.ErrDenied) {
		t.Errorf("err = %v, want the move refused", err)
	}
}

// Cancelling keeps the row so that attendees can see the meeting was called off
// rather than that it silently vanished.
func TestCancelKeepsTheEvent(t *testing.T) {
	database := db(t)
	c := seed(t, database)
	ctx := context.Background()

	created := createMeeting(t, c.svc, database, c.alice, c.aliceCal,
		baseEvent("Standup", utc(2026, 3, 10, 9, 0), "bob@example.com"))

	got, err := c.svc.Cancel(ctx, database, c.alice, created.ID)
	if err != nil {
		t.Fatalf("cancelling: %v", err)
	}
	if !got.Cancelled {
		t.Error("cancelled = false")
	}
	if len(got.Attendees) != 1 {
		t.Error("the guest list should survive a cancellation")
	}
	if got.Sequence <= created.Sequence {
		t.Error("the sequence should advance so clients see this as newer")
	}

	// A cancelled event is not busy time, which is the answer that matters.
	result, err := c.svc.Query(ctx, database, c.alice, calendar.Selector{}, march())
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	if len(result.Occurrences) != 0 {
		t.Errorf("got %d occurrences, want none after the cancellation", len(result.Occurrences))
	}
}

func TestCancelOnSomebodyElsesEventIsDenied(t *testing.T) {
	database := db(t)
	c := seed(t, database)

	created := createMeeting(t, c.svc, database, c.alice, c.aliceCal,
		baseEvent("Standup", utc(2026, 3, 10, 9, 0)))
	if _, err := c.svc.Cancel(context.Background(), database, c.bob, created.ID); !errors.Is(err, calendar.ErrDenied) {
		t.Errorf("err = %v, want ErrDenied", err)
	}
}

// ---------------------------------------------------------------------------
// Query
// ---------------------------------------------------------------------------

// Without this, an invitation is invisible in the recipient's own calendar,
// which is the complaint every calendar user has.
func TestQueryIncludesInvitations(t *testing.T) {
	database := db(t)
	c := seed(t, database)
	ctx := context.Background()

	createMeeting(t, c.svc, database, c.alice, c.aliceCal,
		baseEvent("Alice's meeting", utc(2026, 3, 10, 9, 0), "bob@example.com"))
	// Bob keeps one meeting of his own.
	createMeeting(t, c.svc, database, c.bob, c.bobCal,
		baseEvent("Bob's meeting", utc(2026, 3, 11, 9, 0)))

	without, err := c.svc.Query(ctx, database, c.bob, calendar.Selector{}, march())
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	if len(without.Occurrences) != 1 {
		t.Fatalf("got %d occurrences without invitations, want only bob's", len(without.Occurrences))
	}

	with, err := c.svc.Query(ctx, database, c.bob, calendar.Selector{
		IncludeInvitations: true,
	}, march())
	if err != nil {
		t.Fatalf("querying with invitations: %v", err)
	}
	if len(with.Occurrences) != 2 {
		t.Errorf("got %d occurrences, want both", len(with.Occurrences))
	}
}

// An event the recipient attends that also sits on their own calendar is one
// meeting, not two, and listing it twice draws it twice.
func TestQueryDoesNotDuplicateAnEventInBothPlaces(t *testing.T) {
	database := db(t)
	c := seed(t, database)
	ctx := context.Background()

	createMeeting(t, c.svc, database, c.alice, c.aliceCal,
		baseEvent("Both", utc(2026, 3, 10, 9, 0), "bob@example.com"))
	// Bob adds a copy to his own calendar, which is what some clients do.
	if _, err := c.svc.Create(ctx, database, c.bob, calendar.Draft{
		CalendarID: c.bobCal, Event: baseEvent("Bob's copy", utc(2026, 3, 12, 9, 0)),
	}); err != nil {
		t.Fatalf("creating bob's own: %v", err)
	}

	result, err := c.svc.Query(ctx, database, c.bob, calendar.Selector{
		IncludeInvitations: true,
	}, march())
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	seen := map[int64]int{}
	for _, v := range result.Occurrences {
		if v.EventID != 0 {
			seen[v.EventID]++
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("event %d appears %d times", id, n)
		}
	}
}

// Somebody else's calendar is refused rather than returned empty: "your calendar
// is empty" and "you may not see that calendar" are different answers and only
// one of them is true.
func TestQueryRefusesSomebodyElsesCalendar(t *testing.T) {
	database := db(t)
	c := seed(t, database)

	_, err := c.svc.Query(context.Background(), database, c.bob, calendar.Selector{
		CalendarIDs: []int64{c.aliceCal},
	}, march())
	if policy.CodeOf(err) != policy.CodeNotCalendarOwner {
		t.Errorf("code = %q, want %q", policy.CodeOf(err), policy.CodeNotCalendarOwner)
	}
}

func TestQueryReturnsCalendarsAndRows(t *testing.T) {
	database := db(t)
	c := seed(t, database)
	ctx := context.Background()

	series := baseEvent("Weekly", utc(2026, 3, 3, 9, 0), "bob@example.com")
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	created := createMeeting(t, c.svc, database, c.alice, c.aliceCal, series)

	result, err := c.svc.Query(ctx, database, c.alice, calendar.Selector{}, march())
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	if len(result.Calendars) != 1 || result.Calendars[0].ID != c.aliceCal {
		t.Errorf("calendars = %+v", result.Calendars)
	}
	if len(result.Occurrences) != 5 {
		t.Errorf("got %d occurrences, want five Tuesdays", len(result.Occurrences))
	}
	for _, v := range result.Occurrences {
		if v.EventID != created.ID {
			t.Errorf("occurrence points at row %d, want %d", v.EventID, created.ID)
		}
		if len(v.Attendees) != 1 || v.Attendees[0].Address != "bob@example.com" {
			t.Errorf("attendees = %+v", v.Attendees)
		}
		if v.Organizer != c.alice.Address && v.OrganizerAddress != c.alice.Address {
			t.Errorf("organizer = %q <%q>", v.Organizer, v.OrganizerAddress)
		}
	}
}

// ---------------------------------------------------------------------------
// Free/busy
// ---------------------------------------------------------------------------

func TestFreeBusyForASeveralPeople(t *testing.T) {
	database := db(t)
	c := seed(t, database)
	ctx := context.Background()

	series := baseEvent("Weekly", utc(2026, 3, 3, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	createMeeting(t, c.svc, database, c.alice, c.aliceCal, series)
	createMeeting(t, c.svc, database, c.bob, c.bobCal,
		baseEvent("Bob's all-day", utc(2026, 3, 12, 0, 0)))

	got, err := c.svc.FreeBusy(ctx, database, c.carol, calendar.FreeBusyRequest{
		Addresses: []string{"alice@example.com", "bob@example.com", "nobody@example.com"},
		Window:    march(),
	})
	if err != nil {
		t.Fatalf("FreeBusy: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d answers, want one per address", len(got))
	}
	if len(got[0].Busy) != 5 {
		t.Errorf("alice has %d busy blocks, want 5", len(got[0].Busy))
	}
	if len(got[1].Busy) != 1 {
		t.Errorf("bob has %d busy blocks, want 1", len(got[1].Busy))
	}
	// Somebody with no calendars is free, and that is an answer rather than a
	// missing one.
	if len(got[2].Busy) != 0 || got[2].Hidden {
		t.Errorf("a stranger should be free and visible: %+v", got[2])
	}
	for _, a := range got {
		if a.Busy == nil {
			t.Errorf("%s: Busy is nil, which marshals as null rather than []", a.Address)
		}
	}
}

// The important case: somebody whose free/busy is not shared comes back marked
// hidden, not empty. Reporting them as free is a lie that ends in a double
// booking in front of everyone.
func TestFreeBusyReportsHiddenRatherThanFree(t *testing.T) {
	database := db(t)
	c := seed(t, database)
	ctx := context.Background()

	createMeeting(t, c.svc, database, c.alice, c.aliceCal,
		baseEvent("Private", utc(2026, 3, 10, 9, 0)))
	hide := false
	if err := store.NewCalendars().Update(ctx, database, c.aliceCal,
		store.UpdateCalendarRequest{ShowFreeBusy: &hide}); err != nil {
		t.Fatalf("hiding free/busy: %v", err)
	}

	got, err := c.svc.FreeBusy(ctx, database, c.bob, calendar.FreeBusyRequest{
		Addresses: []string{"alice@example.com"}, Window: march(),
	})
	if err != nil {
		t.Fatalf("FreeBusy: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d answers", len(got))
	}
	if !got[0].Hidden {
		t.Error("Hidden = false, want the private calendar marked hidden")
	}
	if len(got[0].Busy) != 0 {
		t.Errorf("a hidden answer must carry no blocks, got %+v", got[0].Busy)
	}

	// Alice still sees her own availability whatever the setting says, because
	// the setting exists for other people's benefit.
	own, err := c.svc.FreeBusy(ctx, database, c.alice, calendar.FreeBusyRequest{
		Addresses: []string{"alice@example.com"}, Window: march(),
	})
	if err != nil {
		t.Fatalf("FreeBusy: %v", err)
	}
	if own[0].Hidden {
		t.Error("your own availability is never hidden from you")
	}
	if len(own[0].Busy) != 1 {
		t.Errorf("got %d blocks for yourself, want 1", len(own[0].Busy))
	}
}

// Free/busy covers every calendar a person owns, not only the default: the
// "offsite all week" entry lives on a second calendar.
func TestFreeBusyCoversEveryCalendarItsOwnerHas(t *testing.T) {
	database := db(t)
	c := seed(t, database)
	ctx := context.Background()

	createMeeting(t, c.svc, database, c.alice, c.aliceCal,
		baseEvent("Regular", utc(2026, 3, 10, 9, 0)))

	travel, err := store.NewCalendars().Create(ctx, database, store.CreateCalendarRequest{
		Owner: ptr("alice@example.com"), Name: "Travel",
	})
	if err != nil {
		t.Fatalf("creating a second calendar: %v", err)
	}
	createMeeting(t, c.svc, database, c.alice, travel.ID,
		baseEvent("Offsite", utc(2026, 3, 12, 9, 0)))

	got, err := c.svc.FreeBusy(ctx, database, c.bob, calendar.FreeBusyRequest{
		Addresses: []string{"alice@example.com"}, Window: march(),
	})
	if err != nil {
		t.Fatalf("FreeBusy: %v", err)
	}
	if len(got[0].Busy) != 2 {
		t.Errorf("got %d blocks, want both calendars represented", len(got[0].Busy))
	}
}

func TestFreeBusyIgnoresBlankAddresses(t *testing.T) {
	database := db(t)
	c := seed(t, database)

	got, err := c.svc.FreeBusy(context.Background(), database, c.alice,
		calendar.FreeBusyRequest{
			Addresses: []string{"  ", "alice@example.com", ""},
			Window:    march(),
		})
	if err != nil {
		t.Fatalf("FreeBusy: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d answers, want blank addresses dropped", len(got))
	}
}

// WithLimits returns a copy, so two requests with different ceilings cannot race
// on a shared field.
func TestWithLimitsDoesNotMutateTheService(t *testing.T) {
	database := db(t)
	c := seed(t, database)
	ctx := context.Background()

	createMeeting(t, c.svc, database, c.alice, c.aliceCal,
		baseEvent("Standup", utc(2026, 3, 10, 9, 0)))

	tight := c.svc.WithLimits(recurrence.Limits{MaxOccurrences: 1, MaxWindow: time.Hour})
	if _, err := tight.Query(ctx, database, c.alice, calendar.Selector{}, march()); !errors.Is(err, recurrence.ErrWindowTooWide) {
		t.Errorf("the tight copy should refuse, err = %v", err)
	}
	// The original is untouched.
	if _, err := c.svc.Query(ctx, database, c.alice, calendar.Selector{}, march()); err != nil {
		t.Errorf("the original service was affected by its copy: %v", err)
	}
}

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/store"
)

// calendarFixtures is a small world for the calendar tests: two people, a
// workgroup they both belong to, and one calendar each to start from.
type calendarFixtures struct {
	alice     string
	bob       string
	workgroup int64
	aliceCal  int64
	bobCal    int64
}

func seedCalendarFixtures(t *testing.T, db *sqlx.DB) calendarFixtures {
	t.Helper()
	ctx := context.Background()

	if err := store.NewDomains().Create(ctx, db, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("seeding domain: %v", err)
	}
	users := store.NewUsers()
	for _, u := range []struct{ address, name string }{
		{"alice@example.com", "Alice"},
		{"bob@example.com", "Bob"},
	} {
		if err := users.Create(ctx, db, store.CreateUserRequest{
			Address: u.address, Source: policy.SourceLocal, DisplayName: u.name,
		}); err != nil {
			t.Fatalf("seeding %s: %v", u.address, err)
		}
	}

	groups := store.NewWorkgroups()
	wg, err := groups.Create(ctx, db, store.CreateWorkgroupRequest{
		Name:        "support",
		Domain:      "example.com",
		DisplayName: "Support",
		Members: []store.WorkgroupMember{
			{Address: "alice@example.com", Role: store.WorkgroupRoleAdmin},
			{Address: "bob@example.com"},
		},
	})
	if err != nil {
		t.Fatalf("seeding workgroup: %v", err)
	}

	calendars := store.NewCalendars()
	aliceCal, err := calendars.Create(ctx, db, store.CreateCalendarRequest{
		Owner: strptr("alice@example.com"), Name: "Calendar",
		Timezone: "Europe/London", IsDefault: true,
	})
	if err != nil {
		t.Fatalf("seeding alice's calendar: %v", err)
	}
	bobCal, err := calendars.Create(ctx, db, store.CreateCalendarRequest{
		Owner: strptr("bob@example.com"), Name: "Calendar", IsDefault: true,
	})
	if err != nil {
		t.Fatalf("seeding bob's calendar: %v", err)
	}

	return calendarFixtures{
		alice: "alice@example.com", bob: "bob@example.com",
		workgroup: wg.ID, aliceCal: aliceCal.ID, bobCal: bobCal.ID,
	}
}

func strptr(s string) *string { return &s }

func utc(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

// writeEvent builds a store write from a parsed model event, which is how the
// application does it: the iCalendar document is the input and the columns are
// derived from it.
func writeEvent(calendarID int64, ev ical.Event) store.WriteEventRequest {
	req := store.WriteEventRequest{
		CalendarID:       calendarID,
		UID:              ev.UID,
		RecurrenceID:     ev.RecurrenceID,
		ICalBlob:         ev.Marshal(),
		DTStart:          ev.Start,
		DTEnd:            ev.End,
		Timezone:         ev.Timezone,
		AllDay:           ev.AllDay,
		Summary:          ev.Summary,
		Location:         ev.Location,
		Method:           string(ical.MethodRequest),
		Organizer:        ev.Organizer.Name,
		OrganizerAddress: ev.Organizer.Address,
		Status:           ev.Status,
		ShowAs:           ev.ShowAs,
		Sequence:         ev.Sequence,
		DTStamp:          ev.DTStamp,
		Cancelled:        ev.IsCancelled(),
	}
	if ev.RRULE != "" {
		rule := ev.RRULE
		req.RRULE = &rule
	}
	for _, a := range ev.Attendees {
		req.Attendees = append(req.Attendees, store.Attendee{
			Address:     a.Address,
			DisplayName: a.Name,
			Role:        a.Role,
			PartStat:    a.PartStat,
			RSVP:        a.RSVP,
		})
	}
	return req
}

// meeting builds a plain one-hour event.
func meeting(uid, summary string, start time.Time) ical.Event {
	return ical.Event{
		UID: uid, Summary: summary,
		Start: start, End: start.Add(time.Hour),
		DTStamp: start.Add(-24 * time.Hour),
		Status:  ical.StatusConfirmed, ShowAs: ical.ShowAsBusy,
		Organizer: ical.Organizer{Address: "alice@example.com"},
	}
}

// ---------------------------------------------------------------------------
// Calendars
// ---------------------------------------------------------------------------

func TestCalendarCreateAppliesDefaults(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	ctx := context.Background()
	calendars := store.NewCalendars()

	got, err := calendars.Get(ctx, db, f.aliceCal)
	if err != nil {
		t.Fatalf("loading calendar: %v", err)
	}
	if got.Owner == nil || *got.Owner != f.alice {
		t.Errorf("owner = %v, want %q", got.Owner, f.alice)
	}
	if got.WorkgroupID != nil {
		t.Errorf("workgroup_id = %v, want nil for a personal calendar", got.WorkgroupID)
	}
	if got.Color == "" || got.Timezone == "" {
		t.Errorf("color/timezone should have defaults, got %q/%q", got.Color, got.Timezone)
	}
	if !got.ShowFreeBusy {
		t.Error("show_free_busy should default to true")
	}
	if !got.IsDefault {
		t.Error("is_default should have been stored")
	}
	if got.IsWorkgroup() {
		t.Error("a personal calendar is not a workgroup calendar")
	}
}

// A calendar has exactly one holder. Neither blank nor ambiguous is allowed,
// because "whose calendar is this" is the first question every later one depends
// on.
func TestCalendarRequiresExactlyOneHolder(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	calendars := store.NewCalendars()
	ctx := context.Background()

	cases := map[string]store.CreateCalendarRequest{
		"neither":     {Name: "Orphan"},
		"both":        {Owner: strptr(f.alice), WorkgroupID: &f.workgroup, Name: "Both"},
		"empty owner": {Owner: strptr(""), Name: "Empty"},
	}
	for name, req := range cases {
		if _, err := calendars.Create(ctx, db, req); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

func TestWorkgroupCalendar(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	ctx := context.Background()
	calendars := store.NewCalendars()

	got, err := calendars.Create(ctx, db, store.CreateCalendarRequest{
		WorkgroupID: &f.workgroup, Name: "Support rota", Color: "#ef4444",
	})
	if err != nil {
		t.Fatalf("creating workgroup calendar: %v", err)
	}
	if !got.IsWorkgroup() {
		t.Error("a workgroup calendar should report itself as one")
	}
	if got.Owner != nil {
		t.Errorf("owner = %v, want nil", got.Owner)
	}

	list, err := calendars.ByWorkgroup(ctx, db, f.workgroup)
	if err != nil {
		t.Fatalf("listing workgroup calendars: %v", err)
	}
	if len(list) != 1 || list[0].ID != got.ID {
		t.Errorf("ByWorkgroup returned %d calendars", len(list))
	}
}

// A person gets exactly one default calendar, because "which calendar does a new
// event go on" must have one answer. The second attempt is a conflict rather
// than a silent second default.
func TestOnlyOneDefaultCalendarPerOwner(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	calendars := store.NewCalendars()
	ctx := context.Background()

	second, err := calendars.Create(ctx, db, store.CreateCalendarRequest{
		Owner: strptr(f.alice), Name: "Second", IsDefault: true,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("creating a second default: err = %v, want ErrConflict", err)
	}
	_ = second

	// A non-default second calendar is fine.
	if _, err := calendars.Create(ctx, db, store.CreateCalendarRequest{
		Owner: strptr(f.alice), Name: "Travel",
	}); err != nil {
		t.Fatalf("creating a second non-default calendar: %v", err)
	}
	list, err := calendars.ByOwner(ctx, db, f.alice)
	if err != nil {
		t.Fatalf("listing calendars: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d calendars, want 2", len(list))
	}
	// The default sorts first, so a client can take the head of the list.
	if !list[0].IsDefault {
		t.Error("the default calendar should sort first")
	}
}

// Promoting another calendar must demote the incumbent in the same breath,
// or the partial unique index refuses the promotion.
func TestPromotingADefaultDemotesTheOldOne(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	calendars := store.NewCalendars()
	ctx := context.Background()

	other, err := calendars.Create(ctx, db, store.CreateCalendarRequest{
		Owner: strptr(f.alice), Name: "Travel",
	})
	if err != nil {
		t.Fatalf("creating second calendar: %v", err)
	}
	if err := calendars.Update(ctx, db, other.ID, store.UpdateCalendarRequest{
		IsDefault: boolptr(true),
	}); err != nil {
		t.Fatalf("promoting: %v", err)
	}

	list, err := calendars.ByOwner(ctx, db, f.alice)
	if err != nil {
		t.Fatalf("listing calendars: %v", err)
	}
	defaults := 0
	for _, c := range list {
		if c.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("got %d default calendars, want exactly 1", defaults)
	}
}

func boolptr(b bool) *bool { return &b }

// A nil field in an update means unchanged, which is why they are all pointers.
func TestCalendarUpdateLeavesUnsetFieldsAlone(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	calendars := store.NewCalendars()
	ctx := context.Background()

	name := "Renamed"
	if err := calendars.Update(ctx, db, f.aliceCal, store.UpdateCalendarRequest{
		Name: &name,
	}); err != nil {
		t.Fatalf("updating: %v", err)
	}

	got, err := calendars.Get(ctx, db, f.aliceCal)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if got.Name != "Renamed" {
		t.Errorf("name = %q, want the new one", got.Name)
	}
	if got.Timezone != "Europe/London" {
		t.Errorf("timezone = %q, want it untouched", got.Timezone)
	}
	if !got.ShowFreeBusy {
		t.Error("show_free_busy should have been left alone")
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Error("updated_at should have advanced")
	}
}

func TestEnsureDefaultIsIdempotent(t *testing.T) {
	db := testMigratedDB(t)
	seedCalendarFixtures(t, db)
	ctx := context.Background()

	// Carol has no calendars at all yet.
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: "carol@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("seeding carol: %v", err)
	}
	calendars := store.NewCalendars()

	first, err := calendars.EnsureDefault(ctx, db, "carol@example.com", "Europe/Berlin")
	if err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}
	second, err := calendars.EnsureDefault(ctx, db, "carol@example.com", "Europe/Berlin")
	if err != nil {
		t.Fatalf("EnsureDefault again: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("EnsureDefault created two calendars: %d and %d", first.ID, second.ID)
	}
	if first.Timezone != "Europe/Berlin" {
		t.Errorf("timezone = %q, want the one supplied", first.Timezone)
	}

	// A missing calendar is not ErrNotFound from DefaultFor, and a calendar
	// that does exist is returned unchanged.
	if _, err := calendars.DefaultFor(ctx, db, "carol@example.com"); err != nil {
		t.Fatalf("DefaultFor after EnsureDefault: %v", err)
	}
	if _, err := calendars.DefaultFor(ctx, db, "nobody@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DefaultFor for a stranger: err = %v, want ErrNotFound", err)
	}
}

func TestDeletingACalendarTakesItsEvents(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	ctx := context.Background()
	events := store.NewEvents()

	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal,
		meeting("gone@example.com", "Doomed", utc(2026, 3, 2, 9, 0)))); err != nil {
		t.Fatalf("creating event: %v", err)
	}
	if err := store.NewCalendars().Delete(ctx, db, f.aliceCal); err != nil {
		t.Fatalf("deleting calendar: %v", err)
	}

	got, err := events.ListInRange(ctx, db, []int64{f.aliceCal},
		utc(2026, 3, 1, 0, 0), utc(2026, 4, 1, 0, 0))
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d events, want none after the calendar was deleted", len(got))
	}
	if err := store.NewCalendars().Delete(ctx, db, f.aliceCal); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting twice: err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

func TestEventCreateStoresTheDocumentAndItsAttendees(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	ev := meeting("design@example.com", "Design review", utc(2026, 3, 10, 14, 0))
	ev.Location = "Room 3"
	ev.Timezone = "Europe/London"
	ev.Sequence = 2
	ev.Attendees = []ical.Attendee{
		{Address: "bob@example.com", Name: "Bob", Role: ical.RoleRequired,
			PartStat: ical.PartStatAccepted, RSVP: true},
		{Address: "carol@example.com", Role: ical.RoleOptional,
			PartStat: ical.PartStatNeedsAction},
	}

	created, err := events.Create(ctx, db, writeEvent(f.aliceCal, ev))
	if err != nil {
		t.Fatalf("creating event: %v", err)
	}

	got, err := events.Get(ctx, db, created.ID)
	if err != nil {
		t.Fatalf("loading event: %v", err)
	}
	if got.UID != ev.UID {
		t.Errorf("uid = %q", got.UID)
	}
	if got.Summary != "Design review" || got.Location != "Room 3" {
		t.Errorf("summary/location = %q/%q", got.Summary, got.Location)
	}
	if got.Timezone != "Europe/London" {
		t.Errorf("timezone = %q, want it denormalised for expansion", got.Timezone)
	}
	if got.Sequence != 2 {
		t.Errorf("sequence = %d, want 2", got.Sequence)
	}
	if got.OrganizerAddress != "alice@example.com" {
		t.Errorf("organizer_address = %q", got.OrganizerAddress)
	}
	if got.Recurs() {
		t.Error("an event with no RRULE must not report as recurring")
	}
	if got.IsOverride() {
		t.Error("a master event has no recurrence slot")
	}

	// The stored document must still parse, and must still say what it said.
	if len(got.Attendees) != 2 {
		t.Fatalf("got %d attendees, want 2", len(got.Attendees))
	}
	bob, ok := findAttendee(got.Attendees, "bob@example.com")
	if !ok {
		t.Fatal("bob is missing")
	}
	if !bob.Accepts() {
		t.Errorf("bob partstat = %q, want ACCEPTED", bob.PartStat)
	}
	if !bob.RSVP {
		t.Error("bob RSVP should be true")
	}

	back, err := ical.Parse(got.ICalBlob)
	if err != nil {
		t.Fatalf("the stored document does not parse: %v", err)
	}
	if len(back.Events) != 1 || back.Events[0].UID != ev.UID {
		t.Errorf("the stored document does not round-trip: %+v", back.Events)
	}
}

func findAttendee(list []store.Attendee, address string) (store.Attendee, bool) {
	for _, a := range list {
		if a.Address == address {
			return a, true
		}
	}
	return store.Attendee{}, false
}

// The store refuses the impossible before PostgreSQL does, so the caller is told
// which event is wrong rather than receiving a constraint failure.
func TestEventWriteValidation(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	good := writeEvent(f.aliceCal, meeting("v@example.com", "V", utc(2026, 3, 2, 9, 0)))

	cases := map[string]func(store.WriteEventRequest) store.WriteEventRequest{
		"no calendar": func(r store.WriteEventRequest) store.WriteEventRequest { r.CalendarID = 0; return r },
		"no uid":      func(r store.WriteEventRequest) store.WriteEventRequest { r.UID = "  "; return r },
		"no document": func(r store.WriteEventRequest) store.WriteEventRequest { r.ICalBlob = nil; return r },
		"no start":    func(r store.WriteEventRequest) store.WriteEventRequest { r.DTStart = time.Time{}; return r },
		"ends first":  func(r store.WriteEventRequest) store.WriteEventRequest { r.DTEnd = r.DTStart.Add(-time.Hour); return r },
	}
	for name, mutate := range cases {
		if _, err := events.Create(ctx, db, mutate(good)); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

// A recurring series is one row. Its rule and zone are columns, not something to
// be re-parsed out of the blob on every calendar view.
func TestRecurringEventStoresItsRule(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	ev := meeting("weekly@example.com", "Standup", utc(2026, 3, 10, 9, 0))
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=5"
	ev.Timezone = "Europe/London"
	ev.AllDay = false

	created, err := events.Create(ctx, db, writeEvent(f.aliceCal, ev))
	if err != nil {
		t.Fatalf("creating event: %v", err)
	}
	if !created.Recurs() {
		t.Fatal("the event should report as recurring")
	}
	if created.RecurrenceText() != ev.RRULE {
		t.Errorf("rrule = %q, want %q", created.RecurrenceText(), ev.RRULE)
	}

	got, err := events.ByUID(ctx, db, f.aliceCal, ev.UID, nil)
	if err != nil {
		t.Fatalf("loading by UID: %v", err)
	}
	if got.RecurrenceText() != ev.RRULE {
		t.Errorf("rrule = %q after a reload", got.RecurrenceText())
	}
	if got.AllDay {
		t.Error("all_day should be false for a timed event")
	}
}

// A moved occurrence is a second row with the same UID. This is the case the
// original unique constraint made impossible, so it is pinned here.
func TestDetachedInstanceSharesTheSeriesUID(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	series := meeting("series@example.com", "Weekly", utc(2026, 3, 10, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	master, err := events.Create(ctx, db, writeEvent(f.aliceCal, series))
	if err != nil {
		t.Fatalf("creating series: %v", err)
	}

	slot := utc(2026, 3, 17, 9, 0)
	movedTo := utc(2026, 3, 18, 14, 0)
	override := meeting(series.UID, "Weekly (moved)", movedTo)
	override.RecurrenceID = &slot
	override.Sequence = 1

	created, err := events.Create(ctx, db, writeEvent(f.aliceCal, override))
	if err != nil {
		t.Fatalf("creating the detached instance: %v", err)
	}
	if created.ID == master.ID {
		t.Error("the detached instance overwrote the master instead of joining it")
	}
	if !created.IsOverride() {
		t.Error("the detached instance should report as an override")
	}
	// The slot is the one the override replaces, not where it now sits.
	if created.RecurrenceID == nil || !created.RecurrenceID.Equal(slot) {
		t.Errorf("recurrence_id = %v, want the original slot %v", created.RecurrenceID, slot)
	}
	if !created.DTStart.Equal(movedTo) {
		t.Errorf("dtstart = %v, want the new time %v", created.DTStart, movedTo)
	}

	all, err := events.SeriesByUID(ctx, db, f.aliceCal, series.UID)
	if err != nil {
		t.Fatalf("loading the series: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d rows for the series, want 2", len(all))
	}
	// The master sorts first, which is what a caller merging on recurrence_id
	// expects.
	if all[0].RecurrenceID != nil {
		t.Error("the master should sort first")
	}
}

// Two overrides for the same slot are one slot, not two.
func TestDetachedInstanceIsUniquePerSlot(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	series := meeting("s@example.com", "Weekly", utc(2026, 3, 10, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, series)); err != nil {
		t.Fatalf("creating series: %v", err)
	}

	slot := utc(2026, 3, 17, 9, 0)
	first := meeting(series.UID, "Moved", utc(2026, 3, 18, 14, 0))
	first.RecurrenceID = &slot
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, first)); err != nil {
		t.Fatalf("the first override for a slot must be accepted: %v", err)
	}

	second := meeting(series.UID, "Moved again", utc(2026, 3, 19, 15, 0))
	second.RecurrenceID = &slot
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, second)); !errors.Is(err, store.ErrConflict) {
		t.Errorf("a second override for the same slot: err = %v, want ErrConflict", err)
	}

	// A second master for the same UID is also a conflict.
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, series)); !errors.Is(err, store.ErrConflict) {
		t.Errorf("second master: err = %v, want ErrConflict", err)
	}
}

// A different slot is a different override and must be allowed.
func TestDifferentSlotsAreSeparateOverrides(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	series := meeting("s2@example.com", "Weekly", utc(2026, 3, 10, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, series)); err != nil {
		t.Fatalf("creating series: %v", err)
	}
	for i, slot := range []time.Time{utc(2026, 3, 17, 9, 0), utc(2026, 3, 24, 9, 0)} {
		override := meeting(series.UID, "Moved", slot.Add(time.Hour*24))
		s := slot
		override.RecurrenceID = &s
		override.Sequence = i + 1
		if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, override)); err != nil {
			t.Fatalf("override %d: %v", i, err)
		}
	}
}

// The range query is the one every calendar view runs, so its boundaries are the
// thing most worth pinning: half-open, and asymmetric at the two ends.
func TestListInRangeBoundaries(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	// 09:00 to 11:00 on the 10th.
	ev := meeting("bounds@example.com", "Bounds", utc(2026, 3, 10, 9, 0))
	ev.End = utc(2026, 3, 10, 11, 0)
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, ev)); err != nil {
		t.Fatalf("creating event: %v", err)
	}

	from := utc(2026, 3, 10, 0, 0)
	to := utc(2026, 3, 11, 0, 0)
	cases := []struct {
		name     string
		from, to time.Time
		want     int
	}{
		{"covering", from, to, 1},
		{"exactly the event", utc(2026, 3, 10, 9, 0), utc(2026, 3, 10, 11, 0), 1},
		{"overlapping the start", utc(2026, 3, 10, 10, 0), to, 1},
		{"overlapping the end", from, utc(2026, 3, 10, 10, 0), 1},
		{"ending exactly at the window start", utc(2026, 3, 10, 11, 0), to, 0},
		{"starting exactly at the window end", from, utc(2026, 3, 10, 9, 0), 0},
		{"entirely before", utc(2026, 3, 9, 0, 0), from, 0},
		{"entirely after", to, utc(2026, 3, 12, 0, 0), 0},
	}
	for _, tc := range cases {
		got, err := events.ListInRange(ctx, db, []int64{f.aliceCal}, tc.from, tc.to)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: got %d events, want %d", tc.name, len(got), tc.want)
		}
	}
}

// A zero-length event sits inside a window or outside it; it never straddles a
// boundary, so dropping it would make a marker on the day itself disappear.
func TestZeroLengthEventIsFoundInItsOwnWindow(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	at := utc(2026, 3, 10, 9, 0)
	ev := meeting("instant@example.com", "Instant", at)
	ev.End = at
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, ev)); err != nil {
		t.Fatalf("creating event: %v", err)
	}

	got, err := events.ListInRange(ctx, db, []int64{f.aliceCal},
		utc(2026, 3, 10, 0, 0), utc(2026, 3, 11, 0, 0))
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d events, want the zero-length one", len(got))
	}
}

// The query fetches the whole series once any of it is in range. That is what
// makes a moved occurrence visible: its own dates may be nowhere near the window
// the calendar asked about.
func TestListInRangeReturnsTheWholeSeries(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	// A series that starts long before the window and runs into it.
	series := meeting("old@example.com", "Old series", utc(2020, 1, 7, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, series)); err != nil {
		t.Fatalf("creating series: %v", err)
	}

	// An override of a slot from 2020, moved into the window we will query.
	slot := utc(2020, 1, 14, 9, 0)
	movedTo := utc(2026, 3, 11, 14, 0)
	override := meeting(series.UID, "Resurrected", movedTo)
	override.RecurrenceID = &slot
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, override)); err != nil {
		t.Fatalf("creating override: %v", err)
	}

	got, err := events.ListInRange(ctx, db, []int64{f.aliceCal},
		utc(2026, 3, 1, 0, 0), utc(2026, 4, 1, 0, 0))
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	// The master is in range, and its detached instance must come with it even
	// though the instance's own dates are the only ones inside the window.
	if len(got) != 2 {
		t.Fatalf("got %d rows, want the master and its detached instance", len(got))
	}
	var sawOverride bool
	for _, e := range got {
		if e.IsOverride() && e.DTStart.Equal(movedTo) {
			sawOverride = true
		}
	}
	if !sawOverride {
		t.Error("the detached instance was not returned with its series")
	}
}

// A cancelled series must come back with the cancellation attached, so the
// expansion layer can drop the occurrence rather than re-adding it.
func TestListInRangeKeepsCancelledSeries(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	series := meeting("c@example.com", "Called off", utc(2026, 3, 10, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=3"
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, series)); err != nil {
		t.Fatalf("creating series: %v", err)
	}
	// Cancelling one slot is a detached, cancelled instance rather than a
	// change to the series.
	slot := utc(2026, 3, 17, 9, 0)
	cancelled := meeting(series.UID, "Called off", slot)
	cancelled.RecurrenceID = &slot
	cancelled.Status = ical.StatusCancelled
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, cancelled)); err != nil {
		t.Fatalf("creating cancellation: %v", err)
	}

	got, err := events.ListInRange(ctx, db, []int64{f.aliceCal},
		utc(2026, 3, 1, 0, 0), utc(2026, 4, 1, 0, 0))
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want the series and its cancellation", len(got))
	}
	found := false
	for _, e := range got {
		if e.IsOverride() {
			found = true
			if !e.Cancelled || e.Status != ical.StatusCancelled {
				t.Errorf("the detached row should be cancelled, got %v/%q", e.Cancelled, e.Status)
			}
		}
	}
	if !found {
		t.Error("the cancellation was not returned")
	}
}

func TestListInRangeAcrossCalendars(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	for _, cal := range []int64{f.aliceCal, f.bobCal} {
		if _, err := events.Create(ctx, db, writeEvent(cal,
			meeting("x@example.com", "Meeting", utc(2026, 3, 10, 9, 0)))); err != nil {
			t.Fatalf("creating event on %d: %v", cal, err)
		}
	}

	both, err := events.ListInRange(ctx, db, []int64{f.aliceCal, f.bobCal},
		utc(2026, 3, 1, 0, 0), utc(2026, 4, 1, 0, 0))
	if err != nil {
		t.Fatalf("listing both: %v", err)
	}
	if len(both) != 2 {
		t.Errorf("got %d events across both calendars, want 2", len(both))
	}

	// An empty calendar set is not an error and not a full-table scan.
	none, err := events.ListInRange(ctx, db, nil,
		utc(2026, 3, 1, 0, 0), utc(2026, 4, 1, 0, 0))
	if err != nil {
		t.Fatalf("listing with no calendars: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("got %d events with no calendars selected", len(none))
	}
}

// Cancelling keeps the row. Deleting it would leave attendees holding a meeting
// that silently vanished.
func TestCancelKeepsTheRowAndBumpsTheSequence(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	ev := meeting("cancel@example.com", "Cancel me", utc(2026, 3, 10, 9, 0))
	ev.Attendees = []ical.Attendee{{Address: "bob@example.com", PartStat: ical.PartStatAccepted}}
	created, err := events.Create(ctx, db, writeEvent(f.aliceCal, ev))
	if err != nil {
		t.Fatalf("creating event: %v", err)
	}

	got, err := events.Cancel(ctx, db, created.ID)
	if err != nil {
		t.Fatalf("cancelling: %v", err)
	}
	if !got.Cancelled {
		t.Error("cancelled = false, want true")
	}
	if got.Status != ical.StatusCancelled {
		t.Errorf("status = %q", got.Status)
	}
	if got.Sequence != created.Sequence+1 {
		t.Errorf("sequence = %d, want %d so clients see this as newer",
			got.Sequence, created.Sequence+1)
	}
	if len(got.Attendees) != 1 {
		t.Errorf("got %d attendees, the row and its guest list must survive", len(got.Attendees))
	}
	if _, err := events.Cancel(ctx, db, created.ID+9999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cancelling a stranger: err = %v, want ErrNotFound", err)
	}
}

// Upsert is the write half of receiving a scheduling message, where the same
// event legitimately arrives twice.
func TestEventUpsertReplacesTheSameSlot(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	first := meeting("u@example.com", "First subject", utc(2026, 3, 10, 9, 0))
	first.Sequence = 0
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, first)); err != nil {
		t.Fatalf("creating: %v", err)
	}

	second := meeting("u@example.com", "Corrected subject", utc(2026, 3, 10, 11, 0))
	second.Sequence = 1
	second.Attendees = []ical.Attendee{{Address: "bob@example.com", PartStat: ical.PartStatTentative}}
	updated, err := events.Upsert(ctx, db, writeEvent(f.aliceCal, second))
	if err != nil {
		t.Fatalf("upserting: %v", err)
	}

	got, err := events.Get(ctx, db, updated.ID)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if got.Summary != "Corrected subject" {
		t.Errorf("summary = %q, want the replacement", got.Summary)
	}
	if !got.DTStart.Equal(second.Start) {
		t.Errorf("dtstart = %v, want %v", got.DTStart, second.Start)
	}
	if len(got.Attendees) != 1 {
		t.Errorf("got %d attendees, the guest list should be replaced not merged", len(got.Attendees))
	}

	// There is still exactly one row for this UID.
	rows, err := events.SeriesByUID(ctx, db, f.aliceCal, first.UID)
	if err != nil {
		t.Fatalf("loading series: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows after an upsert, want 1", len(rows))
	}
}

// An override is stored under its own slot, so an upsert of the master must not
// disturb it.
func TestUpsertLeavesOverridesAlone(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	series := meeting("keep@example.com", "Weekly", utc(2026, 3, 10, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, series)); err != nil {
		t.Fatalf("creating series: %v", err)
	}
	slot := utc(2026, 3, 17, 9, 0)
	override := meeting(series.UID, "Moved", slot.AddDate(0, 0, 1))
	override.RecurrenceID = &slot
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, override)); err != nil {
		t.Fatalf("creating override: %v", err)
	}

	// Re-store the master with new content.
	resent := meeting(series.UID, "Weekly (renamed)", utc(2026, 3, 10, 10, 0))
	resent.RRULE = series.RRULE
	resent.Sequence = 1
	if _, err := events.Upsert(ctx, db, writeEvent(f.aliceCal, resent)); err != nil {
		t.Fatalf("upserting the master: %v", err)
	}

	rows, err := events.SeriesByUID(ctx, db, f.aliceCal, series.UID)
	if err != nil {
		t.Fatalf("loading series: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the master and the override", len(rows))
	}
	if rows[0].Summary != "Weekly (renamed)" {
		t.Errorf("the master was not replaced: %q", rows[0].Summary)
	}
	if rows[1].Summary != "Moved" {
		t.Errorf("the override was disturbed: %q", rows[1].Summary)
	}
}

// The reply path: an answer arrives naming a UID, not a row id, and must land on
// the right attendee of the right series.
func TestSetPartStatRecordsAnAnswer(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	ev := meeting("r@example.com", "Meeting", utc(2026, 3, 10, 9, 0))
	ev.Attendees = []ical.Attendee{
		{Address: "bob@example.com", PartStat: ical.PartStatNeedsAction, RSVP: true},
		{Address: "carol@example.com", PartStat: ical.PartStatNeedsAction, RSVP: true},
	}
	created, err := events.Create(ctx, db, writeEvent(f.aliceCal, ev))
	if err != nil {
		t.Fatalf("creating event: %v", err)
	}

	id, err := events.SetPartStat(ctx, db, ev.UID, "bob@example.com", ical.PartStatAccepted)
	if err != nil {
		t.Fatalf("recording the answer: %v", err)
	}
	if id != created.ID {
		t.Errorf("the answer landed on event %d, want %d", id, created.ID)
	}

	got, err := events.Get(ctx, db, created.ID)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	bob, _ := findAttendee(got.Attendees, "bob@example.com")
	carol, _ := findAttendee(got.Attendees, "carol@example.com")
	if !bob.Accepts() {
		t.Errorf("bob partstat = %q, want ACCEPTED", bob.PartStat)
	}
	if carol.PartStat != ical.PartStatNeedsAction {
		t.Errorf("carol partstat = %q, an answer from bob says nothing about carol", carol.PartStat)
	}
	if got.Sequence <= created.Sequence {
		t.Errorf("sequence = %d, an answer is a change and should raise it", got.Sequence)
	}
}

// Somebody answering an event they were never invited to is a normal thing for a
// reply to discover, so it is ErrNotFound and not a crash.
func TestSetPartStatForAStranger(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	ev := meeting("s@example.com", "Meeting", utc(2026, 3, 10, 9, 0))
	ev.Attendees = []ical.Attendee{{Address: "bob@example.com"}}
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, ev)); err != nil {
		t.Fatalf("creating event: %v", err)
	}

	if _, err := events.SetPartStat(ctx, db, ev.UID, "nobody@example.com",
		ical.PartStatAccepted); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := events.SetPartStat(ctx, db, "no-such-uid@example.com",
		"bob@example.com", ical.PartStatAccepted); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown uid: err = %v, want ErrNotFound", err)
	}
}

func TestReplaceAttendeesAuthoritative(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	ev := meeting("ra@example.com", "Meeting", utc(2026, 3, 10, 9, 0))
	ev.Attendees = []ical.Attendee{
		{Address: "bob@example.com"},
		{Address: "carol@example.com"},
	}
	created, err := events.Create(ctx, db, writeEvent(f.aliceCal, ev))
	if err != nil {
		t.Fatalf("creating event: %v", err)
	}

	// The document is authoritative, so a removal really removes.
	if err := events.ReplaceAttendees(ctx, db, created.ID, []store.Attendee{
		{Address: "carol@example.com", Role: ical.RoleRequired, PartStat: ical.PartStatAccepted},
	}); err != nil {
		t.Fatalf("replacing attendees: %v", err)
	}
	got, err := events.Get(ctx, db, created.ID)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if len(got.Attendees) != 1 {
		t.Fatalf("got %d attendees, want 1", len(got.Attendees))
	}
	if got.Attendees[0].Address != "carol@example.com" {
		t.Errorf("attendee = %q, want carol", got.Attendees[0].Address)
	}

	removed, err := events.RemoveAttendee(ctx, db, created.ID, "carol@example.com")
	if err != nil {
		t.Fatalf("removing: %v", err)
	}
	if !removed {
		t.Error("removing an attendee should report that it removed something")
	}
	if again, _ := events.RemoveAttendee(ctx, db, created.ID, "carol@example.com"); again {
		t.Error("removing twice should report nothing removed the second time")
	}
}

// An attendee row with no address cannot be replied to, so it is not stored even
// though the document still carries it.
func TestAttendeeWithoutAnAddressIsNotStored(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	ev := meeting("na@example.com", "Meeting", utc(2026, 3, 10, 9, 0))
	ev.Attendees = []ical.Attendee{
		{Address: "bob@example.com"},
		{Address: "   "},
	}
	created, err := events.Create(ctx, db, writeEvent(f.aliceCal, ev))
	if err != nil {
		t.Fatalf("creating event: %v", err)
	}
	got, err := events.Get(ctx, db, created.ID)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if len(got.Attendees) != 1 || got.Attendees[0].Address != "bob@example.com" {
		t.Errorf("got %+v, want just bob", got.Attendees)
	}
}

// An attendee's view of their own calendar comes from the attendee rows, not from
// their own calendar table.
func TestAttendeeEvents(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	events := store.NewEvents()
	ctx := context.Background()

	ev := meeting("invite@example.com", "Alice's meeting", utc(2026, 3, 10, 9, 0))
	ev.Attendees = []ical.Attendee{{Address: "bob@example.com", PartStat: ical.PartStatAccepted}}
	if _, err := events.Create(ctx, db, writeEvent(f.aliceCal, ev)); err != nil {
		t.Fatalf("creating event: %v", err)
	}

	got, err := events.AttendeeEvents(ctx, db, "bob@example.com",
		utc(2026, 3, 1, 0, 0), utc(2026, 4, 1, 0, 0))
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(got) != 1 || got[0].UID != ev.UID {
		t.Errorf("got %d events, want the invitation", len(got))
	}
	// And it is not on bob's own calendar.
	onBob, err := events.ListInRange(ctx, db, []int64{f.bobCal},
		utc(2026, 3, 1, 0, 0), utc(2026, 4, 1, 0, 0))
	if err != nil {
		t.Fatalf("listing bob's calendar: %v", err)
	}
	if len(onBob) != 0 {
		t.Errorf("got %d events on bob's own calendar", len(onBob))
	}

	none, err := events.AttendeeEvents(ctx, db, "bob@example.com",
		utc(2026, 5, 1, 0, 0), utc(2026, 6, 1, 0, 0))
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("got %d events outside the range", len(none))
	}
}

// Free/busy needs every calendar a person owns, not just the default: a second
// calendar is exactly where the "offsite all week" entry lives.
func TestCalendarsForOwnersCoversEveryCalendar(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	ctx := context.Background()
	calendars := store.NewCalendars()

	second, err := calendars.Create(ctx, db, store.CreateCalendarRequest{
		Owner: strptr(f.alice), Name: "Travel",
	})
	if err != nil {
		t.Fatalf("creating second calendar: %v", err)
	}

	ids, err := store.NewEvents().CalendarsForOwners(ctx, db, []string{f.alice})
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("got %d calendar ids, want both of alice's", len(ids))
	}
	found := false
	for _, id := range ids {
		if id == second.ID {
			found = true
		}
	}
	if !found {
		t.Error("the non-default calendar was left out of the free/busy set")
	}

	// Someone with no calendars is not an error; it is an empty answer.
	empty, err := store.NewEvents().CalendarsForOwners(ctx, db, []string{"nobody@example.com"})
	if err != nil {
		t.Fatalf("resolving for a stranger: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("got %d ids for a stranger", len(empty))
	}
	if none, err := store.NewEvents().CalendarsForOwners(ctx, db, nil); err != nil || len(none) != 0 {
		t.Errorf("resolving for nobody: %v/%d", err, len(none))
	}
}

// ---------------------------------------------------------------------------
// Workgroups
// ---------------------------------------------------------------------------

func TestWorkgroupCreateAndMembership(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	groups := store.NewWorkgroups()
	ctx := context.Background()

	got, err := groups.Get(ctx, db, f.workgroup)
	if err != nil {
		t.Fatalf("loading workgroup: %v", err)
	}
	if got.Name != "support" || got.Domain != "example.com" {
		t.Errorf("name/domain = %q/%q", got.Name, got.Domain)
	}
	// A new group can be booked straight away; requiring an explicit opt-in
	// would mean every group starts unusable.
	if !got.OpenBooking {
		t.Error("open_booking should default to true")
	}

	admin, err := groups.IsAdmin(ctx, db, f.workgroup, f.alice)
	if err != nil {
		t.Fatalf("checking administration: %v", err)
	}
	if !admin {
		t.Error("alice was seeded as an admin")
	}
	memberAdmin, err := groups.IsAdmin(ctx, db, f.workgroup, f.bob)
	if err != nil {
		t.Fatalf("checking administration: %v", err)
	}
	if memberAdmin {
		t.Error("bob is a plain member and must not administer the group")
	}
	isMember, err := groups.IsMember(ctx, db, f.workgroup, f.bob)
	if err != nil {
		t.Fatalf("checking membership: %v", err)
	}
	if !isMember {
		t.Error("bob is a member")
	}

	members, err := groups.Members(ctx, db, f.workgroup)
	if err != nil {
		t.Fatalf("listing members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2", len(members))
	}
	// Admins first, so a client can show them at the top without sorting.
	if !members[0].IsAdmin() {
		t.Error("the admin should sort first")
	}
}

// Adding somebody twice is a promotion, not an error.
func TestWorkgroupAddMemberTwicePromotes(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	groups := store.NewWorkgroups()
	ctx := context.Background()

	if err := groups.AddMember(ctx, db, f.workgroup, f.bob, store.WorkgroupRoleAdmin); err != nil {
		t.Fatalf("promoting: %v", err)
	}
	member, err := groups.Member(ctx, db, f.workgroup, f.bob)
	if err != nil {
		t.Fatalf("loading membership: %v", err)
	}
	if !member.IsAdmin() {
		t.Errorf("role = %q, want admin", member.Role)
	}
	if err := groups.AddMember(ctx, db, f.workgroup, f.bob, "wizard"); err == nil {
		t.Error("an unknown role should be refused; the schema constrains it")
	}

	removed, err := groups.RemoveMember(ctx, db, f.workgroup, f.bob)
	if err != nil {
		t.Fatalf("removing: %v", err)
	}
	if !removed {
		t.Error("removing a member should report success")
	}
	if again, _ := groups.RemoveMember(ctx, db, f.workgroup, f.bob); again {
		t.Error("removing twice should report nothing removed")
	}
}

func TestWorkgroupNameIsUniquePerDomain(t *testing.T) {
	db := testMigratedDB(t)
	seedCalendarFixtures(t, db)
	groups := store.NewWorkgroups()
	ctx := context.Background()

	_, err := groups.Create(ctx, db, store.CreateWorkgroupRequest{
		Name: "support", Domain: "example.com",
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}

	// The same name in another domain is a different group.
	if _, err := groups.Create(ctx, db, store.CreateWorkgroupRequest{
		Name: "support", Domain: "other.example",
	}); err != nil {
		t.Errorf("a different domain should be allowed: %v", err)
	}
	if _, err := groups.ByName(ctx, db, "example.com", "support"); err != nil {
		t.Errorf("ByName: %v", err)
	}
	if _, err := groups.ByName(ctx, db, "example.com", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ByName for a stranger: err = %v, want ErrNotFound", err)
	}
}

func TestWorkgroupSetOpenBooking(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	groups := store.NewWorkgroups()
	ctx := context.Background()

	if err := groups.SetOpenBooking(ctx, db, f.workgroup, false); err != nil {
		t.Fatalf("closing booking: %v", err)
	}
	got, err := groups.Get(ctx, db, f.workgroup)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if got.OpenBooking {
		t.Error("open_booking = true, want it closed")
	}
	if err := groups.SetOpenBooking(ctx, db, f.workgroup+9999, true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDeletingAWorkgroupTakesItsCalendars(t *testing.T) {
	db := testMigratedDB(t)
	f := seedCalendarFixtures(t, db)
	ctx := context.Background()

	team, err := store.NewCalendars().Create(ctx, db, store.CreateCalendarRequest{
		WorkgroupID: &f.workgroup, Name: "Rota",
	})
	if err != nil {
		t.Fatalf("creating workgroup calendar: %v", err)
	}
	if err := store.NewWorkgroups().Delete(ctx, db, f.workgroup); err != nil {
		t.Fatalf("deleting workgroup: %v", err)
	}
	if _, err := store.NewCalendars().Get(ctx, db, team.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the workgroup calendar survived its workgroup: %v", err)
	}
}

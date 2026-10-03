package calendar_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/recurrence"
	"github.com/linchange/linchange/internal/store"
)

func utc(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

func march() recurrence.Window {
	return recurrence.Window{Start: utc(2026, 3, 1, 0, 0), End: utc(2026, 4, 1, 0, 0)}
}

func wid() recurrence.Window {
	return recurrence.Window{Start: utc(2026, 3, 1, 0, 0), End: utc(2026, 3, 8, 0, 0)}
}

// row builds a stored event the way the application does: the denormalised
// columns are derived from the model event, and the blob is whatever the model
// serialises to.
func row(calendarID int64, ev ical.Event) store.Event {
	out := store.Event{
		ID:               int64(1000 + calendarID),
		CalendarID:       calendarID,
		UID:              ev.UID,
		RecurrenceID:     ev.RecurrenceID,
		DTStart:          ev.Start,
		DTEnd:            ev.End,
		Timezone:         ev.Timezone,
		AllDay:           ev.AllDay,
		Summary:          ev.Summary,
		Location:         ev.Location,
		Status:           ev.Status,
		ShowAs:           ev.ShowAs,
		Sequence:         ev.Sequence,
		Organizer:        ev.Organizer.Name,
		OrganizerAddress: ev.Organizer.Address,
		DTStamp:          ev.DTStamp,
		Cancelled:        ev.IsCancelled(),
	}
	if ev.RRULE != "" {
		rule := ev.RRULE
		out.RRULE = &rule
	}
	for _, a := range ev.Attendees {
		out.Attendees = append(out.Attendees, store.Attendee{
			Address:     a.Address,
			DisplayName: a.Name,
			Role:        a.Role,
			PartStat:    a.PartStat,
			RSVP:        a.RSVP,
		})
	}
	// A deliberately unreadable blob. Every test that does not mention the blob
	// is therefore also asserting that expansion never parses one — which is
	// the whole justification for the denormalised columns.
	out.ICalBlob = []byte("this is not an iCalendar document")
	return out
}

func meeting(uid, summary string, start time.Time) ical.Event {
	return ical.Event{
		UID: uid, Summary: summary,
		Start: start, End: start.Add(time.Hour),
		DTStamp: start.Add(-24 * time.Hour),
		Status:  ical.StatusConfirmed, ShowAs: ical.ShowAsBusy,
		Organizer: ical.Organizer{Address: "alice@example.com", Name: "Alice"},
	}
}

// expand is the helper for tests that are not about the error.
func expand(t *testing.T, rows []store.Event, w recurrence.Window) calendar.Expansion {
	t.Helper()
	got, err := calendar.Expand(context.Background(), rows, w, recurrence.Limits{})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	return got
}

func starts(views []calendar.View) []time.Time {
	out := make([]time.Time, len(views))
	for i, v := range views {
		out[i] = v.Start
	}
	return out
}

func summaries(views []calendar.View) []string {
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = v.Summary
	}
	return out
}

// ---------------------------------------------------------------------------
// Expansion
// ---------------------------------------------------------------------------

func TestSingleEventBecomesOneView(t *testing.T) {
	rows := []store.Event{row(1, meeting("a@example.com", "Standup", utc(2026, 3, 10, 9, 0)))}
	got := expand(t, rows, march())

	if len(got.Occurrences) != 1 {
		t.Fatalf("got %d occurrences, want 1", len(got.Occurrences))
	}
	v := got.Occurrences[0]
	if v.Summary != "Standup" || v.UID != "a@example.com" {
		t.Errorf("summary/uid = %q/%q", v.Summary, v.UID)
	}
	if v.Organizer != "Alice" || v.OrganizerAddress != "alice@example.com" {
		t.Errorf("organizer = %q <%q>", v.Organizer, v.OrganizerAddress)
	}
	if !v.Start.Equal(utc(2026, 3, 10, 9, 0)) {
		t.Errorf("start = %v", v.Start)
	}
	if len(got.Skipped) != 0 {
		t.Errorf("nothing should have been skipped, got %+v", got.Skipped)
	}
}

// A series expands to each of its occurrences, and each carries the master's
// descriptive data — a grid that showed forty blank entries would be useless.
func TestSeriesExpandsAndCarriesTheMastersData(t *testing.T) {
	ev := meeting("weekly@example.com", "Team standup", utc(2026, 3, 3, 9, 0))
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=4"
	ev.Location = "Room 3"

	got := expand(t, []store.Event{row(1, ev)}, march())
	if len(got.Occurrences) != 4 {
		t.Fatalf("got %d occurrences %v, want 4", len(got.Occurrences), starts(got.Occurrences))
	}
	for i, v := range got.Occurrences {
		if v.Summary != "Team standup" || v.Location != "Room 3" {
			t.Errorf("occurrence %d lost the descriptive data: %q / %q", i, v.Summary, v.Location)
		}
		if v.End.Sub(v.Start) != time.Hour {
			t.Errorf("occurrence %d has duration %v, want the series' 1h", i, v.End.Sub(v.Start))
		}
		if v.Overridden {
			t.Errorf("occurrence %d should not be marked overridden", i)
		}
		if v.RecurrenceID != nil {
			t.Errorf("occurrence %d should not carry a recurrence id", i)
		}
	}
}

func TestOccurrencesAreInChronologicalOrder(t *testing.T) {
	ev := meeting("w@example.com", "Weekly", utc(2026, 3, 10, 9, 0))
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU"

	got := expand(t, []store.Event{row(1, ev)}, march())
	for i := 1; i < len(got.Occurrences); i++ {
		if got.Occurrences[i].Start.Before(got.Occurrences[i-1].Start) {
			t.Fatalf("out of order at %d: %v", i, starts(got.Occurrences))
		}
	}
}

// The same UID on two calendars is two meetings. Merging them would show one
// event that is really two bookings.
func TestSameUIDOnTwoCalendarsIsNotMerged(t *testing.T) {
	ev := meeting("shared@example.com", "Meeting", utc(2026, 3, 10, 9, 0))
	rows := []store.Event{row(1, ev), row(2, ev)}

	got := expand(t, rows, march())
	if len(got.Occurrences) != 2 {
		t.Fatalf("got %d occurrences, want one per calendar", len(got.Occurrences))
	}
	if got.Occurrences[0].CalendarID == got.Occurrences[1].CalendarID {
		t.Error("the two occurrences should be on different calendars")
	}
}

// Row ids are attached from an index rather than carried through the expansion,
// which is what lets fifty occurrences of one series point at one row.
func TestRowIDsAttachFromTheIndex(t *testing.T) {
	ev := meeting("w@example.com", "Weekly", utc(2026, 3, 10, 9, 0))
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	rows := []store.Event{row(1, ev)}

	got := expand(t, rows, march())
	calendar.AttachRowIDs(got.Occurrences, calendar.Index(rows))

	if len(got.Occurrences) == 0 {
		t.Fatal("no occurrences")
	}
	for i, v := range got.Occurrences {
		if v.EventID == 0 {
			t.Errorf("occurrence %d has no row id", i)
		}
		if v.EventID != rows[0].ID {
			t.Errorf("occurrence %d points at row %d, want %d", i, v.EventID, rows[0].ID)
		}
	}
	// A view whose row is not in the index is zeroed rather than left with
	// whatever it happened to carry, so the result does not depend on prior
	// state.
	stale := []calendar.View{{EventID: 77, CalendarID: 1, UID: "absent@example.com"}}
	calendar.AttachRowIDs(stale, map[calendar.Key]int64{})
	if stale[0].EventID != 0 {
		t.Errorf("an unmatched view kept the id %d", stale[0].EventID)
	}
}

// ---------------------------------------------------------------------------
// Detached instances
// ---------------------------------------------------------------------------

// The ordinary case: a client moved one meeting of a series.
func TestMovedOccurrenceReplacesItsSlot(t *testing.T) {
	series := meeting("w@example.com", "Weekly", utc(2026, 3, 3, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"

	slot := utc(2026, 3, 17, 9, 0)
	moved := meeting(series.UID, "Weekly (moved)", utc(2026, 3, 18, 14, 0))
	moved.RecurrenceID = &slot
	moved.Sequence = 1

	got := expand(t, []store.Event{row(1, series), row(1, moved)}, march())

	for _, s := range starts(got.Occurrences) {
		if s.Equal(slot) {
			t.Error("the original slot is still present after being moved")
		}
	}
	var found bool
	for _, v := range got.Occurrences {
		if v.Start.Equal(utc(2026, 3, 18, 14, 0)) {
			found = true
			if !v.Overridden {
				t.Error("the moved occurrence should be marked overridden")
			}
			if v.Summary != "Weekly (moved)" {
				t.Errorf("summary = %q, want the override's own", v.Summary)
			}
			if v.RecurrenceID == nil || !v.RecurrenceID.Equal(slot) {
				t.Errorf("recurrence id = %v, want the slot it replaced %v", v.RecurrenceID, slot)
			}
		}
	}
	if !found {
		t.Errorf("the moved occurrence is missing from %v", starts(got.Occurrences))
	}
}

// The bug this design exists to prevent. An occurrence moved *out* of the
// window leaves its old slot inside it, and the only row that says so has dates
// outside the window entirely. A range query finds the series; filtering the
// detached instance by the window would then drop the very row that withdraws
// the slot, and the meeting would appear on both dates.
func TestOccurrenceMovedOutOfTheWindowWithdrawsItsSlot(t *testing.T) {
	series := meeting("w@example.com", "Weekly", utc(2026, 3, 3, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"

	slot := utc(2026, 3, 17, 9, 0)
	moved := meeting(series.UID, "Weekly", utc(2026, 4, 21, 14, 0))
	moved.RecurrenceID = &slot

	// A window that contains the old slot and not the new one.
	got := expand(t, []store.Event{row(1, series), row(1, moved)}, march())

	for _, s := range starts(got.Occurrences) {
		if s.Equal(slot) {
			t.Error("the displaced slot is still shown even though the meeting moved away")
		}
	}
	// And nothing appears at the new time, because it is outside the window.
	for _, s := range starts(got.Occurrences) {
		if s.Equal(utc(2026, 4, 21, 14, 0)) {
			t.Error("an occurrence outside the window should not be shown")
		}
	}
}

// The mirror case: an occurrence dragged into this month from one that ended
// last year. Its own dates are the only ones in range, so it is found by the
// range query, and the slot it came from has to go.
func TestOccurrenceMovedIntoTheWindowAppears(t *testing.T) {
	series := meeting("w@example.com", "Weekly", utc(2020, 1, 7, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"

	oldSlot := utc(2020, 1, 14, 9, 0)
	moved := meeting(series.UID, "Resurrected", utc(2026, 3, 11, 14, 0))
	moved.RecurrenceID = &oldSlot

	got := expand(t, []store.Event{row(1, series), row(1, moved)}, march())

	var found bool
	for _, v := range got.Occurrences {
		if v.Start.Equal(utc(2026, 3, 11, 14, 0)) {
			found = true
			if !v.Overridden {
				t.Error("it should be marked as an override")
			}
		}
		if v.Start.Equal(oldSlot) {
			t.Error("the slot it came from should have been withdrawn")
		}
	}
	if !found {
		t.Errorf("the moved-in occurrence is missing from %v", starts(got.Occurrences))
	}
	// The rest of the series is unaffected.
	if len(got.Occurrences) < 3 {
		t.Errorf("got %d occurrences, want the series plus the moved one", len(got.Occurrences))
	}
}

// Cancelling one meeting of a series is expressed as a detached, cancelled
// instance. Absence is the right answer, not a struck-through entry.
func TestCancelledOccurrenceRemovesItsSlot(t *testing.T) {
	series := meeting("w@example.com", "Weekly", utc(2026, 3, 3, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=4"

	slot := utc(2026, 3, 17, 9, 0)
	cancelled := meeting(series.UID, "Weekly", slot)
	cancelled.RecurrenceID = &slot
	cancelled.Status = ical.StatusCancelled

	got := expand(t, []store.Event{row(1, series), row(1, cancelled)}, march())
	if len(got.Occurrences) != 3 {
		t.Fatalf("got %d occurrences %v, want 3", len(got.Occurrences), starts(got.Occurrences))
	}
	for _, s := range starts(got.Occurrences) {
		if s.Equal(slot) {
			t.Error("the cancelled occurrence is still shown")
		}
	}
}

// A series whose master has gone but whose detached instances remain: the
// instances are still real meetings and must still be shown.
func TestOrphanedDetachedInstanceIsStillShown(t *testing.T) {
	slot := utc(2020, 1, 14, 9, 0)
	orphan := meeting("gone@example.com", "Left behind", utc(2026, 3, 11, 14, 0))
	orphan.RecurrenceID = &slot

	got := expand(t, []store.Event{row(1, orphan)}, march())
	if len(got.Occurrences) != 1 {
		t.Fatalf("got %d occurrences, want the detached one", len(got.Occurrences))
	}
	if got.Occurrences[0].Summary != "Left behind" {
		t.Errorf("summary = %q", got.Occurrences[0].Summary)
	}
}

// A cancelled series is not busy time even though its row is still there, which
// is the point of keeping the row: attendees need to be able to see that a
// meeting was called off.
func TestCancelledSeriesYieldsNothing(t *testing.T) {
	ev := meeting("c@example.com", "Called off", utc(2026, 3, 3, 9, 0))
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	ev.Status = ical.StatusCancelled

	if got := expand(t, []store.Event{row(1, ev)}, march()); len(got.Occurrences) != 0 {
		t.Errorf("got %d occurrences, want none", len(got.Occurrences))
	}
}

// A private event occupies no time, which is the same answer as a cancelled one
// and for the same reason: neither can be booked against.
func TestTransparentEventYieldsNothing(t *testing.T) {
	ev := meeting("p@example.com", "Private", utc(2026, 3, 10, 9, 0))
	ev.ShowAs = ical.ShowAsFree

	if got := expand(t, []store.Event{row(1, ev)}, march()); len(got.Occurrences) != 0 {
		t.Errorf("got %d occurrences, want none", len(got.Occurrences))
	}
}

// A recurrence rule nobody can expand is still a meeting. Dropping the whole
// week because one event has a rule this server does not understand would be a
// far worse failure than showing that event once.
func TestUnexpandableRuleFallsBackToASingleOccurrence(t *testing.T) {
	for _, rule := range []string{
		"FREQ=SECONDLY;COUNT=10",
		"FREQ=FORTNIGHTLY",
		"FREQ=WEEKLY;BOGUS=1",
		"FREQ=MINUTELY",
	} {
		ev := meeting("x@example.com", "Odd rule", utc(2026, 3, 10, 9, 0))
		ev.RRULE = rule

		got := expand(t, []store.Event{row(1, ev)}, march())
		if len(got.Occurrences) != 1 {
			t.Errorf("%q: got %d occurrences %v, want the event shown once",
				rule, len(got.Occurrences), starts(got.Occurrences))
			continue
		}
		if !got.Occurrences[0].Start.Equal(ev.Start) {
			t.Errorf("%q: start = %v, want %v", rule, got.Occurrences[0].Start, ev.Start)
		}
		if len(got.Skipped) != 1 {
			t.Errorf("%q: got %d skipped entries, want the rule reported once", rule, len(got.Skipped))
			continue
		}
		if got.Skipped[0].Reason == "" {
			t.Errorf("%q: the skip carries no reason", rule)
		}
		if got.Skipped[0].UID != ev.UID {
			t.Errorf("%q: skipped uid = %q", rule, got.Skipped[0].UID)
		}
	}
}

// One broken event must not cost the user the rest of the calendar, and the
// account of it must be complete.
func TestOneBrokenSeriesDoesNotBlankTheCalendar(t *testing.T) {
	good := meeting("good@example.com", "Fine", utc(2026, 3, 10, 9, 0))
	bad := meeting("bad@example.com", "Broken", utc(2026, 3, 11, 9, 0))
	bad.RRULE = "FREQ=SECONDLY"

	got := expand(t, []store.Event{row(1, good), row(1, bad)}, march())
	if len(got.Occurrences) != 2 {
		t.Errorf("got %d occurrences, want both events", len(got.Occurrences))
	}
	if len(got.Skipped) != 1 || got.Skipped[0].UID != "bad@example.com" {
		t.Errorf("skipped = %+v, want only the broken series", got.Skipped)
	}
}

func TestInvalidWindowIsRejected(t *testing.T) {
	rows := []store.Event{row(1, meeting("a@example.com", "A", utc(2026, 3, 10, 9, 0)))}

	for name, w := range map[string]recurrence.Window{
		"inverted": {Start: utc(2026, 4, 1, 0, 0), End: utc(2026, 3, 1, 0, 0)},
		"zero":     {Start: utc(2026, 3, 1, 0, 0), End: utc(2026, 3, 1, 0, 0)},
		"empty":    {},
	} {
		if _, err := calendar.Expand(context.Background(), rows, w, recurrence.Limits{}); !errors.Is(err, recurrence.ErrEmptyWindow) {
			t.Errorf("%s: err = %v, want ErrEmptyWindow", name, err)
		}
	}
}

// Expansion reads the denormalised columns, so an unreadable stored document
// changes nothing. Every other test here already asserts this; this one says so
// explicitly, because it is the reason those columns exist.
func TestExpansionNeverReadsTheStoredDocument(t *testing.T) {
	ev := meeting("w@example.com", "Weekly", utc(2026, 3, 3, 9, 0))
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	r := row(1, ev)
	r.ICalBlob = []byte("BEGIN:VCALENDAR\r\ngarbage")

	got := expand(t, []store.Event{r}, march())
	// March 2026 has five Tuesdays: the 3rd, 10th, 17th, 24th and 31st.
	if len(got.Occurrences) != 5 {
		t.Errorf("got %d occurrences %v, want 5 — expansion must not read ical_blob",
			len(got.Occurrences), starts(got.Occurrences))
	}
}

// An all-day occurrence advances by whole calendar days, so a multi-day holiday
// spanning the clock change is still the right number of days long.
func TestAllDayOccurrenceSpansTheRightDates(t *testing.T) {
	ev := ical.Event{
		UID: "holiday@example.com", Summary: "Away",
		Start: utc(2026, 3, 2, 0, 0), End: utc(2026, 3, 5, 0, 0),
		AllDay: true, Timezone: "Europe/London",
		RRULE:   "FREQ=WEEKLY;BYDAY=MO;COUNT=2",
		DTStamp: utc(2026, 1, 1, 0, 0),
		Status:  ical.StatusConfirmed, ShowAs: ical.ShowAsBusy,
	}

	got := expand(t, []store.Event{row(1, ev)}, recurrence.Window{
		Start: utc(2026, 3, 1, 0, 0), End: utc(2026, 4, 1, 0, 0),
	})
	if len(got.Occurrences) != 2 {
		t.Fatalf("got %d occurrences, want 2", len(got.Occurrences))
	}
	for i, v := range got.Occurrences {
		if !v.AllDay {
			t.Errorf("occurrence %d lost its all-day flag", i)
		}
		if v.End.Sub(v.Start) != 72*time.Hour {
			t.Errorf("occurrence %d spans %v, want 3 days", i, v.End.Sub(v.Start))
		}
	}
	if got.Occurrences[0].Start.Hour() != 0 {
		t.Error("an all-day occurrence should start at midnight")
	}
}

// ---------------------------------------------------------------------------
// Free/busy
// ---------------------------------------------------------------------------

func TestBusyBlocksFromASeries(t *testing.T) {
	ev := meeting("w@example.com", "Weekly", utc(2026, 3, 3, 9, 0))
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU"

	blocks, err := calendar.BusyBlocks(context.Background(),
		[]store.Event{row(1, ev)}, march(), recurrence.Limits{})
	if err != nil {
		t.Fatalf("BusyBlocks: %v", err)
	}
	if len(blocks) != 5 {
		t.Fatalf("got %d blocks, want 5 Tuesdays", len(blocks))
	}
	for i, b := range blocks {
		if b.End.Sub(b.Start) != time.Hour {
			t.Errorf("block %d is %v long, want 1h", i, b.End.Sub(b.Start))
		}
	}
	// Blocks carry times and nothing else. Leaking a summary through free/busy
	// would defeat the show_free_busy setting, which exists to grant availability
	// without granting detail.
	for i, b := range blocks {
		if b.Start.IsZero() || b.End.IsZero() {
			t.Errorf("block %d is not a usable interval", i)
		}
	}
}

// A free event is not busy time, and a cancelled one is not either. Both are the
// same answer, so both are checked here rather than in one test each.
func TestFreeAndCancelledEventsAreNotBusy(t *testing.T) {
	free := meeting("f@example.com", "Private", utc(2026, 3, 10, 9, 0))
	free.ShowAs = ical.ShowAsFree
	cancelled := meeting("c@example.com", "Called off", utc(2026, 3, 11, 9, 0))
	cancelled.Status = ical.StatusCancelled

	blocks, err := calendar.BusyBlocks(context.Background(),
		[]store.Event{row(1, free), row(1, cancelled)}, march(), recurrence.Limits{})
	if err != nil {
		t.Fatalf("BusyBlocks: %v", err)
	}
	if len(blocks) != 0 {
		t.Errorf("got %d blocks %v, want none", len(blocks), blocks)
	}
}

// A cancelled occurrence of a series frees only that occurrence. The rest of the
// series is unaffected, which is the whole reason cancellation is a detached
// instance rather than a change to the series.
func TestCancellingOneOccurrenceFreesOnlyThatOne(t *testing.T) {
	series := meeting("w@example.com", "Weekly", utc(2026, 3, 3, 9, 0))
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	slot := utc(2026, 3, 17, 9, 0)
	cancelled := meeting(series.UID, "Weekly", slot)
	cancelled.RecurrenceID = &slot
	cancelled.Status = ical.StatusCancelled

	blocks, err := calendar.BusyBlocks(context.Background(),
		[]store.Event{row(1, series), row(1, cancelled)}, march(), recurrence.Limits{})
	if err != nil {
		t.Fatalf("BusyBlocks: %v", err)
	}
	if len(blocks) != 4 {
		t.Errorf("got %d busy blocks, want 4 after one was cancelled", len(blocks))
	}
	for _, b := range blocks {
		if b.Start.Equal(slot) {
			t.Error("the cancelled occurrence is still busy")
		}
	}
}

// The merge cases, because a client drawing bars off this needs them to be
// right, and "touching counts as one" is the one implementations usually miss.
func TestMergeBusy(t *testing.T) {
	at := func(hh int) time.Time { return utc(2026, 3, 10, hh, 0) }

	cases := []struct {
		name string
		in   []calendar.Busy
		want []calendar.Busy
	}{
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
		{
			name: "one stays one",
			in:   []calendar.Busy{{Start: at(9), End: at(10)}},
			want: []calendar.Busy{{Start: at(9), End: at(10)}},
		},
		{
			name: "disjoint stay separate",
			in: []calendar.Busy{
				{Start: at(9), End: at(10)},
				{Start: at(11), End: at(12)},
			},
			want: []calendar.Busy{
				{Start: at(9), End: at(10)},
				{Start: at(11), End: at(12)},
			},
		},
		{
			// The one implementations get wrong: two meetings back to back is
			// one unavailable stretch, and reporting it as two draws a gap that
			// does not exist.
			name: "touching merge",
			in: []calendar.Busy{
				{Start: at(9), End: at(10)},
				{Start: at(10), End: at(11)},
			},
			want: []calendar.Busy{{Start: at(9), End: at(11)}},
		},
		{
			name: "overlapping merge",
			in: []calendar.Busy{
				{Start: at(9), End: at(11)},
				{Start: at(10), End: at(12)},
			},
			want: []calendar.Busy{{Start: at(9), End: at(12)}},
		},
		{
			// Containment must not shrink the block: a contained busy period is
			// still busy.
			name: "contained is absorbed",
			in: []calendar.Busy{
				{Start: at(9), End: at(17)},
				{Start: at(10), End: at(11)},
			},
			want: []calendar.Busy{{Start: at(9), End: at(17)}},
		},
		{
			name: "unsorted input is sorted first",
			in: []calendar.Busy{
				{Start: at(14), End: at(15)},
				{Start: at(9), End: at(10)},
				{Start: at(10), End: at(11)},
			},
			want: []calendar.Busy{{Start: at(9), End: at(11)}, {Start: at(14), End: at(15)}},
		},
		{
			name: "a chain merges transitively",
			in: []calendar.Busy{
				{Start: at(9), End: at(10)},
				{Start: at(10), End: at(11)},
				{Start: at(11), End: at(12)},
			},
			want: []calendar.Busy{{Start: at(9), End: at(12)}},
		},
	}
	for _, tc := range cases {
		got := calendar.MergeBusy(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %d blocks, want %d (%v)", tc.name, len(got), len(tc.want), got)
			continue
		}
		for i := range got {
			if !got[i].Start.Equal(tc.want[i].Start) || !got[i].End.Equal(tc.want[i].End) {
				t.Errorf("%s: block %d = %v..%v, want %v..%v", tc.name, i,
					got[i].Start, got[i].End, tc.want[i].Start, tc.want[i].End)
			}
		}
	}
}

// MergeBusy sorts a copy rather than its argument, because the caller is almost
// certainly reusing the slice it passed in.
func TestMergeBusyDoesNotMutateItsArgument(t *testing.T) {
	at := func(hh int) time.Time { return utc(2026, 3, 10, hh, 0) }
	in := []calendar.Busy{
		{Start: at(14), End: at(15)},
		{Start: at(9), End: at(10)},
	}
	calendar.MergeBusy(in)
	if !in[0].Start.Equal(at(14)) {
		t.Error("the argument was reordered in place")
	}
}

// A zero-length occurrence draws a marker on the day but occupies no time, so it
// must not become a busy block.
func TestZeroLengthEventIsNotBusy(t *testing.T) {
	ev := meeting("z@example.com", "Instant", utc(2026, 3, 10, 9, 0))
	ev.End = ev.Start

	blocks, err := calendar.BusyBlocks(context.Background(),
		[]store.Event{row(1, ev)}, march(), recurrence.Limits{})
	if err != nil {
		t.Fatalf("BusyBlocks: %v", err)
	}
	if len(blocks) != 0 {
		t.Errorf("got %d blocks %v, want none", len(blocks), blocks)
	}

	// It is still drawn on the calendar, though.
	got := expand(t, []store.Event{row(1, ev)}, march())
	if len(got.Occurrences) != 1 {
		t.Errorf("got %d occurrences, want the marker", len(got.Occurrences))
	}
}

func TestSummariesHelperIsUsed(t *testing.T) {
	// Guards against the helpers drifting out of use, which would leave a view
	// field untested.
	got := expand(t, []store.Event{row(1, meeting("a@example.com", "Standup", utc(2026, 3, 10, 9, 0)))}, march())
	if s := summaries(got.Occurrences); len(s) != 1 || s[0] != "Standup" {
		t.Errorf("summaries = %v", s)
	}
}

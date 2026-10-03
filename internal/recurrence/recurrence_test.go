package recurrence_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/recurrence"
)

func utc(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

func window(start, end time.Time) recurrence.Window {
	return recurrence.Window{Start: start, End: end}
}

func timedEvent(start time.Time, dur time.Duration) ical.Event {
	return ical.Event{
		UID:      "e@example.com",
		Summary:  "Meeting",
		Start:    start,
		End:      start.Add(dur),
		DTStamp:  start.Add(-24 * time.Hour),
		Status:   ical.StatusConfirmed,
		ShowAs:   ical.ShowAsBusy,
		Sequence: 1,
	}
}

// expand is a helper that fails the test rather than returning an error, for
// the cases where an error is not what is being tested.
func expand(t *testing.T, ev ical.Event, w recurrence.Window) []recurrence.Instance {
	t.Helper()
	got, err := recurrence.Expand(context.Background(), ev, w, recurrence.Limits{})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	return got
}

func starts(in []recurrence.Instance) []time.Time {
	out := make([]time.Time, len(in))
	for i, inst := range in {
		out[i] = inst.Start
	}
	return out
}

// TestSingleEventInWindow is the base case: one event, no rule, inside the range.
func TestSingleEventInWindow(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	got := expand(t, ev, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 1, 0, 0)))

	if len(got) != 1 {
		t.Fatalf("got %d instances, want 1", len(got))
	}
	if !got[0].Start.Equal(ev.Start) || !got[0].End.Equal(ev.End) {
		t.Errorf("got %v..%v, want %v..%v", got[0].Start, got[0].End, ev.Start, ev.End)
	}
}

// TestSingleEventOutsideWindow: an event that cannot be seen is not an error,
// it is simply not there.
func TestSingleEventOutsideWindow(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	if got := expand(t, ev, window(utc(2026, 3, 1, 0, 0), utc(2026, 4, 1, 0, 0))); len(got) != 0 {
		t.Errorf("got %d instances, want none", len(got))
	}
}

// TestEventStraddlingTheWindowStart covers the boundary case that a naive
// "starts after the window" test gets wrong: a meeting that began before the
// window opened but is still running inside it is showing right now.
func TestEventStraddlingTheWindowStart(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), 2*time.Hour) // 09:00–11:00
	// A window opening at 09:30 catches it.
	got := expand(t, ev, window(utc(2026, 2, 10, 9, 30), utc(2026, 2, 10, 10, 0)))
	if len(got) != 1 {
		t.Errorf("a meeting running across the window start must be reported, got %d", len(got))
	}
	// A window that closes exactly when the meeting ends must not, because the
	// interval is half-open and the next window will own that instant.
	got = expand(t, ev, window(utc(2026, 2, 10, 12, 0), utc(2026, 2, 10, 13, 0)))
	if len(got) != 0 {
		t.Errorf("a meeting entirely after the window must not be reported, got %d", len(got))
	}
}

// TestWeeklyRule covers the ordinary case, including that COUNT is honoured
// rather than the rule running forever.
func TestWeeklyRule(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=5"

	got := expand(t, ev, window(utc(2026, 2, 1, 0, 0), utc(2026, 4, 1, 0, 0)))

	want := []time.Time{
		utc(2026, 2, 10, 9, 0), utc(2026, 2, 17, 9, 0), utc(2026, 2, 24, 9, 0),
		utc(2026, 3, 3, 9, 0), utc(2026, 3, 10, 9, 0),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d occurrences %v, want %d", len(got), starts(got), len(want))
	}
	for i := range want {
		if !got[i].Start.Equal(want[i]) {
			t.Errorf("occurrence %d = %v, want %v", i, got[i].Start, want[i])
		}
	}
}

// TestOccurrencesKeepWallClockTimeAcrossDST is the reason expansion happens in
// the event's own zone.
//
// A weekly nine o'clock London meeting runs through the March clock change. The
// UTC instant of each occurrence therefore differs by an hour, but the local
// time must not: it is 09:00 on every Tuesday. Expanding in UTC would silently
// move every meeting in the country an hour twice a year.
func TestOccurrencesKeepWallClockTimeAcrossDST(t *testing.T) {
	if _, err := time.LoadLocation("Europe/London"); err != nil {
		t.Skip("no zoneinfo available")
	}
	ev := timedEvent(utc(2026, 3, 10, 9, 0), time.Hour) // 10 March, GMT
	ev.Timezone = "Europe/London"
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=5"

	// Enough window to cross the 29 March change.
	got := expand(t, ev, window(utc(2026, 3, 1, 0, 0), utc(2026, 4, 15, 0, 0)))

	if len(got) != 5 {
		t.Fatalf("got %d occurrences %v, want 5", len(got), starts(got))
	}
	for i, inst := range got {
		local := inst.Start.In(mustLoad(t, "Europe/London"))
		if local.Hour() != 9 || local.Minute() != 0 {
			t.Errorf("occurrence %d starts at %v local, want 09:00", i, local)
		}
	}
	// And the UTC instants must actually differ, which proves the test is
	// exercising the transition rather than a zone with no DST.
	first := got[0].Start.UTC()
	last := got[len(got)-1].Start.UTC()
	if first.Hour() == last.Hour() {
		t.Errorf("UTC hour is %d for both ends; the DST transition was not crossed",
			first.Hour())
	}
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

// TestAllDayOccurrencesAdvanceByWholeDaysAcrossDST covers the companion bug for
// DATE-valued events. Adding a fixed 24 hours per day would make a holiday
// spanning the clock change 47 hours long and drift it a day earlier each
// spring.
func TestAllDayOccurrencesAdvanceByWholeDaysAcrossDST(t *testing.T) {
	// A one-day all-day event on each of the first of March, April and May.
	makeAllday := func(d time.Time) ical.Event {
		return ical.Event{
			UID:      "holiday@example.com",
			Summary:  "Day off",
			Start:    d,
			End:      d.AddDate(0, 0, 1),
			AllDay:   true,
			DTStamp:  utc(2026, 1, 1, 0, 0),
			Status:   ical.StatusConfirmed,
			ShowAs:   ical.ShowAsBusy,
			RRULE:    "FREQ=MONTHLY;BYMONTHDAY=1;COUNT=3",
			Timezone: "Europe/London",
		}
	}
	ev := makeAllday(utc(2026, 3, 1, 0, 0))

	got := expand(t, ev, window(utc(2026, 2, 1, 0, 0), utc(2026, 6, 1, 0, 0)))

	if len(got) != 3 {
		t.Fatalf("got %d occurrences %v, want 3", len(got), starts(got))
	}
	wantDays := []int{1, 1, 1}
	wantMonths := []time.Month{time.March, time.April, time.May}
	for i, inst := range got {
		if inst.Start.Day() != wantDays[i] || inst.Start.Month() != wantMonths[i] {
			t.Errorf("occurrence %d = %v, want the 1st of %v", i, inst.Start, wantMonths[i])
		}
		// Each occurrence is exactly one calendar day, so the end is the next
		// midnight and the span is 24 hours regardless of the transition.
		if got := inst.End.Sub(inst.Start); got != 24*time.Hour {
			t.Errorf("occurrence %d spans %v, want 24h", i, got)
		}
	}
}

// TestExDateRemovesAnOccurrence is how a client says "skip the 24th".
func TestExDateRemovesAnOccurrence(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU"
	ev.ExDates = []time.Time{utc(2026, 2, 24, 9, 0)}

	got := expand(t, ev, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 5, 0, 0)))

	for _, s := range starts(got) {
		if s.Equal(utc(2026, 2, 24, 9, 0)) {
			t.Error("the excluded Tuesday is still present")
		}
	}
	if len(got) != 3 { // 10th, 17th, 3 March
		t.Errorf("got %d occurrences %v, want 3", len(got), starts(got))
	}
}

// TestRDateAddsAnOccurrence is how a client says "and also the 31st".
func TestRDateAddsAnOccurrence(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=2"
	ev.RDates = []time.Time{utc(2026, 2, 19, 15, 0)}

	got := expand(t, ev, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 5, 0, 0)))

	if len(got) != 3 {
		t.Fatalf("got %d occurrences %v, want 3 (two from the rule, one added)",
			len(got), starts(got))
	}
	found := false
	for _, s := range starts(got) {
		if s.Equal(utc(2026, 2, 19, 15, 0)) {
			found = true
		}
	}
	if !found {
		t.Errorf("the RDATE occurrence is missing from %v", starts(got))
	}
}

// TestDTStartIsAlwaysTheFirstOccurrence covers RFC 5545 §3.8.5.3: DTSTART is
// the first member of the set whether or not it satisfies the rule. An
// invitation whose DTSTART is a Tuesday and whose rule says MO keeps its
// Tuesday.
func TestDTStartIsAlwaysTheFirstOccurrence(t *testing.T) {
	tuesday := utc(2026, 2, 10, 9, 0)
	if got := tuesday.Weekday(); got != time.Tuesday {
		t.Fatalf("fixture is a %v, not a Tuesday", got)
	}
	ev := timedEvent(tuesday, time.Hour)
	ev.RRULE = "FREQ=WEEKLY;BYDAY=MO"

	got := expand(t, ev, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 1, 0, 0)))

	if len(got) == 0 {
		t.Fatal("DTSTART itself must be an occurrence")
	}
	if !got[0].Start.Equal(tuesday) {
		t.Errorf("first occurrence = %v, want the DTSTART %v", got[0].Start, tuesday)
	}
}

// TestDetachedInstanceIsNotExpanded: an override that carries a RECURRENCE-ID
// but no rule is one occurrence, not a series.
func TestDetachedInstanceIsNotExpanded(t *testing.T) {
	slot := utc(2026, 2, 24, 9, 0)
	rid := utc(2026, 2, 17, 9, 0)
	ev := timedEvent(slot, time.Hour)
	ev.RecurrenceID = &rid
	ev.RRULE = ""

	got := expand(t, ev, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 1, 0, 0)))

	if len(got) != 1 {
		t.Fatalf("got %d instances, want exactly the detached one", len(got))
	}
	if !got[0].RecurrenceID.Equal(rid) {
		t.Errorf("recurrence id = %v, want %v", got[0].RecurrenceID, rid)
	}
}

// TestCancelledEventExpandsToNothing matters because a cancelled meeting must
// stop occupying time while its row survives.
func TestCancelledEventExpandsToNothing(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	ev.Status = ical.StatusCancelled

	got := expand(t, ev, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 1, 0, 0)))
	if len(got) != 0 {
		t.Errorf("a cancelled event must yield no occurrences, got %d", len(got))
	}
}

// TestTransparentEventExpandsToNothing: an event marked free is not busy time,
// so it must not appear in a busy list either.
func TestTransparentEventExpandsToNothing(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	ev.ShowAs = ical.ShowAsFree

	got := expand(t, ev, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 1, 0, 0)))
	if len(got) != 0 {
		t.Errorf("a transparent event must yield no occurrences, got %d", len(got))
	}
}

// TestResultsAreChronological: a range query feeds a grid, which assumes order.
func TestResultsAreChronological(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	ev.RRULE = "FREQ=DAILY;INTERVAL=3"

	got := expand(t, ev, window(utc(2026, 2, 1, 0, 0), utc(2026, 4, 1, 0, 0)))

	for i := 1; i < len(got); i++ {
		if got[i].Start.Before(got[i-1].Start) {
			t.Fatalf("occurrence %d (%v) precedes %d (%v)",
				i, got[i].Start, i-1, got[i-1].Start)
		}
	}
}

// TestAsEventNamesItsSlot covers what a client receives for an expanded
// occurrence: same UID, the slot identified, and no rule left to re-expand.
func TestAsEventNamesItsSlot(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=3"

	got := expand(t, ev, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 1, 0, 0)))
	if len(got) != 3 {
		t.Fatalf("got %d occurrences", len(got))
	}

	second := got[1].AsEvent()
	if second.UID != ev.UID {
		t.Errorf("uid = %q, an occurrence is the same event", second.UID)
	}
	if second.RecurrenceID == nil || !second.RecurrenceID.Equal(got[1].Start) {
		t.Errorf("recurrence id = %v, want the slot %v", second.RecurrenceID, got[1].Start)
	}
	if second.RRULE != "" {
		t.Error("an expanded occurrence must not carry the rule")
	}
	if second.Duration() != time.Hour {
		t.Errorf("duration = %v, want the series' 1h", second.Duration())
	}
}

// TestOverridesReplaceTheirSlot is the case a client hits when someone moves one
// meeting of a series: the old time must disappear, not sit there alongside the
// new one.
func TestOverridesReplaceTheirSlot(t *testing.T) {
	series := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=4"
	generated := expand(t, series, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 10, 0, 0)))
	if len(generated) != 4 {
		t.Fatalf("setup: got %d generated occurrences", len(generated))
	}

	movedTo := utc(2026, 2, 18, 14, 0)
	override := timedEvent(movedTo, 2*time.Hour)
	slot := utc(2026, 2, 17, 9, 0)
	override.RecurrenceID = &slot
	override.Summary = "Moved"

	merged := recurrence.ApplyOverrides(generated, []recurrence.Instance{{
		Event: override, Start: movedTo, End: movedTo.Add(2 * time.Hour),
		RecurrenceID: slot,
	}})

	if len(merged) != 4 {
		t.Fatalf("a replacement must not change the count, got %d", len(merged))
	}
	for _, inst := range merged {
		if inst.Start.Equal(utc(2026, 2, 17, 9, 0)) {
			t.Error("the original slot is still present after being moved")
		}
	}
	var found bool
	for _, inst := range merged {
		if inst.Start.Equal(movedTo) {
			found = true
			if !inst.Overridden {
				t.Error("the replacement should be marked as overridden")
			}
		}
	}
	if !found {
		t.Errorf("the moved occurrence is missing from %v", starts(merged))
	}
}

// TestOverrideOutsideTheSeriesIsKept: a client may materialise an occurrence the
// rule does not produce, and dropping it would lose the meeting.
func TestOverrideOutsideTheSeriesIsKept(t *testing.T) {
	series := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=2"
	generated := expand(t, series, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 1, 0, 0)))

	extra := utc(2026, 2, 26, 11, 0)
	slot := extra
	orphan := timedEvent(extra, time.Hour)
	orphan.RecurrenceID = &slot

	merged := recurrence.ApplyOverrides(generated, []recurrence.Instance{{
		Event: orphan, Start: extra, End: extra.Add(time.Hour), RecurrenceID: slot,
	}})

	if len(merged) != 3 {
		t.Fatalf("got %d instances, want the 2 generated plus the orphan", len(merged))
	}
	if merged[len(merged)-1].Start != extra {
		t.Errorf("the orphan should sort last, got %v", starts(merged))
	}
}

// TestCancelledOverrideRemovesItsSlot: deleting one meeting of a series is
// expressed as a cancellation, and absence is the correct answer.
func TestCancelledOverrideRemovesItsSlot(t *testing.T) {
	series := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=4"
	generated := expand(t, series, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 10, 0, 0)))

	slot := utc(2026, 2, 24, 9, 0)
	cancelled := timedEvent(slot, time.Hour)
	cancelled.Status = ical.StatusCancelled
	cancelled.RecurrenceID = &slot

	merged := recurrence.ApplyOverrides(generated, []recurrence.Instance{{
		Event: cancelled, Start: slot, End: slot.Add(time.Hour), RecurrenceID: slot,
	}})

	if len(merged) != 3 {
		t.Fatalf("got %d instances, want 3 after one slot is cancelled", len(merged))
	}
	for _, s := range starts(merged) {
		if s.Equal(slot) {
			t.Error("the cancelled slot is still on the calendar")
		}
	}
}

// TestApplyOverridesSortsItsResult: the replacement can land earlier than the
// occurrence it displaced, so the merge has to re-sort rather than rely on the
// order it was handed.
func TestApplyOverridesSortsItsResult(t *testing.T) {
	series := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=4"
	generated := expand(t, series, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 10, 0, 0)))

	// The last slot moves to a time before the first occurrence.
	slot := utc(2026, 3, 10, 9, 0)
	movedTo := utc(2026, 2, 9, 8, 0)
	override := timedEvent(movedTo, time.Hour)
	override.RecurrenceID = &slot

	merged := recurrence.ApplyOverrides(generated, []recurrence.Instance{{
		Event: override, Start: movedTo, End: movedTo.Add(time.Hour), RecurrenceID: slot,
	}})

	for i := 1; i < len(merged); i++ {
		if merged[i].Start.Before(merged[i-1].Start) {
			t.Fatalf("merged result is out of order at %d: %v", i, starts(merged))
		}
	}
}

// TestOverridesAreIdempotent guards the re-fetch path: merging the same
// overrides twice must not duplicate anything.
func TestOverridesAreIdempotent(t *testing.T) {
	series := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	series.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=3"
	generated := expand(t, series, window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 10, 0, 0)))

	slot := utc(2026, 2, 17, 9, 0)
	override := timedEvent(slot.Add(time.Hour), time.Hour)
	override.RecurrenceID = &slot
	o := []recurrence.Instance{{
		Event: override, Start: slot.Add(time.Hour),
		End: slot.Add(2 * time.Hour), RecurrenceID: slot,
	}}

	once := recurrence.ApplyOverrides(generated, o)
	twice := recurrence.ApplyOverrides(once, o)

	if len(once) != len(generated) {
		t.Errorf("replacing a slot changed the count: %d -> %d", len(generated), len(once))
	}
	if len(twice) != len(once) {
		t.Errorf("merging twice changed the count: %d -> %d", len(once), len(twice))
	}
}

// TestTooDenseRulesAreRefused covers the denial-of-service guard. Both of these
// are legal RFC 5545 that no calendar can display, and expanding them over a
// realistic window is enough to hang a request handler.
func TestTooDenseRulesAreRefused(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	w := window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 1, 0, 0))

	for _, rule := range []string{
		"FREQ=SECONDLY;COUNT=10",
		"FREQ=MINUTELY;COUNT=10",
		"FREQ=MINUTELY;INTERVAL=5;COUNT=10",
	} {
		ev.RRULE = rule
		_, err := recurrence.Expand(context.Background(), ev, w, recurrence.Limits{})
		if !errors.Is(err, recurrence.ErrTooDense) {
			t.Errorf("%q: err = %v, want ErrTooDense", rule, err)
		}
	}
}

// TestHourlyAndAboveAreAllowed is the other side of the same guard: refusing
// must not cost the cadences a real calendar uses.
func TestHourlyAndAboveAreAllowed(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	w := window(utc(2026, 2, 10, 0, 0), utc(2026, 2, 12, 0, 0))

	for _, rule := range []string{
		"FREQ=HOURLY;INTERVAL=6",
		"FREQ=MINUTELY;INTERVAL=120", // coarse enough to be hourly in effect
		"FREQ=DAILY",
		"FREQ=WEEKLY;BYDAY=TU",
		"FREQ=MONTHLY;BYMONTHDAY=10",
		"FREQ=YEARLY",
	} {
		ev.RRULE = rule
		if _, err := recurrence.Expand(context.Background(), ev, w, recurrence.Limits{}); err != nil {
			t.Errorf("%q was refused: %v", rule, err)
		}
	}
}

// TestOccurrenceCountIsCapped: a rule that is legal but huge must fail closed
// with an error rather than return a truncated list that looks complete.
func TestOccurrenceCountIsCapped(t *testing.T) {
	ev := timedEvent(utc(2026, 1, 1, 9, 0), time.Hour)
	ev.RRULE = "FREQ=DAILY"

	// Ten years, but only ten occurrences permitted.
	_, err := recurrence.Expand(context.Background(), ev,
		window(utc(2026, 1, 1, 0, 0), utc(2036, 1, 1, 0, 0)),
		recurrence.Limits{MaxOccurrences: 10, MaxWindow: 20 * 366 * 24 * time.Hour})
	if !errors.Is(err, recurrence.ErrTooManyOccurrences) {
		t.Errorf("err = %v, want ErrTooManyOccurrences", err)
	}
}

// TestWideWindowIsRefused is the cheaper guard: no legitimate single query needs
// a five-year window, and bounding it removes the worst case before expansion
// starts.
func TestWideWindowIsRefused(t *testing.T) {
	ev := timedEvent(utc(2026, 1, 1, 9, 0), time.Hour)
	ev.RRULE = "FREQ=DAILY"

	_, err := recurrence.Expand(context.Background(), ev,
		window(utc(2026, 1, 1, 0, 0), utc(2046, 1, 1, 0, 0)), recurrence.Limits{})
	if !errors.Is(err, recurrence.ErrWindowTooWide) {
		t.Errorf("err = %v, want ErrWindowTooWide", err)
	}
}

// TestEmptyWindowIsRejected: an inverted range would otherwise silently return
// nothing, which reads as "free".
func TestEmptyWindowIsRejected(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)

	for name, w := range map[string]recurrence.Window{
		"inverted": window(utc(2026, 3, 1, 0, 0), utc(2026, 2, 1, 0, 0)),
		"zero":     window(utc(2026, 2, 1, 0, 0), utc(2026, 2, 1, 0, 0)),
		"no end":   {Start: utc(2026, 2, 1, 0, 0)},
	} {
		if _, err := recurrence.Expand(context.Background(), ev, w, recurrence.Limits{}); !errors.Is(err, recurrence.ErrEmptyWindow) {
			t.Errorf("%s window: err = %v, want ErrEmptyWindow", name, err)
		}
	}
}

// TestInvalidRuleIsReported: a rule that cannot be understood is an error, not
// an event that quietly stops recurring.
func TestInvalidRuleIsReported(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)

	for _, rule := range []string{
		"FREQ=FORTNIGHTLY",
		"BYWDAY=TU",           // no FREQ at all
		"FREQ=WEEKLY;BOGUS=1", // unknown part
	} {
		ev.RRULE = rule
		_, err := recurrence.Expand(context.Background(), ev,
			window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 1, 0, 0)), recurrence.Limits{})
		if !errors.Is(err, recurrence.ErrInvalidRule) {
			t.Errorf("%q: err = %v, want ErrInvalidRule", rule, err)
		}
	}
}

// TestRRULEWithRedundantPrefix covers the clients that repeat the property name
// inside the value. Refusing those would drop real invitations.
func TestRRULEWithRedundantPrefix(t *testing.T) {
	ev := timedEvent(utc(2026, 2, 10, 9, 0), time.Hour)
	ev.RRULE = "RRULE:FREQ=WEEKLY;BYDAY=TU;COUNT=2"

	got, err := recurrence.Expand(context.Background(), ev,
		window(utc(2026, 2, 1, 0, 0), utc(2026, 3, 1, 0, 0)), recurrence.Limits{})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d occurrences %v, want 2", len(got), starts(got))
	}
}

// TestCancelledContextStopsExpansion matters because these queries run inside
// HTTP handlers: a client that disconnects must release the work.
func TestCancelledContextStopsExpansion(t *testing.T) {
	ev := timedEvent(utc(2026, 1, 1, 9, 0), time.Hour)
	ev.RRULE = "FREQ=DAILY"

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := recurrence.Expand(ctx, ev,
		window(utc(2026, 1, 1, 0, 0), utc(2027, 1, 1, 0, 0)), recurrence.Limits{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestZeroLimitsFallBackToDefaults: a caller who forgets to set Limits gets
// protection, not an unbounded expansion.
func TestZeroLimitsFallBackToDefaults(t *testing.T) {
	ev := timedEvent(utc(2026, 1, 1, 9, 0), time.Hour)
	ev.RRULE = "FREQ=DAILY"

	_, err := recurrence.Expand(context.Background(), ev,
		window(utc(2026, 1, 1, 0, 0), utc(2046, 1, 1, 0, 0)), recurrence.Limits{})
	if !errors.Is(err, recurrence.ErrWindowTooWide) {
		t.Errorf("a zero Limits gave err = %v, want the default window bound to apply", err)
	}
}

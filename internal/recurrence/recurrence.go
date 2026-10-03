// Package recurrence expands a stored event into the individual occurrences
// that fall inside a window of time.
//
// Expansion is the step that turns "every Tuesday at 09:00" into the Tuesdays a
// given month view has to draw, and it is where a calendar server is most
// likely to be quietly wrong. Two properties are load-bearing and both are
// pinned by tests here:
//
//   - An occurrence keeps its wall-clock time across a daylight-saving change.
//     A 09:00 meeting in Europe/London is at 09:00 on the Tuesday after the
//     clocks go forward, which means a different UTC instant. Expansion happens
//     in the event's own zone precisely so that this holds; expanding in UTC
//     would shift every meeting by an hour twice a year.
//   - Expansion is bounded. A rule is attacker-controlled text arriving over
//     SMTP or HTTP, and FREQ=SECONDLY over a decade is enough to pin a core
//     forever. Limits exist so that a hostile or merely careless rule fails
//     closed with an error instead of taking the process with it.
package recurrence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teambition/rrule-go"

	"github.com/linchange/linchange/internal/ical"
)

// Errors an expansion can fail with. All of them mean "no answer", never "an
// empty answer", so that a caller which ignores the error cannot mistake a
// refusal for a free calendar.
var (
	// ErrInvalidRule means the RRULE could not be understood at all.
	ErrInvalidRule = errors.New("recurrence: invalid RRULE")
	// ErrTooDense means the rule repeats more finely than any calendar can
	// usefully display, so it is refused rather than expanded.
	ErrTooDense = errors.New("recurrence: rule repeats too frequently to expand")
	// ErrTooManyOccurrences means the window would yield more occurrences
	// than Limits.MaxOccurrences permits.
	ErrTooManyOccurrences = errors.New("recurrence: window yields too many occurrences")
	// ErrWindowTooWide means the requested window exceeds Limits.MaxWindow.
	ErrWindowTooWide = errors.New("recurrence: window is wider than permitted")
	// ErrEmptyWindow means the window does not describe a range at all.
	ErrEmptyWindow = errors.New("recurrence: the window is empty or inverted")
)

// Window is the half-open interval [Start, End) that occurrences are wanted
// for. Half-open rather than closed because adjacent occurrences share a
// boundary instant, and a closed interval would report a meeting as running in
// both months it touches.
type Window struct {
	Start time.Time
	End   time.Time
}

// Validate reports whether the window describes a range at all.
//
// It is separate from the limits check because the two are different mistakes: an
// inverted window is malformed input, while a window wider than MaxWindow is
// well-formed but too much work.
func (w Window) Validate() error {
	if w.Start.IsZero() || w.End.IsZero() || !w.End.After(w.Start) {
		return fmt.Errorf("%w: start %s, end %s", ErrEmptyWindow, w.Start, w.End)
	}
	return nil
}

// WithinLimits reports whether the window is narrow enough to expand.
func (w Window) WithinLimits(l Limits) error {
	if err := w.Validate(); err != nil {
		return err
	}
	lim := l.OrDefault()
	if w.End.Sub(w.Start) > lim.MaxWindow {
		return fmt.Errorf("%w: %s exceeds %s", ErrWindowTooWide, w.End.Sub(w.Start), lim.MaxWindow)
	}
	return nil
}

// Limits bound the work one expansion may do. The zero value is replaced by
// DefaultLimits rather than meaning "unbounded", so a caller that forgets to
// set them gets protection instead of a hang.
type Limits struct {
	// MaxOccurrences is the most occurrences one event may produce for one
	// window.
	MaxOccurrences int
	// MaxWindow is the widest span one query may cover.
	MaxWindow time.Duration
}

// DefaultLimits are the values used when Limits is zero. They are chosen so a
// month or quarter view always works and a two-year daily view still does:
// 1000 occurrences covers about three years of daily events.
var DefaultLimits = Limits{
	MaxOccurrences: 1000,
	MaxWindow:      5 * 366 * 24 * time.Hour,
}

// OrDefault fills in any unset limit from DefaultLimits.
//
// It is exported because a caller that batches several events has to be able to
// make the same judgement about a window as the expansion it is about to run:
// checking a window against a zero MaxWindow and then letting the expansion
// substitute a default would refuse every request.
func (l Limits) OrDefault() Limits {
	if l.MaxOccurrences <= 0 {
		l.MaxOccurrences = DefaultLimits.MaxOccurrences
	}
	if l.MaxWindow <= 0 {
		l.MaxWindow = DefaultLimits.MaxWindow
	}
	return l
}

// Instance is one occurrence of an event: the master event's data with its
// times moved to this slot.
type Instance struct {
	// Event is the master event, unchanged. Callers that need the occurrence's
	// own times read Start and End, or call AsEvent.
	Event ical.Event
	// Start and End are the absolute instants this occurrence occupies.
	Start time.Time
	End   time.Time
	// RecurrenceID identifies the slot within the series. For an ordinary
	// occurrence it equals Start; for a detached override it is the slot the
	// override replaces. ApplyOverrides matches on it.
	RecurrenceID time.Time
	// Overridden reports that this instance came from a detached override
	// rather than from the rule, which a caller rendering a diff may want to
	// know.
	Overridden bool
}

// AsEvent renders the instance as a standalone event, carrying the UID and the
// RECURRENCE-ID that tie it back to the series. This is what a client that
// asked for discrete occurrences receives.
func (i Instance) AsEvent() ical.Event {
	return i.Event.Occurrence(i.Start, i.Event.AllDay)
}

// Overlaps reports whether the occurrence occupies any part of the window.
// A zero-length occurrence counts when it falls inside, which matches how a
// client draws a marker on the day itself.
func (i Instance) Overlaps(w Window) bool {
	if !i.End.After(i.Start) {
		return !i.Start.Before(w.Start) && i.Start.Before(w.End)
	}
	return i.Start.Before(w.End) && i.End.After(w.Start)
}

// Expand returns the occurrences of an event that overlap the window, in
// chronological order.
//
// A non-recurring event yields at most one instance. A recurring one yields its
// rule's occurrences plus any RDATEs, minus any EXDATEs. Detached overrides are
// not applied here — see ApplyOverrides — because they live in separate rows
// and the caller has to fetch them.
func Expand(ctx context.Context, ev ical.Event, w Window, lim Limits) ([]Instance, error) {
	lim = lim.OrDefault()
	// Checked first so that an already-cancelled request costs nothing, and so
	// that a caller which never reaches the generation loop still stops.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if w.End.IsZero() || w.Start.IsZero() || !w.End.After(w.Start) {
		return nil, ErrEmptyWindow
	}
	if w.End.Sub(w.Start) > lim.MaxWindow {
		return nil, fmt.Errorf("%w: %s exceeds %s", ErrWindowTooWide,
			w.End.Sub(w.Start), lim.MaxWindow)
	}
	if ev.Start.IsZero() {
		return nil, fmt.Errorf("%w: the event has no start", ErrInvalidRule)
	}

	// An event that is cancelled, or that declares itself free, occupies no
	// time at all. Expanding it would produce instances that every consumer
	// then has to remember to discard.
	if ev.IsFree() {
		return nil, nil
	}

	dur, days := duration(ev)

	starts, err := recurrenceStarts(ctx, ev, w, dur)
	if err != nil {
		return nil, err
	}

	out := make([]Instance, 0, len(starts))
	for _, start := range starts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// An all-day occurrence advances by whole calendar days in the event's
		// own zone. Adding a fixed 24*n hours would make a two-day holiday
		// straddle a daylight-saving boundary as 47 hours and drift a day
		// earlier every spring.
		end := start.Add(dur)
		if ev.AllDay && days > 0 {
			end = start.AddDate(0, 0, days)
		}

		inst := Instance{
			Event:        ev,
			Start:        start,
			End:          end,
			RecurrenceID: slotFor(ev, start),
		}
		if !inst.Overlaps(w) {
			continue
		}
		out = append(out, inst)
		if len(out) > lim.MaxOccurrences {
			return nil, fmt.Errorf("%w: more than %d", ErrTooManyOccurrences, lim.MaxOccurrences)
		}
	}
	return out, nil
}

// recurrenceStarts produces every candidate start for the event: the single
// DTSTART, or the rule's occurrences plus RDATEs, minus EXDATEs.
func recurrenceStarts(ctx context.Context, ev ical.Event, w Window, dur time.Duration) ([]time.Time, error) {
	// An event carrying a RECURRENCE-ID is a detached override, not a series.
	// It fills exactly one slot and never expands: re-expanding its rule would
	// resurrect every occurrence the client asked to move, and its EXDATEs and
	// RDATEs belong to the series rather than to this one occurrence.
	if ev.RecurrenceID != nil {
		return []time.Time{ev.Start}, nil
	}

	var starts []time.Time
	if ev.RRULE == "" {
		starts = []time.Time{ev.Start}
	} else {
		rule, err := parseRule(ev)
		if err != nil {
			return nil, err
		}
		// Ask only for the part of the rule that can reach the window. The
		// leading occurrence is pulled back by one duration so that an event
		// starting before the window but running into it is still returned —
		// a nine-to-ten meeting is not invisible in a window that opens at
		// 09:30.
		ruleStarts, err := ruleStarts(ctx, rule, w, dur, ev)
		if err != nil {
			return nil, err
		}
		starts = ruleStarts
	}

	starts = append(starts, ev.RDates...)
	starts = removeExcluded(starts, ev.ExDates)
	return starts, nil
}

// slotFor names the slot a generated start fills.
//
// For an ordinary occurrence the slot is the start itself. For a detached
// override it is the slot the override *replaces*, not the time it has been
// moved to — that distinction is the whole reason ApplyOverrides can find it,
// and getting it backwards would leave a moved meeting appearing at both the
// old time and the new one.
func slotFor(ev ical.Event, start time.Time) time.Time {
	if ev.RecurrenceID != nil {
		return *ev.RecurrenceID
	}
	return start
}

func parseRule(ev ical.Event) (*rrule.RRule, error) {
	text := strings.TrimSpace(ev.RRULE)
	// Some clients repeat the property name inside the value. Tolerating it
	// costs nothing and saves an invitation that would otherwise be dropped.
	text = strings.TrimPrefix(text, "RRULE:")
	text = strings.TrimPrefix(text, "rrule:")

	rule, err := rrule.StrToRRule(text)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %s", ErrInvalidRule, text, err)
	}
	if rule == nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRule, text)
	}
	if err := checkDensity(rule); err != nil {
		return nil, err
	}
	// Generation happens in the event's own zone. This is the whole reason a
	// weekly nine o'clock meeting stays at nine o'clock when the clocks
	// change: the rule walks wall-clock readings in that zone, so each
	// occurrence is the same nine o'clock and a different UTC instant.
	rule.DTStart(ev.Start.In(startZone(ev)))
	return rule, nil
}

// checkDensity refuses rules finer than hourly.
//
// A minute or second cadence cannot be drawn by any calendar interface, and
// expanding one is how an unbounded loop gets into a request handler. Hourly is
// the coarsest cadence still finer than the ones a UI renders, so refusing below
// it costs nothing real and removes the cheapest denial-of-service.
func checkDensity(rule *rrule.RRule) error {
	opts := rule.OrigOptions
	interval := opts.Interval
	if interval < 1 {
		interval = 1
	}
	switch opts.Freq {
	case rrule.SECONDLY:
		return fmt.Errorf("%w: FREQ=SECONDLY", ErrTooDense)
	case rrule.MINUTELY:
		if interval < 60 {
			return fmt.Errorf("%w: FREQ=MINUTELY;INTERVAL=%d", ErrTooDense, interval)
		}
	}
	return nil
}

func ruleStarts(ctx context.Context, rule *rrule.RRule, w Window, dur time.Duration, ev ical.Event) ([]time.Time, error) {
	// Bounds are computed in the rule's zone so the comparison and the
	// generation agree on what "the same instant" means.
	from := w.Start.Add(-dur).In(startZone(ev))
	to := w.End.In(startZone(ev))

	starts := rule.Between(from, to, true)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// RFC 5545 §3.8.5.3 makes DTSTART the first occurrence of the set whether
	// or not it matches the rule, and rrule-go, correctly enough, only
	// generates what the rule matches. An invitation whose DTSTART is a Tuesday
	// and whose rule says MO therefore keeps its Tuesday here.
	if !ev.Start.Before(from) || !ev.Start.After(to) {
		if len(starts) == 0 || !starts[0].Equal(ev.Start) {
			starts = append([]time.Time{ev.Start}, starts...)
		}
	}
	return starts, nil
}

// startZone is the location occurrences are generated in: the event's own
// timezone when it has a resolvable one, UTC otherwise. An unresolvable TZID
// must not lose the event, so it falls back rather than failing.
func startZone(ev ical.Event) *time.Location {
	if ev.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(ev.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// removeExcluded drops occurrences named by EXDATE. Comparison is on the
// instant, so an exclusion written in UTC still matches an occurrence generated
// in the event's zone.
func removeExcluded(starts []time.Time, excluded []time.Time) []time.Time {
	if len(excluded) == 0 || len(starts) == 0 {
		return starts
	}
	out := starts[:0]
	for _, s := range starts {
		drop := false
		for _, e := range excluded {
			if s.Equal(e) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, s)
		}
	}
	return out
}

// duration reports how long each occurrence lasts, in absolute time and in whole
// days. The day count is what an all-day event advances by; the absolute
// duration is what a timed one uses.
func duration(ev ical.Event) (time.Duration, int) {
	if ev.AllDay {
		days := int(ev.End.Sub(ev.Start).Hours() / 24)
		if days < 1 {
			// A zero-length all-day event still occupies a day.
			days = 1
		}
		return 24 * time.Hour, days
	}
	dur := ev.End.Sub(ev.Start)
	if dur < 0 {
		dur = 0
	}
	return dur, 0
}

// ApplyOverrides folds detached instances into the generated ones.
//
// A detached instance is one stored with a RECURRENCE-ID: the client moved or
// deleted one occurrence of a series without rewriting the series. Three things
// have to happen, and each corresponds to a way clients disagree:
//
//   - An override whose slot is in the generated set replaces that occurrence.
//     Without this, a moved meeting would appear twice, at both times.
//   - An override whose slot is outside the set is kept, because a client is
//     allowed to materialise an occurrence the rule does not produce.
//   - An override that cancels a slot removes it. A cancellation is the one
//     case where absence is the correct answer rather than a gap.
//
// The result is sorted, because a replaced occurrence can otherwise push an
// instance out of order.
func ApplyOverrides(generated, overrides []Instance) []Instance {
	if len(overrides) == 0 {
		return generated
	}
	bySlot := make(map[int64]Instance, len(overrides))
	for _, o := range overrides {
		bySlot[o.RecurrenceID.UTC().UnixNano()] = o
	}

	out := make([]Instance, 0, len(generated)+len(overrides))
	replaced := make(map[int64]bool, len(overrides))

	for _, g := range generated {
		key := g.RecurrenceID.UTC().UnixNano()
		o, ok := bySlot[key]
		if !ok {
			out = append(out, g)
			continue
		}
		replaced[key] = true
		if o.Event.IsCancelled() {
			// The slot was cancelled: it must not appear at all.
			continue
		}
		o.Overridden = true
		out = append(out, o)
	}

	// Whatever the rule did not produce still belongs on the calendar.
	for key, o := range bySlot {
		if replaced[key] {
			continue
		}
		if o.Event.IsCancelled() {
			continue
		}
		o.Overridden = true
		out = append(out, o)
	}

	sortInstances(out)
	return out
}

// sortInstances orders by start, then by recurrence id, so the order does not
// depend on map iteration. Two instances can share a start when a workgroup
// booking and a personal event coincide.
func sortInstances(in []Instance) {
	// Insertion sort: the slices here are at most a few hundred entries and
	// this avoids pulling the sort package in for one call site.
	for i := 1; i < len(in); i++ {
		for j := i; j > 0; j-- {
			if in[j].Start.Before(in[j-1].Start) ||
				(in[j].Start.Equal(in[j-1].Start) &&
					in[j].RecurrenceID.Before(in[j-1].RecurrenceID)) {
				in[j], in[j-1] = in[j-1], in[j]
				continue
			}
			break
		}
	}
}

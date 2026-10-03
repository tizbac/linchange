// Package calendar is the application layer for calendars: it turns stored rows
// into occurrences, answers free/busy queries, and applies scheduling messages.
//
// It sits between the store, which knows SQL, and the recurrence engine, which
// knows arithmetic. The part that is neither — deciding which rows belong
// together as one series, and deciding what a caller should be shown when one
// series cannot be expanded — lives here, because those are questions about the
// product rather than about either layer.
package calendar

import (
	"context"
	"errors"

	"sort"
	"time"

	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/recurrence"
	"github.com/linchange/linchange/internal/store"
)

// View is one occurrence, carrying everything a calendar grid draws.
//
// The times are the occurrence's own; everything else is the row's. A view does
// not hold the iCalendar document, because a grid does not read one and
// re-reading the document per occurrence of a weekly series would mean parsing
// the same bytes forty times for a month view.
type View struct {
	EventID    int64  `json:"eventId"`
	CalendarID int64  `json:"calendarId"`
	UID        string `json:"uid"`
	// RecurrenceID identifies the slot, and is set on a detached instance. A
	// client editing an occurrence sends it back so the edit lands on that slot
	// rather than on the series.
	RecurrenceID *time.Time `json:"recurrenceId,omitempty"`

	Summary  string    `json:"summary"`
	Location string    `json:"location,omitempty"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	AllDay   bool      `json:"allDay"`
	Timezone string    `json:"timezone"`
	Status   string    `json:"status"`

	Organizer        string `json:"organizer,omitempty"`
	OrganizerAddress string `json:"organizerAddress,omitempty"`

	// Sequence is the row's revision counter. A client sending an edit echoes
	// it back, which is how two people editing the same event are told which
	// copy they were looking at.
	Sequence int `json:"sequence"`

	// Overridden marks an occurrence that came from a detached instance rather
	// than from the series rule, so a client can distinguish it from one that
	// has merely been generated.
	Overridden bool `json:"overridden"`

	Attendees []store.Attendee `json:"attendees,omitempty"`
}

// Expansion is the result of expanding stored rows for a window.
//
// Skipped is as important as Occurrences. One event with a recurrence rule
// nobody can expand — a rule from a client this server does not understand, or
// one that repeats far too fast to be displayed — must not blank the whole
// calendar, and must not be silently dropped either: a meeting that vanishes
// from the view is worse than one shown without its repetition.
type Expansion struct {
	Occurrences []View
	// Skipped names the series that could not be expanded. Each is still
	// present in Occurrences as a single occurrence at its start.
	Skipped []SkippedSeries
}

// SkippedSeries is a series whose rule could not be expanded, and why.
type SkippedSeries struct {
	CalendarID int64  `json:"calendarId"`
	UID        string `json:"uid"`
	Summary    string `json:"summary,omitempty"`
	Err        error  `json:"-"`
	// Reason is Err's message, carried as a string so the type can be
	// marshalled for a log or an API response.
	Reason string `json:"reason"`
}

// seriesKey identifies the rows that belong to one meeting.
//
// The calendar is part of it because the same UID may exist on two calendars:
// a meeting copied onto a colleague's calendar is a separate booking even if the
// UID collides, and merging them would show one event that is really two.
type seriesKey struct {
	calendarID int64
	uid        string
}

// Expand turns stored rows into the occurrences that overlap the window.
//
// The rows must be a complete series — every master together with every
// detached instance — which is what Events.ListInRange returns. That
// completeness is what makes the two hard cases work:
//
//   - An occurrence moved *into* the window from a month ago is found by its new
//     dates, but the slot it displaced is in the original series. Both rows have
//     to be present for the swap to happen.
//   - An occurrence moved *out* of the window leaves its old slot behind with
//     nothing to withdraw it, because nothing in the window says anything about
//     it. Detached instances are therefore never filtered by the window; the
//     merged result is filtered instead.
func Expand(ctx context.Context, rows []store.Event, w recurrence.Window, lim recurrence.Limits) (Expansion, error) {
	if w.End.IsZero() || w.Start.IsZero() || !w.End.After(w.Start) {
		return Expansion{}, recurrence.ErrEmptyWindow
	}
	if err := ctx.Err(); err != nil {
		return Expansion{}, err
	}

	groups := make(map[seriesKey][]store.Event, len(rows))
	order := make([]seriesKey, 0, len(rows))
	for _, row := range rows {
		key := seriesKey{calendarID: row.CalendarID, uid: row.UID}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], row)
	}

	out := Expansion{
		Occurrences: make([]View, 0, len(rows)),
		Skipped:     make([]SkippedSeries, 0, 0),
	}

	for _, key := range order {
		if err := ctx.Err(); err != nil {
			return Expansion{}, err
		}
		views, skipped := expandSeries(ctx, groups[key], w, lim)
		if skipped != nil {
			out.Skipped = append(out.Skipped, *skipped)
		}
		for _, v := range views {
			// The calendar is stamped here rather than read off the model
			// event, because a database column has no business inside an
			// iCalendar event — and because a group is by construction one
			// calendar's rows, so the answer is already known.
			v.CalendarID = key.calendarID
			if v.OverlapsWindow(w) {
				out.Occurrences = append(out.Occurrences, v)
			}
		}
	}

	sort.SliceStable(out.Occurrences, func(i, j int) bool {
		a, b := out.Occurrences[i], out.Occurrences[j]
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		if a.CalendarID != b.CalendarID {
			return a.CalendarID < b.CalendarID
		}
		return a.UID < b.UID
	})
	return out, nil
}

// OverlapsWindow reports whether the occurrence occupies any part of the window.
// The interval is half-open, so an occurrence ending exactly as the window opens
// belongs to the window before, and one starting exactly as it closes belongs to
// the next. That is what stops a meeting being drawn twice across two adjacent
// month panes.
func (v View) OverlapsWindow(w recurrence.Window) bool {
	if !v.End.After(v.Start) {
		return !v.Start.Before(w.Start) && v.Start.Before(w.End)
	}
	return v.Start.Before(w.End) && v.End.After(w.Start)
}

func expandSeries(ctx context.Context, rows []store.Event, w recurrence.Window, lim recurrence.Limits) ([]View, *SkippedSeries) {
	// Rows arrive ordered with the master first, but a series can legitimately
	// have no master — a client may have cancelled or deleted it and left the
	// detached instances behind — so the master is looked for rather than
	// assumed.
	var master *store.Event
	overrides := make([]store.Event, 0, len(rows))
	for i := range rows {
		if rows[i].RecurrenceID == nil && master == nil {
			master = &rows[i]
			continue
		}
		overrides = append(overrides, rows[i])
	}

	var (
		generated []recurrence.Instance
		skipped   *SkippedSeries
	)
	if master != nil {
		generated, skipped = expandMaster(ctx, *master, w, lim)
	}

	// Detached instances become exactly one occurrence each, and are not
	// filtered by the window: they describe a slot, and the slot may be on
	// either side of it. Filtering them here is exactly the bug that leaves a
	// moved meeting appearing at both its old time and its new one.
	detached := make([]recurrence.Instance, 0, len(overrides))
	for _, row := range overrides {
		detached = append(detached, detachedInstance(row))
	}

	merged := recurrence.ApplyOverrides(generated, detached)

	views := make([]View, 0, len(merged))
	for _, inst := range merged {
		views = append(views, view(inst))
	}
	return views, skipped
}

// expandMaster expands the series rule, falling back to a single occurrence when
// the rule cannot be expanded.
//
// The fallback is the important part. A rule this server cannot expand is still a
// meeting, and showing that meeting on its start date is far more use than
// showing an empty week. It is reported in Expansion.Skipped rather than
// swallowed, so the operator learns about it and the user sees something.
func expandMaster(ctx context.Context, row store.Event, w recurrence.Window, lim recurrence.Limits) ([]recurrence.Instance, *SkippedSeries) {
	rule := row.RecurrenceText()
	if rule == "" && row.RecurrenceID == nil {
		// Not recurring and not detached: the single DTSTART is the whole
		// answer, and no rule is involved.
		instances, err := recurrence.Expand(ctx, expandable(row), w, lim)
		if err != nil {
			return nil, unexpandable(row, err)
		}
		return instances, nil
	}

	instances, err := recurrence.Expand(ctx, expandable(row), w, lim)
	if err == nil {
		return instances, nil
	}
	// A window problem is the caller's, not this event's, and every other series
	// in the same query will hit it too.
	if errors.Is(err, recurrence.ErrEmptyWindow) || errors.Is(err, recurrence.ErrWindowTooWide) {
		return nil, nil
	}
	// Show the event itself, without its repetition.
	fallback, fbErr := recurrence.Expand(ctx, expandable(withoutRule(row)), w, lim)
	if fbErr != nil {
		return nil, unexpandable(row, err)
	}
	return fallback, unexpandable(row, err)
}

// withoutRule returns the same row with its recurrence removed, which is what the
// fallback expansion runs on.
func withoutRule(row store.Event) store.Event {
	row.RRULE = nil
	return row
}

func unexpandable(row store.Event, err error) *SkippedSeries {
	return &SkippedSeries{
		CalendarID: row.CalendarID,
		UID:        row.UID,
		Summary:    row.Summary,
		Err:        err,
		Reason:     err.Error(),
	}
}

// detachedInstance turns a detached row into the one occurrence it stands for.
//
// It is written out rather than obtained from recurrence.Expand because an
// override is never expanded — expanding its rule would resurrect every
// occurrence the client asked to move, which is the one thing an override exists
// to prevent.
func detachedInstance(row store.Event) recurrence.Instance {
	return recurrence.Instance{
		Event:        expandable(row),
		Start:        row.DTStart,
		End:          row.DTEnd,
		RecurrenceID: *row.RecurrenceID,
	}
}

// expandable projects a stored row into the model the recurrence engine carries.
//
// This reads the denormalised columns and never the iCalendar document. That is
// the reason those columns exist: a range query must be able to expand a series
// without parsing a blob for every row it touched, and a monthly view of a busy
// calendar would otherwise parse the same document forty times.
//
// The descriptive fields ride along because an occurrence has to be rendered
// with them. The engine ignores everything it does not need, and carrying them
// here is what keeps provenance attached to an instance through
// ApplyOverrides, which otherwise has no way to say which row an occurrence came
// from.
func expandable(row store.Event) ical.Event {
	return ical.Event{
		UID:          row.UID,
		Summary:      row.Summary,
		Location:     row.Location,
		Start:        row.DTStart,
		End:          row.DTEnd,
		Timezone:     row.Timezone,
		AllDay:       row.AllDay,
		RRULE:        row.RecurrenceText(),
		RecurrenceID: row.RecurrenceID,
		Sequence:     row.Sequence,
		Status:       row.Status,
		ShowAs:       row.ShowAs,
		Organizer: ical.Organizer{
			Address: row.OrganizerAddress,
			Name:    row.Organizer,
		},
		Attendees: icalAttendees(row.Attendees),
	}
}

// icalAttendees converts stored attendees into model attendees.
func icalAttendees(in []store.Attendee) []ical.Attendee {
	if len(in) == 0 {
		return nil
	}
	out := make([]ical.Attendee, 0, len(in))
	for _, a := range in {
		out = append(out, ical.Attendee{
			Address:  a.Address,
			Name:     a.DisplayName,
			Role:     a.Role,
			PartStat: a.PartStat,
			RSVP:     a.RSVP,
		})
	}
	return out
}

// storeAttendees converts model attendees back into stored ones.
func storeAttendees(in []ical.Attendee) []store.Attendee {
	if len(in) == 0 {
		return nil
	}
	out := make([]store.Attendee, 0, len(in))
	for _, a := range in {
		out = append(out, store.Attendee{
			Address:     a.Address,
			DisplayName: a.Name,
			Role:        a.Role,
			PartStat:    a.PartStat,
			RSVP:        a.RSVP,
		})
	}
	return out
}

// Key identifies one stored row: the series it belongs to, and the slot it fills.
// It is nil-recurrence-id for a master, and carries the displaced slot for a
// detached instance.
type Key struct {
	CalendarID   int64
	UID          string
	RecurrenceID *time.Time
}

// Key identifies the row this occurrence came from.
func (v View) Key() Key {
	return Key{CalendarID: v.CalendarID, UID: v.UID, RecurrenceID: v.RecurrenceID}
}

// Index maps stored rows by the key their occurrences will carry, so a caller
// holding only views can find the row behind one.
//
// The row id lives here rather than on View for a structural reason: the
// recurrence engine carries an iCalendar event, and a database row id has no
// place in a wire model. A series with fifty occurrences needs one lookup, not
// fifty copies of the same number threaded through an expansion.
func Index(rows []store.Event) map[Key]int64 {
	out := make(map[Key]int64, len(rows))
	for _, row := range rows {
		out[Key{
			CalendarID:   row.CalendarID,
			UID:          row.UID,
			RecurrenceID: recurrenceIDKey(row.RecurrenceID),
		}] = row.ID
	}
	return out
}

// AttachRowIDs fills in each view's EventID from an index.
//
// The index is authoritative: a view with no row behind it is set to zero rather
// than left as it was, so the result does not depend on what the views happened
// to carry beforehand. A zero means the row disappeared between the query and
// the expansion, and keeping a stale id would produce a link that 404s.
func AttachRowIDs(views []View, index map[Key]int64) {
	for i := range views {
		views[i].EventID = index[views[i].Key()]
	}
}

// recurrenceIDKey normalises a nullable recurrence id into a comparable value.
//
// The stored value is converted to UTC first because two rows naming the same
// instant in different zones are the same slot, and a map keyed on the raw
// values would treat them as different occurrences.
func recurrenceIDKey(id *time.Time) *time.Time {
	if id == nil {
		return nil
	}
	utc := id.UTC()
	return &utc
}

// view renders an occurrence for a client, taking the descriptive data from the
// row the instance came from.
func view(inst recurrence.Instance) View {
	ev := inst.Event
	// A generated occurrence has no recurrence id of its own: the slot it fills
	// is its start, and naming it is only meaningful for a detached instance.
	// Leaving it nil is what lets Index find the master row for all fifty
	// occurrences of a series.
	return View{
		UID:              ev.UID,
		RecurrenceID:     recurrenceIDKey(ev.RecurrenceID),
		Summary:          ev.Summary,
		Location:         ev.Location,
		Start:            inst.Start,
		End:              inst.End,
		AllDay:           ev.AllDay,
		Timezone:         ev.Timezone,
		Status:           ev.Status,
		Sequence:         ev.Sequence,
		Organizer:        ev.Organizer.Name,
		OrganizerAddress: ev.Organizer.Address,
		Overridden:       inst.Overridden,
		Attendees:        storeAttendees(ev.Attendees),
	}
}

// ---------------------------------------------------------------------------
// Free/busy
// ---------------------------------------------------------------------------

// Busy is one stretch of unavailability.
//
// It carries times and nothing else. Free/busy is the answer to "can I book
// this?" and answering it must not leak what somebody is busy with: the whole
// point of the show_free_busy setting is that being able to see when a colleague
// is occupied is granted even when seeing why is not.
type Busy struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Availability is one person's answer.
type Availability struct {
	Address string `json:"address"`
	Busy    []Busy `json:"busy"`
}

// Busy blocks for a window: the occurrences that occupy time.
//
// Cancelled and transparent events are already excluded by expansion, so nothing
// here re-checks them: a cancelled meeting is free, and a private one occupies
// nothing, which is the same answer in both cases and the same reason.
func BusyBlocks(ctx context.Context, rows []store.Event, w recurrence.Window, lim recurrence.Limits) ([]Busy, error) {
	expansion, err := Expand(ctx, rows, w, lim)
	if err != nil {
		return nil, err
	}
	blocks := make([]Busy, 0, len(expansion.Occurrences))
	for _, v := range expansion.Occurrences {
		// A zero-length event occupies no time. Expansion keeps it so that a
		// marker on a day is drawn, but there is nothing to book against.
		if !v.End.After(v.Start) {
			continue
		}
		blocks = append(blocks, Busy{Start: v.Start, End: v.End})
	}
	return MergeBusy(blocks), nil
}

// MergeBusy coalesces overlapping and touching blocks.
//
// Touching counts, not just overlapping: 09:00–10:00 followed by 10:00–11:00 is
// one unavailable hour and a half, and reporting it as two adjacent blocks makes
// a client draw a gap between them that does not exist.
func MergeBusy(in []Busy) []Busy {
	if len(in) < 2 {
		return in
	}
	sorted := make([]Busy, len(in))
	copy(sorted, in)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Start.Equal(sorted[j].Start) {
			return sorted[i].Start.Before(sorted[j].Start)
		}
		return sorted[i].End.Before(sorted[j].End)
	})

	out := make([]Busy, 0, len(sorted))
	for _, b := range sorted {
		if len(out) == 0 {
			out = append(out, b)
			continue
		}
		last := &out[len(out)-1]
		if b.Start.After(last.End) {
			out = append(out, b)
			continue
		}
		if b.End.After(last.End) {
			// Overlapping, or contained: keep the wider end so nothing that is
			// busy gets dropped by the merge.
			last.End = b.End
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ErrNotFound is returned when a calendar or event does not exist, so a handler
// can answer 404 without inspecting the store's sentinel.
var ErrNotFound = errors.New("calendar: not found")

// ErrDenied is returned when policy refuses. It wraps the policy denial so the
// code survives to the HTTP layer.
var ErrDenied = errors.New("calendar: denied")

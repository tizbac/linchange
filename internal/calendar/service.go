package calendar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/recurrence"
	"github.com/linchange/linchange/internal/store"
)

// Service is the calendar application layer: it authorises, reads, expands and
// writes.
//
// It takes its repositories rather than building them, so a caller can compose
// several calendar operations into one transaction and so a test can substitute
// limits without a database.
type Service struct {
	calendars *store.Calendars
	events    *store.Events
	groups    *store.Workgroups
	limits    recurrence.Limits
}

// NewService returns a service over the given repositories.
func NewService(calendars *store.Calendars, events *store.Events, groups *store.Workgroups) *Service {
	return &Service{calendars: calendars, events: events, groups: groups}
}

// WithLimits returns a copy of the service using the given expansion limits.
//
// It copies rather than mutating because the limits are the one piece of state a
// caller legitimately varies — a month view and a two-year export want different
// ceilings — and a shared mutable field would make that a data race waiting for
// the first concurrent request.
func (s *Service) WithLimits(l recurrence.Limits) *Service {
	clone := *s
	clone.limits = l
	return &clone
}

// Target builds the authorization target for a calendar.
//
// Membership is resolved here rather than in the policy package because it is a
// question about the database, and a policy that issued queries would not be a
// policy. Someone who is not a member is not an error: being an outsider is the
// normal case for booking a team calendar.
func (s *Service) Target(ctx context.Context, q store.Querier, cal store.Calendar, who policy.Principal) (policy.CalendarTarget, error) {
	target := policy.CalendarTarget{ShowFreeBusy: cal.ShowFreeBusy}
	if cal.Owner != nil {
		target.Owner = *cal.Owner
		// A share with yourself is a question the WHERE clause cannot even
		// reach, so the owner's own access is not looked up: one fewer query
		// on every request to discover what the ownership rule says anyway.
		if !strings.EqualFold(*cal.Owner, who.Address) {
			if err := s.resolveShare(ctx, q, cal, who, &target); err != nil {
				return target, err
			}
		}
		return target, nil
	}
	if cal.WorkgroupID == nil {
		// The schema forbids this, so reaching it means the row was written by
		// something that is not going through here.
		return target, fmt.Errorf("calendar %d has no owner and no workgroup", cal.ID)
	}

	target.WorkgroupID = *cal.WorkgroupID
	group, err := s.groups.Get(ctx, q, *cal.WorkgroupID)
	if err != nil {
		return target, err
	}
	target.OpenBooking = group.OpenBooking

	// A team calendar can be shared too — with a group that should be able to
	// edit it without being made administrators of the organization, which is
	// a different and larger grant than writing events.
	if err := s.resolveShare(ctx, q, cal, who, &target); err != nil {
		return target, err
	}

	member, err := s.groups.Member(ctx, q, *cal.WorkgroupID, who.Address)
	switch {
	case err == nil:
		target.RequesterIsMember = true
		target.RequesterIsAdmin = member.IsAdmin()
	case errors.Is(err, store.ErrNotFound):
		// Not a member. That is an ordinary state, not a failure.
	default:
		return target, err
	}
	return target, nil
}

// resolveShare fills in what a calendar share granted this principal.
//
// One lookup answers both kinds of subject, and the group case is resolved by
// the database rather than here: a group that reaches somebody through the
// organization's domains is a fact about the directory, and re-deriving that in
// Go is how the two answers would come apart.
func (s *Service) resolveShare(ctx context.Context, q store.Querier, cal store.Calendar, who policy.Principal, target *policy.CalendarTarget) error {
	level, err := store.NewCalendarShares().AccessLevel(ctx, q, cal.ID, who.Address)
	if err != nil {
		return err
	}
	target.SharedRead = level != ""
	target.SharedWrite = level == store.CalendarShareWrite
	return nil
}

// Level answers what a principal may do with a calendar: "write" if they may
// put events on it, "read" if they may only look at it, and "" if neither.
//
// It asks the same two questions the routes ask, in the same order, rather than
// restating them: the client needs to know whether to offer an edit control,
// and a client-side rule that disagreed with the server would be the worst kind
// of bug — one that only appears after somebody clicks Save.
func (s *Service) Level(ctx context.Context, q store.Querier, who policy.Principal, cal store.Calendar) (string, error) {
	target, err := s.Target(ctx, q, cal, who)
	if err != nil {
		return "", err
	}
	if err := s.Authorize(who, policy.ActionReadCalendar, cal, target); err != nil {
		return "", nil
	}
	action := policy.ActionWriteCalendar
	if cal.IsWorkgroup() {
		// Event writes on a team calendar are bookings, matching how Create,
		// Update and Delete authorize them.
		action = policy.ActionBookWorkgroup
	}
	if err := s.Authorize(who, action, cal, target); err != nil {
		return store.CalendarShareRead, nil
	}
	return store.CalendarShareWrite, nil
}

// Authorize checks an action against a calendar and translates the policy
// refusal into this package's error, so handlers do not have to know that policy
// has a denial type.
//
// The denial is wrapped alongside ErrDenied rather than replaced by its code
// string. That keeps both facts available: errors.Is finds ErrDenied, and
// policy.CodeOf still finds the reason. Collapsing it to a string would have been
// shorter and would have thrown away the machine-readable code that the API
// contract is built on — the detail text would still be in the log, but the code
// would not be in the response.
func (s *Service) Authorize(who policy.Principal, action policy.Action, cal store.Calendar, target policy.CalendarTarget) error {
	if err := who.CanCalendar(action, target); err != nil {
		return fmt.Errorf("%w: %w", ErrDenied, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// Selector names what a calendar query covers.
type Selector struct {
	// CalendarIDs restricts the query to these calendars. When empty, the
	// principal's own calendars are used.
	CalendarIDs []int64
	// WorkgroupID adds a workgroup's calendars to the query. They are readable
	// by anybody, because a team calendar nobody can see cannot be booked.
	WorkgroupID int64
	// IncludeInvitations adds the events this principal attends that live on
	// somebody else's calendar. Without it an invitation is invisible in the
	// recipient's own view, which is the complaint every calendar user has.
	IncludeInvitations bool
}

// Result is an authorized calendar view.
type Result struct {
	Calendars   []store.Calendar `json:"calendars"`
	Occurrences []View           `json:"occurrences"`
	Skipped     []SkippedSeries  `json:"skipped,omitempty"`
}

// Query returns the occurrences a principal may see in a window.
//
// It authorises each calendar rather than filtering afterwards: a calendar the
// principal may not read is refused, not quietly omitted, because "your calendar
// is empty" and "you may not see that calendar" are different answers and only
// one of them is true.
func (s *Service) Query(ctx context.Context, q store.Querier, who policy.Principal, sel Selector, w recurrence.Window) (Result, error) {
	calendars, err := s.visibleCalendars(ctx, q, who, sel)
	if err != nil {
		return Result{}, err
	}

	ids := make([]int64, 0, len(calendars))
	for _, c := range calendars {
		ids = append(ids, c.ID)
	}

	rows, err := s.events.ListInRange(ctx, q, ids, w.Start, w.End)
	if err != nil {
		return Result{}, err
	}

	if sel.IncludeInvitations {
		invited, err := s.events.AttendeeEvents(ctx, q, who.Address, w.Start, w.End)
		if err != nil {
			return Result{}, err
		}
		rows = mergeRows(rows, invited)
	}

	expansion, err := Expand(ctx, rows, w, s.limits)
	if err != nil {
		return Result{}, err
	}
	AttachRowIDs(expansion.Occurrences, Index(rows))

	return Result{
		Calendars:   calendars,
		Occurrences: expansion.Occurrences,
		Skipped:     expansion.Skipped,
	}, nil
}

// visibleCalendars resolves a selector to the calendars a principal may read.
func (s *Service) visibleCalendars(ctx context.Context, q store.Querier, who policy.Principal, sel Selector) ([]store.Calendar, error) {
	var out []store.Calendar

	if len(sel.CalendarIDs) > 0 {
		for _, id := range sel.CalendarIDs {
			cal, err := s.calendars.Get(ctx, q, id)
			if err != nil {
				return nil, err
			}
			target, err := s.Target(ctx, q, cal, who)
			if err != nil {
				return nil, err
			}
			if err := s.Authorize(who, policy.ActionReadCalendar, cal, target); err != nil {
				return nil, err
			}
			out = append(out, cal)
		}
	} else {
		own, err := s.calendars.ByOwner(ctx, q, who.Address)
		if err != nil {
			return nil, err
		}
		for _, cal := range own {
			target, err := s.Target(ctx, q, cal, who)
			if err != nil {
				return nil, err
			}
			// The principal's own calendars are readable by construction; the
			// check is still made so that a change to the rules cannot silently
			// hide a calendar from its owner.
			if err := s.Authorize(who, policy.ActionReadCalendar, cal, target); err != nil {
				return nil, err
			}
			out = append(out, cal)
		}
	}

	if sel.WorkgroupID != 0 {
		group, err := s.calendars.ByWorkgroup(ctx, q, sel.WorkgroupID)
		if err != nil {
			return nil, err
		}
		out = append(out, group...)
	}
	return out, nil
}

// mergeRows combines two row sets by id, keeping the first of any duplicate.
//
// The overlap is real rather than theoretical: an event the principal is an
// attendee on may also sit on their own calendar, and listing it twice would
// draw it twice.
func mergeRows(first, second []store.Event) []store.Event {
	seen := make(map[int64]bool, len(first)+len(second))
	out := make([]store.Event, 0, len(first)+len(second))
	for _, batch := range [][]store.Event{first, second} {
		for _, row := range batch {
			if row.ID != 0 && seen[row.ID] {
				continue
			}
			if row.ID != 0 {
				seen[row.ID] = true
			}
			out = append(out, row)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Free/busy
// ---------------------------------------------------------------------------

// FreeBusyRequest asks when several people are busy.
type FreeBusyRequest struct {
	Addresses []string
	Window    recurrence.Window
}

// Availability answers for one address.
//
// Hidden is the important field. Reporting somebody as free when their free/busy
// is not shared with you is a lie that ends in a double booking in front of
// everyone, so a hidden address is reported as hidden rather than as empty.
type Availability struct {
	Address string `json:"address"`
	Busy    []Busy `json:"busy"`
	Hidden  bool   `json:"hidden,omitempty"`
}

// FreeBusy answers for each requested address.
//
// Addresses the principal may not see come back marked hidden rather than
// omitted, so a client can tell "free" from "unknown" and refuse to book.
func (s *Service) FreeBusy(ctx context.Context, q store.Querier, who policy.Principal, req FreeBusyRequest) ([]Availability, error) {
	out := make([]Availability, 0, len(req.Addresses))
	for _, address := range req.Addresses {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		avail, err := s.freeBusyOne(ctx, q, who, address, req.Window)
		if err != nil {
			return nil, err
		}
		out = append(out, avail)
	}
	return out, nil
}

func (s *Service) freeBusyOne(ctx context.Context, q store.Querier, who policy.Principal, address string, w recurrence.Window) (Availability, error) {
	avail := Availability{Address: address, Busy: []Busy{}}

	calendars, err := s.calendars.ByOwner(ctx, q, address)
	if err != nil {
		return avail, err
	}
	if len(calendars) == 0 {
		// Somebody with no calendars is free. That is a real answer rather than
		// a missing one.
		return avail, nil
	}

	// Every calendar the person owns contributes: a colleague who keeps "work
	// travel" on a second calendar is just as busy as on the first.
	ids := make([]int64, 0, len(calendars))
	for _, cal := range calendars {
		target, err := s.Target(ctx, q, cal, who)
		if err != nil {
			return avail, err
		}
		if err := who.CanCalendar(policy.ActionReadFreeBusy, target); err != nil {
			// Hidden, not an error and not an empty answer.
			avail.Hidden = true
			return avail, nil
		}
		ids = append(ids, cal.ID)
	}

	rows, err := s.events.ListInRange(ctx, q, ids, w.Start, w.End)
	if err != nil {
		return avail, err
	}
	blocks, err := BusyBlocks(ctx, rows, w, s.limits)
	if err != nil {
		return avail, err
	}
	avail.Busy = blocks
	return avail, nil
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// ErrStale is returned when an edit is based on a revision that is no longer
// current. Two people editing the same event is normal, and the loser of the race
// has to be told rather than allowed to overwrite the other silently.
var ErrStale = errors.New("calendar: the event has changed since it was read")

// Draft is a proposed event.
//
// It is a model event plus the calendar it belongs to. Everything the server
// insists on — a UID, an organiser, a timestamp, a sequence — is filled in by
// Create and Update rather than by the client, because a client that gets those
// wrong produces an invitation nobody can cancel.
type Draft struct {
	CalendarID int64
	Event      ical.Event
}

// Create stores a new event and returns the stored row.
//
// An event created here is a local creation, not a scheduling message: nothing
// is sent. Turning it into a REQUEST is a separate, explicit step, because a
// meeting saved without inviting anybody is a legitimate thing to want.
func (s *Service) Create(ctx context.Context, q store.Querier, who policy.Principal, draft Draft) (store.Event, error) {
	cal, err := s.writableCalendar(ctx, q, who, draft.CalendarID)
	if err != nil {
		return store.Event{}, err
	}

	ev := draft.Event
	if ev.UID == "" {
		uid, err := newUID()
		if err != nil {
			return store.Event{}, err
		}
		ev.UID = uid
	}
	if ev.Organizer.Address == "" {
		ev.Organizer.Address = who.Address
	}
	if ev.DTStamp.IsZero() {
		ev.DTStamp = time.Now()
	}
	if ev.Status == "" {
		ev.Status = ical.StatusConfirmed
	}
	if ev.Timezone == "" {
		// The calendar's zone is the sensible default, and defaulting to UTC
		// instead would put a London user's afternoon standup in the wrong
		// hour for everyone they invite.
		ev.Timezone = cal.Timezone
	}
	if ev.Sequence <= 0 {
		ev.Sequence = 1
	}
	if ev.ShowAs == "" {
		ev.ShowAs = ical.ShowAsBusy
	}

	return s.events.Create(ctx, q, writeRequest(cal.ID, who.Address, ev))
}

// Update replaces an event and returns the stored row.
//
// expectSequence is the revision the client was looking at. Passing zero skips
// the check, which is what a server-side caller that has just read the row wants;
// a client should always pass the sequence it was given.
func (s *Service) Update(ctx context.Context, q store.Querier, who policy.Principal, id int64, draft Draft, expectSequence int) (store.Event, error) {
	existing, err := s.events.Get(ctx, q, id)
	if err != nil {
		return store.Event{}, err
	}
	cal, err := s.writableCalendar(ctx, q, who, draft.CalendarID)
	if err != nil {
		return store.Event{}, err
	}
	// Moving an event between calendars is not a thing a UI does, so the row's
	// own calendar governs the permission and the draft cannot redirect the
	// event somewhere the caller has no rights on.
	if cal.ID != existing.CalendarID {
		return store.Event{}, fmt.Errorf("%w: an event cannot be moved to another calendar",
			ErrDenied)
	}
	if expectSequence > 0 && existing.Sequence != expectSequence {
		return store.Event{}, fmt.Errorf("%w: expected sequence %d, found %d",
			ErrStale, expectSequence, existing.Sequence)
	}

	ev := draft.Event
	ev.UID = existing.UID
	// The slot is the row's, not the draft's: a client editing one occurrence of
	// a series must not be able to turn it into the master or into a different
	// slot.
	ev.RecurrenceID = existing.RecurrenceID
	if ev.Organizer.Address == "" {
		ev.Organizer.Address = existing.OrganizerAddress
	}
	if ev.DTStamp.IsZero() {
		ev.DTStamp = time.Now()
	}
	if ev.Status == "" {
		ev.Status = ical.StatusConfirmed
	}
	if ev.Timezone == "" {
		ev.Timezone = existing.Timezone
	}
	if ev.ShowAs == "" {
		ev.ShowAs = ical.ShowAsBusy
	}
	// The sequence always advances, whatever the client asked for, because that
	// is the only signal the other side has that something changed.
	ev.Sequence = existing.Sequence + 1

	req := writeRequest(cal.ID, who.Address, ev)
	// A cancellation is a status, and a client editing a cancelled event back
	// into a live one has to be allowed to clear it.
	req.Cancelled = ev.IsCancelled()
	return s.events.Upsert(ctx, q, req)
}

// Cancel withdraws an event while keeping the row.
//
// The row survives so that attendees can see that a meeting was called off
// rather than that it silently vanished, which they cannot distinguish from a
// delivery failure.
func (s *Service) Cancel(ctx context.Context, q store.Querier, who policy.Principal, id int64) (store.Event, error) {
	if err := s.authorizeEventWrite(ctx, q, who, id); err != nil {
		return store.Event{}, err
	}
	return s.events.Cancel(ctx, q, id)
}

// Delete removes an event outright.
//
// It goes through the same check as Cancel: the event decides which calendar
// the request is really about, so the check cannot be pointed at a calendar the
// event never lived on. A route that deleted by id alone was, until this
// existed, a way for any signed-in person to remove anybody's event.
func (s *Service) Delete(ctx context.Context, q store.Querier, who policy.Principal, id int64) error {
	if err := s.authorizeEventWrite(ctx, q, who, id); err != nil {
		return err
	}
	return s.events.Delete(ctx, q, id)
}

// Readable returns a calendar the principal may see, or the refusal.
//
// Every route that hands back a calendar's contents goes through here, so that
// reading is one rule asked in one place rather than a check each handler
// remembers to add.
func (s *Service) Readable(ctx context.Context, q store.Querier, who policy.Principal, calendarID int64) (store.Calendar, error) {
	cal, err := s.calendars.Get(ctx, q, calendarID)
	if err != nil {
		return store.Calendar{}, err
	}
	target, err := s.Target(ctx, q, cal, who)
	if err != nil {
		return store.Calendar{}, err
	}
	if err := s.Authorize(who, policy.ActionReadCalendar, cal, target); err != nil {
		return store.Calendar{}, err
	}
	return cal, nil
}

// authorizeEventWrite checks that the principal may write to the calendar the
// event lives on. It loads the event first for that reason: the calendar is a
// property of the row, not of the URL the row was asked for.
func (s *Service) authorizeEventWrite(ctx context.Context, q store.Querier, who policy.Principal, eventID int64) error {
	existing, err := s.events.Get(ctx, q, eventID)
	if err != nil {
		return err
	}
	cal, err := s.calendars.Get(ctx, q, existing.CalendarID)
	if err != nil {
		return err
	}
	return s.authorizeWriteTo(ctx, q, who, cal)
}

// authorizeWriteTo checks the event-write rule for a calendar: the owner, a
// share that granted write, or — on a team calendar — the booking rule, which
// is what keeps a meeting room bookable by whoever needs it.
func (s *Service) authorizeWriteTo(ctx context.Context, q store.Querier, who policy.Principal, cal store.Calendar) error {
	target, err := s.Target(ctx, q, cal, who)
	if err != nil {
		return err
	}
	action := policy.ActionWriteCalendar
	if cal.IsWorkgroup() {
		// Writing an event onto a team calendar is booking, not administering
		// it: an outsider is welcome to put a meeting in the room.
		action = policy.ActionBookWorkgroup
	}
	return s.Authorize(who, action, cal, target)
}

// writableCalendar resolves a draft's calendar and checks that the principal may
// write to it.
//
// A calendar id of zero means the principal's default calendar, which is what a
// client that has not chosen one sends.
func (s *Service) writableCalendar(ctx context.Context, q store.Querier, who policy.Principal, calendarID int64) (store.Calendar, error) {
	var (
		cal store.Calendar
		err error
	)
	if calendarID == 0 {
		cal, err = s.calendars.EnsureDefault(ctx, q, who.Address, defaultTimezone())
		if err != nil {
			return cal, err
		}
	} else {
		cal, err = s.calendars.Get(ctx, q, calendarID)
		if err != nil {
			return cal, err
		}
	}
	if err := s.authorizeWriteTo(ctx, q, who, cal); err != nil {
		return cal, err
	}
	return cal, nil
}

// defaultTimezone is the zone a newly created default calendar uses.
//
// It is UTC rather than the server's local zone because the server's zone is a
// property of the host, and a calendar whose times depend on which machine
// created it is a calendar that changes meaning when the machine moves.
func defaultTimezone() string { return "UTC" }

// writeRequest projects a model event into a stored row.
//
// The document is the marshalled model and the columns are derived from it here,
// in one place, so the two can never disagree: an event whose columns described
// one thing and whose document another would be impossible to diagnose and
// impossible to fix.
func writeRequest(calendarID int64, creator string, ev ical.Event) store.WriteEventRequest {
	creatorAddr := creator
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
	if creator != "" {
		req.CreatedBy = &creatorAddr
	}
	return req
}

// newUID returns a fresh event UID.
//
// The RFC 5545 preferred form appends a domain, and it is not used here on
// purpose: the domain would suggest the address part identifies a mailbox, and
// the first implementation to rely on that would break as soon as a UID moved
// between servers. The random part is what makes it unique, and it is 16 bytes
// so that collisions do not depend on luck.
func newUID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("calendar: generating a UID: %w", err)
	}
	return hex.EncodeToString(buf[:]) + "@linchange", nil
}

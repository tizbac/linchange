package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/ical"
)

// Calendar is a container of events: either one person's, or a workgroup's.
//
// Exactly one of Owner and WorkgroupID is set, which the schema enforces. The
// two are different in what they mean rather than in how they are stored: a
// personal calendar is fully controlled by its owner, while a workgroup
// calendar can be booked into by anyone and administered only by its members.
type Calendar struct {
	ID          int64   `db:"id"`
	Owner       *string `db:"owner"`
	WorkgroupID *int64  `db:"workgroup_id"`
	Name        string  `db:"name"`
	Color       string  `db:"color"`
	// Timezone is the calendar's default zone, applied to events created
	// through this server. It is a default rather than a constraint: an event
	// carries its own TZID, because a meeting in Lisbon belongs in a London
	// user's calendar and must not be reinterpreted as nine in the morning.
	Timezone    string `db:"timezone"`
	Description string `db:"description"`
	// ShowFreeBusy is whether other users may see when this calendar is busy.
	// It mirrors Exchange's setting and is separate from whether the calendar
	// may be read: a colleague may be told "Alice is busy" without being able
	// to read what Alice is busy with.
	ShowFreeBusy bool      `db:"show_free_busy"`
	IsDefault    bool      `db:"is_default"`
	CreatedAt    time.Time `db:"created_at"`
	UpdatedAt    time.Time `db:"updated_at"`
}

// IsWorkgroup reports whether this is a shared team calendar.
func (c Calendar) IsWorkgroup() bool { return c.WorkgroupID != nil }

// CreateCalendarRequest declares a new calendar. Exactly one of Owner and
// WorkgroupID must be set; the rest have defaults so a caller creating a plain
// personal calendar only has to supply an owner.
type CreateCalendarRequest struct {
	Owner        *string
	WorkgroupID  *int64
	Name         string
	Color        string
	Timezone     string
	Description  string
	ShowFreeBusy *bool
	IsDefault    bool
}

// Calendars reads and writes the calendars table.
type Calendars struct{}

// NewCalendars returns the repository. It is stateless, so this is free.
func NewCalendars() *Calendars { return &Calendars{} }

const calendarColumns = `
	id, owner, workgroup_id, name, color, timezone, description,
	show_free_busy, is_default, created_at, updated_at`

// Create adds a calendar.
func (c *Calendars) Create(ctx context.Context, q Querier, req CreateCalendarRequest) (Calendar, error) {
	owner, workgroup, err := calendarOwner(req.Owner, req.WorkgroupID)
	if err != nil {
		return Calendar{}, err
	}
	if req.Name == "" {
		req.Name = "Calendar"
	}
	if req.Color == "" {
		req.Color = "#3b82f6"
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	showFreeBusy := true
	if req.ShowFreeBusy != nil {
		showFreeBusy = *req.ShowFreeBusy
	}

	const query = `
		INSERT INTO calendars (owner, workgroup_id, name, color, timezone,
		                       description, show_free_busy, is_default)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING ` + calendarColumns

	var out Calendar
	if err := q.GetContext(ctx, &out, query,
		owner, workgroup, req.Name, req.Color, req.Timezone,
		req.Description, showFreeBusy, req.IsDefault); err != nil {
		if IsConflict(err) {
			return Calendar{}, ErrConflict
		}
		return Calendar{}, fmt.Errorf("store: creating calendar: %w", err)
	}
	return out, nil
}

// calendarOwner validates that exactly one of owner and workgroup is set, so
// that a caller cannot create an orphan or an ambiguous calendar.
func calendarOwner(owner *string, workgroup *int64) (*string, *int64, error) {
	hasOwner := owner != nil && *owner != ""
	hasWorkgroup := workgroup != nil
	switch {
	case hasOwner && hasWorkgroup:
		return nil, nil, errors.New("store: a calendar has an owner or a workgroup, not both")
	case !hasOwner && !hasWorkgroup:
		return nil, nil, errors.New("store: a calendar needs an owner or a workgroup")
	case hasOwner:
		return owner, nil, nil
	default:
		return nil, workgroup, nil
	}
}

// Get returns a calendar by id, or ErrNotFound.
func (c *Calendars) Get(ctx context.Context, q Querier, id int64) (Calendar, error) {
	var out Calendar
	err := q.GetContext(ctx, &out,
		`SELECT `+calendarColumns+` FROM calendars WHERE id = $1`, id)
	if IsNotFound(err) {
		return Calendar{}, ErrNotFound
	}
	if err != nil {
		return Calendar{}, fmt.Errorf("store: loading calendar: %w", err)
	}
	return out, nil
}

// ByOwner returns a person's calendars, default first then by name.
func (c *Calendars) ByOwner(ctx context.Context, q Querier, owner string) ([]Calendar, error) {
	return c.list(ctx, q,
		`SELECT `+calendarColumns+` FROM calendars WHERE owner = $1
		 ORDER BY is_default DESC, name, id`, owner)
}

// ByWorkgroup returns the calendars belonging to a workgroup.
func (c *Calendars) ByWorkgroup(ctx context.Context, q Querier, workgroupID int64) ([]Calendar, error) {
	return c.list(ctx, q,
		`SELECT `+calendarColumns+` FROM calendars WHERE workgroup_id = $1
		 ORDER BY name, id`, workgroupID)
}

func (c *Calendars) list(ctx context.Context, q Querier, query string, args ...any) ([]Calendar, error) {
	rows, err := q.QueryxContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing calendars: %w", err)
	}
	defer rows.Close()

	out := make([]Calendar, 0, 8)
	for rows.Next() {
		var row Calendar
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning calendar: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating calendars: %w", err)
	}
	return out, nil
}

// DefaultFor returns a person's default calendar, or ErrNotFound when they have
// none yet. A default is what an event created with no calendar specified lands
// on, so provisioning has to guarantee one exists.
func (c *Calendars) DefaultFor(ctx context.Context, q Querier, owner string) (Calendar, error) {
	var out Calendar
	err := q.GetContext(ctx, &out,
		`SELECT `+calendarColumns+` FROM calendars
		 WHERE owner = $1 ORDER BY is_default DESC, id LIMIT 1`, owner)
	if IsNotFound(err) {
		return Calendar{}, ErrNotFound
	}
	if err != nil {
		return Calendar{}, fmt.Errorf("store: loading the default calendar: %w", err)
	}
	return out, nil
}

// EnsureDefault returns a person's default calendar, creating one if they have
// none.
//
// It is idempotent under concurrency because the whole thing runs in one
// transaction and the loser of a race sees ErrConflict and reads the winner's
// row. Two default calendars per person would break every "which calendar does
// a new event go on" question with an arbitrary answer, and the partial unique
// index would refuse the second one anyway.
func (c *Calendars) EnsureDefault(ctx context.Context, q Querier, owner, timezone string) (Calendar, error) {
	existing, err := c.DefaultFor(ctx, q, owner)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Calendar{}, err
	}

	var out Calendar
	err = InQuerierTx(ctx, q, func(tx *sqlx.Tx) error {
		// Re-read inside the transaction: another request may have created it
		// between the check outside and here.
		if found, err := c.DefaultFor(ctx, tx, owner); err == nil {
			out = found
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}

		created, err := c.Create(ctx, tx, CreateCalendarRequest{
			Owner:     &owner,
			Name:      "Calendar",
			Timezone:  timezone,
			IsDefault: true,
		})
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	if err == nil {
		return out, nil
	}
	// A concurrent creator won the race; its calendar is the default.
	if errors.Is(err, ErrConflict) {
		if existing, readErr := c.DefaultFor(ctx, q, owner); readErr == nil {
			return existing, nil
		}
	}
	return Calendar{}, err
}

// UpdateCalendarRequest changes a calendar's presentation. Identity fields —
// owner and workgroup — are deliberately not updatable: moving a calendar
// between people would silently hand over every event on it, which is not
// something a settings form should be able to do by accident.
type UpdateCalendarRequest struct {
	Name         *string
	Color        *string
	Timezone     *string
	Description  *string
	ShowFreeBusy *bool
	IsDefault    *bool
}

// Update applies the changes that were set and leaves the rest alone. A nil
// field means "unchanged", which is why every field is a pointer.
func (c *Calendars) Update(ctx context.Context, q Querier, id int64, req UpdateCalendarRequest) error {
	if req.IsDefault != nil && *req.IsDefault {
		// The partial unique index permits only one default per owner, so the
		// demotion has to happen in the same statement as the promotion.
		owner, err := c.ownerOf(ctx, q, id)
		if err != nil {
			return err
		}
		if err := c.clearDefault(ctx, q, owner, id); err != nil {
			return err
		}
	}

	res, err := q.ExecContext(ctx, `
		UPDATE calendars SET
			name         = COALESCE($2, name),
			color        = COALESCE($3, color),
			timezone     = COALESCE($4, timezone),
			description  = COALESCE($5, description),
			show_free_busy = COALESCE($6, show_free_busy),
			is_default   = COALESCE($7, is_default),
			updated_at   = now()
		WHERE id = $1`, id, req.Name, req.Color, req.Timezone,
		req.Description, req.ShowFreeBusy, req.IsDefault)
	if err != nil {
		if IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: updating calendar: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (c *Calendars) ownerOf(ctx context.Context, q Querier, id int64) (*string, error) {
	var owner *string
	err := q.GetContext(ctx, &owner, `SELECT owner FROM calendars WHERE id = $1`, id)
	if IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading calendar owner: %w", err)
	}
	return owner, nil
}

// clearDefault demotes every default calendar except keep, so that promoting
// one cannot collide with the index.
func (c *Calendars) clearDefault(ctx context.Context, q Querier, owner *string, keep int64) error {
	if owner == nil {
		// A workgroup calendar cannot be a personal default, and the index
		// only covers personal ones, so there is nothing to demote.
		return nil
	}
	if _, err := q.ExecContext(ctx,
		`UPDATE calendars SET is_default = false, updated_at = now()
		 WHERE owner = $1 AND is_default AND id <> $2`, *owner, keep); err != nil {
		return fmt.Errorf("store: clearing the default calendar: %w", err)
	}
	return nil
}

// Delete removes a calendar and everything on it.
func (c *Calendars) Delete(ctx context.Context, q Querier, id int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM calendars WHERE id = $1`, id)
	if err != nil {
		if IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: deleting calendar: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// Event is one stored calendar entry.
//
// It is a recurring *series* when RRULE is set and RecurrenceID is nil, and a
// detached *instance* when RecurrenceID is set. Both share the UID: they are the
// same meeting as far as every client and every scheduling message is concerned.
type Event struct {
	ID         int64  `db:"id"`
	CalendarID int64  `db:"calendar_id"`
	UID        string `db:"uid"`

	// RecurrenceID names the occurrence of the series this row replaces, and
	// is nil on the master. It is the slot in the *original* series, so a moved
	// occurrence can still be found by the time it used to occupy.
	RecurrenceID *time.Time `db:"recurrence_id"`

	// ICalBlob is the document this row was built from, kept verbatim so a
	// property the schema does not model still reaches another client. The
	// columns below are a query index over it, never a replacement for it.
	ICalBlob []byte `db:"ical_blob"`

	// DTStart, DTEnd, Timezone, AllDay and RRULE are the denormalised form of
	// the same data, present because a range query cannot parse iCalendar for
	// every row it touches and because expansion needs the timezone without
	// loading the blob at all.
	DTStart  time.Time `db:"dtstart"`
	DTEnd    time.Time `db:"dtend"`
	Timezone string    `db:"timezone"`
	AllDay   bool      `db:"all_day"`
	RRULE    *string   `db:"rrule"`

	Summary          string `db:"summary"`
	Location         string `db:"location"`
	Method           string `db:"method"`
	Organizer        string `db:"organizer"`
	OrganizerAddress string `db:"organizer_address"`
	Status           string `db:"status"`
	ShowAs           string `db:"show_as"`

	// Sequence is bumped on every change and is how a client, and this server
	// when it receives a scheduling message, decides which copy is newer.
	Sequence  int       `db:"sequence"`
	DTStamp   time.Time `db:"dtstamp"`
	Cancelled bool      `db:"cancelled"`

	CreatedBy *string   `db:"created_by"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`

	// Attendees is loaded only by the methods that say so; it is not a column.
	Attendees []Attendee `db:"-"`
}

// Recurs reports whether this row is a series rather than a single event.
func (e Event) Recurs() bool { return e.RRULE != nil && *e.RRULE != "" }

// IsOverride reports whether this row is a detached instance of a series.
func (e Event) IsOverride() bool { return e.RecurrenceID != nil }

// RecurrenceText is the RRULE value, or "" when the event does not recur.
func (e Event) RecurrenceText() string {
	if e.RRULE == nil {
		return ""
	}
	return *e.RRULE
}

// Attendee is one invited participant.
type Attendee struct {
	EventID     int64  `db:"event_id"`
	Address     string `db:"address"`
	DisplayName string `db:"display_name"`
	// Role and PartStat mirror the iCalendar ROLE and PARTSTAT parameters.
	Role     string `db:"role"`
	PartStat string `db:"partstat"`
	// RSVP is whether an answer was asked for. An event with no RSVP attendee
	// gets no reply, which is the difference between a meeting and a notice.
	RSVP bool `db:"rsvp"`
}

// Accepts reports whether the attendee has agreed to come, which is what
// distinguishes a confirmed booking from an unanswered invitation.
func (a Attendee) Accepts() bool { return a.PartStat == ical.PartStatAccepted }

// Events reads and writes the events and attendees tables.
type Events struct{}

// NewEvents returns the repository. It is stateless, so this is free.
func NewEvents() *Events { return &Events{} }

const eventColumns = `
	id, calendar_id, uid, recurrence_id, ical_blob, dtstart, dtend, timezone,
	all_day, rrule, summary, location, method, organizer, organizer_address,
	status, show_as, sequence, dtstamp, cancelled, created_by, created_at,
	updated_at`

// WriteEventRequest is a whole event, as the store holds it.
//
// It is a flat struct rather than a set of field pointers because an event is
// always replaced as a unit: an update that changed one property but left the
// blob describing the old one would make the denormalised columns and the
// stored document disagree, and there would be no way to tell which is right.
type WriteEventRequest struct {
	CalendarID int64
	UID        string

	RecurrenceID *time.Time
	ICalBlob     []byte
	DTStart      time.Time
	DTEnd        time.Time
	Timezone     string
	AllDay       bool
	RRULE        *string

	Summary          string
	Location         string
	Method           string
	Organizer        string
	OrganizerAddress string
	Status           string
	ShowAs           string
	Sequence         int
	DTStamp          time.Time
	Cancelled        bool
	CreatedBy        *string

	// Attendees is written in the same transaction as the event, so an event
	// never exists with no attendees or with the previous revision's.
	Attendees []Attendee
}

// Create stores a new event and its attendees.
func (e *Events) Create(ctx context.Context, q Querier, req WriteEventRequest) (Event, error) {
	if err := validateWrite(req); err != nil {
		return Event{}, err
	}

	var out Event
	err := InQuerierTx(ctx, q, func(tx *sqlx.Tx) error {
		row, err := e.insert(ctx, tx, req)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return Event{}, err
	}
	return out, nil
}

func validateWrite(req WriteEventRequest) error {
	if req.CalendarID == 0 {
		return errors.New("store: an event needs a calendar")
	}
	if strings.TrimSpace(req.UID) == "" {
		return errors.New("store: an event needs a UID")
	}
	if req.ICalBlob == nil {
		return errors.New("store: an event needs its iCalendar document")
	}
	if req.DTStart.IsZero() {
		return errors.New("store: an event needs a start")
	}
	// The schema refuses dtend < dtstart, so catching it here reports the
	// caller's mistake against the event rather than as a constraint failure.
	if req.DTEnd.Before(req.DTStart) {
		return fmt.Errorf("store: the event ends (%s) before it starts (%s)",
			req.DTEnd, req.DTStart)
	}
	return nil
}

const insertEvent = `
	INSERT INTO events (
		calendar_id, uid, recurrence_id, ical_blob, dtstart, dtend, timezone,
		all_day, rrule, summary, location, method, organizer, organizer_address,
		status, show_as, sequence, dtstamp, cancelled, created_by)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
	        $16, $17, $18, $19, $20)
	RETURNING ` + eventColumns

func (e *Events) insert(ctx context.Context, q Querier, req WriteEventRequest) (Event, error) {
	var out Event
	err := q.GetContext(ctx, &out, insertEvent,
		req.CalendarID, req.UID, req.RecurrenceID, req.ICalBlob,
		req.DTStart, req.DTEnd, req.Timezone, req.AllDay, req.RRULE,
		req.Summary, req.Location, req.Method, req.Organizer, req.OrganizerAddress,
		req.Status, req.ShowAs, req.Sequence, req.DTStamp, req.Cancelled, req.CreatedBy)
	if err != nil {
		if IsConflict(err) {
			return Event{}, ErrConflict
		}
		return Event{}, fmt.Errorf("store: inserting event: %w", err)
	}
	if err := e.replaceAttendees(ctx, q, out.ID, req.Attendees); err != nil {
		return Event{}, err
	}
	out.Attendees = req.Attendees
	return out, nil
}

// Upsert stores an event, replacing any existing row with the same
// calendar, UID and recurrence slot.
//
// It is the write half of receiving a scheduling message, where the same event
// legitimately arrives more than once. The sequence is *not* compared here: the
// caller has to decide whether an arriving message is newer, and that decision
// belongs with the iTIP rules rather than with a SQL statement.
func (e *Events) Upsert(ctx context.Context, q Querier, req WriteEventRequest) (Event, error) {
	if err := validateWrite(req); err != nil {
		return Event{}, err
	}

	var out Event
	err := InQuerierTx(ctx, q, func(tx *sqlx.Tx) error {
		// The conflict target cannot name a nullable column, so the update is
		// written as a delete-then-insert. That is safe here because both
		// statements run in this transaction and nothing outside it can see the
		// gap.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM events
			WHERE calendar_id = $1 AND uid = $2
			  AND recurrence_id IS NOT DISTINCT FROM $3`,
			req.CalendarID, req.UID, req.RecurrenceID); err != nil {
			return fmt.Errorf("store: replacing event: %w", err)
		}
		row, err := e.insert(ctx, tx, req)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return Event{}, err
	}
	return out, nil
}

// Get returns one event with its attendees, or ErrNotFound.
func (e *Events) Get(ctx context.Context, q Querier, id int64) (Event, error) {
	var out Event
	err := q.GetContext(ctx, &out,
		`SELECT `+eventColumns+` FROM events WHERE id = $1`, id)
	if IsNotFound(err) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("store: loading event: %w", err)
	}
	att, err := e.attendeesOf(ctx, q, id)
	if err != nil {
		return Event{}, err
	}
	out.Attendees = att
	return out, nil
}

// ByUID returns the master of a series, or the single event with that UID.
//
// The recurrence slot is part of the key, so a detached instance is fetched by
// passing the slot it replaces. Passing nil for an event that has overrides
// returns the master, which is what a caller who knows only the UID wants.
func (e *Events) ByUID(ctx context.Context, q Querier, calendarID int64, uid string, recurrenceID *time.Time) (Event, error) {
	var out Event
	err := q.GetContext(ctx, &out, `
		SELECT `+eventColumns+` FROM events
		WHERE calendar_id = $1 AND uid = $2 AND recurrence_id IS NOT DISTINCT FROM $3`,
		calendarID, uid, recurrenceID)
	if IsNotFound(err) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("store: loading event by UID: %w", err)
	}
	return out, nil
}

// SeriesByUID returns every row sharing a UID in a calendar: the master and all
// of its detached instances.
//
// The whole series is needed rather than the rows in a date range, because an
// occurrence that was moved into the window from outside it has its own dates in
// the window while the slot it displaced is the one that needs cancelling.
func (e *Events) SeriesByUID(ctx context.Context, q Querier, calendarID int64, uid string) ([]Event, error) {
	return e.scan(ctx, q, `
		SELECT `+eventColumns+` FROM events
		WHERE calendar_id = $1 AND uid = $2
		ORDER BY recurrence_id NULLS FIRST, dtstart, id`, calendarID, uid)
}

// ListInRange returns the events on any of the calendars that overlap the
// window, together with every detached instance of the same events.
//
// The second half is why this is a single query with a common table expression.
// A range predicate alone is wrong in two ways: an occurrence moved into the
// window from a month ago is not found by its own dates, and an occurrence moved
// *out* of the window leaves its cancelled slot behind with nothing to cancel
// it. So the range query is used only to find which UIDs are involved, and then
// every row for those UIDs is fetched. Both halves are index-backed: the first
// by the range index, the second by the UID indexes.
func (e *Events) ListInRange(ctx context.Context, q Querier, calendarIDs []int64, from, to time.Time) ([]Event, error) {
	if len(calendarIDs) == 0 {
		return nil, nil
	}
	return e.scan(ctx, q, `
		WITH candidates AS (
		    SELECT DISTINCT calendar_id, uid
		    FROM events
		    WHERE calendar_id = ANY($1)
		      AND dtstart < $3
		      AND (dtend > $2 OR dtend = dtstart)
		)
		SELECT `+prefixedColumns("e")+`
		FROM events e
		JOIN candidates c
		  ON c.calendar_id = e.calendar_id AND c.uid = e.uid
		ORDER BY e.dtstart, e.id`,
		calendarIDs, from, to)
}

// The overlap predicate above is asymmetric on purpose. An event ending exactly
// at the window's start belongs to the previous window, and an event starting
// exactly at the window's end belongs to the next one; including either would
// show a meeting twice in a calendar drawn as adjacent panes. The zero-length
// case is the exception — `dtend = dtstart` — because such an event sits inside
// the window or outside it, never across a boundary, and dropping it would make
// a marker on the day itself disappear.

// prefixedColumns qualifies eventColumns with a table alias, for queries that
// join.
func prefixedColumns(alias string) string {
	parts := strings.Split(eventColumns, ",")
	for i, p := range parts {
		fields := strings.Fields(p)
		if len(fields) == 0 {
			continue
		}
		parts[i] = alias + "." + fields[len(fields)-1]
	}
	return strings.Join(parts, ", ")
}

func (e *Events) scan(ctx context.Context, q Querier, query string, args ...any) ([]Event, error) {
	rows, err := q.QueryxContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing events: %w", err)
	}
	defer rows.Close()

	out := make([]Event, 0, 32)
	ids := make([]int64, 0, 32)
	byID := make(map[int64]int, 32)
	for rows.Next() {
		var row Event
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning event: %w", err)
		}
		byID[row.ID] = len(out)
		ids = append(ids, row.ID)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating events: %w", err)
	}

	if err := e.attachAttendees(ctx, q, ids, byID, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachAttendees loads the attendees for a page of events in one query, rather
// than one per event: a month view of thirty meetings would otherwise be sixty
// round trips.
func (e *Events) attachAttendees(ctx context.Context, q Querier, ids []int64, byID map[int64]int, out []Event) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.QueryxContext(ctx, `
		SELECT event_id, address, display_name, role, partstat, rsvp
		FROM attendees WHERE event_id = ANY($1) ORDER BY address`, ids)
	if err != nil {
		return fmt.Errorf("store: loading attendees: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var a Attendee
		if err := rows.StructScan(&a); err != nil {
			return fmt.Errorf("store: scanning attendee: %w", err)
		}
		if i, ok := byID[a.EventID]; ok {
			out[i].Attendees = append(out[i].Attendees, a)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterating attendees: %w", err)
	}
	return nil
}

func (e *Events) attendeesOf(ctx context.Context, q Querier, eventID int64) ([]Attendee, error) {
	out := make([]Attendee, 0, 4)
	err := q.SelectContext(ctx, &out, `
		SELECT event_id, address, display_name, role, partstat, rsvp
		FROM attendees WHERE event_id = $1 ORDER BY address`, eventID)
	if IsNotFound(err) {
		return []Attendee{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading attendees: %w", err)
	}
	return out, nil
}

// Cancel withdraws an event while keeping the row.
//
// Deleting instead would leave every attendee holding a meeting that silently
// vanished, with no way to tell a cancellation from a delivery failure. The
// sequence is bumped so that a client which already has the event recognises
// the withdrawal as the newer revision.
func (e *Events) Cancel(ctx context.Context, q Querier, id int64) (Event, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE events
		SET cancelled = true, status = $2, sequence = sequence + 1, updated_at = now()
		WHERE id = $1`, id, ical.StatusCancelled)
	if err != nil {
		return Event{}, fmt.Errorf("store: cancelling event: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Event{}, ErrNotFound
	}
	return e.Get(ctx, q, id)
}

// Delete removes an event for good. It is for a user discarding their own event,
// not for a cancellation; use Cancel for anything another party has to be told
// about.
func (e *Events) Delete(ctx context.Context, q Querier, id int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM events WHERE id = $1`, id)
	if err != nil {
		if IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: deleting event: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Attendees
// ---------------------------------------------------------------------------

func (e *Events) replaceAttendees(ctx context.Context, q Querier, eventID int64, attendees []Attendee) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM attendees WHERE event_id = $1`, eventID); err != nil {
		return fmt.Errorf("store: clearing attendees: %w", err)
	}
	for _, a := range attendees {
		if strings.TrimSpace(a.Address) == "" {
			// An attendee row with no address cannot be replied to, so it is
			// not stored. The document still carries it.
			continue
		}
		role := a.Role
		if role == "" {
			role = ical.RoleRequired
		}
		partstat := a.PartStat
		if partstat == "" {
			partstat = ical.PartStatNeedsAction
		}
		if _, err := q.ExecContext(ctx, `
			INSERT INTO attendees (event_id, address, display_name, role, partstat, rsvp)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (event_id, address) DO UPDATE
			SET display_name = EXCLUDED.display_name,
			    role         = EXCLUDED.role,
			    partstat     = EXCLUDED.partstat,
			    rsvp         = EXCLUDED.rsvp`,
			eventID, a.Address, a.DisplayName, role, partstat, a.RSVP); err != nil {
			if IsConflict(err) {
				return fmt.Errorf("store: adding attendee %s: %w", a.Address, ErrConflict)
			}
			return fmt.Errorf("store: adding attendee: %w", err)
		}
	}
	return nil
}

// ReplaceAttendees sets the full attendee list of an event.
//
// This is how an organiser's own edit reaches the database: the attendee list is
// authoritative in the document, so it is replaced wholesale rather than
// merged, and a removal really does remove.
func (e *Events) ReplaceAttendees(ctx context.Context, q Querier, eventID int64, attendees []Attendee) error {
	return e.replaceAttendees(ctx, q, eventID, attendees)
}

// SetPartStat records one attendee's answer.
//
// It is the reply path. It matches on the UID's master event, because a REPLY
// carries the UID and the slot rather than the row id, and an answer about a
// detached occurrence updates the series. ErrNotFound means nobody here is
// invited to that event, which is a normal thing for a reply to discover and not
// an error worth alerting on.
func (e *Events) SetPartStat(ctx context.Context, q Querier, uid string, address, partstat string) (int64, error) {
	var id int64
	err := q.GetContext(ctx, &id, `
		UPDATE events SET sequence = sequence + 1, updated_at = now()
		WHERE id = (
		    SELECT e.id FROM events e
		    WHERE e.uid = $1
		      AND EXISTS (SELECT 1 FROM attendees a
		                  WHERE a.event_id = e.id AND a.address = $2)
		    ORDER BY e.recurrence_id NULLS FIRST, e.id
		    LIMIT 1)
		RETURNING id`, uid, address)
	if IsNotFound(err) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: recording an attendance reply: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE attendees SET partstat = $3 WHERE event_id = $1 AND address = $2`,
		id, address, partstat); err != nil {
		return 0, fmt.Errorf("store: updating an attendee: %w", err)
	}
	return id, nil
}

// RemoveAttendee takes one address off an event's guest list.
func (e *Events) RemoveAttendee(ctx context.Context, q Querier, eventID int64, address string) (bool, error) {
	res, err := q.ExecContext(ctx,
		`DELETE FROM attendees WHERE event_id = $1 AND address = $2`, eventID, address)
	if err != nil {
		return false, fmt.Errorf("store: removing an attendee: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AttendeeEvents returns the events an address has been invited to that overlap
// the window.
//
// This is the "my calendar" view as seen by an attendee rather than an
// organiser: the meetings live on someone else's calendar, and the only thing
// linking them to this address is the attendee row.
func (e *Events) AttendeeEvents(ctx context.Context, q Querier, address string, from, to time.Time) ([]Event, error) {
	return e.scan(ctx, q, `
		SELECT `+prefixedColumns("e")+`
		FROM events e
		WHERE e.id IN (
		    SELECT event_id FROM attendees WHERE address = $1)
		  AND e.dtstart < $3
		  AND (e.dtend > $2 OR e.dtend = e.dtstart)
		ORDER BY e.dtstart, e.id`,
		address, from, to)
}

// CalendarsForOwners resolves a set of addresses to the calendar ids that hold
// their free/busy information.
//
// A person may have several calendars and all of them contribute, so this
// returns every one of them rather than only the default: a colleague who keeps
// "Work travel" on a second calendar is just as busy as on the first.
func (e *Events) CalendarsForOwners(ctx context.Context, q Querier, owners []string) ([]int64, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(owners))
	err := q.SelectContext(ctx, &ids,
		`SELECT id FROM calendars WHERE owner = ANY($1)`, owners)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: resolving calendars for owners: %w", err)
	}
	return ids, nil
}

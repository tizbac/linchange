package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Share subject kinds: a share names either a person or one of a group's
// groups. They are separate columns rather than one polymorphic id because the
// two reference different tables, and the pair of CHECK constraints is what
// keeps a row from claiming one and carrying the other.
const (
	// ShareSubjectUser shares with one address.
	ShareSubjectUser = "user"
	// ShareSubjectGroup shares with every address a group expands to.
	ShareSubjectGroup = "group"
)

// Calendar share levels, ordered: a write share includes everything a read
// share does.
const (
	// CalendarShareRead may see the calendar's events.
	CalendarShareRead = "read"
	// CalendarShareWrite may also create, change and delete them.
	CalendarShareWrite = "write"
)

// Calendar share statuses.
//
// The difference is one of who decided: a share the organization's
// administrator made is the organization sharing its own calendar, and it
// grants the moment it is written. A share a colleague made through their own
// calendar is an offer, and it grants nothing until the person it names has
// said yes.
const (
	// CalendarSharePending is an offer nobody has answered yet.
	CalendarSharePending = "pending"
	// CalendarShareActive grants the level it names.
	CalendarShareActive = "active"
)

// ShareDecisions a subject can give an offer.
const (
	// ShareDecisionAccepted grants the share's level from then on.
	ShareDecisionAccepted = "accepted"
	// ShareDecisionDeclined leaves the share granting nothing, and closes the
	// offer so it stops asking.
	ShareDecisionDeclined = "declined"
)

// ShareLevelAtLeast reports whether one level includes another.
//
// The comparison is by rank rather than alphabetically, so that reordering the
// constants or renaming one cannot silently invert every decision made here.
func ShareLevelAtLeast(level, want string) bool {
	rank := func(l string) int {
		switch l {
		case CalendarShareRead:
			return 1
		case CalendarShareWrite:
			return 2
		default:
			return 0
		}
	}
	return rank(level) >= rank(want) && rank(want) > 0
}

// CalendarShare grants one subject a level of access to one calendar.
//
// The subject is a person or a group, never both and never neither — the
// schema enforces that, and this struct carries pointers so that the row shape
// survives the trip to the client unchanged.
type CalendarShare struct {
	ID             int64   `db:"id"`
	CalendarID     int64   `db:"calendar_id"`
	SubjectKind    string  `db:"subject_kind"`
	SubjectAddress *string `db:"subject_address"`
	SubjectGroupID *int64  `db:"subject_group_id"`
	Level          string  `db:"level"`
	// Status is pending until a share made by a person has been accepted by
	// the subject it names. An administrator's share is active from the
	// moment it is written.
	Status    string    `db:"status"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// IsUser reports whether this share names a person.
func (s CalendarShare) IsUser() bool { return s.SubjectKind == ShareSubjectUser }

// GroupID returns the group a group share names, or zero for a person.
func (s CalendarShare) GroupID() int64 {
	if s.SubjectGroupID == nil {
		return 0
	}
	return *s.SubjectGroupID
}

// CalendarShares reads and writes the calendar_shares table. It is stateless.
type CalendarShares struct{}

// NewCalendarShares returns the repository.
func NewCalendarShares() *CalendarShares { return &CalendarShares{} }

const calendarShareColumns = `id, calendar_id, subject_kind, subject_address,
	subject_group_id, level, status, created_at, updated_at`

// grantedClause decides whether a share is worth granting its subject: it is
// active, or the person it names accepted an offer that was pending.
//
// Every lookup that answers "may this address use this calendar" asks it, so
// the list of shared calendars, the access level on one calendar, and the
// events inside it cannot come to different answers about a share that is
// still waiting for its answer.
//
// The address placeholder is passed in because the two queries that use it
// count their parameters differently: one asks about a calendar and an
// address, the other only about an address.
func grantedClause(addressParam string) string {
	return fmt.Sprintf(`(cs.status = 'active'
	OR EXISTS (
		SELECT 1 FROM calendar_share_decisions d
		WHERE d.share_id = cs.id AND d.address = %[1]s::citext
		  AND d.decision = 'accepted'))`, addressParam)
}

// List returns the shares on one calendar, people first and then groups, each
// by name or address so a dialog renders them in a stable order.
func (c *CalendarShares) List(ctx context.Context, q Querier, calendarID int64) ([]CalendarShare, error) {
	out := make([]CalendarShare, 0, 4)
	err := q.SelectContext(ctx, &out, `
		SELECT `+calendarShareColumns+`
		FROM calendar_shares
		WHERE calendar_id = $1
		ORDER BY subject_kind, COALESCE(subject_address::text, subject_group_id::text)`,
		calendarID)
	if err != nil {
		return nil, fmt.Errorf("store: listing calendar shares: %w", err)
	}
	return out, nil
}

// Put adds a share, or raises an existing one to the level asked for.
//
// Re-sharing is an edit rather than a second row: two rows naming the same
// subject would make "who can see this" a question with two answers, and the
// stronger of the two would be invisible in the list that is meant to show it.
//
// An edit that makes a share pending again also forgets the answers to the
// offer it replaces. What somebody accepted was the thing offered to them
// then — a different level, from a calendar that may since have been renamed
// or emptied — and carrying that yes over would mean the share they accepted
// is not the share they are now using.
func (c *CalendarShares) Put(ctx context.Context, q Querier, share CalendarShare) (CalendarShare, error) {
	level, err := validShareLevel(share.Level)
	if err != nil {
		return CalendarShare{}, err
	}
	status, err := validShareStatus(share.Status)
	if err != nil {
		return CalendarShare{}, err
	}

	// The decision reset rides in the same statement as the upsert: sharing
	// and forgetting the old answers are one act, and a caller that does one
	// without the other would leave an accepted share pending on somebody's
	// screen while already granting.
	const upsertInto = `
		WITH upsert AS (
			INSERT INTO calendar_shares (calendar_id, subject_kind, subject_address, subject_group_id, level, status)
			VALUES (%s)
			ON CONFLICT %s
			DO UPDATE SET level = EXCLUDED.level, status = EXCLUDED.status, updated_at = now()
			RETURNING *, (xmax = 0) AS is_new
		), reset AS (
			DELETE FROM calendar_share_decisions d
			USING upsert u
			WHERE d.share_id = u.id AND NOT u.is_new
		)
		SELECT ` + calendarShareColumns + ` FROM upsert`

	var (
		out   CalendarShare
		query string
		args  []any
	)
	switch share.SubjectKind {
	case ShareSubjectUser:
		if share.SubjectAddress == nil || strings.TrimSpace(*share.SubjectAddress) == "" {
			return CalendarShare{}, fmt.Errorf("store: a user share needs an address")
		}
		query = fmt.Sprintf(upsertInto, "$1, $2, $3, NULL, $4, $5",
			"(calendar_id, subject_address) WHERE subject_address IS NOT NULL")
		args = []any{share.CalendarID, ShareSubjectUser, *share.SubjectAddress, level, status}
	case ShareSubjectGroup:
		if share.SubjectGroupID == nil || *share.SubjectGroupID == 0 {
			return CalendarShare{}, fmt.Errorf("store: a group share needs a group id")
		}
		query = fmt.Sprintf(upsertInto, "$1, $2, NULL, $3, $4, $5",
			"(calendar_id, subject_group_id) WHERE subject_group_id IS NOT NULL")
		args = []any{share.CalendarID, ShareSubjectGroup, *share.SubjectGroupID, level, status}
	default:
		return CalendarShare{}, fmt.Errorf("store: unknown share subject kind %q", share.SubjectKind)
	}

	if err := q.GetContext(ctx, &out, query, args...); err != nil {
		if IsConflict(err) {
			return CalendarShare{}, fmt.Errorf("store: sharing calendar: %w", ErrConflict)
		}
		return CalendarShare{}, fmt.Errorf("store: sharing calendar: %w", err)
	}
	return out, nil
}

// Delete removes a share by id.
func (c *CalendarShares) Delete(ctx context.Context, q Querier, id int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM calendar_shares WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: deleting calendar share: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSubject removes the share naming a particular subject on a calendar.
// It reports whether anything was there to remove.
func (c *CalendarShares) DeleteSubject(ctx context.Context, q Querier, calendarID int64,
	kind string, address *string, groupID *int64) (bool, error) {
	var (
		res sql.Result
		err error
	)
	switch kind {
	case ShareSubjectUser:
		res, err = q.ExecContext(ctx,
			`DELETE FROM calendar_shares WHERE calendar_id = $1 AND subject_kind = 'user' AND subject_address = $2`,
			calendarID, address)
	case ShareSubjectGroup:
		res, err = q.ExecContext(ctx,
			`DELETE FROM calendar_shares WHERE calendar_id = $1 AND subject_kind = 'group' AND subject_group_id = $2`,
			calendarID, groupID)
	default:
		return false, fmt.Errorf("store: unknown share subject kind %q", kind)
	}
	if err != nil {
		return false, fmt.Errorf("store: removing calendar share: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AccessLevel answers what a share grants an address on a calendar: the empty
// string when nothing does, otherwise "read" or "write".
//
// One query decides both kinds of subject, because the answer has to be the
// strongest grant across all of them: a person may be named directly and also
// sit in a group, and "you may write because your team may write" must not be
// lost because the direct share said read.
//
// A group reaches the address in one of two ways, and both are checked here
// rather than in the caller so that no call site can forget one: the address is
// listed in the group, or the group is the organization's own Everyone group
// and the address lives in its domains.
func (c *CalendarShares) AccessLevel(ctx context.Context, q Querier, calendarID int64, address string) (string, error) {
	var level string
	// The GROUP BY is the whole trick: an aggregate without one always returns
	// a row, so a calendar with no shares at all would answer 'read' from a
	// bool_or over nothing, and every calendar on the server would be readable.
	// With it, no matching share means no row, which is the answer meant.
	err := q.GetContext(ctx, &level, `
		SELECT CASE WHEN bool_or(cs.level = 'write') THEN 'write' ELSE 'read' END
		FROM calendar_shares cs
		WHERE cs.calendar_id = $1
		  AND (
		    (cs.subject_kind = 'user' AND cs.subject_address = $2::citext)
		    OR (cs.subject_kind = 'group'
		        AND workgroup_group_has_address(cs.subject_group_id, $2::citext))
		  )
		  AND `+grantedClause("$2")+`
		GROUP BY cs.calendar_id`,
		calendarID, address)
	if err != nil {
		if IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("store: checking calendar share: %w", err)
	}
	return level, nil
}

// SharedCalendars returns the calendars shared with an address that it does
// not own: the ones a colleague let it see, at whatever level it was granted.
//
// It is a separate query from AccessLevel rather than a loop over it because
// the calendar list is the one place that asks "which calendars name me" instead
// of "what may I do with this one" — a loop would be a query per calendar on
// the server's own list of them. Both ask workgroup_group_has_address, so a
// calendar that appears here is a calendar whose access check will agree.
func (c *CalendarShares) SharedCalendars(ctx context.Context, q Querier, address string) ([]Calendar, error) {
	out := make([]Calendar, 0, 4)
	err := q.SelectContext(ctx, &out, `
		SELECT c.id, c.owner, c.workgroup_id, c.name, c.display_name, c.color,
		       c.timezone, c.description, c.show_free_busy, c.is_default,
		       c.created_at, c.updated_at
		FROM calendars c
		JOIN calendar_shares cs ON cs.calendar_id = c.id
		WHERE ((cs.subject_kind = 'user' AND cs.subject_address = $1::citext)
		       OR (cs.subject_kind = 'group'
		           AND workgroup_group_has_address(cs.subject_group_id, $1::citext)))
		  AND `+grantedClause("$1")+`
		ORDER BY c.name`, address)
	if err != nil {
		return nil, fmt.Errorf("store: listing calendars shared with an address: %w", err)
	}
	return out, nil
}

// Get reads one share by its id.
func (c *CalendarShares) Get(ctx context.Context, q Querier, id int64) (CalendarShare, error) {
	var out CalendarShare
	if err := q.GetContext(ctx, &out, `
		SELECT `+calendarShareColumns+`
		FROM calendar_shares WHERE id = $1`, id); err != nil {
		if IsNotFound(err) {
			return CalendarShare{}, ErrNotFound
		}
		return CalendarShare{}, fmt.Errorf("store: reading calendar share: %w", err)
	}
	return out, nil
}

// CalendarShareOffer is a share that is waiting for the person it names.
//
// The calendar comes along with it because an offer that says only "share
// #47, read" is not something anybody can answer: the name and the owner are
// what the answer is about.
type CalendarShareOffer struct {
	CalendarShare
	CalendarName  string  `db:"name"`
	CalendarOwner *string `db:"owner"`
}

// Offers returns the shares waiting for an address to accept them.
//
// A share is listed once per address it reaches, so a group offer appears in
// each member's own list and nowhere else: the answer is theirs alone, and
// one that was already given is not an offer any more.
func (c *CalendarShares) Offers(ctx context.Context, q Querier, address string) ([]CalendarShareOffer, error) {
	out := make([]CalendarShareOffer, 0, 4)
	err := q.SelectContext(ctx, &out, `
		SELECT cs.id, cs.calendar_id, cs.subject_kind, cs.subject_address,
		       cs.subject_group_id, cs.level, cs.status, cs.created_at, cs.updated_at,
		       c.name, c.owner
		FROM calendar_shares cs
		JOIN calendars c ON c.id = cs.calendar_id
		WHERE cs.status = 'pending'
		  AND (
		    (cs.subject_kind = 'user' AND cs.subject_address = $1::citext)
		    OR (cs.subject_kind = 'group'
		        AND workgroup_group_has_address(cs.subject_group_id, $1::citext))
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM calendar_share_decisions d
		    WHERE d.share_id = cs.id AND d.address = $1::citext)
		ORDER BY cs.created_at, cs.id`, address)
	if err != nil {
		return nil, fmt.Errorf("store: listing calendar share offers: %w", err)
	}
	return out, nil
}

// Decide records a person's answer to a share that names them.
//
// The whole question — is this share pending, and does it name this address,
// directly or through a group — is asked in the insert itself rather than in a
// read followed by a write. A share deleted, or raised to active by an
// administrator, between the two would leave a decision about a thing that no
// longer exists; and a share offered to somebody else would otherwise be
// answerable by guessing its id.
//
// An answer can be changed: accepting after declining is somebody making up
// their mind, which is not the same as deciding something new, so it updates
// the row that is there.
func (c *CalendarShares) Decide(ctx context.Context, q Querier, id int64, address, decision string) (CalendarShare, error) {
	switch decision {
	case ShareDecisionAccepted, ShareDecisionDeclined:
	default:
		return CalendarShare{}, fmt.Errorf("store: unknown share decision %q", decision)
	}

	res, err := q.ExecContext(ctx, `
		INSERT INTO calendar_share_decisions (share_id, address, decision)
		SELECT s.id, $2::citext, $3
		FROM calendar_shares s
		WHERE s.id = $1
		  AND s.status = 'pending'
		  AND (
		    (s.subject_kind = 'user' AND s.subject_address = $2::citext)
		    OR (s.subject_kind = 'group'
		        AND workgroup_group_has_address(s.subject_group_id, $2::citext))
		  )
		ON CONFLICT (share_id, address)
		DO UPDATE SET decision = EXCLUDED.decision, decided_at = now()`,
		id, address, decision)
	if err != nil {
		return CalendarShare{}, fmt.Errorf("store: deciding on calendar share: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either the share is gone or it is not this person's to answer.
		// Both are "there is no such thing here for you", which is what the
		// caller reports rather than which of the two it was: the difference
		// is not the caller's business, and saying which would let an
		// address probe which shares exist.
		return CalendarShare{}, ErrNotFound
	}
	return c.Get(ctx, q, id)
}

// validShareLevel rejects an unknown level at the store boundary, where the
// CHECK constraint would otherwise turn it into a database error with nothing
// to say about which field was wrong.
func validShareLevel(level string) (string, error) {
	switch level {
	case CalendarShareRead, CalendarShareWrite:
		return level, nil
	case "":
		// A share that does not say what it grants is a read share: the
		// narrower answer is the one to default to when a client omits the
		// field, since granting write by forgetting is the worse mistake.
		return CalendarShareRead, nil
	default:
		return "", fmt.Errorf("store: unknown calendar share level %q", level)
	}
}

// validShareStatus rejects an unknown status at the store boundary, and
// defaults the empty one to active.
//
// Active is the default because that is what every share was before offers
// existed: a caller that does not say which of the two it means is asking for
// the behaviour it had, and a caller that wants an offer says so.
func validShareStatus(status string) (string, error) {
	switch status {
	case CalendarSharePending, CalendarShareActive:
		return status, nil
	case "":
		return CalendarShareActive, nil
	default:
		return "", fmt.Errorf("store: unknown calendar share status %q", status)
	}
}

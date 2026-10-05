package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CalendarChange is one recorded change to a calendar.
//
// It is keyed by what an event is rather than by its row id, because a change has
// to survive the row: the deletion that matters is reported after the row is gone,
// and a key that referred to the row could not name it any more.
type CalendarChange struct {
	ID           int64      `db:"id"`
	CalendarID   int64      `db:"calendar_id"`
	UID          string     `db:"uid"`
	RecurrenceID *time.Time `db:"recurrence_id"`
	Removed      bool       `db:"removed"`
	ChangedAt    time.Time  `db:"changed_at"`
}

// CalendarChanges is the change log a CalDAV client synchronises against.
//
// It exists because sync-collection is defined in terms of what changed rather
// than what is there now, and a log of what changed is not derivable from the
// things that changed: a deleted event leaves nothing behind to say it was
// deleted.
type CalendarChanges struct{}

// NewCalendarChanges returns the repository. It is stateless, so this is free.
func NewCalendarChanges() *CalendarChanges { return &CalendarChanges{} }

// changeRetention is how long a change is reportable.
//
// A client that comes back inside this window is told what it missed. One that
// comes back later is refused with DAV:valid-sync-token, which the RFC requires
// for a token the server cannot honour and which clients handle by starting a new
// synchronisation. Four weeks is longer than the longest holiday a person takes
// and short enough that the log does not outlive the deployments it is kept for.
const changeRetention = 28 * 24 * time.Hour

// Record notes that a resource in a calendar changed, or that it went away.
//
// The returned position is the token a client synchronising up to now should be
// given, so a caller that has recorded several changes can pass the last one back
// rather than reading the head separately and racing with another writer.
func (c *CalendarChanges) Record(ctx context.Context, q Querier, calendarID int64, uid string, recurrenceID *time.Time, removed bool) (int64, error) {
	var id int64
	err := q.GetContext(ctx, &id, `
		INSERT INTO calendar_changes (calendar_id, uid, recurrence_id, removed)
		VALUES ($1, $2, $3, $4)
		RETURNING id`, calendarID, uid, recurrenceID, removed)
	if err != nil {
		return 0, fmt.Errorf("store: recording a calendar change: %w", err)
	}
	// Pruned on the way past rather than by a cron job, because a job that
	// fails silently is a log that grows for ever and nobody notices until the
	// disk fills. Recording is the only thing that grows the table, so doing it
	// here bounds the table by the write rate and by nothing else.
	if _, err := q.ExecContext(ctx,
		`DELETE FROM calendar_changes WHERE changed_at < $1`,
		time.Now().Add(-changeRetention)); err != nil {
		return 0, fmt.Errorf("store: pruning the calendar change log: %w", err)
	}
	return id, nil
}

// Head is the position of the most recent change in a calendar, which is the
// token identifying the collection's current state.
func (c *CalendarChanges) Head(ctx context.Context, q Querier, calendarID int64) (int64, error) {
	var head sql.NullInt64
	err := q.GetContext(ctx, &head,
		`SELECT MAX(id) FROM calendar_changes WHERE calendar_id = $1`, calendarID)
	if err != nil {
		return 0, fmt.Errorf("store: reading the calendar's change position: %w", err)
	}
	return head.Int64, nil
}

// Since returns the changes to a calendar after a position, in the order they
// happened.
//
// A position past the head returns nothing rather than an error: a client
// synchronising twice against an unchanged calendar sends the same token twice,
// and that is the ordinary case, not a mistake. A limit of zero or less means all
// of them; a limit asks for one more than the caller needs, because that is how it
// finds out whether there were more.
func (c *CalendarChanges) Since(ctx context.Context, q Querier, calendarID, position int64, limit int) ([]CalendarChange, error) {
	query := `
		SELECT id, calendar_id, uid, recurrence_id, removed, changed_at
		FROM calendar_changes
		WHERE calendar_id = $1 AND id > $2
		ORDER BY id`
	args := []any{calendarID, position}
	if limit > 0 {
		query += ` LIMIT $3`
		args = append(args, limit)
	}

	var rows []CalendarChange
	if err := q.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("store: reading calendar changes: %w", err)
	}
	return rows, nil
}

// Forget drops a calendar's history, which is what deleting the calendar does to
// it through the foreign key. It is here so that the cascade is deliberate rather
// than incidental: the log is only meaningful for a collection that exists, and a
// row left behind would keep the uid of an event nobody can reach any more.
func (c *CalendarChanges) Forget(ctx context.Context, q Querier, calendarID int64) error {
	if _, err := q.ExecContext(ctx,
		`DELETE FROM calendar_changes WHERE calendar_id = $1`, calendarID); err != nil {
		return fmt.Errorf("store: forgetting a calendar's history: %w", err)
	}
	return nil
}

package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// ---------------------------------------------------------------------------
// Outbox repository.
// ---------------------------------------------------------------------------

// OutboxRepository provides access to the outbox and outbox_recipients tables.
// Every method accepts a *sqlx.DB or *sqlx.Tx so that callers can compose
// several operations in one transaction.
type OutboxRepository struct {
	db *sqlx.DB
}

// NewOutboxRepository creates a new outbox repository backed by db.
// Pass nil db to create a repository that only works within transactions.
func NewOutboxRepository(db *sqlx.DB) *OutboxRepository {
	return &OutboxRepository{db: db}
}

// ---------------------------------------------------------------------------
// Insert a new outbox message with recipients.
// ---------------------------------------------------------------------------

// InsertMessage inserts an outbox row and its recipients in a single
// transaction. The returned ID can be used for follow-up operations.
//
// recipients must be non-empty and sorted by position. Each recipient's
// Kind must be one of "to", "cc", "bcc".
func (r *OutboxRepository) InsertMessage(ctx context.Context, tx *sqlx.Tx, submittedBy, envelopeFrom string, blobSHA256 []byte, size int64, recipients []OutboxRecipient) (int64, error) {
	var id int64

	if tx != nil {
		row := tx.QueryRowContext(ctx, `
			INSERT INTO outbox (submitted_by, envelope_from, blob_sha256, size)
			VALUES ($1, $2, $3, $4)
			RETURNING id`, submittedBy, envelopeFrom, blobSHA256, size)
		if err := row.Scan(&id); err != nil {
			return 0, fmt.Errorf("insert outbox: %w", err)
		}
	} else {
		row := r.db.QueryRowContext(ctx, `
			INSERT INTO outbox (submitted_by, envelope_from, blob_sha256, size)
			VALUES ($1, $2, $3, $4)
			RETURNING id`, submittedBy, envelopeFrom, blobSHA256, size)
		if err := row.Scan(&id); err != nil {
			return 0, fmt.Errorf("insert outbox: %w", err)
		}
	}

	for _, rec := range recipients {
		var execResult sqlx.ExtContext
		if tx != nil {
			execResult = tx
		} else {
			execResult = r.db
		}
		if _, err := execResult.ExecContext(ctx, `
			INSERT INTO outbox_recipients (outbox_id, position, address, kind)
			VALUES ($1, $2, $3, $4)`, id, rec.Position, rec.Address, rec.Kind); err != nil {
			return 0, fmt.Errorf("insert recipient position %d: %w", rec.Position, err)
		}
	}

	return id, nil
}

// ---------------------------------------------------------------------------
// Claim due messages for delivery.
// ---------------------------------------------------------------------------

// ClaimForSending takes the next batch of due messages out of the queue,
// marking every one of them 'sending' in the same statement that reads them
// back.
//
// The mark and the read are one statement because they are one decision. Read
// the due rows in one call and mark each one in the next, and every worker
// polling at that moment selects the same rows: FOR UPDATE SKIP LOCKED holds
// its lock only for the statement that took it, and there was no transaction
// around the pair. The losers then reported "message already claimed" as an
// error for mail that was being delivered perfectly well — a log line that
// reads like a delivery failure precisely when somebody is reading the log
// for one.
//
// A message taken here belongs to this caller until it settles or is given
// back: nothing retries a message in the 'sending' state, so a worker that
// stops before reaching one it claimed has to Release it first.
func (r *OutboxRepository) ClaimForSending(ctx context.Context, maxMessages int) ([]OutboxMessage, error) {
	if maxMessages < 1 {
		maxMessages = 1
	}
	rows, err := r.db.QueryContext(ctx, `
		UPDATE outbox
		SET state = 'sending'
		WHERE id IN (
			SELECT id
			FROM outbox
			WHERE state IN ('queued', 'deferred')
			  AND next_attempt_at <= now()
			ORDER BY next_attempt_at ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, submitted_by, envelope_from, blob_sha256, size, state,
		          attempts, next_attempt_at, last_error, failed_at, dsn_at,
		          sent_at, created_at`, maxMessages)
	if err != nil {
		return nil, fmt.Errorf("claim messages: %w", err)
	}
	defer rows.Close()

	var messages []OutboxMessage
	for rows.Next() {
		var msg OutboxMessage
		if err := scanOutboxMessage(rows, &msg); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	// The recipients are read after the claim, outside it: the claim made
	// these rows this caller's own, so no other worker can change them in the
	// meantime, and a second query keeps the claiming statement itself small.
	for i := range messages {
		if messages[i].Recipients, err = r.loadRecipients(ctx, nil, messages[i].ID); err != nil {
			// The claim stands even though the recipients could not be read,
			// so the messages go back rather than waiting for an administrator
			// to notice them 'sending' with nobody working on them.
			ids := make([]int64, 0, len(messages))
			for _, m := range messages {
				ids = append(ids, m.ID)
			}
			_ = r.Release(context.WithoutCancel(ctx), ids)
			return nil, err
		}
	}

	return messages, nil
}

// Release puts claimed messages back into the queue without attempting them.
//
// It is what a worker owes the messages it took and never reached: the queue's
// due query looks only at 'queued' and 'deferred', so a message left
// 'sending' by a shutdown is retried by nobody and waits for somebody to
// notice it in the queue.
//
// Only a message still 'sending' is released. One the worker settled is not
// put back for a second delivery, whatever state it settled into.
func (r *OutboxRepository) Release(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	query, args, err := sqlx.In(`
		UPDATE outbox
		SET state = 'queued', next_attempt_at = now()
		WHERE state = 'sending' AND id IN (?)`, ids)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// scanOutboxMessage scans an OutboxMessage from a sql.Rows cursor.
func scanOutboxMessage(rows *sql.Rows, msg *OutboxMessage) error {
	return rows.Scan(
		&msg.ID,
		&msg.SubmittedBy,
		&msg.EnvelopeFrom,
		&msg.BlobSHA256,
		&msg.Size,
		(*string)(&msg.State),
		&msg.Attempts,
		&msg.NextAttemptAt,
		&msg.LastError,
		&msg.FailedAt,
		&msg.DSNAt,
		&msg.SentAt,
		&msg.CreatedAt,
	)
}

// loadRecipients returns all recipients for a given outbox message.
func (r *OutboxRepository) loadRecipients(ctx context.Context, tx *sqlx.Tx, outboxID int64) ([]OutboxRecipient, error) {
	var query string
	var args []any

	if tx != nil {
		query = `
			SELECT position, address, kind, state, attempts, last_error, delivered_at
			FROM outbox_recipients
			WHERE outbox_id = $1
			ORDER BY position`
		args = []any{outboxID}
	} else {
		query = `
			SELECT position, address, kind, state, attempts, last_error, delivered_at
			FROM outbox_recipients
			WHERE outbox_id = $1
			ORDER BY position`
		args = []any{outboxID}
	}

	rows, err := r.queryRows(ctx, tx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query recipients: %w", err)
	}
	defer rows.Close()

	var recipients []OutboxRecipient
	for rows.Next() {
		var rec OutboxRecipient
		if err := rows.Scan(
			&rec.Position,
			&rec.Address,
			(*string)(&rec.Kind),
			(*string)(&rec.State),
			&rec.Attempts,
			&rec.LastError,
			&rec.DeliveredAt,
		); err != nil {
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		recipients = append(recipients, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recipients: %w", err)
	}

	return recipients, nil
}

// queryRows executes a query and returns *sql.Rows, using the provided tx or db.
func (r *OutboxRepository) queryRows(ctx context.Context, tx *sqlx.Tx, query string, args ...any) (*sql.Rows, error) {
	if tx != nil {
		return tx.QueryContext(ctx, query, args...)
	}
	return r.db.QueryContext(ctx, query, args...)
}

// ---------------------------------------------------------------------------
// Update message and recipient states after delivery.
// ---------------------------------------------------------------------------

// MarkSent marks the message and all its recipients as delivered.
func (r *OutboxRepository) MarkSent(ctx context.Context, tx *sqlx.Tx, messageID int64) error {
	if tx != nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbox SET state = 'sent', sent_at = now() WHERE id = $1`, messageID); err != nil {
			return fmt.Errorf("mark sent: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbox_recipients SET state = 'delivered', delivered_at = now()
			WHERE outbox_id = $1 AND state IN ('pending', 'deferred')`, messageID); err != nil {
			return fmt.Errorf("mark recipients sent: %w", err)
		}
	} else {
		if _, err := r.db.ExecContext(ctx, `
			UPDATE outbox SET state = 'sent', sent_at = now() WHERE id = $1`, messageID); err != nil {
			return fmt.Errorf("mark sent: %w", err)
		}
		if _, err := r.db.ExecContext(ctx, `
			UPDATE outbox_recipients SET state = 'delivered', delivered_at = now()
			WHERE outbox_id = $1 AND state IN ('pending', 'deferred')`, messageID); err != nil {
			return fmt.Errorf("mark recipients sent: %w", err)
		}
	}
	return nil
}

// MarkFailed marks the message as failed and all its pending recipients as failed.
// If dsnDelay > 0, dsn_at is set to now() + dsnDelay so the DSN processor can
// generate a bounce after the configured delay.
func (r *OutboxRepository) MarkFailed(ctx context.Context, tx *sqlx.Tx, messageID int64, attempts int, lastError string, dsnDelay time.Duration) error {
	dsnAt := "NULL"
	if dsnDelay > 0 {
		dsnAt = "now() + interval '" + dsnDelay.String() + "'"
	}
	if tx != nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbox SET state = 'failed', attempts = $1, last_error = $2,
			failed_at = now(), dsn_at = `+dsnAt+` WHERE id = $3`, attempts, lastError, messageID); err != nil {
			return fmt.Errorf("mark failed: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbox_recipients SET state = 'failed', last_error = $1, delivered_at = now()
			WHERE outbox_id = $2 AND state IN ('pending', 'deferred')`, lastError, messageID); err != nil {
			return fmt.Errorf("mark recipients failed: %w", err)
		}
	} else {
		if _, err := r.db.ExecContext(ctx, `
			UPDATE outbox SET state = 'failed', attempts = $1, last_error = $2,
			failed_at = now(), dsn_at = `+dsnAt+` WHERE id = $3`, attempts, lastError, messageID); err != nil {
			return fmt.Errorf("mark failed: %w", err)
		}
		if _, err := r.db.ExecContext(ctx, `
			UPDATE outbox_recipients SET state = 'failed', last_error = $1, delivered_at = now()
			WHERE outbox_id = $2 AND state IN ('pending', 'deferred')`, lastError, messageID); err != nil {
			return fmt.Errorf("mark recipients failed: %w", err)
		}
	}
	return nil
}

// MarkDeferred marks the message for retry and updates next_attempt_at.
func (r *OutboxRepository) MarkDeferred(ctx context.Context, tx *sqlx.Tx, messageID int64, attempts int, lastError string, nextAttemptAt time.Time) error {
	if tx != nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbox SET state = 'deferred', attempts = $1, last_error = $2,
			next_attempt_at = $3 WHERE id = $4`, attempts, lastError, nextAttemptAt, messageID); err != nil {
			return fmt.Errorf("mark deferred: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbox_recipients SET state = 'deferred', last_error = $1
			WHERE outbox_id = $2 AND state IN ('pending', 'deferred')`, lastError, messageID); err != nil {
			return fmt.Errorf("mark recipients deferred: %w", err)
		}
	} else {
		if _, err := r.db.ExecContext(ctx, `
			UPDATE outbox SET state = 'deferred', attempts = $1, last_error = $2,
			next_attempt_at = $3 WHERE id = $4`, attempts, lastError, nextAttemptAt, messageID); err != nil {
			return fmt.Errorf("mark deferred: %w", err)
		}
		if _, err := r.db.ExecContext(ctx, `
			UPDATE outbox_recipients SET state = 'deferred', last_error = $1
			WHERE outbox_id = $2 AND state IN ('pending', 'deferred')`, lastError, messageID); err != nil {
			return fmt.Errorf("mark recipients deferred: %w", err)
		}
	}
	return nil
}

// MarkBounced marks the message as bounced (DSN generated).
func (r *OutboxRepository) MarkBounced(ctx context.Context, tx *sqlx.Tx, messageID int64) error {
	if tx != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state = 'bounced' WHERE id = $1`, messageID); err != nil {
			return fmt.Errorf("mark bounced: %w", err)
		}
	} else {
		if _, err := r.db.ExecContext(ctx, `UPDATE outbox SET state = 'bounced' WHERE id = $1`, messageID); err != nil {
			return fmt.Errorf("mark bounced: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// DSN: claim messages due for bounce generation.
// ---------------------------------------------------------------------------

// ClaimDSNMessages returns messages whose failed_at has passed and dsn_at has
// arrived, meaning all retries are exhausted and a DSN should be generated.
func (r *OutboxRepository) ClaimDSNMessages(ctx context.Context, tx *sqlx.Tx, maxMessages int) ([]OutboxMessage, error) {
	rows, err := r.queryRows(ctx, tx, `
		SELECT id, submitted_by, envelope_from, blob_sha256, size, state,
		       attempts, next_attempt_at, last_error, failed_at, dsn_at, sent_at, created_at
		FROM outbox
		WHERE state = 'failed'
		  AND dsn_at IS NOT NULL
		  AND dsn_at <= now()
		ORDER BY dsn_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, maxMessages)
	if err != nil {
		return nil, fmt.Errorf("claim DSN messages: %w", err)
	}
	defer rows.Close()

	var messages []OutboxMessage
	for rows.Next() {
		var msg OutboxMessage
		if err := scanOutboxMessage(rows, &msg); err != nil {
			return nil, fmt.Errorf("scan DSN message: %w", err)
		}
		msg.Recipients, err = r.loadRecipients(ctx, tx, msg.ID)
		if err != nil {
			return nil, fmt.Errorf("load DSN recipients for message %d: %w", msg.ID, err)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate DSN messages: %w", err)
	}

	return messages, nil
}

// ---------------------------------------------------------------------------
// Per-recipient partial delivery tracking.
// ---------------------------------------------------------------------------

// UpdateRecipientState marks a single recipient's delivery state.
func (r *OutboxRepository) UpdateRecipientState(ctx context.Context, tx *sqlx.Tx, outboxID int64, position int, state RecipientState, lastError string) error {
	// $1 is used both as an enum value and as text in the CASE, so Postgres
	// needs the cast spelled out or it cannot deduce a single type.
	if tx != nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbox_recipients SET state = $1::recipient_state, last_error = $2,
			delivered_at = CASE WHEN $1::text = 'delivered' THEN now() ELSE delivered_at END
			WHERE outbox_id = $3 AND position = $4`, string(state), lastError, outboxID, position); err != nil {
			return fmt.Errorf("update recipient state: %w", err)
		}
	} else {
		if _, err := r.db.ExecContext(ctx, `
			UPDATE outbox_recipients SET state = $1::recipient_state, last_error = $2,
			delivered_at = CASE WHEN $1::text = 'delivered' THEN now() ELSE delivered_at END
			WHERE outbox_id = $3 AND position = $4`, string(state), lastError, outboxID, position); err != nil {
			return fmt.Errorf("update recipient state: %w", err)
		}
	}
	return nil
}

// PendingRecipientCount returns the number of pending recipients for a message.
func (r *OutboxRepository) PendingRecipientCount(ctx context.Context, tx *sqlx.Tx, outboxID int64) (int, error) {
	var count int
	if tx != nil {
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM outbox_recipients WHERE outbox_id = $1 AND state = 'pending'`, outboxID).Scan(&count); err != nil {
			return 0, fmt.Errorf("count pending recipients: %w", err)
		}
	} else {
		if err := r.db.QueryRowContext(ctx, `
			SELECT count(*) FROM outbox_recipients WHERE outbox_id = $1 AND state = 'pending'`, outboxID).Scan(&count); err != nil {
			return 0, fmt.Errorf("count pending recipients: %w", err)
		}
	}
	return count, nil
}

// execCtx executes a query using the provided tx or db.
func (r *OutboxRepository) execCtx(ctx context.Context, tx *sqlx.Tx, query string, args ...any) (sql.Result, error) {
	if tx != nil {
		return tx.ExecContext(ctx, query, args...)
	}
	return r.db.ExecContext(ctx, query, args...)
}

// ---------------------------------------------------------------------------
// The administrator's view of the queue.
// ---------------------------------------------------------------------------

// QueueFilter narrows a queue listing. The zero value asks for every message,
// which is the answer to "what is in the queue at all" — the first thing
// wanted when mail has not arrived, before anybody knows what to ask next.
type QueueFilter struct {
	// State limits the listing to one state; empty means every state.
	State OutboxState
	// Submitter limits it to one sender's messages.
	Submitter string
	// Limit caps the page: zero takes the default, and the default is capped
	// so that one mistaken request cannot ask for the whole table.
	Limit int
	// Offset pages past the first Limit rows.
	Offset int
}

const (
	defaultQueueLimit = 50
	maxQueueLimit     = 500
)

// OutboxQueueItem is one message as a queue listing draws it: the row, plus
// the two counts that answer "did it go out?" without opening every message.
//
// The counts are computed in the same statement rather than gathered per row
// because a listing with one extra query per message is how a queue page
// becomes the slowest page on the server precisely when the server is busiest.
type OutboxQueueItem struct {
	ID             int64        `db:"id"`
	SubmittedBy    string       `db:"submitted_by"`
	EnvelopeFrom   string       `db:"envelope_from"`
	Size           int64        `db:"size"`
	State          OutboxState  `db:"state"`
	Attempts       int          `db:"attempts"`
	NextAttemptAt  time.Time    `db:"next_attempt_at"`
	LastError      string       `db:"last_error"`
	FailedAt       sql.NullTime `db:"failed_at"`
	DSNAt          sql.NullTime `db:"dsn_at"`
	SentAt         sql.NullTime `db:"sent_at"`
	CreatedAt      time.Time    `db:"created_at"`
	RecipientCount int          `db:"recipient_count"`
	DeliveredCount int          `db:"delivered_count"`
}

// ListQueue returns recent messages, newest first.
func (r *OutboxRepository) ListQueue(ctx context.Context, tx *sqlx.Tx, filter QueueFilter) ([]OutboxQueueItem, error) {
	query := `
		SELECT o.id, o.submitted_by, o.envelope_from, o.size, o.state, o.attempts,
		       o.next_attempt_at, o.last_error, o.failed_at, o.dsn_at, o.sent_at,
		       o.created_at,
		       (SELECT count(*) FROM outbox_recipients r
		        WHERE r.outbox_id = o.id) AS recipient_count,
		       (SELECT count(*) FROM outbox_recipients r
		        WHERE r.outbox_id = o.id AND r.state = 'delivered') AS delivered_count
		FROM outbox o
		WHERE 1 = 1`
	args := make([]any, 0, 4)
	if filter.State != "" {
		args = append(args, filter.State)
		// The cast is spelled out because state arrives as text and the column
		// is an enum: without it Postgres is asked to compare an enum with an
		// untyped parameter and cannot choose a side.
		query += fmt.Sprintf(" AND o.state = $%d::outbox_state", len(args))
	}
	if filter.Submitter != "" {
		args = append(args, filter.Submitter)
		query += fmt.Sprintf(" AND o.submitted_by = $%d::citext", len(args))
	}
	query += ` ORDER BY o.created_at DESC, o.id DESC`

	limit := filter.Limit
	if limit <= 0 {
		limit = defaultQueueLimit
	} else if limit > maxQueueLimit {
		limit = maxQueueLimit
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit, offset)
	query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := r.queryRows(ctx, tx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list queue: %w", err)
	}
	defer rows.Close()

	out := make([]OutboxQueueItem, 0, limit)
	for rows.Next() {
		var item OutboxQueueItem
		if err := scanQueueItem(rows, &item); err != nil {
			return nil, fmt.Errorf("scan queue message: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate queue: %w", err)
	}
	return out, nil
}

// scanQueueItem reads one row of a queue listing.
//
// The columns are read by hand rather than through sqlx because the rest of
// this file does: the driver returns enums as text, and the cast is spelled
// out here rather than left to a tag reflection would have to guess at.
func scanQueueItem(rows *sql.Rows, item *OutboxQueueItem) error {
	return rows.Scan(
		&item.ID,
		&item.SubmittedBy,
		&item.EnvelopeFrom,
		&item.Size,
		(*string)(&item.State),
		&item.Attempts,
		&item.NextAttemptAt,
		&item.LastError,
		&item.FailedAt,
		&item.DSNAt,
		&item.SentAt,
		&item.CreatedAt,
		&item.RecipientCount,
		&item.DeliveredCount,
	)
}

// Get returns one message with its recipients.
//
// An id nobody has is sql.ErrNoRows, which callers turn into "no such
// message" — the same answer the routes give for anything else that is not
// there, rather than a fault about a missing row.
func (r *OutboxRepository) Get(ctx context.Context, tx *sqlx.Tx, id int64) (OutboxMessage, error) {
	rows, err := r.queryRows(ctx, tx, `
		SELECT id, submitted_by, envelope_from, blob_sha256, size, state, attempts,
		       next_attempt_at, last_error, failed_at, dsn_at, sent_at, created_at
		FROM outbox
		WHERE id = $1`, id)
	if err != nil {
		return OutboxMessage{}, fmt.Errorf("get message: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return OutboxMessage{}, fmt.Errorf("get message: %w", err)
		}
		return OutboxMessage{}, sql.ErrNoRows
	}
	var msg OutboxMessage
	if err := scanOutboxMessage(rows, &msg); err != nil {
		return OutboxMessage{}, fmt.Errorf("scan message: %w", err)
	}
	if err := rows.Err(); err != nil {
		return OutboxMessage{}, fmt.Errorf("iterate message: %w", err)
	}
	if msg.Recipients, err = r.loadRecipients(ctx, tx, id); err != nil {
		return OutboxMessage{}, err
	}
	return msg, nil
}

// Requeue puts a message back into the queue for an immediate attempt: the
// backoff is discarded, the failure timestamps are cleared so no bounce is
// generated for a message being tried again, and every recipient that had
// settled as refused is asked to try once more.
//
// Both updates happen in one transaction because they are one decision. A
// message marked queued while its refused recipients stayed refused would be
// claimed, find nobody willing to receive it, and settle without a single
// attempt — a retry that looks like it happened and did not.
//
// A message in flight is not touched, and neither is one already sent: there
// is nothing left to retry, and offering a button that promises a resend the
// worker would not make is worse than not offering it. Both cases report
// sql.ErrNoRows.
func (r *OutboxRepository) Requeue(ctx context.Context, id int64) error {
	if r.db == nil {
		return errors.New("outbox: requeue requires a database handle")
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("requeue: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	res, err := tx.ExecContext(ctx, `
		UPDATE outbox
		SET state = 'queued', next_attempt_at = now(),
		    failed_at = NULL, dsn_at = NULL
		WHERE id = $1 AND state IN ('queued', 'deferred', 'failed', 'bounced')`, id)
	if err != nil {
		return fmt.Errorf("requeue: updating message: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}

	// 'failed' recipients are the ones that need asking again: to the worker a
	// settled failure is a refusal it will not retry, and it skips straight
	// past them. 'pending' and 'deferred' are already owed an attempt, and
	// 'delivered' must not be asked twice — that is how a resend duplicates
	// mail people already received.
	if _, err := tx.ExecContext(ctx, `
		UPDATE outbox_recipients
		SET state = 'pending'
		WHERE outbox_id = $1 AND state = 'failed'`, id); err != nil {
		return fmt.Errorf("requeue: resetting recipients: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("requeue: committing: %w", err)
	}
	return nil
}

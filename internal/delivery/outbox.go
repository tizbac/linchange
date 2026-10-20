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

// ClaimMessages returns up to maxMessages queued messages whose next_attempt_at
// has passed, locking them for update. The returned messages include their
// recipients.
func (r *OutboxRepository) ClaimMessages(ctx context.Context, tx *sqlx.Tx, maxMessages int) ([]OutboxMessage, error) {
	var query string
	var args []any

	if tx != nil {
		query = `
			SELECT id, submitted_by, envelope_from, blob_sha256, size, state,
			       attempts, next_attempt_at, last_error, failed_at, dsn_at, sent_at, created_at
			FROM outbox
			WHERE state IN ('queued', 'deferred')
			  AND next_attempt_at <= now()
			ORDER BY next_attempt_at ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED`
		args = []any{maxMessages}
	} else {
		query = `
			SELECT id, submitted_by, envelope_from, blob_sha256, size, state,
			       attempts, next_attempt_at, last_error, failed_at, dsn_at, sent_at, created_at
			FROM outbox
			WHERE state IN ('queued', 'deferred')
			  AND next_attempt_at <= now()
			ORDER BY next_attempt_at ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED`
		args = []any{maxMessages}
	}

	rows, err := r.queryRows(ctx, tx, query, args...)
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
		msg.Recipients, err = r.loadRecipients(ctx, tx, msg.ID)
		if err != nil {
			return nil, fmt.Errorf("load recipients for message %d: %w", msg.ID, err)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	return messages, nil
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

// MarkSending updates the message state to "sending" to claim it for delivery.
func (r *OutboxRepository) MarkSending(ctx context.Context, tx *sqlx.Tx, messageID int64) error {
	var query string
	var args []any

	if tx != nil {
		query = `UPDATE outbox SET state = 'sending' WHERE id = $1 AND state IN ('queued', 'deferred')`
		args = []any{messageID}
	} else {
		query = `UPDATE outbox SET state = 'sending' WHERE id = $1 AND state IN ('queued', 'deferred')`
		args = []any{messageID}
	}

	result, err := r.execCtx(ctx, tx, query, args...)
	if err != nil {
		return fmt.Errorf("mark sending: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return errors.New("mark sending: message already claimed or not found")
	}
	return nil
}

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

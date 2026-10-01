package store

import (
	"context"
	"fmt"
	"time"
)

// QuotaUsage is what an owner currently holds, across all of their mailboxes.
// It is a cache of a fact that lives in the messages themselves, kept as a
// counter so the check on every delivery does not have to sum a table.
type QuotaUsage struct {
	Owner        string    `db:"owner"`
	UsedBytes    int64     `db:"used_bytes"`
	MessageCount int64     `db:"message_count"`
	UpdatedAt    time.Time `db:"updated_at"`
}

// Quotas reads and writes quota_usage.
type Quotas struct{}

// NewQuotas returns the repository. It is stateless, so this is free.
func NewQuotas() *Quotas { return &Quotas{} }

// Get returns what an owner is using. An owner who has never been written a
// row has used nothing, which is a zero reading rather than ErrNotFound: an
// absent counter is not an error, it is an empty mailbox.
func (q *Quotas) Get(ctx context.Context, qr Querier, owner string) (QuotaUsage, error) {
	var row QuotaUsage
	err := qr.GetContext(ctx, &row, `
		SELECT owner, used_bytes, message_count, updated_at
		FROM quota_usage
		WHERE owner = $1`, owner)
	if err == nil {
		return row, nil
	}
	if IsNotFound(err) {
		return QuotaUsage{Owner: owner}, nil
	}
	return QuotaUsage{}, fmt.Errorf("store: reading quota usage: %w", err)
}

// Adjust moves an owner's usage by a signed delta and returns the new
// reading. The update is atomic, so two concurrent deliveries cannot both read
// the same starting figure and lose one of the messages between them.
//
// The inserted values are clamped at zero because a negative delta has nothing
// to subtract from on a first delivery: Postgres evaluates the proposed
// insert before it resolves the conflict, so passing the raw negative delta
// would trip the non-negative check on a row that does not exist yet. Over-
// subtracting a row that does exist is still refused, which is the case that
// would mean the counter and the mail disagree.
func (q *Quotas) Adjust(ctx context.Context, qr Querier, owner string, bytes, messages int64) (QuotaUsage, error) {
	const query = `
		INSERT INTO quota_usage (owner, used_bytes, message_count)
		VALUES ($1, GREATEST($2, 0), GREATEST($3, 0))
		ON CONFLICT (owner) DO UPDATE SET
			used_bytes    = quota_usage.used_bytes + $2,
			message_count = quota_usage.message_count + $3,
			updated_at    = now()
		RETURNING owner, used_bytes, message_count, updated_at`
	var row QuotaUsage
	if err := qr.GetContext(ctx, &row, query, owner, bytes, messages); err != nil {
		return QuotaUsage{}, fmt.Errorf("store: adjusting quota usage: %w", err)
	}
	return row, nil
}

// Reset zeroes an owner's usage, which is what a quota recalculation or a
// restore needs when the counter no longer matches the mail.
func (q *Quotas) Reset(ctx context.Context, qr Querier, owner string, bytes, messages int64) error {
	const query = `
		INSERT INTO quota_usage (owner, used_bytes, message_count)
		VALUES ($1, $2, $3)
		ON CONFLICT (owner) DO UPDATE SET
			used_bytes = EXCLUDED.used_bytes,
			message_count = EXCLUDED.message_count,
			updated_at = now()`
	if _, err := qr.ExecContext(ctx, query, owner, bytes, messages); err != nil {
		return fmt.Errorf("store: resetting quota usage: %w", err)
	}
	return nil
}

// Exceeded reports whether a delta would push an owner past their quota, and
// is the check the delivery path makes before it accepts a message. A quota of
// zero or less means no limit, which is how an unlimited account is spelled.
func (q *Quotas) Exceeded(ctx context.Context, qr Querier, owner string, limit int64, adding int64) (bool, error) {
	if limit <= 0 {
		return false, nil
	}
	usage, err := q.Get(ctx, qr, owner)
	if err != nil {
		return false, err
	}
	return usage.UsedBytes+adding > limit, nil
}

// SendCounter is how much one principal has sent on one UTC day. The cap is
// checked and incremented in the same transaction that queues the message, so
// a caller cannot slip past it by racing two submissions.
type SendCounter struct {
	Owner     string    `db:"owner"`
	Day       time.Time `db:"day"`
	Count     int       `db:"count"`
	UpdatedAt time.Time `db:"updated_at"`
}

// SendCounters reads and writes send_counters.
type SendCounters struct{}

// NewSendCounters returns the repository. It is stateless.
func NewSendCounters() *SendCounters { return &SendCounters{} }

// Increment adds to today's count for an owner and returns the new total. The
// caller is expected to compare that against the limit in the same
// transaction, so that the number it compares is the number it just wrote.
func (s *SendCounters) Increment(ctx context.Context, qr Querier, owner string, n int) (int, error) {
	const query = `
		INSERT INTO send_counters (owner, day, count)
		VALUES ($1, date_trunc('day', now() AT TIME ZONE 'UTC')::date, $2)
		ON CONFLICT (owner, day) DO UPDATE SET
			count = send_counters.count + $2,
			updated_at = now()
		RETURNING count`
	var count int
	if err := qr.GetContext(ctx, &count, query, owner, n); err != nil {
		return 0, fmt.Errorf("store: incrementing send counter: %w", err)
	}
	return count, nil
}

// Today returns what an owner has sent today, which is zero for an owner who
// has not sent anything, as with quota usage.
func (s *SendCounters) Today(ctx context.Context, qr Querier, owner string) (SendCounter, error) {
	const query = `
		SELECT owner, day, count, updated_at
		FROM send_counters
		WHERE owner = $1 AND day = date_trunc('day', now() AT TIME ZONE 'UTC')::date`
	var row SendCounter
	if err := qr.GetContext(ctx, &row, query, owner); err != nil {
		if IsNotFound(err) {
			return SendCounter{Owner: owner, Day: todayUTC()}, nil
		}
		return SendCounter{}, fmt.Errorf("store: reading send counter: %w", err)
	}
	return row, nil
}

func todayUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// Prune deletes counters before the given day, keeping the table proportional
// to active senders rather than to all the mail ever sent. Counters from
// earlier days are already dead weight: the cap is a daily one.
func (s *SendCounters) Prune(ctx context.Context, qr Querier, before time.Time) (int64, error) {
	res, err := qr.ExecContext(ctx, `DELETE FROM send_counters WHERE day < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("store: pruning send counters: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"time"

	"github.com/linchange/linchange/internal/auth/lockout"
)

// AuthAttempts records authentication failures, per account and per source
// address.
//
// It stores history and nothing else: whether a subject is locked is decided by
// the lockout policy from the recorded state, so the thresholds can be changed
// without a migration and without rewriting rows. That split is deliberate. A
// table that stored "locked_until" as the answer would make a change of policy
// apply only to subjects that had not yet been locked, which is the kind of
// inconsistency nobody notices until it matters.
type AuthAttempts struct{}

// NewAuthAttempts returns a repository. It is stateless, so this is free.
func NewAuthAttempts() *AuthAttempts { return &AuthAttempts{} }

// attemptsRow is the shape shared by auth_attempts and auth_ip_attempts.
type attemptsRow struct {
	FailedAttempts  int          `db:"failed_attempts"`
	WindowStartedAt time.Time    `db:"window_started_at"`
	LastFailedAt    time.Time    `db:"last_failed_at"`
	LockedUntil     sql.NullTime `db:"locked_until"`
}

func (r attemptsRow) state() lockout.State {
	s := lockout.State{
		FailedAttempts:  r.FailedAttempts,
		WindowStartedAt: r.WindowStartedAt,
		LastFailedAt:    r.LastFailedAt,
	}
	if r.LockedUntil.Valid {
		s.LockedUntil = r.LockedUntil.Time
	}
	return s
}

func stateRow(s lockout.State) (failed int, window, last, locked any) {
	return s.FailedAttempts, s.WindowStartedAt, s.LastFailedAt, nullableTime(s.LockedUntil)
}

// withTimestamps fills in the timestamps a caller may have left at their zero
// value.
//
// The policy always sets both, but this repository is reachable directly and a
// hand-built state is easy to write. Storing a zero last_failed_at would leave a
// row that looks locked yet is pruned by the very next sweep, so a limit would
// quietly evaporate for no visible reason. Defaulting to now is the safe reading
// of "no timestamp given": it ages out on schedule rather than immediately.
func withTimestamps(s lockout.State, now time.Time) lockout.State {
	if s.WindowStartedAt.IsZero() {
		s.WindowStartedAt = now
	}
	if s.LastFailedAt.IsZero() {
		s.LastFailedAt = now
	}
	return s
}

// AccountState reads the recorded failures for an address.
//
// An address with no row is a clean slate rather than an error, so the caller
// does not have to distinguish "never failed" from "row was pruned", and a
// missing row is the common case on a successful login.
func (a *AuthAttempts) AccountState(ctx context.Context, q Querier, address string) (lockout.State, error) {
	const query = `
		SELECT failed_attempts, window_started_at, last_failed_at, locked_until
		FROM auth_attempts
		WHERE address = $1`

	var row attemptsRow
	if err := q.GetContext(ctx, &row, query, address); err != nil {
		if IsNotFound(err) {
			return lockout.State{}, nil
		}
		return lockout.State{}, fmt.Errorf("store: reading auth attempts: %w", err)
	}
	return row.state(), nil
}

// RecordAccountFailure stores the state after one more failure for an address.
//
// The read and the write are meant to run inside a transaction, because two
// simultaneous attempts against the same account would otherwise both read the
// same count and both write count+1, losing a failure. Under-counting is exactly
// the wrong direction to be wrong in, since it lets an attacker probe a
// threshold by firing attempts in parallel.
func (a *AuthAttempts) RecordAccountFailure(ctx context.Context, q Querier, address string, s lockout.State) error {
	const upsert = `
		INSERT INTO auth_attempts
			(address, failed_attempts, window_started_at, last_failed_at, locked_until)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (address) DO UPDATE
		SET failed_attempts   = EXCLUDED.failed_attempts,
		    window_started_at = EXCLUDED.window_started_at,
		    last_failed_at    = EXCLUDED.last_failed_at,
		    locked_until      = EXCLUDED.locked_until`

	failed, window, last, locked := stateRow(withTimestamps(s, time.Now()))
	if _, err := q.ExecContext(ctx, upsert, address, failed, window, last, locked); err != nil {
		// The foreign key is the one case worth naming: an address with no user
		// has no row to record against, and the caller should fall back to the
		// per-address limit rather than inventing a user.
		if IsConflict(err) {
			return ErrNotFound
		}
		return fmt.Errorf("store: recording auth attempt: %w", err)
	}
	return nil
}

// ResetAccount clears an address's history after a successful authentication.
//
// A success clears the per-account count. It does not clear the per-address
// count, because an attacker holding one valid account could otherwise launder
// unlimited guesses through it: log in, clear the address counter, guess again.
func (a *AuthAttempts) ResetAccount(ctx context.Context, q Querier, address string) error {
	const del = `DELETE FROM auth_attempts WHERE address = $1`
	if _, err := q.ExecContext(ctx, del, address); err != nil {
		return fmt.Errorf("store: resetting auth attempts: %w", err)
	}
	return nil
}

// IPState reads the recorded failures for a source address.
func (a *AuthAttempts) IPState(ctx context.Context, q Querier, ip netip.Addr) (lockout.State, error) {
	if !ip.IsValid() {
		// A client that supplied no usable address cannot be counted against.
		// Returning a clean slate rather than an error keeps the caller's
		// per-account limit working on its own.
		return lockout.State{}, nil
	}

	const query = `
		SELECT failed_attempts, window_started_at, last_failed_at, locked_until
		FROM auth_ip_attempts
		WHERE ip = $1`

	var row attemptsRow
	if err := q.GetContext(ctx, &row, query, ip.String()); err != nil {
		if IsNotFound(err) {
			return lockout.State{}, nil
		}
		return lockout.State{}, fmt.Errorf("store: reading address attempts: %w", err)
	}
	return row.state(), nil
}

// RecordIPFailure stores the state after one more failure from an address.
func (a *AuthAttempts) RecordIPFailure(ctx context.Context, q Querier, ip netip.Addr, s lockout.State) error {
	if !ip.IsValid() {
		return nil
	}

	const upsert = `
		INSERT INTO auth_ip_attempts
			(ip, failed_attempts, window_started_at, last_failed_at, locked_until)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (ip) DO UPDATE
		SET failed_attempts   = EXCLUDED.failed_attempts,
		    window_started_at = EXCLUDED.window_started_at,
		    last_failed_at    = EXCLUDED.last_failed_at,
		    locked_until      = EXCLUDED.locked_until`

	failed, window, last, locked := stateRow(withTimestamps(s, time.Now()))
	if _, err := q.ExecContext(ctx, upsert, ip.String(), failed, window, last, locked); err != nil {
		return fmt.Errorf("store: recording address attempt: %w", err)
	}
	return nil
}

// PruneIPStale deletes address rows with no recent failure and reports how many
// went.
//
// The per-address table is the one that can grow without bound, since it accepts
// any address that ever failed rather than only addresses with a user. An
// operator pruning it on a schedule is the counterpart to the reason it is not
// foreign keyed: a table that cannot grow is not worth attacking, and one that
// can has to be swept.
//
// olderThan is deliberately left to the caller rather than taken from the
// lockout window, because how long to keep a quiet address is an operational
// choice and not something the authentication policy should decide.
func (a *AuthAttempts) PruneIPStale(ctx context.Context, q Querier, olderThan time.Duration) (int64, error) {
	const del = `DELETE FROM auth_ip_attempts WHERE last_failed_at < now() - $1::interval`
	res, err := q.ExecContext(ctx, del, intervalArg(olderThan))
	if err != nil {
		return 0, fmt.Errorf("store: pruning address attempts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: pruning address attempts: %w", err)
	}
	return n, nil
}

// intervalArg renders a duration as a PostgreSQL interval.
//
// The parameter is cast to interval in the query rather than interpolated, so
// the value is still passed as a bind parameter and cannot inject SQL. Passing
// a Go duration straight to the driver would arrive as a number of nanoseconds,
// which PostgreSQL would then read as that many seconds.
func intervalArg(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d/time.Second))
}

// nullableTime maps the zero time to NULL, which is how a subject with no lock
// is stored.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

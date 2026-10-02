package inbound

import (
	"context"
	"net/netip"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/store"
)

// GreylistAction is the greylist verdict for one MAIL FROM.
type GreylistAction string

const (
	// GreyAccept means the sender may continue this transaction.
	GreyAccept GreylistAction = "accept"
	// GreyDefer means the transaction must be rejected with 450 and retried
	// later; legitimate MTAs retry, most spam does not.
	GreyDefer GreylistAction = "defer"
)

// Greylist implements deferred-first-contact greylisting on top of the
// greylist table. State per (sender ip, envelope from):
//
//	first contact        -> defer, remember first_seen_at
//	retry before Delay   -> defer
//	retry after Delay,
//	   within RetryWindow -> accept, remember accepted_at
//	contact after Expiry -> restart the cycle
//	known sender         -> accept
//
// A null envelope sender (bounce) is never greylisted.
type Greylist struct {
	cfg config.Greylisting
}

// NewGreylist builds the greylist from configuration.
func NewGreylist(cfg config.Greylisting) *Greylist {
	return &Greylist{cfg: cfg}
}

// Enabled reports whether greylisting is switched on.
func (g *Greylist) Enabled() bool { return g.cfg.Enabled }

// Check returns the greylist verdict for this (ip, envelope from) pair. A
// non-nil error means the state could not be consulted; the returned action is
// the fail-open choice and the caller should log the error.
func (g *Greylist) Check(ctx context.Context, db *sqlx.DB, ip netip.Addr, envelopeFrom string, now time.Time) (GreylistAction, error) {
	if !g.Enabled() || envelopeFrom == "" {
		return GreyAccept, nil
	}

	var row greyRow
	err := db.GetContext(ctx, &row,
		`SELECT first_seen_at, last_seen_at, expires_at, accepted_at
		 FROM greylist WHERE sender_ip = $1 AND envelope_from = $2`,
		ip.String(), envelopeFrom)
	if err != nil {
		if store.IsNotFound(err) {
			_, uerr := db.ExecContext(ctx, `
				INSERT INTO greylist (sender_ip, envelope_from, first_seen_at, last_seen_at, expires_at)
				VALUES ($1, $2, $3, $3, $4)
				ON CONFLICT (sender_ip, envelope_from) DO UPDATE SET last_seen_at = $3`,
				ip.String(), envelopeFrom, now, now.Add(g.cfg.Expiry))
			return GreyDefer, uerr
		}
		return GreyAccept, err
	}

	if row.AcceptedAt != nil {
		return GreyAccept, nil
	}

	refresh := func() error {
		_, err := db.ExecContext(ctx, `
			UPDATE greylist
			SET first_seen_at = $3, last_seen_at = $3, expires_at = $4, accepted_at = NULL
			WHERE sender_ip = $1 AND envelope_from = $2`,
			ip.String(), envelopeFrom, now, now.Add(g.cfg.Expiry))
		return err
	}
	touch := func() error {
		_, err := db.ExecContext(ctx,
			`UPDATE greylist SET last_seen_at = $3 WHERE sender_ip = $1 AND envelope_from = $2`,
			ip.String(), envelopeFrom, now)
		return err
	}

	// The entry expired without a valid retry: start the cycle over.
	if now.After(row.ExpiresAt) {
		return GreyDefer, refresh()
	}
	// Still cooling down.
	if now.Before(row.FirstSeenAt.Add(g.cfg.Delay)) {
		return GreyDefer, touch()
	}
	// The delay has passed; accept if the retry arrived within the window.
	retryDeadline := row.FirstSeenAt.Add(g.cfg.Delay).Add(g.cfg.RetryWindow)
	if !now.After(retryDeadline) {
		_, err := db.ExecContext(ctx,
			`UPDATE greylist SET accepted_at = $3, last_seen_at = $3
			 WHERE sender_ip = $1 AND envelope_from = $2`,
			ip.String(), envelopeFrom, now)
		return GreyAccept, err
	}
	// The retry window has lapsed: greylist the sender again.
	return GreyDefer, refresh()
}

// Prune removes entries that expired without being accepted. It is called
// opportunistically per connection, not on a timer.
func (g *Greylist) Prune(ctx context.Context, db *sqlx.DB) (int64, error) {
	res, err := db.ExecContext(ctx,
		`DELETE FROM greylist WHERE expires_at < now() AND accepted_at IS NULL`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

type greyRow struct {
	FirstSeenAt time.Time  `db:"first_seen_at"`
	LastSeenAt  time.Time  `db:"last_seen_at"`
	ExpiresAt   time.Time  `db:"expires_at"`
	AcceptedAt  *time.Time `db:"accepted_at"`
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/linchange/linchange/internal/auth/secret"
)

// ErrNoChallenge is returned for an id that does not exist, has expired, or has
// already been consumed.
//
// All three report the same error on purpose. A caller learns whether a login is
// still waiting for a code or whether the window closed, and either way the
// answer is that this attempt has to start again.
var ErrNoChallenge = errors.New("store: no such challenge")

// challengeIDBytes is the entropy in a challenge id. It is a bearer secret for
// as long as the challenge lives, so it is sized like a session id rather than
// like a counter.
const challengeIDBytes = 24

// Challenge is a login that has cleared the password step and is waiting for a
// second factor.
type Challenge struct {
	ID            string
	Address       string
	Impersonating string
	IP            netip.Addr
	UserAgent     string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	// ConsumedAt is non-zero once a second factor has been accepted. The value
	// is kept rather than deleted so that an attempt to reuse a spent challenge
	// is distinguishable from an id that was never real.
	ConsumedAt time.Time
}

// Live reports whether the challenge can still be exchanged at now.
func (c Challenge) Live(now time.Time) bool {
	return c.ConsumedAt.IsZero() && now.Before(c.ExpiresAt)
}

// Challenges holds unconsumed pre-authentication challenges.
type Challenges struct{}

func NewChallenges() *Challenges { return &Challenges{} }

// CreateChallengeRequest is what the password step hands over.
type CreateChallengeRequest struct {
	Address string
	// Impersonating is carried from the first step so the second one cannot
	// change its mind about who is being acted as.
	Impersonating string
	IP            netip.Addr
	UserAgent     string
	TTL           time.Duration
}

type challengeRow struct {
	ID            string         `db:"id"`
	Address       string         `db:"address"`
	Impersonating sql.NullString `db:"impersonating"`
	IPHost        sql.NullString `db:"ip_host"`
	UserAgent     string         `db:"user_agent"`
	CreatedAt     time.Time      `db:"created_at"`
	ExpiresAt     time.Time      `db:"expires_at"`
	ConsumedAt    sql.NullTime   `db:"consumed_at"`
}

func (r challengeRow) toChallenge() Challenge {
	c := Challenge{
		ID:            r.ID,
		Address:       r.Address,
		Impersonating: r.Impersonating.String,
		UserAgent:     r.UserAgent,
		CreatedAt:     r.CreatedAt,
		ExpiresAt:     r.ExpiresAt,
	}
	if r.ConsumedAt.Valid {
		c.ConsumedAt = r.ConsumedAt.Time
	}
	if r.IPHost.Valid {
		if addr, ok := parseIP(r.IPHost.String); ok {
			c.IP = addr
		}
	}
	return c
}

const challengeSelect = `
	SELECT c.id, c.address, c.impersonating, host(c.ip) AS ip_host,
	       c.user_agent, c.created_at, c.expires_at, c.consumed_at
	FROM auth_challenges c`

// CreateChallenge issues an id and stores the pending login.
//
// The user is joined in the read and the write, so a challenge cannot outlive
// the account it names: a disabled user cannot be sent down a second-factor
// path, and a deleted one cannot leave a challenge behind that resolves to
// nothing.
func (c *Challenges) CreateChallenge(ctx context.Context, q Querier, req CreateChallengeRequest) (Challenge, error) {
	var out Challenge

	if req.Address == "" {
		return out, errors.New("store: a challenge needs an address")
	}
	if req.TTL <= 0 {
		return out, errors.New("store: a challenge needs a positive lifetime")
	}

	id, err := secret.Generate(challengeIDBytes)
	if err != nil {
		return out, fmt.Errorf("store: generating a challenge id: %w", err)
	}

	const insert = `
		INSERT INTO auth_challenges (id, address, impersonating, ip, user_agent, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + $6::interval)
		RETURNING created_at, expires_at`

	var createdAt, expiresAt time.Time
	err = q.QueryRowxContext(ctx, insert, id, req.Address, nullIfEmpty(req.Impersonating),
		ipArg(req.IP), req.UserAgent, intervalArg(req.TTL)).Scan(&createdAt, &expiresAt)
	if err != nil {
		return out, fmt.Errorf("store: creating a challenge: %w", err)
	}

	return Challenge{
		ID:            id,
		Address:       req.Address,
		Impersonating: req.Impersonating,
		IP:            req.IP,
		UserAgent:     req.UserAgent,
		CreatedAt:     createdAt,
		ExpiresAt:     expiresAt,
	}, nil
}

// LoadChallenge returns an unconsumed, unexpired challenge.
//
// Expired and consumed rows are not filtered in SQL, because the caller needs no
// difference between them and a row that never existed. They are still filtered
// here so that a stale id cannot be handed to the second-factor step and
// mistaken for a live one.
func (c *Challenges) LoadChallenge(ctx context.Context, q Querier, id string, now time.Time) (Challenge, error) {
	var out Challenge
	if id == "" {
		return out, ErrNoChallenge
	}

	var row challengeRow
	query := challengeSelect + `
		JOIN users u ON u.address = c.address
		WHERE c.id = $1 AND c.consumed_at IS NULL AND c.expires_at > $2 AND u.deleted_at IS NULL`
	if err := q.GetContext(ctx, &row, query, id, now); err != nil {
		if IsNotFound(err) {
			return out, ErrNoChallenge
		}
		return out, fmt.Errorf("store: reading a challenge: %w", err)
	}

	out = row.toChallenge()
	return out, nil
}

// ConsumeChallenge marks a challenge spent and reports whether this call was the
// one that did it.
//
// The update carries the expiry and the unconsumed condition in its WHERE
// clause, so the check and the write are one statement. Two requests presenting
// the same valid second factor at the same moment both pass verification, and
// whichever loses the race gets false here and issues no tokens. Making the
// read-then-write pair atomic is the whole point: done in two statements, both
// requests would see an unconsumed challenge and both would mint a session.
func (c *Challenges) ConsumeChallenge(ctx context.Context, q Querier, id string, now time.Time) (bool, error) {
	if id == "" {
		return false, nil
	}

	const update = `
		UPDATE auth_challenges
		SET consumed_at = now()
		WHERE id = $1 AND consumed_at IS NULL AND expires_at > $2`

	res, err := q.ExecContext(ctx, update, id, now)
	if err != nil {
		return false, fmt.Errorf("store: consuming a challenge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: consuming a challenge: %w", err)
	}
	return n > 0, nil
}

// PurgeChallenges deletes challenges that expired long enough ago to be of no
// forensic interest.
//
// The retention gap is a real trade: deleting the moment a challenge expires
// would keep the table small but destroy the record of how many login attempts a
// given account was in the middle of, which is exactly what an investigation
// wants.
func (c *Challenges) PurgeChallenges(ctx context.Context, q Querier, olderThan time.Duration) (int64, error) {
	const delete = `DELETE FROM auth_challenges WHERE expires_at < now() - $1::interval`

	res, err := q.ExecContext(ctx, delete, intervalArg(olderThan))
	if err != nil {
		return 0, fmt.Errorf("store: purging challenges: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purging challenges: %w", err)
	}
	return n, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ipArg renders an address for an inet column. A zero netip.Addr is SQL NULL
// rather than an invalid address, because a login from behind an unknown proxy
// has no address to record and must still be able to sign in.
func ipArg(addr netip.Addr) any {
	if !addr.IsValid() {
		return nil
	}
	return addr.String()
}

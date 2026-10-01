package store

import (
	"context"
	"fmt"
	"time"
)

// ImpersonationGrant is a domain admin's standing permission to act as one
// specific user. It is narrower than the blanket ability a domain admin has,
// and it expires, because the general power should not quietly become a
// permanent way of reading one colleague's mail.
type ImpersonationGrant struct {
	ActorAddress  string     `db:"actor_address"`
	TargetAddress string     `db:"target_address"`
	Reason        string     `db:"reason"`
	CreatedAt     time.Time  `db:"created_at"`
	ExpiresAt     *time.Time `db:"expires_at"`
	RevokedAt     *time.Time `db:"revoked_at"`
}

// Active reports whether the grant may be relied on right now. A revoked or
// expired grant is not an error, it is simply no longer in force.
func (g ImpersonationGrant) Active(now time.Time) bool {
	if g.RevokedAt != nil {
		return false
	}
	return g.ExpiresAt == nil || g.ExpiresAt.After(now)
}

// ImpersonationGrants reads and writes impersonation_grants.
type ImpersonationGrants struct{}

// NewImpersonationGrants returns the repository. It is stateless.
func NewImpersonationGrants() *ImpersonationGrants { return &ImpersonationGrants{} }

// Grant records a permission. Re-granting the same pair replaces the previous
// one, since the pair is the key and there is only ever one such permission
// between two principals.
func (g *ImpersonationGrants) Grant(ctx context.Context, qr Querier, actor, target, reason string, expiresAt *time.Time) error {
	const query = `
		INSERT INTO impersonation_grants (actor_address, target_address, reason, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (actor_address, target_address) DO UPDATE SET
			reason     = EXCLUDED.reason,
			expires_at = EXCLUDED.expires_at,
			revoked_at = NULL`
	if _, err := qr.ExecContext(ctx, query, actor, target, reason, expiresAt); err != nil {
		if IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: granting impersonation: %w", err)
	}
	return nil
}

const impersonationSelect = `
	SELECT actor_address, target_address, reason, created_at, expires_at, revoked_at
	FROM impersonation_grants`

// Get returns the grant between two principals, or ErrNotFound when there is
// no such grant.
func (g *ImpersonationGrants) Get(ctx context.Context, qr Querier, actor, target string) (ImpersonationGrant, error) {
	var row ImpersonationGrant
	if err := qr.GetContext(ctx, &row, impersonationSelect+`
		WHERE actor_address = $1 AND target_address = $2`, actor, target); err != nil {
		if IsNotFound(err) {
			return ImpersonationGrant{}, ErrNotFound
		}
		return ImpersonationGrant{}, err
	}
	return row, nil
}

// ActiveFor reports whether the actor may impersonate the target right now. A
// missing grant is a plain false rather than an error, because "not permitted"
// is the answer and not a failure to look one up.
func (g *ImpersonationGrants) ActiveFor(ctx context.Context, qr Querier, actor, target string) (bool, error) {
	row, err := g.Get(ctx, qr, actor, target)
	if err != nil {
		if err == ErrNotFound {
			return false, nil
		}
		return false, err
	}
	return row.Active(time.Now()), nil
}

// ListForActor returns every grant an admin holds, live or not, so an
// administration page can show what has been handed out.
func (g *ImpersonationGrants) ListForActor(ctx context.Context, qr Querier, actor string) ([]ImpersonationGrant, error) {
	rows, err := qr.QueryxContext(ctx, impersonationSelect+`
		WHERE actor_address = $1 ORDER BY target_address`, actor)
	if err != nil {
		return nil, fmt.Errorf("store: listing impersonation grants: %w", err)
	}
	defer rows.Close()

	out := make([]ImpersonationGrant, 0, 4)
	for rows.Next() {
		var row ImpersonationGrant
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning impersonation grant: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating impersonation grants: %w", err)
	}
	return out, nil
}

// ListForTarget returns who may impersonate a given user, which is the view
// that answers "who is allowed to read this person's mail".
func (g *ImpersonationGrants) ListForTarget(ctx context.Context, qr Querier, target string) ([]ImpersonationGrant, error) {
	rows, err := qr.QueryxContext(ctx, impersonationSelect+`
		WHERE target_address = $1 ORDER BY actor_address`, target)
	if err != nil {
		return nil, fmt.Errorf("store: listing impersonators: %w", err)
	}
	defer rows.Close()

	out := make([]ImpersonationGrant, 0, 4)
	for rows.Next() {
		var row ImpersonationGrant
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning impersonation grant: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating impersonation grants: %w", err)
	}
	return out, nil
}

// Revoke withdraws a grant. Revoking something that was never granted is
// ErrNotFound, so a caller can tell a real withdrawal from a no-op.
func (g *ImpersonationGrants) Revoke(ctx context.Context, qr Querier, actor, target string) error {
	res, err := qr.ExecContext(ctx,
		`UPDATE impersonation_grants SET revoked_at = now()
		 WHERE actor_address = $1 AND target_address = $2 AND revoked_at IS NULL`,
		actor, target)
	if err != nil {
		return fmt.Errorf("store: revoking impersonation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

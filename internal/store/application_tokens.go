// Package store holds the database access for application passwords.
//
// An application password is a credential of its own: a client presents it in
// place of the account password, the server checks it against the row named by
// its public prefix, and the account's own password is never involved. Rows
// named here are also the short-lived pending approvals through which a client
// asks the browser to create one.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors of the application-password repositories.
var (
	// ErrNoApplicationToken means the prefix names no live credential.
	ErrNoApplicationToken = errors.New("store: no such application password")
	// ErrNoApplicationRequest means the device or user code names no request
	// that can still be answered.
	ErrNoApplicationRequest = errors.New("store: no such application-password request")
)

// ApplicationToken is one application password as the account owner sees it.
type ApplicationToken struct {
	ID          int64      `db:"id" json:"id"`
	Address     string     `db:"address" json:"-"`
	Prefix      string     `db:"prefix" json:"prefix"`
	TokenHash   string     `db:"token_hash" json:"-"`
	Description string     `db:"description" json:"description"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
	LastUsedAt  *time.Time `db:"last_used_at" json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `db:"revoked_at" json:"revoked_at,omitempty"`
}

// ApplicationTokenRequest is a pending approval for a client that is signing
// in through the browser.
type ApplicationTokenRequest struct {
	ID              int64      `db:"id" json:"-"`
	UserCode        string     `db:"user_code" json:"user_code"`
	Description     string     `db:"description" json:"description"`
	Address         *string    `db:"address" json:"-"`
	Status          string     `db:"status" json:"status"`
	IntervalSeconds int        `db:"interval_seconds" json:"interval_seconds"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
	ExpiresAt       time.Time  `db:"expires_at" json:"expires_at"`
	ApprovedAt      *time.Time `db:"approved_at" json:"approved_at,omitempty"`
	ConsumedAt      *time.Time `db:"consumed_at" json:"consumed_at,omitempty"`
}

// ApplicationTokens reads and writes application passwords.
type ApplicationTokens struct{}

// NewApplicationTokens returns the repository.
func NewApplicationTokens() *ApplicationTokens { return &ApplicationTokens{} }

const applicationTokenColumns = `id, address, prefix, token_hash, description, created_at, last_used_at, revoked_at`

// Create stores a new application password. The caller has already generated
// the prefix and hashed the secret part.
func (a *ApplicationTokens) Create(ctx context.Context, q Querier, address, prefix, tokenHash, description string) (ApplicationToken, error) {
	var out ApplicationToken
	prefix = strings.TrimSpace(prefix)
	if address == "" {
		return out, errors.New("store: an application password needs an address")
	}
	if prefix == "" || tokenHash == "" {
		return out, errors.New("store: an application password needs a prefix and a hash")
	}
	const insert = `
		INSERT INTO application_tokens (address, prefix, token_hash, description)
		VALUES ($1, $2, $3, $4)
		RETURNING ` + applicationTokenColumns
	if err := q.GetContext(ctx, &out, insert, address, prefix, tokenHash, strings.TrimSpace(description)); err != nil {
		return ApplicationToken{}, fmt.Errorf("store: creating an application password: %w", err)
	}
	return out, nil
}

// List returns the live application passwords of one account, newest first.
func (a *ApplicationTokens) List(ctx context.Context, q Querier, address string) ([]ApplicationToken, error) {
	out := make([]ApplicationToken, 0, 8)
	const query = `
		SELECT ` + applicationTokenColumns + `
		FROM application_tokens
		WHERE address = $1 AND revoked_at IS NULL
		ORDER BY created_at DESC, id DESC`
	if err := q.SelectContext(ctx, &out, query, address); err != nil {
		return nil, fmt.Errorf("store: listing application passwords: %w", err)
	}
	return out, nil
}

// Revoke ends one application password of one account. A password of another
// account is not found, so the owner cannot revoke somebody else's by id.
func (a *ApplicationTokens) Revoke(ctx context.Context, q Querier, address string, id int64) error {
	row := q.QueryRowxContext(ctx, `
		UPDATE application_tokens
		SET revoked_at = now()
		WHERE id = $1 AND address = $2 AND revoked_at IS NULL
		RETURNING id`, id, address)
	var found int64
	if err := row.Scan(&found); err != nil {
		if IsNotFound(err) {
			return ErrNoApplicationToken
		}
		return fmt.Errorf("store: revoking an application password: %w", err)
	}
	return nil
}

// Find returns the live application password with this public prefix. It does
// not compare the secret: the caller does that in constant time.
func (a *ApplicationTokens) Find(ctx context.Context, q Querier, prefix string) (ApplicationToken, error) {
	var out ApplicationToken
	const query = `
		SELECT ` + applicationTokenColumns + `
		FROM application_tokens
		WHERE prefix = $1 AND revoked_at IS NULL`
	if err := q.GetContext(ctx, &out, query, prefix); err != nil {
		if IsNotFound(err) {
			return ApplicationToken{}, ErrNoApplicationToken
		}
		return ApplicationToken{}, fmt.Errorf("store: reading an application password: %w", err)
	}
	return out, nil
}

// Touch records that an application password was used now.
func (a *ApplicationTokens) Touch(ctx context.Context, q Querier, id int64) error {
	if _, err := q.ExecContext(ctx, `UPDATE application_tokens SET last_used_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: recording application-password use: %w", err)
	}
	return nil
}

// ApplicationTokenRequests reads and writes pending client approvals.
type ApplicationTokenRequests struct{}

// NewApplicationTokenRequests returns the repository.
func NewApplicationTokenRequests() *ApplicationTokenRequests { return &ApplicationTokenRequests{} }

const applicationTokenRequestColumns = `
	id, user_code, description, address, status, interval_seconds,
	created_at, expires_at, approved_at, consumed_at`

// Create stores a pending approval. The device code is already hashed.
func (r *ApplicationTokenRequests) Create(
	ctx context.Context,
	q Querier,
	deviceHash, userCode, description string,
	interval time.Duration,
	ttl time.Duration,
) (ApplicationTokenRequest, error) {
	var out ApplicationTokenRequest
	if deviceHash == "" || userCode == "" {
		return out, errors.New("store: a request needs a device code and a user code")
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if ttl <= 0 {
		return out, errors.New("store: a request needs a positive lifetime")
	}
	const insert = `
		INSERT INTO application_token_requests
			(device_hash, user_code, description, interval_seconds, expires_at)
		VALUES ($1, $2, $3, $4, now() + ($5 * interval '1 second'))
		RETURNING ` + applicationTokenRequestColumns
	seconds := int(interval.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	ttlSeconds := int(ttl.Seconds())
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	if err := q.GetContext(ctx, &out, insert, deviceHash, userCode, strings.TrimSpace(description), seconds, ttlSeconds); err != nil {
		return ApplicationTokenRequest{}, fmt.Errorf("store: creating an application-password request: %w", err)
	}
	return out, nil
}

// ByDeviceHash returns the request a client is polling for.
func (r *ApplicationTokenRequests) ByDeviceHash(ctx context.Context, q Querier, deviceHash string) (ApplicationTokenRequest, error) {
	var out ApplicationTokenRequest
	const query = `SELECT ` + applicationTokenRequestColumns + ` FROM application_token_requests WHERE device_hash = $1`
	if err := q.GetContext(ctx, &out, query, deviceHash); err != nil {
		if IsNotFound(err) {
			return ApplicationTokenRequest{}, ErrNoApplicationRequest
		}
		return ApplicationTokenRequest{}, fmt.Errorf("store: reading an application-password request: %w", err)
	}
	return out, nil
}

// ByUserCode returns a request the browser is showing.
func (r *ApplicationTokenRequests) ByUserCode(ctx context.Context, q Querier, userCode string) (ApplicationTokenRequest, error) {
	var out ApplicationTokenRequest
	const query = `SELECT ` + applicationTokenRequestColumns + ` FROM application_token_requests WHERE user_code = $1`
	if err := q.GetContext(ctx, &out, query, strings.ToUpper(strings.TrimSpace(userCode))); err != nil {
		if IsNotFound(err) {
			return ApplicationTokenRequest{}, ErrNoApplicationRequest
		}
		return ApplicationTokenRequest{}, fmt.Errorf("store: reading an application-password request: %w", err)
	}
	return out, nil
}

// Approve binds a pending request to the signed-in account. Only a request
// that is still pending and not expired can be answered.
func (r *ApplicationTokenRequests) Approve(ctx context.Context, q Querier, userCode, address string) error {
	row := q.QueryRowxContext(ctx, `
		UPDATE application_token_requests
		SET status = 'approved', address = $2, approved_at = now()
		WHERE user_code = $1 AND status = 'pending' AND expires_at > now()
		RETURNING id`, strings.ToUpper(strings.TrimSpace(userCode)), address)
	var id int64
	if err := row.Scan(&id); err != nil {
		if IsNotFound(err) {
			return ErrNoApplicationRequest
		}
		return fmt.Errorf("store: approving an application-password request: %w", err)
	}
	return nil
}

// Deny closes a pending request without creating a password.
func (r *ApplicationTokenRequests) Deny(ctx context.Context, q Querier, userCode string) error {
	row := q.QueryRowxContext(ctx, `
		UPDATE application_token_requests
		SET status = 'denied', consumed_at = now()
		WHERE user_code = $1 AND status = 'pending'
		RETURNING id`, strings.ToUpper(strings.TrimSpace(userCode)))
	var id int64
	if err := row.Scan(&id); err != nil {
		if IsNotFound(err) {
			return ErrNoApplicationRequest
		}
		return fmt.Errorf("store: denying an application-password request: %w", err)
	}
	return nil
}

// Redeem marks an approved request consumed and returns it. The update is the
// permission to create the password: two pollers cannot both redeem one
// approval, and a request approved after it expired is not redeemed.
func (r *ApplicationTokenRequests) Redeem(ctx context.Context, q Querier, deviceHash string) (ApplicationTokenRequest, bool, error) {
	var out ApplicationTokenRequest
	const update = `
		UPDATE application_token_requests
		SET status = 'consumed', consumed_at = now()
		WHERE device_hash = $1 AND status = 'approved' AND expires_at > now()
		RETURNING ` + applicationTokenRequestColumns
	if err := q.GetContext(ctx, &out, update, deviceHash); err != nil {
		if IsNotFound(err) {
			return ApplicationTokenRequest{}, false, nil
		}
		return ApplicationTokenRequest{}, false, fmt.Errorf("store: redeeming an application-password request: %w", err)
	}
	return out, true, nil
}

// Expired reports whether a request can no longer be answered.
func (r ApplicationTokenRequest) Expired(now time.Time) bool {
	return !now.Before(r.ExpiresAt)
}

// Pending reports whether the request is still waiting for the browser.
func (r ApplicationTokenRequest) Pending() bool { return r.Status == "pending" }

// NullableAddress is what an approved request carries; nil until then.
func (r ApplicationTokenRequest) NullableAddress() sql.NullString {
	if r.Address == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *r.Address, Valid: true}
}

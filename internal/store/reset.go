package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ResetTokens issues and redeems the single-use tokens behind self-service
// password reset.
//
// A reset token is 256 bits of randomness. Only its SHA-256 digest ever reaches
// the database, for the same reason a session refresh token's does not: a dump
// of the table must not hand an attacker working reset links. The plaintext
// rides in exactly one email; the digest is what lets the complete step find the
// row that email points at, and nothing else.
type ResetTokens struct{}

// NewResetTokens returns a repository. It is stateless, so this is free and the
// value can be constructed anywhere.
func NewResetTokens() *ResetTokens { return &ResetTokens{} }

// ResetToken is one row of reset_tokens.
type ResetToken struct {
	ID        int64
	Address   string
	ExpiresAt time.Time
	// SentTo is where the link went, kept for the audit trail.
	SentTo    string
	CreatedAt time.Time
	// UsedAt is nil until the token is redeemed.
	UsedAt *time.Time
}

// ErrNoResetToken means the presented digest matches no row at all: the token
// was never issued, or it is simply not one this server knows. It is distinct
// from an expired or already-used token, because the complete step may safely
// tell those apart to a caller who already holds a link.
var ErrNoResetToken = errors.New("store: no live reset token")

// CreateResetTokenRequest is what the request step supplies.
type CreateResetTokenRequest struct {
	Address string
	// TokenHash is the digest of the token; the plaintext never touches the
	// database.
	TokenHash []byte
	// SentTo is the one address the link was sent to.
	SentTo string
	// ExpiresAt is when the link stops working.
	ExpiresAt time.Time
}

// Create stores a fresh token.
func (r *ResetTokens) Create(ctx context.Context, q Querier, req CreateResetTokenRequest) (ResetToken, error) {
	var out ResetToken
	if req.Address == "" || len(req.TokenHash) == 0 || req.SentTo == "" {
		return out, errors.New("store: a reset token needs an address, a digest and a recipient")
	}
	if !req.ExpiresAt.After(time.Now()) {
		return out, errors.New("store: a reset token must expire in the future")
	}

	const insert = `
		INSERT INTO reset_tokens (address, token_hash, expires_at, sent_to)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`
	var createdAt time.Time
	if err := q.QueryRowxContext(ctx, insert,
		req.Address, req.TokenHash, req.ExpiresAt, req.SentTo).Scan(&out.ID, &createdAt); err != nil {
		return out, fmt.Errorf("store: creating a reset token: %w", err)
	}
	out.Address = req.Address
	out.ExpiresAt = req.ExpiresAt
	out.SentTo = req.SentTo
	out.CreatedAt = createdAt
	return out, nil
}

// Find returns the row for a digest, whatever its state, or ErrNoResetToken if
// there is none.
//
// Expired and redeemed rows are not filtered out. The complete step holds the
// secret by the time it calls here, so it may be told "this link expired" or
// "this link was already used" rather than the generic "never seen it", and the
// atomic Consume is what settles any race between two such calls.
func (r *ResetTokens) Find(ctx context.Context, q Querier, tokenHash []byte) (ResetToken, error) {
	var out ResetToken
	if len(tokenHash) == 0 {
		return out, ErrNoResetToken
	}

	var row struct {
		ID        int64        `db:"id"`
		Address   string       `db:"address"`
		CreatedAt time.Time    `db:"created_at"`
		ExpiresAt time.Time    `db:"expires_at"`
		UsedAt    sql.NullTime `db:"used_at"`
		SentTo    string       `db:"sent_to"`
	}
	const query = `
		SELECT id, address, created_at, expires_at, used_at, sent_to
		FROM reset_tokens
		WHERE token_hash = $1`
	if err := q.GetContext(ctx, &row, query, tokenHash); err != nil {
		if IsNotFound(err) {
			return out, ErrNoResetToken
		}
		return out, fmt.Errorf("store: reading a reset token: %w", err)
	}

	out.ID = row.ID
	out.Address = row.Address
	out.CreatedAt = row.CreatedAt
	out.ExpiresAt = row.ExpiresAt
	out.SentTo = row.SentTo
	if row.UsedAt.Valid {
		used := row.UsedAt.Time
		out.UsedAt = &used
	}
	return out, nil
}

// Consume marks a token redeemed and reports whether this call was the one that
// did it.
//
// The update carries "not already used" in its WHERE clause, so the check and
// the write are one statement. Two tabs posting the same link at the same
// moment both pass the complete step's checks, and whichever loses this race
// gets false and must not set a password. Done in two statements, both would
// see an unconsumed token and both would write a password.
func (r *ResetTokens) Consume(ctx context.Context, q Querier, id int64) (bool, error) {
	const update = `
		UPDATE reset_tokens
		SET used_at = now()
		WHERE id = $1 AND used_at IS NULL`
	res, err := q.ExecContext(ctx, update, id)
	if err != nil {
		return false, fmt.Errorf("store: redeeming a reset token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: redeeming a reset token: %w", err)
	}
	return n > 0, nil
}

// PurgeExpired deletes tokens old enough to be of no use and no forensic
// interest. Housekeeping, not security: an expired token is already refused by
// Consume, so nothing depends on this running on time. The retention gap keeps
// the table small without erasing the record of a reset someone actually used.
func (r *ResetTokens) PurgeExpired(ctx context.Context, q Querier) (int64, error) {
	const delete = `DELETE FROM reset_tokens WHERE expires_at < now() - interval '1 day'`
	res, err := q.ExecContext(ctx, delete)
	if err != nil {
		return 0, fmt.Errorf("store: purging reset tokens: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purging reset tokens: %w", err)
	}
	return n, nil
}

// SecondaryEmails holds the out-of-band addresses a principal can be reached at
// for password reset. A reset link is a credential, so only these are ever
// recipients of one, never the primary mailbox (which is what is being reset).
type SecondaryEmails struct{}

// NewSecondaryEmails returns a repository. It is stateless.
func NewSecondaryEmails() *SecondaryEmails { return &SecondaryEmails{} }

// SecondaryEmail is one row of user_secondary_emails.
type SecondaryEmail struct {
	Email     string
	IsPrimary bool
	// VerifiedAt is nil until the user has proved they hold the address. Only
	// a verified address may receive a reset link.
	VerifiedAt *time.Time
}

// List returns every secondary email for an address, the designated reset
// target first. An address with no rows comes back empty and not an error:
// "no out-of-band address" is a normal state the caller must not treat as a
// fault.
func (s *SecondaryEmails) List(ctx context.Context, q Querier, address string) ([]SecondaryEmail, error) {
	if address == "" {
		return nil, nil
	}

	const query = `
		SELECT email, is_primary, verified_at
		FROM user_secondary_emails
		WHERE address = $1
		ORDER BY is_primary DESC, email ASC`
	rows, err := q.QueryxContext(ctx, query, address)
	if err != nil {
		return nil, fmt.Errorf("store: listing secondary emails: %w", err)
	}
	defer rows.Close()

	var out []SecondaryEmail
	for rows.Next() {
		var e SecondaryEmail
		var verified sql.NullTime
		if err := rows.Scan(&e.Email, &e.IsPrimary, &verified); err != nil {
			return nil, fmt.Errorf("store: scanning a secondary email: %w", err)
		}
		if verified.Valid {
			t := verified.Time
			e.VerifiedAt = &t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

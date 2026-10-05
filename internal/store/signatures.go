package store

import (
	"context"
	"fmt"
)

// Signature is the HTML a user appends to the mail they send.
//
// The zero value is a valid signature: HTML is empty and there is nothing to
// append. That is deliberately the same as having no row at all, because the two
// mean the same thing to a sender and collapsing them means the read path cannot
// tell a user who has never set one from a user who has cleared one.
type Signature struct {
	Owner string `db:"owner"`
	HTML  string `db:"html"`
}

// Signatures reads and writes the user_signatures table.
type Signatures struct{}

// NewSignatures returns the repository. It is stateless, so this is free.
func NewSignatures() *Signatures { return &Signatures{} }

const signatureColumns = `owner, html`

// Get returns an owner's signature.
//
// A missing row is not an error: every mailbox owner is allowed to send without
// one, so the not-found case is the ordinary case and it comes back as an empty
// signature. Reporting it as ErrNotFound would push a "does this user have a
// signature" decision onto every caller for no gain.
func (s *Signatures) Get(ctx context.Context, q Querier, owner string) (Signature, error) {
	const query = `SELECT ` + signatureColumns + ` FROM user_signatures WHERE owner = $1`

	var sig Signature
	err := q.GetContext(ctx, &sig, query, owner)
	if err != nil {
		if IsNotFound(err) {
			return Signature{Owner: owner}, nil
		}
		return Signature{}, fmt.Errorf("store: read signature for %s: %w", owner, err)
	}
	return sig, nil
}

// Set writes an owner's signature, replacing whatever was there.
//
// The owner is a parameter rather than something taken from the Signature so
// that it cannot disagree with the column that is the foreign key: a signature
// stored under somebody else's address would be appended to their mail by every
// sender, and the caller that supplied it was acting on a token, not on a row.
//
// An empty HTML is stored rather than deleted. The distinction between "has a
// signature that happens to be empty" and "has never set one" is not one any
// caller acts on, and keeping the row means Save is idempotent rather than
// alternating between an insert and a delete.
func (s *Signatures) Set(ctx context.Context, q Querier, owner, html string) error {
	const query = `
		INSERT INTO user_signatures (owner, html, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (owner) DO UPDATE
		SET html = EXCLUDED.html, updated_at = now()`

	if _, err := q.ExecContext(ctx, query, owner, html); err != nil {
		return fmt.Errorf("store: write signature for %s: %w", owner, err)
	}
	return nil
}

// Clear removes an owner's signature, so they send without one.
func (s *Signatures) Clear(ctx context.Context, q Querier, owner string) error {
	const query = `DELETE FROM user_signatures WHERE owner = $1`

	if _, err := q.ExecContext(ctx, query, owner); err != nil {
		return fmt.Errorf("store: clear signature for %s: %w", owner, err)
	}
	return nil
}

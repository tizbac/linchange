package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
)

// LoadPrincipal reads the policy identity for an address.
//
// The session and login paths both need the current attributes rather than a
// principal captured earlier, because the row may have been disabled or had its
// mailbox rights withdrawn in between. Re-reading is the difference between a
// session that reflects the database and one that reflects a memory of it.
//
// A disabled account is reported as ErrNotFound rather than as a principal with
// Enabled false, so a caller cannot accidentally treat "disabled" as a login
// that merely needs a different password.
func LoadPrincipal(ctx context.Context, q Querier, address string) (policy.Principal, error) {
	if address == "" {
		return policy.Principal{}, ErrNotFound
	}

	const query = `
		SELECT address, source, enabled, mailbox_enabled, must_change_password,
		       is_domain_admin
		FROM users
		WHERE address = $1 AND deleted_at IS NULL`

	var row principalRow
	if err := q.GetContext(ctx, &row, query, address); err != nil {
		if IsNotFound(err) {
			return policy.Principal{}, ErrNotFound
		}
		return policy.Principal{}, err
	}

	if !row.Enabled {
		return policy.Principal{}, ErrNotFound
	}
	return row.principal(), nil
}

// principalRow is the subset of users that a policy decision needs.
type principalRow struct {
	Address            string `db:"address"`
	Source             string `db:"source"`
	Enabled            bool   `db:"enabled"`
	MailboxEnabled     bool   `db:"mailbox_enabled"`
	MustChangePassword bool   `db:"must_change_password"`
	IsDomainAdmin      bool   `db:"is_domain_admin"`
}

// principal converts a row into a policy identity.
//
// Authenticated is false: reading a user from the database says who they are,
// not that they have proved it. Only the credential paths set it, so a caller
// cannot mistake a lookup for a successful authentication.
func (r principalRow) principal() policy.Principal {
	return policy.Principal{
		Address:            r.Address,
		Source:             policy.Source(r.Source),
		Enabled:            r.Enabled,
		MailboxEnabled:     r.MailboxEnabled,
		MustChangePassword: r.MustChangePassword,
		IsDomainAdmin:      r.IsDomainAdmin,
		Authenticated:      false,
	}
}

// Querier is the subset of sqlx that the repositories in this package use.
//
// Both *sqlx.DB and *sqlx.Tx satisfy it, which is what lets a caller run a
// session operation inside a transaction without the repository knowing which
// it was handed.
type Querier interface {
	GetContext(ctx context.Context, dest any, query string, args ...any) error
	QueryRowxContext(ctx context.Context, query string, args ...any) *sqlx.Row
	QueryxContext(ctx context.Context, query string, args ...any) (*sqlx.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	NamedExecContext(ctx context.Context, query string, arg any) (sql.Result, error)
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
}

// assert both handle types satisfy the interface at compile time.
var (
	_ Querier = (*sqlx.DB)(nil)
	_ Querier = (*sqlx.Tx)(nil)
)

// parseIP turns the textual form of an inet column into an address.
//
// The column is read as text rather than scanned straight into netip.Addr
// because a NULL inet has no zero value to scan into, and a session row is
// allowed to have no recorded address.
func parseIP(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}

// SetMustChangePassword records whether a principal must choose a new password
// before doing anything except signing in and changing it.
//
// The flag lives on users rather than alongside the password hash because it is a
// property of the principal: a domain can require a change for an account whose
// password is not held here at all, and a session or an access token carries
// this copy, so it is the one already on the paths that enforce it. Keeping a
// second copy on credentials meant two flags for one fact, and the ways they
// could disagree were both bad.
func SetMustChangePassword(ctx context.Context, q Querier, address string, mustChange bool) error {
	const update = `
		UPDATE users
		SET must_change_password = $2, updated_at = now()
		WHERE address = $1 AND deleted_at IS NULL`

	res, err := q.ExecContext(ctx, update, address, mustChange)
	if err != nil {
		return fmt.Errorf("store: setting must_change_password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

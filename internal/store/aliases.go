package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AliasTargetKind mirrors the alias_target_kind enum.
type AliasTargetKind string

const (
	// AliasTargetUser delivers into a person's mailbox.
	AliasTargetUser AliasTargetKind = "user"
	// AliasTargetShared delivers into a shared mailbox.
	AliasTargetShared AliasTargetKind = "shared"
	// AliasTargetWorkgroup expands to a workgroup's members.
	AliasTargetWorkgroup AliasTargetKind = "workgroup"
)

// Alias is a second address that delivers into something that already exists.
// It has no folders, no messages and no quota of its own: deleting it changes
// nothing that was already received, which is what separates it from a shared
// mailbox.
type Alias struct {
	Address    string          `db:"address"`
	TargetKind AliasTargetKind `db:"target_kind"`
	// Exactly one of TargetAddress or WorkgroupID is set, matching TargetKind;
	// the schema check makes that true.
	TargetAddress *string   `db:"target_address"`
	WorkgroupID   *int64    `db:"workgroup_id"`
	DisplayName   string    `db:"display_name"`
	Description   string    `db:"description"`
	Enabled       bool      `db:"enabled"`
	CreatedAt     time.Time `db:"created_at"`
	UpdatedAt     time.Time `db:"updated_at"`
}

// CreateAliasRequest names a new alias and what it resolves to. The caller sets
// the target that matches the kind; the other is ignored, so a workgroup alias
// cannot accidentally carry a stray address.
type CreateAliasRequest struct {
	Address       string
	TargetKind    AliasTargetKind
	TargetAddress *string
	WorkgroupID   *int64
	DisplayName   string
	Description   string
	UpdatedBy     *string
}

// Aliases reads and writes the aliases table.
type Aliases struct{}

// NewAliases returns the repository. It is stateless, so this is free.
func NewAliases() *Aliases { return &Aliases{} }

// Create adds an alias. The target must already exist (a foreign key), and the
// kind must agree with the target (a check constraint); both surface as
// ErrConflict so the caller can tell a bad reference from a real failure.
func (a *Aliases) Create(ctx context.Context, q Querier, req CreateAliasRequest) error {
	if req.Address == "" {
		return errors.New("store: alias address is empty")
	}
	target := req.TargetAddress
	workgroup := req.WorkgroupID
	switch req.TargetKind {
	case AliasTargetWorkgroup:
		target = nil
	case AliasTargetUser, AliasTargetShared:
		workgroup = nil
	default:
		return fmt.Errorf("store: unknown alias target kind %q", req.TargetKind)
	}

	const query = `
		INSERT INTO aliases (address, target_kind, target_address, workgroup_id,
		                      display_name, description, enabled, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, true, $7)`
	if _, err := q.ExecContext(ctx, query,
		req.Address, req.TargetKind, target, workgroup,
		req.DisplayName, req.Description, req.UpdatedBy); err != nil {
		if IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: creating alias: %w", err)
	}
	return nil
}

const aliasSelect = `
	SELECT address, target_kind, target_address, workgroup_id, display_name,
	       description, enabled, created_at, updated_at
	FROM aliases`

// Get returns an alias by its address, or ErrNotFound.
func (a *Aliases) Get(ctx context.Context, q Querier, address string) (Alias, error) {
	var row Alias
	if err := q.GetContext(ctx, &row, aliasSelect+` WHERE address = $1`, address); err != nil {
		if IsNotFound(err) {
			return Alias{}, ErrNotFound
		}
		return Alias{}, err
	}
	return row, nil
}

// List returns every alias, ordered by address.
func (a *Aliases) List(ctx context.Context, q Querier) ([]Alias, error) {
	rows, err := q.QueryxContext(ctx, aliasSelect+` ORDER BY address`)
	if err != nil {
		return nil, fmt.Errorf("store: listing aliases: %w", err)
	}
	defer rows.Close()

	out := make([]Alias, 0, 16)
	for rows.Next() {
		var row Alias
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning alias: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating aliases: %w", err)
	}
	return out, nil
}

// ListForTarget answers "which aliases deliver into this address". It is the
// reverse lookup the delivery path uses, so it is kept here rather than
// scattered through the mail code.
func (a *Aliases) ListForTarget(ctx context.Context, q Querier, target string) ([]Alias, error) {
	rows, err := q.QueryxContext(ctx, aliasSelect+` WHERE target_address = $1 ORDER BY address`, target)
	if err != nil {
		return nil, fmt.Errorf("store: resolving aliases: %w", err)
	}
	defer rows.Close()

	out := make([]Alias, 0, 4)
	for rows.Next() {
		var row Alias
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning alias: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating aliases: %w", err)
	}
	return out, nil
}

// SetEnabled turns an alias on or off. A disabled alias is left in place but
// delivers nowhere, which is how you park an address without losing its
// history of what it pointed at.
func (a *Aliases) SetEnabled(ctx context.Context, q Querier, address string, enabled bool) error {
	res, err := q.ExecContext(ctx,
		`UPDATE aliases SET enabled = $2, updated_at = now() WHERE address = $1`,
		address, enabled)
	if err != nil {
		return fmt.Errorf("store: setting alias enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes an alias. Anything already delivered to its target is
// untouched, because the alias owned nothing.
func (a *Aliases) Delete(ctx context.Context, q Querier, address string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM aliases WHERE address = $1`, address)
	if err != nil {
		return fmt.Errorf("store: deleting alias: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

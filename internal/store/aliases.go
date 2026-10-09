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

// AliasTarget represents one destination of an alias.
type AliasTarget struct {
	AliasAddress string          `db:"alias_address"`
	TargetKind   AliasTargetKind `db:"target_kind"`
	TargetAddress *string        `db:"target_address"`
	WorkgroupID  *int64          `db:"workgroup_id"`
	Position     int             `db:"position"`
	CreatedAt    time.Time       `db:"created_at"`
}

// Alias is a second address that delivers into one or more destinations.
// It has no folders, no messages and no quota of its own: deleting it changes
// nothing that was already received, which is what separates it from a shared
// mailbox.
type Alias struct {
	Address      string          `db:"address"`
	DisplayName  string          `db:"display_name"`
	Description  string          `db:"description"`
	Enabled      bool            `db:"enabled"`
	CreatedAt    time.Time       `db:"created_at"`
	UpdatedAt    time.Time       `db:"updated_at"`
	// Targets are populated by List/Get when needed
	Targets []AliasTarget `db:"-"`
}

// CreateAliasRequest names a new alias and what it resolves to.
type CreateAliasRequest struct {
	Address   string
	Targets   []AliasTargetRequest
	DisplayName string
	Description string
	UpdatedBy   *string
}

// AliasTargetRequest represents a single target in a create/update request.
type AliasTargetRequest struct {
	TargetKind    AliasTargetKind
	TargetAddress *string
	WorkgroupID   *int64
	Position      int
}

// Aliases reads and writes the aliases table.
type Aliases struct{}

// NewAliases returns the repository. It is stateless, so this is free.
func NewAliases() *Aliases { return &Aliases{} }

// Create adds an alias with one or more targets.
// The caller is responsible for transaction management; q may be a *sqlx.DB or *sqlx.Tx.
func (a *Aliases) Create(ctx context.Context, q Querier, req CreateAliasRequest) error {
	if req.Address == "" {
		return errors.New("store: alias address is empty")
	}
	if len(req.Targets) == 0 {
		return errors.New("store: alias must have at least one target")
	}

	// Create the alias
	const aliasQuery = `
		INSERT INTO aliases (address, display_name, description, enabled, updated_by)
		VALUES ($1, $2, $3, true, $4)`
	if _, err := q.ExecContext(ctx, aliasQuery,
		req.Address, req.DisplayName, req.Description, req.UpdatedBy); err != nil {
		if IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: creating alias: %w", err)
	}

	// Create targets
	const targetQuery = `
		INSERT INTO alias_targets (alias_address, target_kind, target_address, workgroup_id, position)
		VALUES ($1, $2, $3, $4, $5)`
	for _, t := range req.Targets {
		target := t.TargetAddress
		workgroup := t.WorkgroupID
		switch t.TargetKind {
		case AliasTargetWorkgroup:
			target = nil
		case AliasTargetUser, AliasTargetShared:
			workgroup = nil
		default:
			return fmt.Errorf("store: unknown alias target kind %q", t.TargetKind)
		}
		if _, err := q.ExecContext(ctx, targetQuery,
			req.Address, t.TargetKind, target, workgroup, t.Position); err != nil {
			if IsConflict(err) {
				return ErrConflict
			}
			return fmt.Errorf("store: creating alias target: %w", err)
		}
	}

	return nil
}

// Get returns an alias by its address with its targets, or ErrNotFound.
func (a *Aliases) Get(ctx context.Context, q Querier, address string) (Alias, error) {
	var row Alias
	const aliasQuery = `
		SELECT address, display_name, description, enabled, created_at, updated_at
		FROM aliases WHERE address = $1`
	if err := q.GetContext(ctx, &row, aliasQuery, address); err != nil {
		if IsNotFound(err) {
			return Alias{}, ErrNotFound
		}
		return Alias{}, err
	}

	// Load targets
	const targetQuery = `
		SELECT alias_address, target_kind, target_address, workgroup_id, position, created_at
		FROM alias_targets WHERE alias_address = $1 ORDER BY position, created_at`
	if err := q.SelectContext(ctx, &row.Targets, targetQuery, address); err != nil {
		return Alias{}, fmt.Errorf("store: loading alias targets: %w", err)
	}
	return row, nil
}

// List returns every alias with its targets, ordered by address.
func (a *Aliases) List(ctx context.Context, q Querier) ([]Alias, error) {
	rows, err := q.QueryxContext(ctx, `
		SELECT address, display_name, description, enabled, created_at, updated_at
		FROM aliases ORDER BY address`)
	if err != nil {
		return nil, fmt.Errorf("store: listing aliases: %w", err)
	}
	defer rows.Close()

	aliases := make([]Alias, 0, 16)
	for rows.Next() {
		var a Alias
		if err := rows.StructScan(&a); err != nil {
			return nil, fmt.Errorf("store: scanning alias: %w", err)
		}
		aliases = append(aliases, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating aliases: %w", err)
	}

	// Load targets for each alias
	for i := range aliases {
		const targetQuery = `
			SELECT alias_address, target_kind, target_address, workgroup_id, position, created_at
			FROM alias_targets WHERE alias_address = $1 ORDER BY position, created_at`
		if err := q.SelectContext(ctx, &aliases[i].Targets, targetQuery, aliases[i].Address); err != nil {
			return nil, fmt.Errorf("store: loading targets for %s: %w", aliases[i].Address, err)
		}
	}
	return aliases, nil
}

// ListForTarget answers "which aliases deliver into this address".
func (a *Aliases) ListForTarget(ctx context.Context, q Querier, target string) ([]Alias, error) {
	rows, err := q.QueryxContext(ctx, `
		SELECT DISTINCT a.address, a.display_name, a.description, a.enabled, a.created_at, a.updated_at
		FROM aliases a
		JOIN alias_targets at ON at.alias_address = a.address
		WHERE at.target_address = $1
		ORDER BY a.address`, target)
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

// SetEnabled turns an alias on or off.
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

// Delete removes an alias and all its targets.
func (a *Aliases) Delete(ctx context.Context, q Querier, address string) error {
	// Targets are deleted by cascade
	res, err := q.ExecContext(ctx, `DELETE FROM aliases WHERE address = $1`, address)
	if err != nil {
		return fmt.Errorf("store: deleting alias: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateTargets replaces all targets for an alias.
// The caller is responsible for transaction management; q may be a *sqlx.DB or *sqlx.Tx.
func (a *Aliases) UpdateTargets(ctx context.Context, q Querier, address string, targets []AliasTargetRequest) error {
	if len(targets) == 0 {
		return errors.New("store: alias must have at least one target")
	}

	// Delete existing targets
	if _, err := q.ExecContext(ctx, `DELETE FROM alias_targets WHERE alias_address = $1`, address); err != nil {
		return fmt.Errorf("store: deleting old targets: %w", err)
	}

	// Insert new targets
	const targetQuery = `
		INSERT INTO alias_targets (alias_address, target_kind, target_address, workgroup_id, position)
		VALUES ($1, $2, $3, $4, $5)`
	for _, t := range targets {
		target := t.TargetAddress
		workgroup := t.WorkgroupID
		switch t.TargetKind {
		case AliasTargetWorkgroup:
			target = nil
		case AliasTargetUser, AliasTargetShared:
			workgroup = nil
		default:
			return fmt.Errorf("store: unknown alias target kind %q", t.TargetKind)
		}
		if _, err := q.ExecContext(ctx, targetQuery,
			address, t.TargetKind, target, workgroup, t.Position); err != nil {
			if IsConflict(err) {
				return ErrConflict
			}
			return fmt.Errorf("store: creating alias target: %w", err)
		}
	}

	// Update alias timestamp
	if _, err := q.ExecContext(ctx,
		`UPDATE aliases SET updated_at = now() WHERE address = $1`, address); err != nil {
		return fmt.Errorf("store: updating alias timestamp: %w", err)
	}

	return nil
}
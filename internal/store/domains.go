package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DomainStatus mirrors the domain_status enum. It is a string so a value that
// crosses a package boundary stays readable in a log.
type DomainStatus string

const (
	// DomainStatusActive accepts and delivers mail.
	DomainStatusActive DomainStatus = "active"
	// DomainStatusSuspended holds mail but does not accept new inbound.
	DomainStatusSuspended DomainStatus = "suspended"
	// DomainStatusDisabled does not accept or deliver mail at all.
	DomainStatusDisabled DomainStatus = "disabled"
)

// Valid reports whether s is one of the statuses the enum allows. A status
// arriving from a request is checked with this rather than by hand, so the
// API refuses a bad value with a message naming it instead of letting it reach
// the database as a cast error.
func (s DomainStatus) Valid() bool {
	switch s {
	case DomainStatusActive, DomainStatusSuspended, DomainStatusDisabled:
		return true
	}
	return false
}

// DefaultDomainQuotaBytes is what a new domain inherits for mailboxes that do
// not carry a quota of their own. It matches the column default in the schema.
const DefaultDomainQuotaBytes int64 = 10 * 1024 * 1024 * 1024

// Domain is a domain this server hosts.
type Domain struct {
	Name              string
	Status            DomainStatus
	IsLocal           bool
	Description       string
	DefaultQuotaBytes int64
	// DailySendLimit is a per-user override of the submission cap; nil inherits.
	DailySendLimit *int
	// DKIMSelector is the selector under which this domain's signing key lives.
	DKIMSelector *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CreateDomainRequest is what an administrator supplies to add a domain. Nil
// pointers fall back to the schema defaults, so a minimal request is just a
// name.
type CreateDomainRequest struct {
	Name              string
	Status            DomainStatus // empty -> active
	IsLocal           bool
	Description       string
	DefaultQuotaBytes *int64
	DailySendLimit    *int
	DKIMSelector      *string
}

// UpdateDomainRequest carries the mutable fields of a domain. Unlike the
// create form these are not optional: the caller supplies the whole row, which
// is what a settings page already has.
type UpdateDomainRequest struct {
	Status            DomainStatus
	IsLocal           bool
	Description       string
	DefaultQuotaBytes int64
	DailySendLimit    *int
	DKIMSelector      *string
}

// domainRow is the table as scanned. Nullable columns are read through
// sql.NullString so a row without them is not a scan error.
type domainRow struct {
	Name              string         `db:"name"`
	Status            DomainStatus   `db:"status"`
	IsLocal           bool           `db:"is_local"`
	Description       sql.NullString `db:"description"`
	DefaultQuotaBytes int64          `db:"default_quota_bytes"`
	DailySendLimit    *int           `db:"daily_send_limit"`
	DKIMSelector      *string        `db:"dkim_selector"`
	CreatedAt         time.Time      `db:"created_at"`
	UpdatedAt         time.Time      `db:"updated_at"`
}

func (r domainRow) domain() Domain {
	desc := ""
	if r.Description.Valid {
		desc = r.Description.String
	}
	return Domain{
		Name:              r.Name,
		Status:            r.Status,
		IsLocal:           r.IsLocal,
		Description:       desc,
		DefaultQuotaBytes: r.DefaultQuotaBytes,
		DailySendLimit:    r.DailySendLimit,
		DKIMSelector:      r.DKIMSelector,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

const domainSelect = `
	SELECT name, status, is_local, description, default_quota_bytes,
	       daily_send_limit, dkim_selector, created_at, updated_at
	FROM domains`

// Domains reads and writes the domains table.
type Domains struct{}

// NewDomains returns the repository. It is stateless, so this is free.
func NewDomains() *Domains { return &Domains{} }

// Create adds a domain. A name that already exists is reported as ErrConflict
// so the caller can distinguish "taken" from a real failure.
func (d *Domains) Create(ctx context.Context, q Querier, req CreateDomainRequest) error {
	if req.Name == "" {
		return errors.New("store: domain name is empty")
	}
	status := req.Status
	if status == "" {
		status = DomainStatusActive
	}
	quota := DefaultDomainQuotaBytes
	if req.DefaultQuotaBytes != nil {
		quota = *req.DefaultQuotaBytes
	}

	const query = `
		INSERT INTO domains (name, status, is_local, description,
		                      default_quota_bytes, daily_send_limit, dkim_selector)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	if _, err := q.ExecContext(ctx, query,
		req.Name, status, req.IsLocal, req.Description,
		quota, req.DailySendLimit, req.DKIMSelector); err != nil {
		if IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: creating domain: %w", err)
	}
	return nil
}

// Get returns a domain by name, or ErrNotFound.
func (d *Domains) Get(ctx context.Context, q Querier, name string) (Domain, error) {
	var row domainRow
	if err := q.GetContext(ctx, &row, domainSelect+` WHERE name = $1`, name); err != nil {
		if IsNotFound(err) {
			return Domain{}, ErrNotFound
		}
		return Domain{}, err
	}
	return row.domain(), nil
}

// List returns every domain, ordered by name.
func (d *Domains) List(ctx context.Context, q Querier) ([]Domain, error) {
	rows, err := q.QueryxContext(ctx, domainSelect+` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing domains: %w", err)
	}
	defer rows.Close()

	out := make([]Domain, 0, 8)
	for rows.Next() {
		var row domainRow
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning domain: %w", err)
		}
		out = append(out, row.domain())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating domains: %w", err)
	}
	return out, nil
}

// Update replaces the mutable fields of a domain.
func (d *Domains) Update(ctx context.Context, q Querier, name string, req UpdateDomainRequest) error {
	const query = `
		UPDATE domains SET
			status = $2, is_local = $3, description = $4,
			default_quota_bytes = $5, daily_send_limit = $6, dkim_selector = $7,
			updated_at = now()
		WHERE name = $1`
	res, err := q.ExecContext(ctx, query,
		name, req.Status, req.IsLocal, req.Description,
		req.DefaultQuotaBytes, req.DailySendLimit, req.DKIMSelector)
	if err != nil {
		return fmt.Errorf("store: updating domain: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetStatus changes only the status of a domain, leaving the rest untouched.
func (d *Domains) SetStatus(ctx context.Context, q Querier, name string, status DomainStatus) error {
	res, err := q.ExecContext(ctx,
		`UPDATE domains SET status = $2, updated_at = now() WHERE name = $1`,
		name, status)
	if err != nil {
		return fmt.Errorf("store: setting domain status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a domain.
func (d *Domains) Delete(ctx context.Context, q Querier, name string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM domains WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("store: deleting domain: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

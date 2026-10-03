package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// Workgroup is a named group of people that can be addressed as a recipient and
// can own a shared calendar.
//
// Two things about it are worth stating because they are easy to get backwards:
//
//   - Any authenticated user may book time on a workgroup calendar, not only its
//     members. A meeting room or a rota is booked by whoever needs it.
//   - Membership controls administration of the workgroup itself: its calendars,
//     its address and its description. Booking rights and administrative rights
//     are deliberately separate, which is what the open_booking flag records.
type Workgroup struct {
	ID          int64     `db:"id"`
	Name        string    `db:"name"`
	Domain      string    `db:"domain"`
	DisplayName string    `db:"display_name"`
	Description string    `db:"description"`
	OpenBooking bool      `db:"open_booking"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

// Membership roles.
const (
	// WorkgroupRoleMember can book onto the group's calendars.
	WorkgroupRoleMember = "member"
	// WorkgroupRoleAdmin can also administer the workgroup and its calendars.
	WorkgroupRoleAdmin = "admin"
)

// WorkgroupMember is one person's membership of a workgroup.
type WorkgroupMember struct {
	WorkgroupID int64     `db:"workgroup_id"`
	Address     string    `db:"address"`
	Role        string    `db:"role"`
	CreatedAt   time.Time `db:"created_at"`
}

// IsAdmin reports whether the member may administer the workgroup.
func (m WorkgroupMember) IsAdmin() bool { return m.Role == WorkgroupRoleAdmin }

// Workgroups reads and writes the workgroups tables.
type Workgroups struct{}

// NewWorkgroups returns the repository. It is stateless, so this is free.
func NewWorkgroups() *Workgroups { return &Workgroups{} }

// CreateWorkgroupRequest declares a new workgroup.
type CreateWorkgroupRequest struct {
	Name        string
	Domain      string
	DisplayName string
	Description string
	// OpenBooking defaults to true when nil, matching the schema: a new group
	// can be booked straight away rather than needing an explicit opt-in.
	OpenBooking *bool
	// Members are added in the same transaction as the workgroup, so a group is
	// never briefly visible with nobody in it.
	Members []WorkgroupMember
}

const workgroupColumns = `
	id, name, domain, display_name, description, open_booking,
	created_at, updated_at`

// Create adds a workgroup and its founding members.
func (w *Workgroups) Create(ctx context.Context, q Querier, req CreateWorkgroupRequest) (Workgroup, error) {
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Domain) == "" {
		return Workgroup{}, errors.New("store: a workgroup needs a name and a domain")
	}
	openBooking := true
	if req.OpenBooking != nil {
		openBooking = *req.OpenBooking
	}

	var out Workgroup
	err := InQuerierTx(ctx, q, func(tx *sqlx.Tx) error {
		err := tx.GetContext(ctx, &out, `
			INSERT INTO workgroups (name, domain, display_name, description, open_booking)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+workgroupColumns,
			req.Name, req.Domain, req.DisplayName, req.Description, openBooking)
		if err != nil {
			if IsConflict(err) {
				return ErrConflict
			}
			return fmt.Errorf("store: creating workgroup: %w", err)
		}
		for _, m := range req.Members {
			role := m.Role
			if role == "" {
				role = WorkgroupRoleMember
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO workgroup_members (workgroup_id, address, role)
				VALUES ($1, $2, $3)`, out.ID, m.Address, role); err != nil {
				if IsConflict(err) {
					return fmt.Errorf("store: adding workgroup member: %w", ErrConflict)
				}
				return fmt.Errorf("store: adding workgroup member: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return Workgroup{}, err
	}
	return out, nil
}

// Get returns a workgroup by id, or ErrNotFound.
func (w *Workgroups) Get(ctx context.Context, q Querier, id int64) (Workgroup, error) {
	var out Workgroup
	err := q.GetContext(ctx, &out,
		`SELECT `+workgroupColumns+` FROM workgroups WHERE id = $1`, id)
	if IsNotFound(err) {
		return Workgroup{}, ErrNotFound
	}
	if err != nil {
		return Workgroup{}, fmt.Errorf("store: loading workgroup: %w", err)
	}
	return out, nil
}

// ByName returns a workgroup by the name it is addressed by, which is how an
// alias or a calendar setting refers to one.
func (w *Workgroups) ByName(ctx context.Context, q Querier, domain, name string) (Workgroup, error) {
	var out Workgroup
	err := q.GetContext(ctx, &out,
		`SELECT `+workgroupColumns+` FROM workgroups WHERE domain = $1 AND name = $2`,
		domain, name)
	if IsNotFound(err) {
		return Workgroup{}, ErrNotFound
	}
	if err != nil {
		return Workgroup{}, fmt.Errorf("store: loading workgroup by name: %w", err)
	}
	return out, nil
}

// List returns every workgroup, by domain and then name.
func (w *Workgroups) List(ctx context.Context, q Querier) ([]Workgroup, error) {
	rows, err := q.QueryxContext(ctx,
		`SELECT `+workgroupColumns+` FROM workgroups ORDER BY domain, name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing workgroups: %w", err)
	}
	defer rows.Close()

	out := make([]Workgroup, 0, 8)
	for rows.Next() {
		var row Workgroup
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning workgroup: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating workgroups: %w", err)
	}
	return out, nil
}

// Members returns a workgroup's membership.
func (w *Workgroups) Members(ctx context.Context, q Querier, workgroupID int64) ([]WorkgroupMember, error) {
	out := make([]WorkgroupMember, 0, 8)
	err := q.SelectContext(ctx, &out, `
		SELECT workgroup_id, address, role, created_at
		FROM workgroup_members WHERE workgroup_id = $1
		ORDER BY CASE role WHEN 'admin' THEN 0 ELSE 1 END, address`, workgroupID)
	if err != nil && !IsNotFound(err) {
		return nil, fmt.Errorf("store: listing workgroup members: %w", err)
	}
	return out, nil
}

// Member returns one person's membership, or ErrNotFound.
func (w *Workgroups) Member(ctx context.Context, q Querier, workgroupID int64, address string) (WorkgroupMember, error) {
	var out WorkgroupMember
	err := q.GetContext(ctx, &out, `
		SELECT workgroup_id, address, role, created_at
		FROM workgroup_members WHERE workgroup_id = $1 AND address = $2`,
		workgroupID, address)
	if IsNotFound(err) {
		return WorkgroupMember{}, ErrNotFound
	}
	if err != nil {
		return WorkgroupMember{}, fmt.Errorf("store: loading workgroup membership: %w", err)
	}
	return out, nil
}

// IsMember reports whether an address belongs to a workgroup.
func (w *Workgroups) IsMember(ctx context.Context, q Querier, workgroupID int64, address string) (bool, error) {
	var exists bool
	err := q.GetContext(ctx, &exists, `
		SELECT EXISTS (SELECT 1 FROM workgroup_members
		               WHERE workgroup_id = $1 AND address = $2)`,
		workgroupID, address)
	if err != nil {
		return false, fmt.Errorf("store: checking workgroup membership: %w", err)
	}
	return exists, nil
}

// IsAdmin reports whether an address administers a workgroup.
func (w *Workgroups) IsAdmin(ctx context.Context, q Querier, workgroupID int64, address string) (bool, error) {
	var exists bool
	err := q.GetContext(ctx, &exists, `
		SELECT EXISTS (SELECT 1 FROM workgroup_members
		               WHERE workgroup_id = $1 AND address = $2 AND role = $3)`,
		workgroupID, address, WorkgroupRoleAdmin)
	if err != nil {
		return false, fmt.Errorf("store: checking workgroup administration: %w", err)
	}
	return exists, nil
}

// AddMember adds or re-roles an address. Adding somebody twice updates their
// role rather than failing, because promoting the founder of a group is the same
// operation as adding them.
func (w *Workgroups) AddMember(ctx context.Context, q Querier, workgroupID int64, address, role string) error {
	if role == "" {
		role = WorkgroupRoleMember
	}
	if role != WorkgroupRoleMember && role != WorkgroupRoleAdmin {
		return fmt.Errorf("store: unknown workgroup role %q", role)
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO workgroup_members (workgroup_id, address, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (workgroup_id, address) DO UPDATE SET role = EXCLUDED.role`,
		workgroupID, address, role)
	if err != nil {
		if IsConflict(err) {
			return fmt.Errorf("store: adding workgroup member: %w", ErrConflict)
		}
		return fmt.Errorf("store: adding workgroup member: %w", err)
	}
	return nil
}

// RemoveMember takes an address out of a workgroup.
func (w *Workgroups) RemoveMember(ctx context.Context, q Querier, workgroupID int64, address string) (bool, error) {
	res, err := q.ExecContext(ctx,
		`DELETE FROM workgroup_members WHERE workgroup_id = $1 AND address = $2`,
		workgroupID, address)
	if err != nil {
		return false, fmt.Errorf("store: removing workgroup member: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetOpenBooking records whether anyone may book onto the group's calendars
// without being a member.
func (w *Workgroups) SetOpenBooking(ctx context.Context, q Querier, id int64, open bool) error {
	res, err := q.ExecContext(ctx,
		`UPDATE workgroups SET open_booking = $2, updated_at = now() WHERE id = $1`, id, open)
	if err != nil {
		return fmt.Errorf("store: setting open booking: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a workgroup, its calendars and its alias together.
func (w *Workgroups) Delete(ctx context.Context, q Querier, id int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM workgroups WHERE id = $1`, id)
	if err != nil {
		if IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: deleting workgroup: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

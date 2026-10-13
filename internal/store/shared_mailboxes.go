package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
)

// SharedMailbox is the declaration that a principal is a shared mailbox. The
// principal itself lives in users with source 'shared'; this row is what makes
// it a team mailbox that holds mail in its own right, with its own quota and
// members. Deleting the principal (not this row) is what removes it, because
// that is the only path that also takes the mail with it.
type SharedMailbox struct {
	Address            string     `db:"address"`
	DisplayName        string     `db:"display_name"`
	Description        string     `db:"description"`
	Colour             *string    `db:"colour"`
	OpenAccess         bool       `db:"open_access"`
	AutoReplySubject   *string    `db:"auto_reply_subject"`
	AutoReplyBody      *string    `db:"auto_reply_body"`
	AutoReplyExpiresAt *time.Time `db:"auto_reply_expires_at"`
	CreatedAt          time.Time  `db:"created_at"`
	UpdatedAt          time.Time  `db:"updated_at"`
	UpdatedBy          *string    `db:"updated_by"`
}

// CreateSharedMailboxRequest declares an already-provisioned shared principal.
type CreateSharedMailboxRequest struct {
	Address     string
	DisplayName string
	Description string
	Colour      *string
	OpenAccess  bool
	UpdatedBy   *string
}

// UpdateSharedMailboxRequest replaces the editable fields of a shared mailbox.
type UpdateSharedMailboxRequest struct {
	DisplayName        string
	Description        string
	Colour             *string
	OpenAccess         bool
	AutoReplySubject   *string
	AutoReplyBody      *string
	AutoReplyExpiresAt *time.Time
	UpdatedBy          *string
}

// SharedMailboxes reads and writes the shared_mailboxes table.
type SharedMailboxes struct{}

// NewSharedMailboxes returns the repository. It is stateless, so this is free.
func NewSharedMailboxes() *SharedMailboxes { return &SharedMailboxes{} }

// Declare marks an existing shared principal as a shared mailbox. The
// principal must already exist with source 'shared' and a live mailbox; a
// database guard refuses a person, so a team can never be handed a private
// mailbox. A principal that already has a declaration is ErrConflict.
func (s *SharedMailboxes) Declare(ctx context.Context, q Querier, req CreateSharedMailboxRequest) error {
	const query = `
		INSERT INTO shared_mailboxes (address, display_name, description, colour, open_access, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := q.ExecContext(ctx, query,
		req.Address, req.DisplayName, req.Description, req.Colour, req.OpenAccess, req.UpdatedBy); err != nil {
		if IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: declaring shared mailbox: %w", err)
	}
	return nil
}

const sharedSelect = `
	SELECT address, display_name, description, colour, open_access,
	       auto_reply_subject, auto_reply_body, auto_reply_expires_at,
	       created_at, updated_at, updated_by
	FROM shared_mailboxes`

// Get returns a shared mailbox declaration by address, or ErrNotFound.
func (s *SharedMailboxes) Get(ctx context.Context, q Querier, address string) (SharedMailbox, error) {
	var row SharedMailbox
	if err := q.GetContext(ctx, &row, sharedSelect+` WHERE address = $1`, address); err != nil {
		if IsNotFound(err) {
			return SharedMailbox{}, ErrNotFound
		}
		return SharedMailbox{}, err
	}
	return row, nil
}

// List returns every declared shared mailbox, ordered by address.
func (s *SharedMailboxes) List(ctx context.Context, q Querier) ([]SharedMailbox, error) {
	rows, err := q.QueryxContext(ctx, sharedSelect+` ORDER BY address`)
	if err != nil {
		return nil, fmt.Errorf("store: listing shared mailboxes: %w", err)
	}
	defer rows.Close()

	out := make([]SharedMailbox, 0, 8)
	for rows.Next() {
		var row SharedMailbox
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning shared mailbox: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating shared mailboxes: %w", err)
	}
	return out, nil
}

// Update replaces the editable fields of a shared mailbox.
func (s *SharedMailboxes) Update(ctx context.Context, q Querier, address string, req UpdateSharedMailboxRequest) error {
	const query = `
		UPDATE shared_mailboxes SET
			display_name = $2, description = $3, colour = $4, open_access = $5,
			auto_reply_subject = $6, auto_reply_body = $7, auto_reply_expires_at = $8,
			updated_by = $9, updated_at = now()
		WHERE address = $1`
	res, err := q.ExecContext(ctx, query,
		address, req.DisplayName, req.Description, req.Colour, req.OpenAccess,
		req.AutoReplySubject, req.AutoReplyBody, req.AutoReplyExpiresAt, req.UpdatedBy)
	if err != nil {
		return fmt.Errorf("store: updating shared mailbox: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Undeclare removes the shared-mailbox declaration. It does not delete the
// principal or any mail: that happens by deleting the user, which cascades.
// Undeclare is the step that lets a former shared mailbox be relabelled, which
// is otherwise refused while the declaration exists.
func (s *SharedMailboxes) Undeclare(ctx context.Context, q Querier, address string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM shared_mailboxes WHERE address = $1`, address)
	if err != nil {
		return fmt.Errorf("store: undeclaring shared mailbox: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SharedMailboxGrant is one member's access to a shared mailbox.
type SharedMailboxGrant struct {
	SharedAddress     string            `db:"shared_address"`
	MemberAddress     string            `db:"member_address"`
	Level             policy.GrantLevel `db:"level"`
	PersonalAutoReply *string           `db:"personal_auto_reply"`
	GrantedBy         *string           `db:"granted_by"`
	CreatedAt         time.Time         `db:"created_at"`
}

// SharedMailboxGrants reads and writes shared_mailbox_grants.
type SharedMailboxGrants struct{}

// NewSharedMailboxGrants returns the repository. It is stateless.
func NewSharedMailboxGrants() *SharedMailboxGrants { return &SharedMailboxGrants{} }

// Grant gives a member access to a shared mailbox, or raises an existing
// grant to a new level. A member who cannot hold mail (a domain admin) is
// refused by a database guard, because administrators read mail through
// audited impersonation instead.
func (g *SharedMailboxGrants) Grant(ctx context.Context, q Querier, shared, member string, level policy.GrantLevel, grantedBy *string) error {
	const query = `
		INSERT INTO shared_mailbox_grants (shared_address, member_address, level, granted_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (shared_address, member_address)
		DO UPDATE SET level = EXCLUDED.level, granted_by = EXCLUDED.granted_by`
	if _, err := q.ExecContext(ctx, query, shared, member, level, grantedBy); err != nil {
		// The database is the last line of defence here, and its rules are not
		// all "already exists": a guard trigger refuses a member who cannot
		// hold mail, and a foreign key refuses a mailbox that was never
		// declared. Reporting either as a conflict would tell the administrator
		// something exists when the opposite is true, so each code keeps its
		// own sentinel.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23514": // check_violation
				if pgErr.ConstraintName == "shared_grant_not_self" {
					return fmt.Errorf("%w: a shared mailbox cannot be a member of itself", ErrConflict)
				}
				return fmt.Errorf("%w: %s cannot hold mail", ErrNoMailbox, member)
			case "23503": // foreign_key_violation
				return fmt.Errorf("%w: %s is not a declared shared mailbox, or %s is not a user",
					ErrNotFound, shared, member)
			case "23505": // unique_violation
				return ErrConflict
			}
		}
		if IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: granting shared mailbox access: %w", err)
	}
	return nil
}

// Revoke removes a member's access.
func (g *SharedMailboxGrants) Revoke(ctx context.Context, q Querier, shared, member string) error {
	res, err := q.ExecContext(ctx,
		`DELETE FROM shared_mailbox_grants WHERE shared_address = $1 AND member_address = $2`,
		shared, member)
	if err != nil {
		return fmt.Errorf("store: revoking shared mailbox access: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const grantSelect = `
	SELECT shared_address, member_address, level, personal_auto_reply,
	       granted_by, created_at
	FROM shared_mailbox_grants`

// ListForShared returns every grant on one shared mailbox.
func (g *SharedMailboxGrants) ListForShared(ctx context.Context, q Querier, shared string) ([]SharedMailboxGrant, error) {
	rows, err := q.QueryxContext(ctx, grantSelect+` WHERE shared_address = $1 ORDER BY member_address`, shared)
	if err != nil {
		return nil, fmt.Errorf("store: listing grants: %w", err)
	}
	defer rows.Close()
	return scanGrants(rows)
}

// ListForMember returns every shared mailbox a member can open.
func (g *SharedMailboxGrants) ListForMember(ctx context.Context, q Querier, member string) ([]SharedMailboxGrant, error) {
	rows, err := q.QueryxContext(ctx, grantSelect+` WHERE member_address = $1 ORDER BY shared_address`, member)
	if err != nil {
		return nil, fmt.Errorf("store: listing member grants: %w", err)
	}
	defer rows.Close()
	return scanGrants(rows)
}

func scanGrants(rows *sqlx.Rows) ([]SharedMailboxGrant, error) {
	out := make([]SharedMailboxGrant, 0, 8)
	for rows.Next() {
		var row SharedMailboxGrant
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning grant: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating grants: %w", err)
	}
	return out, nil
}

// Access is the authorization lookup every mail protocol uses: it returns the
// level a member holds on a shared mailbox, together with whether the mailbox
// is open to the whole domain. A missing grant is not an error; it is an empty
// level, and the policy package decides what that allows.
func (g *SharedMailboxGrants) Access(ctx context.Context, q Querier, shared, member string) (policy.SharedAccess, error) {
	var open bool
	if err := q.GetContext(ctx, &open,
		`SELECT open_access FROM shared_mailboxes WHERE address = $1`, shared); err != nil {
		if IsNotFound(err) {
			return policy.SharedAccess{}, ErrNotFound
		}
		return policy.SharedAccess{}, err
	}

	var level sql.NullString
	if err := q.GetContext(ctx, &level,
		`SELECT level FROM shared_mailbox_grants WHERE shared_address = $1 AND member_address = $2`,
		shared, member); err != nil {
		if !IsNotFound(err) {
			return policy.SharedAccess{}, err
		}
		level = sql.NullString{}
	}

	access := policy.SharedAccess{Open: open}
	if level.Valid {
		access.Level = policy.GrantLevel(level.String)
	}
	return access, nil
}

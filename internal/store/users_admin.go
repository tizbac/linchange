package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/auth/policy"
)

// User is an admin's view of a principal row: enough of it to render a list,
// edit a settings page, and reason about whether the account may hold mail.
type User struct {
	Address     string        `db:"address"`
	Source      policy.Source `db:"source"`
	DisplayName string        `db:"display_name"`
	Enabled     bool          `db:"enabled"`
	// MailboxEnabled is false for a domain admin; the database enforces that
	// pairing, this field carries it.
	MailboxEnabled bool `db:"mailbox_enabled"`
	// IsDomainAdmin is the administration grant. It is independent of source,
	// so it can be true for a local or LDAP user who also receives mail; a
	// domain_admin row always has it.
	IsDomainAdmin bool `db:"is_domain_admin"`
	// QuotaBytes is nil when the user inherits the domain (and then global)
	// default. Zero, when explicit, means unlimited.
	QuotaBytes     *int64 `db:"quota_bytes"`
	DailySendLimit *int   `db:"daily_send_limit"`
	Timezone       string `db:"timezone"`
	Language       string `db:"language"`
	// Theme is the user's explicit presentation choice, empty when they have
	// not chosen and the organization or deployment default applies.
	Theme string `db:"theme"`
	// RecoveryEmail is an address the account can be reached at when the
	// mailbox itself is unavailable. Empty when none is set.
	RecoveryEmail      string `db:"recovery_email"`
	MustChangePassword bool   `db:"must_change_password"`
	// AddressLocal is the lowercased local part, the part before the "@". It is
	// what the "#sharedmailbox" login convention splits on.
	AddressLocal string `db:"address_local"`
	// ContactGroupID is the contact group this user belongs to (derived from domain).
	ContactGroupID *int64     `db:"contact_group_id"`
	LastLoginAt    *time.Time `db:"last_login_at"`
	CreatedAt      time.Time  `db:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at"`
}

// CreateUserRequest provisions a principal. Source is fixed at creation: it is
// what decides where a password lives, and changing it later would strand the
// credential. A nil Enabled/MailboxEnabled falls back to the sensible default;
// a domain admin always comes out with MailboxEnabled false, whether asked or
// not, because that pairing is a rule and not a preference.
type CreateUserRequest struct {
	Address     string
	Source      policy.Source
	DisplayName string
	Enabled     *bool
	// MailboxEnabled is forced false for the domain_admin source, whatever the
	// caller asked for.
	MailboxEnabled *bool
	// IsDomainAdmin grants administration. It is forced true for the
	// domain_admin source, so that source and grant cannot be created in
	// opposition to each other.
	IsDomainAdmin      bool
	QuotaBytes         *int64
	DailySendLimit     *int
	Timezone           string
	Language           string
	MustChangePassword bool
}

// UpdateUserRequest replaces the mutable fields of a user. The caller supplies
// the whole row, as a settings page already has it.
type UpdateUserRequest struct {
	DisplayName    string
	Enabled        bool
	MailboxEnabled bool
	// IsDomainAdmin is the administration grant, forced true for the
	// domain_admin source on the same grounds as Create.
	IsDomainAdmin      bool
	QuotaBytes         *int64
	DailySendLimit     *int
	Timezone           string
	Language           string
	MustChangePassword bool
}

// Users reads and writes the users table for administration. It is deliberately
// separate from LoadPrincipal: that answers "may this identity act", while these
// answer "what is the administrator changing".
type Users struct{}

// NewUsers returns the repository. It is stateless, so this is free.
func NewUsers() *Users { return &Users{} }

// addressLocal splits an address into its lowercased local part. An address
// without a domain (which should not happen, but must not panic) is returned
// whole.
func addressLocal(address string) string {
	if i := strings.IndexByte(address, '@'); i >= 0 {
		address = address[:i]
	}
	return strings.ToLower(address)
}

func (u *Users) Create(ctx context.Context, q Querier, req CreateUserRequest) error {
	if req.Address == "" {
		return errors.New("store: user address is empty")
	}
	source := req.Source
	if !source.Valid() {
		return fmt.Errorf("store: unknown user source %q", source)
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	mailbox := source != policy.SourceDomainAdmin
	if req.MailboxEnabled != nil {
		mailbox = *req.MailboxEnabled
	}
	isDomainAdmin := req.IsDomainAdmin
	if source == policy.SourceDomainAdmin {
		// A domain admin administers mail and never receives it. This is
		// enforced by a CHECK constraint too, but refusing it here means the
		// caller gets a clean answer instead of a check violation.
		mailbox = false
		// The source carries the administration grant with it: a domain_admin
		// row that was not an administrator is the one state that cannot exist,
		// so it is not left to the caller to ask for.
		isDomainAdmin = true
	}
	if source == policy.SourceShared {
		// A shared mailbox is reached through grants and never authenticates,
		// so there is nothing for an administration grant to be spent on. The
		// schema refuses the pairing; this is the clean answer to it.
		isDomainAdmin = false
	}
	tz := req.Timezone
	if tz == "" {
		tz = "UTC"
	}
	lang := req.Language
	if lang == "" {
		lang = "en"
	}

	const query = `
		INSERT INTO users (address, source, display_name, enabled, mailbox_enabled,
		                   is_domain_admin, quota_bytes, daily_send_limit,
		                   timezone, language, must_change_password, address_local)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
	if _, err := q.ExecContext(ctx, query,
		req.Address, source, req.DisplayName, enabled, mailbox, isDomainAdmin,
		req.QuotaBytes, req.DailySendLimit, tz, lang,
		req.MustChangePassword, addressLocal(req.Address)); err != nil {
		if IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: creating user: %w", err)
	}
	return nil
}

const userSelect = `
	SELECT u.address, u.source, u.display_name, u.enabled, u.mailbox_enabled,
	       u.is_domain_admin, COALESCE(u.quota_bytes, d.default_quota_bytes) AS quota_bytes,
	       u.daily_send_limit, u.timezone, u.language, COALESCE(u.theme, '') AS theme,
	       COALESCE(u.recovery_email, '') AS recovery_email,
	       u.must_change_password, u.address_local, u.last_login_at,
	       u.created_at, u.updated_at
	FROM users u
	LEFT JOIN domains d ON d.name = split_part(u.address, '@', 2) AND d.status = 'active'`

// Get returns a user by address, or ErrNotFound. A disabled account is still
// returned: an administrator needs to see it in order to re-enable it. A
// deleted one is not; like List, Get treats a soft-deleted principal as gone,
// which is what keeps the admin view honest.
func (u *Users) Get(ctx context.Context, q Querier, address string) (User, error) {
	var row User
	if err := q.GetContext(ctx, &row, userSelect+` WHERE u.address = $1 AND u.deleted_at IS NULL`, address); err != nil {
		if IsNotFound(err) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	return row, nil
}

// SetTheme records a user's presentation choice. An empty value clears it so
// the organization or deployment default applies again.
func (u *Users) SetTheme(ctx context.Context, q Querier, address, theme string) error {
	res, err := q.ExecContext(ctx, `
		UPDATE users SET theme = NULLIF($2, ''), updated_at = now()
		WHERE address = $1 AND deleted_at IS NULL`, address, strings.TrimSpace(theme))
	if err != nil {
		return fmt.Errorf("store: setting user theme: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetRecoveryEmail records an address the account can be reached at when the
// mailbox is unavailable. An empty value clears it.
func (u *Users) SetRecoveryEmail(ctx context.Context, q Querier, address, email string) error {
	res, err := q.ExecContext(ctx, `
		UPDATE users SET recovery_email = NULLIF($2, ''), updated_at = now()
		WHERE address = $1 AND deleted_at IS NULL`, address, strings.TrimSpace(email))
	if err != nil {
		return fmt.Errorf("store: setting recovery email: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns every non-deleted user, ordered by address.
func (u *Users) List(ctx context.Context, q Querier) ([]User, error) {
	rows, err := q.QueryxContext(ctx, userSelect+` WHERE u.deleted_at IS NULL ORDER BY u.address`)
	if err != nil {
		return nil, fmt.Errorf("store: listing users: %w", err)
	}
	defer rows.Close()

	out := make([]User, 0, 32)
	for rows.Next() {
		var row User
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning user: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating users: %w", err)
	}
	return out, nil
}

// Update replaces the mutable fields of a user.
func (u *Users) Update(ctx context.Context, q Querier, address string, req UpdateUserRequest) error {
	mailbox := req.MailboxEnabled
	isDomainAdmin := req.IsDomainAdmin
	if source, err := sourceOf(ctx, q, address); err == nil {
		switch source {
		case policy.SourceDomainAdmin:
			mailbox = false
			isDomainAdmin = true
		case policy.SourceShared:
			isDomainAdmin = false
		}
	}
	tz := req.Timezone
	if tz == "" {
		tz = "UTC"
	}
	lang := req.Language
	if lang == "" {
		lang = "en"
	}

	const query = `
		UPDATE users SET
			display_name = $2, enabled = $3, mailbox_enabled = $4,
			is_domain_admin = $5, quota_bytes = $6, daily_send_limit = $7,
			timezone = $8, language = $9, must_change_password = $10,
			updated_at = now()
		WHERE address = $1 AND deleted_at IS NULL`
	res, err := q.ExecContext(ctx, query,
		address, req.DisplayName, req.Enabled, mailbox, isDomainAdmin,
		req.QuotaBytes, req.DailySendLimit, tz, lang, req.MustChangePassword)
	if err != nil {
		return fmt.Errorf("store: updating user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetEnabled enables or disables a principal. A disabled account
// authenticates nowhere and receives nothing, but the row and its mail remain.
func (u *Users) SetEnabled(ctx context.Context, q Querier, address string, enabled bool) error {
	res, err := q.ExecContext(ctx,
		`UPDATE users SET enabled = $2, updated_at = now()
		 WHERE address = $1 AND deleted_at IS NULL`,
		address, enabled)
	if err != nil {
		return fmt.Errorf("store: setting user enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetQuota sets an explicit quota for a user. A nil quota clears the override
// so the user inherits from the domain; the domain and then the global default
// supply the value.
func (u *Users) SetQuota(ctx context.Context, q Querier, address string, quota *int64) error {
	res, err := q.ExecContext(ctx,
		`UPDATE users SET quota_bytes = $2, updated_at = now()
		 WHERE address = $1 AND deleted_at IS NULL`,
		address, quota)
	if err != nil {
		return fmt.Errorf("store: setting user quota: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDailySendLimit changes a user's daily outbound message limit. Nil means
// unlimited, which is what an emptied field in the edit screen asks for.
func (u *Users) SetDailySendLimit(ctx context.Context, q Querier, address string, limit *int) error {
	res, err := q.ExecContext(ctx,
		`UPDATE users SET daily_send_limit = $2, updated_at = now()
		 WHERE address = $1 AND deleted_at IS NULL`,
		address, limit)
	if err != nil {
		return fmt.Errorf("store: setting daily send limit: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a principal by soft-deleting it. The row, its mail and its
// folders all remain: a hard delete would cascade through mailboxes to the
// messages they hold, which is not what "remove this account" means. Nothing
// may authenticate as the address while it is deleted, because the login and
// list paths all exclude deleted_at IS NULL.
func (u *Users) Delete(ctx context.Context, q Querier, address string) error {
	res, err := q.ExecContext(ctx,
		`UPDATE users SET deleted_at = now(), updated_at = now()
		 WHERE address = $1 AND deleted_at IS NULL`,
		address)
	if err != nil {
		return fmt.Errorf("store: deleting user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// sourceOf reads only the source of a user, for the domain-admin rule in
// Update. It is a small helper rather than a Get so an update does not pay for
// the whole row it is about to overwrite.
func sourceOf(ctx context.Context, q Querier, address string) (policy.Source, error) {
	var s policy.Source
	err := q.GetContext(ctx, &s,
		`SELECT source FROM users WHERE address = $1 AND deleted_at IS NULL`, address)
	if err != nil {
		return "", err
	}
	return s, nil
}

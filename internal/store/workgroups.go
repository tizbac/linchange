package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
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
	ID          int64  `db:"id"`
	Name        string `db:"name"`
	Domain      string `db:"domain"`
	DisplayName string `db:"display_name"`
	Description string `db:"description"`
	OpenBooking bool   `db:"open_booking"`
	// LogoUID is the public handle of the organization's logo image, empty
	// when it has none. LogoSHA256 and LogoMIME describe the blob it names.
	LogoUID    string    `db:"logo_uid"`
	LogoSHA256 []byte    `db:"logo_sha256"`
	LogoMIME   string    `db:"logo_mime"`
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
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

// Contact is one address belonging to an organization, with the display name its
// mailbox carries. It is what the address list and the composer's suggestions
// show.
type Contact struct {
	Address     string `db:"address"`
	DisplayName string `db:"display_name"`
}

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
	COALESCE(logo_uid, '') AS logo_uid,
	COALESCE(logo_mime, '') AS logo_mime,
	logo_sha256, created_at, updated_at`

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

// ForDomain returns the organization that owns a domain.
//
// A group owns a domain in one of two ways: it is the group's home domain, the
// one its own address lives in, or the domain row names the group. Both count,
// because a deployment that predates domains.workgroup_id only has the first.
// ErrNotFound means the domain belongs to no organization.
func (w *Workgroups) ForDomain(ctx context.Context, q Querier, domain string) (Workgroup, error) {
	if domain == "" {
		return Workgroup{}, ErrNotFound
	}
	var out Workgroup
	err := q.GetContext(ctx, &out, `
		SELECT `+workgroupColumns+`
		FROM workgroups w
		WHERE w.domain = $1
		   OR EXISTS (SELECT 1 FROM domains d
                      WHERE d.name = $1 AND d.workgroup_id = w.id)
		ORDER BY w.domain, w.name
		LIMIT 1`, domain)
	if IsNotFound(err) {
		return Workgroup{}, ErrNotFound
	}
	if err != nil {
		return Workgroup{}, fmt.Errorf("store: loading workgroup for domain: %w", err)
	}
	return out, nil
}

// ForAddress is ForDomain applied to the domain part of an address. Every
// caller that has an address and wants the organization behind it uses this,
// rather than splitting the string itself.
func (w *Workgroups) ForAddress(ctx context.Context, q Querier, address string) (Workgroup, error) {
	_, domain, found := strings.Cut(strings.ToLower(strings.TrimSpace(address)), "@")
	if !found {
		return Workgroup{}, ErrNotFound
	}
	return w.ForDomain(ctx, q, domain)
}

// ByLogoUID resolves the public handle used in a logo URL. It is deliberately
// separate from Get: the browser holding an image URL knows the handle and
// nothing else.
func (w *Workgroups) ByLogoUID(ctx context.Context, q Querier, uid string) (Workgroup, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return Workgroup{}, ErrNotFound
	}
	var out Workgroup
	err := q.GetContext(ctx, &out,
		`SELECT `+workgroupColumns+` FROM workgroups WHERE logo_uid = $1`, uid)
	if IsNotFound(err) {
		return Workgroup{}, ErrNotFound
	}
	if err != nil {
		return Workgroup{}, fmt.Errorf("store: loading workgroup logo: %w", err)
	}
	return out, nil
}

// SetLogo records the blob that is an organization's logo. The bytes are
// already in the blob store; this only updates the reference.
func (w *Workgroups) SetLogo(ctx context.Context, q Querier, id int64, uid, mime string, sha []byte) error {
	res, err := q.ExecContext(ctx, `
		UPDATE workgroups
		SET logo_uid = $2, logo_mime = $3, logo_sha256 = $4, updated_at = now()
		WHERE id = $1`, id, uid, mime, sha)
	if err != nil {
		return fmt.Errorf("store: setting workgroup logo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearLogo removes the reference; the blob itself is left for the store's
// collector, as every other unreferenced blob is.
func (w *Workgroups) ClearLogo(ctx context.Context, q Querier, id int64) error {
	res, err := q.ExecContext(ctx, `
		UPDATE workgroups
		SET logo_uid = NULL, logo_mime = NULL, logo_sha256 = NULL, updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: clearing workgroup logo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// LogoURL is the public path of the organization's logo, or empty when it has
// none. The digest prefix is a cache key: a new image changes the URL, so a
// browser that cached the old one fetches the new one.
func (w Workgroup) LogoURL() string {
	if w.LogoUID == "" || len(w.LogoSHA256) == 0 {
		return ""
	}
	version := hex.EncodeToString(w.LogoSHA256)
	if len(version) > 12 {
		version = version[:12]
	}
	return "/api/v1/branding/logo?org=" + url.QueryEscape(w.LogoUID) + "&v=" + version
}

// Domains returns the domains an organization owns: its home domain first, then
// any others attached to it.
func (w *Workgroups) Domains(ctx context.Context, q Querier, workgroupID int64) ([]string, error) {
	wg, err := w.Get(ctx, q, workgroupID)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryxContext(ctx, `
		SELECT d.name
		FROM domains d
		WHERE d.workgroup_id = $1 AND d.name <> $2
		ORDER BY d.name`, workgroupID, wg.Domain)
	if err != nil {
		return nil, fmt.Errorf("store: listing workgroup domains: %w", err)
	}
	defer rows.Close()

	out := []string{wg.Domain}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scanning workgroup domain: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating workgroup domains: %w", err)
	}
	return out, nil
}

// People returns the enabled mailboxes in the domains an organization owns.
//
// Membership in the organization is not the filter here: everyone with an
// address in one of its domains is a contact, which is what makes an
// organization-wide address list useful in the first place.
func (w *Workgroups) People(ctx context.Context, q Querier, workgroupID int64) ([]Contact, error) {
	domains, err := w.Domains(ctx, q, workgroupID)
	if err != nil {
		return nil, err
	}
	return w.PeopleInDomains(ctx, q, domains)
}

// PeopleInDomains returns the enabled mailboxes whose addresses sit in one of
// the given domains.
func (w *Workgroups) PeopleInDomains(ctx context.Context, q Querier, domains []string) ([]Contact, error) {
	if len(domains) == 0 {
		return nil, nil
	}
	rows, err := q.QueryxContext(ctx, `
		SELECT u.address, u.display_name
		FROM users u
		WHERE u.enabled AND lower(split_part(u.address, '@', 2)) = ANY($1)
		ORDER BY COALESCE(NULLIF(u.display_name, ''), u.address)`, domains)
	if err != nil {
		return nil, fmt.Errorf("store: listing organization contacts: %w", err)
	}
	defer rows.Close()

	out := make([]Contact, 0, 16)
	for rows.Next() {
		var c Contact
		if err := rows.StructScan(&c); err != nil {
			return nil, fmt.Errorf("store: scanning organization contact: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating organization contacts: %w", err)
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

// UpdateWorkgroupRequest carries the mutable fields of a workgroup.
type UpdateWorkgroupRequest struct {
	Name        string
	DisplayName string
	Description string
	OpenBooking *bool
}

// Update updates a workgroup's mutable fields.
func (w *Workgroups) Update(ctx context.Context, q Querier, id int64, req UpdateWorkgroupRequest) (Workgroup, error) {
	var out Workgroup
	err := q.GetContext(ctx, &out, `
		UPDATE workgroups SET
			name = $2, display_name = $3, description = $4,
			open_booking = $5, updated_at = now()
		WHERE id = $1
		RETURNING id, name, domain, display_name, description, open_booking, created_at, updated_at`,
		id, req.Name, req.DisplayName, req.Description, req.OpenBooking)
	if err != nil {
		if IsNotFound(err) {
			return Workgroup{}, ErrNotFound
		}
		return Workgroup{}, fmt.Errorf("store: updating workgroup: %w", err)
	}
	return out, nil
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

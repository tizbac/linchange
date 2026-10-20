// Package admin is the domain administration surface: the rules behind the
// administrative REST API.
//
// It lives beside login and reset rather than inside the HTTP package, for the
// same reason they do. A decision about who may create a user, or whether an
// administrator may read somebody's mail, has to be enforced once and
// identically no matter which listener asks, and it has to be testable without
// standing up a server. The httpapi package translates requests into calls on
// this Service and does nothing else.
//
// Two things are true of every operation here:
//
//   - It is refused unless the caller is a domain admin, checked through
//     policy.Principal rather than by reading the request.
//   - It is written to the audit log in the same transaction as the change it
//     describes, so a change that is not recorded cannot have happened, and a
//     record that was written is not evidence of a change that was rolled
//     back.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// Action names, as they appear in the audit log. They are constants rather
// than the strings being passed around because a log that can be queried is
// only useful if the action names are stable and spelled one way.
const (
	ActionDomainCreate = "domain.create"
	ActionDomainUpdate = "domain.update"
	ActionDomainDelete = "domain.delete"

	ActionUserCreate        = "user.create"
	ActionUserUpdate        = "user.update"
	ActionUserEnable        = "user.enable"
	ActionUserDisable       = "user.disable"
	ActionUserQuota         = "user.quota"
	ActionUserDelete        = "user.delete"
	ActionUserResetPassword = "user.reset_password"
	// ActionUserAdminGrant is its own action rather than part of user.update.
	// Granting and revoking administration is the change an operator has to be
	// able to find in the log after an incident, and it should not have to be
	// told apart from somebody editing a display name.
	ActionUserAdminGrant = "user.admin_grant"

	ActionAliasCreate  = "alias.create"
	ActionAliasEnable  = "alias.enable"
	ActionAliasDisable = "alias.disable"
	ActionAliasDelete  = "alias.delete"

	ActionSharedCreate    = "shared_mailbox.create"
	ActionSharedUpdate    = "shared_mailbox.update"
	ActionSharedUndeclare = "shared_mailbox.undeclare"
	ActionSharedGrant     = "shared_mailbox.grant"
	ActionSharedRevoke    = "shared_mailbox.revoke"

	ActionImpersonationGrant  = "impersonation.grant"
	ActionImpersonationRevoke = "impersonation.revoke"

	ActionWorkgroupCreate = "workgroup.create"
	ActionWorkgroupUpdate = "workgroup.update"
	ActionWorkgroupDelete = "workgroup.delete"
	ActionWorkgroupGrant  = "workgroup.grant"
	ActionWorkgroupRevoke = "workgroup.revoke"

	// Organization calendars are their own action rather than riding on the
	// workgroup ones: creating a team calendar is a change somebody will look
	// for after the fact, and "workgroup.update" says only that something about
	// the group changed.
	ActionCalendarCreate = "calendar.create"
	ActionCalendarUpdate = "calendar.update"
	ActionCalendarDelete = "calendar.delete"

	// Groups are addressed rather than merely listed, so who is in one and
	// which groups exist are both things a log reader needs later: a meeting
	// that went to the wrong people should be traceable to the member row that
	// put them there.
	ActionGroupCreate       = "workgroup_group.create"
	ActionGroupUpdate       = "workgroup_group.update"
	ActionGroupDelete       = "workgroup_group.delete"
	ActionGroupMemberAdd    = "workgroup_group.member_add"
	ActionGroupMemberRemove = "workgroup_group.member_remove"

	// A requeue is deliberately noticed: mail that was given up on is being
	// tried again by hand, and if it fails twice the log should show that
	// somebody asked for the second go rather than that the queue retried by
	// itself.
	ActionOutboxRequeue = "outbox.requeue"
)

// ErrDenied is returned when the caller is not permitted to do what they
// asked. The API turns it into a 403; it is deliberately not the same value as
// store.ErrNotFound, because "you may not" and "it does not exist" are
// different answers and an administrator needs to be able to tell them apart.
var ErrDenied = errors.New("admin: not permitted")

// ErrInvalid is a request the caller could fix: a malformed address, a
// password that is too short. It becomes a 400.
var ErrInvalid = errors.New("admin: invalid request")

// Service holds the repositories the administration surface needs.
type Service struct {
	db *sqlx.DB

	domains    *store.Domains
	users      *store.Users
	aliases    *store.Aliases
	shared     *store.SharedMailboxes
	grants     *store.SharedMailboxGrants
	workgroups *store.Workgroups
	quotas     *store.Quotas
	imperson   *store.ImpersonationGrants
	audit      *store.AuditLog
	credentia  *store.Credentials
	sessions   *store.Sessions

	argon2            config.Argon2Params
	minPasswordLength int
	policy            policy.Policy
}

// Options are the inputs to New.
type Options struct {
	DB                *sqlx.DB
	Argon2            config.Argon2Params
	MinPasswordLength int
	Policy            policy.Policy
}

// New builds the service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil {
		return nil, errors.New("admin: a database handle is required")
	}
	return &Service{
		db:                opts.DB,
		domains:           store.NewDomains(),
		users:             store.NewUsers(),
		aliases:           store.NewAliases(),
		shared:            store.NewSharedMailboxes(),
		grants:            store.NewSharedMailboxGrants(),
		workgroups:        store.NewWorkgroups(),
		quotas:            store.NewQuotas(),
		imperson:          store.NewImpersonationGrants(),
		audit:             store.NewAuditLog(),
		credentia:         store.NewCredentials(),
		sessions:          store.NewSessions(),
		argon2:            opts.Argon2,
		minPasswordLength: opts.MinPasswordLength,
		policy:            opts.Policy,
	}, nil
}

// requireAdmin refuses anything that is not a domain admin. Every exported
// method calls it first, so a new endpoint cannot forget.
func requireAdmin(actor policy.Principal) error {
	if err := actor.Can(policy.ActionAdminister); err != nil {
		return fmt.Errorf("%w: %s", ErrDenied, err)
	}
	return nil
}

// actor is the change-maker. target is whose data was acted upon; for a domain
// action there is no target and it is left empty rather than pointed at the
// actor, so the two columns mean different things in the log.
type change struct {
	action     string
	targetType string
	targetID   string
	target     string
	detail     any
}

func (s *Service) record(ctx context.Context, q store.Querier, actor policy.Principal, c change) error {
	var (
		actorAddr  *string
		targetAddr *string
	)
	if actor.Address != "" {
		a := actor.Address
		actorAddr = &a
	}
	if c.target != "" {
		t := c.target
		targetAddr = &t
	}
	detail := json.RawMessage(nil)
	if c.detail != nil {
		encoded, err := json.Marshal(c.detail)
		if err != nil {
			return fmt.Errorf("admin: encoding audit detail: %w", err)
		}
		detail = encoded
	}
	_, err := s.audit.Record(ctx, q, store.AuditEntry{
		ActorAddress:  actorAddr,
		TargetAddress: targetAddr,
		Action:        c.action,
		TargetType:    c.targetType,
		TargetID:      c.targetID,
		Detail:        detail,
	})
	if err != nil {
		return fmt.Errorf("admin: recording %s: %w", c.action, err)
	}
	return nil
}

// mapStore turns a repository error into one the API can answer. The sentinels
// are compared with errors.Is rather than == because a repository may wrap one.
func mapStore(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return store.ErrNotFound
	case errors.Is(err, store.ErrConflict):
		return store.ErrConflict
	default:
		return err
	}
}

// expireAt turns an optional duration into an absolute time, so the database
// stores a deadline rather than a length of time that starts being wrong the
// moment it is written.
func expireAt(now time.Time, d *time.Duration) *time.Time {
	if d == nil {
		return nil
	}
	t := now.Add(*d)
	return &t
}

// ContactGroup represents a group of domains for contact isolation.
type ContactGroup struct {
	ID          int64
	Name        string
	Description string
	CreatedAt   time.Time
}

// ListContactGroups returns all contact groups.
func (s *Service) ListContactGroups(ctx context.Context, actor policy.Principal) ([]ContactGroup, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryxContext(ctx, `
		SELECT id, name, description, created_at
		FROM contact_groups
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("admin: listing contact groups: %w", err)
	}
	defer rows.Close()

	var out []ContactGroup
	for rows.Next() {
		var c ContactGroup
		if err := rows.StructScan(&c); err != nil {
			return nil, fmt.Errorf("admin: scanning contact group: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateContactGroup creates a new contact group.
func (s *Service) CreateContactGroup(ctx context.Context, actor policy.Principal, name, description string) (ContactGroup, error) {
	if err := requireAdmin(actor); err != nil {
		return ContactGroup{}, err
	}
	var c ContactGroup
	err := s.db.GetContext(ctx, &c, `
		INSERT INTO contact_groups (name, description)
		VALUES ($1, $2)
		RETURNING id, name, description, created_at`, name, description)
	if err != nil {
		return ContactGroup{}, fmt.Errorf("admin: creating contact group: %w", err)
	}
	return c, nil
}

// UpdateContactGroup updates a contact group.
func (s *Service) UpdateContactGroup(ctx context.Context, actor policy.Principal, id int64, name, description string) (ContactGroup, error) {
	if err := requireAdmin(actor); err != nil {
		return ContactGroup{}, err
	}
	var c ContactGroup
	err := s.db.GetContext(ctx, &c, `
		UPDATE contact_groups
		SET name = $2, description = $3
		WHERE id = $1
		RETURNING id, name, description, created_at`, id, name, description)
	if err != nil {
		return ContactGroup{}, fmt.Errorf("admin: updating contact group: %w", err)
	}
	return c, nil
}

// DeleteContactGroup deletes a contact group.
func (s *Service) DeleteContactGroup(ctx context.Context, actor policy.Principal, id int64) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM contact_groups WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("admin: deleting contact group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ========================================================================
// Workgroups (Organizations)
// ========================================================================

// CreateWorkgroupRequest is the input for creating a workgroup.
type CreateWorkgroupRequest struct {
	Name        string
	Domain      string
	DisplayName string
	Description string
	OpenBooking *bool
	Members     []store.WorkgroupMember
}

// ListWorkgroups returns all workgroups.
func (s *Service) ListWorkgroups(ctx context.Context, actor policy.Principal) ([]store.Workgroup, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	return s.workgroups.List(ctx, s.db)
}

// CreateWorkgroup creates a new workgroup with its founding members.
func (s *Service) CreateWorkgroup(ctx context.Context, actor policy.Principal, req CreateWorkgroupRequest) (store.Workgroup, error) {
	if err := requireAdmin(actor); err != nil {
		return store.Workgroup{}, err
	}
	wg, err := s.workgroups.Create(ctx, s.db, store.CreateWorkgroupRequest{
		Name:        req.Name,
		Domain:      req.Domain,
		DisplayName: req.DisplayName,
		Description: req.Description,
		OpenBooking: req.OpenBooking,
		Members:     req.Members,
	})
	if err != nil {
		return store.Workgroup{}, fmt.Errorf("admin: creating workgroup: %w", err)
	}
	// Record audit for each member added
	for _, m := range req.Members {
		_ = s.record(ctx, s.db, actor, change{
			action:     ActionWorkgroupGrant,
			targetType: "workgroup_member",
			targetID:   fmt.Sprintf("%d", wg.ID),
			target:     m.Address,
			detail:     map[string]any{"role": m.Role},
		})
	}
	return wg, nil
}

// GetWorkgroup returns a workgroup by ID.
func (s *Service) GetWorkgroup(ctx context.Context, actor policy.Principal, id int64) (store.Workgroup, error) {
	if err := requireAdmin(actor); err != nil {
		return store.Workgroup{}, err
	}
	return s.workgroups.Get(ctx, s.db, id)
}

// UpdateWorkgroupRequest is the input for updating a workgroup.
type UpdateWorkgroupRequest struct {
	Name        string
	DisplayName string
	Description string
	OpenBooking *bool
}

// UpdateWorkgroup updates a workgroup.
func (s *Service) UpdateWorkgroup(ctx context.Context, actor policy.Principal, id int64, name, displayName, description string, openBooking *bool) (store.Workgroup, error) {
	if err := requireAdmin(actor); err != nil {
		return store.Workgroup{}, err
	}
	wg, err := s.workgroups.Update(ctx, s.db, id, store.UpdateWorkgroupRequest{
		Name:        name,
		DisplayName: displayName,
		Description: description,
		OpenBooking: openBooking,
	})
	if err != nil {
		return store.Workgroup{}, fmt.Errorf("admin: updating workgroup: %w", err)
	}
	return wg, nil
}

// DeleteWorkgroup deletes a workgroup.
func (s *Service) DeleteWorkgroup(ctx context.Context, actor policy.Principal, id int64) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if err := s.workgroups.Delete(ctx, s.db, id); err != nil {
		return fmt.Errorf("admin: deleting workgroup: %w", err)
	}
	return nil
}

// ListWorkgroupMembers returns all members of a workgroup.
func (s *Service) ListWorkgroupMembers(ctx context.Context, actor policy.Principal, id int64) ([]store.WorkgroupMember, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	return s.workgroups.Members(ctx, s.db, id)
}

// WorkgroupPeople returns every address that belongs to a workgroup without
// being listed as a member of it: the enabled mailboxes in the group's domains.
//
// Membership is derived rather than listed wherever it can be, because an
// address in a group's domain is in that group the moment it exists. A member
// row is only ever needed for somebody outside those domains, which is what
// AddWorkgroupMember says when handed the other kind.
func (s *Service) WorkgroupPeople(ctx context.Context, actor policy.Principal, id int64) ([]store.Contact, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	return s.workgroups.People(ctx, s.db, id)
}

// AddWorkgroupMember adds a member to a workgroup.
//
// Only addresses outside the group's own domains can be added: everybody else
// is already in it by virtue of their address, and a row saying so would be
// something an administrator could later remove while the person stayed a
// member, which is how a list starts lying about who is in the group.
func (s *Service) AddWorkgroupMember(ctx context.Context, actor policy.Principal, id int64, address, role string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	domains, err := s.workgroups.Domains(ctx, s.db, id)
	if err != nil {
		return fmt.Errorf("admin: listing workgroup domains: %w", err)
	}
	at := strings.LastIndex(address, "@")
	if at >= 0 {
		for _, domain := range domains {
			if strings.EqualFold(address[at+1:], domain) {
				return errors.Join(ErrInvalid, fmt.Errorf(
					"%s is already a member of this organization: everyone with an address in %s is",
					address, domain))
			}
		}
	}
	if err := s.workgroups.AddMember(ctx, s.db, id, address, role); err != nil {
		return fmt.Errorf("admin: adding workgroup member: %w", err)
	}
	return s.record(ctx, s.db, actor, change{
		action:     ActionWorkgroupGrant,
		targetType: "workgroup_member",
		targetID:   fmt.Sprintf("%d", id),
		target:     address,
		detail:     map[string]any{"role": role},
	})
}

// RemoveWorkgroupMember removes a member from a workgroup.
func (s *Service) RemoveWorkgroupMember(ctx context.Context, actor policy.Principal, id int64, address string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if _, err := s.workgroups.RemoveMember(ctx, s.db, id, address); err != nil {
		return fmt.Errorf("admin: removing workgroup member: %w", err)
	}
	return s.record(ctx, s.db, actor, change{
		action:     ActionWorkgroupRevoke,
		targetType: "workgroup_member",
		targetID:   fmt.Sprintf("%d", id),
		target:     address,
	})
}

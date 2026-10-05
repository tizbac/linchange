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

	domains   *store.Domains
	users     *store.Users
	aliases   *store.Aliases
	shared    *store.SharedMailboxes
	grants    *store.SharedMailboxGrants
	quotas    *store.Quotas
	imperson  *store.ImpersonationGrants
	audit     *store.AuditLog
	credentia *store.Credentials
	sessions  *store.Sessions

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

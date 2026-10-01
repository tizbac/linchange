package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// GrantImpersonationRequest hands one administrator permission to act as one
// user.
type GrantImpersonationRequest struct {
	Actor  string
	Target string
	Reason string
	// TTL bounds the grant. A nil TTL means it stands until it is withdrawn,
	// which is legitimate for somebody who permanently administers another's
	// account and a mistake for anybody else.
	TTL *time.Duration
}

// GrantImpersonation records a standing permission to impersonate.
//
// The reason is required here, not merely recommended: policy.CanImpersonate
// asks for one at the moment of use, and this row is the written record of why
// the power was handed over in the first place.
func (s *Service) GrantImpersonation(ctx context.Context, actor policy.Principal, req GrantImpersonationRequest) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if err := validAddress(req.Actor); err != nil {
		return err
	}
	if err := validAddress(req.Target); err != nil {
		return err
	}
	if req.Reason == "" {
		return errors.Join(ErrInvalid, errors.New("a grant needs a reason"))
	}
	// An administrator cannot grant themselves the power to impersonate
	// themselves: it is a distinction without a difference, and letting it
	// through would put a meaningless row in a table people audit.
	if strings.EqualFold(req.Actor, req.Target) {
		return errors.Join(ErrInvalid, errors.New("an administrator cannot impersonate themselves"))
	}
	// Nor may a grant be handed to somebody who is not an administrator,
	// because the power being granted is the power to administer.
	target, err := s.users.Get(ctx, s.db, req.Actor)
	if err != nil {
		return mapStore(err)
	}
	if target.Source != policy.SourceDomainAdmin {
		return errors.Join(ErrInvalid, errors.New("only a domain admin can be given impersonation"))
	}

	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.imperson.Grant(ctx, q, req.Actor, req.Target, req.Reason, expireAt(time.Now(), req.TTL)); err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionImpersonationGrant, targetType: "user", targetID: req.Target, target: req.Target,
			detail: map[string]any{
				"actor":      req.Actor,
				"reason":     req.Reason,
				"expires_in": req.TTL,
			},
		})
	})
}

// RevokeImpersonation withdraws a standing permission.
func (s *Service) RevokeImpersonation(ctx context.Context, actor policy.Principal, actorAddr, target string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.imperson.Revoke(ctx, q, actorAddr, target); err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionImpersonationRevoke, targetType: "user", targetID: target, target: target,
			detail: map[string]any{"actor": actorAddr},
		})
	})
}

// ImpersonationView is a grant plus whether it is currently in force, so an
// administration page does not have to reimplement the expiry rule.
type ImpersonationView struct {
	store.ImpersonationGrant
	Active bool `json:"active"`
}

// ListImpersonationGrants returns every grant an administrator holds, live or
// not.
func (s *Service) ListImpersonationGrants(ctx context.Context, actor policy.Principal, actorAddr string) ([]ImpersonationView, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	grants, err := s.imperson.ListForActor(ctx, s.db, actorAddr)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]ImpersonationView, 0, len(grants))
	for _, g := range grants {
		out = append(out, ImpersonationView{ImpersonationGrant: g, Active: g.Active(now)})
	}
	return out, nil
}

// WhoCanImpersonate answers "who is allowed to read this person's mail", which
// is the question a user or auditor asks about a particular address.
func (s *Service) WhoCanImpersonate(ctx context.Context, actor policy.Principal, target string) ([]ImpersonationView, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	grants, err := s.imperson.ListForTarget(ctx, s.db, target)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]ImpersonationView, 0, len(grants))
	for _, g := range grants {
		out = append(out, ImpersonationView{ImpersonationGrant: g, Active: g.Active(now)})
	}
	return out, nil
}

// ListAudit returns the audit log, filtered and newest first. It is the one
// read on this service that is not about a single object.
func (s *Service) ListAudit(ctx context.Context, actor policy.Principal, f store.AuditFilter) ([]store.AuditEntry, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	return s.audit.List(ctx, s.db, f)
}

// Note records an event that is not a change: a sign-in, a reset request, a
// refused action. These are the entries that matter most when nothing happened,
// and they are written here so the login and reset paths do not each grow their
// own idea of what an audit entry looks like.
func (s *Service) Note(ctx context.Context, entry store.AuditEntry) error {
	if _, err := s.audit.Record(ctx, s.db, entry); err != nil {
		return fmt.Errorf("admin: recording %s: %w", entry.Action, err)
	}
	return nil
}

// NoteFor is the shorthand for a note with a known actor and target.
func (s *Service) NoteFor(ctx context.Context, actor, target, action string, ip netip.Addr, detail any) error {
	var encoded json.RawMessage
	if detail != nil {
		blob, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("admin: encoding audit detail: %w", err)
		}
		encoded = blob
	}
	entry := store.AuditEntry{
		Action:     action,
		TargetType: "user",
		Detail:     encoded,
	}
	if actor != "" {
		entry.ActorAddress = &actor
	}
	if target != "" {
		entry.TargetAddress = &target
		entry.TargetID = target
	}
	if ip.IsValid() {
		entry.IP = &ip
	}
	return s.Note(ctx, entry)
}

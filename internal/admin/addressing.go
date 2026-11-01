package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
)

func ptr[T any](v T) *T { return &v }

// CreateDomain adds a domain.
func (s *Service) CreateDomain(ctx context.Context, actor policy.Principal, req store.CreateDomainRequest) (store.Domain, error) {
	if err := requireAdmin(actor); err != nil {
		return store.Domain{}, err
	}
	var (
		created store.Domain
		txErr   error
	)
	err := store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.domains.Create(ctx, q, req); err != nil {
			return mapStore(err)
		}
		created, txErr = s.domains.Get(ctx, q, req.Name)
		if txErr != nil {
			return mapStore(txErr)
		}
		return s.record(ctx, q, actor, change{
			action: ActionDomainCreate, targetType: "domain", targetID: created.Name,
			detail: map[string]any{"is_local": created.IsLocal},
		})
	})
	if err != nil {
		return store.Domain{}, err
	}
	return created, nil
}

// UpdateDomain changes a domain.
func (s *Service) UpdateDomain(ctx context.Context, actor policy.Principal, name string, req store.UpdateDomainRequest) (store.Domain, error) {
	if err := requireAdmin(actor); err != nil {
		return store.Domain{}, err
	}
	var updated store.Domain
	err := store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		before, err := s.domains.Get(ctx, q, name)
		if err != nil {
			return mapStore(err)
		}
		// Status is preserved: the store writes the whole row, and an empty
		// status is not a valid enum value, so a caller that did not intend to
		// change it would otherwise get a constraint failure. SetDomainStatus
		// is the one way to change it.
		carry := req
		carry.Status = before.Status
		// The allowed sources are stored in the form the inbound listener
		// matches against, and an unparseable entry is refused here rather than
		// accepted and then silently shutting the domain to the world.
		networks, err := store.NormalizeAntispamNetworks(req.AntispamNetworks)
		if err != nil {
			return errors.Join(ErrInvalid, err)
		}
		carry.AntispamNetworks = networks
		if err := s.domains.Update(ctx, q, name, carry); err != nil {
			return mapStore(err)
		}
		updated, err = s.domains.Get(ctx, q, name)
		if err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionDomainUpdate, targetType: "domain", targetID: name,
			detail: map[string]any{
				"before": before.Status,
				"after":  updated.Status,
				// The list is written out in full, not as a flag: who is
				// allowed to deliver mail to a domain is the kind of change
				// an operator wants to read back after an incident.
				"antispam_networks": updated.AntispamNetworks,
			},
		})
	})
	if err != nil {
		return store.Domain{}, err
	}
	return updated, nil
}

// SetDomainStatus suspends or reactivates a domain.
//
// This is the most destructive thing in the administration API: a suspended
// domain refuses new mail and hides every mailbox in it, so it is separated
// from the ordinary update path and named for what it does.
func (s *Service) SetDomainStatus(ctx context.Context, actor policy.Principal, name string, status store.DomainStatus) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if !status.Valid() {
		return errors.Join(ErrInvalid, errors.New("unknown domain status "+string(status)))
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		before, err := s.domains.Get(ctx, q, name)
		if err != nil {
			return mapStore(err)
		}
		if err := s.domains.SetStatus(ctx, q, name, status); err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionDomainUpdate, targetType: "domain", targetID: name,
			detail: map[string]any{"before": before.Status, "after": status},
		})
	})
}

// ListDomains returns every domain.
func (s *Service) ListDomains(ctx context.Context, actor policy.Principal) ([]store.Domain, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	return s.domains.List(ctx, s.db)
}

// GetDomain returns one domain.
func (s *Service) GetDomain(ctx context.Context, actor policy.Principal, name string) (store.Domain, error) {
	if err := requireAdmin(actor); err != nil {
		return store.Domain{}, err
	}
	d, err := s.domains.Get(ctx, s.db, name)
	return d, mapStore(err)
}

// --- per-domain inbound anti-spam rules ---

// maxHeaderRuleHeader bounds the header name a rule may test. A header name is
// a token in the protocol, and anything longer than this is a typo rather than
// a header an anti-spam service writes.
const maxHeaderRuleHeader = 100

// maxHeaderRuleValue bounds the text a rule compares against. Anti-spam
// verdicts are short; a value this long is a paste of a whole header, which
// belongs in a per-user mail rule, not a domain rule.
const maxHeaderRuleValue = 500

// validateHeaderRule checks a normalized request before it reaches the schema,
// so the answer names the field that is wrong instead of reporting a constraint
// violation from the database.
func validateHeaderRule(req store.DomainHeaderRuleRequest) error {
	if req.Header == "" {
		return errors.Join(ErrInvalid, errors.New("the header name is required"))
	}
	if len(req.Header) > maxHeaderRuleHeader {
		return errors.Join(ErrInvalid, fmt.Errorf("the header name is %d characters, over the %d limit",
			len(req.Header), maxHeaderRuleHeader))
	}
	if !store.ValidHeaderMatch(req.Match) {
		return errors.Join(ErrInvalid, fmt.Errorf("unknown match %q", req.Match))
	}
	if !store.ValidHeaderAction(req.Action) {
		return errors.Join(ErrInvalid, fmt.Errorf("unknown action %q", req.Action))
	}
	if len(req.Value) > maxHeaderRuleValue {
		return errors.Join(ErrInvalid, fmt.Errorf("the value is %d characters, over the %d limit",
			len(req.Value), maxHeaderRuleValue))
	}
	if req.Match != store.HeaderMatchExists && req.Value == "" {
		return errors.Join(ErrInvalid, fmt.Errorf("the match %q needs a value", req.Match))
	}
	return nil
}

// ListDomainHeaderRules returns a domain's header rules, enabled and disabled,
// in the order the inbound listener runs them.
func (s *Service) ListDomainHeaderRules(ctx context.Context, actor policy.Principal, name string) ([]store.DomainHeaderRule, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	if _, err := s.domains.Get(ctx, s.db, name); err != nil {
		return nil, mapStore(err)
	}
	return s.headerRules.List(ctx, s.db, name)
}

// AddDomainHeaderRule adds one header rule to a domain.
func (s *Service) AddDomainHeaderRule(ctx context.Context, actor policy.Principal, name string, req store.DomainHeaderRuleRequest) (store.DomainHeaderRule, error) {
	if err := requireAdmin(actor); err != nil {
		return store.DomainHeaderRule{}, err
	}
	req = req.Normalize()
	if err := validateHeaderRule(req); err != nil {
		return store.DomainHeaderRule{}, err
	}

	var rule store.DomainHeaderRule
	err := store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if _, err := s.domains.Get(ctx, q, name); err != nil {
			return mapStore(err)
		}
		created, err := s.headerRules.Create(ctx, q, name, req)
		if err != nil {
			return mapStore(err)
		}
		rule = created
		return s.record(ctx, q, actor, change{
			action: ActionHeaderRuleCreate, targetType: "domain", targetID: name,
			detail: map[string]any{
				"header": created.Header,
				"value":  created.Value,
				"match":  created.Match,
				"action": created.Action,
			},
		})
	})
	if err != nil {
		return store.DomainHeaderRule{}, err
	}
	return rule, nil
}

// UpdateDomainHeaderRule changes one rule of one domain.
//
// The rule is scoped to the domain in the URL: the ids of two domains' rules
// are not related, and an unscoped update would let a page that knows one id
// change another domain's mail.
func (s *Service) UpdateDomainHeaderRule(ctx context.Context, actor policy.Principal, name string, id int64, up store.DomainHeaderRuleUpdate) (store.DomainHeaderRule, error) {
	if err := requireAdmin(actor); err != nil {
		return store.DomainHeaderRule{}, err
	}
	err := store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		current, err := s.headerRules.GetForDomain(ctx, q, name, id)
		if err != nil {
			return mapStore(err)
		}
		next := store.DomainHeaderRuleRequest{
			Header:        current.Header,
			Value:         current.Value,
			Match:         current.Match,
			Action:        current.Action,
			CaseSensitive: current.CaseSensitive,
		}
		enabled := current.Enabled
		if up.Header != nil {
			next.Header = *up.Header
		}
		if up.Value != nil {
			next.Value = *up.Value
		}
		if up.Match != nil {
			next.Match = *up.Match
		}
		if up.Action != nil {
			next.Action = *up.Action
		}
		if up.CaseSensitive != nil {
			next.CaseSensitive = *up.CaseSensitive
		}
		if up.Enabled != nil {
			enabled = *up.Enabled
		}
		next = next.Normalize()
		if err := validateHeaderRule(next); err != nil {
			return err
		}
		if err := s.headerRules.Update(ctx, q, name, id, store.DomainHeaderRuleUpdate{
			Header:        &next.Header,
			Value:         &next.Value,
			Match:         &next.Match,
			Action:        &next.Action,
			CaseSensitive: &next.CaseSensitive,
			Enabled:       &enabled,
		}); err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionHeaderRuleUpdate, targetType: "domain", targetID: name,
			detail: map[string]any{
				"rule_id": id,
				"header":  next.Header,
				"value":   next.Value,
				"match":   next.Match,
				"action":  next.Action,
				"enabled": next.Enabled,
			},
		})
	})
	if err != nil {
		return store.DomainHeaderRule{}, err
	}
	return s.headerRules.GetForDomain(ctx, s.db, name, id)
}

// DeleteDomainHeaderRule removes one rule of one domain.
func (s *Service) DeleteDomainHeaderRule(ctx context.Context, actor policy.Principal, name string, id int64) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.headerRules.Delete(ctx, q, name, id); err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionHeaderRuleDelete, targetType: "domain", targetID: name,
			detail: map[string]any{"rule_id": id},
		})
	})
}

// --- aliases ---

// CreateAlias adds an alias with one or more targets.
func (s *Service) CreateAlias(ctx context.Context, actor policy.Principal, req store.CreateAliasRequest) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if err := validAddress(req.Address); err != nil {
		return err
	}
	if len(req.Targets) == 0 {
		return ErrInvalid
	}
	for _, t := range req.Targets {
		if t.TargetAddress != nil {
			if err := validAddress(*t.TargetAddress); err != nil {
				return err
			}
		}
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.aliases.Create(ctx, q, req); err != nil {
			return mapStore(err)
		}
		targetsDetail := make([]map[string]any, len(req.Targets))
		for i, t := range req.Targets {
			targetsDetail[i] = map[string]any{
				"target_kind":    t.TargetKind,
				"target_address": t.TargetAddress,
				"workgroup_id":   t.WorkgroupID,
				"position":       t.Position,
			}
		}
		return s.record(ctx, q, actor, change{
			action: ActionAliasCreate, targetType: "alias", targetID: req.Address,
			target: req.Address,
			detail: map[string]any{"targets": targetsDetail},
		})
	})
}

// ListAliases returns every alias.
func (s *Service) ListAliases(ctx context.Context, actor policy.Principal) ([]store.Alias, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	return s.aliases.List(ctx, s.db)
}

// SetAliasEnabled turns an alias on or off.
func (s *Service) SetAliasEnabled(ctx context.Context, actor policy.Principal, address string, enabled bool) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	action := ActionAliasEnable
	if !enabled {
		action = ActionAliasDisable
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.aliases.SetEnabled(ctx, q, address, enabled); err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: action, targetType: "alias", targetID: address, target: address,
		})
	})
}

// DeleteAlias removes an alias.
func (s *Service) DeleteAlias(ctx context.Context, actor policy.Principal, address string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.aliases.Delete(ctx, q, address); err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionAliasDelete, targetType: "alias", targetID: address, target: address,
		})
	})
}

// UpdateAliasTargets replaces all targets for an alias.
func (s *Service) UpdateAliasTargets(ctx context.Context, actor policy.Principal, address string, targets []store.AliasTargetRequest) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if len(targets) == 0 {
		return ErrInvalid
	}
	for _, t := range targets {
		if t.TargetAddress != nil {
			if err := validAddress(*t.TargetAddress); err != nil {
				return err
			}
		}
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.aliases.UpdateTargets(ctx, q, address, targets); err != nil {
			return mapStore(err)
		}
		targetsDetail := make([]map[string]any, len(targets))
		for i, t := range targets {
			targetsDetail[i] = map[string]any{
				"target_kind":    t.TargetKind,
				"target_address": t.TargetAddress,
				"workgroup_id":   t.WorkgroupID,
				"position":       t.Position,
			}
		}
		return s.record(ctx, q, actor, change{
			action: string(policy.ActionAliasUpdate), targetType: "alias", targetID: address, target: address,
			detail: map[string]any{"targets": targetsDetail},
		})
	})
}

// --- shared mailboxes ---

// DeclareSharedMailbox turns an existing shared principal into a team mailbox.
// If the shared principal does not exist, it is created automatically.
func (s *Service) DeclareSharedMailbox(ctx context.Context, actor policy.Principal, req store.CreateSharedMailboxRequest) (store.SharedMailbox, error) {
	if err := requireAdmin(actor); err != nil {
		return store.SharedMailbox{}, err
	}
	if err := validAddress(req.Address); err != nil {
		return store.SharedMailbox{}, err
	}
	if req.Colour != nil && !isHexColour(*req.Colour) {
		return store.SharedMailbox{}, errors.Join(ErrInvalid, errors.New("colour must be #rrggbb"))
	}

	var created store.SharedMailbox
	err := store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		// Ensure the shared principal exists
		if _, err := s.users.Get(ctx, q, req.Address); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				if err := s.users.Create(ctx, q, store.CreateUserRequest{
					Address:        req.Address,
					Source:         policy.SourceShared,
					DisplayName:    req.DisplayName,
					Enabled:        ptr(true),
					MailboxEnabled: ptr(true),
					QuotaBytes:     nil,
					Timezone:       "UTC",
					Language:       "en",
				}); err != nil {
					return mapStore(err)
				}
			} else {
				return mapStore(err)
			}
		}
		if err := s.shared.Declare(ctx, q, req); err != nil {
			return mapStore(err)
		}
		// The standard folders are part of a usable mailbox, and a member's
		// first look at a shared one usually happens in the web client before
		// anything has been delivered to it. Without this the mailbox opens
		// with no folder tree at all, which reads as a broken share rather than
		// as a folder set nobody has created yet.
		if err := imapd.NewMailboxRepo().EnsureFolders(ctx, q, req.Address); err != nil {
			return fmt.Errorf("creating the standard folders: %w", err)
		}
		var err error
		created, err = s.shared.Get(ctx, q, req.Address)
		if err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionSharedCreate, targetType: "shared_mailbox", targetID: req.Address,
			target: req.Address,
			detail: map[string]any{"display_name": req.DisplayName, "open_access": req.OpenAccess},
		})
	})
	if err != nil {
		return store.SharedMailbox{}, err
	}
	return created, nil
}

// UpdateSharedMailbox changes a shared mailbox.
func (s *Service) UpdateSharedMailbox(ctx context.Context, actor policy.Principal, address string, req store.UpdateSharedMailboxRequest) (store.SharedMailbox, error) {
	if err := requireAdmin(actor); err != nil {
		return store.SharedMailbox{}, err
	}
	if req.Colour != nil && !isHexColour(*req.Colour) {
		return store.SharedMailbox{}, errors.Join(ErrInvalid, errors.New("colour must be #rrggbb"))
	}
	var (
		updated store.SharedMailbox
		txErr   error
	)
	err := store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if _, err := s.shared.Get(ctx, q, address); err != nil {
			return mapStore(err)
		}
		if err := s.shared.Update(ctx, q, address, req); err != nil {
			return mapStore(err)
		}
		updated, txErr = s.shared.Get(ctx, q, address)
		if txErr != nil {
			return mapStore(txErr)
		}
		return s.record(ctx, q, actor, change{
			action: ActionSharedUpdate, targetType: "shared_mailbox", targetID: address, target: address,
			detail: map[string]any{
				"display_name":           updated.DisplayName,
				"open_access":            updated.OpenAccess,
				"auto_reply_subject_set": updated.AutoReplySubject != nil,
			},
		})
	})
	if err != nil {
		return store.SharedMailbox{}, err
	}
	return updated, nil
}

// UndeclareSharedMailbox removes the shared declaration, leaving the principal
// and its mail alone. Deleting the principal is what removes a shared mailbox
// and its mail; this is only for the case where an address stops being a team
// mailbox without being decommissioned.
func (s *Service) UndeclareSharedMailbox(ctx context.Context, actor policy.Principal, address string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.shared.Undeclare(ctx, q, address); err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionSharedUndeclare, targetType: "shared_mailbox", targetID: address, target: address,
		})
	})
}

// ListSharedMailboxes returns every shared mailbox, with its members.
func (s *Service) ListSharedMailboxes(ctx context.Context, actor policy.Principal) ([]SharedMailboxView, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	boxes, err := s.shared.List(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]SharedMailboxView, 0, len(boxes))
	for _, b := range boxes {
		grants, err := s.grants.ListForShared(ctx, s.db, b.Address)
		if err != nil {
			return nil, err
		}
		out = append(out, SharedMailboxView{SharedMailbox: b, Members: grants})
	}
	return out, nil
}

// SharedMailboxView is a shared mailbox with the members who can open it, which
// is what an administration page needs to render the row it is showing.
type SharedMailboxView struct {
	store.SharedMailbox
	Members []store.SharedMailboxGrant `json:"members"`
}

// GrantSharedMailboxAccess lets a member open a shared mailbox. Re-granting
// changes the level rather than failing, because that is what an administrator
// changing somebody's permissions means.
func (s *Service) GrantSharedMailboxAccess(ctx context.Context, actor policy.Principal, shared, member string, level policy.GrantLevel) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if !level.Valid() {
		return errors.Join(ErrInvalid, errors.New("unknown grant level "+string(level)))
	}
	if err := validAddress(member); err != nil {
		return err
	}
	// A mailbox may not be granted to itself: the owner already has every
	// permission, and a self-grant would let a member appear removable.
	if strings.EqualFold(shared, member) {
		return errors.Join(ErrInvalid, errors.New("a shared mailbox cannot be granted to itself"))
	}
	// The shared mailbox has to be declared. Without this the foreign key is
	// the only thing that notices, and a violation of it reports as a conflict
	// ("that already exists") about the very thing that does not exist.
	var declared bool
	if err := s.db.GetContext(ctx, &declared,
		`SELECT EXISTS (SELECT 1 FROM shared_mailboxes WHERE address = $1)`, shared); err != nil {
		return err
	}
	if !declared {
		return errors.Join(ErrInvalid,
			fmt.Errorf("%s is not a declared shared mailbox", shared))
	}
	// The member must be able to hold mail (mailbox_enabled = true). Domain
	// admins and disabled accounts cannot be members; they use impersonation.
	var canHoldMail bool
	err := s.db.GetContext(ctx, &canHoldMail,
		`SELECT mailbox_enabled FROM users WHERE address = $1 AND deleted_at IS NULL`, member)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.Join(ErrInvalid, errors.New("member address does not exist"))
		}
		return err
	}
	if !canHoldMail {
		return errors.Join(ErrInvalid, errors.New("member cannot hold mail: only users with an enabled mailbox may be granted access"))
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.grants.Grant(ctx, q, shared, member, level, &actor.Address); err != nil {
			// The guard trigger is a second opinion on the check above; if it
			// ever fires the answer is still "this member cannot hold mail",
			// which the caller can fix, not a conflict with something else.
			if errors.Is(err, store.ErrNoMailbox) {
				return errors.Join(ErrInvalid,
					fmt.Errorf("%s cannot be a member: it cannot hold mail", member))
			}
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionSharedGrant, targetType: "shared_mailbox", targetID: shared, target: member,
			detail: map[string]any{"shared_address": shared, "member_address": member, "level": level},
		})
	})
}

// RevokeSharedMailboxAccess removes a member's access.
func (s *Service) RevokeSharedMailboxAccess(ctx context.Context, actor policy.Principal, shared, member string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.grants.Revoke(ctx, q, shared, member); err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionSharedRevoke, targetType: "shared_mailbox", targetID: shared, target: member,
			detail: map[string]any{"shared_address": shared, "member_address": member},
		})
	})
}

// isHexColour matches the check constraint, so a bad colour is refused at the
// API boundary with a message naming the problem rather than a constraint
// violation surfacing as a 500.
func isHexColour(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for _, c := range s[1:] {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

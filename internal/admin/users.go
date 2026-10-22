package admin

import (
	"context"
	"fmt"
	"net/mail"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// UserView is a user as the administration API reports it.
type UserView struct {
	store.User
	QuotaUsedBytes    int64 `json:"quota_used_bytes"`
	QuotaMessageCount int64 `json:"quota_message_count"`
}

// CreateUserRequest is a request to provision a principal.
type CreateUserRequest struct {
	Address     string
	Source      policy.Source
	DisplayName string
	Password    string
	// QuotaBytes, when set, overrides the domain default.
	QuotaBytes *int64
	// MustChangePassword defaults to true for a local user, because an
	// administrator choosing somebody else's password has no way to know
	// whether that user can keep it.
	MustChangePassword *bool
	DailySendLimit     *int
	Timezone           string
	Language           string
	// IsDomainAdmin grants administration at creation. It is implied by the
	// domain_admin source and has to be asked for otherwise, since the usual
	// administrator of a small deployment holds mail and is created as local.
	IsDomainAdmin bool
	// RecoveryEmail is the person's own address outside this server, where an
	// administrator can send an invitation or a reset link.
	RecoveryEmail string
}

// CreateUser provisions a principal. The user row, the password and the audit
// entry are written in one transaction: an account that exists with no password,
// or a password with no account, is not a state this can produce.
func (s *Service) CreateUser(ctx context.Context, actor policy.Principal, req CreateUserRequest) (store.User, error) {
	if err := requireAdmin(actor); err != nil {
		return store.User{}, err
	}
	if err := validAddress(req.Address); err != nil {
		return store.User{}, err
	}
	if !req.Source.Valid() {
		return store.User{}, fmt.Errorf("%w: unknown user source %q", ErrInvalid, req.Source)
	}
	recovery, perr := normalisePersonalEmail(req.RecoveryEmail)
	if perr != nil {
		return store.User{}, perr
	}

	// A local user needs a password. A directory or shared principal is
	// authenticated somewhere else, so demanding one would be wrong.
	if req.Source == policy.SourceLocal {
		if err := password.Validate(req.Password, s.minPasswordLength); err != nil {
			return store.User{}, fmt.Errorf("%w: %s", ErrInvalid, err)
		}
	}

	var created store.User
	err := store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		// A local account's password is hashed before the transaction opens the
		// row, because argon2 is deliberately slow and holding a write lock
		// across it would serialise every account creation.
		var hash string
		if req.Source == policy.SourceLocal {
			var err error
			if hash, err = password.Hash(req.Password, s.argon2); err != nil {
				return fmt.Errorf("admin: hashing the password: %w", err)
			}
		}

		mustChange := true
		if req.MustChangePassword != nil {
			mustChange = *req.MustChangePassword
		}
		if err := s.users.Create(ctx, q, store.CreateUserRequest{
			Address:            req.Address,
			Source:             req.Source,
			DisplayName:        req.DisplayName,
			IsDomainAdmin:      req.IsDomainAdmin,
			QuotaBytes:         req.QuotaBytes,
			DailySendLimit:     req.DailySendLimit,
			Timezone:           req.Timezone,
			Language:           req.Language,
			MustChangePassword: mustChange,
		}); err != nil {
			return mapStore(err)
		}
		// The repository reports only whether the write worked, so the row is
		// read back to answer the caller with the values the database settled
		// on, such as the quota inherited from the domain.
		user, err := s.users.Get(ctx, q, req.Address)
		if err != nil {
			return mapStore(err)
		}
		if recovery != "" {
			if err := s.users.SetRecoveryEmail(ctx, q, user.Address, recovery); err != nil {
				return fmt.Errorf("admin: storing the personal email: %w", err)
			}
			user.RecoveryEmail = recovery
		}

		if hash != "" {
			if err := s.credentia.Set(ctx, q, user.Address, hash); err != nil {
				return fmt.Errorf("admin: storing the password: %w", err)
			}
		}
		if err := s.record(ctx, q, actor, change{
			action:     ActionUserCreate,
			targetType: "user",
			targetID:   user.Address,
			target:     user.Address,
			detail: map[string]any{
				"source":          user.Source,
				"display_name":    user.DisplayName,
				"is_domain_admin": user.IsDomainAdmin,
			},
		}); err != nil {
			return err
		}
		created = user
		return nil
	})
	if err != nil {
		return store.User{}, err
	}
	return created, nil
}

// UpdateUserRequest replaces the mutable fields of a user.
//
// It deliberately does not carry Enabled or MailboxEnabled. The store's update
// takes both as plain bools and writes whatever it is given, so a request that
// simply omitted them would disable the account and take away its mailbox. The
// service preserves them here instead, which makes an off-path caller that
// forgets them harmless rather than destructive, and leaves SetUserEnabled as
// the one way to switch an account off.
//
// IsDomainAdmin is a pointer for the same reason, and with a sharper edge:
// administration is a grant, and an update that left it out would quietly
// revoke it.
type UpdateUserRequest struct {
	// The string fields are pointers so an update that only changes one thing
	// leaves the rest alone. A caller sending {"recovery_email": ...} must not
	// erase the display name, timezone and language by omission.
	DisplayName    *string
	QuotaBytes     *int64
	DailySendLimit *int
	Timezone       *string
	Language       *string
	IsDomainAdmin  *bool
	RecoveryEmail  *string
}

// UpdateUser changes a user in place.
func (s *Service) UpdateUser(ctx context.Context, actor policy.Principal, address string, req UpdateUserRequest) (store.User, error) {
	if err := requireAdmin(actor); err != nil {
		return store.User{}, err
	}
	// An administrator who takes their own grant away cannot undo it: the next
	// request is refused for not being a domain admin, including this update's
	// own retry. Somebody else has to restore it.
	if req.IsDomainAdmin != nil && !*req.IsDomainAdmin && actor.DomainAdmin() &&
		strings.EqualFold(actor.Address, address) {
		return store.User{}, fmt.Errorf("%w: an administrator cannot remove their own administration grant", ErrInvalid)
	}
	var updated store.User
	err := store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		before, err := s.users.Get(ctx, q, address)
		if err != nil {
			return mapStore(err)
		}
		grant := before.IsDomainAdmin
		if req.IsDomainAdmin != nil {
			grant = *req.IsDomainAdmin
		}
		displayName := before.DisplayName
		if req.DisplayName != nil {
			displayName = *req.DisplayName
		}
		timezone := before.Timezone
		if req.Timezone != nil {
			timezone = *req.Timezone
		}
		language := before.Language
		if req.Language != nil {
			language = *req.Language
		}
		quota := before.QuotaBytes
		if req.QuotaBytes != nil {
			quota = req.QuotaBytes
		}
		dailyLimit := before.DailySendLimit
		if req.DailySendLimit != nil {
			dailyLimit = req.DailySendLimit
		}
		if req.RecoveryEmail != nil {
			personal, err := normalisePersonalEmail(*req.RecoveryEmail)
			if err != nil {
				return err
			}
			if err := s.users.SetRecoveryEmail(ctx, q, address, personal); err != nil {
				return fmt.Errorf("admin: storing the personal email: %w", err)
			}
		}
		if err := s.users.Update(ctx, q, address, store.UpdateUserRequest{
			DisplayName: displayName,
			// Carried over from the row just read: see UpdateUserRequest.
			Enabled:        before.Enabled,
			MailboxEnabled: before.MailboxEnabled,
			IsDomainAdmin:  grant,
			QuotaBytes:     quota,
			DailySendLimit: dailyLimit,
			Timezone:       timezone,
			Language:       language,
		}); err != nil {
			return mapStore(err)
		}
		after, err := s.users.Get(ctx, q, address)
		if err != nil {
			return mapStore(err)
		}
		if err := s.record(ctx, q, actor, change{
			action:     ActionUserUpdate,
			targetType: "user",
			targetID:   address,
			target:     address,
			detail: map[string]any{
				"before": map[string]any{"display_name": before.DisplayName},
				"after":  map[string]any{"display_name": after.DisplayName},
			},
		}); err != nil {
			return err
		}
		// Changing who may administer is a separate fact from changing a
		// display name, so it is audited as its own entry. Folding it into the
		// update above would mean a grant change could not be found in the log
		// without also reading every unrelated profile edit.
		if grant != before.IsDomainAdmin {
			if err := s.record(ctx, q, actor, change{
				action:     ActionUserAdminGrant,
				targetType: "user",
				targetID:   address,
				target:     address,
				detail: map[string]any{
					"before": before.IsDomainAdmin,
					"after":  grant,
				},
			}); err != nil {
				return err
			}
		}
		updated = after
		return nil
	})
	if err != nil {
		return store.User{}, err
	}
	return updated, nil
}

// SetUserEnabled turns an account on or off. Disabling ends every live session
// as well: an account that has been switched off must stop being usable by a
// token that was already in flight, or disabling is not a control.
func (s *Service) SetUserEnabled(ctx context.Context, actor policy.Principal, address string, enabled bool) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	action := ActionUserEnable
	if !enabled {
		action = ActionUserDisable
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.users.SetEnabled(ctx, q, address, enabled); err != nil {
			return mapStore(err)
		}
		if !enabled {
			if _, err := s.sessions.RevokeAllForAddress(ctx, q, address); err != nil {
				return fmt.Errorf("admin: ending sessions for a disabled account: %w", err)
			}
		}
		return s.record(ctx, q, actor, change{
			action: action, targetType: "user", targetID: address, target: address,
		})
	})
}

// SetUserQuota changes the quota. The usage counter is left alone: it is a
// measurement of mail that already exists, and a new limit says nothing about
// how much mail exists.
func (s *Service) SetUserQuota(ctx context.Context, actor policy.Principal, address string, quota *int64) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if quota != nil && *quota < 0 {
		return fmt.Errorf("%w: a quota cannot be negative", ErrInvalid)
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		before, err := s.users.Get(ctx, q, address)
		if err != nil {
			return mapStore(err)
		}
		if err := s.users.SetQuota(ctx, q, address, quota); err != nil {
			return mapStore(err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionUserQuota, targetType: "user", targetID: address, target: address,
			detail: map[string]any{"before": before.QuotaBytes, "after": quota},
		})
	})
}

// ResetUserPassword sets somebody's password. It ends their sessions, so
// changing a password actually revokes access rather than leaving an old token
// valid until it expires.
func (s *Service) ResetUserPassword(ctx context.Context, actor policy.Principal, address, newPassword string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if err := password.Validate(newPassword, s.minPasswordLength); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err)
	}

	hash, err := password.Hash(newPassword, s.argon2)
	if err != nil {
		return fmt.Errorf("admin: hashing the password: %w", err)
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if _, err := s.users.Get(ctx, q, address); err != nil {
			return mapStore(err)
		}
		if err := s.credentia.Set(ctx, q, address, hash); err != nil {
			return fmt.Errorf("admin: storing the password: %w", err)
		}
		if _, err := s.sessions.RevokeAllForAddress(ctx, q, address); err != nil {
			return fmt.Errorf("admin: ending sessions after a password reset: %w", err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionUserResetPassword, targetType: "user", targetID: address, target: address,
		})
	})
}

// DeleteUser removes a principal. It is a soft delete, because the mail behind
// it is the user's and a mistaken click should not destroy it; the sessions go
// regardless.
func (s *Service) DeleteUser(ctx context.Context, actor policy.Principal, address string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	return store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		if err := s.users.Delete(ctx, q, address); err != nil {
			return mapStore(err)
		}
		if _, err := s.sessions.RevokeAllForAddress(ctx, q, address); err != nil {
			return fmt.Errorf("admin: ending sessions for a deleted account: %w", err)
		}
		return s.record(ctx, q, actor, change{
			action: ActionUserDelete, targetType: "user", targetID: address, target: address,
		})
	})
}

// GetUser returns one user with its measured usage.
func (s *Service) GetUser(ctx context.Context, actor policy.Principal, address string) (UserView, error) {
	if err := requireAdmin(actor); err != nil {
		return UserView{}, err
	}
	user, err := s.users.Get(ctx, s.db, address)
	if err != nil {
		return UserView{}, mapStore(err)
	}
	usage, err := s.quotas.Get(ctx, s.db, address)
	if err != nil {
		return UserView{}, err
	}
	return UserView{User: user, QuotaUsedBytes: usage.UsedBytes, QuotaMessageCount: usage.MessageCount}, nil
}

// ListUsers returns every principal, with usage, so a list page can show what
// each account is holding without a request per row.
func (s *Service) ListUsers(ctx context.Context, actor policy.Principal) ([]UserView, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	users, err := s.users.List(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]UserView, 0, len(users))
	for _, u := range users {
		usage, err := s.quotas.Get(ctx, s.db, u.Address)
		if err != nil {
			return nil, err
		}
		out = append(out, UserView{User: u, QuotaUsedBytes: usage.UsedBytes, QuotaMessageCount: usage.MessageCount})
	}
	return out, nil
}

// validAddress rejects anything that is not a plain address, so a mistake at
// the API boundary is reported rather than becoming a row the delivery path
// will keep failing on.
// normalisePersonalEmail accepts an address the administrator typed for the
// person, outside this server. An empty value is allowed and means "none".
func normalisePersonalEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", nil
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || !strings.EqualFold(addr.Address, email) || addr.Name != "" {
		return "", fmt.Errorf("%w: %q is not a valid personal email address", ErrInvalid, email)
	}
	return email, nil
}

// InviteUser records an invitation and returns the account it is for. The
// caller sends the mail: the admin service has no transport, and the HTTP layer
// already holds the outbox and the public URL the link has to point at.
func (s *Service) InviteUser(ctx context.Context, actor policy.Principal, address string) (store.User, error) {
	if err := requireAdmin(actor); err != nil {
		return store.User{}, err
	}
	var invited store.User
	err := store.InTx(ctx, s.db, func(q *sqlx.Tx) error {
		user, err := s.users.Get(ctx, q, address)
		if err != nil {
			return mapStore(err)
		}
		if !user.Enabled {
			return fmt.Errorf("%w: the account is disabled", ErrInvalid)
		}
		if user.RecoveryEmail == "" {
			return fmt.Errorf("%w: set a personal email before inviting this account", ErrInvalid)
		}
		if _, err := normalisePersonalEmail(user.RecoveryEmail); err != nil {
			return err
		}
		if err := s.record(ctx, q, actor, change{
			action:     ActionUserInvite,
			targetType: "user",
			targetID:   user.Address,
			target:     user.Address,
			detail:     map[string]any{"sent_to": user.RecoveryEmail},
		}); err != nil {
			return err
		}
		invited = user
		return nil
	})
	if err != nil {
		return store.User{}, err
	}
	return invited, nil
}

func validAddress(address string) error {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return fmt.Errorf("%w: the address is empty", ErrInvalid)
	}
	if _, err := mail.ParseAddress(trimmed); err != nil {
		return fmt.Errorf("%w: %q is not an address", ErrInvalid, address)
	}
	if trimmed != address {
		return fmt.Errorf("%w: the address has surrounding whitespace", ErrInvalid)
	}
	return nil
}

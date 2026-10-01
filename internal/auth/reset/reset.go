// Package reset implements self-service password reset for principals whose
// password is held here.
//
// The flow is two steps, and the security lives in the gap between them. The
// request step proves nothing and must never reveal whether an address exists,
// so it answers in the same shape in every case and only actually issues a
// token and sends a link when there is a reset-able account with a verified
// out-of-band address. The complete step is the only one that holds a secret
// (the link), so it is the only one allowed to be specific with a caller.
package reset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/secret"
	"github.com/linchange/linchange/internal/store"
)

// Errors the complete step may return to a caller. By the time someone is
// completing a reset they already hold a link, so being specific leaks nothing
// about accounts and is exactly what lets them correct a mistake and retry.
var (
	// ErrInvalidToken means the presented token matches nothing.
	ErrInvalidToken = errors.New("reset: this reset link is not valid")
	// ErrExpired means the link is real but past its deadline.
	ErrExpired = errors.New("reset: this reset link has expired")
	// ErrAlreadyUsed means the link was already redeemed.
	ErrAlreadyUsed = errors.New("reset: this reset link has already been used")
)

// Sinker delivers the reset email. The production one is an outbox send (P3);
// a development setup or a test stands in something that logs or records it.
// The plaintext token is only ever passed here.
type Sinker func(ctx context.Context, to, token string, expiresAt time.Time) error

// Options is everything a Resetter needs.
type Options struct {
	SecondaryEmails *store.SecondaryEmails
	Tokens          *store.ResetTokens
	Credentials     *store.Credentials
	// Sessions, if set, are revoked in full when a password is reset, because a
	// session opened under the old password is otherwise still live.
	Sessions *store.Sessions
	// Sinker delivers the email. Nil means "no transport yet", which is what a
	// disabled reset or a not-yet-wired outbox does.
	Sinker Sinker
	// Enabled mirrors auth.reset_password.enabled. A disabled reset still
	// answers the request step, but issues nothing.
	Enabled           bool
	Argon2            config.Argon2Params
	MinPasswordLength int
	TokenTTL          time.Duration
	Logger            *slog.Logger
	// Now overrides the clock, so a test can reach an expiry exactly.
	Now func() time.Time
}

// Resetter drives the two reset steps.
type Resetter struct {
	emails      *store.SecondaryEmails
	tokens      *store.ResetTokens
	credentials *store.Credentials
	sessions    *store.Sessions
	sinker      Sinker
	enabled     bool
	argon2      config.Argon2Params
	minLength   int
	tokenTTL    time.Duration
	log         *slog.Logger
	now         func() time.Time
}

// New builds a Resetter, filling in the pieces a caller left out.
func New(opts Options) (*Resetter, error) {
	switch {
	case opts.Tokens == nil:
		return nil, errors.New("reset: token repository is required")
	case opts.SecondaryEmails == nil:
		return nil, errors.New("reset: secondary email repository is required")
	case opts.Credentials == nil:
		return nil, errors.New("reset: credentials repository is required")
	case opts.TokenTTL <= 0:
		return nil, errors.New("reset: a positive token lifetime is required")
	}

	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Resetter{
		emails:      opts.SecondaryEmails,
		tokens:      opts.Tokens,
		credentials: opts.Credentials,
		sessions:    opts.Sessions,
		sinker:      opts.Sinker,
		enabled:     opts.Enabled,
		argon2:      opts.Argon2,
		minLength:   opts.MinPasswordLength,
		tokenTTL:    opts.TokenTTL,
		log:         log,
		now:         now,
	}, nil
}

// Request is the first step. It is handed an address and reports whether a
// reset may proceed, without ever revealing whether the address exists.
type Request struct {
	Address string
	IP      netip.Addr
}

// Requested reports the outcome to a caller that holds no secret.
//
// Delivered, Token and SentTo exist for the server and its tests. A handler
// must respond to the browser the same way whether Delivered is true or false,
// or a reset form becomes an account-enumeration oracle: the only safe response
// is "if that address is registered, a link is on its way".
type Requested struct {
	// Delivered is true only when a link was actually sent somewhere.
	Delivered bool
	// Token is the plaintext that was emailed. Never send it back to whoever
	// asked for the reset.
	Token     string `json:"-"`
	SentTo    string `json:"-"`
	ExpiresAt time.Time
}

// Request decides whether and where to send a reset link.
func (r *Resetter) Request(ctx context.Context, q store.Querier, req Request) (Requested, error) {
	var out Requested
	if req.Address == "" {
		return out, nil
	}

	principal, perr := store.LoadPrincipal(ctx, q, req.Address)
	if perr != nil {
		// Unknown, disabled, or deleted: nothing to send, and no error, so the
		// answer is indistinguishable from a request for an address that simply
		// has no out-of-band email.
		return out, nil
	}
	if !r.enabled {
		return out, nil
	}
	if principal.Source != policy.SourceLocal && principal.Source != policy.SourceDomainAdmin {
		// A directory user's password lives in the directory, and a shared
		// mailbox has no password. Neither resets here, and the caller must not
		// be able to tell this from an address that does not exist.
		return out, nil
	}

	to, err := r.resetTarget(ctx, q, req.Address)
	if err != nil {
		// A database problem on this read is a server fault, not "no target",
		// so it is surfaced rather than swallowed into a silent no-op.
		return out, err
	}
	if to == "" {
		// The account is reset-able but has no verified out-of-band address.
		return out, nil
	}

	token, err := secret.GenerateToken()
	if err != nil {
		return out, fmt.Errorf("reset: generating a token: %w", err)
	}
	expiresAt := r.now().Add(r.tokenTTL)
	if _, err := r.tokens.Create(ctx, q, store.CreateResetTokenRequest{
		Address:   req.Address,
		TokenHash: secret.Hash(token),
		SentTo:    to,
		ExpiresAt: expiresAt,
	}); err != nil {
		return out, err
	}

	if r.sinker != nil {
		if err := r.sinker(ctx, to, token, expiresAt); err != nil {
			// The token is stored but the email failed. It will expire on its
			// own. The operator sees the failure; the handler still answers the
			// browser generically.
			r.log.ErrorContext(ctx, "sending the reset email failed",
				"address", req.Address, "to", to, "error", err)
			return out, fmt.Errorf("reset: sending the reset email: %w", err)
		}
	}

	out.Delivered = true
	out.Token = token
	out.SentTo = to
	out.ExpiresAt = expiresAt
	r.log.InfoContext(ctx, "reset requested",
		"address", req.Address, "to", to, "expires_at", expiresAt)
	return out, nil
}

// CompleteRequest is what the reset form posts back: the link's token and the
// new password.
type CompleteRequest struct {
	Token       string
	NewPassword string
	IP          netip.Addr
}

// Completed names the principal that just reset.
type Completed struct {
	Address string
}

// Complete redeems a link and sets a new password.
//
// The password is validated before the token is spent, so a rejected password
// leaves the link usable for a corrected attempt. The token is spent before the
// password is written, so two requests carrying the same link cannot both set a
// password.
func (r *Resetter) Complete(ctx context.Context, q store.Querier, req CompleteRequest) (Completed, error) {
	var out Completed
	if !r.enabled || req.Token == "" {
		return out, ErrInvalidToken
	}

	stored, err := r.tokens.Find(ctx, q, secret.Hash(req.Token))
	if err != nil {
		if errors.Is(err, store.ErrNoResetToken) {
			return out, ErrInvalidToken
		}
		return out, err
	}
	if stored.UsedAt != nil {
		return out, ErrAlreadyUsed
	}
	if !stored.ExpiresAt.After(r.now()) {
		return out, ErrExpired
	}

	if err := password.Validate(req.NewPassword, r.minLength); err != nil {
		return out, err
	}

	spent, err := r.tokens.Consume(ctx, q, stored.ID)
	if err != nil {
		return out, err
	}
	if !spent {
		return out, ErrAlreadyUsed
	}

	hash, err := password.Hash(req.NewPassword, r.argon2)
	if err != nil {
		return out, fmt.Errorf("reset: hashing the new password: %w", err)
	}
	if err := r.credentials.Set(ctx, q, stored.Address, hash); err != nil {
		return out, fmt.Errorf("reset: storing the new password: %w", err)
	}
	// A self-reset means the user now controls the password, so the forced-
	// change flag is cleared.
	if err := store.SetMustChangePassword(ctx, q, stored.Address, false); err != nil {
		return out, fmt.Errorf("reset: clearing the forced-change flag: %w", err)
	}
	// Every session opened under the old password is now invalid.
	if r.sessions != nil {
		if _, err := r.sessions.RevokeAllForAddress(ctx, q, stored.Address); err != nil {
			return out, fmt.Errorf("reset: revoking sessions: %w", err)
		}
	}

	r.log.InfoContext(ctx, "password reset",
		"address", stored.Address, "ip", ipString(req.IP))
	out.Address = stored.Address
	return out, nil
}

// resetTarget picks the single verified address a link should go to, or "" if
// there is none. The designated primary verified email wins; failing that, any
// verified one. Unverified addresses are skipped: a reset link is a credential
// and must only be sent somewhere the user has proved to hold.
func (r *Resetter) resetTarget(ctx context.Context, q store.Querier, address string) (string, error) {
	list, err := r.emails.List(ctx, q, address)
	if err != nil {
		return "", err
	}
	fallback := ""
	for _, e := range list {
		if e.VerifiedAt == nil {
			continue
		}
		if e.IsPrimary {
			return e.Email, nil
		}
		if fallback == "" {
			fallback = e.Email
		}
	}
	return fallback, nil
}

func ipString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

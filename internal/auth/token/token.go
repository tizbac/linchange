// Package token issues and verifies the short-lived access tokens used by the
// web interface and the Exchange endpoints.
//
// Access tokens are self-contained so that a request does not need a database
// round trip, which is what keeps a mailbox listing fast. The cost is that
// revoking one is not immediate, so the token carries the session id and every
// privileged request still checks that session against the sessions table.
// Access tokens are therefore a cache, not an authority.
package token

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/secret"
)

// Errors returned by this package.
//
// The HTTP layer maps ErrExpired to 401 and everything else to 401 as well,
// because a caller learns nothing useful from the difference, but the
// distinction is kept for the logs.
var (
	// ErrInvalid means the token is malformed, badly signed, or from a
	// different issuer.
	ErrInvalid = errors.New("token: invalid")
	// ErrExpired means the token was well formed but has passed its expiry.
	ErrExpired = errors.New("token: expired")
	// ErrWrongType means a refresh token was presented where an access token
	// was expected, or the reverse. Catching this stops a stolen refresh token
	// being used directly as a bearer credential.
	ErrWrongType = errors.New("token: wrong token type")
	// ErrNotYetValid means the token's nbf is in the future, which normally
	// means a clock problem rather than an attack.
	ErrNotYetValid = errors.New("token: not yet valid")
)

// TokenType distinguishes the two tokens. Both are signed with the same key, so
// without this a refresh token would be accepted wherever an access token is
// expected.
type TokenType string

const (
	// TypeAccess is the short-lived token sent on every request.
	TypeAccess TokenType = "access"
	// TypeRefresh is the long-lived token used only to mint access tokens.
	TypeRefresh TokenType = "refresh"
)

// Manager issues and verifies tokens.
type Manager struct {
	secret     []byte
	issuer     string
	accessTTL  time.Duration
	refreshTTL time.Duration
	// leeway absorbs clock drift between this server and the browser, which
	// is routinely a minute or more.
	leeway time.Duration
	// now is overridable so expiry can be tested without sleeping.
	now func() time.Time
}

// NewManager builds a Manager from configuration.
func NewManager(cfg config.JWTAuth) (*Manager, error) {
	if len(cfg.Secret) < 32 {
		return nil, fmt.Errorf("token: auth.jwt.secret must be at least 32 bytes, got %d", len(cfg.Secret))
	}
	if cfg.AccessTTL <= 0 {
		return nil, errors.New("token: auth.jwt.access_ttl must be positive")
	}
	if cfg.RefreshTTL < cfg.AccessTTL {
		return nil, errors.New("token: auth.jwt.refresh_ttl must not be shorter than access_ttl")
	}

	issuer := strings.TrimSpace(cfg.Issuer)
	if issuer == "" {
		issuer = "linchange"
	}

	return &Manager{
		secret:     []byte(cfg.Secret),
		issuer:     issuer,
		accessTTL:  cfg.AccessTTL,
		refreshTTL: cfg.RefreshTTL,
		leeway:     30 * time.Second,
		now:        time.Now,
	}, nil
}

// SetClock replaces the time source. It exists for tests; production code must
// leave the default in place.
func (m *Manager) SetClock(now func() time.Time) { m.now = now }

// Issuer is the value the iss claim is checked against.
func (m *Manager) Issuer() string { return m.issuer }

// AccessTTL is how long an access token is valid.
func (m *Manager) AccessTTL() time.Duration { return m.accessTTL }

// RefreshTTL is how long a refresh token is valid.
func (m *Manager) RefreshTTL() time.Duration { return m.refreshTTL }

// claims is the token body. It is deliberately unexported so that a caller
// cannot construct one and pass it to a verifying function by mistake.
type claims struct {
	jwt.RegisteredClaims

	// SessionID ties the token to a row in sessions, which is what makes
	// revocation possible despite the token being self-contained.
	SessionID string `json:"sid"`
	// Source is the principal's source, so a policy decision can be made
	// without a database read.
	Source string `json:"src"`
	// Type separates access from refresh tokens.
	Type TokenType `json:"typ"`
	// MustChangePassword is carried so a request can be refused without a
	// database read.
	MustChangePassword bool `json:"pwd"`
	// MailboxEnabled is false for a domain admin.
	MailboxEnabled bool `json:"mbx"`
	// Impersonating names the principal a domain admin is acting as, or is
	// empty. It is in the token so that an audit record cannot be forgotten at
	// the point of use.
	Impersonating string `json:"imp,omitempty"`
}

// Issued is a freshly minted token.
type Issued struct {
	// Token is the signed, URL-safe string.
	Token string
	// ExpiresAt is when the token stops being valid.
	ExpiresAt time.Time
}

// IssueAccess mints an access token for p against session s.
//
// s must be a real, unrevoked session. Nothing here checks that, because
// issuing is the login path's job and it holds the session; verifying that a
// session still exists is every privileged request's job.
func (m *Manager) IssueAccess(p policy.Principal, sessionID string, impersonating string) (Issued, error) {
	return m.issue(p, sessionID, TypeAccess, m.accessTTL, impersonating)
}

// IssueRefresh mints a refresh token. It carries the same claims as the access
// token so that a refresh does not need to look the principal up again, and
// the same session id so refreshing does not outlive a revocation.
func (m *Manager) IssueRefresh(p policy.Principal, sessionID string, impersonating string) (Issued, error) {
	return m.issue(p, sessionID, TypeRefresh, m.refreshTTL, impersonating)
}

func (m *Manager) issue(p policy.Principal, sessionID string, kind TokenType, ttl time.Duration, impersonating string) (Issued, error) {
	if sessionID == "" {
		return Issued{}, errors.New("token: a session id is required")
	}
	if p.Address == "" {
		return Issued{}, errors.New("token: a principal is required")
	}

	now := m.now()
	expires := now.Add(ttl)

	body := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   p.Address,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
			ID:        newID(),
		},
		SessionID:          sessionID,
		Source:             string(p.Source),
		Type:               kind,
		MustChangePassword: p.MustChangePassword,
		MailboxEnabled:     p.MailboxEnabled,
		Impersonating:      impersonating,
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, body).SignedString(m.secret)
	if err != nil {
		return Issued{}, fmt.Errorf("token: signing: %w", err)
	}
	return Issued{Token: signed, ExpiresAt: expires}, nil
}

// Parse verifies a token and returns the principal it names.
//
// The returned principal is not authenticated in the policy sense: it is
// reconstructed from signed claims, which authenticates the token but not the
// session behind it. A caller acting on anything privileged must additionally
// confirm the session is still live.
func (m *Manager) Parse(raw string, want TokenType) (policy.Principal, string, error) {
	var noPrincipal policy.Principal

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return noPrincipal, "", ErrInvalid
	}

	var body claims
	parsed, err := jwt.ParseWithClaims(raw, &body, m.keyFunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(m.issuer),
		jwt.WithLeeway(m.leeway),
		jwt.WithTimeFunc(m.now),
	)
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return noPrincipal, "", ErrExpired
		case errors.Is(err, jwt.ErrTokenNotValidYet):
			return noPrincipal, "", ErrNotYetValid
		default:
			return noPrincipal, "", ErrInvalid
		}
	}
	if !parsed.Valid {
		return noPrincipal, "", ErrInvalid
	}

	// A token signed with the right key but of the wrong kind is a real attack
	// shape: stealing a refresh token should not yield an API credential.
	if body.Type != want {
		return noPrincipal, "", ErrWrongType
	}
	// The subject is the principal's address, and it is the only claim that
	// names one. Nothing is gained by asserting it against a second copy of
	// itself: every claim here is covered by the same signature, so two copies
	// of the same fact would only catch a bug in the signing path, not an
	// attacker. What does have to hold is that the token names somebody and
	// belongs to a session, since a token with neither is unusable.
	if body.Subject == "" {
		return noPrincipal, "", ErrInvalid
	}
	if body.SessionID == "" {
		return noPrincipal, "", ErrInvalid
	}
	// An unrecognised source would make every policy decision deny, which fails
	// closed but reads as a permissions bug in the logs rather than a bad
	// token. Reject it outright so the cause is legible.
	if !policy.Source(body.Source).Valid() {
		return noPrincipal, "", ErrInvalid
	}

	p := policy.Principal{
		Address:            body.Subject,
		Source:             policy.Source(body.Source),
		Enabled:            true,
		MailboxEnabled:     body.MailboxEnabled,
		MustChangePassword: body.MustChangePassword,
		Authenticated:      true,
	}
	return p, body.SessionID, nil
}

// keyFunc supplies the verification key.
//
// It pins the algorithm rather than trusting the token header. A token that
// names "none", or an asymmetric algorithm, is rejected here instead of being
// verified with the wrong kind of key.
func (m *Manager) keyFunc(t *jwt.Token) (any, error) {
	if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
		return nil, fmt.Errorf("token: unexpected signing method %v", t.Header["alg"])
	}
	return m.secret, nil
}

// BearerToken extracts the credential from an Authorization header value.
//
// It returns an error rather than an empty string so that a caller cannot
// accidentally treat "no header" and "wrong scheme" as the same thing and log
// them identically.
func BearerToken(header string) (string, error) {
	const prefix = "bearer "

	// RFC 7235 allows optional whitespace around the field value, so padding
	// is trimmed once here instead of being special-cased at each step.
	header = strings.TrimSpace(header)
	if header == "" {
		return "", ErrInvalid
	}
	// The scheme is case-insensitive.
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", ErrInvalid
	}

	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", ErrInvalid
	}
	return token, nil
}

// newID returns a unique token id.
//
// It exists so two tokens minted in the same second with the same claims are
// still distinguishable, which makes a support conversation about "which token
// was this" answerable from the logs.
func newID() string {
	id, err := secret.Generate(12)
	if err != nil {
		// A random source failure means the token is still signed and still
		// unique by (session, issued-at); only the diagnostic id is lost.
		return ""
	}
	return id
}

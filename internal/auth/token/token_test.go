package token_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/token"
)

const secret32 = "0123456789abcdef0123456789abcdef0123456789"

func cfg() config.JWTAuth {
	return config.JWTAuth{
		Secret:     secret32,
		Issuer:     "linchange",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 720 * time.Hour,
	}
}

func newManager(t *testing.T, c config.JWTAuth) *token.Manager {
	t.Helper()
	m, err := token.NewManager(c)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func principal() policy.Principal {
	return policy.Principal{
		Address:        "alice@example.com",
		Source:         policy.SourceLocal,
		Enabled:        true,
		MailboxEnabled: true,
		Authenticated:  true,
	}
}

func TestIssueAndParseAccess(t *testing.T) {
	m := newManager(t, cfg())

	issued, err := m.IssueAccess(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	if issued.Token == "" {
		t.Fatal("IssueAccess produced an empty token")
	}
	if strings.Count(issued.Token, ".") != 2 {
		t.Errorf("token %q does not look like a JWT", issued.Token)
	}

	got, sessionID, err := m.Parse(issued.Token, token.TypeAccess)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sessionID != "sess-1" {
		t.Errorf("session id = %q, want sess-1", sessionID)
	}
	if got.Address != "alice@example.com" {
		t.Errorf("address = %q", got.Address)
	}
	if got.Source != policy.SourceLocal {
		t.Errorf("source = %q, want local", got.Source)
	}
	if !got.MailboxEnabled {
		t.Error("mailbox_enabled was lost in the round trip")
	}
	if !got.Authenticated {
		t.Error("a parsed principal should be marked authenticated")
	}
}

// A stolen refresh token must not work as an API credential. Both tokens are
// signed with the same key, so the type claim is the only thing separating them.
func TestRefreshTokenIsNotAcceptedAsAnAccessToken(t *testing.T) {
	m := newManager(t, cfg())

	refresh, err := m.IssueRefresh(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueRefresh: %v", err)
	}
	if _, _, err := m.Parse(refresh.Token, token.TypeAccess); !errors.Is(err, token.ErrWrongType) {
		t.Errorf("a refresh token was accepted as an access token: %v", err)
	}

	access, err := m.IssueAccess(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	if _, _, err := m.Parse(access.Token, token.TypeRefresh); !errors.Is(err, token.ErrWrongType) {
		t.Errorf("an access token was accepted as a refresh token: %v", err)
	}
}

func TestTokensAreSignedWithTheConfiguredSecret(t *testing.T) {
	issued, err := newManager(t, cfg()).IssueAccess(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	// A different server with a different secret must not accept it.
	other := cfg()
	other.Secret = strings.Repeat("z", 40)
	if _, _, err := newManager(t, other).Parse(issued.Token, token.TypeAccess); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("a token signed with another secret was accepted: %v", err)
	}
}

func TestTokensFromAnotherIssuerAreRejected(t *testing.T) {
	issued, err := newManager(t, cfg()).IssueAccess(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	other := cfg()
	other.Issuer = "someone-else"
	if _, _, err := newManager(t, other).Parse(issued.Token, token.TypeAccess); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("a token from another issuer was accepted: %v", err)
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	m := newManager(t, cfg())
	base := time.Unix(1_700_000_000, 0)
	m.SetClock(func() time.Time { return base })

	issued, err := m.IssueAccess(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	// Inside the lifetime.
	m.SetClock(func() time.Time { return base.Add(14 * time.Minute) })
	if _, _, err := m.Parse(issued.Token, token.TypeAccess); err != nil {
		t.Errorf("a token inside its lifetime was rejected: %v", err)
	}

	// Past it, allowing for the leeway a browser clock needs.
	m.SetClock(func() time.Time { return base.Add(16 * time.Minute) })
	if _, _, err := m.Parse(issued.Token, token.TypeAccess); !errors.Is(err, token.ErrExpired) {
		t.Errorf("an expired token was accepted: %v", err)
	}
}

// Leeway exists because browser clocks are routinely a minute out. Without it
// every user whose clock runs fast sees a spurious logout.
func TestLeewayToleratesAClockThatRunsFast(t *testing.T) {
	m := newManager(t, cfg())
	base := time.Unix(1_700_000_000, 0)
	m.SetClock(func() time.Time { return base })

	issued, err := m.IssueAccess(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	// 20 seconds past a 15 minute life is within the 30 second leeway.
	m.SetClock(func() time.Time { return base.Add(15*time.Minute + 20*time.Second) })
	if _, _, err := m.Parse(issued.Token, token.TypeAccess); err != nil {
		t.Errorf("a token just past expiry was rejected despite the leeway: %v", err)
	}

	// Well past the leeway it must not be honoured.
	m.SetClock(func() time.Time { return base.Add(15*time.Minute + 5*time.Minute) })
	if _, _, err := m.Parse(issued.Token, token.TypeAccess); !errors.Is(err, token.ErrExpired) {
		t.Errorf("a long expired token was accepted: %v", err)
	}
}

func TestNotBeforeIsEnforced(t *testing.T) {
	m := newManager(t, cfg())
	base := time.Unix(1_700_000_000, 0)

	// Mint the token with a clock that is behind, then verify with one that is
	// ahead, so the token looks like it starts in the future.
	m.SetClock(func() time.Time { return base })
	issued, err := m.IssueAccess(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	// 10 minutes earlier is outside the 30 second leeway, so nbf is in the
	// future from the verifier's point of view.
	m.SetClock(func() time.Time { return base.Add(-10 * time.Minute) })
	if _, _, err := m.Parse(issued.Token, token.TypeAccess); !errors.Is(err, token.ErrNotYetValid) {
		t.Errorf("a token whose nbf is in the future was accepted: %v", err)
	}
}

// The algorithm is pinned in the header rather than trusted from the token, so
// a token asking for "none" or for an asymmetric algorithm is rejected outright.
func TestAlgorithmConfusionIsRejected(t *testing.T) {
	m := newManager(t, cfg())
	issued, err := m.IssueAccess(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	parts := strings.Split(issued.Token, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a three part JWT: %q", issued.Token)
	}

	// Re-sign the same claims with alg=none.
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"iss": "linchange", "sub": "alice@example.com",
		"addr": "alice@example.com", "src": "local", "sid": "sess-1",
		"typ": "access", "exp": time.Now().Add(time.Hour).Unix(),
	})
	none, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("building an alg=none token: %v", err)
	}
	if _, _, err := m.Parse(none, token.TypeAccess); err == nil {
		t.Error("an alg=none token was accepted")
	}

	// Corrupt the signature.
	tampered := parts[0] + "." + parts[1] + ".AAAA"
	if _, _, err := m.Parse(tampered, token.TypeAccess); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("a token with a broken signature was accepted: %v", err)
	}

	// Corrupt the payload, which must invalidate the signature.
	if _, _, err := m.Parse(parts[0]+".eyJmb28iOiJiYXIifQ."+parts[2], token.TypeAccess); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("a token with a tampered payload was accepted: %v", err)
	}
}

// The subject names the principal and is the only claim that does, so there is
// deliberately no second copy of it to cross-check: the signature already
// covers every claim equally. What protects the subject is the signature, and
// these are the tests that say so.
func TestSubjectCannotBeChangedWithoutTheSecret(t *testing.T) {
	m := newManager(t, cfg())
	issued, err := m.IssueAccess(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}

	segments := strings.Split(issued.Token, ".")
	if len(segments) != 3 {
		t.Fatalf("token is not a three part JWT: %q", issued.Token)
	}

	// Re-encode the claims with the subject pointed at somebody else, keeping
	// the original signature.
	forged := jwt.MapClaims{
		"iss": "linchange", "sub": "bob@example.com", "src": "local",
		"sid": "sess-1", "typ": "access", "mbx": true, "pwd": false,
		"iat": time.Now().Unix(), "nbf": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	payload, err := json.Marshal(forged)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	swapped := segments[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + segments[2]
	if _, _, err := m.Parse(swapped, token.TypeAccess); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("a token whose subject was swapped was accepted: %v", err)
	}

	// A token with no session id cannot be tied to anything revocable, so it is
	// rejected even though it is correctly signed.
	noSession, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "linchange", "sub": "alice@example.com", "src": "local",
		"typ": "access", "mbx": true,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}).SignedString([]byte(secret32))
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	if _, _, err := m.Parse(noSession, token.TypeAccess); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("a token with no session id was accepted: %v", err)
	}
}

// An unrecognised source would make every policy decision deny. That fails
// closed, but it would be reported as a permissions bug, so it is rejected as
// a bad token instead.
func TestUnknownSourceIsRejected(t *testing.T) {
	m := newManager(t, cfg())
	unknown, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "linchange", "sub": "alice@example.com", "src": "wizard",
		"sid": "sess-1", "typ": "access",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}).SignedString([]byte(secret32))
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	if _, _, err := m.Parse(unknown, token.TypeAccess); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("a token with an unknown source was accepted: %v", err)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	m := newManager(t, cfg())
	for _, bad := range []string{
		"",
		"   ",
		"not-a-token",
		"a.b",
		"a.b.c.d",
		"....",
	} {
		if _, _, err := m.Parse(bad, token.TypeAccess); err == nil {
			t.Errorf("Parse accepted %q", bad)
		}
	}
}

func TestIssueRequiresASessionAndPrincipal(t *testing.T) {
	m := newManager(t, cfg())

	if _, err := m.IssueAccess(principal(), "", ""); err == nil {
		t.Error("IssueAccess accepted an empty session id")
	}
	if _, err := m.IssueAccess(policy.Principal{}, "sess-1", ""); err == nil {
		t.Error("IssueAccess accepted an empty principal")
	}
}

// Carrying the impersonation in the token is what stops an audit record being
// forgotten at the point of use.
func TestImpersonationSurvivesTheRoundTrip(t *testing.T) {
	m := newManager(t, cfg())
	issued, err := m.IssueAccess(principal(), "sess-1", "bob@example.com")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	if !strings.Contains(issued.Token, ".") {
		t.Fatal("no token")
	}

	// The impersonation is inside the signed payload, so it cannot be swapped.
	segments := strings.Split(issued.Token, ".")
	if len(segments) != 3 {
		t.Fatalf("token is not a three part JWT: %q", issued.Token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if !strings.Contains(string(raw), "bob@example.com") {
		t.Errorf("the impersonated principal is not in the signed payload: %s", raw)
	}
}

func TestNewManagerRejectsUnsafeConfig(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*config.JWTAuth)
	}{
		{"empty secret", func(c *config.JWTAuth) { c.Secret = "" }},
		{"short secret", func(c *config.JWTAuth) { c.Secret = "tooshort" }},
		{"zero access ttl", func(c *config.JWTAuth) { c.AccessTTL = 0 }},
		{"negative access ttl", func(c *config.JWTAuth) { c.AccessTTL = -time.Minute }},
		{"refresh shorter than access", func(c *config.JWTAuth) { c.RefreshTTL = time.Minute }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg()
			tc.mut(&c)
			if _, err := token.NewManager(c); err == nil {
				t.Error("NewManager accepted an unsafe configuration")
			}
		})
	}
}

func TestRefreshTTLIsLongerThanAccessTTL(t *testing.T) {
	m := newManager(t, cfg())
	if m.RefreshTTL() <= m.AccessTTL() {
		t.Errorf("refresh TTL %v is not longer than access TTL %v", m.RefreshTTL(), m.AccessTTL())
	}
}

func TestTwoTokensDiffer(t *testing.T) {
	m := newManager(t, cfg())
	first, err := m.IssueAccess(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	second, err := m.IssueAccess(principal(), "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	if first.Token == second.Token {
		t.Error("two tokens for the same principal and session are identical, " +
			"so the jti cannot be used to tell them apart")
	}
}

func TestBearerToken(t *testing.T) {
	got, err := token.BearerToken("Bearer abc.def.ghi")
	if err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
	if got != "abc.def.ghi" {
		t.Errorf("got %q", got)
	}

	// The scheme is case-insensitive per RFC 7235.
	for _, header := range []string{"bearer abc", "BEARER abc", "BeArEr abc"} {
		if _, err := token.BearerToken(header); err != nil {
			t.Errorf("BearerToken(%q): %v", header, err)
		}
	}

	// Leading and trailing space is common in a hand-built header.
	if got, err := token.BearerToken("  Bearer   abc  "); err != nil || got != "abc" {
		t.Errorf("BearerToken with padding = %q, %v", got, err)
	}

	for _, bad := range []string{
		"",
		"   ",
		"abc",
		"Basic abc",
		"Bearer",
		"Bearer ",
		"Token abc",
	} {
		if _, err := token.BearerToken(bad); err == nil {
			t.Errorf("BearerToken accepted %q", bad)
		}
	}
}

// A domain admin's token must still say it cannot hold mail, so a policy check
// made without a database read reaches the right answer.
func TestDomainAdminTokenCarriesItsInabilityToHoldMail(t *testing.T) {
	m := newManager(t, cfg())
	admin := policy.Principal{
		Address:        "admin@example.com",
		Source:         policy.SourceDomainAdmin,
		Enabled:        true,
		MailboxEnabled: false,
		Authenticated:  true,
	}

	issued, err := m.IssueAccess(admin, "sess-1", "")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	got, _, err := m.Parse(issued.Token, token.TypeAccess)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.MailboxEnabled {
		t.Error("a domain admin's token claims it can hold mail")
	}
	if err := got.Can(policy.ActionIMAP); !policy.IsDenied(err, policy.CodeNoMailbox) {
		t.Errorf("a policy check on a parsed domain admin allowed IMAP: %v", err)
	}
	// Administration is still permitted.
	if err := got.Can(policy.ActionAdminister); err != nil {
		t.Errorf("a parsed domain admin could not administer: %v", err)
	}
}

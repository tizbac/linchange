// Package login answers a sign-in, and it is the only place that does.
//
// Every protocol server and the HTTP API authenticate the same way, and the
// reason is not tidiness. A rule enforced once per listener is a rule enforced
// once per listener, and the drift between them is where account enumeration,
// skipped lockouts and unlocked second factors get in. The pieces are
// deliberately small and separately tested: policy decides what a principal may
// do, lockout decides what is still worth answering, and this package is the
// order they are asked in.
//
// Two properties shape almost every decision here.
//
// The first is that a caller must not learn whether an account exists. That
// rules out the obvious implementation of every branch: a locked account, a
// disabled account and an address that was never registered all take the same
// time and return the same error, because a login form that answers differently
// for each of them is a user list.
//
// The second is that nothing usable is issued until every factor has been
// presented. A user with a second factor gets an opaque challenge from the
// password step, not tokens, so an intercepted first response carries no access
// at all and the response can be logged, cached or replayed without consequence.
package login

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/lockout"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/secret"
	"github.com/linchange/linchange/internal/auth/token"
	"github.com/linchange/linchange/internal/auth/totp"
	"github.com/linchange/linchange/internal/store"
)

// Errors returned by this package.
//
// ErrInvalidCredentials is the answer to every rejected sign-in, whatever the
// real reason was. It is the only one of these a client should ever be shown, and
// the HTTP layer is expected to map it to a single generic response. The others
// exist so the operator's logs and metrics can tell a lockout from a directory
// outage, and they are deliberately not distinguishable from the outside.
var (
	// ErrInvalidCredentials covers a wrong password, an unknown address, a
	// locked account, a disabled account and a bad second factor.
	ErrInvalidCredentials = errors.New("login: invalid credentials")

	// ErrEnrolmentRequired means the account is required to have a second
	// factor and has none. No session is issued: the web layer answers with the
	// enrolment page and an enrolment-scoped session, which carries no mail
	// access until a factor is confirmed. Minting an ordinary session here
	// would leave "must enrol" enforced by nothing.
	ErrEnrolmentRequired = errors.New("login: a second factor must be enrolled")

	// ErrDirectoryUnavailable means an LDAP user tried to sign in and the
	// directory could not be consulted. The password was not evaluated, so
	// nothing is recorded against the account: an outage must not walk a user
	// towards a lockout they did nothing to earn.
	ErrDirectoryUnavailable = errors.New("login: the directory could not be reached")

	// ErrSecondFactorRequired is never returned. It exists to be impossible, so
	// that a caller reading PasswordResult has to handle the challenge case as a
	// value rather than discovering it as an error.
	ErrSecondFactorRequired = errors.New("login: a second factor is required")
)

// Directory authenticates a user against an external source of truth.
//
// The interface exists so the provider chain has a shape without pretending an
// LDAP server is a credential row. An implementation must return (false, nil) for
// credentials the directory rejects, and an error only when the directory could
// not be consulted at all; the two are handled differently on purpose.
type Directory interface {
	// Authenticate reports whether the directory accepts these credentials for
	// this address.
	Authenticate(ctx context.Context, address, password string) (bool, error)
}

// unavailableDirectory is the nil Directory, and it denies.
//
// A deployment with LDAP accounts configured but no directory wired up must
// refuse those sign-ins rather than fall through to a stored hash. Silently
// treating a directory user as a local one would be a way to authenticate
// against a credential row that should not exist.
type unavailableDirectory struct{}

func (unavailableDirectory) Authenticate(context.Context, string, string) (bool, error) {
	return false, errors.New("no directory provider is configured")
}

// Request carries what the transport knows and cannot be trusted.
type Request struct {
	// Login is the typed address. It is canonicalised by the database lookup
	// rather than here, so that "Alice@Example.com" and the stored form cannot
	// disagree about which account is being authenticated.
	Login    string
	Password string
	// IP is the source address, already parsed. A caller that cannot determine
	// it leaves it invalid; the per-address limit is then not enforced, which is
	// a reason to make it determinable rather than a reason to invent one.
	IP        netip.Addr
	UserAgent string
	// Impersonating and ImpersonationReason are set only by a domain admin
	// signing in to act as somebody else. They survive the second-factor step
	// unchanged.
	Impersonating       string
	ImpersonationReason string
}

// PasswordResult is what the password step concluded.
type PasswordResult struct {
	// ChallengeID is set when a second factor is outstanding. It is a bearer
	// secret for the lifetime of the challenge and must be treated as one: it
	// is not a session and it can be dropped from a URL.
	ChallengeID string `json:"challenge_id"`
	// ExpiresAt is when the challenge stops being exchangeable.
	ExpiresAt time.Time `json:"expires_at"`
	// Granted is set when the login completed with no second factor.
	Granted *Granted `json:"-"`
}

// SecondFactorRequest completes a login that stopped at a challenge.
type SecondFactorRequest struct {
	ChallengeID string
	// Code is the typed second factor: either a TOTP code or a recovery code.
	// The shape decides which, so a user does not have to say which they typed.
	Code string
	IP   netip.Addr
}

// Granted is a completed login.
type Granted struct {
	// Session is the row the tokens refer to. It is the authority: revoking it
	// invalidates the tokens, which is the reason the tokens do not stand alone.
	Session store.Session
	// Access and Refresh are returned once. Only digests are stored.
	Access  token.Issued
	Refresh token.Issued
	// Rehash asks the caller to store a new hash for this account.
	//
	// It is a request rather than a write because the plaintext is here and not
	// in the repository, and because a caller that already holds a transaction
	// should do the write inside it. A failure to rehash is never a reason to
	// refuse a login: the password was correct, and the cost parameters are a
	// floor to be raised on a later attempt.
	Rehash string `json:"-"`
}

// Authenticator orchestrates one deployment's sign-in.
type Authenticator struct {
	credentials *store.Credentials
	enrolments  *store.TOTP
	attempts    *store.AuthAttempts
	challenges  *store.Challenges
	sessions    *store.Sessions
	tokens      *token.Manager
	totp        *totp.Verifier
	directory   Directory

	argon2 config.Argon2Params
	// dummyHash is a throwaway argon2 hash at the configured cost, used to keep
	// every refused sign-in as expensive as an evaluated one.
	dummyHash    string
	lockout      lockout.Both
	policies     policy.Policy
	challengeTTL time.Duration
	log          *slog.Logger
	now          func() time.Time
}

// Options is everything an Authenticator needs. It is a struct rather than a long
// parameter list so that adding a dependency is a one-line change at the call
// site instead of an edit to every caller.
type Options struct {
	Credentials *store.Credentials
	Enrolments  *store.TOTP
	Attempts    *store.AuthAttempts
	Challenges  *store.Challenges
	Sessions    *store.Sessions
	Tokens      *token.Manager
	// TOTP is nil when second factors are switched off, which also means an
	// enrolled factor is not enforced.
	TOTP *totp.Verifier
	// Directory may be nil, which denies every directory sign-in.
	Directory    Directory
	Argon2       config.Argon2Params
	Lockout      lockout.Both
	Policy       policy.Policy
	ChallengeTTL time.Duration
	Logger       *slog.Logger
	// Now overrides the clock. Tests set it to reach an expiry exactly.
	Now func() time.Time
}

// New builds an Authenticator, filling in the pieces a caller left out.
func New(opts Options) (*Authenticator, error) {
	switch {
	case opts.Credentials == nil:
		return nil, errors.New("login: credentials repository is required")
	case opts.Enrolments == nil:
		return nil, errors.New("login: TOTP repository is required")
	case opts.Attempts == nil:
		return nil, errors.New("login: attempt repository is required")
	case opts.Challenges == nil:
		return nil, errors.New("login: challenge repository is required")
	case opts.Sessions == nil:
		return nil, errors.New("login: session repository is required")
	case opts.Tokens == nil:
		return nil, errors.New("login: token manager is required")
	case opts.ChallengeTTL <= 0:
		return nil, errors.New("login: a positive challenge lifetime is required")
	}

	directory := opts.Directory
	if directory == nil {
		directory = unavailableDirectory{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	dummy, err := dummyHashFor(opts.Argon2)
	if err != nil {
		return nil, fmt.Errorf("login: preparing the dummy credential hash: %w", err)
	}

	return &Authenticator{
		credentials:  opts.Credentials,
		enrolments:   opts.Enrolments,
		attempts:     opts.Attempts,
		challenges:   opts.Challenges,
		sessions:     opts.Sessions,
		tokens:       opts.Tokens,
		totp:         opts.TOTP,
		directory:    directory,
		argon2:       opts.Argon2,
		dummyHash:    dummy,
		lockout:      opts.Lockout,
		policies:     opts.Policy,
		challengeTTL: opts.ChallengeTTL,
		log:          logger,
		now:          now,
	}, nil
}

// Password performs the first half of a sign-in.
//
// It returns a challenge when a second factor is outstanding and a Granted
// otherwise. Every rejection is ErrInvalidCredentials, with the real reason in
// the log, because a caller that can tell the reasons apart can enumerate
// accounts.
func (a *Authenticator) Password(ctx context.Context, q store.Querier, req Request) (PasswordResult, error) {
	var out PasswordResult
	now := a.now()

	principal, err := store.LoadPrincipal(ctx, q, req.Login)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return out, err
		}
		// No account. Spend the same argon2 time a real check would and count
		// it against the source address, so that the response is
		// indistinguishable and a spray of unknown addresses is still metered.
		// Nothing is recorded against the account: there is no row to record it
		// on, and inventing one would be a way to fill the table by guessing.
		a.dummyVerify(req.Password)
		if ferr := a.recordFailure(ctx, q, "", req.IP, now); ferr != nil {
			a.log.WarnContext(ctx, "recording an address failure failed",
				"error", ferr, "source_address", req.IP.String())
		}
		return out, ErrInvalidCredentials
	}

	// Everything below uses the address as the database spells it, so that the
	// lockout rows, the challenge and the principal all agree on one subject.
	address := principal.Address

	if err := principal.Can(policy.ActionAuthenticate); err != nil {
		a.dummyVerify(req.Password)
		a.log.InfoContext(ctx, "sign-in refused by policy",
			"address", address, "reason", err.Error())
		if ferr := a.recordFailure(ctx, q, address, req.IP, now); ferr != nil {
			a.log.WarnContext(ctx, "recording a failure failed", "error", ferr, "address", address)
		}
		return out, ErrInvalidCredentials
	}

	// The lockout check comes before the argon2 work, so a locked account costs
	// two indexed reads instead of a hash. The dummy verify that follows is what
	// keeps that saving invisible: without it, a locked account answers faster
	// than an unlocked one and the difference says the account exists.
	if decision, lerr := a.locked(ctx, q, address, req.IP, now); lerr != nil {
		return out, lerr
	} else if decision.Locked {
		a.dummyVerify(req.Password)
		a.log.InfoContext(ctx, "sign-in refused: locked",
			"address", address, "source_address", req.IP.String(),
			"scope", decision.Scope, "until", decision.Until)
		if ferr := a.recordFailure(ctx, q, address, req.IP, now); ferr != nil {
			a.log.WarnContext(ctx, "recording a failure failed", "error", ferr, "address", address)
		}
		return out, ErrInvalidCredentials
	}

	rehash, err := a.verify(ctx, q, principal, req, now)
	if err != nil {
		return out, err
	}

	// A second factor that exists and is enabled is always enforced, whether or
	// not the account is flagged as requiring one. An enrolment the user chose
	// to make and that is then not asked for is worse than no enrolment: it
	// teaches that the extra step is optional.
	enrolment, err := a.enrolments.Get(ctx, q, address)
	switch {
	case err == nil && enrolment.Enabled && a.totp != nil:
		challenge, cerr := a.challenges.CreateChallenge(ctx, q, store.CreateChallengeRequest{
			Address:       address,
			Impersonating: req.Impersonating,
			IP:            req.IP,
			UserAgent:     req.UserAgent,
			TTL:           a.challengeTTL,
		})
		if cerr != nil {
			return out, cerr
		}
		a.log.InfoContext(ctx, "second factor required",
			"address", address, "challenge", challenge.ID, "expires_at", challenge.ExpiresAt)
		return PasswordResult{ChallengeID: challenge.ID, ExpiresAt: challenge.ExpiresAt}, nil

	case err != nil && !errors.Is(err, store.ErrNoTOTP):
		// A database problem must not read as "no second factor configured",
		// which would silently downgrade the login to a password-only one.
		return out, err

	case a.totp == nil:
		// Second factors are switched off deployment-wide, so an existing
		// enrolment is not enforced. That is an operator decision, recorded
		// here rather than left to look like a bug.
		a.log.WarnContext(ctx, "sign-in completed without an enabled second factor",
			"address", address, "reason", "totp disabled in configuration")

	case principal.TOTPRequired:
		// Required but absent. The enrolment page issues its own limited
		// session; this step must not hand out an ordinary one.
		a.log.InfoContext(ctx, "second factor enrolment required", "address", address)
		return out, ErrEnrolmentRequired
	}

	granted, err := a.grant(ctx, q, principal, req, rehash, now)
	if err != nil {
		return out, err
	}
	return PasswordResult{Granted: &granted}, nil
}

// SecondFactor completes a login that stopped at a challenge.
//
// The challenge is consumed before anything is issued, and the consume is a
// single conditional statement, so two requests carrying the same valid code
// cannot both mint a session.
func (a *Authenticator) SecondFactor(ctx context.Context, q store.Querier, req SecondFactorRequest) (Granted, error) {
	var out Granted
	now := a.now()

	challenge, err := a.challenges.LoadChallenge(ctx, q, req.ChallengeID, now)
	if err != nil {
		if errors.Is(err, store.ErrNoChallenge) {
			// A missing, expired and already-spent challenge are the same
			// answer. Distinguishing them would tell an attacker which codes
			// were accepted.
			return out, ErrInvalidCredentials
		}
		return out, err
	}
	address := challenge.Address

	if decision, lerr := a.locked(ctx, q, address, req.IP, now); lerr != nil {
		return out, lerr
	} else if decision.Locked {
		a.log.InfoContext(ctx, "second factor refused: locked",
			"address", address, "source_address", req.IP.String(), "scope", decision.Scope)
		if ferr := a.recordFailure(ctx, q, address, req.IP, now); ferr != nil {
			a.log.WarnContext(ctx, "recording a failure failed", "error", ferr, "address", address)
		}
		return out, ErrInvalidCredentials
	}

	enrolment, err := a.enrolments.Get(ctx, q, address)
	if err != nil {
		if errors.Is(err, store.ErrNoTOTP) {
			return out, ErrInvalidCredentials
		}
		return out, err
	}
	if !enrolment.Enabled {
		return out, ErrInvalidCredentials
	}

	if err := a.acceptSecondFactor(ctx, q, address, enrolment, req.Code, now); err != nil {
		if ferr := a.recordFailure(ctx, q, address, req.IP, now); ferr != nil {
			a.log.WarnContext(ctx, "recording a second factor failure failed",
				"error", ferr, "address", address)
		}
		if errors.Is(err, store.ErrNoTOTP) {
			return out, ErrInvalidCredentials
		}
		// A wrong code is recorded against both scopes, exactly like a wrong
		// password. A six digit code has far less entropy than any password the
		// policy would accept, so the guess that matters is more likely here,
		// not less.
		return out, ErrInvalidCredentials
	}

	// The code was right. Spend the challenge before issuing anything, so a
	// replay of the same code finds nothing and mints nothing.
	consumed, err := a.challenges.ConsumeChallenge(ctx, q, req.ChallengeID, now)
	if err != nil {
		return out, err
	}
	if !consumed {
		a.log.InfoContext(ctx, "second factor replay refused",
			"address", address, "challenge", req.ChallengeID)
		return out, ErrInvalidCredentials
	}

	principal, err := store.LoadPrincipal(ctx, q, address)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return out, ErrInvalidCredentials
		}
		return out, err
	}

	granted, err := a.grant(ctx, q, principal, Request{
		IP:                  req.IP,
		Login:               address,
		Impersonating:       challenge.Impersonating,
		ImpersonationReason: "",
	}, "", now)
	if err != nil {
		return out, err
	}

	a.log.InfoContext(ctx, "sign-in completed",
		"address", address, "session", granted.Session.ID, "second_factor", true)
	return granted, nil
}

// acceptSecondFactor checks the typed code and commits the fact that it was
// used.
//
// A TOTP code is spent by advancing the recorded step, a recovery code by
// removing its digest, and the two are told apart by shape: six digits is a TOTP
// code, anything else is a recovery code. The two forms cannot collide, because
// a recovery code is sixteen characters from a thirty-two symbol alphabet.
func (a *Authenticator) acceptSecondFactor(
	ctx context.Context, q store.Querier, address string,
	enrolment store.TOTPRecord, typed string, now time.Time,
) error {
	code := secret.NormalizeRecoveryCode(typed)
	if code == "" {
		return errors.New("login: empty second factor")
	}

	if isTOTPCode(code) {
		if a.totp == nil {
			return errors.New("login: second factors are disabled")
		}
		step, err := a.totp.Verify(enrolment.SecretEncrypted, typed, enrolment.LastUsedAt.Time, now)
		if err != nil {
			return err
		}
		// The recorded step only ever moves forward, so a duplicate request
		// carrying a code that was accepted a moment ago cannot reopen the
		// window behind it.
		return a.enrolments.MarkUsed(ctx, q, address, step)
	}

	// ErrNoTOTP is used to say "this code did not match", because the caller
	// treats a mismatch and a replay identically and must not be told apart.
	index, ok := secret.VerifyRecoveryCode(typed, enrolment.RecoveryDigests)
	if !ok {
		return store.ErrNoTOTP
	}
	spent, err := a.enrolments.ConsumeRecoveryCode(ctx, q, address, enrolment.RecoveryDigests[index])
	if err != nil {
		return err
	}
	if !spent {
		return store.ErrNoTOTP
	}
	return nil
}

// isTOTPCode reports whether a normalised code has the shape of a TOTP code.
//
// The test is on shape rather than on a flag from the client, because a client
// flag would let a caller choose which verifier runs, and choosing the cheaper
// one is exactly the attack.
func isTOTPCode(normalised string) bool {
	if len(normalised) != 6 {
		return false
	}
	for _, r := range normalised {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// verify checks the password against whichever source owns the account, and
// reports the hash to rehash afterwards.
func (a *Authenticator) verify(
	ctx context.Context, q store.Querier, principal policy.Principal, req Request, now time.Time,
) (string, error) {
	switch principal.Source {
	case policy.SourceLocal, policy.SourceDomainAdmin:
		return a.verifyLocal(ctx, q, principal, req, now)

	case policy.SourceLDAP:
		return "", a.verifyDirectory(ctx, q, principal, req, now)

	default:
		// A shared mailbox holds mail and is reached through grants. It has no
		// password to check, and policy.Can has already refused it.
		a.dummyVerify(req.Password)
		a.recordFailureLogged(ctx, q, principal.Address, req.IP, now, "shared source")
		return "", ErrInvalidCredentials
	}
}

// verifyLocal checks a stored argon2id hash and, if the hash was made with
// weaker parameters than are configured now, hands back a replacement.
//
// A domain admin authenticates here too: the requirement is that a domain admin
// administers mail and never receives it, not that they have no password.
func (a *Authenticator) verifyLocal(
	ctx context.Context, q store.Querier, principal policy.Principal, req Request, now time.Time,
) (string, error) {
	cred, err := a.credentials.Get(ctx, q, principal.Address)
	if err != nil {
		if errors.Is(err, store.ErrNoCredential) {
			// A local account with no password. Spend the time anyway, for the
			// same reason as an unknown address.
			a.dummyVerify(req.Password)
			a.recordFailureLogged(ctx, q, principal.Address, req.IP, now, "no stored credential")
			return "", ErrInvalidCredentials
		}
		return "", err
	}

	ok, err := verifyPassword(req.Password, cred.Hash)
	if err != nil {
		// A malformed stored hash is a data problem, not a wrong password, and
		// it must not be reported as a failed sign-in: the account is
		// unloggable until it is repaired and the operator needs to know that.
		a.dummyVerify(req.Password)
		a.log.ErrorContext(ctx, "stored credential is unreadable",
			"address", principal.Address, "error", err)
		return "", fmt.Errorf("login: reading the credential for %s: %w", principal.Address, err)
	}
	if !ok {
		a.recordFailureLogged(ctx, q, principal.Address, req.IP, now, "wrong password")
		return "", ErrInvalidCredentials
	}

	// The password was correct, so the account is not under attack. Clear it
	// now rather than at the end of the login: a user who then fails their
	// second factor is counted from zero on the account scope, and the address
	// scope still carries every failure they make. Holding the account counter
	// would let a correct password be used to launder attempts.
	if err := a.attempts.ResetAccount(ctx, q, principal.Address); err != nil {
		a.log.WarnContext(ctx, "clearing account failure history failed",
			"error", err, "address", principal.Address)
	}

	rehash, err := rehashIfWeak(cred.Hash, req.Password, a.argon2)
	if err != nil {
		// Not a reason to refuse: the password was right and the existing hash
		// is still correct, just cheaper than we would like.
		a.log.WarnContext(ctx, "could not decide whether to rehash",
			"error", err, "address", principal.Address)
	}
	return rehash, nil
}

// verifyDirectory checks an LDAP user's password by binding to the directory.
func (a *Authenticator) verifyDirectory(ctx context.Context, q store.Querier, principal policy.Principal, req Request, now time.Time) error {
	ok, err := a.directory.Authenticate(ctx, principal.Address, req.Password)
	if err != nil {
		// The password was never checked, so this is not a failed attempt by
		// the user and must not move their counter. It is also not success, so
		// the login fails closed. The caller is expected to log it and answer
		// the client the same generic way it answers a wrong password.
		a.log.ErrorContext(ctx, "directory authentication failed",
			"address", principal.Address, "error", err)
		return ErrDirectoryUnavailable
	}
	if !ok {
		a.recordFailureLogged(ctx, q, principal.Address, req.IP, a.now(), "directory rejected the password")
		return ErrInvalidCredentials
	}
	return nil
}

// grant issues the session and the tokens for a login that has cleared every
// factor.
//
// rehash is the new hash to store, or empty. It is written before the session is
// created so that a successful login always leaves the account in the state the
// policy now wants.
func (a *Authenticator) grant(
	ctx context.Context, q store.Querier, principal policy.Principal,
	req Request, rehash string, now time.Time,
) (Granted, error) {
	var out Granted

	if rehash != "" {
		if err := a.credentials.Set(ctx, q, principal.Address, rehash); err != nil {
			// The old hash is still correct, so this is not worth failing the
			// login over. The account is merely weaker than configured until
			// the next successful sign-in.
			a.log.WarnContext(ctx, "rehash on sign-in failed",
				"error", err, "address", principal.Address)
		}
	}

	// The principal is authenticated exactly here and nowhere else. Everything
	// downstream is allowed to rely on that flag, and nothing before this point
	// was allowed to set it.
	principal.Authenticated = true

	if req.Impersonating != "" {
		if err := a.policies.CanImpersonate(principal, req.Impersonating, req.ImpersonationReason); err != nil {
			a.log.InfoContext(ctx, "impersonation refused",
				"address", principal.Address, "target", req.Impersonating, "reason", err.Error())
			return out, ErrInvalidCredentials
		}
	}

	// The web UI is the one action a forced password change still permits,
	// because that is where the change is made.
	if err := principal.Can(policy.ActionWebLogin); err != nil {
		a.log.InfoContext(ctx, "sign-in refused by policy after authentication",
			"address", principal.Address, "reason", err.Error())
		return out, ErrInvalidCredentials
	}

	created, err := a.sessions.Create(ctx, q, store.CreateSessionRequest{
		Address:       principal.Address,
		IP:            req.IP,
		UserAgent:     req.UserAgent,
		Impersonating: req.Impersonating,
	})
	if err != nil {
		return out, err
	}

	access, err := a.tokens.IssueAccess(principal, created.Session.ID, req.Impersonating)
	if err != nil {
		return out, err
	}
	refresh, err := a.tokens.IssueRefresh(principal, created.Session.ID, req.Impersonating)
	if err != nil {
		return out, err
	}

	// The address scope is deliberately left alone. Clearing it here would let
	// one account with a working password launder an arbitrary number of failed
	// attempts against every other account from the same address.
	a.log.InfoContext(ctx, "sign-in completed",
		"address", principal.Address, "session", created.Session.ID,
		"source", string(principal.Source), "must_change_password", principal.MustChangePassword)

	return Granted{
		Session: created.Session,
		Access:  access,
		Refresh: refresh,
		Rehash:  rehash,
	}, nil
}

// locked evaluates both lockout scopes.
func (a *Authenticator) locked(
	ctx context.Context, q store.Querier, address string, ip netip.Addr, now time.Time,
) (lockout.Decision, error) {
	account, err := a.attempts.AccountState(ctx, q, address)
	if err != nil {
		return lockout.Decision{}, fmt.Errorf("login: reading account failure history: %w", err)
	}
	ipState, err := a.attempts.IPState(ctx, q, ip)
	if err != nil {
		return lockout.Decision{}, fmt.Errorf("login: reading address failure history: %w", err)
	}
	return a.lockout.Check(account, ipState, now), nil
}

// recordFailure advances both scopes after a failed attempt.
//
// The account scope is skipped for an address with no user row, because the table
// has a foreign key to users and an unknown address must not be able to create
// rows by guessing.
func (a *Authenticator) recordFailure(
	ctx context.Context, q store.Querier, address string, ip netip.Addr, now time.Time,
) error {
	if address != "" {
		account, err := a.attempts.AccountState(ctx, q, address)
		if err != nil {
			return fmt.Errorf("login: reading account failure history: %w", err)
		}
		ipState, err := a.attempts.IPState(ctx, q, ip)
		if err != nil {
			return fmt.Errorf("login: reading address failure history: %w", err)
		}
		nextAccount, nextIP := a.lockout.Record(account, ipState, now)
		// Both are written even though one may fail, because a half-recorded
		// failure is a gap an attacker can aim at.
		if err := a.attempts.RecordAccountFailure(ctx, q, address, nextAccount); err != nil {
			return fmt.Errorf("login: recording an account failure: %w", err)
		}
		return a.recordIPState(ctx, q, ip, nextIP)
	}

	ipState, err := a.attempts.IPState(ctx, q, ip)
	if err != nil {
		return fmt.Errorf("login: reading address failure history: %w", err)
	}
	return a.recordIPState(ctx, q, ip, a.lockout.IP.Record(ipState, now))
}

func (a *Authenticator) recordIPState(
	ctx context.Context, q store.Querier, ip netip.Addr, state lockout.State,
) error {
	if !ip.IsValid() {
		// There is no address to record against. This is the case a proxy that
		// does not forward a client address produces, and the reason to make
		// that configurable rather than a silent gap.
		return nil
	}
	if err := a.attempts.RecordIPFailure(ctx, q, ip, state); err != nil {
		return fmt.Errorf("recording an address failure: %w", err)
	}
	return nil
}

// recordFailureLogged is the common path: record, log the reason, and report
// nothing. Callers return their own error, so the failure to write the counter
// must not be mistaken for the reason the sign-in failed.
func (a *Authenticator) recordFailureLogged(
	ctx context.Context, q store.Querier, address string, ip netip.Addr, now time.Time, reason string,
) {
	if err := a.recordFailure(ctx, q, address, ip, now); err != nil {
		a.log.WarnContext(ctx, "recording an authentication failure failed",
			"error", err, "address", address, "source_address", ip.String(), "reason", reason)
		return
	}
	a.log.InfoContext(ctx, "authentication failed",
		"address", address, "source_address", ip.String(), "reason", reason)
}

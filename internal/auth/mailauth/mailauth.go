// Package mailauth authenticates the login presented by a mail client.
//
// IMAP, SMTP submission and POP3 all ask the same question — is this person who
// they say they are, and may they use this protocol — and the rules for answering
// it live here, once. Each listener used to carry its own copy of the answer, and
// the copies had drifted: they skipped the policy checks, skipped the second
// factor question, and, worst of all, treated "this account has no stored
// password" as "any password will do". That last one was an open door, because
// every directory-backed account is stored without a local password.
//
// The split between this package and the web login path is deliberate. Both
// verify a password against whichever source owns the account, but a browser can
// be shown a second factor and a challenge, can be counted against a lockout, and
// can be issued a session; none of that belongs in an IMAP or SMTP exchange. So
// the two share the rule and not the ceremony.
package mailauth

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/secret"
	"github.com/linchange/linchange/internal/store"
)

// Directory authenticates an account against an external source of truth.
//
// An implementation returns (false, nil) for credentials the directory refuses,
// and an error only when the directory could not be consulted at all. The two are
// handled differently on purpose: a refusal is the user's mistake and may be
// counted, while an outage is nobody's and must fail closed without being blamed
// on anyone.
type Directory interface {
	Authenticate(ctx context.Context, address, password string) (bool, error)
}

// The outcomes of a verification. A listener answers its client the same way for
// both of them, so that the answer never reveals which one it was.
var (
	// ErrRejected means the credentials were refused: an unknown account, a
	// disabled one, a wrong password, or an account that may not use this
	// protocol.
	ErrRejected = errors.New("mailauth: the credentials were refused")
	// ErrUnavailable means the directory could not be reached, so the password
	// was never checked. There is no other source to fall back to and the
	// answer is still no.
	ErrUnavailable = errors.New("mailauth: the directory could not be consulted")
)

// Verifier checks mail protocol logins against an open database.
type Verifier struct {
	db          *sqlx.DB
	log         *slog.Logger
	directory   Directory
	credentials *store.Credentials
	tokens      *store.ApplicationTokens
	workgroups  *store.Workgroups
	totp        *store.TOTP
	// allowPassword lets the account's main password sign in. It is false by
	// default in the server: clients use application passwords, which can be
	// revoked without exposing the password that changes the account.
	allowPassword bool
	// failures throttles repeated wrong passwords for one login. It is nil
	// when protocol_lockout is disabled.
	failures *failureLimiter
}

// Options is what a deployment decides about protocol sign-ins.
type Options struct {
	// Lockout throttles repeated failures on the mail protocols.
	Lockout config.LockoutScope
	// AllowMainPassword lets the account password sign in to the protocols.
	// Application passwords are always accepted.
	AllowMainPassword bool
}

// New returns a Verifier without a protocol lockout. Tests and embedders that
// do not have a configuration use it; the server uses NewWithOptions.
func New(db *sqlx.DB, log *slog.Logger, directory Directory) *Verifier {
	return NewWithOptions(db, log, directory, Options{AllowMainPassword: true})
}

// NewWithLockout returns a Verifier with the configured protocol lockout and
// the account password allowed, as the mail listeners had it before
// application passwords. The server uses NewWithOptions and its configuration.
func NewWithLockout(db *sqlx.DB, log *slog.Logger, directory Directory, lockout config.LockoutScope) *Verifier {
	return NewWithOptions(db, log, directory, Options{Lockout: lockout, AllowMainPassword: true})
}

// NewWithOptions returns a Verifier with the deployment's decisions. A nil
// directory denies: a deployment with directory-backed accounts but no
// directory wired up must refuse those logins rather than admit anyone whose
// account happens to have no local password.
func NewWithOptions(db *sqlx.DB, log *slog.Logger, directory Directory, opts Options) *Verifier {
	if log == nil {
		log = slog.Default()
	}
	if directory == nil {
		directory = unavailableDirectory{}
	}
	v := &Verifier{
		db:            db,
		log:           log,
		directory:     directory,
		credentials:   store.NewCredentials(),
		tokens:        store.NewApplicationTokens(),
		workgroups:    store.NewWorkgroups(),
		totp:          store.NewTOTP(),
		allowPassword: opts.AllowMainPassword,
	}
	if opts.Lockout.Enabled() {
		v.failures = newFailureLimiter(opts.Lockout.MaxAttempts, opts.Lockout.Window, opts.Lockout.Duration)
	}
	return v
}

// applicationTokenPrefix names the credentials this server issues for mail
// clients. A presented password that starts with it is tried as one, never as
// the account password, so a wrong application password cannot fall back to
// the main one.
const applicationTokenPrefix = "lcapp_"

// splitApplicationToken returns the public prefix and the secret part of a
// credential that has the application-password shape.
func splitApplicationToken(pw string) (string, string, bool) {
	rest, ok := strings.CutPrefix(pw, applicationTokenPrefix)
	if !ok {
		return "", "", false
	}
	prefix, secretPart, ok := strings.Cut(rest, "_")
	if !ok || prefix == "" || secretPart == "" {
		return "", "", false
	}
	return prefix, secretPart, true
}

// unavailableDirectory stands in for a nil Directory and refuses.
type unavailableDirectory struct{}

func (unavailableDirectory) Authenticate(context.Context, string, string) (bool, error) {
	return false, errors.New("mailauth: no directory is configured")
}

// failureLimiter is an in-memory, per-login failure counter. It is deliberately
// not the web lockout: that one is backed by rows because it protects a
// session-issuing endpoint, while this one protects listeners that cannot
// present a second factor and only need to stop a burst of guesses.
type failureLimiter struct {
	mu       sync.Mutex
	entries  map[string]failureEntry
	max      int
	window   time.Duration
	duration time.Duration
	now      func() time.Time
}

type failureEntry struct {
	count       int
	windowStart time.Time
	until       time.Time
}

const maxFailureEntries = 4096

func newFailureLimiter(max int, window, duration time.Duration) *failureLimiter {
	return &failureLimiter{
		entries:  make(map[string]failureEntry),
		max:      max,
		window:   window,
		duration: duration,
		now:      time.Now,
	}
}

// locked reports whether the login is currently in a cooldown.
func (l *failureLimiter) locked(login string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[login]
	if !ok {
		return false
	}
	now := l.now()
	if now.Before(entry.until) {
		return true
	}
	if entry.windowStart.IsZero() || now.Sub(entry.windowStart) > l.window {
		delete(l.entries, login)
	}
	return false
}

// record counts one refused password. Reaching the threshold starts the
// cooldown; a failure during a cooldown does not extend it.
func (l *failureLimiter) record(login string) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	entry := l.entries[login]
	if !entry.until.IsZero() && now.Before(entry.until) {
		return
	}
	if entry.windowStart.IsZero() || now.Sub(entry.windowStart) > l.window {
		entry = failureEntry{windowStart: now}
	}
	entry.count++
	if entry.count >= l.max {
		entry.count = 0
		entry.until = now.Add(l.duration)
	}
	l.entries[login] = entry

	if len(l.entries) > maxFailureEntries {
		l.prune(now)
	}
}

// clear forgets a login after a successful authentication.
func (l *failureLimiter) clear(login string) {
	l.mu.Lock()
	delete(l.entries, login)
	l.mu.Unlock()
}

// prune drops entries that can no longer lock anybody. If a spray still fills
// the table with distinct unknown logins, the oldest entries are dropped: the
// limiter must not itself become an unbounded memory target.
func (l *failureLimiter) prune(now time.Time) {
	for login, entry := range l.entries {
		if entry.until.IsZero() && now.Sub(entry.windowStart) > l.window {
			delete(l.entries, login)
			continue
		}
		if !entry.until.IsZero() && !now.Before(entry.until) {
			delete(l.entries, login)
		}
	}
	for login := range l.entries {
		if len(l.entries) <= maxFailureEntries/2 {
			return
		}
		delete(l.entries, login)
	}
}

// Authenticate verifies login and password for a client using the protocol named
// by action, and returns the authenticated principal on success.
//
// Every failure is either ErrRejected or ErrUnavailable. An unexpected error is
// returned wrapped rather than flattened, so the operator sees a real problem in
// the log even though the client is told only that the login failed.
func (v *Verifier) Authenticate(
	ctx context.Context, login, pw string, action policy.Action,
) (policy.Principal, error) {
	key := strings.ToLower(strings.TrimSpace(login))
	if v.failures != nil && v.failures.locked(key) {
		// Keep the answer indistinguishable from a wrong password and spend
		// the same time a real comparison would.
		password.DummyVerify(pw)
		v.log.InfoContext(ctx, "mail login refused: locked",
			"address", key, "protocol", string(action))
		return policy.Principal{}, ErrRejected
	}
	refuse := func() {
		if v.failures != nil {
			v.failures.record(key)
		}
	}

	// An empty password is refused before anything is consulted. A directory
	// asked to bind with no password performs an anonymous bind, which
	// succeeds without proving anything, so this case must never reach one.
	if pw == "" {
		password.DummyVerify(pw)
		refuse()
		return policy.Principal{}, ErrRejected
	}

	principal, err := store.LoadPrincipal(ctx, v.db, strings.TrimSpace(login))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Spend the time a real comparison would take, so that a login
			// name nobody holds is not measurably faster than one that is.
			password.DummyVerify(pw)
			refuse()
			return policy.Principal{}, ErrRejected
		}
		return policy.Principal{}, fmt.Errorf("mailauth: loading %s: %w", login, err)
	}

	// May this identity authenticate at all? This is the question asked of an
	// identity nobody has proved yet, so it is the only one asked now.
	if err := principal.Can(policy.ActionAuthenticate); err != nil {
		password.DummyVerify(pw)
		v.log.InfoContext(ctx, "mail login refused by policy",
			"address", principal.Address, "protocol", string(action), "code", denialCode(err))
		refuse()
		return policy.Principal{}, ErrRejected
	}

	if prefix, secretPart, looksLikeToken := splitApplicationToken(pw); looksLikeToken {
		if err := v.verifyApplicationToken(ctx, principal.Address, prefix, secretPart); err != nil {
			if errors.Is(err, ErrRejected) {
				refuse()
			}
			return policy.Principal{}, err
		}
	} else {
		allowed, allowedErr := v.mainPasswordAllowed(ctx, principal.Address)
		if allowedErr != nil {
			v.log.ErrorContext(ctx, "mail login refused: could not read the organization's legacy-auth setting",
				"address", principal.Address, "protocol", string(action), "error", allowedErr)
			password.DummyVerify(pw)
			refuse()
			return policy.Principal{}, ErrRejected
		}
		if !allowed {
			// The account password is for the browser, where a second factor
			// can be shown. A mail client uses an application password made
			// from the account page, so a compromised computer never holds the
			// credential that can change the account.
			password.DummyVerify(pw)
			refuse()
			v.log.InfoContext(ctx, "mail login refused: the account password is not accepted here",
				"address", principal.Address, "protocol", string(action))
			return policy.Principal{}, ErrRejected
		}
		if err := v.verifyPassword(ctx, principal, pw); err != nil {
			if errors.Is(err, ErrRejected) {
				refuse()
			}
			return policy.Principal{}, err
		}
	}

	// The identity is now proved, so it can be asked what it may do with mail.
	// A domain admin, a shared mailbox, an account with no mailbox and one that
	// must change its password are all refused here.
	principal.Authenticated = true
	if err := principal.CanMailProtocol(action); err != nil {
		if v.failures != nil {
			v.failures.clear(key)
		}
		v.log.InfoContext(ctx, "mail login refused by policy",
			"address", principal.Address, "protocol", string(action), "code", denialCode(err))
		return policy.Principal{}, ErrRejected
	}
	if v.failures != nil {
		v.failures.clear(key)
	}
	return principal, nil
}

// mainPasswordAllowed reports whether the account password may sign in to a
// mail protocol for this address. The organization's administrator decides
// for its people; an address in no organization follows the deployment's
// default, which is off.
func (v *Verifier) mainPasswordAllowed(ctx context.Context, address string) (bool, error) {
	// A second factor is a promise that a password alone is not enough. A mail
	// protocol that cannot present one never sees the account password, even
	// where the organization allows legacy authentication generally.
	enrolment, err := v.totp.Get(ctx, v.db, address)
	switch {
	case err == nil && enrolment.Enabled:
		return false, nil
	case err != nil && !errors.Is(err, store.ErrNoTOTP):
		return false, err
	}
	allowed, err := v.workgroups.LegacyMailAuthForAddress(ctx, v.db, address)
	if errors.Is(err, store.ErrNotFound) {
		return v.allowPassword, nil
	}
	if err != nil {
		return false, err
	}
	return allowed, nil
}

// verifyApplicationToken checks the secret of an application password against
// the row its public prefix names. A token for another account is refused: the
// login name and the token's owner must be the same person.
func (v *Verifier) verifyApplicationToken(ctx context.Context, address, prefix, secretPart string) error {
	row, err := v.tokens.Find(ctx, v.db, prefix)
	if err != nil {
		if errors.Is(err, store.ErrNoApplicationToken) {
			dummyApplicationToken(secretPart)
			return ErrRejected
		}
		return fmt.Errorf("mailauth: reading an application password: %w", err)
	}
	if !strings.EqualFold(row.Address, address) {
		dummyApplicationToken(secretPart)
		return ErrRejected
	}
	stored, err := hex.DecodeString(row.TokenHash)
	if err != nil || len(stored) == 0 {
		return fmt.Errorf("mailauth: the stored application password for %s is unreadable", address)
	}
	if !secret.Equal(secretPart, stored) {
		return ErrRejected
	}
	// Recording the use is a convenience for the owner, never a reason to
	// refuse an otherwise correct credential.
	if err := v.tokens.Touch(ctx, v.db, row.ID); err != nil {
		v.log.WarnContext(ctx, "could not record application-password use", "address", address, "error", err)
	}
	return nil
}

// dummyApplicationToken spends a comparison for a prefix nobody has, so a
// missing row is not measurably faster than a wrong secret.
func dummyApplicationToken(secretPart string) {
	_ = secret.Equal(secretPart, make([]byte, 32))
}

// verifyPassword checks pw against whichever source owns the account.
func (v *Verifier) verifyPassword(ctx context.Context, principal policy.Principal, pw string) error {
	switch principal.Source {
	case policy.SourceLocal:
		return v.verifyLocal(ctx, principal.Address, pw)

	case policy.SourceDomainAdmin:
		// A domain admin may hold a password set here or have one held by the
		// directory, depending on where the account was made. The presence of a
		// local credential decides which, exactly as on the web path.
		stored, err := v.hasStoredCredential(ctx, principal.Address)
		if err != nil {
			return err
		}
		if stored {
			return v.verifyLocal(ctx, principal.Address, pw)
		}
		return v.verifyDirectory(ctx, principal.Address, pw)

	case policy.SourceLDAP:
		return v.verifyDirectory(ctx, principal.Address, pw)

	default:
		// A shared mailbox holds no password of its own; it is reached through
		// grants. The policy has already refused it, and this refuses it again
		// in case that ever changes.
		password.DummyVerify(pw)
		return ErrRejected
	}
}

// verifyLocal checks a stored argon2id hash.
func (v *Verifier) verifyLocal(ctx context.Context, address, pw string) error {
	cred, err := v.credentials.Get(ctx, v.db, address)
	if err != nil {
		if errors.Is(err, store.ErrNoCredential) {
			// A local account with no password set cannot log in. This is the
			// case the protocol listeners used to read as success.
			password.DummyVerify(pw)
			return ErrRejected
		}
		return fmt.Errorf("mailauth: reading the credential for %s: %w", address, err)
	}
	match, err := password.Verify(pw, cred.Hash)
	if err != nil {
		// An unreadable hash is a data problem, not a wrong password. The
		// account cannot be admitted until it is repaired, and the operator
		// needs to see why, so the cause is kept rather than flattened.
		password.DummyVerify(pw)
		return fmt.Errorf("mailauth: the stored credential for %s is unreadable: %w", address, err)
	}
	if !match {
		return ErrRejected
	}
	return nil
}

// verifyDirectory checks the password against the configured directory.
func (v *Verifier) verifyDirectory(ctx context.Context, address, pw string) error {
	ok, err := v.directory.Authenticate(ctx, address, pw)
	if err != nil {
		// The password was never checked. There is nowhere else to check it,
		// so this fails closed; it is reported separately from a refusal so
		// that an outage is not mistaken for a user mistyping.
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if !ok {
		return ErrRejected
	}
	return nil
}

// hasStoredCredential reports whether this account has a password set here.
func (v *Verifier) hasStoredCredential(ctx context.Context, address string) (bool, error) {
	if _, err := v.credentials.Get(ctx, v.db, address); err != nil {
		if errors.Is(err, store.ErrNoCredential) {
			return false, nil
		}
		return false, fmt.Errorf("mailauth: reading the credential for %s: %w", address, err)
	}
	return true, nil
}

// denialCode extracts the stable reason from a policy refusal, for the log.
func denialCode(err error) string {
	var denied *policy.Denied
	if errors.As(err, &denied) {
		return denied.Code
	}
	return "unknown"
}

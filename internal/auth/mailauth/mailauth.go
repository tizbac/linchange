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
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/policy"
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
}

// New returns a Verifier. A nil directory denies: a deployment with
// directory-backed accounts but no directory wired up must refuse those logins
// rather than admit anyone whose account happens to have no local password.
func New(db *sqlx.DB, log *slog.Logger, directory Directory) *Verifier {
	if log == nil {
		log = slog.Default()
	}
	if directory == nil {
		directory = unavailableDirectory{}
	}
	return &Verifier{
		db:          db,
		log:         log,
		directory:   directory,
		credentials: store.NewCredentials(),
	}
}

// unavailableDirectory stands in for a nil Directory and refuses.
type unavailableDirectory struct{}

func (unavailableDirectory) Authenticate(context.Context, string, string) (bool, error) {
	return false, errors.New("mailauth: no directory is configured")
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
	// An empty password is refused before anything is consulted. A directory
	// asked to bind with no password performs an anonymous bind, which
	// succeeds without proving anything, so this case must never reach one.
	if pw == "" {
		password.DummyVerify(pw)
		return policy.Principal{}, ErrRejected
	}

	principal, err := store.LoadPrincipal(ctx, v.db, strings.TrimSpace(login))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Spend the time a real comparison would take, so that a login
			// name nobody holds is not measurably faster than one that is.
			password.DummyVerify(pw)
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
		return policy.Principal{}, ErrRejected
	}

	if err := v.verifyPassword(ctx, principal, pw); err != nil {
		return policy.Principal{}, err
	}

	// The identity is now proved, so it can be asked what it may do with mail.
	// A domain admin, a shared mailbox, an account with no mailbox and one that
	// must change its password are all refused here.
	principal.Authenticated = true
	if err := principal.CanMailProtocol(action); err != nil {
		v.log.InfoContext(ctx, "mail login refused by policy",
			"address", principal.Address, "protocol", string(action), "code", denialCode(err))
		return policy.Principal{}, ErrRejected
	}
	return principal, nil
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

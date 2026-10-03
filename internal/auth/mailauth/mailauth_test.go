package mailauth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/mailauth"
	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

// stubDirectory stands in for a real directory. The answers are set per test so
// that the rules, rather than the wire protocol, are what is under test; the
// bind itself is exercised in internal/auth/ldap against the fixture.
type stubDirectory struct {
	ok   bool
	err  error
	seen []string
}

func (s *stubDirectory) Authenticate(_ context.Context, address, _ string) (bool, error) {
	s.seen = append(s.seen, address)
	return s.ok, s.err
}

func testArgon2() config.Argon2Params {
	return config.Argon2Params{MemoryKib: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
}

func newVerifier(t *testing.T, db *sqlx.DB, dir mailauth.Directory) *mailauth.Verifier {
	t.Helper()
	return mailauth.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), dir)
}

// createUser provisions a principal, which is the shape the reconcile leaves
// behind: a directory user has no local credential row at all.
func createUser(t *testing.T, db *sqlx.DB, address string, source policy.Source, mailbox bool) {
	t.Helper()
	if err := store.NewUsers().Create(context.Background(), db, store.CreateUserRequest{
		Address: address, Source: source, MailboxEnabled: &mailbox,
	}); err != nil {
		t.Fatalf("creating %s: %v", address, err)
	}
}

func setPassword(t *testing.T, db *sqlx.DB, address, plaintext string) {
	t.Helper()
	hash, err := password.Hash(plaintext, testArgon2())
	if err != nil {
		t.Fatalf("hashing for %s: %v", address, err)
	}
	if err := store.NewCredentials().Set(context.Background(), db, address, hash); err != nil {
		t.Fatalf("setting the credential for %s: %v", address, err)
	}
}

// TestDirectoryUserIsVerifiedAgainstTheDirectory is the case this package
// exists for: a principal with no local password whose password the directory
// holds.
func TestDirectoryUserIsVerifiedAgainstTheDirectory(t *testing.T) {
	db := testdb.Migrated(t)
	createUser(t, db, "dana@corp.example.com", policy.SourceLDAP, true)
	dir := &stubDirectory{ok: true}
	v := newVerifier(t, db, dir)

	got, err := v.Authenticate(context.Background(), "dana@corp.example.com", "from-the-directory", policy.ActionIMAP)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.Address != "dana@corp.example.com" {
		t.Errorf("address = %q", got.Address)
	}
	if !got.Authenticated {
		t.Error("a successful login must return an authenticated principal")
	}
	if len(dir.seen) != 1 || dir.seen[0] != "dana@corp.example.com" {
		t.Errorf("the directory saw %v, want one attempt for dana", dir.seen)
	}
}

// TestDirectoryRejectionAndOutageAreDistinct is the property the listeners rely
// on to log the right thing: a refusal and an unreachable directory both fail,
// but not with the same error.
func TestDirectoryRejectionAndOutageAreDistinct(t *testing.T) {
	db := testdb.Migrated(t)
	createUser(t, db, "dana@corp.example.com", policy.SourceLDAP, true)
	ctx := context.Background()

	// A directory that refuses is the user's mistake.
	v := newVerifier(t, db, &stubDirectory{ok: false})
	if _, err := v.Authenticate(ctx, "dana@corp.example.com", "wrong", policy.ActionIMAP); !errors.Is(err, mailauth.ErrRejected) {
		t.Errorf("a refusal = %v, want ErrRejected", err)
	}

	// A directory that cannot be reached is nobody's.
	down := errors.New("the directory is unreachable")
	v = newVerifier(t, db, &stubDirectory{err: down})
	_, err := v.Authenticate(ctx, "dana@corp.example.com", "whatever", policy.ActionIMAP)
	if !errors.Is(err, mailauth.ErrUnavailable) {
		t.Errorf("an outage = %v, want ErrUnavailable", err)
	}
	if !errors.Is(err, down) {
		t.Error("the underlying cause must be kept for the operator")
	}
}

// TestNoStoredCredentialIsNotAPass is the bypass this package closed. Every one
// of the three protocol listeners used to read "this account has no local
// password" as success, which admitted any password for every directory-backed
// mailbox.
func TestNoStoredCredentialIsNotAPass(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	v := newVerifier(t, db, nil) // no directory at all

	// A local account with no password set.
	createUser(t, db, "nopass@example.com", policy.SourceLocal, true)
	for _, pw := range []string{"", "anything", "guess"} {
		if _, err := v.Authenticate(ctx, "nopass@example.com", pw, policy.ActionIMAP); err == nil {
			t.Errorf("a local account with no password was admitted with %q", pw)
		}
	}

	// A directory-backed account with the directory unreachable.
	createUser(t, db, "dana@corp.example.com", policy.SourceLDAP, true)
	if _, err := v.Authenticate(ctx, "dana@corp.example.com", "anything", policy.ActionPOP3); !errors.Is(err, mailauth.ErrUnavailable) {
		t.Errorf("with no directory = %v, want ErrUnavailable", err)
	}

	// A domain admin with no password anywhere.
	createUser(t, db, "admin@example.com", policy.SourceDomainAdmin, false)
	if _, err := v.Authenticate(ctx, "admin@example.com", "anything", policy.ActionSMTP); err == nil {
		t.Error("a domain admin with no password anywhere was admitted")
	}
}

// TestEmptyPasswordNeverReachesTheDirectory covers a subtle trap: a bind with an
// empty password is an anonymous bind, which a directory answers successfully
// without proving anything.
func TestEmptyPasswordNeverReachesTheDirectory(t *testing.T) {
	db := testdb.Migrated(t)
	createUser(t, db, "dana@corp.example.com", policy.SourceLDAP, true)
	dir := &stubDirectory{ok: true}
	v := newVerifier(t, db, dir)

	if _, err := v.Authenticate(context.Background(), "dana@corp.example.com", "", policy.ActionIMAP); !errors.Is(err, mailauth.ErrRejected) {
		t.Fatalf("an empty password = %v, want ErrRejected", err)
	}
	if len(dir.seen) != 0 {
		t.Errorf("the directory was consulted %d times, want none", len(dir.seen))
	}
}

// TestLocalPasswordStillWorks guards the ordinary path the listeners had before.
func TestLocalPasswordStillWorks(t *testing.T) {
	db := testdb.Migrated(t)
	createUser(t, db, "alice@example.com", policy.SourceLocal, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	ctx := context.Background()
	dir := &stubDirectory{ok: true}
	v := newVerifier(t, db, dir)

	if _, err := v.Authenticate(ctx, "alice@example.com", "correct horse battery staple", policy.ActionIMAP); err != nil {
		t.Fatalf("the right password was refused: %v", err)
	}
	if len(dir.seen) != 0 {
		t.Error("a local account must not be checked against the directory")
	}
	if _, err := v.Authenticate(ctx, "alice@example.com", "wrong", policy.ActionIMAP); !errors.Is(err, mailauth.ErrRejected) {
		t.Errorf("a wrong password = %v, want ErrRejected", err)
	}
}

// TestUnreadableCredentialIsRefused proves a corrupted hash fails closed. The
// SMTP listener used to let any password through when the hash did not start
// with "$argon2", because its comparison sat inside a conditional it could fall
// past.
func TestUnreadableCredentialIsRefused(t *testing.T) {
	db := testdb.Migrated(t)
	createUser(t, db, "alice@example.com", policy.SourceLocal, true)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO credentials (address, password_hash) VALUES ('alice@example.com', 'not-a-hash')`,
	); err != nil {
		t.Fatalf("inserting a corrupt hash: %v", err)
	}

	v := newVerifier(t, db, nil)
	// The cause is kept, so it is neither sentinel that matters here.
	_, err := v.Authenticate(ctx, "alice@example.com", "anything", policy.ActionIMAP)
	if err == nil {
		t.Fatal("a corrupt stored hash must not admit anyone")
	}
	if errors.Is(err, mailauth.ErrRejected) {
		t.Error("a data problem should be reported as such, not as a wrong password")
	}
}

// TestPolicyRefusals is the mail-protocol rule set: an account that may sign in
// to the web still may not move mail.
func TestPolicyRefusals(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	v := newVerifier(t, db, nil)

	// Each case owns its address, because a principal's source is fixed at
	// creation and a shared one would be the second version of a different
	// thing.
	cases := []struct {
		name    string
		address string
		source  policy.Source
		mailbox bool
		action  policy.Action
	}{
		{"a domain admin may not read mail", "admin@example.com", policy.SourceDomainAdmin, false, policy.ActionIMAP},
		{"a domain admin may not submit", "admin2@example.com", policy.SourceDomainAdmin, false, policy.ActionSMTP},
		{"an account with no mailbox may not read mail", "nomail@example.com", policy.SourceLocal, false, policy.ActionIMAP},
		{"a shared mailbox may not log in as itself", "team@example.com", policy.SourceShared, true, policy.ActionIMAP},
		{"a shared mailbox may not submit", "team2@example.com", policy.SourceShared, true, policy.ActionSMTP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			createUser(t, db, tc.address, tc.source, tc.mailbox)
			setPassword(t, db, tc.address, "pw")
			if _, err := v.Authenticate(ctx, tc.address, "pw", tc.action); !errors.Is(err, mailauth.ErrRejected) {
				t.Errorf("= %v, want ErrRejected", err)
			}
		})
	}

	t.Run("a disabled account is refused", func(t *testing.T) {
		createUser(t, db, "off@example.com", policy.SourceLocal, true)
		setPassword(t, db, "off@example.com", "pw")
		if err := store.NewUsers().SetEnabled(ctx, db, "off@example.com", false); err != nil {
			t.Fatalf("disabling: %v", err)
		}
		if _, err := v.Authenticate(ctx, "off@example.com", "pw", policy.ActionIMAP); !errors.Is(err, mailauth.ErrRejected) {
			t.Errorf("= %v, want ErrRejected", err)
		}
	})

	t.Run("an account that must change its password is refused", func(t *testing.T) {
		createUser(t, db, "stale@example.com", policy.SourceLocal, true)
		setPassword(t, db, "stale@example.com", "pw")
		if _, err := db.ExecContext(ctx,
			`UPDATE users SET must_change_password = true WHERE address = 'stale@example.com'`,
		); err != nil {
			t.Fatalf("requiring a password change: %v", err)
		}
		if _, err := v.Authenticate(ctx, "stale@example.com", "pw", policy.ActionIMAP); !errors.Is(err, mailauth.ErrRejected) {
			t.Errorf("= %v, want ErrRejected", err)
		}
	})
}

// TestDomainAdminUsesWhicheverHoldsItsPassword mirrors the web path: a domain
// admin made here has a local password, one the reconcile provisioned does not.
func TestDomainAdminUsesWhicheverHoldsItsPassword(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	createUser(t, db, "admin@example.com", policy.SourceDomainAdmin, false)

	// With no stored credential the directory answers.
	dir := &stubDirectory{ok: true}
	// A domain admin may never use a mail protocol, so this asserts only that
	// the password check itself happened against the directory; the refusal
	// that follows is the policy's, which the previous test covers.
	if _, err := newVerifier(t, db, dir).Authenticate(ctx, "admin@example.com", "dir-pw", policy.ActionAdminister); !errors.Is(err, mailauth.ErrRejected) {
		t.Fatalf("a domain admin should be refused by policy, got %v", err)
	}
	if len(dir.seen) != 1 {
		t.Fatalf("the directory saw %v, want one attempt", dir.seen)
	}
	if len(dir.seen) != 1 {
		t.Fatalf("the directory saw %v, want one attempt", dir.seen)
	}

	// With a stored credential it wins, and the directory is not consulted.
	setPassword(t, db, "admin@example.com", "local-pw")
	dir = &stubDirectory{ok: false}
	v := newVerifier(t, db, dir)
	if _, err := v.Authenticate(ctx, "admin@example.com", "local-pw", policy.ActionAdminister); !errors.Is(err, mailauth.ErrRejected) {
		t.Fatalf("the stored password should have been accepted before policy refused, got %v", err)
	}
	if len(dir.seen) != 0 {
		t.Error("a domain admin with a stored password must not be checked against the directory")
	}
	if _, err := v.Authenticate(ctx, "admin@example.com", "dir-pw", policy.ActionAdminister); !errors.Is(err, mailauth.ErrRejected) {
		t.Errorf("the directory password = %v, want ErrRejected", err)
	}
}

// TestUnknownAccountIsRefused is the baseline every listener relies on.
func TestUnknownAccountIsRefused(t *testing.T) {
	db := testdb.Migrated(t)
	v := newVerifier(t, db, nil)
	for _, login := range []string{"", "nobody@example.com", "   "} {
		if _, err := v.Authenticate(context.Background(), login, "pw", policy.ActionIMAP); !errors.Is(err, mailauth.ErrRejected) {
			t.Errorf("%q = %v, want ErrRejected", login, err)
		}
	}
}

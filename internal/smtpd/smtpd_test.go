package smtpd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/login"
	"github.com/linchange/linchange/internal/auth/mailauth"
	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

func boolPtr(b bool) *bool { return &b }

// These tests drive the session's own authenticate method rather than a
// listener over a socket. What is under test is the rule the submission
// listener applies to a login, not the SMTP framing around it, and the
// framing is the part the protocol already speaks correctly.

// stubDirectory stands in for a real directory, so that the rule being tested is
// the listener's rather than the bind's.
type stubDirectory struct {
	ok   bool
	err  error
	seen []string
}

func (s *stubDirectory) Authenticate(_ context.Context, address, _ string) (bool, error) {
	s.seen = append(s.seen, address)
	return s.ok, s.err
}

// newBackend creates a backend for testing with the given directory stub.
func newBackend(t *testing.T, dir login.Directory) (*Backend, *sqlx.DB) {
	t.Helper()
	db := testdb.Migrated(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewBackend(
		db,
		config.Submission{},
		store.NewAliases(),
		mailauth.New(db, log, dir),
		nil, // the outbox is not reached by an authentication
		nil, // nor the blobstore
		log,
		store.NewSendCounters(),
		store.NewSharedMailboxes(),
		store.NewSharedMailboxGrants(),
	), db
}

func createUser(t *testing.T, db *sqlx.DB, address string, source policy.Source) {
	t.Helper()
	if err := store.NewUsers().Create(context.Background(), db, store.CreateUserRequest{
		Address: address, Source: source, DisplayName: "Test User",
	}); err != nil {
		t.Fatalf("creating %s: %v", address, err)
	}
}

func setPassword(t *testing.T, db *sqlx.DB, address, plaintext string) {
	t.Helper()
	hash, err := password.Hash(plaintext, config.Argon2Params{
		MemoryKib: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	})
	if err != nil {
		t.Fatalf("hashing for %s: %v", address, err)
	}
	if err := store.NewCredentials().Set(context.Background(), db, address, hash); err != nil {
		t.Fatalf("setting the credential for %s: %v", address, err)
	}
}

// TestAuthenticateWithDirectoryPassword covers the case the listener takes a
// verifier for: a directory-backed account is admitted on the directory's
// answer, not on a stored hash, and its sender identity becomes the stored
// address rather than whatever the client typed.
func TestAuthenticateWithDirectoryPassword(t *testing.T) {
	dir := &stubDirectory{ok: true}
	backend, db := newBackend(t, dir)
	createUser(t, db, "dana@corp.example.com", policy.SourceLDAP)

	s := &session{backend: backend}
	if err := s.authenticate("DANA@corp.example.com", "from-the-directory"); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !s.authenticated {
		t.Error("a successful login must mark the session authenticated")
	}
	if s.from != "dana@corp.example.com" {
		t.Errorf("sender identity = %q, want the stored address", s.from)
	}
	if len(dir.seen) != 1 || dir.seen[0] != "dana@corp.example.com" {
		t.Errorf("the directory saw %v, want one attempt for dana", dir.seen)
	}
}

// TestAuthenticateDirectoryRefused is the negative half: a refusal is a failed
// login.
func TestAuthenticateDirectoryRefused(t *testing.T) {
	backend, db := newBackend(t, &stubDirectory{ok: false})
	createUser(t, db, "dana@corp.example.com", policy.SourceLDAP)

	s := &session{backend: backend}
	if err := s.authenticate("dana@corp.example.com", "wrong"); err == nil {
		t.Fatal("a password the directory refused must not authenticate")
	}
	if s.authenticated {
		t.Error("a refused login must leave the session unauthenticated")
	}
}

// TestAuthenticateDirectoryUnavailable proves the listener fails closed rather
// than falling through to something else when the directory cannot be reached.
func TestAuthenticateDirectoryUnavailable(t *testing.T) {
	backend, db := newBackend(t, &stubDirectory{err: errors.New("the directory is unreachable")})
	createUser(t, db, "dana@corp.example.com", policy.SourceLDAP)

	s := &session{backend: backend}
	if err := s.authenticate("dana@corp.example.com", "whatever"); err == nil {
		t.Fatal("an unreachable directory must not authenticate anyone")
	}
}

// TestAuthenticateWithoutCredentialIsRefused is the bypass this closed: this
// account has no stored password, which the listener used to read as "any
// password will do", leaving every directory-backed mailbox open on every port.
func TestAuthenticateWithoutCredentialIsRefused(t *testing.T) {
	// No directory is wired up, so nothing can answer for a principal whose
	// password nobody holds.
	backend, db := newBackend(t, nil)

	for _, source := range []policy.Source{policy.SourceLDAP, policy.SourceLocal} {
		address := "dana@corp.example.com"
		if source == policy.SourceLocal {
			address = "nopass@example.com"
		}
		createUser(t, db, address, source)

		for _, pw := range []string{"", "anything", "guess"} {
			s := &session{backend: backend}
			if err := s.authenticate(address, pw); err == nil {
				t.Errorf("a %s account with no stored password was admitted with %q", source, pw)
			}
			if s.authenticated {
				t.Errorf("a %s account with no stored password was marked authenticated", source)
			}
		}
	}
}

// TestAuthenticateDomainAdminRefused shows a principal that may sign in to the
// web still cannot submit mail.
func TestAuthenticateDomainAdminRefused(t *testing.T) {
	backend, db := newBackend(t, &stubDirectory{ok: true})
	createUser(t, db, "admin@example.com", policy.SourceDomainAdmin)
	setPassword(t, db, "admin@example.com", "admin-pw")

	s := &session{backend: backend}
	if err := s.authenticate("admin@example.com", "admin-pw"); err == nil {
		t.Fatal("a domain admin must not be able to submit mail")
	}
}

// TestAuthenticateLocalPasswordStillWorks guards the ordinary path the listener
// had before, including that a local account is never checked against the
// directory.
func TestAuthenticateLocalPasswordStillWorks(t *testing.T) {
	dir := &stubDirectory{ok: true}
	backend, db := newBackend(t, dir)
	createUser(t, db, "alice@example.com", policy.SourceLocal)
	setPassword(t, db, "alice@example.com", "testpassword123")

	s := &session{backend: backend}
	if err := s.authenticate("alice@example.com", "testpassword123"); err != nil {
		t.Fatalf("the right password was refused: %v", err)
	}
	if s.from != "alice@example.com" {
		t.Errorf("sender identity = %q", s.from)
	}
	if len(dir.seen) != 0 {
		t.Error("a local account must not be checked against the directory")
	}

	s = &session{backend: backend}
	if err := s.authenticate("alice@example.com", "wrongpassword"); err == nil {
		t.Fatal("a wrong password must not authenticate")
	}
}

// TestAuthenticateUnreadableCredentialIsRefused proves a corrupted hash fails
// closed. The submission listener used to let any password through when the
// stored hash did not begin with "$argon2", because its comparison sat inside a
// conditional it could fall past.
func TestAuthenticateUnreadableCredentialIsRefused(t *testing.T) {
	backend, db := newBackend(t, nil)
	createUser(t, db, "alice@example.com", policy.SourceLocal)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO credentials (address, password_hash) VALUES ('alice@example.com', 'not-a-hash')`,
	); err != nil {
		t.Fatalf("inserting a corrupt hash: %v", err)
	}

	s := &session{backend: backend}
	if err := s.authenticate("alice@example.com", "anything"); err == nil {
		t.Fatal("a corrupt stored hash must not admit anyone")
	}
	if s.authenticated {
		t.Error("a corrupt stored hash must leave the session unauthenticated")
	}
}

// TestAuthenticateSharedMailbox proves that "user#shared" syntax works for
// submitting on behalf of a shared mailbox.
func TestAuthenticateSharedMailbox(t *testing.T) {
	dir := &stubDirectory{ok: false}
	backend, db := newBackend(t, dir)
	createUser(t, db, "user@example.com", policy.SourceLocal)
	setPassword(t, db, "user@example.com", "password")

	// Create the shared mailbox principal with source='shared'.
	if err := store.NewUsers().Create(context.Background(), db, store.CreateUserRequest{
		Address: "shared@example.com", Source: policy.SourceShared, DisplayName: "Shared", MailboxEnabled: boolPtr(true),
	}); err != nil {
		t.Fatalf("creating shared principal: %v", err)
	}
	sharedHash, err := password.Hash("shared-password", config.Argon2Params{
		MemoryKib: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	})
	if err != nil {
		t.Fatalf("hashing shared password: %v", err)
	}
	if err := store.NewCredentials().Set(context.Background(), db, "shared@example.com", sharedHash); err != nil {
		t.Fatalf("setting shared credential: %v", err)
	}

	// Create a shared mailbox and grant write access.
	if err := store.NewSharedMailboxes().Declare(context.Background(), db, store.CreateSharedMailboxRequest{
		Address:     "shared@example.com",
		DisplayName: "Shared",
		UpdatedBy:   &[]string{"user@example.com"}[0],
	}); err != nil {
		t.Fatalf("declaring shared mailbox: %v", err)
	}
	if err := store.NewSharedMailboxGrants().Grant(context.Background(), db, "shared@example.com", "user@example.com", policy.GrantEditor, &[]string{"user@example.com"}[0]); err != nil {
		t.Fatalf("granting shared access: %v", err)
	}

	s := &session{backend: backend}
	// Authenticate as user with #shared target
	if err := s.authenticate("user@example.com#shared@example.com", "password"); err != nil {
		t.Fatalf("authenticate with shared target: %v", err)
	}
	if !s.authenticated {
		t.Error("login must succeed")
	}
	// The sender identity should be the shared mailbox
	if s.from != "shared@example.com" {
		t.Errorf("sender identity = %q, want shared@example.com", s.from)
	}
	if s.sharedTarget != "shared@example.com" {
		t.Errorf("sharedTarget = %q, want shared@example.com", s.sharedTarget)
	}
}

// TestAuthenticateSharedMailboxAccessDenied proves that a user without
// write access to a shared mailbox cannot use the #shared syntax.
func TestAuthenticateSharedMailboxAccessDenied(t *testing.T) {
	dir := &stubDirectory{ok: false}
	backend, db := newBackend(t, dir)
	createUser(t, db, "user@example.com", policy.SourceLocal)
	setPassword(t, db, "user@example.com", "password")

	// Create the shared mailbox principal with source='shared'.
	if err := store.NewUsers().Create(context.Background(), db, store.CreateUserRequest{
		Address: "shared@example.com", Source: policy.SourceShared, DisplayName: "Shared", MailboxEnabled: boolPtr(true),
	}); err != nil {
		t.Fatalf("creating shared principal: %v", err)
	}
	sharedHash, err := password.Hash("shared-password", config.Argon2Params{
		MemoryKib: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	})
	if err != nil {
		t.Fatalf("hashing shared password: %v", err)
	}
	if err := store.NewCredentials().Set(context.Background(), db, "shared@example.com", sharedHash); err != nil {
		t.Fatalf("setting shared credential: %v", err)
	}

	// Create a shared mailbox but grant only read access.
	if err := store.NewSharedMailboxes().Declare(context.Background(), db, store.CreateSharedMailboxRequest{
		Address:     "shared@example.com",
		DisplayName: "Shared",
		UpdatedBy:   &[]string{"user@example.com"}[0],
	}); err != nil {
		t.Fatalf("declaring shared mailbox: %v", err)
	}
	if err := store.NewSharedMailboxGrants().Grant(context.Background(), db, "shared@example.com", "user@example.com", policy.GrantRead, &[]string{"user@example.com"}[0]); err != nil {
		t.Fatalf("granting shared access: %v", err)
	}

	s := &session{backend: backend}
	err = s.authenticate("user@example.com#shared@example.com", "password")
	if err == nil {
		t.Fatal("login must fail when user only has read access")
	}
	if s.authenticated {
		t.Error("login must not succeed")
	}
}

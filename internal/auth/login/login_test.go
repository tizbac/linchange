package login_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/pquerna/otp"
	libtotp "github.com/pquerna/otp/totp"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/lockout"
	"github.com/linchange/linchange/internal/auth/login"
	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/secret"
	"github.com/linchange/linchange/internal/auth/token"
	"github.com/linchange/linchange/internal/auth/totp"
	"github.com/linchange/linchange/internal/store"
)

const (
	testDBURLEnv   = "LINCHANGE_TEST_DATABASE_URL"
	defaultTestURL = "postgres://linchange:linchange@127.0.0.1:55432/linchange?sslmode=disable"

	// A fixed, valid base32 secret so the expected TOTP code can be computed
	// independently of the verifier under test.
	bobSecret = "JBSWY3DPEHPK3PXP"
)

var testIP = netip.MustParseAddr("203.0.113.10")

// testMigratedDB gives each test a throwaway database that has had every
// migration applied, so a test that leaves rows behind can never affect any
// other. It mirrors the store package's harness because the login package has
// to open its own connection.
func testMigratedDB(t *testing.T) *sqlx.DB {
	t.Helper()

	base := os.Getenv(testDBURLEnv)
	if base == "" {
		base = defaultTestURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := store.Open(ctx, store.Options{
		URL: base, MaxOpen: 2, ConnectTimeout: 10 * time.Second,
		ApplicationName: "linchange-login-test-admin",
	})
	if err != nil {
		if os.Getenv(testDBURLEnv) != "" {
			t.Fatalf("%s is set to %q but is unreachable: %v", testDBURLEnv, base, err)
		}
		t.Skipf("no test database at %s (%v); run `make up` or set %s", testDBURLEnv, err, testDBURLEnv)
	}

	name := fmt.Sprintf("lc_login_test_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	db, err := store.Open(ctx, store.Options{
		URL: replaceDatabase(base, name), MaxOpen: 8, MaxIdle: 4,
		ConnectTimeout: 10 * time.Second, ApplicationName: "linchange-login-test",
	})
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup: dropping %s: %v", name, err)
		}
		admin.Close()
	})

	if err := store.Migrate(ctx, db, nil); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return db
}

func replaceDatabase(dsn, name string) string {
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+1:]
	if q := strings.Index(rest, "?"); q >= 0 {
		rest = rest[:q]
	}
	return dsn[:i+1] + name + dsn[i+1+len(rest):]
}

func exec(t *testing.T, db *sqlx.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("executing %q: %v", query, err)
	}
}

// seedFixture creates the domain and one principal of each relevant kind.
func seedFixture(t *testing.T, db *sqlx.DB) {
	t.Helper()
	exec(t, db, `INSERT INTO domains (name) VALUES ('example.com')`)
	for _, u := range []struct {
		address, local, source string
		mailbox, enabled       bool
	}{
		{"alice@example.com", "alice", "local", true, true},
		{"bob@example.com", "bob", "local", true, true},
		{"dana@example.com", "dana", "ldap", true, true},
		{"support@example.com", "support", "shared", true, true},
		{"admin@example.com", "admin", "domain_admin", false, true},
		{"carol@example.com", "carol", "local", true, false},
	} {
		exec(t, db,
			`INSERT INTO users (address, address_local, source, mailbox_enabled, enabled)
			 VALUES ($1, $2, $3, $4, $5)`,
			u.address, u.local, u.source, u.mailbox, u.enabled)
	}
}

func testArgon2() config.Argon2Params {
	return config.Argon2Params{MemoryKib: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
}

func strongArgon2() config.Argon2Params {
	return config.Argon2Params{MemoryKib: 65536, Iterations: 3, Parallelism: 2, SaltLength: 16, KeyLength: 32}
}

func setPassword(t *testing.T, db *sqlx.DB, address, plaintext string, params config.Argon2Params) {
	t.Helper()
	hash, err := password.Hash(plaintext, params)
	if err != nil {
		t.Fatalf("hashing %s: %v", address, err)
	}
	if err := store.NewCredentials().Set(context.Background(), db, address, hash); err != nil {
		t.Fatalf("storing credential for %s: %v", address, err)
	}
}

func newVerifier(t *testing.T) *totp.Verifier {
	t.Helper()
	v, err := totp.New(config.TOTPAuth{Enabled: true, Issuer: "linchange", Window: 1, RecoveryCodes: 8},
		"0123456789abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatalf("building TOTP verifier: %v", err)
	}
	return v
}

// enrolBob seals a secret for bob and stores an active enrolment, returning the
// recovery codes so a test can spend one and assert exactly one digest went.
func enrolBob(t *testing.T, db *sqlx.DB, v *totp.Verifier) []secret.RecoveryCode {
	t.Helper()
	codes, err := secret.GenerateRecoveryCodes(8)
	if err != nil {
		t.Fatalf("generating recovery codes: %v", err)
	}
	digests := make([][]byte, len(codes))
	for i, c := range codes {
		digests[i] = c.Digest
	}
	enc, err := v.Encrypt(bobSecret, digests)
	if err != nil {
		t.Fatalf("encrypting the TOTP secret: %v", err)
	}
	if err := store.NewTOTP().Store(context.Background(), db, "bob@example.com", enc.Ciphertext, digests, true); err != nil {
		t.Fatalf("storing the TOTP enrolment: %v", err)
	}
	return codes
}

func totpCode(t *testing.T, at time.Time) string {
	t.Helper()
	code, err := libtotp.GenerateCodeCustom(bobSecret, at.UTC(), libtotp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("generating TOTP code: %v", err)
	}
	return code
}

// authDeps collects the things a test may want to vary about the orchestrator.
// Everything a test does not override gets a safe default.
type authDeps struct {
	dir    login.Directory
	pol    policy.Policy
	acct   lockout.Policy
	ip     lockout.Policy
	argon2 config.Argon2Params
	totp   *totp.Verifier
	ttl    time.Duration
	now    func() time.Time
}

func stdDeps(t *testing.T) authDeps {
	return authDeps{
		pol:    policy.Policy{},
		acct:   lockout.Policy{MaxAttempts: 1000, Window: 15 * time.Minute, Duration: time.Hour},
		ip:     lockout.Policy{MaxAttempts: 1000, Window: 15 * time.Minute, Duration: time.Hour},
		argon2: testArgon2(),
		totp:   newVerifier(t),
		ttl:    5 * time.Minute,
		now:    time.Now,
	}
}

// optionsFor assembles the full option set so a test can either hand it to New
// directly or adjust one field before the constructor sees it.
func optionsFor(t *testing.T, d authDeps) login.Options {
	t.Helper()
	mgr, err := token.NewManager(config.JWTAuth{
		Secret: strings.Repeat("unit-test-signing-key-01", 2),
		Issuer: "linchange-test", AccessTTL: 15 * time.Minute, RefreshTTL: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("building the token manager: %v", err)
	}
	return login.Options{
		Credentials:  store.NewCredentials(),
		Enrolments:   store.NewTOTP(),
		Attempts:     store.NewAuthAttempts(),
		Challenges:   store.NewChallenges(),
		Sessions:     store.NewSessions(),
		Grants:       store.NewImpersonationGrants(),
		Audit:        store.NewAuditLog(),
		Tokens:       mgr,
		TOTP:         d.totp,
		Directory:    d.dir,
		Argon2:       d.argon2,
		Lockout:      lockout.Both{Account: d.acct, IP: d.ip},
		Policy:       d.pol,
		ChallengeTTL: d.ttl,
		Now:          d.now,
	}
}

func newAuth(t *testing.T, db *sqlx.DB, d authDeps) *login.Authenticator {
	t.Helper()
	auth, err := login.New(optionsFor(t, d))
	if err != nil {
		t.Fatalf("building the authenticator: %v", err)
	}
	return auth
}

func req(loginName, pw string) login.Request {
	return login.Request{Login: loginName, Password: pw, IP: testIP, UserAgent: "login-test"}
}

type fakeDirectory struct {
	ok  bool
	err error
}

func (f fakeDirectory) Authenticate(context.Context, string, string) (bool, error) {
	return f.ok, f.err
}

// accountFailures reads the stored account-scope failure count for assertions.
func accountFailures(t *testing.T, db *sqlx.DB, address string) int {
	t.Helper()
	state, err := store.NewAuthAttempts().AccountState(context.Background(), db, address)
	if err != nil {
		t.Fatalf("reading account state for %s: %v", address, err)
	}
	return state.FailedAttempts
}

func TestPasswordLocalSuccessIssuesTokens(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "alice@example.com", "correct horse battery staple", testArgon2())
	auth := newAuth(t, db, stdDeps(t))

	got, err := auth.Password(context.Background(), db, req("alice@example.com", "correct horse battery staple"))
	if err != nil {
		t.Fatalf("Password: %v", err)
	}
	if got.ChallengeID != "" {
		t.Errorf("no challenge is expected, got %q", got.ChallengeID)
	}
	if got.Granted == nil {
		t.Fatal("a local login without a second factor should grant immediately")
	}
	if got.Granted.Access.Token == "" || got.Granted.Refresh.Token == "" {
		t.Error("both tokens must be issued")
	}
	if got.Granted.Session.Address != "alice@example.com" || got.Granted.Session.ID == "" {
		t.Errorf("unexpected session: %+v", got.Granted.Session)
	}
}

func TestPasswordWrongPasswordIsRejectedAndRecorded(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "alice@example.com", "correct horse battery staple", testArgon2())
	auth := newAuth(t, db, stdDeps(t))

	if _, err := auth.Password(context.Background(), db, req("alice@example.com", "wrong password")); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Fatalf("wrong password = %v, want ErrInvalidCredentials", err)
	}
	if n := accountFailures(t, db, "alice@example.com"); n != 1 {
		t.Errorf("account failures = %d, want 1", n)
	}

	// A subsequent correct password clears the account history.
	if _, err := auth.Password(context.Background(), db, req("alice@example.com", "correct horse battery staple")); err != nil {
		t.Fatalf("correct password after a failure: %v", err)
	}
	if n := accountFailures(t, db, "alice@example.com"); n != 0 {
		t.Errorf("account failures after success = %d, want 0", n)
	}
}

func TestPasswordUnknownAddressIsRejected(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	auth := newAuth(t, db, stdDeps(t))

	if _, err := auth.Password(context.Background(), db, req("ghost@example.com", "whatever")); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Fatalf("unknown address = %v, want ErrInvalidCredentials", err)
	}
	// No account row may be created by guessing.
	if n := accountFailures(t, db, "ghost@example.com"); n != 0 {
		t.Errorf("a guessed address must not gain an account history, got %d", n)
	}
}

func TestPasswordDisabledUserIsRejected(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "carol@example.com", "correct horse battery staple", testArgon2())
	auth := newAuth(t, db, stdDeps(t))

	if _, err := auth.Password(context.Background(), db, req("carol@example.com", "correct horse battery staple")); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Fatalf("disabled user = %v, want ErrInvalidCredentials", err)
	}
}

func TestPasswordSharedMailboxIsRejected(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	auth := newAuth(t, db, stdDeps(t))

	if _, err := auth.Password(context.Background(), db, req("support@example.com", "anything")); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Fatalf("shared mailbox = %v, want ErrInvalidCredentials", err)
	}
}

func TestPasswordLDAPUnavailableFailsClosed(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	// No directory is wired, so an LDAP sign-in must be refused, and it must be
	// told apart from a wrong password in the error so it is not recorded.
	auth := newAuth(t, db, stdDeps(t))

	if _, err := auth.Password(context.Background(), db, req("dana@example.com", "secret")); !errors.Is(err, login.ErrDirectoryUnavailable) {
		t.Fatalf("unavailable directory = %v, want ErrDirectoryUnavailable", err)
	}
	if n := accountFailures(t, db, "dana@example.com"); n != 0 {
		t.Errorf("an outage must not be counted as a failure, got %d", n)
	}
}

func TestPasswordLDAPRejectedByDirectory(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	d := stdDeps(t)
	d.dir = fakeDirectory{ok: false}
	auth := newAuth(t, db, d)

	if _, err := auth.Password(context.Background(), db, req("dana@example.com", "bad")); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Fatalf("directory rejection = %v, want ErrInvalidCredentials", err)
	}
	if n := accountFailures(t, db, "dana@example.com"); n != 1 {
		t.Errorf("a rejected password should be recorded, got %d", n)
	}
}

func TestPasswordLDAPSuccess(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	d := stdDeps(t)
	d.dir = fakeDirectory{ok: true}
	auth := newAuth(t, db, d)

	got, err := auth.Password(context.Background(), db, req("dana@example.com", "from-the-directory"))
	if err != nil {
		t.Fatalf("Password: %v", err)
	}
	if got.Granted == nil {
		t.Fatal("a directory-authenticated user should be granted")
	}
	if got.Granted.Session.Address != "dana@example.com" {
		t.Errorf("session principal = %q", got.Granted.Session.Address)
	}
}

func TestPasswordWithSecondFactorReturnsChallenge(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	d := stdDeps(t)
	setPassword(t, db, "bob@example.com", "correct horse battery staple", testArgon2())
	enrolBob(t, db, d.totp)
	auth := newAuth(t, db, d)

	got, err := auth.Password(context.Background(), db, req("bob@example.com", "correct horse battery staple"))
	if err != nil {
		t.Fatalf("Password: %v", err)
	}
	if got.Granted != nil {
		t.Fatal("a user with an active second factor must not be granted yet")
	}
	if got.ChallengeID == "" {
		t.Fatal("a second factor must produce a challenge id")
	}
	if !got.ExpiresAt.After(time.Now()) {
		t.Errorf("challenge has no useful lifetime: %v", got.ExpiresAt)
	}
}

func TestPasswordSkipsSecondFactorWhenTOTPDisabled(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "bob@example.com", "correct horse battery staple", testArgon2())
	// An enabled enrolment exists, but the deployment has switched TOTP off. The
	// enrolment must not trap the user behind a challenge no verifier can pass.
	enrolBob(t, db, newVerifier(t))
	d := stdDeps(t)
	d.totp = nil
	auth := newAuth(t, db, d)

	got, err := auth.Password(context.Background(), db, req("bob@example.com", "correct horse battery staple"))
	if err != nil {
		t.Fatalf("Password: %v", err)
	}
	if got.ChallengeID != "" {
		t.Fatalf("a disabled second factor must not issue a challenge, got %q", got.ChallengeID)
	}
	if got.Granted == nil {
		t.Fatal("a user whose second factor is disabled deployment-wide must still be able to sign in")
	}
}

func TestSecondFactorWithValidTOTPCode(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	d := stdDeps(t)
	setPassword(t, db, "bob@example.com", "correct horse battery staple", testArgon2())
	enrolBob(t, db, d.totp)
	auth := newAuth(t, db, d)
	ctx := context.Background()

	ch, err := auth.Password(ctx, db, req("bob@example.com", "correct horse battery staple"))
	if err != nil {
		t.Fatalf("Password: %v", err)
	}
	code := totpCode(t, time.Now())
	got, err := auth.SecondFactor(ctx, db, login.SecondFactorRequest{
		ChallengeID: ch.ChallengeID, Code: code, IP: testIP,
	})
	if err != nil {
		t.Fatalf("SecondFactor: %v", err)
	}
	if got.Session.Address != "bob@example.com" || got.Access.Token == "" {
		t.Errorf("unexpected grant: %+v", got)
	}
}

func TestSecondFactorWithRecoveryCode(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	d := stdDeps(t)
	setPassword(t, db, "bob@example.com", "correct horse battery staple", testArgon2())
	codes := enrolBob(t, db, d.totp)
	auth := newAuth(t, db, d)
	ctx := context.Background()

	ch, err := auth.Password(ctx, db, req("bob@example.com", "correct horse battery staple"))
	if err != nil {
		t.Fatalf("Password: %v", err)
	}
	got, err := auth.SecondFactor(ctx, db, login.SecondFactorRequest{
		ChallengeID: ch.ChallengeID, Code: codes[0].Code, IP: testIP,
	})
	if err != nil {
		t.Fatalf("SecondFactor with a recovery code: %v", err)
	}
	if got.Session.Address != "bob@example.com" {
		t.Errorf("unexpected grant: %+v", got)
	}

	// Exactly one digest must have been spent.
	rec, err := store.NewTOTP().Get(ctx, db, "bob@example.com")
	if err != nil {
		t.Fatalf("reading enrolment: %v", err)
	}
	if len(rec.RecoveryDigests) != len(codes)-1 {
		t.Errorf("recovery digests = %d, want %d", len(rec.RecoveryDigests), len(codes)-1)
	}
}

func TestSecondFactorSpendRecoveryCodeOnce(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	d := stdDeps(t)
	setPassword(t, db, "bob@example.com", "correct horse battery staple", testArgon2())
	codes := enrolBob(t, db, d.totp)
	auth := newAuth(t, db, d)
	ctx := context.Background()

	spent := codes[0].Code
	first, err := auth.Password(ctx, db, req("bob@example.com", "correct horse battery staple"))
	if err != nil {
		t.Fatalf("first Password: %v", err)
	}
	if _, err := auth.SecondFactor(ctx, db, login.SecondFactorRequest{ChallengeID: first.ChallengeID, Code: spent, IP: testIP}); err != nil {
		t.Fatalf("first SecondFactor: %v", err)
	}

	// A fresh challenge must no longer accept the same recovery code.
	second, err := auth.Password(ctx, db, req("bob@example.com", "correct horse battery staple"))
	if err != nil {
		t.Fatalf("second Password: %v", err)
	}
	if _, err := auth.SecondFactor(ctx, db, login.SecondFactorRequest{ChallengeID: second.ChallengeID, Code: spent, IP: testIP}); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Errorf("replayed recovery code = %v, want ErrInvalidCredentials", err)
	}
}

func TestSecondFactorWrongCodeRefusedButChallengeSurvives(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	d := stdDeps(t)
	setPassword(t, db, "bob@example.com", "correct horse battery staple", testArgon2())
	enrolBob(t, db, d.totp)
	auth := newAuth(t, db, d)
	ctx := context.Background()

	ch, err := auth.Password(ctx, db, req("bob@example.com", "correct horse battery staple"))
	if err != nil {
		t.Fatalf("Password: %v", err)
	}

	if _, err := auth.SecondFactor(ctx, db, login.SecondFactorRequest{ChallengeID: ch.ChallengeID, Code: "000000", IP: testIP}); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Fatalf("wrong code = %v, want ErrInvalidCredentials", err)
	}
	// A wrong code must not consume the challenge: the right code still works.
	got, err := auth.SecondFactor(ctx, db, login.SecondFactorRequest{ChallengeID: ch.ChallengeID, Code: totpCode(t, time.Now()), IP: testIP})
	if err != nil {
		t.Fatalf("correct code after a wrong one: %v", err)
	}
	if got.Session.Address != "bob@example.com" {
		t.Errorf("unexpected grant: %+v", got)
	}
}

func TestSecondFactorChallengeIsSingleUse(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	d := stdDeps(t)
	setPassword(t, db, "bob@example.com", "correct horse battery staple", testArgon2())
	enrolBob(t, db, d.totp)
	auth := newAuth(t, db, d)
	ctx := context.Background()

	ch, err := auth.Password(ctx, db, req("bob@example.com", "correct horse battery staple"))
	if err != nil {
		t.Fatalf("Password: %v", err)
	}
	if _, err := auth.SecondFactor(ctx, db, login.SecondFactorRequest{ChallengeID: ch.ChallengeID, Code: totpCode(t, time.Now()), IP: testIP}); err != nil {
		t.Fatalf("first SecondFactor: %v", err)
	}
	// The same challenge id, with a fresh valid code, must now be refused.
	if _, err := auth.SecondFactor(ctx, db, login.SecondFactorRequest{ChallengeID: ch.ChallengeID, Code: totpCode(t, time.Now()), IP: testIP}); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Errorf("replayed challenge = %v, want ErrInvalidCredentials", err)
	}
}

func TestSecondFactorExpiredChallengeIsRefused(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	d := stdDeps(t)
	d.ttl = time.Minute

	now := time.Now().UTC().Truncate(time.Second)
	d.now = func() time.Time { return now }
	setPassword(t, db, "bob@example.com", "correct horse battery staple", testArgon2())
	enrolBob(t, db, d.totp)
	auth := newAuth(t, db, d)
	ctx := context.Background()

	ch, err := auth.Password(ctx, db, req("bob@example.com", "correct horse battery staple"))
	if err != nil {
		t.Fatalf("Password: %v", err)
	}
	// Move the clock well past the challenge's expiry.
	now = now.Add(time.Hour)

	if _, err := auth.SecondFactor(ctx, db, login.SecondFactorRequest{ChallengeID: ch.ChallengeID, Code: totpCode(t, now), IP: testIP}); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Errorf("expired challenge = %v, want ErrInvalidCredentials", err)
	}
}

func TestAccountLockoutAfterRepeatedFailures(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "alice@example.com", "correct horse battery staple", testArgon2())
	d := stdDeps(t)
	d.acct = lockout.Policy{MaxAttempts: 2, Window: 15 * time.Minute, Duration: time.Hour}
	auth := newAuth(t, db, d)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := auth.Password(ctx, db, req("alice@example.com", "wrong")); !errors.Is(err, login.ErrInvalidCredentials) {
			t.Fatalf("attempt %d = %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	// Locked now: even the correct password is refused before being checked.
	if _, err := auth.Password(ctx, db, req("alice@example.com", "correct horse battery staple")); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Errorf("locked account = %v, want ErrInvalidCredentials", err)
	}
}

func TestImpersonationByDomainAdmin(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "admin@example.com", "correct horse battery staple", testArgon2())
	d := stdDeps(t)
	d.pol = policy.Policy{Impersonation: policy.Impersonation{Allowed: true}}
	auth := newAuth(t, db, d)

	r := req("admin@example.com", "correct horse battery staple")
	r.Impersonating = "alice@example.com"
	got, err := auth.Password(context.Background(), db, r)
	if err != nil {
		t.Fatalf("Password: %v", err)
	}
	if got.Granted == nil {
		t.Fatal("an admin impersonation should grant")
	}
	if got.Granted.Session.Impersonating != "alice@example.com" {
		t.Errorf("session impersonating = %q, want alice@example.com", got.Granted.Session.Impersonating)
	}
	if got.Granted.Session.Address != "admin@example.com" {
		t.Errorf("session principal = %q, want the admin", got.Granted.Session.Address)
	}
}

func TestImpersonationDeniedWhenPolicyForbids(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "admin@example.com", "correct horse battery staple", testArgon2())
	// stdDeps has policy.Policy{}, i.e. Impersonation.Allowed is false.
	auth := newAuth(t, db, stdDeps(t))

	r := req("admin@example.com", "correct horse battery staple")
	r.Impersonating = "alice@example.com"
	if _, err := auth.Password(context.Background(), db, r); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Errorf("forbidden impersonation = %v, want ErrInvalidCredentials", err)
	}
}

func TestSuccessfulLoginRehashesAWeakHash(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	// Seed at the low test cost, then log in with an authenticator configured
	// for a higher cost: the successful sign-in must transparently upgrade it.
	setPassword(t, db, "alice@example.com", "correct horse battery staple", testArgon2())
	d := stdDeps(t)
	d.argon2 = strongArgon2()
	auth := newAuth(t, db, d)

	got, err := auth.Password(context.Background(), db, req("alice@example.com", "correct horse battery staple"))
	if err != nil {
		t.Fatalf("Password: %v", err)
	}
	if got.Granted == nil {
		t.Fatal("login should succeed")
	}
	if got.Granted.Rehash == "" {
		t.Fatal("a weaker hash should have been rehashed on sign-in")
	}
	// The stored hash is no longer weak relative to the configured cost.
	cred, err := store.NewCredentials().Get(context.Background(), db, "alice@example.com")
	if err != nil {
		t.Fatalf("reading credential: %v", err)
	}
	if weak, _ := password.NeedsRehash(cred.Hash, strongArgon2()); weak {
		t.Error("the stored hash is still weaker than configured after a successful login")
	}
}

// Impersonation is the most sensitive action the system performs, so a
// successful one leaves a row in the audit log. An impersonation with no record
// would leave an operator unable to answer "who was reading this person's mail".
func TestImpersonationIsAudited(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "admin@example.com", "correct horse battery staple", testArgon2())
	d := stdDeps(t)
	d.pol = policy.Policy{Impersonation: policy.Impersonation{Allowed: true}}
	auth := newAuth(t, db, d)

	r := req("admin@example.com", "correct horse battery staple")
	r.Impersonating = "alice@example.com"
	r.ImpersonationReason = "supporting a migration"
	if _, err := auth.Password(context.Background(), db, r); err != nil {
		t.Fatalf("Password: %v", err)
	}

	entries, err := store.NewAuditLog().List(context.Background(), db,
		store.AuditFilter{Action: login.ActionImpersonationStart})
	if err != nil {
		t.Fatalf("listing the audit log: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d impersonation.start entries, want exactly 1", len(entries))
	}
	e := entries[0]
	if e.ActorAddress == nil || *e.ActorAddress != "admin@example.com" {
		t.Errorf("actor = %v, want the admin", e.ActorAddress)
	}
	if e.TargetAddress == nil || *e.TargetAddress != "alice@example.com" {
		t.Errorf("target = %v, want alice", e.TargetAddress)
	}
	if !strings.Contains(string(e.Detail), "supporting a migration") {
		t.Errorf("detail = %s, want the reason to be recorded", e.Detail)
	}
}

// A login that is not an impersonation must not produce a record, or the audit
// log fills with noise that hides the real ones.
func TestOrdinaryLoginIsNotAuditedAsImpersonation(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "alice@example.com", "correct horse battery staple", testArgon2())
	d := stdDeps(t)
	d.pol = policy.Policy{Impersonation: policy.Impersonation{Allowed: true}}
	auth := newAuth(t, db, d)

	if _, err := auth.Password(context.Background(), db,
		req("alice@example.com", "correct horse battery staple")); err != nil {
		t.Fatalf("Password: %v", err)
	}
	n, err := store.NewAuditLog().CountFor(context.Background(), db, login.ActionImpersonationStart)
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d impersonation.start entries from an ordinary login, want 0", n)
	}
}

// With RequireGrant the configuration switch is not enough: the admin needs a
// live row naming the target. Without this check the grant table the admin API
// manages would be decorative.
func TestImpersonationRequiresALiveGrantWhenConfigured(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "admin@example.com", "correct horse battery staple", testArgon2())
	d := stdDeps(t)
	d.pol = policy.Policy{Impersonation: policy.Impersonation{Allowed: true, RequireGrant: true}}
	auth := newAuth(t, db, d)

	r := req("admin@example.com", "correct horse battery staple")
	r.Impersonating = "alice@example.com"
	if _, err := auth.Password(context.Background(), db, r); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Fatalf("impersonation without a grant = %v, want ErrInvalidCredentials", err)
	}

	grants := store.NewImpersonationGrants()
	if err := grants.Grant(context.Background(), db,
		"admin@example.com", "alice@example.com", "supporting a migration", nil); err != nil {
		t.Fatalf("granting: %v", err)
	}
	got, err := auth.Password(context.Background(), db, r)
	if err != nil {
		t.Fatalf("impersonation with a grant: %v", err)
	}
	if got.Granted == nil || got.Granted.Session.Impersonating != "alice@example.com" {
		t.Fatalf("want a granted impersonation session, got %+v", got.Granted)
	}

	// A revoked grant withdraws the ability, and it must take effect on the
	// next sign-in rather than when the session expires.
	if err := grants.Revoke(context.Background(), db,
		"admin@example.com", "alice@example.com"); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if _, err := auth.Password(context.Background(), db, r); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Errorf("impersonation after revocation = %v, want ErrInvalidCredentials", err)
	}
}

// A grant names one user. It must not become a key to the whole domain.
func TestAGrantDoesNotCoverAnotherTarget(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "admin@example.com", "correct horse battery staple", testArgon2())
	if err := store.NewImpersonationGrants().Grant(context.Background(), db,
		"admin@example.com", "alice@example.com", "supporting a migration", nil); err != nil {
		t.Fatalf("granting: %v", err)
	}
	d := stdDeps(t)
	d.pol = policy.Policy{Impersonation: policy.Impersonation{Allowed: true, RequireGrant: true}}
	auth := newAuth(t, db, d)

	r := req("admin@example.com", "correct horse battery staple")
	r.Impersonating = "bob@example.com"
	if _, err := auth.Password(context.Background(), db, r); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Errorf("impersonating an ungranted user = %v, want ErrInvalidCredentials", err)
	}
}

// A domain admin can never hold a mailbox, so naming yourself as the
// impersonation target is a sign-in with an extra word typed. It has to succeed
// as an ordinary sign-in rather than fail, and it must leave no audit entry:
// the alternative is a UI that reliably breaks when an admin selects themselves.
func TestAnAdminNeedNoGrantToActAsThemselves(t *testing.T) {
	db := testMigratedDB(t)
	seedFixture(t, db)
	setPassword(t, db, "admin@example.com", "correct horse battery staple", testArgon2())
	d := stdDeps(t)
	d.pol = policy.Policy{Impersonation: policy.Impersonation{Allowed: true, RequireGrant: true}}
	auth := newAuth(t, db, d)

	// No grant row exists for the self pair, and none is needed. The name is
	// spelled with different case on purpose: the comparison is
	// case-insensitive, so a lookup keyed on the raw string would miss and lock
	// the admin out of their own account.
	r := req("admin@example.com", "correct horse battery staple")
	r.Impersonating = "ADMIN@example.com"
	got, err := auth.Password(context.Background(), db, r)
	if err != nil {
		t.Fatalf("a self-impersonation was refused: %v", err)
	}
	if got.Granted == nil {
		t.Fatal("want a granted session")
	}
	if got.Granted.Session.Impersonating != "" {
		t.Errorf("session impersonating = %q, want it cleared", got.Granted.Session.Impersonating)
	}
	if got.Granted.Session.Address != "admin@example.com" {
		t.Errorf("session principal = %q, want the admin", got.Granted.Session.Address)
	}
	n, err := store.NewAuditLog().CountFor(context.Background(), db, login.ActionImpersonationStart)
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Errorf("a self-impersonation wrote %d audit entries, want 0", n)
	}
}

// New must refuse a configuration that demands a grant without the repository
// that holds one, rather than failing at the first impersonation.
func TestNewRejectsRequireGrantWithoutTheRepository(t *testing.T) {
	d := stdDeps(t)
	d.pol = policy.Policy{Impersonation: policy.Impersonation{Allowed: true, RequireGrant: true}}
	opts := optionsFor(t, d)
	opts.Grants = nil
	if _, err := login.New(opts); err == nil {
		t.Error("New accepted RequireGrant with no grant repository")
	}
}

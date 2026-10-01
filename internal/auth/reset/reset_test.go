package reset_test

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

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/reset"
	"github.com/linchange/linchange/internal/store"
)

const (
	testDBURLEnv   = "LINCHANGE_TEST_DATABASE_URL"
	defaultTestURL = "postgres://linchange:linchange@127.0.0.1:55432/linchange?sslmode=disable"

	testAddress   = "alice@example.com"
	testSecondary = "alice.other@example.com"
)

var testIP = netip.MustParseAddr("203.0.113.20")

// testMigratedDB gives each test a throwaway, fully-migrated database so rows
// left behind can never leak into another test. It mirrors the store and login
// harnesses, because the reset package has to open its own connection.
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
		ApplicationName: "linchange-reset-test-admin",
	})
	if err != nil {
		if os.Getenv(testDBURLEnv) != "" {
			t.Fatalf("%s is set to %q but is unreachable: %v", testDBURLEnv, base, err)
		}
		t.Skipf("no test database at %s (%v); run `make up` or set %s", testDBURLEnv, err, testDBURLEnv)
	}

	name := fmt.Sprintf("lc_reset_test_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	db, err := store.Open(ctx, store.Options{
		URL: replaceDatabase(base, name), MaxOpen: 8, MaxIdle: 4,
		ConnectTimeout: 10 * time.Second, ApplicationName: "linchange-reset-test",
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

// localPart is the part of an address before the domain, for users.address_local.
func localPart(address string) string {
	if i := strings.IndexByte(address, '@'); i >= 0 {
		return address[:i]
	}
	return address
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

// seedUser creates the example.com domain and one principal.
func seedUser(t *testing.T, db *sqlx.DB, address, source string, mailbox, enabled bool) {
	t.Helper()
	exec(t, db, `INSERT INTO domains (name) VALUES ('example.com') ON CONFLICT DO NOTHING`)
	exec(t, db,
		`INSERT INTO users (address, address_local, source, mailbox_enabled, enabled)
		 VALUES ($1, $2, $3, $4, $5)`,
		address, localPart(address), source, mailbox, enabled)
}

// addSecondaryEmail gives an address an out-of-band email.
func addSecondaryEmail(t *testing.T, db *sqlx.DB, address, email string, verified, isPrimary bool) {
	t.Helper()
	var at any
	if verified {
		at = time.Now()
	}
	exec(t, db,
		`INSERT INTO user_secondary_emails (address, email, is_primary, verified_at)
		 VALUES ($1, $2, $3, $4)`, address, email, isPrimary, at)
}

func testArgon2Params() config.Argon2Params {
	return config.Argon2Params{MemoryKib: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
}

// sinkerRecorder stands in for the email transport and records what it was
// asked to send.
type sinkerRecorder struct {
	calls int
	to    string
	token string
}

func (r *sinkerRecorder) sink(_ context.Context, to, token string, _ time.Time) error {
	r.calls++
	r.to = to
	r.token = token
	return nil
}

// newResetter builds a resetter with the default dependencies and a test
// Argon2 cost; pass a nil sinker to model "no transport yet".
func newResetter(t *testing.T, db *sqlx.DB, enabled bool, sinker reset.Sinker, now func() time.Time) *reset.Resetter {
	t.Helper()
	r, err := reset.New(reset.Options{
		SecondaryEmails:   store.NewSecondaryEmails(),
		Tokens:            store.NewResetTokens(),
		Credentials:       store.NewCredentials(),
		Sessions:          store.NewSessions(),
		Sinker:            sinker,
		Enabled:           enabled,
		Argon2:            testArgon2Params(),
		MinPasswordLength: 12,
		TokenTTL:          5 * time.Minute,
		Now:               now,
	})
	if err != nil {
		t.Fatalf("building the resetter: %v", err)
	}
	return r
}

func TestRequestIssuesLinkToVerifiedSecondary(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, testAddress, "local", true, true)
	addSecondaryEmail(t, db, testAddress, testSecondary, true, true)

	rec := &sinkerRecorder{}
	r := newResetter(t, db, true, rec.sink, time.Now)

	got, err := r.Request(context.Background(), db, reset.Request{Address: testAddress, IP: testIP})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !got.Delivered || got.Token == "" || got.SentTo != testSecondary {
		t.Fatalf("want a delivered link to %s, got %+v", testSecondary, got)
	}
	if rec.calls != 1 || rec.token != got.Token || rec.to != testSecondary {
		t.Fatalf("the transport was not called once with the token: %+v", rec)
	}
	if !got.ExpiresAt.After(time.Now()) {
		t.Fatalf("the link should expire in the future: %v", got.ExpiresAt)
	}
}

func TestRequestUnknownAddressDeliversNothing(t *testing.T) {
	db := testMigratedDB(t)
	rec := &sinkerRecorder{}
	r := newResetter(t, db, true, rec.sink, time.Now)

	got, err := r.Request(context.Background(), db, reset.Request{Address: "ghost@example.com", IP: testIP})
	if err != nil {
		t.Fatalf("Request for an unknown address must not error: %v", err)
	}
	if got.Delivered || got.Token != "" || rec.calls != 0 {
		t.Fatalf("an unknown address must not send a link: %+v (calls=%d)", got, rec.calls)
	}
}

func TestRequestWithoutVerifiedSecondaryDeliversNothing(t *testing.T) {
	db := testMigratedDB(t)
	// A reset-able local account, but with no out-of-band address at all.
	seedUser(t, db, testAddress, "local", true, true)
	rec := &sinkerRecorder{}
	r := newResetter(t, db, true, rec.sink, time.Now)

	got, err := r.Request(context.Background(), db, reset.Request{Address: testAddress, IP: testIP})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got.Delivered || rec.calls != 0 {
		t.Fatalf("no verified secondary means no link: %+v", got)
	}
}

func TestRequestSkipsUnverifiedSecondary(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, testAddress, "local", true, true)
	// Only an unverified address is on file.
	addSecondaryEmail(t, db, testAddress, testSecondary, false, true)
	rec := &sinkerRecorder{}
	r := newResetter(t, db, true, rec.sink, time.Now)

	got, err := r.Request(context.Background(), db, reset.Request{Address: testAddress, IP: testIP})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got.Delivered || rec.calls != 0 {
		t.Fatalf("an unverified secondary must not receive a credential link: %+v", got)
	}
}

func TestRequestDisabledDeliversNothing(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, testAddress, "local", true, true)
	addSecondaryEmail(t, db, testAddress, testSecondary, true, true)
	rec := &sinkerRecorder{}
	r := newResetter(t, db, false, rec.sink, time.Now)

	got, err := r.Request(context.Background(), db, reset.Request{Address: testAddress, IP: testIP})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got.Delivered || rec.calls != 0 {
		t.Fatalf("a disabled reset must not send a link: %+v", got)
	}
}

func TestRequestLDAPUserDeliversNothing(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, "dana@example.com", "ldap", true, true)
	addSecondaryEmail(t, db, "dana@example.com", "dana.other@example.com", true, true)
	rec := &sinkerRecorder{}
	r := newResetter(t, db, true, rec.sink, time.Now)

	got, err := r.Request(context.Background(), db, reset.Request{Address: "dana@example.com", IP: testIP})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got.Delivered || rec.calls != 0 {
		t.Fatalf("a directory user's password is not reset here: %+v", got)
	}
}

func TestRequestPrefersPrimaryVerified(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, testAddress, "local", true, true)
	addSecondaryEmail(t, db, testAddress, "alice.secondary@example.com", true, false)
	addSecondaryEmail(t, db, testAddress, testSecondary, true, true)
	rec := &sinkerRecorder{}
	r := newResetter(t, db, true, rec.sink, time.Now)

	got, err := r.Request(context.Background(), db, reset.Request{Address: testAddress, IP: testIP})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got.SentTo != testSecondary {
		t.Fatalf("the primary verified email must win, got %q", got.SentTo)
	}
}

func TestCompleteSetsPasswordClearsFlagAndRevokesSessions(t *testing.T) {
	ctx := context.Background()
	db := testMigratedDB(t)
	seedUser(t, db, testAddress, "local", true, true)
	addSecondaryEmail(t, db, testAddress, testSecondary, true, true)
	// Start from a forced-change state and an open session, so the reset's
	// side effects are observable.
	exec(t, db, `UPDATE users SET must_change_password = true WHERE address = $1`, testAddress)
	sessions := store.NewSessions()
	created, err := sessions.Create(ctx, db, store.CreateSessionRequest{
		Address: testAddress, IP: testIP, UserAgent: "reset-test",
	})
	if err != nil {
		t.Fatalf("creating a session: %v", err)
	}

	rec := &sinkerRecorder{}
	r := newResetter(t, db, true, rec.sink, time.Now)
	got, err := r.Request(ctx, db, reset.Request{Address: testAddress, IP: testIP})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	const newPassword = "correct horse battery staple"
	if _, err := r.Complete(ctx, db, reset.CompleteRequest{Token: got.Token, NewPassword: newPassword}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	cred, err := store.NewCredentials().Get(ctx, db, testAddress)
	if err != nil {
		t.Fatalf("reading the new credential: %v", err)
	}
	if ok, err := password.Verify(newPassword, cred.Hash); err != nil || !ok {
		t.Fatalf("the stored password does not verify: ok=%v err=%v", ok, err)
	}

	var must bool
	if err := db.GetContext(ctx, &must, `SELECT must_change_password FROM users WHERE address = $1`, testAddress); err != nil {
		t.Fatalf("reading must_change_password: %v", err)
	}
	if must {
		t.Error("a successful reset must clear the forced-change flag")
	}

	if _, err := sessions.Load(ctx, db, created.Session.ID); !errors.Is(err, store.ErrSessionRevoked) {
		t.Errorf("an open session should be revoked, got %v", err)
	}
}

func TestCompleteInvalidTokenIsRefused(t *testing.T) {
	ctx := context.Background()
	db := testMigratedDB(t)
	seedUser(t, db, testAddress, "local", true, true)
	addSecondaryEmail(t, db, testAddress, testSecondary, true, true)
	r := newResetter(t, db, true, nil, time.Now)

	_, err := r.Complete(ctx, db, reset.CompleteRequest{Token: "not-a-real-token", NewPassword: "correct horse battery staple"})
	if !errors.Is(err, reset.ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}

func TestCompleteExpiredTokenIsRefused(t *testing.T) {
	ctx := context.Background()
	db := testMigratedDB(t)
	seedUser(t, db, testAddress, "local", true, true)
	addSecondaryEmail(t, db, testAddress, testSecondary, true, true)

	start := time.Now()
	clock := &start
	rec := &sinkerRecorder{}
	r := newResetter(t, db, true, rec.sink, func() time.Time { return *clock })

	got, err := r.Request(ctx, db, reset.Request{Address: testAddress, IP: testIP})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	*clock = start.Add(10 * time.Minute)
	_, err = r.Complete(ctx, db, reset.CompleteRequest{Token: got.Token, NewPassword: "correct horse battery staple"})
	if !errors.Is(err, reset.ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestCompleteAlreadyUsedIsRefused(t *testing.T) {
	ctx := context.Background()
	db := testMigratedDB(t)
	seedUser(t, db, testAddress, "local", true, true)
	addSecondaryEmail(t, db, testAddress, testSecondary, true, true)
	r := newResetter(t, db, true, nil, time.Now)

	got, err := r.Request(ctx, db, reset.Request{Address: testAddress, IP: testIP})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	const newPassword = "correct horse battery staple"
	if _, err := r.Complete(ctx, db, reset.CompleteRequest{Token: got.Token, NewPassword: newPassword}); err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	// A replay of the same link must not set a second password.
	if _, err := r.Complete(ctx, db, reset.CompleteRequest{Token: got.Token, NewPassword: "another valid password 1"}); !errors.Is(err, reset.ErrAlreadyUsed) {
		t.Fatalf("want ErrAlreadyUsed, got %v", err)
	}
}

func TestCompleteWeakPasswordKeepsTokenUsable(t *testing.T) {
	ctx := context.Background()
	db := testMigratedDB(t)
	seedUser(t, db, testAddress, "local", true, true)
	addSecondaryEmail(t, db, testAddress, testSecondary, true, true)
	r := newResetter(t, db, true, nil, time.Now)

	got, err := r.Request(ctx, db, reset.Request{Address: testAddress, IP: testIP})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// Too short: refused, and the token must survive so the user can retry.
	if _, err := r.Complete(ctx, db, reset.CompleteRequest{Token: got.Token, NewPassword: "short"}); err == nil {
		t.Fatal("a short password must be refused")
	}
	if _, err := r.Complete(ctx, db, reset.CompleteRequest{Token: got.Token, NewPassword: "correct horse battery staple"}); err != nil {
		t.Fatalf("the token should still be usable after a rejected password: %v", err)
	}
}

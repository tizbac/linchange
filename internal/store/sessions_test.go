package store_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/secret"
	"github.com/linchange/linchange/internal/store"
)

// The session table is what makes an access token revocable. An access token is
// self-contained so a request need not hit the database to learn who is asking,
// which means it cannot be withdrawn once issued. The session id in its sid
// claim is the handle back to the row that can still say no.
//
// The tests below cover the properties that handle has to have. Most of them are
// about refusal: a session that keeps working after a logout, a password change
// or a disable is a security bug, and each of those is a separate path through
// this code.

func seedSessionFixtures(t *testing.T, db *sqlx.DB) {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO domains (name) VALUES ('example.com')`); err != nil {
		t.Fatalf("seeding domain: %v", err)
	}
	for _, u := range []struct {
		address, local, source string
	}{
		{"alice@example.com", "alice", "local"},
		{"bob@example.com", "bob", "local"},
		{"support@example.com", "support", "shared"},
		// A domain admin administers mail and never receives it, so the row has
		// to say so explicitly; the schema rejects the alternative.
		{"admin@example.com", "admin", "domain_admin"},
	} {
		mailbox := u.source != "domain_admin"
		if _, err := db.ExecContext(ctx,
			`INSERT INTO users (address, address_local, source, mailbox_enabled)
			 VALUES ($1,$2,$3,$4)`,
			u.address, u.local, u.source, mailbox); err != nil {
			t.Fatalf("seeding user %s: %v", u.address, err)
		}
	}
}

func createSession(t *testing.T, db *sqlx.DB, address string) store.CreatedSession {
	t.Helper()
	created, err := store.NewSessions().Create(context.Background(), db,
		store.CreateSessionRequest{
			Address:   address,
			IP:        netip.MustParseAddr("203.0.113.5"),
			UserAgent: "Mozilla/5.0 (test)",
		})
	if err != nil {
		t.Fatalf("creating a session for %s: %v", address, err)
	}
	return created
}

func TestCreateSessionReturnsATokenAndStoresOnlyItsDigest(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()

	created := createSession(t, db, "alice@example.com")
	if created.RefreshToken == "" {
		t.Fatal("Create returned no refresh token")
	}
	if created.Session.ID == "" {
		t.Fatal("Create returned no session id")
	}
	if created.Session.ID == created.RefreshToken {
		t.Error("the session id is the refresh token, so the id is a credential")
	}
	if !created.Session.Live(time.Now()) {
		t.Error("a freshly created session does not report itself live")
	}
	if created.Session.Principal.Address != "alice@example.com" {
		t.Errorf("principal address = %q", created.Session.Principal.Address)
	}

	// The row must hold the digest, not the token. This is the property that
	// makes a database dump useless to whoever gets one.
	var stored []byte
	if err := db.GetContext(ctx, &stored,
		`SELECT refresh_token_hash FROM sessions WHERE id = $1`, created.Session.ID); err != nil {
		t.Fatalf("reading the stored digest: %v", err)
	}
	if string(stored) == created.RefreshToken {
		t.Fatal("the refresh token is stored in the clear")
	}
	if !secret.Equal(created.RefreshToken, stored) {
		t.Error("the stored digest does not match the returned token")
	}
}

func TestCreateSessionForAnUnknownOrDisabledUser(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	sessions := store.NewSessions()

	if _, err := sessions.Create(ctx, db,
		store.CreateSessionRequest{Address: "nobody@example.com"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a session was created for an unknown address: %v", err)
	}

	if _, err := sessions.Create(ctx, db, store.CreateSessionRequest{Address: ""}); err == nil {
		t.Error("a session was created with no address")
	}

	// A disabled account must be refused, and reported as missing rather than
	// as a principal that merely needs a different password.
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET enabled = false WHERE address = 'bob@example.com'`); err != nil {
		t.Fatalf("disabling bob: %v", err)
	}
	if _, err := sessions.Create(ctx, db,
		store.CreateSessionRequest{Address: "bob@example.com"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a session was created for a disabled account: %v", err)
	}
}

func TestCreateSessionRefusesAnEmptyAddress(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)

	if _, err := store.NewSessions().Create(context.Background(), db,
		store.CreateSessionRequest{Address: ""}); err == nil {
		t.Error("an empty address was accepted")
	}
}

// A domain admin can sign in, and can sign in as somebody else. An ordinary
// user cannot, or the impersonation would be a way to read another mailbox
// without an audit trail.
func TestImpersonationIsReservedToDomainAdmins(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	sessions := store.NewSessions()

	admin, err := sessions.Create(ctx, db, store.CreateSessionRequest{
		Address:       "admin@example.com",
		Impersonating: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("a domain admin could not create an impersonation session: %v", err)
	}
	if admin.Session.Impersonating != "alice@example.com" {
		t.Errorf("impersonating = %q", admin.Session.Impersonating)
	}

	if _, err := sessions.Create(ctx, db, store.CreateSessionRequest{
		Address:       "alice@example.com",
		Impersonating: "bob@example.com",
	}); err == nil {
		t.Error("an ordinary user created an impersonation session")
	}

	// There is nothing to impersonate into for a principal that cannot hold
	// mail, so pointing an admin at another admin is refused rather than
	// producing a session that can do nothing.
	if _, err := sessions.Create(ctx, db, store.CreateSessionRequest{
		Address:       "admin@example.com",
		Impersonating: "admin@example.com",
	}); !errors.Is(err, store.ErrNoMailbox) {
		t.Errorf("impersonating a principal that cannot hold mail: %v", err)
	}
}

func TestLoadReturnsAFreshPrincipal(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	created := createSession(t, db, "alice@example.com")

	// Change the user underneath the session, the way an administrator
	// forcing a password reset would.
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET must_change_password = true WHERE address = 'alice@example.com'`); err != nil {
		t.Fatalf("setting must_change_password: %v", err)
	}

	loaded, err := store.NewSessions().Load(ctx, db, created.Session.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.Principal.MustChangePassword {
		t.Error("Load returned a stale principal: a token minted before the " +
			"change would still have said no password change was needed")
	}
	// The recorded address and user agent come back for the signed-in-as view.
	if loaded.IP.String() != "203.0.113.5" {
		t.Errorf("IP = %v", loaded.IP)
	}
	if loaded.UserAgent != "Mozilla/5.0 (test)" {
		t.Errorf("UserAgent = %q", loaded.UserAgent)
	}
}

func TestLoadAdvancesLastSeen(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	created := createSession(t, db, "alice@example.com")

	// Backdate the session so the update is observable.
	if _, err := db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = now() - interval '1 hour'
		 WHERE id = $1`, created.Session.ID); err != nil {
		t.Fatalf("backdating last_seen_at: %v", err)
	}

	loaded, err := store.NewSessions().Load(ctx, db, created.Session.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.LastSeenAt.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("last_seen_at was not advanced: %v", loaded.LastSeenAt)
	}
}

func TestLoadRefusesARevokedSession(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	sessions := store.NewSessions()
	created := createSession(t, db, "alice@example.com")

	if err := sessions.Revoke(ctx, db, created.Session.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := sessions.Load(ctx, db, created.Session.ID); !errors.Is(err, store.ErrSessionRevoked) {
		t.Errorf("a revoked session was loaded: %v", err)
	}

	// Logging out twice is not a user-visible error, so a second revoke is
	// accepted and changes nothing.
	if err := sessions.Revoke(ctx, db, created.Session.ID); err != nil {
		t.Errorf("revoking an already revoked session: %v", err)
	}
}

func TestLoadRefusesAnExpiredSession(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()

	// A deadline in the past, reached by waiting rather than by editing the row,
	// so the test does not depend on how the repository computes the deadline.
	created, err := store.NewSessions().Create(ctx, db, store.CreateSessionRequest{
		Address:     "alice@example.com",
		AbsoluteTTL: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	if _, err := store.NewSessions().Load(ctx, db, created.Session.ID); !errors.Is(err, store.ErrSessionExpired) {
		t.Errorf("an expired session was loaded: %v", err)
	}
}

func TestLoadRefusesAnIdleSession(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	created := createSession(t, db, "alice@example.com")

	// Backdate past the idle deadline rather than waiting for it, which would
	// take two weeks.
	if _, err := db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = now() - interval '400 days'
		 WHERE id = $1`, created.Session.ID); err != nil {
		t.Fatalf("backdating last_seen_at: %v", err)
	}

	if _, err := store.NewSessions().Load(ctx, db, created.Session.ID); !errors.Is(err, store.ErrSessionExpired) {
		t.Errorf("an idle session was loaded: %v", err)
	}
}

// A disabled account has to stop working immediately, not when its token
// expires. This is the whole reason Load joins the users table.
func TestLoadRefusesASessionWhoseUserWasDisabled(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	created := createSession(t, db, "alice@example.com")

	if _, err := db.ExecContext(ctx,
		`UPDATE users SET enabled = false WHERE address = 'alice@example.com'`); err != nil {
		t.Fatalf("disabling alice: %v", err)
	}

	if _, err := store.NewSessions().Load(ctx, db, created.Session.ID); !errors.Is(err, store.ErrSessionRevoked) {
		t.Errorf("a session for a disabled user was loaded: %v", err)
	}
}

func TestLoadForAnUnknownOrEmptySession(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()

	for _, id := range []string{"", "no-such-session"} {
		if _, err := store.NewSessions().Load(ctx, db, id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Load(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestRefreshRequiresTheMatchingToken(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	sessions := store.NewSessions()
	created := createSession(t, db, "alice@example.com")

	loaded, err := sessions.LoadForRefresh(ctx, db, created.Session.ID, created.RefreshToken)
	if err != nil {
		t.Fatalf("LoadForRefresh with the right token: %v", err)
	}
	if loaded.ID != created.Session.ID {
		t.Errorf("session id = %q", loaded.ID)
	}
}

// A session id on its own must not be enough. It travels inside every access
// token, so treating it as a credential would mean any reader of a token could
// mint a new one.
func TestRefreshTokenIsRequiredNotJustTheSessionID(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	created := createSession(t, db, "alice@example.com")

	// A blank token is a malformed request rather than a mismatch, and it is
	// reported as missing so that it cannot reach the branch which ends the
	// session. Otherwise anyone who knew a session id could log a user out by
	// sending an empty refresh.
	if _, err := store.NewSessions().LoadForRefresh(ctx, db, created.Session.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LoadForRefresh with a blank token = %v, want ErrNotFound", err)
	}
	if _, err := store.NewSessions().Load(ctx, db, created.Session.ID); err != nil {
		t.Errorf("a blank refresh attempt ended the session: %v", err)
	}

	for _, bad := range []string{"not-the-token", created.Session.ID} {
		if _, err := store.NewSessions().LoadForRefresh(ctx, db, created.Session.ID, bad); !errors.Is(err, store.ErrSessionTokenMismatch) {
			t.Errorf("LoadForRefresh with %q = %v, want ErrSessionTokenMismatch", bad, err)
		}
	}

	// The first mismatch already ended the session, which is the point: the
	// server cannot tell the impostor from the real user, so it ends it for
	// both and lets the real user notice.
	if _, err := store.NewSessions().Load(ctx, db, created.Session.ID); !errors.Is(err, store.ErrSessionRevoked) {
		t.Errorf("a session survived a mismatched refresh token: %v", err)
	}
}

func TestRotateInvalidatesTheOldRefreshToken(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	sessions := store.NewSessions()
	created := createSession(t, db, "alice@example.com")

	rotated, err := sessions.Rotate(ctx, db, created.Session.ID)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rotated == created.RefreshToken {
		t.Fatal("Rotate returned the same token, so rotation changed nothing")
	}

	// The new token works, which is the point of returning it.
	if _, err := sessions.LoadForRefresh(ctx, db, created.Session.ID, rotated); err != nil {
		t.Errorf("the rotated token was refused: %v", err)
	}

	// And the old one no longer does.
	if _, err := sessions.LoadForRefresh(ctx, db, created.Session.ID, created.RefreshToken); !errors.Is(err, store.ErrSessionTokenMismatch) {
		t.Errorf("the superseded token still worked: %v", err)
	}
}

func TestRotateRefusesARevokedSession(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	sessions := store.NewSessions()
	created := createSession(t, db, "alice@example.com")

	if err := sessions.Revoke(ctx, db, created.Session.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := sessions.Rotate(ctx, db, created.Session.ID); !errors.Is(err, store.ErrSessionRevoked) {
		t.Errorf("Rotate on a revoked session = %v, want ErrSessionRevoked", err)
	}
}

// A password change is only a security measure if it ends the sessions that
// were opened with the old credential.
func TestRevokeAllForAddressEndsEveryLiveSession(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	sessions := store.NewSessions()

	first := createSession(t, db, "alice@example.com")
	second := createSession(t, db, "alice@example.com")
	other := createSession(t, db, "bob@example.com")

	ended, err := sessions.RevokeAllForAddress(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("RevokeAllForAddress: %v", err)
	}
	if ended != 2 {
		t.Errorf("ended %d sessions, want 2", ended)
	}

	for _, id := range []string{first.Session.ID, second.Session.ID} {
		if _, err := sessions.Load(ctx, db, id); !errors.Is(err, store.ErrSessionRevoked) {
			t.Errorf("session %s survived a bulk revoke: %v", id, err)
		}
	}
	// Somebody else's session is untouched, or one user's password change would
	// log out the whole domain.
	if _, err := sessions.Load(ctx, db, other.Session.ID); err != nil {
		t.Errorf("another user's session was revoked: %v", err)
	}

	// A second call has nothing left to end.
	ended, err = sessions.RevokeAllForAddress(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("RevokeAllForAddress: %v", err)
	}
	if ended != 0 {
		t.Errorf("a repeated revoke ended %d sessions", ended)
	}
}

func TestListForAddressShowsOnlyLiveSessions(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	sessions := store.NewSessions()

	first := createSession(t, db, "alice@example.com")
	second := createSession(t, db, "alice@example.com")
	if err := sessions.Revoke(ctx, db, second.Session.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	listed, err := sessions.ListForAddress(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("ListForAddress: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d sessions, want 1", len(listed))
	}
	if listed[0].ID != first.Session.ID {
		t.Errorf("listed session %s, want %s", listed[0].ID, first.Session.ID)
	}
}

func TestPurgeExpiredKeepsLiveSessions(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	sessions := store.NewSessions()

	live := createSession(t, db, "alice@example.com")
	revoked := createSession(t, db, "alice@example.com")
	expired := createSession(t, db, "bob@example.com")

	if err := sessions.Revoke(ctx, db, revoked.Session.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// A revocation is kept for a week so a user can see that a session ended,
	// and the recently revoked row must survive this purge.
	if _, err := db.ExecContext(ctx,
		`UPDATE sessions SET absolute_expires_at = now() - interval '1 day'
		 WHERE id = $1`, revoked.Session.ID); err != nil {
		t.Fatalf("ageing the revoked session: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE sessions SET absolute_expires_at = now() - interval '1 day'
		 WHERE id = $1`, expired.Session.ID); err != nil {
		t.Fatalf("ageing the expired session: %v", err)
	}

	purged, err := sessions.PurgeExpired(ctx, db)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if purged != 2 {
		t.Errorf("purged %d sessions, want 2", purged)
	}
	if _, err := sessions.Load(ctx, db, live.Session.ID); err != nil {
		t.Errorf("a live session was purged: %v", err)
	}
}

// LoadPrincipal is what the credential paths call, and it deliberately reports a
// disabled user as missing so a caller cannot mistake "disabled" for "wrong
// password".
func TestLoadPrincipal(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()

	p, err := store.LoadPrincipal(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("LoadPrincipal: %v", err)
	}
	if p.Address != "alice@example.com" || p.Source != policy.SourceLocal {
		t.Errorf("principal = %+v", p)
	}
	if !p.MailboxEnabled {
		t.Error("alice should be able to hold mail")
	}
	if p.Authenticated {
		t.Error("reading a user from the database must not mark it authenticated")
	}

	// A domain admin can sign in but cannot hold mail, and the row has to say so
	// for a policy check made without a database read to reach the right answer.
	admin, err := store.LoadPrincipal(ctx, db, "admin@example.com")
	if err != nil {
		t.Fatalf("LoadPrincipal for the admin: %v", err)
	}
	if admin.Source != policy.SourceDomainAdmin {
		t.Errorf("source = %q", admin.Source)
	}
	if admin.MailboxEnabled {
		t.Error("a domain admin was loaded as able to hold mail")
	}

	// A row read is not a credential, so every action is still denied. The
	// credential paths are what set Authenticated, and a caller that skipped
	// them gets a principal that authorises nothing rather than everything.
	if err := admin.Can(policy.ActionAdminister); !policy.IsDenied(err, policy.CodeNotAuthenticated) {
		t.Errorf("an unauthenticated principal authorised something: %v", err)
	}

	// Once the credential path has authenticated it, the same row authorises
	// administration and still refuses mail.
	admin.Authenticated = true
	if err := admin.Can(policy.ActionAdminister); err != nil {
		t.Errorf("an authenticated domain admin could not administer: %v", err)
	}
	if err := admin.Can(policy.ActionIMAP); !policy.IsDenied(err, policy.CodeNoMailbox) {
		t.Errorf("an authenticated domain admin was allowed to hold mail: %v", err)
	}

	if _, err := store.LoadPrincipal(ctx, db, "nobody@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LoadPrincipal for an unknown address = %v", err)
	}
	if _, err := store.LoadPrincipal(ctx, db, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LoadPrincipal for an empty address = %v", err)
	}
}

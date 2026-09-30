package store_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/linchange/linchange/internal/auth/lockout"
	"github.com/linchange/linchange/internal/store"
)

// The per-account rows are foreign keyed to users, which is the whole reason
// they are not simply keyed by the address string: an LDAP user has no
// credentials row but does need a limit, and an address that does not exist must
// not be able to create a row.
func TestAccountAttemptsRequireARealUser(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	attempts := store.NewAuthAttempts()

	if err := attempts.RecordAccountFailure(ctx, db, "nobody@example.com",
		lockout.State{FailedAttempts: 1, WindowStartedAt: time.Now()}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a row was recorded for an address with no user: %v", err)
	}

	// An existing principal is recorded, including an LDAP one, whose lack of a
	// credentials row is exactly why the state lives in its own table.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (address, address_local, source) VALUES ('dana@example.com','dana','ldap')`); err != nil {
		t.Fatalf("seeding an LDAP user: %v", err)
	}
	for _, address := range []string{"alice@example.com", "dana@example.com"} {
		if err := attempts.RecordAccountFailure(ctx, db, address,
			lockout.State{FailedAttempts: 3, WindowStartedAt: time.Now()}); err != nil {
			t.Fatalf("recording a failure for %s: %v", address, err)
		}
		s, err := attempts.AccountState(ctx, db, address)
		if err != nil {
			t.Fatalf("AccountState(%s): %v", address, err)
		}
		if s.FailedAttempts != 3 {
			t.Errorf("%s failed attempts = %d, want 3", address, s.FailedAttempts)
		}
	}
}

// An address with no row is a clean slate, not an error, so a caller does not
// have to distinguish "never failed" from "pruned".
func TestAccountStateForAnUnknownAddressIsClean(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)

	s, err := store.NewAuthAttempts().AccountState(context.Background(), db, "nobody@example.com")
	if err != nil {
		t.Fatalf("AccountState: %v", err)
	}
	if s != (lockout.State{}) {
		t.Errorf("state = %+v, want the zero state", s)
	}
}

func TestRecordAccountFailureUpserts(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	attempts := store.NewAuthAttempts()

	now := time.Now()
	for i := 1; i <= 3; i++ {
		if err := attempts.RecordAccountFailure(ctx, db, "alice@example.com",
			lockout.State{FailedAttempts: i, WindowStartedAt: now}); err != nil {
			t.Fatalf("recording failure %d: %v", i, err)
		}
	}

	var count int
	if err := db.GetContext(ctx, &count,
		`SELECT count(*) FROM auth_attempts WHERE address = 'alice@example.com'`); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 1 {
		t.Errorf("recording three failures left %d rows, want 1", count)
	}

	s, err := attempts.AccountState(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("AccountState: %v", err)
	}
	if s.FailedAttempts != 3 {
		t.Errorf("failed attempts = %d, want 3", s.FailedAttempts)
	}
}

// A lock is stored as a timestamp and read back as one, and a subject with no
// lock reads back as the zero time rather than as some ancient date.
func TestAccountLockRoundTrips(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	attempts := store.NewAuthAttempts()

	locked := time.Now().Add(15 * time.Minute).Truncate(time.Microsecond)
	if err := attempts.RecordAccountFailure(ctx, db, "alice@example.com",
		lockout.State{FailedAttempts: 5, WindowStartedAt: time.Now(), LockedUntil: locked}); err != nil {
		t.Fatalf("recording: %v", err)
	}

	s, err := attempts.AccountState(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("AccountState: %v", err)
	}
	if !s.LockedUntil.Equal(locked) {
		t.Errorf("locked until %v, want %v", s.LockedUntil, locked)
	}
	if !s.Locked(locked.Add(-time.Minute)) {
		t.Error("the stored lock is not reported as a lock")
	}
	if s.Locked(locked.Add(time.Minute)) {
		t.Error("the stored lock is still in force after it should have lifted")
	}

	// Storing no lock must come back as the zero time, not as a NULL that
	// scans as something else.
	if err := attempts.RecordAccountFailure(ctx, db, "alice@example.com",
		lockout.State{FailedAttempts: 1, WindowStartedAt: time.Now()}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	s, err = attempts.AccountState(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("AccountState: %v", err)
	}
	if !s.LockedUntil.IsZero() {
		t.Errorf("locked until %v, want the zero time", s.LockedUntil)
	}
	if s.Locked(time.Now()) {
		t.Error("a subject with no stored lock reports itself locked")
	}
}

func TestResetAccountClearsTheHistory(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	attempts := store.NewAuthAttempts()

	if err := attempts.RecordAccountFailure(ctx, db, "alice@example.com",
		lockout.State{FailedAttempts: 5, WindowStartedAt: time.Now(),
			LockedUntil: time.Now().Add(15 * time.Minute)}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if err := attempts.ResetAccount(ctx, db, "alice@example.com"); err != nil {
		t.Fatalf("ResetAccount: %v", err)
	}
	s, err := attempts.AccountState(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("AccountState: %v", err)
	}
	if s != (lockout.State{}) {
		t.Errorf("state after reset = %+v, want clean", s)
	}

	// Resetting an address with no history is not an error.
	if err := attempts.ResetAccount(ctx, db, "alice@example.com"); err != nil {
		t.Errorf("ResetAccount on a clean address: %v", err)
	}
}

// The per-address table is not foreign keyed, so it accepts an address that has
// never been seen, and its rows go away with the pruning an operator schedules.
func TestIPAttemptsAreRecordedAndPruned(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	attempts := store.NewAuthAttempts()
	ip := netip.MustParseAddr("198.51.100.7")

	if err := attempts.RecordIPFailure(ctx, db, ip,
		lockout.State{FailedAttempts: 20, WindowStartedAt: time.Now(),
			LockedUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("RecordIPFailure: %v", err)
	}

	s, err := attempts.IPState(ctx, db, ip)
	if err != nil {
		t.Fatalf("IPState: %v", err)
	}
	if s.FailedAttempts != 20 || !s.Locked(time.Now()) {
		t.Errorf("state = %+v, want 20 failures and a lock", s)
	}

	// A fresh address reads clean.
	other := netip.MustParseAddr("198.51.100.8")
	if s, err := attempts.IPState(ctx, db, other); err != nil || s != (lockout.State{}) {
		t.Errorf("IPState for an unseen address = %+v, %v", s, err)
	}

	// Pruning with a zero age removes everything, which is the operator asking
	// for a clean table.
	purged, err := attempts.PruneIPStale(ctx, db, 0)
	if err != nil {
		t.Fatalf("PruneIPStale: %v", err)
	}
	if purged != 1 {
		t.Errorf("pruned %d rows, want 1", purged)
	}
	if s, err := attempts.IPState(ctx, db, ip); err != nil || s != (lockout.State{}) {
		t.Errorf("state after pruning = %+v, %v", s, err)
	}
}

func TestPruneIPStaleKeepsRecentFailures(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	attempts := store.NewAuthAttempts()

	recent := netip.MustParseAddr("198.51.100.20")
	stale := netip.MustParseAddr("198.51.100.21")
	if err := attempts.RecordIPFailure(ctx, db, recent,
		lockout.State{FailedAttempts: 1, WindowStartedAt: time.Now()}); err != nil {
		t.Fatalf("RecordIPFailure: %v", err)
	}
	if err := attempts.RecordIPFailure(ctx, db, stale,
		lockout.State{FailedAttempts: 1, WindowStartedAt: time.Now()}); err != nil {
		t.Fatalf("RecordIPFailure: %v", err)
	}
	// Backdate one row past the pruning horizon.
	if _, err := db.ExecContext(ctx,
		`UPDATE auth_ip_attempts SET last_failed_at = now() - interval '40 days' WHERE ip = $1`,
		stale.String()); err != nil {
		t.Fatalf("backdating: %v", err)
	}

	purged, err := attempts.PruneIPStale(ctx, db, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneIPStale: %v", err)
	}
	if purged != 1 {
		t.Errorf("pruned %d rows, want 1", purged)
	}
	if s, _ := attempts.IPState(ctx, db, recent); s.FailedAttempts != 1 {
		t.Error("a recent failure was pruned")
	}
	if s, _ := attempts.IPState(ctx, db, stale); s.FailedAttempts != 0 {
		t.Error("a stale failure survived pruning")
	}
}

// A client that supplied no usable address is counted against by name only. The
// per-account limit has to keep working on its own, and an error here would take
// the whole login path down for a request with a missing header.
func TestMissingSourceAddressIsNotAnError(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	attempts := store.NewAuthAttempts()

	if s, err := attempts.IPState(ctx, db, netip.Addr{}); err != nil || s != (lockout.State{}) {
		t.Errorf("IPState with no address = %+v, %v", s, err)
	}
	if err := attempts.RecordIPFailure(ctx, db, netip.Addr{},
		lockout.State{FailedAttempts: 1, WindowStartedAt: time.Now()}); err != nil {
		t.Errorf("RecordIPFailure with no address: %v", err)
	}

	var count int
	if err := db.GetContext(ctx, &count, `SELECT count(*) FROM auth_ip_attempts`); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != 0 {
		t.Errorf("a row was stored for a missing address")
	}
}

// The state has to survive the round trip through the policy, or the stored
// columns and the policy would disagree about what a lock is.
func TestStoredStateDrivesThePolicy(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	attempts := store.NewAuthAttempts()
	policy := lockout.Policy{MaxAttempts: 3, Window: 15 * time.Minute, Duration: 15 * time.Minute}

	// Three failures recorded through the policy, stored, read back, and the
	// fourth attempt refused.
	s, err := attempts.AccountState(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("AccountState: %v", err)
	}
	now := time.Now()
	for i := 0; i < 3; i++ {
		s = policy.Record(s, now)
		if err := attempts.RecordAccountFailure(ctx, db, "alice@example.com", s); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}
	s, err = attempts.AccountState(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("AccountState: %v", err)
	}
	if d := policy.Check(s, now); !d.Locked {
		t.Errorf("decision = %+v, want a lock after three stored failures", d)
	}
}

// The per-account row goes away with the user, so deleting an account cannot
// leave a limit behind that a recreated address would inherit.
func TestAccountAttemptsGoWithTheUser(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	attempts := store.NewAuthAttempts()

	if err := attempts.RecordAccountFailure(ctx, db, "bob@example.com",
		lockout.State{FailedAttempts: 5, WindowStartedAt: time.Now(),
			LockedUntil: time.Now().Add(15 * time.Minute)}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM users WHERE address = 'bob@example.com'`); err != nil {
		t.Fatalf("deleting bob: %v", err)
	}

	var count int
	if err := db.GetContext(ctx, &count,
		`SELECT count(*) FROM auth_attempts WHERE address = 'bob@example.com'`); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != 0 {
		t.Error("the lockout row outlived the user it belonged to")
	}
}

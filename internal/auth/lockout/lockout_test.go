package lockout_test

import (
	"testing"
	"time"

	"github.com/linchange/linchange/internal/auth/lockout"
)

func policy(max int, window, duration time.Duration) lockout.Policy {
	return lockout.Policy{MaxAttempts: max, Window: window, Duration: duration}
}

var base = time.Unix(1_700_000_000, 0)

func TestLockedOnlyAfterTheThreshold(t *testing.T) {
	p := policy(3, 15*time.Minute, 15*time.Minute)
	s := lockout.State{}

	for i := 1; i <= 2; i++ {
		s = p.Record(s, base)
		if s.Locked(base) {
			t.Fatalf("locked after only %d failures with a threshold of 3", i)
		}
	}
	if got := p.Check(s, base).Remaining; got != 1 {
		t.Errorf("remaining = %d, want 1", got)
	}

	s = p.Record(s, base)
	if !s.Locked(base) {
		t.Error("not locked on the third failure, which is the threshold")
	}
	if d := p.Check(s, base); !d.Locked || !d.Until.Equal(base.Add(15*time.Minute)) {
		t.Errorf("decision = %+v, want locked until %v", d, base.Add(15*time.Minute))
	}
}

// A run of failures that ages out must not accumulate into a permanent lock.
// Otherwise a determined attacker is saved by their own patience and an
// ordinary user is locked out by a handful of mistypes over a month.
func TestFailuresOutsideTheWindowDoNotCount(t *testing.T) {
	p := policy(3, 15*time.Minute, 15*time.Minute)
	s := lockout.State{}

	s = p.Record(s, base)
	s = p.Record(s, base.Add(10*time.Minute))
	if s.Locked(base.Add(25 * time.Minute)) {
		t.Error("locked by two failures that were 15 minutes apart")
	}

	// The third failure lands in a new window, so the first two are dropped and
	// the counter restarts rather than continuing at two.
	s = p.Record(s, base.Add(25*time.Minute))
	if s.FailedAttempts != 1 {
		t.Errorf("failed attempts = %d, want 1: the window should have restarted", s.FailedAttempts)
	}
	if s.Locked(base.Add(25 * time.Minute)) {
		t.Error("a failure in a fresh window locked the subject")
	}
}

// A lock that has served its duration is cleared, so a user who fumbles once
// after waiting it out is not instantly locked again by a stale counter.
func TestLockExpiryClearsTheCounter(t *testing.T) {
	p := policy(2, 15*time.Minute, 15*time.Minute)
	s := p.Record(p.Record(lockout.State{}, base), base)
	if !s.Locked(base) {
		t.Fatal("setup: expected a lock")
	}

	// One second before the lock lifts, it still holds.
	if !s.Locked(base.Add(15*time.Minute - time.Second)) {
		t.Error("the lock lifted early")
	}

	// After it lifts, one failure is tolerated again rather than re-locking.
	s = p.Record(s, base.Add(16*time.Minute))
	if s.Locked(base.Add(16 * time.Minute)) {
		t.Error("a single failure re-locked the subject straight after a lock expired")
	}
	if s.FailedAttempts != 1 {
		t.Errorf("failed attempts = %d, want 1", s.FailedAttempts)
	}
}

// Attempts made while already locked must not push the counter further past the
// threshold, or the lock would extend every time somebody keeps typing.
func TestFailuresDuringALockDoNotExtendIt(t *testing.T) {
	p := policy(2, 15*time.Minute, 10*time.Minute)
	s := p.Record(p.Record(lockout.State{}, base), base)
	until := s.LockedUntil

	s = p.Record(s, base.Add(5*time.Minute))
	if !s.LockedUntil.Equal(until) {
		t.Errorf("lock moved from %v to %v during the lock", until, s.LockedUntil)
	}
	if s.FailedAttempts != 2 {
		t.Errorf("failed attempts = %d, want 2 unchanged", s.FailedAttempts)
	}
}

// A disabled scope must record nothing, so an operator who turns a limit off
// does not accumulate rows that are never read and cannot be reasoned about.
func TestDisabledPolicyRecordsNothing(t *testing.T) {
	for _, p := range []lockout.Policy{
		{MaxAttempts: 0, Window: time.Minute, Duration: time.Minute},
		{MaxAttempts: 5, Window: 0, Duration: time.Minute},
		{MaxAttempts: 5, Window: time.Minute, Duration: 0},
		{MaxAttempts: -1, Window: time.Minute, Duration: time.Minute},
	} {
		if p.Enabled() {
			t.Errorf("policy %+v reports itself enabled", p)
		}
		if d := p.Check(lockout.State{FailedAttempts: 99}, base); d.Locked {
			t.Errorf("a disabled policy locked a subject: %+v", d)
		}
		s := p.Record(lockout.State{}, base)
		if s != (lockout.State{}) {
			t.Errorf("a disabled policy recorded state: %+v", s)
		}
	}
}

func TestCheckDoesNotLockBeforeTheThreshold(t *testing.T) {
	p := policy(5, 15*time.Minute, 15*time.Minute)
	// Remaining is reported so an operator can see how close a subject is to
	// being locked, and it counts down to zero without going negative. One
	// failure is already recorded, so these are the counts after 2, 3, 4 and 5
	// failures against a threshold of 5.
	s := p.Record(lockout.State{}, base)
	if d := p.Check(s, base); d.Locked {
		t.Errorf("one failure locked a subject with a threshold of 5: %+v", d)
	}
	for i, want := range []int{3, 2, 1, 0} {
		s = p.Record(s, base)
		if got := p.Check(s, base).Remaining; got != want {
			t.Errorf("after %d failures remaining = %d, want %d", i+2, got, want)
		}
	}
}

// Both scopes are evaluated, and the account is reported first because it is the
// more specific finding for whoever reads the log.
func TestBothScopes(t *testing.T) {
	both := lockout.Both{
		Account: policy(2, 15*time.Minute, 15*time.Minute),
		IP:      policy(10, 15*time.Minute, time.Hour),
	}

	lockedAccount := both.Account.Record(both.Account.Record(lockout.State{}, base), base)
	if d := both.Check(lockedAccount, lockout.State{}, base); !d.Locked || d.Scope != "account" {
		t.Errorf("decision = %+v, want an account lock", d)
	}

	lockedIP := both.IP.Record(lockout.State{}, base)
	for i := 0; i < 9; i++ {
		lockedIP = both.IP.Record(lockedIP, base)
	}
	if !lockedIP.Locked(base) {
		t.Fatal("setup: expected an IP lock")
	}
	if d := both.Check(lockout.State{}, lockedIP, base); !d.Locked || d.Scope != "ip" {
		t.Errorf("decision = %+v, want an IP lock", d)
	}

	// With both scopes clear the attempt is allowed.
	if d := both.Check(lockout.State{}, lockout.State{}, base); d.Locked {
		t.Errorf("decision = %+v, want allowed", d)
	}
}

// An account one failure from being locked is still allowed, but the decision
// says which scope is about to decide, so the log records it.
func TestBothScopesReportTheImminentLock(t *testing.T) {
	both := lockout.Both{
		Account: policy(1, 15*time.Minute, 15*time.Minute),
		IP:      policy(10, 15*time.Minute, time.Hour),
	}
	// One below the account threshold.
	account := both.Account.Record(lockout.State{}, base.Add(-time.Hour))
	d := both.Check(account, lockout.State{}, base)
	if d.Locked {
		t.Errorf("decision = %+v, want allowed", d)
	}
	if d.Scope != "account" {
		t.Errorf("scope = %q, want account to be named as the impending limit", d.Scope)
	}
	if d.Reason() == "" {
		t.Error("Reason is empty")
	}
}

// Recording a failure advances both scopes, so interleaving targets cannot be
// used to keep one of the two counters from moving.
func TestRecordAdvancesBothScopes(t *testing.T) {
	both := lockout.Both{
		Account: policy(3, 15*time.Minute, 15*time.Minute),
		IP:      policy(3, 15*time.Minute, 15*time.Minute),
	}
	account, ip := both.Record(lockout.State{}, lockout.State{}, base)
	if account.FailedAttempts != 1 || ip.FailedAttempts != 1 {
		t.Errorf("account = %d, ip = %d; want 1 and 1", account.FailedAttempts, ip.FailedAttempts)
	}
}

func TestResetClearsEverything(t *testing.T) {
	p := policy(2, 15*time.Minute, 15*time.Minute)
	s := p.Record(p.Record(lockout.State{}, base), base)
	if !s.Locked(base) {
		t.Fatal("setup: expected a lock")
	}
	if s = p.Reset(); s.Locked(base) || s.FailedAttempts != 0 {
		t.Errorf("state after reset = %+v, want clean", s)
	}
}

func TestLockedOnAZeroTimeIsFalse(t *testing.T) {
	// A subject with no recorded history must not read as locked, which is what
	// keeps the zero State usable as a clean slate.
	if (lockout.State{}).Locked(base) {
		t.Error("the zero state reports itself locked")
	}
}

func TestReasonNamesTheScope(t *testing.T) {
	if got := (lockout.Decision{}).Reason(); got != "allowed" {
		t.Errorf("Reason for an allowed decision = %q", got)
	}
	if got := (lockout.Decision{Locked: true, Scope: "ip"}).Reason(); got != "locked by ip" {
		t.Errorf("Reason = %q", got)
	}
	if got := (lockout.Decision{Locked: true}).Reason(); got != "locked" {
		t.Errorf("Reason = %q", got)
	}
}

// A counter left far past the threshold must not report a negative number of
// remaining attempts, which a caller might subtract from something.
func TestRemainingNeverGoesNegative(t *testing.T) {
	p := policy(1, time.Minute, time.Minute)
	if got := p.Check(lockout.State{FailedAttempts: 1000}, base).Remaining; got < 0 {
		t.Errorf("Remaining = %d, want >= 0", got)
	}
}

// A lock shorter than its window must still give the subject a fresh set of
// attempts once it has been served.
//
// This configuration is plausible enough to be worth pinning: a short lock with
// a long window leaves the counter sitting at the threshold when the lock lifts,
// so if the counter were not cleared the next failure would re-lock for another
// full duration. The subject would be locked permanently, and the only way out
// would be for the window to age out, which is exactly what the lock was meant
// to avoid.
func TestAShortLockDoesNotLockTheSubjectPermanently(t *testing.T) {
	p := policy(3, time.Hour, time.Minute)

	s := p.Record(p.Record(p.Record(lockout.State{}, base), base), base)
	if !s.Locked(base) {
		t.Fatal("setup: expected a lock")
	}
	// The window is still wide open when the one minute lock is served.
	after := base.Add(2 * time.Minute)
	if after.Sub(s.WindowStartedAt) >= p.Window {
		t.Fatal("setup: the window should still be open")
	}

	s = p.Record(s, after)
	if s.Locked(after) {
		t.Error("the subject was re-locked immediately after serving a lock, " +
			"which makes the lockout permanent")
	}
	if s.FailedAttempts != 1 {
		t.Errorf("failed attempts = %d, want 1: the counter should restart "+
			"when a lock is served", s.FailedAttempts)
	}

	// And the subject gets the full set of attempts again, not just one.
	for i := 1; i < p.MaxAttempts; i++ {
		s = p.Record(s, after)
	}
	if !s.Locked(after) {
		t.Error("the subject should be locked again after a full fresh run of failures")
	}
}

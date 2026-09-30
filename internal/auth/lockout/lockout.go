// Package lockout decides when repeated authentication failures should stop
// being answered.
//
// It is deliberately free of database and configuration types. The rules here
// are the part that is easy to get subtly wrong and worth testing directly:
// which failures still count, when a lock expires, and what a caller is told
// without learning whether an account exists. The storage that records the
// history lives in internal/store, and the thresholds arrive as a Policy.
//
// Two scopes are enforced together, because either alone leaves an obvious way
// through. A per-account limit cannot see one host spraying many accounts; a
// per-IP limit cannot see a slow guess at one account from many hosts. Each
// catches what the other misses.
package lockout

import "time"

// State is the recorded failure history for one account or one address.
//
// The zero value is a clean slate, so a caller that has never seen a subject
// before can use it directly.
type State struct {
	// FailedAttempts is the number of failures in the current window.
	FailedAttempts int
	// WindowStartedAt is when the current run of failures began. A failure
	// arriving after Window has elapsed starts a new run rather than joining
	// this one, which is what stops a determined attacker from being saved by
	// their own patience.
	WindowStartedAt time.Time
	// LastFailedAt is when the most recent failure was recorded.
	LastFailedAt time.Time
	// LockedUntil is when a lock stops applying. The zero value means the
	// subject is not locked.
	LockedUntil time.Time
}

// Locked reports whether the subject is locked at now.
func (s State) Locked(now time.Time) bool {
	return !s.LockedUntil.IsZero() && now.Before(s.LockedUntil)
}

// Policy is the threshold set for one scope.
type Policy struct {
	// MaxAttempts is how many failures are tolerated inside Window before the
	// subject is locked. Zero or less disables this scope, and a disabled
	// scope records nothing at all, so an operator who turns it off does not
	// accumulate rows that are never read.
	MaxAttempts int
	// Window is the period a run of failures covers.
	Window time.Duration
	// Duration is how long a lock lasts once the threshold is reached.
	Duration time.Duration
}

// Enabled reports whether this scope does anything.
func (p Policy) Enabled() bool {
	return p.MaxAttempts > 0 && p.Window > 0 && p.Duration > 0
}

// Both is a pair of policies, one per scope.
type Both struct {
	Account Policy
	IP      Policy
}

// Decision is the answer for one subject.
type Decision struct {
	// Locked is true when the attempt must be refused without being evaluated,
	// so that a locked account is not distinguishable from a missing one by
	// timing or by error.
	Locked bool
	// Until is when the lock lifts. Zero when not locked.
	Until time.Time
	// Remaining is how many further failures are tolerated before the next one
	// locks. It is for the operator's benefit and must never reach a client,
	// since it tells an attacker how close they are.
	Remaining int
	// Scope names which limit decided this, for the log line.
	Scope string
}

// Reason is the human-readable form of a Decision, for logs and metrics.
func (d Decision) Reason() string {
	if !d.Locked {
		return "allowed"
	}
	if d.Scope == "" {
		return "locked"
	}
	return "locked by " + d.Scope
}

// Check evaluates a recorded state without changing it.
//
// The caller runs this before doing the work of an authentication attempt, so
// that a locked account costs a single indexed read rather than an argon2
// verification. It matters more than it looks: an attacker who can keep an
// expensive hash running against a locked account has found a way to spend the
// server's CPU at will.
func (p Policy) Check(s State, now time.Time) Decision {
	if !p.Enabled() {
		return Decision{Remaining: -1}
	}
	if s.Locked(now) {
		return Decision{Locked: true, Until: s.LockedUntil, Remaining: 0}
	}
	remaining := p.MaxAttempts - s.FailedAttempts
	if remaining < 0 {
		remaining = 0
	}
	return Decision{Remaining: remaining}
}

// Record returns the state after one more failure at now.
//
// A run of failures that has aged past Window is replaced rather than extended,
// so the threshold is "failures within the window" and not "failures ever".
// A lock that has already served its Duration is cleared outright.
func (p Policy) Record(s State, now time.Time) State {
	if !p.Enabled() {
		return State{}
	}

	// A lock that has served its full Duration is cleared, so a user who fumbles
	// once after waiting out a lock is not immediately locked again by a counter
	// that was never reset.
	//
	// This cannot be left to the window check below. With a lock shorter than
	// the window, the counter would still be sitting at the threshold when the
	// lock lifted, so the very next failure would re-lock for another full
	// duration and the subject would never get another attempt: a permanent
	// lockout produced by a perfectly plausible configuration.
	if !s.LockedUntil.IsZero() {
		if s.LockedUntil.After(now) {
			// Still locked. Counting attempts made during a lock would push the
			// counter past the threshold for no benefit, so only the timestamp
			// moves.
			return s
		}
		s = State{WindowStartedAt: now}
	} else if s.FailedAttempts == 0 || (!s.WindowStartedAt.IsZero() && now.Sub(s.WindowStartedAt) >= p.Window) {
		// The previous run has aged out: start a new one.
		s = State{WindowStartedAt: now}
	}

	s.FailedAttempts++
	s.LastFailedAt = now
	if s.FailedAttempts >= p.MaxAttempts {
		s.LockedUntil = now.Add(p.Duration)
	}
	return s
}

// Reset returns the clean state used after a successful authentication.
func (p Policy) Reset() State {
	return State{}
}

// Check evaluates both scopes and reports the lock that decided the outcome.
//
// Account is checked first, because an account lock is the more specific
// finding: "this account is locked" is worth logging ahead of "this host is
// throttled", and an operator reading the log wants the account.
func (b Both) Check(account, ip State, now time.Time) Decision {
	if d := b.Account.Check(account, now); d.Locked {
		d.Scope = "account"
		return d
	}
	if d := b.IP.Check(ip, now); d.Locked {
		d.Scope = "ip"
		return d
	}
	d := b.Account.Check(account, now)
	if d.Remaining == 0 {
		// Not locked yet, but the next failure will be. Worth saying so in the
		// log without telling the client.
		d.Scope = "account"
	}
	return d
}

// Record applies one failure to both scopes.
//
// Both are recorded even though one of them may already have been locked,
// because a host spraying many accounts still needs its per-IP count kept, and
// skipping it would let an attacker launder progress by interleaving targets.
func (b Both) Record(account, ip State, now time.Time) (State, State) {
	return b.Account.Record(account, now), b.IP.Record(ip, now)
}

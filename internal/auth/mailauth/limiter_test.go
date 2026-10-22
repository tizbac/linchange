package mailauth

import (
	"testing"
	"time"
)

func TestFailureLimiterLocksAfterThreshold(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	now := start
	limiter := newFailureLimiter(10, 5*time.Minute, 5*time.Minute)
	limiter.now = func() time.Time { return now }

	for i := 0; i < 9; i++ {
		limiter.record("bob@example.com")
	}
	if limiter.locked("bob@example.com") {
		t.Fatal("locked before the tenth failure")
	}
	limiter.record("bob@example.com")
	if !limiter.locked("bob@example.com") {
		t.Fatal("not locked after the tenth failure")
	}

	now = now.Add(4 * time.Minute)
	if !limiter.locked("bob@example.com") {
		t.Fatal("lock lifted before five minutes")
	}
	now = now.Add(time.Minute)
	if limiter.locked("bob@example.com") {
		t.Fatal("lock did not lift after five minutes")
	}
}

func TestFailureLimiterIsPerLoginAndClearsOnSuccess(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	now := start
	limiter := newFailureLimiter(10, 5*time.Minute, 5*time.Minute)
	limiter.now = func() time.Time { return now }

	for i := 0; i < 10; i++ {
		limiter.record("bob@example.com")
	}
	if !limiter.locked("bob@example.com") {
		t.Fatal("bob was not locked")
	}
	if limiter.locked("carol@example.com") {
		t.Fatal("a different login inherited bob's lock")
	}

	limiter.clear("bob@example.com")
	if limiter.locked("bob@example.com") {
		t.Fatal("successful authentication did not clear the counter")
	}
}

func TestFailureLimiterWindowResets(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	now := start
	limiter := newFailureLimiter(10, 5*time.Minute, 5*time.Minute)
	limiter.now = func() time.Time { return now }

	for i := 0; i < 9; i++ {
		limiter.record("bob@example.com")
	}
	now = now.Add(6 * time.Minute)
	limiter.record("bob@example.com")
	if limiter.locked("bob@example.com") {
		t.Fatal("a failure outside the window joined the previous run")
	}
}

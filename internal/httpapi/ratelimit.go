package httpapi

import (
	"sync"
	"time"
)

// rateLimiter is a small in-memory sliding-window limiter.
//
// It exists for the unauthenticated password-reset form, where the captcha
// proves a human solved one challenge but does not by itself stop that human
// from submitting the form repeatedly. Entries are pruned as they are touched,
// so the map stays proportional to the number of active callers rather than to
// the total request count.
type rateLimiter struct {
	mu      sync.Mutex
	entries map[string][]time.Time
	max     int
	window  time.Duration
	now     func() time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	if max <= 0 {
		max = 1
	}
	return &rateLimiter{
		entries: make(map[string][]time.Time),
		max:     max,
		window:  window,
		now:     time.Now,
	}
}

// Allow reports whether key may perform another action now, and records it.
func (l *rateLimiter) Allow(key string) bool {
	if l == nil {
		return true
	}
	now := l.now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	// Keep the map from growing without bound when many distinct sources
	// arrive; a burst of IPv6 addresses is otherwise an unbounded allocation.
	if len(l.entries) > 4096 {
		for k, times := range l.entries {
			if len(times) == 0 || !times[len(times)-1].After(cutoff) {
				delete(l.entries, k)
			}
		}
	}

	recent := l.entries[key][:0]
	for _, at := range l.entries[key] {
		if at.After(cutoff) {
			recent = append(recent, at)
		}
	}
	if len(recent) >= l.max {
		l.entries[key] = recent
		return false
	}
	l.entries[key] = append(recent, now)
	return true
}

package delivery_test

import (
	"testing"
	"time"

	"github.com/linchange/linchange/internal/delivery"
)

func TestBackoffCalculateBackoff(t *testing.T) {
	cfg := delivery.BackoffConfig{
		Base:     time.Minute,
		Max:      time.Hour,
		Lifetime: 24 * time.Hour,
	}

	createdAt := time.Now().Add(-1 * time.Hour) // Message created 1 hour ago.

	tests := []struct {
		name      string
		attempts  int
		wantMin   time.Duration
		wantMax   time.Duration
		wantValid bool
	}{
		{
			name:      "zero attempts returns base delay",
			attempts:  0,
			wantMin:   time.Minute,
			wantMax:   time.Minute*60/4 + time.Minute, // base + 25% jitter
			wantValid: true,
		},
		{
			name:      "one attempt doubles",
			attempts:  1,
			wantMin:   time.Minute * 2,
			wantMax:   time.Minute*2 + time.Minute*30/4,
			wantValid: true,
		},
		{
			name:      "capped at max",
			attempts:  10,
			wantMin:   time.Hour,
			wantMax:   time.Hour + time.Hour/4,
			wantValid: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			delay, valid := cfg.CalculateBackoff(tc.attempts, createdAt)
			if valid != tc.wantValid {
				t.Errorf("valid = %v, want %v", valid, tc.wantValid)
			}
			if tc.wantValid {
				if delay < tc.wantMin || delay > tc.wantMax {
					t.Errorf("delay = %v, want between %v and %v", delay, tc.wantMin, tc.wantMax)
				}
			}
		})
	}
}

func TestBackoffExpired(t *testing.T) {
	cfg := delivery.BackoffConfig{
		Base:     time.Minute,
		Max:      time.Hour,
		Lifetime: 10 * time.Minute,
	}

	createdAt := time.Now().Add(-15 * time.Minute) // Message is 15 minutes old, exceeding 10-minute lifetime.

	_, valid := cfg.CalculateBackoff(0, createdAt)
	if valid {
		t.Error("expected expired, got valid")
	}
}

func TestBackoffZeroBase(t *testing.T) {
	cfg := delivery.BackoffConfig{
		Base:     0,
		Max:      time.Hour,
		Lifetime: 24 * time.Hour,
	}

	_, valid := cfg.CalculateBackoff(5, time.Now())
	if !valid {
		t.Error("expected valid with zero base")
	}
}

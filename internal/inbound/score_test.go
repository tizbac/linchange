package inbound

import (
	"testing"

	"github.com/emersion/go-msgauth/dmarc"
)

// policyPtr exists because dmarc.PolicyReject is an untyped constant and
// &dmarc.PolicyReject is not addressable.
func policyPtr(p dmarc.Policy) *dmarc.Policy { return &p }

func TestScoreMessageClean(t *testing.T) {
	sc := ScoreMessage(Signals{
		SPF:         SPFPass,
		HasDKIM:     true,
		DKIMValid:   true,
		DMARCPolicy: policyPtr(dmarc.PolicyReject),
		DMARCFails:  false,
		PTRPresent:  true,
	})
	if sc.Total != 0 {
		t.Errorf("clean message scored %v, want 0 (parts: %v)", sc.Total, sc.Parts)
	}
}

func TestScoreMessageTypicalSpam(t *testing.T) {
	// No SPF, no DKIM, unaligned DMARC reject policy, no PTR, HELO is an IP
	// and the source is rate limited: 0.5 + 1.0 + 3.0 + 2.0 + 1.0 + 3.0 =
	// 10.5, above the default junk threshold of 5 but below the reject
	// threshold of 12.
	sc := ScoreMessage(Signals{
		SPF:         SPFNone,
		DMARCPolicy: policyPtr(dmarc.PolicyReject),
		DMARCFails:  true,
		HELOIsIP:    true,
		RateCount:   RateLimit + 1,
	})
	if sc.Total != 10.5 {
		t.Errorf("total = %v, want 10.5 (parts: %v)", sc.Total, sc.Parts)
	}
	if sc.Total >= 12 {
		t.Errorf("typical spam %v reached the reject threshold", sc.Total)
	}
}

func TestScoreMessageHostile(t *testing.T) {
	// Everything fails at once: it must reach the reject threshold.
	sc := ScoreMessage(Signals{
		SPF:         SPFFail,
		HasDKIM:     true,
		DKIMValid:   false,
		DMARCPolicy: policyPtr(dmarc.PolicyReject),
		DMARCFails:  true,
		PTRPresent:  false,
		HELOIsIP:    true,
		RateCount:   RateLimit + 1,
	})
	// 2.0 + 2.0 + 3.0 + 2.0 + 1.0 + 3.0 = 13.0
	if sc.Total != 13.0 {
		t.Errorf("total = %v, want 13 (parts: %v)", sc.Total, sc.Parts)
	}
	if sc.Total < 12 {
		t.Errorf("hostile sender %v did not reach the reject threshold", sc.Total)
	}
}

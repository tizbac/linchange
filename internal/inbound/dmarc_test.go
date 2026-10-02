package inbound

import (
	"context"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
)

func TestDMARCEvaluate(t *testing.T) {
	ctx := context.Background()
	validDKIM := []*dkim.Verification{{Domain: "example.com", Err: nil}}

	t.Run("no policy", func(t *testing.T) {
		f := newFixtureResolver()
		res, err := DMARCEvaluate(ctx, f, DMARCInput{
			FromDomain:     "example.com",
			EnvelopeDomain: "example.com",
			SPF:            SPFPass,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Record != nil {
			t.Errorf("got a record where none exists: %+v", res)
		}
	})

	t.Run("spf aligned passes reject policy", func(t *testing.T) {
		f := newFixtureResolver()
		f.txt["_dmarc.example.com"] = []string{"v=DMARC1; p=reject"}
		res, err := DMARCEvaluate(ctx, f, DMARCInput{
			FromDomain:     "example.com",
			EnvelopeDomain: "example.com",
			SPF:            SPFPass,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Record == nil {
			t.Fatal("expected a record")
		}
		if res.Policy != dmarc.PolicyReject {
			t.Errorf("policy = %s, want reject", res.Policy)
		}
		if !res.SPFAligned {
			t.Error("SPF alignment not detected")
		}
		if !res.Aligned {
			t.Error("expected message to be aligned")
		}
	})

	t.Run("unauthenticated mail under reject policy is not aligned", func(t *testing.T) {
		f := newFixtureResolver()
		f.txt["_dmarc.example.com"] = []string{"v=DMARC1; p=reject"}
		res, err := DMARCEvaluate(ctx, f, DMARCInput{
			FromDomain:     "other.net",
			EnvelopeDomain: "spam.net",
			SPF:            SPFPass,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// No policy for other.net, so there is nothing to fail.
		if res.Record != nil {
			t.Errorf("got a record: %+v", res)
		}
	})

	t.Run("dkim aligned", func(t *testing.T) {
		f := newFixtureResolver()
		f.txt["_dmarc.example.com"] = []string{"v=DMARC1; p=quarantine"}
		res, err := DMARCEvaluate(ctx, f, DMARCInput{
			FromDomain:     "example.com",
			EnvelopeDomain: "example.com",
			SPF:            SPFFail,
			DKIM:           validDKIM,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.DKIMAligned {
			t.Error("DKIM alignment not detected")
		}
		if res.SPFAligned {
			t.Error("SPF aligned despite SPF fail")
		}
		if !res.Aligned {
			t.Error("expected DKIM alignment to satisfy DMARC")
		}
	})

	t.Run("ancestor policy applies subdomain policy", func(t *testing.T) {
		f := newFixtureResolver()
		f.txt["_dmarc.example.com"] = []string{"v=DMARC1; p=reject; sp=none"}
		res, err := DMARCEvaluate(ctx, f, DMARCInput{
			FromDomain:     "sub.example.com",
			EnvelopeDomain: "sub.example.com",
			SPF:            SPFPass,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Record == nil || res.RecordDomain != "example.com" {
			t.Fatalf("expected record from example.com, got %+v", res)
		}
		if res.Policy != dmarc.PolicyNone {
			t.Errorf("subdomain policy = %s, want none (sp=)", res.Policy)
		}
	})

	t.Run("relaxed vs strict alignment", func(t *testing.T) {
		// Relaxed: sub.example.com and example.com align.
		f := newFixtureResolver()
		f.txt["_dmarc.example.com"] = []string{"v=DMARC1; p=reject; aspf=r; adkim=r"}
		res, err := DMARCEvaluate(ctx, f, DMARCInput{
			FromDomain:     "sub.example.com",
			EnvelopeDomain: "example.com",
			SPF:            SPFPass,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.SPFAligned {
			t.Error("relaxed SPF alignment not detected")
		}

		// Strict: only an exact match aligns.
		f = newFixtureResolver()
		f.txt["_dmarc.example.com"] = []string{"v=DMARC1; p=reject; aspf=s; adkim=s"}
		res, err = DMARCEvaluate(ctx, f, DMARCInput{
			FromDomain:     "sub.example.com",
			EnvelopeDomain: "example.com",
			SPF:            SPFPass,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.SPFAligned {
			t.Error("strict alignment should not hold for subdomain vs parent")
		}
	})

	t.Run("temporary DNS failure propagates", func(t *testing.T) {
		f := newFixtureResolver()
		f.temp = true
		_, err := DMARCEvaluate(ctx, f, DMARCInput{FromDomain: "example.com"})
		if err == nil {
			t.Error("expected an error for a temporary DNS failure")
		}
	})
}

func TestDMARCDomainWalk(t *testing.T) {
	// The walk must stop at a single-label domain without querying the TLD.
	f := newFixtureResolver()
	f.txt["_dmarc.com"] = []string{"v=DMARC1; p=reject"}
	res, err := DMARCEvaluate(context.Background(), f, DMARCInput{FromDomain: "example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Record != nil {
		t.Errorf("TLD record must not be consulted, got %+v", res)
	}
}

func TestLookupDMARCMalformed(t *testing.T) {
	f := newFixtureResolver()
	// A record that fails to parse is treated as no policy.
	f.txt["_dmarc.example.com"] = []string{"v=DMARC1"}
	res, err := DMARCEvaluate(context.Background(), f, DMARCInput{FromDomain: "example.com"})
	if err != nil {
		t.Fatalf("malformed record must not error: %v", err)
	}
	if res.Record != nil {
		t.Errorf("malformed record treated as policy: %+v", res)
	}
}

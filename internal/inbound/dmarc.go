package inbound

import (
	"context"
	"errors"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
)

// DMARCInput carries the authentication results DMARC has to align: the From
// domain, the identity that carried the message (envelope sender or HELO), the
// SPF result and the DKIM verifications.
type DMARCInput struct {
	FromDomain     string
	EnvelopeDomain string
	SPF            SPFResult
	DKIM           []*dkim.Verification
}

// DMARCResult is the outcome of a DMARC evaluation.
type DMARCResult struct {
	// Record is nil when no DMARC policy applies to the From domain.
	Record       *dmarc.Record
	RecordDomain string
	// SPFAligned and DKIMAligned report which identifier alignment held.
	SPFAligned  bool
	DKIMAligned bool
	// Aligned is true when either identifier aligned; an aligned message
	// passes DMARC regardless of the policy.
	Aligned bool
	// Policy is the policy that applies to the From domain: the subdomain
	// policy (sp=) when the record lives on an ancestor domain.
	Policy dmarc.Policy
}

// DMARCEvaluate implements the RFC 7489 receiver evaluation for one message.
// It walks from the From domain to its ancestors looking for a policy record.
func DMARCEvaluate(ctx context.Context, dns Resolver, in DMARCInput) (DMARCResult, error) {
	fromDomain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.FromDomain), "."))
	envDomain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.EnvelopeDomain), "."))

	var result DMARCResult
	d := fromDomain
	for d != "" && strings.Count(d, ".") >= 1 {
		rec, err := lookupDMARC(ctx, dns, d)
		if err != nil {
			return DMARCResult{}, err
		}
		if rec != nil {
			result.Record = rec
			result.RecordDomain = d
			break
		}
		if i := strings.IndexByte(d, '.'); i > 0 {
			d = d[i+1:]
		} else {
			d = ""
		}
	}
	if result.Record == nil {
		return result, nil
	}

	rec := result.Record
	// Policy selection: a record on an ancestor domain applies to subdomains
	// via its sp= parameter, falling back to p=.
	result.Policy = rec.Policy
	if result.RecordDomain != fromDomain && rec.SubdomainPolicy != "" {
		result.Policy = rec.SubdomainPolicy
	}

	if in.SPF == SPFPass && envDomain != "" {
		result.SPFAligned = domainsAligned(fromDomain, envDomain, rec.SPFAlignment)
	}
	for _, v := range in.DKIM {
		if v.Err != nil {
			continue
		}
		if domainsAligned(fromDomain, v.Domain, rec.DKIMAlignment) {
			result.DKIMAligned = true
			break
		}
	}
	result.Aligned = result.SPFAligned || result.DKIMAligned
	return result, nil
}

func lookupDMARC(ctx context.Context, dns Resolver, domain string) (*dmarc.Record, error) {
	opts := &dmarc.LookupOptions{
		LookupTXT: func(name string) ([]string, error) {
			return dns.TXT(ctx, name)
		},
	}
	rec, err := dmarc.LookupWithOptions(domain, opts)
	switch {
	case err == nil:
		return rec, nil
	case errors.Is(err, dmarc.ErrNoPolicy):
		return nil, nil
	case dmarc.IsTempFail(err):
		return nil, err
	default:
		// A malformed record means "no policy", per RFC 7489 6.3.
		return nil, nil
	}
}

// domainsAligned implements RFC 7489 5.2 identifier alignment.
func domainsAligned(from, other string, mode dmarc.AlignmentMode) bool {
	from = strings.ToLower(strings.TrimSuffix(from, "."))
	other = strings.ToLower(strings.TrimSuffix(other, "."))
	if from == "" || other == "" {
		return false
	}
	if from == other {
		return true
	}
	if mode == dmarc.AlignmentStrict {
		return false
	}
	return strings.HasSuffix(from, "."+other) || strings.HasSuffix(other, "."+from)
}

package inbound

import (
	"bytes"
	"context"

	"github.com/emersion/go-msgauth/dkim"
)

// DKIMVerify checks every DKIM-Signature header in msg and returns one
// Verification per signature, in header order. A nil result with no signatures
// means the message is unsigned.
//
// Signature verification failures are reported on each Verification.Err; a
// non-nil error from this function means the message could not be parsed at
// all, in which case there are no verifications to report.
func DKIMVerify(ctx context.Context, msg []byte, dns Resolver) []*dkim.Verification {
	opts := &dkim.VerifyOptions{
		LookupTXT: func(domain string) ([]string, error) {
			return dns.TXT(ctx, domain)
		},
		// Bound the work one message can force on us: real messages carry one
		// or two signatures at most.
		MaxVerifications: 3,
	}
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(msg), opts)
	if err != nil {
		return nil
	}
	return verifs
}

// DKIMValid reports whether any verification succeeded.
func DKIMValid(verifs []*dkim.Verification) bool {
	for _, v := range verifs {
		if v.Err == nil {
			return true
		}
	}
	return false
}

// DKIMDomains returns the d= domains of the verifications, for DMARC
// alignment.
func DKIMDomains(verifs []*dkim.Verification) []string {
	var out []string
	for _, v := range verifs {
		out = append(out, v.Domain)
	}
	return out
}

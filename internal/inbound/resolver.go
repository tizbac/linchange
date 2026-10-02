// Package inbound implements the :25 MTA listener: recipient resolution,
// greylisting, SPF/DKIM/DMARC evaluation, and spam-scored routing into
// INBOX or Junk.
package inbound

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sort"
)

// Resolver is the subset of DNS operations the inbound checks need.
// Production wires NewResolver; tests use an in-memory fixture so behaviour is
// deterministic without touching real DNS.
type Resolver interface {
	// TXT returns the character-strings of the TXT records for name, in
	// DNS order. A missing name returns an error.
	TXT(ctx context.Context, name string) ([]string, error)
	// A returns the IPv4 addresses for name.
	A(ctx context.Context, name string) ([]netip.Addr, error)
	// MX returns the MX hosts for name, highest priority first.
	MX(ctx context.Context, name string) ([]string, error)
	// PTR returns the canonical name that ip reverse-resolves to, or "" if
	// there is no reverse record.
	PTR(ctx context.Context, ip netip.Addr) (string, error)
}

// NewResolver returns the Resolver backed by the system resolver.
func NewResolver() Resolver { return &netResolver{} }

type netResolver struct{}

func (n *netResolver) TXT(ctx context.Context, name string) ([]string, error) {
	return net.LookupTXT(name)
}

func (n *netResolver) A(ctx context.Context, name string) ([]netip.Addr, error) {
	ips, err := net.LookupIP(name)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip); ok && a.Is4() {
			out = append(out, a)
		}
	}
	return out, nil
}

func (n *netResolver) MX(ctx context.Context, name string) ([]string, error) {
	mxs, err := net.LookupMX(name)
	if err != nil {
		return nil, err
	}
	hosts := make([]string, 0, len(mxs))
	for _, mx := range mxs {
		hosts = append(hosts, mx.Host)
	}
	sort.Slice(hosts, func(i, j int) bool { return mxs[i].Pref < mxs[j].Pref })
	return hosts, nil
}

func (n *netResolver) PTR(ctx context.Context, ip netip.Addr) (string, error) {
	names, err := net.LookupAddr(ip.String())
	if err != nil || len(names) == 0 {
		return "", nil
	}
	return names[0], nil
}

// IsNoRecordsError reports whether err is a definitive "no such record"
// answer (NXDOMAIN), as opposed to a temporary network failure. Callers use
// this to tell "the record does not exist" from "DNS is unreachable": the
// first ends an evaluation, the second must be reported as a temporary
// failure.
func IsNoRecordsError(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

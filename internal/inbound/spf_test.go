package inbound

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
)

// fixtureResolver is an in-memory Resolver. Missing names yield the same
// *net.DNSError the system resolver produces for NXDOMAIN, so
// IsNoRecordsError behaves exactly as it will in production.
type fixtureResolver struct {
	txt  map[string][]string
	a    map[string][]netip.Addr
	mx   map[string][]string
	ptr  map[string]string // ip string -> canonical name
	temp bool              // force a temporary failure on every lookup
}

func newFixtureResolver() *fixtureResolver {
	return &fixtureResolver{
		txt: map[string][]string{},
		a:   map[string][]netip.Addr{},
		mx:  map[string][]string{},
		ptr: map[string]string{},
	}
}

// errTimeout mimics the system resolver's i/o timeout answer, which the
// evaluation code must classify as temporary.
var errTimeout = &net.DNSError{Err: "i/o timeout", IsTemporary: true}

func (f *fixtureResolver) TXT(ctx context.Context, name string) ([]string, error) {
	if f.temp {
		return nil, errTimeout
	}
	if r, ok := f.txt[name]; ok {
		return r, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fixtureResolver) A(ctx context.Context, name string) ([]netip.Addr, error) {
	if f.temp {
		return nil, errTimeout
	}
	if r, ok := f.a[name]; ok {
		return r, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fixtureResolver) MX(ctx context.Context, name string) ([]string, error) {
	if f.temp {
		return nil, errTimeout
	}
	if r, ok := f.mx[name]; ok {
		return r, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fixtureResolver) PTR(ctx context.Context, ip netip.Addr) (string, error) {
	if f.temp {
		return "", errTimeout
	}
	return f.ptr[ip.String()], nil
}

const testClientIP = "192.0.2.10"

func TestCheckSPF(t *testing.T) {
	ip := netip.MustParseAddr(testClientIP)
	ctx := context.Background()

	records := map[string]SPFResult{
		"v=spf1 -all":                     SPFFail,
		"v=spf1 ~all":                     SPFSoftfail,
		"v=spf1 ?all":                     SPFNeutral,
		"v=spf1 +all":                     SPFPass,
		"v=spf1 all":                      SPFPass,
		"v=spf1 exp=e.example.com -all":   SPFFail,
		"v=spf1 ip4:192.0.2.0/24 -all":    SPFPass,
		"v=spf1 ip4:198.51.100.0/24 -all": SPFFail,
		"v=spf1 ip4:192.0.2.10 -all":      SPFPass,
		"v=spf1 ip4:192.0.2.11 -all":      SPFFail,
		"v=spf1 ip6:2001:db8::1 -all":     SPFFail,
	}
	for rec, want := range records {
		f := newFixtureResolver()
		f.txt["example.com"] = []string{rec}
		got, err := CheckSPF(ctx, ip, "example.com", f)
		if err != nil {
			t.Fatalf("CheckSPF(%q): unexpected error: %v", rec, err)
		}
		if got != want {
			t.Errorf("CheckSPF(%q) = %s, want %s", rec, got, want)
		}
	}
}

func TestCheckSPFNoRecord(t *testing.T) {
	ip := netip.MustParseAddr(testClientIP)
	ctx := context.Background()

	// NXDOMAIN: no TXT at all.
	f := newFixtureResolver()
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFNone {
		t.Errorf("NXDOMAIN: got %s, want none", got)
	}

	// TXT that is not an SPF record.
	f.txt["example.com"] = []string{"v=foo", "hello"}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFNone {
		t.Errorf("non-SPF TXT: got %s, want none", got)
	}

	// Temporary failure.
	f.temp = true
	f.txt = map[string][]string{"example.com": {"v=spf1 -all"}}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFTempError {
		t.Errorf("temp failure: got %s, want temperror", got)
	}
}

func TestCheckSPFMalformed(t *testing.T) {
	ip := netip.MustParseAddr(testClientIP)
	ctx := context.Background()

	cases := map[string]SPFResult{
		"two SPF records":         SPFPermError,
		"mechanisms and redirect": SPFPermError,
		"unknown mechanism":       SPFPermError,
		"include with empty arg":  SPFPermError,
	}
	setup := map[string]func(f *fixtureResolver){
		"two SPF records": func(f *fixtureResolver) {
			f.txt["example.com"] = []string{"v=spf1 -all", "v=spf1 +all"}
		},
		"mechanisms and redirect": func(f *fixtureResolver) {
			f.txt["example.com"] = []string{"v=spf1 ip4:192.0.2.10 redirect=other.example.com"}
		},
		"unknown mechanism": func(f *fixtureResolver) {
			f.txt["example.com"] = []string{"v=spf1 bogus:1.2.3.4 -all"}
		},
		"include with empty arg": func(f *fixtureResolver) {
			f.txt["example.com"] = []string{"v=spf1 include: -all"}
		},
	}
	for name, want := range cases {
		f := newFixtureResolver()
		setup[name](f)
		got, err := CheckSPF(ctx, ip, "example.com", f)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
	}
}

func TestCheckSPFA(t *testing.T) {
	ip := netip.MustParseAddr(testClientIP)
	ctx := context.Background()

	// a matches the domain's A record.
	f := newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1 a -all"}
	f.a["example.com"] = []netip.Addr{ip}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFPass {
		t.Errorf("a: got %s, want pass", got)
	}

	// a/24 matches a record in the same /24.
	f = newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1 a/24 -all"}
	f.a["example.com"] = []netip.Addr{netip.MustParseAddr("192.0.2.77")}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFPass {
		t.Errorf("a/24 same net: got %s, want pass", got)
	}

	// a/32 does not.
	f = newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1 a/32 -all"}
	f.a["example.com"] = []netip.Addr{netip.MustParseAddr("192.0.2.77")}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFFail {
		t.Errorf("a/32 other host: got %s, want fail", got)
	}
}

func TestCheckSPFmx(t *testing.T) {
	ip := netip.MustParseAddr(testClientIP)
	ctx := context.Background()

	f := newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1 mx -all"}
	f.mx["example.com"] = []string{"mail.example.com"}
	f.a["mail.example.com"] = []netip.Addr{ip}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFPass {
		t.Errorf("mx: got %s, want pass", got)
	}
}

func TestCheckSPFptr(t *testing.T) {
	ip := netip.MustParseAddr(testClientIP)
	ctx := context.Background()

	// ptr:domain matches when the PTR lands under domain and the PTR's A
	// record is the client.
	f := newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1 ptr:example.com -all"}
	f.ptr[ip.String()] = "mail.example.com"
	f.a["mail.example.com"] = []netip.Addr{ip}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFPass {
		t.Errorf("ptr aligned: got %s, want pass", got)
	}

	// PTR to a different zone fails.
	f = newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1 ptr:example.com -all"}
	f.ptr[ip.String()] = "mail.other.net"
	f.a["mail.other.net"] = []netip.Addr{ip}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFFail {
		t.Errorf("ptr misaligned: got %s, want fail", got)
	}
}

func TestCheckSPFInclude(t *testing.T) {
	ip := netip.MustParseAddr(testClientIP)
	ctx := context.Background()

	// include splices in the included record's result.
	f := newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1 include:sub.example.com -all"}
	f.txt["sub.example.com"] = []string{"v=spf1 ip4:192.0.2.10 -all"}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFPass {
		t.Errorf("include pass: got %s, want pass", got)
	}

	// An included domain with no SPF record is a permerror, RFC 7208 5.2.
	f = newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1 include:sub.example.com -all"}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFPermError {
		t.Errorf("include missing record: got %s, want permerror", got)
	}

	// A temporary failure in the included domain is a temperror.
	f = newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1 include:sub.example.com -all"}
	f.temp = true
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFTempError {
		t.Errorf("include temp: got %s, want temperror", got)
	}
}

func TestCheckSPFRedirect(t *testing.T) {
	ip := netip.MustParseAddr(testClientIP)
	ctx := context.Background()

	f := newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1 redirect=real.example.com"}
	f.txt["real.example.com"] = []string{"v=spf1 ip4:192.0.2.10 -all"}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFPass {
		t.Errorf("redirect: got %s, want pass", got)
	}

	// A redirect to a domain with no SPF record is a permerror, RFC 7208 5.1.
	f = newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1 redirect=missing.example.com"}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFPermError {
		t.Errorf("redirect to missing: got %s, want permerror", got)
	}

	// A bare "v=spf1" is the implicit ?all.
	f = newFixtureResolver()
	f.txt["example.com"] = []string{"v=spf1"}
	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFNeutral {
		t.Errorf("bare record: got %s, want neutral", got)
	}
}

func TestCheckSPFMaxLookups(t *testing.T) {
	ip := netip.MustParseAddr(testClientIP)
	ctx := context.Background()

	// A chain of includes that forces more than maxSPFLookups DNS lookups
	// must stop with a permerror.
	f := newFixtureResolver()
	depth := 12
	for i := 0; i < depth; i++ {
		next := fmt.Sprintf("lvl%d.example.com", i+1)
		f.txt[fmt.Sprintf("lvl%d.example.com", i)] = []string{"v=spf1 include:" + next}
	}
	f.txt[fmt.Sprintf("lvl%d.example.com", depth)] = []string{"v=spf1 -all"}
	f.txt["example.com"] = []string{"v=spf1 include:lvl0.example.com"}

	if got, _ := CheckSPF(ctx, ip, "example.com", f); got != SPFPermError {
		t.Errorf("lookup cap: got %s, want permerror", got)
	}
}

func TestCheckSPFEmptyDomain(t *testing.T) {
	ip := netip.MustParseAddr(testClientIP)
	f := newFixtureResolver()
	if got, _ := CheckSPF(context.Background(), ip, "", f); got != SPFNone {
		t.Errorf("empty domain: got %s, want none", got)
	}
	if got, _ := CheckSPF(context.Background(), ip, "<>", f); got != SPFNone {
		t.Errorf("null sender: got %s, want none", got)
	}
}

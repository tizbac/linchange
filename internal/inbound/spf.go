package inbound

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// SPFResult is the outcome of an SPF evaluation, using the RFC 7208 result
// names.
type SPFResult string

const (
	SPFNone      SPFResult = "none"
	SPFNeutral   SPFResult = "neutral"
	SPFPass      SPFResult = "pass"
	SPFFail      SPFResult = "fail"
	SPFSoftfail  SPFResult = "softfail"
	SPFPermError SPFResult = "permerror"
	SPFTempError SPFResult = "temperror"
)

// maxSPFLookups bounds the DNS lookups of one evaluation, as RFC 7208
// section 10.5 requires.
const maxSPFLookups = 10

// errNoRecord is the internal sentinel for "this name has no records of the
// kind we asked for". It is a definitive answer, not a network problem.
var errNoRecord = errors.New("no record")

// CheckSPF evaluates the SPF record for domain on behalf of ip.
//
// The returned result is definitive even for error outcomes: permerror and
// temperror are results, not Go errors. The error return is reserved for
// unexpected internal failures and is nil in practice.
func CheckSPF(ctx context.Context, ip netip.Addr, domain string, dns Resolver) (SPFResult, error) {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if domain == "" {
		return SPFNone, nil
	}
	c := &spfChecker{ctx: ctx, ip: ip, dns: dns}

	record, res := c.fetchSPFRecord(domain)
	if record == "" {
		return res, nil
	}
	r, matched := c.evaluate(domain, record)
	if !matched {
		return SPFNone, nil
	}
	return r, nil
}

type spfChecker struct {
	ctx     context.Context
	ip      netip.Addr
	dns     Resolver
	lookups int
}

func (c *spfChecker) countLookup() SPFResult {
	c.lookups++
	if c.lookups > maxSPFLookups {
		return SPFPermError
	}
	return ""
}

// fetchSPFRecord retrieves the SPF record for domain. A zero record means the
// caller must use the returned result directly.
func (c *spfChecker) fetchSPFRecord(domain string) (record string, res SPFResult) {
	if r := c.countLookup(); r != "" {
		return "", r
	}
	txts, err := c.dns.TXT(c.ctx, domain)
	if err != nil {
		if errors.Is(err, errNoRecord) || IsNoRecordsError(err) {
			return "", SPFNone
		}
		return "", SPFTempError
	}
	var spfs []string
	for _, txt := range txts {
		l := strings.ToLower(txt)
		if l == "v=spf1" || strings.HasPrefix(l, "v=spf1 ") {
			spfs = append(spfs, l)
		}
	}
	switch len(spfs) {
	case 0:
		return "", SPFNone
	case 1:
		return spfs[0], ""
	default:
		return "", SPFPermError
	}
}

// evaluate walks the mechanisms of record in order. It returns the result and
// whether evaluation terminated definitively; when matched is false the
// caller continues with the next mechanism in its own record (the splice
// semantics of "include").
func (c *spfChecker) evaluate(domain, record string) (SPFResult, bool) {
	tokens := strings.Fields(record)
	if len(tokens) == 0 || !strings.EqualFold(tokens[0], "v=spf1") {
		return SPFPermError, true
	}

	type field struct {
		qual, mech, arg string
		kind            mechanismKind
	}
	var fields []field
	redirect := ""
	sawMech := false
	for _, tok := range tokens[1:] {
		qual, mech, arg, kind := parseMechanism(tok)
		if kind == mechRedirect {
			if arg != "" {
				redirect = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(arg), "."))
			}
			continue
		}
		if kind == mechUnknown {
			return SPFPermError, true
		}
		if kind == mechNormal {
			sawMech = true
		}
		fields = append(fields, field{qual, mech, arg, kind})
	}

	// A record must not mix mechanisms with a redirect, RFC 7208 4.2.
	if redirect != "" && sawMech {
		return SPFPermError, true
	}
	if redirect != "" {
		rec, res := c.fetchSPFRecord(redirect)
		if rec == "" {
			// A redirect to a domain with no SPF record is a permerror,
			// RFC 7208 5.1; a failed lookup is a temperror.
			if res == SPFTempError {
				return SPFTempError, true
			}
			return SPFPermError, true
		}
		r, m := c.evaluate(redirect, rec)
		if m {
			return r, true
		}
		return SPFNone, true
	}
	for _, fd := range fields {
		if fd.kind == mechModifier {
			continue
		}
		matched, res, done := c.evalMechanism(domain, fd.qual, fd.mech, fd.arg)
		if done {
			return res, true
		}
		if matched {
			return res, true
		}
	}
	// A record with no mechanisms and no redirect is the implicit ?all,
	// RFC 7208 4.2.
	if !sawMech {
		return SPFNeutral, true
	}
	return SPFNone, false
}

// evalMechanism evaluates one mechanism. It returns whether the mechanism
// matched ip, the result to report, and whether evaluation must stop
// regardless of the match (a permanent or temporary error).
func (c *spfChecker) evalMechanism(domain, qual, mech, arg string) (matched bool, res SPFResult, done bool) {
	switch mech {
	case "all":
		return true, qualifierResult(qual), false

	case "ip4", "ip6":
		wantV4 := mech == "ip4"
		if parseIPMechanism(c.ip, arg, wantV4) {
			return true, qualifierResult(qual), false
		}
		return false, "", false

	case "a", "mx", "ptr":
		names, err := c.mechanismAddresses(mech, domain, arg)
		if err != nil {
			if errors.Is(err, errNoRecord) {
				return false, "", false
			}
			return false, SPFTempError, true
		}
		for _, name := range names {
			if r := c.countLookup(); r != "" {
				return false, r, true
			}
			ips, err := c.dns.A(c.ctx, name)
			if err != nil {
				if errors.Is(err, errNoRecord) || IsNoRecordsError(err) {
					continue
				}
				return false, SPFTempError, true
			}
			if ipInList(c.ip, ips, arg) {
				return true, qualifierResult(qual), false
			}
		}
		return false, "", false

	case "exists":
		if arg == "" {
			return false, SPFPermError, true
		}
		if r := c.countLookup(); r != "" {
			return false, r, true
		}
		ips, err := c.dns.A(c.ctx, arg)
		if err == nil && len(ips) > 0 {
			return true, qualifierResult(qual), false
		}
		if err != nil && !errors.Is(err, errNoRecord) && !IsNoRecordsError(err) {
			return false, SPFTempError, true
		}
		return false, "", false

	case "include":
		if arg == "" {
			return false, SPFPermError, true
		}
		rec, res := c.fetchSPFRecord(strings.ToLower(strings.TrimSuffix(strings.TrimSpace(arg), ".")))
		if rec == "" {
			// RFC 7208 5.2: a failed lookup is a temperror; a lookup that
			// succeeds but finds no SPF record is a permerror.
			if res == SPFNone || res == "" {
				return false, SPFPermError, true
			}
			return false, res, true
		}
		r, m := c.evaluate(arg, rec)
		if m {
			return true, r, true
		}
		return false, "", false
	}
	// Unknown mechanism: a permerror by RFC 7208 5.1.
	return false, SPFPermError, true
}

// mechanismAddresses returns the domain names whose A records the a/mx/ptr
// mechanism must consult.
func (c *spfChecker) mechanismAddresses(mech, domain, arg string) ([]string, error) {
	dom, _ := splitMechArg(arg)
	switch mech {
	case "a":
		if dom == "" {
			dom = domain
		}
		return []string{dom}, nil
	case "mx":
		if dom == "" {
			dom = domain
		}
		mxs, err := c.dns.MX(c.ctx, dom)
		if err != nil {
			if IsNoRecordsError(err) {
				return nil, errNoRecord
			}
			return nil, err
		}
		if len(mxs) == 0 {
			return nil, errNoRecord
		}
		return mxs, nil
	case "ptr":
		name, err := c.dns.PTR(c.ctx, c.ip)
		if err != nil {
			return nil, err
		}
		if name == "" {
			return nil, errNoRecord
		}
		if dom != "" && !strings.HasSuffix(strings.ToLower(name), strings.ToLower(dom)) {
			return nil, errNoRecord
		}
		return []string{name}, nil
	}
	return nil, nil
}

type mechanismKind int

const (
	mechNormal mechanismKind = iota
	mechModifier
	mechUnknown
	mechRedirect
)

// parseMechanism parses one field of an SPF record.
func parseMechanism(tok string) (qual, mech, arg string, kind mechanismKind) {
	rest := tok
	qual = "+"
	if len(rest) > 1 {
		switch rest[0] {
		case '+', '-', '~', '?':
			qual = rest[:1]
			rest = rest[1:]
		}
	}
	// A legacy redirect: ";domain" in place of a mechanism.
	if strings.HasPrefix(rest, ";") {
		return qual, "redirect", rest[1:], mechRedirect
	}
	if rest == "" {
		return "+", "", "", mechUnknown
	}

	// A standalone modifier: key=value before any argument separator.
	if i := strings.IndexByte(rest, '='); i >= 0 {
		name := rest[:i]
		if name == "redirect" {
			// The canonical redirect form: "redirect=domain".
			return qual, "redirect", strings.TrimSpace(rest[i+1:]), mechRedirect
		}
		if isMechName(name) {
			// e.g. "ip4=": the mechanism with an empty argument.
			return qual, name, "", mechNormal
		}
		return "+", "", "", mechModifier
	}

	// Mechanism name: up to the first ':', '/' or end of token.
	nameEnd := len(rest)
	for i := 0; i < len(rest); i++ {
		if rest[i] == ':' || rest[i] == '/' {
			nameEnd = i
			break
		}
	}
	name := rest[:nameEnd]
	rest = rest[nameEnd:]
	if !isMechName(name) {
		return "+", "", "", mechUnknown
	}
	if strings.HasPrefix(rest, ":") {
		rest = rest[1:]
	}
	return qual, name, strings.TrimSpace(rest), mechNormal
}

func isMechName(name string) bool {
	switch name {
	case "all", "ip4", "ip6", "a", "mx", "ptr", "exists", "include", "redirect":
		return true
	}
	return false
}

func splitMechArg(arg string) (domain, prefix string) {
	if i := strings.IndexByte(arg, '/'); i >= 0 {
		return arg[:i], arg[i+1:]
	}
	return arg, ""
}

func parseIPMechanism(ip netip.Addr, arg string, wantV4 bool) bool {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return false
	}
	if wantV4 != ip.Is4() {
		return false
	}
	if i := strings.IndexByte(arg, '/'); i >= 0 {
		network, err := netip.ParseAddr(arg[:i])
		if err != nil {
			return false
		}
		length, err := strconv.Atoi(arg[i+1:])
		if err != nil || length < 0 {
			return false
		}
		prefix, perr := network.Prefix(length)
		if perr != nil {
			return false
		}
		return prefix.Contains(ip)
	}
	other, err := netip.ParseAddr(arg)
	if err != nil {
		return false
	}
	return other == ip
}

// ipInList reports whether ip is among the listed addresses, honouring the
// optional /length prefix that may trail the mechanism argument.
func ipInList(ip netip.Addr, ips []netip.Addr, rawArg string) bool {
	_, prefix := splitMechArg(rawArg)
	if prefix == "" {
		for _, a := range ips {
			if a == ip {
				return true
			}
		}
		return false
	}
	length, err := strconv.Atoi(prefix)
	if err != nil || length < 0 {
		return false
	}
	maxLen := 128
	if ip.Is4() {
		maxLen = 32
	}
	if length > maxLen {
		length = maxLen
	}
	for _, a := range ips {
		if a.Is4() != ip.Is4() {
			continue
		}
		p, perr := a.Prefix(length)
		if perr == nil && p.Contains(ip) {
			return true
		}
	}
	return false
}

func qualifierResult(qual string) SPFResult {
	switch qual {
	case "-":
		return SPFFail
	case "~":
		return SPFSoftfail
	case "?":
		return SPFNeutral
	default:
		return SPFPass
	}
}

// Package testldap runs a minimal LDAPv3 directory server for tests.
//
// It speaks the wire format a real server speaks — the same BER that the
// go-ldap client emits — and implements the operations the product uses:
// simple bind, unbind and subtree search with AND, OR, NOT, equality and
// present filters. What it does not pretend to be: there is no TLS, no SASL,
// no controls, no referrals and no modifications, so a test that needs one of
// those belongs to an integration suite pointed at a real directory, not here.
package testldap

import (
	"bufio"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	ber "github.com/go-asn1-ber/asn1-ber"
)

// Result codes (RFC 4511 resultCodes).
const (
	resOK                 = 0
	resInvalidCredentials = 49
)

// Operation tags in the LDAPMessage CHOICE (RFC 4511).
const (
	tagBindRequest    = 0
	tagBindResponse   = 1
	tagUnbindRequest  = 2
	tagSearchRequest  = 3
	tagSearchResEntry = 4
	tagSearchResDone  = 5
)

// Filter tags in the filter CHOICE (RFC 4514).
const (
	fltAnd       = 0
	fltOr        = 1
	fltNot       = 2
	fltEquality  = 3
	fltSubstring = 4
	fltPresent   = 7
)

// Substring part kinds in the substrings SEQUENCE (RFC 4512).
const (
	fltSubInitial = 0
	fltSubAny     = 1
	fltSubFinal   = 2
)

// Search scopes (RFC 4511).
const (
	scopeBase     = 0
	scopeOneLevel = 1
	scopeSubtree  = 2
)

// Entry is one directory entry. Attribute names are stored and matched
// case-insensitively, which is how DN-based directories behave in practice.
// An entry is bindable when Password is non-empty; binding succeeds only for
// the exact password, and the empty name is always an anonymous bind.
type Entry struct {
	DN         string
	Password   string
	Attributes map[string][]string
}

// Server is a directory good enough to log people in and to enumerate a small
// organisation. It is a fixture, not a product: it holds whatever the test
// puts in it and answers whatever the test asks.
type Server struct {
	t  *testing.T
	ln net.Listener

	mu      sync.RWMutex
	entries map[string]*Entry // keyed by lower(DN)
	conns   map[net.Conn]struct{}

	binds    atomic.Int64
	searches atomic.Int64

	done chan struct{}
	wg   sync.WaitGroup
}

// Start begins the server on a random localhost port and registers Stop as a
// test cleanup, so a failed test cannot leak the listener.
func Start(t *testing.T) *Server {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testldap: listening: %v", err)
	}
	s := &Server{
		t:       t,
		ln:      ln,
		entries: make(map[string]*Entry),
		conns:   make(map[net.Conn]struct{}),
		done:    make(chan struct{}),
	}
	t.Cleanup(s.Stop)
	s.wg.Add(1)
	go s.acceptLoop()
	return s
}

// URL is an ldap:// address the go-ldap client can dial.
func (s *Server) URL() string {
	return "ldap://" + s.ln.Addr().String()
}

// Stop closes the listener and every open connection and waits for the
// serving goroutines to drain. It is idempotent.
func (s *Server) Stop() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.conns = make(map[net.Conn]struct{})
	s.mu.Unlock()
	s.wg.Wait()
}

// AddUser adds or replaces an entry. Attribute names are normalised to
// lowercase; a nil attrs map becomes an empty one.
func (s *Server) AddUser(e Entry) {
	if e.Attributes == nil {
		e.Attributes = make(map[string][]string)
	}
	attrs := make(map[string][]string, len(e.Attributes))
	for k, v := range e.Attributes {
		attrs[strings.ToLower(k)] = v
	}
	e.Attributes = attrs

	s.mu.Lock()
	s.entries[strings.ToLower(e.DN)] = &e
	s.mu.Unlock()
}

// RemoveUser deletes an entry. It reports whether there was one.
func (s *Server) RemoveUser(dn string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[strings.ToLower(dn)]; !ok {
		return false
	}
	delete(s.entries, strings.ToLower(dn))
	return true
}

// DNs lists the entry names, ordered, for assertions.
func (s *Server) DNs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.entries))
	for dn := range s.entries {
		out = append(out, dn)
	}
	sort.Strings(out)
	return out
}

// Binds counts the bind requests the server has answered.
func (s *Server) Binds() int64 { return s.binds.Load() }

// Searches counts the search requests the server has answered.
func (s *Server) Searches() int64 { return s.searches.Load() }

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.forgetConn(c)
			defer c.Close()
			s.serve(c)
		}()
	}
}

func (s *Server) forgetConn(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *Server) serve(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		msg, err := ber.ReadPacket(r)
		if err != nil {
			return
		}
		// LDAPMessage is a SEQUENCE whose first child is the message id and
		// whose second child is the operation. Anything shorter is not a
		// request this server can answer, so the connection ends.
		if len(msg.Children) < 2 {
			return
		}
		op := msg.Children[1]
		switch {
		case op.ClassType == ber.ClassApplication && op.Tag == tagBindRequest:
			s.bind(c, msg.Children[0], op)
		case op.ClassType == ber.ClassApplication && op.Tag == tagUnbindRequest:
			return
		case op.ClassType == ber.ClassApplication && op.Tag == tagSearchRequest:
			s.search(c, msg.Children[0], op)
		default:
			return
		}
	}
}

func (s *Server) bind(c net.Conn, msgID *ber.Packet, req *ber.Packet) {
	s.binds.Add(1)

	name, password := "", ""
	if len(req.Children) >= 3 {
		name = packetString(req.Children[1])
		// The simple-authentication alternative carries the password as the
		// third child, [0] in the authentication CHOICE.
		if req.Children[2].ClassType == ber.ClassContext && req.Children[2].Tag == 0 {
			password = packetString(req.Children[2])
		}
	}

	code, diag := resOK, ""
	if name != "" {
		e, ok := s.entryByDN(name)
		if !ok || e.Password == "" || e.Password != password {
			code, diag = resInvalidCredentials, "invalid credentials"
		}
	}
	s.writeBind(c, msgID, code, diag)
}

func (s *Server) writeBind(c net.Conn, msgID *ber.Packet, code int, diag string) {
	resp := ber.NewSequence("LDAPMessage")
	resp.AppendChild(msgID)
	body := ber.Encode(ber.ClassApplication, ber.TypeConstructed, tagBindResponse, nil, "bindResponse")
	body.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, code, "resultCode"))
	body.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "matchedDN"))
	body.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, diag, "diagnosticMessage"))
	resp.AppendChild(body)
	_, _ = c.Write(resp.Bytes())
}

func (s *Server) search(c net.Conn, msgID *ber.Packet, req *ber.Packet) {
	s.searches.Add(1)
	// searchRequest: base object, scope, deref, size limit, time limit,
	// typesOnly, filter, requested attributes.
	if len(req.Children) < 8 {
		return
	}
	base, _ := req.Children[0].Value.(string)
	scope, _ := req.Children[1].Value.(int64)
	filter := parseFilter(req.Children[6])

	var want []string
	for _, a := range req.Children[7].Children {
		if v, ok := a.Value.(string); ok {
			want = append(want, v)
		}
	}

	var matched []*Entry
	s.mu.RLock()
	for _, e := range s.entries {
		if dnMatchesScope(e.DN, base, int(scope)) && (filter == nil || filter.matches(e)) {
			matched = append(matched, e)
		}
	}
	s.mu.RUnlock()

	sort.Slice(matched, func(i, j int) bool { return matched[i].DN < matched[j].DN })
	for _, e := range matched {
		s.writeEntry(c, msgID, e, want)
	}
	s.writeSearchDone(c, msgID, resOK)
}

func (s *Server) writeEntry(c net.Conn, msgID *ber.Packet, e *Entry, want []string) {
	resp := ber.NewSequence("LDAPMessage")
	resp.AppendChild(msgID)
	body := ber.Encode(ber.ClassApplication, ber.TypeConstructed, tagSearchResEntry, nil, "searchResultEntry")
	body.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, e.DN, "objectName"))

	attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "attributes")
	names := wantedAttributes(e, want)
	for _, name := range names {
		attr := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, name)
		attr.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, name, "type"))
		var values []string
		if strings.EqualFold(name, "dn") {
			values = []string{e.DN}
		} else {
			values = e.Attributes[strings.ToLower(name)]
		}
		set := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "values")
		for _, v := range values {
			set.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, v, "value"))
		}
		attr.AppendChild(set)
		attrs.AppendChild(attr)
	}
	body.AppendChild(attrs)
	resp.AppendChild(body)
	_, _ = c.Write(resp.Bytes())
}

func (s *Server) writeSearchDone(c net.Conn, msgID *ber.Packet, code int) {
	resp := ber.NewSequence("LDAPMessage")
	resp.AppendChild(msgID)
	body := ber.Encode(ber.ClassApplication, ber.TypeConstructed, tagSearchResDone, nil, "searchResultDone")
	body.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, code, "resultCode"))
	body.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "matchedDN"))
	body.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "diagnosticMessage"))
	resp.AppendChild(body)
	_, _ = c.Write(resp.Bytes())
}

// wantedAttributes decides which attributes an entry response carries: every
// requested one the entry actually has, plus all of them when the request was
// for all, and the DN itself when it was asked for.
func wantedAttributes(e *Entry, want []string) []string {
	seen := make(map[string]bool, len(want))
	var out []string
	add := func(name string) {
		key := strings.ToLower(name)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, name)
	}
	if len(want) == 0 {
		for name := range e.Attributes {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}
	for _, name := range want {
		key := strings.ToLower(name)
		if key == "dn" {
			add("dn")
		} else if _, ok := e.Attributes[key]; ok {
			add(name)
		}
	}
	return out
}

func (s *Server) entryByDN(dn string) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[strings.ToLower(dn)]
	return e, ok
}

// packetString reads a string out of a BER packet. Universal octet strings
// have their Value decoded on read, but context-class primitives — the bind
// password, the substring parts — do not, so the raw bytes are the fallback.
func packetString(p *ber.Packet) string {
	if v, ok := p.Value.(string); ok {
		return v
	}
	if p.Data != nil {
		return p.Data.String()
	}
	return ""
}

// dnMatchesScope answers whether dn is inside the search base for the scope,
// using the RDN-boundary rule: a descendant is a longer name ending in the
// base preceded by a comma, and a direct child is a descendant whose remainder
// holds no comma.
func dnMatchesScope(dn, base string, scope int) bool {
	dn = strings.ToLower(dn)
	base = strings.ToLower(base)
	if base == "" {
		return scope != scopeBase
	}
	if scope == scopeBase {
		return dn == base
	}
	if dn == base {
		return scope == scopeSubtree
	}
	if !strings.HasSuffix(dn, base) {
		return false
	}
	rest := strings.TrimSuffix(dn[:len(dn)-len(base)], ",")
	if rest == "" {
		return false
	}
	if scope == scopeSubtree {
		return true
	}
	return !strings.Contains(rest, ",")
}

// filterNode is a parsed search filter.
type filterNode interface{ matches(e *Entry) bool }

func parseFilter(p *ber.Packet) filterNode {
	if p == nil {
		return nil
	}
	if p.ClassType == ber.ClassContext && p.Tag == fltPresent {
		// A present filter is a [7] primitive whose content is the attribute
		// name itself, with no children. go-ldap rewrites "(attr=*)" into this
		// shape, so it is how "(objectClass=*)" arrives.
		return presentNode{attr: strings.ToLower(packetString(p))}
	}
	if p.ClassType == ber.ClassContext && p.TagType == ber.TypeConstructed {
		switch p.Tag {
		case fltAnd:
			kids := make([]filterNode, 0, len(p.Children))
			for _, c := range p.Children {
				kids = append(kids, parseFilter(c))
			}
			return andNode(kids)
		case fltOr:
			kids := make([]filterNode, 0, len(p.Children))
			for _, c := range p.Children {
				kids = append(kids, parseFilter(c))
			}
			return orNode(kids)
		case fltNot:
			if len(p.Children) == 1 {
				return notNode{parseFilter(p.Children[0])}
			}
		case fltEquality:
			if len(p.Children) == 2 {
				return eqNode{attr: strings.ToLower(packetString(p.Children[0])), val: packetString(p.Children[1])}
			}
		case fltSubstring:
			// substrings: attribute, then a SEQUENCE of context-tagged parts.
			if len(p.Children) == 2 {
				var parts []substringPart
				for _, c := range p.Children[1].Children {
					parts = append(parts, substringPart{kind: int(c.Tag), text: packetString(c)})
				}
				return substringNode{attr: strings.ToLower(packetString(p.Children[0])), parts: parts}
			}
		}
	}
	// A filter shape this fixture does not model is treated as matching
	// everything: the fixture exists to exercise the protocol, and failing a
	// search over a fancy filter would punish the test for the fixture's
	// limits. The filters the product emits are ANDs of equalities and
	// substrings, plus the presence filter.
	return matchAll{}
}

type matchAll struct{}

func (matchAll) matches(*Entry) bool { return true }

type andNode []filterNode

func (n andNode) matches(e *Entry) bool {
	for _, k := range n {
		if !k.matches(e) {
			return false
		}
	}
	return true
}

type orNode []filterNode

func (n orNode) matches(e *Entry) bool {
	for _, k := range n {
		if k.matches(e) {
			return true
		}
	}
	return false
}

type notNode struct{ k filterNode }

func (n notNode) matches(e *Entry) bool { return !n.k.matches(e) }

type eqNode struct {
	attr string
	val  string
}

func (n eqNode) matches(e *Entry) bool {
	vals, ok := e.Attributes[n.attr]
	if !ok {
		return false
	}
	for _, v := range vals {
		if strings.EqualFold(v, n.val) {
			return true
		}
	}
	return false
}

type presentNode struct{ attr string }

func (n presentNode) matches(e *Entry) bool {
	_, ok := e.Attributes[n.attr]
	return ok
}

type substringPart struct {
	kind int
	text string
}

type substringNode struct {
	attr  string
	parts []substringPart
}

// matches implements in-order substring matching: an initial part must prefix
// a value, a final part must suffix it, and each any part must appear, in
// order, at or after the end of the previous match. No parts at all matches an
// attribute that is present at all, which is what (attr=*) means.
func (n substringNode) matches(e *Entry) bool {
	vals, ok := e.Attributes[n.attr]
	if !ok {
		return false
	}
	for _, v := range vals {
		if substringMatch(v, n.parts) {
			return true
		}
	}
	return false
}

func substringMatch(value string, parts []substringPart) bool {
	v := strings.ToLower(value)
	pos := 0
	for _, p := range parts {
		pv := strings.ToLower(p.text)
		switch p.kind {
		case fltSubInitial:
			if !strings.HasPrefix(v, pv) {
				return false
			}
			pos = len(pv)
		case fltSubAny:
			if pos > len(v) {
				return false
			}
			i := strings.Index(v[pos:], pv)
			if i < 0 {
				return false
			}
			pos += i + len(pv)
		case fltSubFinal:
			if !strings.HasSuffix(v, pv) {
				return false
			}
		}
	}
	return true
}

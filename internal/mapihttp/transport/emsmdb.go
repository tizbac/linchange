package transport

import (
	"strings"

	"github.com/linchange/linchange/internal/mapihttp/rop"
)

// The status codes the mailbox endpoint's transport-level operations answer
// with. They are the values MS-OXCRPC 3.1.4.1, 3.1.4.2 and 3.1.4.3 list as
// these operations' return values, and the same ones the reference returns.
const (
	ecSuccess      = 0x00000000
	ecServerOOM    = 0x000003F0
	ecRpcFailed    = 0x80040115
	ecAccessDenied = 0x80070005

	// rpcXBadStubData is what a request too large to be a request is
	// answered with: the body parsed, and then refused for its size
	// rather than for its shape.
	rpcXBadStubData = 0x000006F7

	// connectPollsMax, connectRetry and connectRetryDelay are what a
	// successful Connect promises: poll this many times for an answer,
	// retry this often, wait this long between retries (milliseconds).
	// The reference answers every successful Connect with these three
	// numbers, and a client that can honour them keeps waiting rather
	// than deciding the server has gone away.
	connectPollsMax   = 60000
	connectRetry      = 6
	connectRetryDelay = 10000

	// ropBufferMaxIn and auxiliaryBufferMaxIn bound what a client may
	// claim its ROP list and auxiliary buffer measure, and ropHeaderSize
	// is the smallest ROP list — a header is the whole of it (MS-OXCRPC
	// 3.1.4.2).
	ropBufferMaxIn       = 0x40000
	auxiliaryBufferMaxIn = 0x1008
	ropHeaderSize        = 8
)

// dispatchEMSMDB runs one request against the mailbox endpoint
// (MS-OXCMAPIHTTP 2.2.4).
//
// The order is the reference's: a PING is answered before anything asks what
// kind of request it is, because a PING is how a client checks that a session
// is still there and the answer must not depend on the session having been
// able to parse anything.
func (q *request) dispatchEMSMDB(body []byte) {
	if strings.EqualFold(q.requestType, "PING") {
		q.pingResponse()
		return
	}
	var (
		pdu     []byte
		handled bool
	)
	switch {
	case strings.EqualFold(q.requestType, "Connect"):
		pdu, handled = q.connect(body)
	case strings.EqualFold(q.requestType, "Execute"):
		pdu, handled = q.execute(body)
	case strings.EqualFold(q.requestType, "Disconnect"):
		pdu, handled = q.disconnect(body)
	case strings.EqualFold(q.requestType, "NotificationWait"):
		pdu, handled = q.notificationWait(body)
	default:
		handled = q.errorResponse(respInvalidRequestType)
	}
	if handled {
		return
	}
	q.normalResponse(pdu)
}

// loadEMSMDBCookies validates a mailbox endpoint request's session cookies
// against the endpoint's session table (MS-OXCMAPIHTTP 3.1.5.1).
//
// A Connect is treated more gently than everything else: a client whose
// cookies are stale, mangled or simply not ours is starting a new session
// anyway, so it is handed an empty one rather than an error, and the sid it
// sent is dropped rather than echoed back. Every other request is asking for
// a session it has to name correctly first.
func (q *request) loadEMSMDBCookies() bool {
	rt := q.requestType
	raw := strings.Join(q.r.Header.Values("Cookie"), ";")
	if raw == "" {
		if !strings.EqualFold(rt, "Connect") {
			q.errorResponse(respMissingCookie)
			return false
		}
		return true
	}
	isConnect := strings.EqualFold(rt, "Connect")
	fresh := func() bool {
		q.sid, q.sequence, q.session = "", "", nil
		return true
	}

	cookies := parseCookies(raw)
	sid, ok := cookies["sid"]
	if !ok || sid == "" || len(sid) > sidLength {
		if isConnect {
			return fresh()
		}
		q.errorResponse(respInvalidContextCookie)
		return false
	}
	// A PING and a NotificationWait are the two requests that may arrive
	// without a sequence: the first changes nothing and the second is the
	// one long-running request a client cannot be expected to keep a
	// rotation in step with while it waits.
	if !strings.EqualFold(rt, "PING") && !strings.EqualFold(rt, "NotificationWait") {
		seq, ok := cookies["sequence"]
		if !ok || !isSequence(seq) {
			if isConnect {
				return fresh()
			}
			q.errorResponse(respInvalidContextCookie)
			return false
		}
		q.sequence = seq
	}

	s, ok := q.t.lookupSession(endpointEMSMDB, sid)
	if !ok {
		if isConnect {
			return fresh()
		}
		q.errorResponse(respContextNotFound)
		return false
	}
	if !strings.EqualFold(s.username, q.principal.Address) {
		// MS-OXCMAPIHTTP 3.2.5.1: the authentication context the
		// Session Context was created with has to be the one
		// presenting it now. The cookie is only as private as the
		// password that got this far, so a context minted for one
		// account is not transferable to another.
		if isConnect {
			return fresh()
		}
		q.errorResponse(respContextNotFound)
		return false
	}
	q.sid = sid
	q.session = s

	// An Execute may only be outstanding once per Session Context. A
	// second one arriving before the first has been answered would be
	// written into a handle table the first is still using, so it is
	// refused as an invalid sequence rather than run concurrently.
	if strings.EqualFold(rt, "Execute") && q.sequence != s.sequence {
		q.errorResponse(respInvalidSequence)
		return false
	}

	// Everything except a PING, a Disconnect and a NotificationWait moves
	// the sequence on, so that the value a client sends back next is one
	// nobody who watched this exchange can replay into the session.
	if !strings.EqualFold(rt, "PING") &&
		!strings.EqualFold(rt, "Disconnect") &&
		!strings.EqualFold(rt, "NotificationWait") {
		seq, err := newSequence()
		if err != nil {
			q.t.log.ErrorContext(q.r.Context(), "mapi/http: could not mint a sequence value",
				"endpoint", q.endpoint, "error", err)
			q.failureResponse(ecServerOOM)
			return false
		}
		q.sequence = seq
		s.sequence = seq
	}
	s.expires = q.start.Add(sessionIdle)
	return true
}

// connect establishes a Session Context (MS-OXCMAPIHTTP 2.2.4.1).
//
// The one thing it decides is whether the UserDn the client claims names the
// account the client authenticated as. The reference resolves the DN to an
// account and compares the two addresses; this build reads the DN's last RDN
// and compares that, because the DN a client sends came from this server's
// own Autodiscover or address book and its last RDN is the only part of it
// that says who the mailbox belongs to. A DN naming anyone else is refused
// here, before the cookie that would open the mailbox is minted.
func (q *request) connect(body []byte) ([]byte, bool) {
	userDN, err := readConnect(body)
	if err != nil {
		return nil, q.errorResponse(respInvalidRequestBody)
	}

	display := q.displayName()
	result := uint32(ecSuccess)
	cn, parsed := splitLastCN(userDN)
	switch {
	case userDN == "":
		result = ecAccessDenied
	case !parsed:
		// A DN with no RDN to read names no account this server can
		// find, which the reference reports as a failure to resolve.
		result = ecRpcFailed
	case !namesAccount(cn, q.principal.Address, display):
		// The mailbox named is not the account that authenticated.
		result = ecAccessDenied
	}

	if result == ecSuccess && !q.establishSession() {
		return nil, false
	}
	return connectResponse(result, display), false
}

// readConnect parses a Connect request body (MS-OXCMAPIHTTP 2.2.4.1.1):
// UserDn, Flags, DefaultCodePage, two locale ids, and the auxiliary buffer.
//
// The Flags field asks for privileges. The reference refuses any client that
// asks; this build reads it and drops it instead, because nothing here grants
// or withholds a privilege either way and refusing a connection that is
// otherwise exactly what the protocol asked for is a worse answer than
// granting a request that has no effect.
func readConnect(body []byte) (string, error) {
	r := newReader(body)
	userDN, err := r.cstr()
	if err != nil {
		return "", err
	}
	if _, err = r.u32(); err != nil { // flags
		return "", err
	}
	if _, err = r.u32(); err != nil { // default code page
		return "", err
	}
	// The reference reads the two locale ids in the order the field
	// description lists them, the specification's diagram in the other.
	// Both are u32 and neither is used, so the order costs nothing to
	// match and nothing to differ on.
	if _, err = r.u32(); err != nil { // locale id
		return "", err
	}
	if _, err = r.u32(); err != nil { // locale id
		return "", err
	}
	if err = r.aux(); err != nil {
		return "", err
	}
	return userDN, nil
}

// establishSession gives the request a Session Context to belong to, either
// by opening a fresh one or by giving the live context it reconnected with a
// new ROP state — a Connect is a new logon, and the handle table the previous
// one built is not the new one's.
func (q *request) establishSession() bool {
	sess := rop.NewSession()
	sess.SetOwner(q.principal.Address)
	if q.session != nil {
		// The sid and the already-rotated sequence belong to this
		// request; only the ROP state is replaced.
		q.session.rop = sess
		return true
	}
	sid, err := newSID()
	if err != nil {
		q.t.log.ErrorContext(q.r.Context(), "mapi/http: could not mint a session id",
			"endpoint", q.endpoint, "error", err)
		q.failureResponse(ecServerOOM)
		return false
	}
	seq, err := newSequence()
	if err != nil {
		q.t.log.ErrorContext(q.r.Context(), "mapi/http: could not mint a sequence value",
			"endpoint", q.endpoint, "error", err)
		q.failureResponse(ecServerOOM)
		return false
	}
	q.sid, q.sequence, q.session = sid, seq, &session{
		username: q.principal.Address,
		sequence: seq,
		expires:  q.start.Add(sessionIdle),
		rop:      sess,
	}
	q.t.putSession(endpointEMSMDB, sid, q.session)
	return true
}

// connectResponse builds the Connect response body (MS-OXCMAPIHTTP
// 2.2.4.1.2).
//
// A failed Connect reports the fields the reference's connect_ex zeroes on
// its way out: no polling interval to promise, no retries to count, and no
// display name for a mailbox that was not opened. The specification also
// defines a shorter failure body (2.2.4.1.3) that this build does not emit —
// the reference serialises one shape whatever the result, and a client can
// tell the two apart by the first u32 either way.
func connectResponse(result uint32, display string) []byte {
	w := &writer{}
	w.u32(0) // StatusCode: always 0
	w.u32(result)
	if result == ecSuccess {
		w.u32(connectPollsMax)
		w.u32(connectRetry)
		w.u32(connectRetryDelay)
		w.str("") // DnPrefix: this build issues no DN prefix
		w.wstr(display)
	} else {
		w.u32(0)
		w.u32(0)
		w.u32(0)
		w.str("")
		w.wstr("")
	}
	w.u32(0) // AuxiliaryBufferSize: nothing to say in an auxiliary buffer
	return w.buf
}

// displayName returns the account's display name, which is what a Connect
// reports to the client and the name a profile shows while it is still
// deciding whether it points at the right mailbox.
//
// The address is the fallback rather than the empty string: a client that is
// told nothing shows a blank mailbox where it would otherwise show the
// account it just authenticated as, and an address is a name this server
// always has.
func (q *request) displayName() string {
	if q.t.opts.Users == nil || q.t.opts.DB == nil {
		return q.principal.Address
	}
	u, err := q.t.opts.Users.Get(q.r.Context(), q.t.opts.DB, q.principal.Address)
	if err != nil || u.DisplayName == "" {
		return q.principal.Address
	}
	return u.DisplayName
}

// splitLastCN reads a distinguished name's final RDN, which is the part of it
// that says which recipient it names. The separator is matched without case
// because a DN is written by whichever component produced it and the two in
// circulation here differ only in how they were typed.
func splitLastCN(dn string) (string, bool) {
	const marker = "/cn="
	i := strings.LastIndex(strings.ToLower(dn), marker)
	if i < 0 {
		return "", false
	}
	return dn[i+len(marker):], true
}

// namesAccount reports whether a distinguished name's last RDN names this
// account. Three spellings of that RDN are in circulation: the local part of
// the address (what Autodiscover hands out), the address itself, and the
// display name (what the address book issues). Which one a client sends
// depends on where it got the profile rather than on who it is, so all three
// are accepted and anything else is not this account.
func namesAccount(cn, address, display string) bool {
	if cn == "" {
		return false
	}
	local, _, _ := strings.Cut(address, "@")
	return strings.EqualFold(cn, address) ||
		(local != "" && strings.EqualFold(cn, local)) ||
		(display != "" && strings.EqualFold(cn, display))
}

// execute runs one ROP list (MS-OXCMAPIHTTP 2.2.4.2).
//
// The session is not looked up again: loadEMSMDBCookies already refused any
// Execute that did not name a live one.
func (q *request) execute(body []byte) ([]byte, bool) {
	req, err := readExecute(body)
	if err != nil {
		return nil, q.errorResponse(respInvalidRequestBody)
	}
	// The bounds MS-OXCRPC 3.1.4.2 puts on a single call. A ROP list
	// over the limit is a malformed call rather than a large one, and is
	// refused before any of it is executed; an auxiliary buffer over the
	// limit is refused for the same reason.
	if len(req.in) > ropBufferMaxIn {
		q.failureResponse(rpcXBadStubData)
		return nil, true
	}
	if len(req.aux) > auxiliaryBufferMaxIn {
		q.failureResponse(rpcXBadStubData)
		return nil, true
	}

	result := uint32(ecSuccess)
	var out []byte
	if len(req.in) < ropHeaderSize || req.maxOut < ropHeaderSize {
		// A ROP list smaller than its own header cannot be a ROP
		// list, and a buffer offered for the answer that cannot hold
		// one cannot receive one.
		result = ecRpcFailed
	} else {
		out, err = q.session.rop.Execute(q.r.Context(), q.t.emsmdb, q.t.log, req.in)
		if err != nil {
			out = nil
			if code, ok := rop.ErrorCode(err); ok {
				result = code
			} else {
				// The ROP layer refuses a whole call for reasons
				// it could not put a status on: a buffer it could
				// not parse, a store that would not answer.
				result = ecRpcFailed
				q.t.log.InfoContext(q.r.Context(), "mapi/http: the ROP list could not be run",
					"endpoint", q.endpoint, "error", err)
			}
		}
	}
	if uint64(len(out)) > uint64(req.maxOut) {
		out = out[:int(req.maxOut)]
	}

	w := &writer{}
	w.u32(0) // StatusCode: always 0
	w.u32(result)
	w.u32(0) // Flags: reserved, and the server sets it to zero
	w.blob(out)
	w.u32(0) // AuxiliaryBufferSize
	return w.buf, false
}

// executeRequest is a parsed Execute request body.
type executeRequest struct {
	in     []byte // the ROP list
	maxOut uint32 // MaxRopOut: the largest answer the client can take
	aux    []byte // the auxiliary buffer, read for its bounds and nothing else
}

// readExecute parses an Execute request body: Flags, RopBufferSize, RopBuffer,
// MaxRopOut, AuxiliaryBufferSize, AuxiliaryBuffer.
//
// The ROP list and the auxiliary buffer come back as views onto the body that
// was read once, so a client that claims forty megabytes of either costs
// nothing extra to discover it does not have them: the claim runs off the end
// of what it actually sent.
func readExecute(body []byte) (executeRequest, error) {
	var req executeRequest
	r := newReader(body)
	if _, err := r.u32(); err != nil { // flags
		return req, err
	}
	n, err := r.u32()
	if err != nil {
		return req, err
	}
	if req.in, err = r.blob(n); err != nil {
		return req, err
	}
	if req.maxOut, err = r.u32(); err != nil {
		return req, err
	}
	if n, err = r.u32(); err != nil {
		return req, err
	}
	if req.aux, err = r.blob(n); err != nil {
		return req, err
	}
	return req, nil
}

// disconnect deletes the Session Context (MS-OXCMAPIHTTP 2.2.4.3).
//
// The cookie still travels back with the response — MS-OXCMAPIHTTP 3.2.5.4
// requires the Set-Cookie headers even though the context they name has just
// been invalidated — so the client is told plainly, on its next request, that
// the context it kept is gone.
func (q *request) disconnect(body []byte) ([]byte, bool) {
	if err := newReader(body).aux(); err != nil {
		return nil, q.errorResponse(respInvalidRequestBody)
	}
	q.t.dropSession(endpointEMSMDB, q.sid)
	w := &writer{}
	w.u32(0) // StatusCode
	w.u32(ecSuccess)
	w.u32(0) // AuxiliaryBufferSize
	return w.buf, false
}

// notificationWait asks to be told when a long-running request finishes
// (MS-OXCMAPIHTTP 2.2.4.4).
//
// This build answers at once with no event pending rather than parking the
// request until one arrives: the reference keeps the request open and
// completes it from a background wakeup, which needs a second chunk to be
// written before the body is known — something the framing here does not
// reach for. A client told there is no event sends the Execute the
// specification tells it to send, so nothing is lost but the wait.
func (q *request) notificationWait(body []byte) ([]byte, bool) {
	r := newReader(body)
	if _, err := r.u32(); err != nil { // flags, ignored by the server
		return nil, q.errorResponse(respInvalidRequestBody)
	}
	if err := r.aux(); err != nil {
		return nil, q.errorResponse(respInvalidRequestBody)
	}
	w := &writer{}
	w.u32(0) // StatusCode
	w.u32(0) // ErrorCode
	w.u32(0) // EventPending: no event, and none queued
	w.u32(0) // AuxiliaryBufferSize
	return w.buf, false
}

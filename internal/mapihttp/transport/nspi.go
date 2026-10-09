package transport

import (
	"encoding/binary"
	"strings"

	"github.com/linchange/linchange/internal/mapihttp/nspi"
)

// dispatchNSPI runs one request against the address book endpoint
// (MS-OXCMAPIHTTP 2.2.5).
//
// The reference lowercases X-RequestType before looking it up, so the
// client's spelling does not matter, and the lookup happens after the
// cookies have been checked — a request nobody has heard of still has to
// belong to a session to be worth rejecting.
func (q *request) dispatchNSPI(body []byte) {
	name := strings.ToLower(q.requestType)
	if !nspi.IsOperation(name) {
		q.errorResponse(respInvalidRequestType)
		return
	}

	// A Bind without cookies has no session yet; the operation mints one
	// if it succeeds. Everything else was given one by loadNSPICookies.
	s := q.sessionNspi()
	out, err := s.Execute(q.r.Context(), q.t.nspi, name, q.principal.Address, body)
	if err != nil {
		if nspi.IsFormatError(err) {
			q.errorResponse(respInvalidRequestBody)
			return
		}
		// Anything else is the server failing to answer rather than
		// the client failing to ask: the directory could not be read.
		q.t.log.ErrorContext(q.r.Context(), "mapi/http: the address book operation failed",
			"endpoint", q.endpoint, "operation", name, "error", err)
		q.errorResponse(respUnknownFailure)
		return
	}

	switch name {
	case "bind":
		if !q.recordBind(s, out) {
			return
		}
	case "unbind":
		// The context is released before the reply goes out, as the
		// reference does, so a request that races this one finds it
		// gone rather than half-torn-down.
		q.t.dropSession(endpointNSPI, q.sid)
	}
	q.normalResponse(out)
}

// sessionNspi returns the address book state this request should run on: the
// one its session holds, or a fresh one for a Bind that has not yet earned a
// session of its own.
func (q *request) sessionNspi() *nspi.Session {
	if q.session != nil && q.session.nspi != nil {
		return q.session.nspi
	}
	return nspi.NewSession()
}

// loadNSPICookies validates an address book request's session cookies
// against the endpoint's session table (MS-OXCMAPIHTTP 3.1.5.1).
//
// Unlike a Connect, a Bind with unusable cookies is refused rather than
// quietly restarted: there is no "start over" for an address book session,
// because the client asked for one by name and getting that wrong is a
// cookie that was not ours.
func (q *request) loadNSPICookies() bool {
	rt := q.requestType
	raw := strings.Join(q.r.Header.Values("Cookie"), ";")
	if raw == "" {
		// A Bind without cookies is a client asking for a context it
		// does not have yet; everything else is a client using one it
		// never named.
		if !strings.EqualFold(rt, "Bind") {
			q.errorResponse(respMissingCookie)
			return false
		}
		return true
	}

	cookies := parseCookies(raw)
	sid, ok := cookies["sid"]
	if !ok || sid == "" || len(sid) > sidLength {
		q.errorResponse(respInvalidContextCookie)
		return false
	}
	// A PING and an Unbind are exempt from the sequence, the first
	// because it changes nothing and the second because it is the last
	// thing a client does before letting its cookies lapse.
	if !strings.EqualFold(rt, "PING") && !strings.EqualFold(rt, "Unbind") {
		seq, ok := cookies["sequence"]
		if !ok || !isSequence(seq) {
			q.errorResponse(respInvalidContextCookie)
			return false
		}
		q.sequence = seq
	}

	s, ok := q.t.lookupSession(endpointNSPI, sid)
	if !ok {
		q.errorResponse(respContextNotFound)
		return false
	}
	if !strings.EqualFold(s.username, q.principal.Address) {
		// MS-OXCMAPIHTTP 3.2.5.1 again: the context belongs to the
		// account that established it, and presenting somebody
		// else's cookie is not the same as presenting their
		// credentials.
		q.errorResponse(respContextNotFound)
		return false
	}
	q.sid = sid
	q.session = s

	// A second request in this context before the first was answered is
	// refused rather than interleaved: the address book keeps state
	// between operations (a position in the GAL, a version counter) that
	// only makes sense if they happen one at a time.
	if !strings.EqualFold(rt, "PING") &&
		!strings.EqualFold(rt, "Bind") &&
		!strings.EqualFold(rt, "Unbind") &&
		q.sequence != s.sequence {
		q.errorResponse(respInvalidSequence)
		return false
	}
	// Everything except a PING and an Unbind moves the sequence on — a
	// Bind included, so that a rebind costs the old sequence its value.
	if !strings.EqualFold(rt, "PING") && !strings.EqualFold(rt, "Unbind") {
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

	if strings.EqualFold(rt, "PING") {
		q.pingResponse()
		return false
	}
	return true
}

// recordBind gives a successful Bind a Session Context of its own. A Bind
// that failed answers with its error and leaves the client holding whatever
// cookies it arrived with, because no context was created to hand back.
// It reports false when the response it had to write instead was a failure
// of this server's own.
//
// A rebind — a Bind that arrived with a live context — keeps that context:
// the sid stays, the rotated sequence stays, and only the address book state
// is renewed, which is what the reference does when it replaces the handle
// behind an existing session.
func (q *request) recordBind(s *nspi.Session, out []byte) bool {
	if !bindSucceeded(out) {
		return true
	}
	if q.session != nil {
		q.session.nspi = s
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
		nspi:     s,
	}
	q.t.putSession(endpointNSPI, sid, q.session)
	return true
}

// bindSucceeded reads a Bind response's result, which is the second u32 of
// the PDU (MS-OXNSPI 2.2.13: StatusCode, ErrorCode, ServerGuid). The result
// is the only thing here that says whether a context exists now — the
// response itself is the same shape either way, so the cookie decision has
// to be read off it.
func bindSucceeded(out []byte) bool {
	if len(out) < 8 {
		return false
	}
	return binary.LittleEndian.Uint32(out[4:8]) == 0
}

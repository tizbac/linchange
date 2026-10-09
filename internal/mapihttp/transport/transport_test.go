package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

// These tests are about the framing: what is refused and with which
// X-ResponseCode, which cookies a request has to present, how a response is
// chunked, and which bytes say what. The operations a body asks for live in
// the rop and nspi packages and are tested there.

// fixedAuthenticator accepts any credentials and answers with the account the
// login names, so one test can present two people to one session.
type fixedAuthenticator struct {
	// action records the last action asked about, which is how a test
	// checks that a MAPI/HTTP login is decided by the MAPI/HTTP rule and
	// not by another protocol's.
	action policy.Action
}

func (f *fixedAuthenticator) Authenticate(_ context.Context, login, _ string, action policy.Action) (policy.Principal, error) {
	f.action = action
	if login == "" {
		return policy.Principal{}, errors.New("no such account")
	}
	return policy.Principal{Address: strings.ToLower(login), MailboxEnabled: true}, nil
}

// refusingAuthenticator rejects every login, as a verifier does for a wrong
// password.
type refusingAuthenticator struct{}

func (refusingAuthenticator) Authenticate(context.Context, string, string, policy.Action) (policy.Principal, error) {
	return policy.Principal{}, errors.New("rejected")
}

func newTestTransport(auth Authenticator) *Transport {
	return New(Options{
		Hostname: "mail.example.test",
		Auth:     auth,
	})
}

// mount serves the transport at the path the production router mounts it at,
// so the routing these tests exercise is the routing a client gets.
func mount(tr *Transport) http.Handler {
	root := chi.NewRouter()
	root.Mount("/mapi", tr.Handler())
	return root
}

// mapiPost builds a request that is well formed except for whatever the test
// goes on to remove. The trailing slash in the path is deliberate: it is part
// of the request target this protocol uses, not an accident of the query.
func mapiPost(endpoint, requestType, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mapi/"+endpoint+"/?MailboxId=alice@example.test",
		strings.NewReader(body))
	req.Header.Set("X-RequestType", requestType)
	req.Header.Set("X-RequestId", "00000001")
	req.Header.Set("X-ClientInfo", "linchange-test")
	req.SetBasicAuth("alice@example.test", "correct-horse")
	return req
}

// serve runs one request and returns what the client would see.
func serve(t *testing.T, tr *Transport, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	mount(tr).ServeHTTP(rr, req)
	return rr
}

// cookieNamed finds one Set-Cookie by name.
func cookieNamed(t *testing.T, rr *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q cookie in the response; got %v", name, rr.Result().Cookies())
	return nil
}

// pdu returns what follows the meta-tags: the operation's own response body.
func pdu(t *testing.T, rr *httptest.ResponseRecorder) []byte {
	t.Helper()
	body := rr.Body.Bytes()
	i := bytes.Index(body, []byte("\r\n\r\n"))
	if i < 0 {
		t.Fatalf("response has no end of meta-tags: %q", body)
	}
	return body[i+4:]
}

// wantCode checks the X-ResponseCode a request was answered with.
func wantCode(t *testing.T, rr *httptest.ResponseRecorder, code int) {
	t.Helper()
	if got := rr.Header().Get("X-ResponseCode"); got != strconv.Itoa(code) {
		t.Errorf("X-ResponseCode = %q, want %d; body: %s", got, code, rr.Body.String())
	}
}

// testDN is the shape this server's own components hand out: an Exchange-style
// distinguished name whose last RDN names the recipient. A bare "cn=alice" is
// not one — it has no RDN separator to read the name out of — which is why
// the tests build their DNs through here rather than typing them inline.
func testDN(cn string) string {
	return "/o=linchange/ou=Exchange Administrative Group (FYDIBOHF23SPDLT)" +
		"/cn=Recipients/cn=" + cn
}

// connectBody builds a Connect request naming the given user DN.
func connectBody(userDN string) string {
	b := append([]byte(userDN), 0)
	for _, v := range []uint32{
		0,    // flags
		1252, // default code page
		1033, // locale id
		1033, // locale id
		0,    // auxiliary buffer size
	} {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	return string(b)
}

// shortROPBody is an Execute whose ROP list is shorter than a ROP header: it
// parses as a request and cannot be run as one.
func shortROPBody() string {
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, 0)      // flags
	b = binary.LittleEndian.AppendUint32(b, 4)      // RopBufferSize
	b = append(b, 0, 0, 0, 0)                       // RopBuffer
	b = binary.LittleEndian.AppendUint32(b, 0x8000) // MaxRopOut
	b = binary.LittleEndian.AppendUint32(b, 0)      // AuxiliaryBufferSize
	return string(b)
}

// longROPBody is an Execute whose ROP list is over the 256 KiB a single call
// may carry, but which really is that long — so the refusal is for the size,
// not for a claim that ran off the end of the body.
func longROPBody(size uint32) string {
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint32(b, size)
	b = append(b, make([]byte, size)...)
	b = binary.LittleEndian.AppendUint32(b, 0x8000)
	b = binary.LittleEndian.AppendUint32(b, 0)
	return string(b)
}

// A server with no verifier refuses everything. Credentials are the only
// thing that says whose mail a MAPI/HTTP request may touch, and the refusal
// has to come before the request is even read: a caller who cannot
// authenticate learns nothing about which headers this endpoint wants.
func TestRequestWithoutCredentialsIsRefusedBeforeAnythingElse(t *testing.T) {
	tr := newTestTransport(refusingAuthenticator{})

	for _, endpoint := range []string{"emsmdb", "nspi"} {
		req := httptest.NewRequest(http.MethodPost, "/mapi/"+endpoint+"/", strings.NewReader(""))
		rr := serve(t, tr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", endpoint, rr.Code)
		}
		if got := rr.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
			t.Errorf("%s: WWW-Authenticate = %q, want a Basic challenge", endpoint, got)
		}
		// Not an X-ResponseCode: the refusal predates the protocol.
		if got := rr.Header().Get("X-ResponseCode"); got != "" {
			t.Errorf("%s: X-ResponseCode = %q on an unauthenticated refusal", endpoint, got)
		}
	}
}

// One account has one answer to "may this account use this protocol", and
// MAPI/HTTP is one of the protocols that asks. Asking with another action's
// rule would let an account use MAPI/HTTP under a rule meant for something
// else.
func TestLoginIsCheckedWithTheMAPIHTTPAction(t *testing.T) {
	auth := &fixedAuthenticator{}
	tr := newTestTransport(auth)

	serve(t, tr, mapiPost("emsmdb", "Connect", connectBody("/o=linchange/ou=Exchange Administrative Group/cn=Recipients/cn=alice")))

	if auth.action != policy.ActionMAPIHTTP {
		t.Errorf("action = %q, want %q", auth.action, policy.ActionMAPIHTTP)
	}
}

// The request type names the operation. Without it there is nothing to
// dispatch, and the code says so rather than guessing a default.
func TestMissingRequestTypeIsRefusedWithInvalidVerb(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	req := mapiPost("emsmdb", "Connect", connectBody(testDN("alice")))
	req.Header.Del("X-RequestType")
	rr := serve(t, tr, req)

	wantCode(t, rr, respInvalidVerb)
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want an HTML diagnostic", ct)
	}
	if !strings.Contains(rr.Body.String(), "MAPI OVER HTTP ERROR") {
		t.Errorf("the diagnostic body is not the protocol's own: %s", rr.Body.String())
	}
	if cl, err := strconv.Atoi(rr.Header().Get("Content-Length")); err != nil || cl != rr.Body.Len() {
		t.Errorf("Content-Length = %q, want the %d bytes actually sent",
			rr.Header().Get("Content-Length"), rr.Body.Len())
	}
}

// X-RequestId and X-ClientInfo are what a failure can be traced back to. A
// request carrying neither cannot be talked about, so it is refused before
// it is parsed.
func TestMissingTracingHeadersAreRefused(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	for _, header := range []string{"X-RequestId", "X-ClientInfo"} {
		req := mapiPost("emsmdb", "Connect", connectBody(testDN("alice")))
		req.Header.Del(header)
		rr := serve(t, tr, req)

		wantCode(t, rr, respMissingHeader)
	}
}

// A request body is bounded because it is read before it is authenticated
// into anything useful and an unbounded read is a memory-exhaustion vector
// on a protocol that has to answer before it can do any work.
func TestOversizedBodyIsRefused(t *testing.T) {
	tr := New(Options{
		Hostname:     "mail.example.test",
		Auth:         &fixedAuthenticator{},
		MaxBodyBytes: 64,
	})

	rr := serve(t, tr, mapiPost("emsmdb", "Connect", strings.Repeat("x", 128)))
	wantCode(t, rr, respTooLarge)
}

// A Connect with no cookies is a client asking for a session it does not have
// yet. It is answered with one: a sid, a sequence, and a body that says which
// mailbox was opened and how long the client should wait for an answer.
func TestConnectEstablishesASession(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	rr := serve(t, tr, mapiPost("emsmdb", "Connect",
		connectBody("/o=linchange/ou=Exchange Administrative Group/cn=Recipients/cn=alice")))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-Type") != "application/mapi-http" {
		t.Errorf("Content-Type = %q, want application/mapi-http", rr.Header().Get("Content-Type"))
	}
	sid := cookieNamed(t, rr, "sid")
	if len(sid.Value) != sidLength {
		t.Errorf("sid = %q, want %d characters", sid.Value, sidLength)
	}
	seq := cookieNamed(t, rr, "sequence")
	if !isSequence(seq.Value) {
		t.Errorf("sequence = %q, want a GUID", seq.Value)
	}

	r := newReader(pdu(t, rr))
	status := nextU32(t, r)
	result := nextU32(t, r)
	maxPolls := nextU32(t, r)
	maxRetry := nextU32(t, r)
	retryDelay := nextU32(t, r)
	if status != 0 {
		t.Errorf("StatusCode = %#x, want 0", status)
	}
	if result != 0 {
		t.Errorf("ErrorCode = %#x, want success", result)
	}
	if maxPolls != connectPollsMax || maxRetry != connectRetry ||
		retryDelay != connectRetryDelay {
		t.Errorf("polling promise = (%d, %d, %d), want (%d, %d, %d)",
			maxPolls, maxRetry, retryDelay,
			connectPollsMax, connectRetry, connectRetryDelay)
	}
	if prefix, err := r.cstr(); err != nil || prefix != "" {
		t.Errorf("DnPrefix = %q (%v), want the empty string: this build issues none", prefix, err)
	}
	if display := wstrIn(t, r); display != "alice@example.test" {
		t.Errorf("DisplayName = %q, want the account's", display)
	}
}

// Every 200 carries the same header block. They are not decoration: a client
// reads X-PendingPeriod to know how long to wait before asking again and
// X-ExpirationInfo to know how long its session survives being idle.
func TestResponseCarriesTheProtocolHeaders(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	rr := serve(t, tr, mapiPost("emsmdb", "Connect", connectBody(testDN("alice"))))

	for header, want := range map[string]string{
		"X-RequestType":       "Connect",
		"X-RequestId":         "00000001",
		"X-ClientInfo":        "linchange-test",
		"X-ResponseCode":      "0",
		"X-PendingPeriod":     "30000",
		"X-ExpirationInfo":    "900000",
		"X-ServerApplication": "Exchange/" + defaultServerVersion,
		"Cache-Control":       "private",
	} {
		if got := rr.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// A normal response cannot name its own length before it has been written:
// the meta-tags go out first so a client can see the request was accepted,
// and the body follows. That is a chunked response, and the terminal 0-chunk
// belongs to the framing, not to the body.
func TestNormalResponseIsChunkedAndEndsWithTheBody(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})
	srv := httptest.NewServer(mount(tr))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mapi/emsmdb/?MailboxId=alice",
		strings.NewReader(connectBody(testDN("alice"))))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("X-RequestType", "Connect")
	req.Header.Set("X-RequestId", "00000001")
	req.Header.Set("X-ClientInfo", "linchange-test")
	req.SetBasicAuth("alice@example.test", "correct-horse")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sending the request: %v", err)
	}
	defer resp.Body.Close()
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}

	if resp.ContentLength != -1 || len(resp.TransferEncoding) == 0 ||
		resp.TransferEncoding[0] != "chunked" {
		t.Errorf("response was Content-Length %d / %v, want chunked",
			resp.ContentLength, resp.TransferEncoding)
	}
	if !bytes.HasPrefix(body, []byte("PROCESSING\r\nDONE\r\n")) {
		t.Errorf("the body does not start with the meta-tags: %q", head(body, 64))
	}
	if bytes.Contains(body, []byte("\r\n0\r\n\r\n")) {
		t.Errorf("the terminal chunk was written into the body: %q", tail(body, 32))
	}
}

// A PING changes nothing, so it is answered with the meta-tags alone and a
// Content-Length, because the whole answer is known before it is written.
func TestPingAnswersWithTheMetaTagsAlone(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	first := serve(t, tr, mapiPost("emsmdb", "Connect", connectBody(testDN("alice"))))
	sid, seq := cookieNamed(t, first, "sid"), cookieNamed(t, first, "sequence")

	req := mapiPost("emsmdb", "PING", "")
	req.AddCookie(sid)
	req.AddCookie(seq)
	rr := serve(t, tr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-RequestType"); got != "PING" {
		t.Errorf("X-RequestType = %q, want PING", got)
	}
	if _, err := strconv.Atoi(rr.Header().Get("Content-Length")); err != nil {
		t.Errorf("a PING has to be Content-Length'd, got %q", rr.Header().Get("Content-Length"))
	}
	if got := pdu(t, rr); len(got) != 0 {
		t.Errorf("a PING carries no body, got %d bytes", len(got))
	}
}

// Everything but a Connect needs a session to belong to. A client that sends
// no cookie at all is told it forgot one rather than being told which session
// it was probably looking for.
func TestRequestsWithoutACookieAreRefused(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	for _, tc := range []struct{ endpoint, requestType string }{
		{"emsmdb", "Execute"},
		{"emsmdb", "Disconnect"},
		{"emsmdb", "NotificationWait"},
		{"emsmdb", "PING"},
		{"nspi", "GetProps"},
		{"nspi", "Unbind"},
		{"nspi", "PING"},
	} {
		rr := serve(t, tr, mapiPost(tc.endpoint, tc.requestType, ""))
		wantCode(t, rr, respMissingCookie)
	}
}

// A Connect whose cookies are stale, mangled or not ours starts a new session
// rather than refusing to connect — the client is establishing one anyway.
// Any other request using the same cookies is refused instead.
func TestStaleSessionCookieIsForgivenOnlyForConnect(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	stale := &http.Cookie{Name: "sid", Value: strings.Repeat("0", sidLength)}

	req := mapiPost("emsmdb", "Connect", connectBody(testDN("alice")))
	req.AddCookie(stale)
	rr := serve(t, tr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Connect = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if got := cookieNamed(t, rr, "sid").Value; got == stale.Value {
		t.Errorf("Connect kept a sid for a session that does not exist: %q", got)
	}

	req = mapiPost("emsmdb", "Execute", shortROPBody())
	req.AddCookie(stale)
	req.AddCookie(&http.Cookie{Name: "sequence", Value: newTestSequence(t)})
	rr = serve(t, tr, req)
	wantCode(t, rr, respContextNotFound)
}

// The sequence cookie is the proof that a request is the one the client
// really sent: a value nobody who watched the last exchange can produce. An
// Execute that presents the wrong one, or presents none, is refused rather
// than run, and the value rotates on every request that is allowed through.
func TestExecuteEnforcesAndRotatesTheSequence(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	first := serve(t, tr, mapiPost("emsmdb", "Connect", connectBody(testDN("alice"))))
	sid, seq := cookieNamed(t, first, "sid"), cookieNamed(t, first, "sequence")

	// No sequence at all: an invalid context cookie, because the session
	// is named but the pairing is not.
	req := mapiPost("emsmdb", "Execute", shortROPBody())
	req.AddCookie(sid)
	rr := serve(t, tr, req)
	wantCode(t, rr, respInvalidContextCookie)

	// The right shape, the wrong value: an invalid sequence.
	req = mapiPost("emsmdb", "Execute", shortROPBody())
	req.AddCookie(sid)
	req.AddCookie(&http.Cookie{Name: "sequence", Value: newTestSequence(t)})
	rr = serve(t, tr, req)
	wantCode(t, rr, respInvalidSequence)

	// The value the last response issued: allowed, and rotated.
	req = mapiPost("emsmdb", "Execute", shortROPBody())
	req.AddCookie(sid)
	req.AddCookie(seq)
	rr = serve(t, tr, req)
	wantCode(t, rr, respSuccess)
	next := cookieNamed(t, rr, "sequence")
	if next.Value == seq.Value {
		t.Errorf("the sequence was not rotated: still %q", next.Value)
	}

	// The sequence a refused request would have consumed still works, so
	// a refused request did not move the session on.
	req = mapiPost("emsmdb", "Execute", shortROPBody())
	req.AddCookie(sid)
	req.AddCookie(next)
	rr = serve(t, tr, req)
	wantCode(t, rr, respSuccess)
}

// A ROP list shorter than its own header parses as a request and cannot be
// run as one. The failure comes back in the body's own result field with a
// success transport status, because the transport did its part: it is the
// operation that could not be carried out.
func TestShortROPListFailsInTheBodyNotInTheTransport(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	connect := serve(t, tr, mapiPost("emsmdb", "Connect", connectBody(testDN("alice"))))
	sid, seq := cookieNamed(t, connect, "sid"), cookieNamed(t, connect, "sequence")

	req := mapiPost("emsmdb", "Execute", shortROPBody())
	req.AddCookie(sid)
	req.AddCookie(seq)
	rr := serve(t, tr, req)

	wantCode(t, rr, respSuccess)
	body := pdu(t, rr)
	if len(body) < 16 {
		t.Fatalf("the Execute body is %d bytes, want at least 16: %x", len(body), body)
	}
	if status := binary.LittleEndian.Uint32(body[0:]); status != 0 {
		t.Errorf("StatusCode = %#x, want 0: the transport accepted the request", status)
	}
	if result := binary.LittleEndian.Uint32(body[4:]); result != ecRpcFailed {
		t.Errorf("ErrorCode = %#x, want ecRpcFailed (%#x)", result, ecRpcFailed)
	}
	if flags := binary.LittleEndian.Uint32(body[8:]); flags != 0 {
		t.Errorf("Flags = %#x, want 0: the field is reserved", flags)
	}
	if n := binary.LittleEndian.Uint32(body[12:]); n != 0 {
		t.Errorf("RopBufferSize = %d, want 0 for an answer that could not be built", n)
	}
}

// A ROP list over the 256 KiB a single call may carry is refused as a failed
// call, with the status twice, which is the shape a failure that never ran
// anything is answered in.
func TestOversizedROPListIsRefusedAsAFailedCall(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	connect := serve(t, tr, mapiPost("emsmdb", "Connect", connectBody(testDN("alice"))))
	sid, seq := cookieNamed(t, connect, "sid"), cookieNamed(t, connect, "sequence")

	req := mapiPost("emsmdb", "Execute", longROPBody(ropBufferMaxIn+1))
	req.AddCookie(sid)
	req.AddCookie(seq)
	rr := serve(t, tr, req)

	wantCode(t, rr, respSuccess)
	body := rr.Body.Bytes()
	cl, err := strconv.Atoi(rr.Header().Get("Content-Length"))
	if err != nil || cl != len(body) {
		t.Fatalf("Content-Length = %q but %d bytes were written",
			rr.Header().Get("Content-Length"), len(body))
	}
	if len(body) < 8 {
		t.Fatalf("the response is %d bytes, want the meta-tags plus 8", len(body))
	}
	lo := binary.LittleEndian.Uint32(body[len(body)-8:])
	hi := binary.LittleEndian.Uint32(body[len(body)-4:])
	if lo != rpcXBadStubData || hi != rpcXBadStubData {
		t.Errorf("failure status = (%#x, %#x), want (%#x, %#x)",
			lo, hi, rpcXBadStubData, rpcXBadStubData)
	}
}

// The UserDn a Connect carries is the whole of this endpoint's idea of which
// mailbox is being opened, so a DN naming somebody else is refused before
// the context that would open it exists.
func TestConnectRefusesADnThatIsNotTheCallers(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	for _, tc := range []struct {
		name string
		dn   string
		want uint32
	}{
		{
			name: "another account",
			dn:   "/o=linchange/ou=Exchange Administrative Group/cn=Recipients/cn=bob",
			want: ecAccessDenied,
		},
		{
			name: "no RDN to read",
			dn:   "/o=linchange/ou=Exchange Administrative Group",
			want: ecRpcFailed,
		},
		{
			name: "empty",
			dn:   "",
			want: ecAccessDenied,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := serve(t, tr, mapiPost("emsmdb", "Connect", connectBody(tc.dn)))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			body := pdu(t, rr)
			if got := binary.LittleEndian.Uint32(body[4:]); got != tc.want {
				t.Errorf("ErrorCode = %#x, want %#x", got, tc.want)
			}
			if got := binary.LittleEndian.Uint32(body[8:]); got != 0 {
				t.Errorf("PollsMax = %d, want 0: no session was opened", got)
			}
			if _, ok := cookieLookedUp(t, tr, endpointEMSMDB); ok {
				t.Error("a refused Connect left a session behind")
			}
		})
	}
}

// The DN a client sends came from this server's own Autodiscover, which names
// the account by the local part of its address. Any other RDN that does not
// name the caller is refused.
func TestConnectAcceptsTheDNFormsThisServerIssues(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	rr := serve(t, tr, mapiPost("emsmdb", "Connect", connectBody(testDN("alice"))))
	body := pdu(t, rr)
	if got := binary.LittleEndian.Uint32(body[4:]); got != 0 {
		t.Errorf("Connect ErrorCode = %#x, want success", got)
	}

	// A bare address is not a distinguished name: it has no RDN to read
	// out of, so it names no mailbox this server could open. The
	// reference's own conversion demands the same shape.
	rr = serve(t, tr, mapiPost("emsmdb", "Connect", connectBody("alice@example.test")))
	body = pdu(t, rr)
	if got := binary.LittleEndian.Uint32(body[4:]); got != ecRpcFailed {
		t.Errorf("Connect with a bare address = %#x, want %#x", got, ecRpcFailed)
	}
}

// The context belongs to the account that established it. The cookie is only
// as private as the password that produced it, so presenting it under
// somebody else's credentials is not presenting it at all.
func TestSessionIsBoundToTheAccountThatEstablishedIt(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	connect := serve(t, tr, mapiPost("emsmdb", "Connect", connectBody(testDN("alice"))))
	sid, seq := cookieNamed(t, connect, "sid"), cookieNamed(t, connect, "sequence")

	req := mapiPost("emsmdb", "Execute", shortROPBody())
	req.AddCookie(sid)
	req.AddCookie(seq)
	req.SetBasicAuth("bob@example.test", "correct-horse")
	rr := serve(t, tr, req)

	wantCode(t, rr, respContextNotFound)
}

// Each endpoint has its own session table. A sid minted by one names nothing
// at the other, which is what a cookie scoped to one path arriving at another
// should mean.
func TestASessionFromOneEndpointIsUnknownAtTheOther(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	connect := serve(t, tr, mapiPost("emsmdb", "Connect", connectBody(testDN("alice"))))
	sid, seq := cookieNamed(t, connect, "sid"), cookieNamed(t, connect, "sequence")

	req := mapiPost("nspi", "GetProps", "")
	req.AddCookie(sid)
	req.AddCookie(seq)
	rr := serve(t, tr, req)

	wantCode(t, rr, respContextNotFound)
}

// An unknown request type is refused once the cookies say the request is
// real — a request nobody has heard of still has to belong to a session.
func TestUnknownRequestTypeIsRefusedOnItsEndpoint(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	connect := serve(t, tr, mapiPost("emsmdb", "Connect", connectBody(testDN("alice"))))
	sid, seq := cookieNamed(t, connect, "sid"), cookieNamed(t, connect, "sequence")

	req := mapiPost("emsmdb", "Frobnicate", "")
	req.AddCookie(sid)
	req.AddCookie(seq)
	rr := serve(t, tr, req)

	wantCode(t, rr, respInvalidRequestType)
}

// The request target is /mapi/<endpoint>/, with or without the trailing
// slash, and nothing under it. The two spellings are the same request: one
// client writes the path the reference requires and another drops the slash.
func TestBothSpellingsOfThePathAreServed(t *testing.T) {
	tr := newTestTransport(&fixedAuthenticator{})

	for _, path := range []string{
		"/mapi/emsmdb/?MailboxId=alice",
		"/mapi/emsmdb?MailboxId=alice",
		"/mapi/nspi/?MailboxId=alice",
		"/mapi/nspi?MailboxId=alice",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(connectBody(testDN("alice"))))
		req.Header.Set("X-RequestType", "Connect")
		req.Header.Set("X-RequestId", "00000001")
		req.Header.Set("X-ClientInfo", "linchange-test")
		req.SetBasicAuth("alice@example.test", "correct-horse")
		rr := serve(t, tr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("POST %s = %d, want 200; body: %s", path, rr.Code, rr.Body.String())
		}
	}

	// A path under the endpoint is not the endpoint.
	req := httptest.NewRequest(http.MethodPost, "/mapi/emsmdb/extra", strings.NewReader(""))
	req.SetBasicAuth("alice@example.test", "correct-horse")
	if rr := serve(t, tr, req); rr.Code != http.StatusNotFound {
		t.Errorf("POST /mapi/emsmdb/extra = %d, want 404", rr.Code)
	}
	// And the endpoint answers POST only.
	req = httptest.NewRequest(http.MethodGet, "/mapi/emsmdb/", strings.NewReader(""))
	req.SetBasicAuth("alice@example.test", "correct-horse")
	if rr := serve(t, tr, req); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mapi/emsmdb/ = %d, want 405", rr.Code)
	}
}

// The address book endpoint holds a session of its own, established by Bind
// and released by Unbind. After Unbind the sid names nothing, which is what
// releasing a context has to mean: a cookie that still works is a context
// that was never released.
func TestAddressBookSessionIsEstablishedByBindAndReleasedByUnbind(t *testing.T) {
	db := testdb.Migrated(t)
	tr := New(Options{
		DB:       db,
		Users:    store.NewUsers(),
		Hostname: "mail.example.test",
		Auth:     &fixedAuthenticator{},
	})

	// Bind: flags, no STAT, no auxiliary buffer.
	bindBody := func() string {
		var b []byte
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = append(b, 0)
		b = binary.LittleEndian.AppendUint32(b, 0)
		return string(b)
	}

	rr := serve(t, tr, mapiPost("nspi", "Bind", bindBody()))
	if rr.Code != http.StatusOK {
		t.Fatalf("Bind = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	body := pdu(t, rr)
	if len(body) != 28 {
		t.Fatalf("the Bind body is %d bytes, want 28: %x", len(body), body)
	}
	if got := binary.LittleEndian.Uint32(body[4:]); got != 0 {
		t.Fatalf("Bind ErrorCode = %#x, want success", got)
	}
	if bytes.Equal(body[8:24], make([]byte, 16)) {
		t.Error("a successful Bind answered with a zeroed ServerGuid")
	}
	sid, seq := cookieNamed(t, rr, "sid"), cookieNamed(t, rr, "sequence")

	// A PING with those cookies is answered with the meta-tags alone.
	ping := mapiPost("nspi", "PING", "")
	ping.AddCookie(sid)
	ping.AddCookie(seq)
	pr := serve(t, tr, ping)
	wantCode(t, pr, respSuccess)
	if len(pdu(t, pr)) != 0 {
		t.Error("a PING carries a body")
	}

	// Another account's credentials are not this session's.
	unbind := mapiPost("nspi", "Unbind", unbindBody())
	unbind.AddCookie(sid)
	unbind.AddCookie(seq)
	unbind.SetBasicAuth("bob@example.test", "correct-horse")
	if rr := serve(t, tr, unbind); rr.Header().Get("X-ResponseCode") != "10" {
		t.Errorf("another account's Unbind was answered with %q, want 10",
			rr.Header().Get("X-ResponseCode"))
	}

	// Our own Unbind releases it...
	unbind = mapiPost("nspi", "Unbind", unbindBody())
	unbind.AddCookie(sid)
	unbind.AddCookie(seq)
	if rr := serve(t, tr, unbind); rr.Code != http.StatusOK {
		t.Fatalf("Unbind = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	// ...and the sid that named it no longer names anything.
	next := mapiPost("nspi", "GetProps", getPropsBody())
	next.AddCookie(sid)
	next.AddCookie(seq)
	if rr := serve(t, tr, next); rr.Header().Get("X-ResponseCode") != "10" {
		t.Errorf("a released session was answered with %q, want 10",
			rr.Header().Get("X-ResponseCode"))
	}
}

// unbindBody is a reserved dword and an empty auxiliary buffer.
func unbindBody() string {
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 0)
	return string(b)
}

// getPropsBody is flags, no STAT, no property list, no auxiliary buffer.
func getPropsBody() string {
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, 0, 0)
	b = binary.LittleEndian.AppendUint32(b, 0)
	return string(b)
}

// wstrIn reads a NUL-terminated UTF-16LE string. Nothing in the protocol
// reads one — only the Connect and Bind responses carry them — so the reader
// has no method for it and the test decodes it from the bytes the reader has
// left.
func wstrIn(t *testing.T, r *reader) string {
	t.Helper()
	var runes []rune
	for {
		if r.remaining() < 2 {
			t.Fatalf("the display name ran out of the body: %q", runes)
		}
		v := binary.LittleEndian.Uint16(r.buf[r.off:])
		r.off += 2
		if v == 0 {
			return string(runes)
		}
		runes = append(runes, rune(v))
	}
}

// nextU32 reads one little-endian field and names it, so a test can walk a
// response body the way the specification lists it.
func nextU32(t *testing.T, r *reader) uint32 {
	t.Helper()
	v, err := r.u32()
	if err != nil {
		t.Fatalf("reading a u32 field: %v", err)
	}
	return v
}

// cookieLookedUp reports whether the endpoint's table holds any session, so a
// test can assert that a refused request created none.
func cookieLookedUp(t *testing.T, tr *Transport, endpoint string) (string, bool) {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for sid := range tr.sessionsFor(endpoint) {
		return sid, true
	}
	return "", false
}

func newTestSequence(t *testing.T) string {
	t.Helper()
	seq, err := newSequence()
	if err != nil {
		t.Fatalf("minting a sequence value: %v", err)
	}
	return seq
}

func readAll(resp *http.Response) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func head(b []byte, n int) string {
	if len(b) < n {
		return string(b)
	}
	return string(b[:n])
}

func tail(b []byte, n int) string {
	if len(b) < n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

package transport

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// The response codes of MS-OXCMAPIHTTP 2.2.3.3.3. An X-ResponseCode of 0
// means the transport accepted the request and the client should parse the
// body for the operation's own result; anything else means the body is HTML
// describing what went wrong instead.
const (
	respSuccess              = 0
	respUnknownFailure       = 1
	respInvalidVerb          = 2
	respInvalidRequestType   = 5
	respInvalidContextCookie = 6
	respMissingHeader        = 7
	respTooLarge             = 9
	respContextNotFound      = 10
	respInvalidRequestBody   = 12
	respMissingCookie        = 13
	respInvalidSequence      = 15
)

// respCodeText is the diagnostic paragraph an X-ResponseCode is answered
// with. The wording is the reference's wherever the reference sends that
// code; where it does not send one at all (9), it is the specification's own
// description. Code 1 is the one place the wording is the specification's
// rather than the reference's: the reference labels it "Out of memory."
// because it only sends it when a cookie header cannot be parsed, while this
// server sends 1 for any failure it cannot diagnose, which is what
// MS-OXCMAPIHTTP 2.2.3.3.3 calls an unknown failure.
func respCodeText(code int) string {
	switch code {
	case respSuccess:
		return "The request was properly formatted and accepted."
	case respUnknownFailure:
		return "The request produced an unknown failure."
	case respInvalidVerb:
		return "The request has an invalid verb."
	case respInvalidRequestType:
		return "Invalid request type for this endpoint."
	case respInvalidContextCookie:
		return "The request has an invalid session context cookie."
	case respMissingHeader:
		return "The request has a missing required header."
	case respTooLarge:
		return "The request is too large."
	case respContextNotFound:
		return "The Session Context was not found."
	case 11:
		return "The client has no privileges to the Session Context."
	case respInvalidRequestBody:
		return "The request body is invalid."
	case respMissingCookie:
		return "The request is missing a required cookie."
	case respInvalidSequence:
		return "The request has violated the sequencing requirement of one request at a time per Session Context."
	}
	return "Unknown failure."
}

const (
	// defaultServerVersion is what X-ServerApplication reports when the
	// deployment names no version. It is the reference's own default, and
	// it is a dotted Exchange version because that is the shape
	// MS-OXCMAPIHTTP 2.2.3.3.7 specifies and a client parses.
	defaultServerVersion = "15.00.0847.4040"
	// defaultMaxBodyBytes bounds a request body when the deployment names
	// no bound: ten megabytes, which is generous for a ROP list and small
	// enough that one request cannot fill memory.
	defaultMaxBodyBytes = 10 << 20

	// pendingPeriodMs and expirationInfoMs are the X-PendingPeriod and
	// X-ExpirationInfo header values, in milliseconds. The first tells a
	// client how often to expect a keep-alive if a request were to run
	// long; the second is how long the session will survive inactivity,
	// which is also how long the server gives a session before reaping it.
	pendingPeriodMs  = 30000
	expirationInfoMs = 900000
)

// trackedWriter records whether anything has been written, so that a panic
// can still be answered with a 500 when nothing has gone out on the wire and
// left alone when something has — appending a second response to a body
// already in flight would be worse than the panic.
type trackedWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *trackedWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *trackedWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

// Flush makes the writer a Flusher, which is how the meta-tags reach the
// client before the body that follows them is known.
func (w *trackedWriter) Flush() {
	w.wrote = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// setCommonHeaders writes the header block every 200 response starts with
// (MS-OXCMAPIHTTP 2.2.2.2).
//
// The Date header is left to net/http, which writes it in the same RFC1123
// form the reference formats it in — so setting it here would only risk
// sending two.
//
// requestType is the value echoed in X-RequestType. It is a parameter rather
// than a field because a PING answers with "PING" whatever the client spelled
// the header as.
//
// withSequence is false only for a PING, which is the one response the
// reference builds from the common header alone: a PING acknowledges that a
// session is alive and changes nothing in it, so it has no new sequence to
// hand back.
func (q *request) setCommonHeaders(requestType string, withSequence bool) {
	h := q.w.Header()
	h.Set("Cache-Control", "private")
	h.Set("Content-Type", "application/mapi-http")
	h.Set("X-RequestType", requestType)
	h.Set("X-RequestId", q.requestID)
	h.Set("X-ClientInfo", q.clientInfo)
	h.Set("X-ResponseCode", "0")
	h.Set("X-PendingPeriod", strconv.Itoa(pendingPeriodMs))
	h.Set("X-ExpirationInfo", strconv.Itoa(expirationInfoMs))
	h.Set("X-ServerApplication", "Exchange/"+q.t.opts.ServerVersion)
	if q.sid != "" {
		h.Add("Set-Cookie", "sid="+q.sid)
	}
	if withSequence && q.sequence != "" {
		h.Add("Set-Cookie", "sequence="+q.sequence)
	}
}

// renderContent builds the response meta-tags (MS-OXCMAPIHTTP 2.2.7): the
// request was queued and has finished, how long it took, and when it started.
//
// Elapsed time is in whole seconds, as the reference counts it — the clock
// this protocol runs on is a client's patience, not a stopwatch.
func (q *request) renderContent() string {
	elapsed := int(time.Since(q.start).Seconds())
	return fmt.Sprintf("PROCESSING\r\nDONE\r\nX-ElapsedTime: %d\r\nX-StartTime: %s\r\n\r\n",
		elapsed, q.start.UTC().Format(http.TimeFormat))
}

// normalResponse answers a request whose operation ran: the meta-tags first,
// flushed, then the operation's own PDU.
//
// No Content-Length is set and the meta-tags are flushed before the body is
// written, which is what makes net/http answer with Transfer-Encoding:
// chunked. The terminating 0-chunk is its to write: putting our own in the
// body would leave seven bytes of framing after the PDU for a client to read
// as part of the response.
func (q *request) normalResponse(pdu []byte) {
	q.setCommonHeaders(q.requestType, true)
	if _, err := io.WriteString(q.w, q.renderContent()); err != nil {
		q.t.log.InfoContext(q.r.Context(), "mapi/http: could not write the meta-tags",
			"endpoint", q.endpoint, "error", err)
		return
	}
	q.w.Flush()
	if _, err := q.w.Write(pdu); err != nil {
		q.t.log.InfoContext(q.r.Context(), "mapi/http: could not write the response body",
			"endpoint", q.endpoint, "error", err)
	}
}

// pingResponse answers a PING: the meta-tags alone, with a Content-Length
// because the whole answer is known at once (MS-OXCMAPIHTTP 2.2.6).
func (q *request) pingResponse() {
	ct := q.renderContent()
	q.setCommonHeaders("PING", false)
	q.w.Header().Set("Content-Length", strconv.Itoa(len(ct)))
	q.w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(q.w, ct)
}

// failureResponse answers with the meta-tags followed by the same u32 status
// twice, which is the reference's shape for a failure it detected while
// building a response rather than while parsing a request — a session it
// could not allocate, a PDU it could not serialise. It is a complete,
// Content-Length'd response, so a client reads it as one.
func (q *request) failureResponse(status uint32) {
	ct := q.renderContent()
	q.setCommonHeaders(q.requestType, true)
	q.w.Header().Set("Content-Length", strconv.Itoa(len(ct)+8))
	q.w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(q.w, ct)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], status)
	_, _ = q.w.Write(b[:])
	_, _ = q.w.Write(b[:])
}

// errorResponse answers with an X-ResponseCode and an HTML diagnostic
// instead of an operation's body (MS-OXCMAPIHTTP 2.2.3.3.3). It always
// reports 200: everything short of an authentication failure or an
// exceptional condition is a 200 with a code, because a client that sees a
// 500 retries and a client that sees a code gives up.
//
// It returns true so that a caller can end with `return q.errorResponse(...)`.
func (q *request) errorResponse(code int) bool {
	body := "<!DOCTYPE HTML PUBLIC \"-//IETF//DTD HTML 2.0//EN\">\r\n" +
		"<html><head>\r\n<title>MAPI OVER HTTP ERROR</title>\r\n</head><body>\r\n" +
		"<h1>Diagnostic Information</h1>\r\n<p>" + respCodeText(code) + "</p>\r\n" +
		"</body></html>\r\n"
	h := q.w.Header()
	h.Set("Cache-Control", "private")
	h.Set("Content-Type", "text/html")
	h.Set("X-ResponseCode", strconv.Itoa(code))
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("X-ServerApplication", "Exchange/"+q.t.opts.ServerVersion)
	q.w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(q.w, body)
	return true
}

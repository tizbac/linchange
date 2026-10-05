package caldav

import (
	"bytes"
	"errors"
	"net/http"
	"unicode/utf8"
)

// davError is a DAV failure: an HTTP status, and the precondition or
// postcondition element that says which rule was broken.
//
// The status alone is not enough for a DAV client. RFC 4918 §16 defines a set of
// condition elements precisely so a client can tell "you may not write here" from
// "what you sent conflicts with what is stored", which have the same status but
// opposite recoveries: one is retried after a permission change, the other after
// a merge. A server that answers 403 with no body forces the client to guess, and
// what it guesses is a resend loop.
type davError struct {
	Status int
	// Code is a DAV: condition element name such as "need-privileges", or a
	// precondition token such as "If-Match". Empty means there is no condition,
	// which is right for a plain 404.
	Code string
	// Detail is a human-readable explanation. It goes in
	// DAV:responsedescription, which is for logging and for the user; a client
	// is not meant to parse it, so it is safe to change.
	Detail string
}

func (e *davError) Error() string {
	if e.Detail == "" {
		return "caldav: " + http.StatusText(e.Status)
	}
	return "caldav: " + e.Detail
}

// write emits the error as a DAV:error document.
//
// A GET or PUT failure is answered in the body rather than by the status line
// alone, because a DAV client on a failed write has to decide whether to re-read
// the resource and merge, and the condition element is what tells it.
func (e *davError) write(w http.ResponseWriter) {
	body := el(nsDAV, "error")
	if e.Code != "" {
		body.kids = append(body.kids, el(nsDAV, e.Code))
	}
	if e.Detail != "" {
		body.kids = append(body.kids, leaf(nsDAV, "responsedescription", e.Detail))
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(e.Status)
	_, _ = w.Write(render(body))
}

// The property-level failures, as sentinel errors so the set/validate path in
// properties.go can return one and the handler can turn it into a propstat.
//
// They are sentinels rather than davError values because a propstat carries a
// status line in its own right: a PROPPATCH that sets three properties and fails
// one does not fail the request, it reports 200 for the two and 403 for the one
// inside the same 207 response. Raising an error to the top level would lose that.
var (
	// errProtectedProperty is a live property. RFC 4918 §9.2 makes writing one
	// forbidden, and a client that tries is a client whose model of the server
	// is wrong.
	errProtectedProperty = &davError{
		Status: http.StatusForbidden,
		Code:   "DAV:cannot-modify-protected-property",
		Detail: "this property is defined by the server and cannot be set",
	}
	// errBadPropertyValue is a value the property will not accept.
	errBadPropertyValue = &davError{
		Status: http.StatusConflict,
		Code:   "DAV:valid-resourcetype",
		Detail: "the value given for this property is not one it accepts",
	}
)

// AsDAVError extracts a davError from an error, or wraps one.
//
// The sentinel errors above are compared with errors.Is, and a value returned by
// set or validate may be either one of them or a fresh davError carrying a
// specific message. Anything else is a bug rather than a refusal, and is reported
// as one.
func asDAVError(err error) *davError {
	if err == nil {
		return nil
	}
	var de *davError
	if errors.As(err, &de) {
		return de
	}
	return &davError{Status: http.StatusInternalServerError, Detail: err.Error()}
}

// isUTF8 reports whether a string is valid UTF-8.
//
// utf8.ValidString is the check, called out separately because every caller here
// is asking it as a precondition on storing user-supplied text rather than as a
// general-purpose utility.
func isUTF8(s string) bool { return utf8.ValidString(s) }

// ---------------------------------------------------------------------------
// multistatus
// ---------------------------------------------------------------------------

// response is one DAV:response: a resource and the outcome for each property
// asked about.
type response struct {
	href string
	// found and missing split the requested properties by whether the server has
	// them. The split is not cosmetic: RFC 4918 §9.1 requires a property that
	// does not exist on a resource to be reported 404, distinct from the 200
	// for the ones that do.
	found   []*node
	missing []*node
	// status is set when the response reports one status for the whole
	// resource, which is what a failed precondition looks like: there is no
	// propstat because nothing was evaluated.
	status *davError
}

// multiStatus is a DAV:multistatus document.
type multiStatus struct {
	responses []response
	// syncToken is emitted as a child of the multistatus, after the responses,
	// which is where RFC 6578 §3.2 puts it and the only place a client looks for
	// it. The report is about a collection, so the token belongs to the collection
	// rather than to any one resource in it; attaching it to whichever resource
	// happened to be reported first would be a coincidence a client cannot rely on.
	syncToken string
}

// add appends a response for a resource with no properties.
func (m *multiStatus) add(href string) *response {
	m.responses = append(m.responses, response{href: hrefOf(href)})
	return &m.responses[len(m.responses)-1]
}

// write emits the document as a 207.
//
// 207 is the status for a multistatus and it is not optional: a client that
// expects 207 and receives 200 treats the body as something else entirely and
// usually shows an empty result rather than an error.
func (m *multiStatus) write(w http.ResponseWriter) {
	root := el(nsDAV, "multistatus")
	for _, resp := range m.responses {
		root.kids = append(root.kids, m.renderResponse(resp))
	}
	if m.syncToken != "" {
		root.kids = append(root.kids, leaf(nsDAV, "sync-token", m.syncToken))
	}
	body := render(root)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write(body)
}

// renderResponse builds one DAV:response.
//
// The short form -- href and status, no propstat -- is used when the whole
// resource shares one status, and the propstat form when properties came back
// with more than one status between them. Both are permitted by RFC 4918 §13 and
// clients handle both; using the short form where it applies keeps the common
// case small, and a full PROPFIND of a calendar home set is a large document.
func (m *multiStatus) renderResponse(resp response) *node {
	out := el(nsDAV, "response", href(resp.href))

	if resp.status != nil {
		out.kids = append(out.kids, leaf(nsDAV, "status", statusLine(resp.status.Status)))
		// The condition element names the rule that was broken, and it is only
		// written where one exists: a resource reported as gone by a sync has no
		// rule behind it, and RFC 6578 §3.6 shows that response carrying a status
		// and nothing else.
		if resp.status.Code != "" {
			out.kids = append(out.kids, el(nsDAV, "error", el(nsDAV, resp.status.Code)))
		}
		return out
	}

	// Both buckets present, or neither: one propstat.
	if (len(resp.found) == 0) == (len(resp.missing) == 0) {
		out.kids = append(out.kids,
			el(nsDAV, "propstat",
				el(nsDAV, "prop", resp.found...),
				leaf(nsDAV, "status", statusLine(http.StatusOK))),
			el(nsDAV, "propstat",
				el(nsDAV, "prop", resp.missing...),
				leaf(nsDAV, "status", statusLine(http.StatusNotFound))),
		)
		return out
	}
	if len(resp.found) > 0 {
		out.kids = append(out.kids,
			el(nsDAV, "propstat",
				el(nsDAV, "prop", resp.found...),
				leaf(nsDAV, "status", statusLine(http.StatusOK))))
	}
	if len(resp.missing) > 0 {
		out.kids = append(out.kids,
			el(nsDAV, "propstat",
				el(nsDAV, "prop", resp.missing...),
				leaf(nsDAV, "status", statusLine(http.StatusNotFound))))
	}
	return out
}

// statusLine is an HTTP status line as DAV writes it inside a document.
//
// DAV puts status lines in a document that is itself carried by an HTTP response,
// so the line repeats the response's own semantics at the inner level. It is
// spelled with the version because that is what RFC 4918 §13.4.2 shows and what
// clients compare against.
func statusLine(code int) string {
	return "HTTP/1.1 " + itoa(code) + " " + http.StatusText(code)
}

// itoa is strconv.Itoa for a non-negative int, kept local so the status-line
// helper does not carry a conversion for values it never sees negative.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// writeText answers with a plain-text body, for the failures a DAV client
// surfaces verbatim.
//
// A DAV error document is for a DAV client. An oversized body or a malformed
// request is something a person will end up looking at while reporting a bug, and
// a DAV:error document rendered in a browser or a log is not readable.
func writeText(w http.ResponseWriter, code int, msg string) {
	var buf bytes.Buffer
	buf.WriteString(msg)
	buf.WriteByte('\n')
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

// fail writes a davError, and reports anything that is not one as a server
// fault rather than as a refusal.
func fail(w http.ResponseWriter, err error) {
	asDAVError(err).write(w)
}

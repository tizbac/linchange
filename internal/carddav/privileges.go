package carddav

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// correctPrivileges fixes the current-user-privilege-set that go-webdav reports
// for address books.
//
// The library writes read+write for every collection (its own TODO). A CardDAV
// client uses that property to decide whether a book may be changed, so without
// this a read-only share looks writable until its first PUT or DELETE is
// refused. The server already knows the access level; this wrapper rewrites the
// property in the PROPFIND response to match it.
func (s *Server) correctPrivileges(next http.Handler) http.Handler {
	return &privilegeHandler{next: next, server: s}
}

type privilegeHandler struct {
	next   http.Handler
	server *Server
}

func (h *privilegeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PROPFIND" {
		h.next.ServeHTTP(w, r)
		return
	}
	who, ok := r.Context().Value(principalKey{}).(policy.Principal)
	if !ok || who.Address == "" {
		h.next.ServeHTTP(w, r)
		return
	}
	address := strings.ToLower(strings.TrimSpace(who.Address))
	books, err := h.server.contacts.Books(r.Context(), address)
	if err != nil {
		h.next.ServeHTTP(w, r)
		return
	}
	readOnly := make(map[string]bool)
	for _, book := range books {
		if book.Level == store.AddressBookShareWrite {
			continue
		}
		readOnly[normalizeHref(bookPath(h.server.prefix, address, book))] = true
	}
	if len(readOnly) == 0 {
		h.next.ServeHTTP(w, r)
		return
	}

	captured := &responseCapture{header: make(http.Header)}
	h.next.ServeHTTP(captured, r)
	status := captured.status
	if status == 0 {
		status = http.StatusOK
	}
	h.copyHeaders(w.Header(), captured.header)
	if status != http.StatusMultiStatus {
		h.finish(w, status, captured.body.Bytes())
		return
	}
	body, changed := rewritePrivileges(captured.body.Bytes(), readOnly)
	if !changed {
		body = captured.body.Bytes()
	}
	h.finish(w, status, body)
}

func (h *privilegeHandler) copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	// The corrected body is a different length.
	dst.Del("Content-Length")
}

func (h *privilegeHandler) finish(w http.ResponseWriter, status int, body []byte) {
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

type responseCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *responseCapture) Header() http.Header { return c.header }

func (c *responseCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *responseCapture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p)
}

// rewritePrivileges removes the write privilege from every response whose href
// is a read-only book. Other responses, including the home and writable books,
// are left as the library wrote them.
//
// The response is cut out of the original bytes rather than decoded and encoded
// again: Go's XML encoder would add a namespace declaration for every element
// on top of the ones already in the document, which is not valid XML.
func rewritePrivileges(body []byte, readOnly map[string]bool) ([]byte, bool) {
	type removal struct{ start, end int64 }
	var removals []removal
	decoder := xml.NewDecoder(bytes.NewReader(body))
	inResponse := false
	readOnlyResponse := false
	hrefSeen := false
	capturingHref := false
	var href strings.Builder
	privilegeDepth := 0
	writeStart := int64(0)
	writeDepth := 0
	for {
		start := decoder.InputOffset()
		token, err := decoder.RawToken()
		end := decoder.InputOffset()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}
		switch t := token.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if writeDepth > 0 {
				writeDepth++
				continue
			}
			if inResponse && privilegeDepth > 0 && readOnlyResponse && name == "write" {
				writeStart = start
				writeDepth = 1
				continue
			}
			switch name {
			case "response":
				inResponse = true
				readOnlyResponse = false
				hrefSeen = false
				capturingHref = false
				privilegeDepth = 0
				href.Reset()
			case "href":
				if inResponse && !hrefSeen {
					hrefSeen = true
					capturingHref = true
				}
			case "current-user-privilege-set":
				if inResponse {
					privilegeDepth++
				}
			}
		case xml.EndElement:
			name := t.Name.Local
			if writeDepth > 0 {
				writeDepth--
				if writeDepth == 0 {
					removals = append(removals, removal{start: writeStart, end: end})
				}
				continue
			}
			if capturingHref && name == "href" {
				capturingHref = false
				readOnlyResponse = readOnly[normalizeHref(href.String())]
				href.Reset()
			}
			if inResponse {
				switch name {
				case "current-user-privilege-set":
					if privilegeDepth > 0 {
						privilegeDepth--
					}
				case "response":
					inResponse = false
				}
			}
		case xml.CharData:
			if capturingHref {
				href.Write(t)
			}
		}
	}
	if len(removals) == 0 {
		return body, false
	}
	var out bytes.Buffer
	at := int64(0)
	for _, cut := range removals {
		if cut.start < at || cut.start >= cut.end || cut.end > int64(len(body)) {
			continue
		}
		out.Write(body[at:cut.start])
		at = cut.end
	}
	out.Write(body[at:])
	return out.Bytes(), true
}

// normalizeHref makes the path in a DAV href comparable with the path this
// server knows a book by: an absolute URL loses its scheme and host, escapes are
// decoded, and the trailing slash that both forms may carry is dropped.
func normalizeHref(href string) string {
	href = strings.TrimSpace(href)
	if parsed, err := url.Parse(href); err == nil && parsed.Path != "" {
		href = parsed.Path
	}
	if unescaped, err := url.PathUnescape(href); err == nil {
		href = unescaped
	}
	return strings.TrimSuffix(href, "/")
}

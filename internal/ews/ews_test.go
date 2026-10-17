package ews

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// An EWS service without a verifier refuses every request: credentials are the
// only thing that says whose mail an operation may touch, and a server that
// serves without them is a server that serves one person's mail to another.
// The tests below use a fixed authenticator that accepts one principal, which is
// enough for the transport questions they are asking -- decoding, dispatch,
// escaping -- and the authentication contract itself is tested against an
// authenticator that refuses.

// fixedAuthenticator accepts any credentials and answers with one principal.
type fixedAuthenticator struct{ who policy.Principal }

func (f fixedAuthenticator) Authenticate(ctx context.Context, login, pw string, action policy.Action) (policy.Principal, error) {
	if f.who.Address == "" {
		return policy.Principal{}, errors.New("no such account")
	}
	return f.who, nil
}

// refusingAuthenticator rejects every login, as a verifier would for a wrong
// password.
type refusingAuthenticator struct{}

func (refusingAuthenticator) Authenticate(ctx context.Context, login, pw string, action policy.Action) (policy.Principal, error) {
	return policy.Principal{}, errors.New("rejected")
}

func newTestServer() *EWS {
	return New(Options{
		Config: config.New(),
		Policy: policy.Policy{},
		Auth: fixedAuthenticator{who: policy.Principal{
			Address: "alice@example.test", MailboxEnabled: true,
		}},
	})
}

// soapPost builds a POST carrying one operation, with credentials the fixed
// authenticator accepts.
func soapPost(operation string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/exchange.asmx",
		strings.NewReader(soapRequest(operation)))
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.SetBasicAuth("alice@example.test", "correct-horse")
	return req
}

// The fault body quotes back the operation name, which the caller supplies in
// the SOAPAction header. Reflecting that verbatim puts the caller's markup
// inside the response, so the escaping has to be real.
func TestUnsupportedOperationIsEscapedInTheFault(t *testing.T) {
	e := newTestServer()

	req := soapPost("FindItem")
	// The operation is the last segment of the SOAPAction URI, so this names an
	// operation called `Op<injected>1`.
	req.Header.Set("SOAPAction", `http://schemas.microsoft.com/exchange/services/2006/messages/Op<injected>1`)
	rr := httptest.NewRecorder()

	e.Handler().ServeHTTP(rr, req)

	body := rr.Body.String()
	if strings.Contains(body, "<injected>") {
		t.Errorf("the caller's markup was reflected into the fault: %s", body)
	}
	if !strings.Contains(body, "Op&lt;injected&gt;1") {
		t.Errorf("the operation name should appear escaped in the fault: %s", body)
	}
	// One envelope open, one close. A second open tag is what an injected close
	// followed by the real close would look like.
	if got := strings.Count(body, "<soap:Envelope"); got != 1 {
		t.Errorf("fault contains %d envelopes, want 1: %s", got, body)
	}
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 for an operation the service does not implement", rr.Code)
	}
}

// A request arrives as a real SOAP envelope, and the operation is named by the
// element in the body. Both have to be understood: the transport used to reject
// every envelope because the struct tag named a namespace that does not exist,
// and defaulted to FindItem when the SOAPAction header was absent.
func TestOperationIsTakenFromTheBodyWhenSOAPActionIsAbsent(t *testing.T) {
	e := newTestServer()

	for _, operation := range []string{"FindItem", "GetItem", "CreateItem", "ResolveNames"} {
		req := soapPost(operation)
		rr := httptest.NewRecorder()

		e.Handler().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200; body: %s", operation, rr.Code, rr.Body.String())
			continue
		}
		if want := operation + "Response"; !strings.Contains(rr.Body.String(), want) {
			t.Errorf("%s did not answer with %s: %s", operation, want, rr.Body.String())
		}
	}
}

// The response has to be a parseable envelope in the SOAP namespace, not a
// document a client rejects on sight.
func TestResponseIsASOAPEnvelope(t *testing.T) {
	e := newTestServer()

	req := soapPost("FindItem")
	rr := httptest.NewRecorder()

	e.Handler().ServeHTTP(rr, req)

	var env SOAPEnvelope
	if err := xml.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("the response is not a SOAP envelope: %v\n%s", err, rr.Body.String())
	}
	if env.Body == nil {
		t.Fatal("the response envelope has no body")
	}
	if got := env.Body.Operation(); got != "FindItemResponse" {
		t.Errorf("body operation = %q, want FindItemResponse", got)
	}
}

// The middleware only lets a SOAP content type through, and it answers anything
// else before the operation layer sees it.
func TestNonSOAPContentTypeIsRejected(t *testing.T) {
	e := newTestServer()

	req := soapPost("FindItem")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	e.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a non-SOAP content type", rr.Code)
	}
}

// The endpoint answers at the path its mount point implies. The server mounts
// this handler at /ews, so the route here must not repeat the prefix.
func TestHandlerAnswersAtTheMountRelativePath(t *testing.T) {
	e := newTestServer()

	req := soapPost("FindItem")
	rr := httptest.NewRecorder()

	e.Handler().ServeHTTP(rr, req)

	if rr.Code == http.StatusNotFound || rr.Code == http.StatusMethodNotAllowed {
		t.Errorf("POST /exchange.asmx = %d; the endpoint must answer at the mount-relative path", rr.Code)
	}
}

// The server mounts this handler at /ews. A route here that repeats the prefix
// answers at /ews/ews/exchange.asmx instead, which is not the path MS-OXECWS
// specifies and not the path Autodiscover reports, so it has to stay unrouted.
func TestHandlerDoesNotAnswerAtTheDoubledPath(t *testing.T) {
	e := newTestServer()

	req := soapPost("FindItem")
	req.URL.Path = "/ews/exchange.asmx"
	rr := httptest.NewRecorder()

	e.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("POST /ews/exchange.asmx = %d, want 404; the mount point supplies the prefix", rr.Code)
	}
}

// The authentication contract: credentials are required, and a login the
// verifier refuses is a 401 with a Basic challenge, never a response that might
// have come from somebody else's mailbox.
func TestAuthIsAlwaysRequired(t *testing.T) {
	// No verifier at all: serving anyway would serve one person's mail to
	// whoever asked.
	eNoAuth := New(Options{Config: config.New(), Policy: policy.Policy{}})
	// A verifier that rejects every login, standing in for a wrong password.
	eRefusing := New(Options{Config: config.New(), Policy: policy.Policy{}, Auth: refusingAuthenticator{}})

	for _, tc := range []struct {
		name string
		svc  *EWS
		auth bool
	}{
		{"nil verifier", eNoAuth, false},
		{"no credentials sent", eRefusing, false},
		{"verifier refuses", eRefusing, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := soapPost("FindItem")
			if !tc.auth {
				req.Header.Del("Authorization")
			}
			rr := httptest.NewRecorder()

			tc.svc.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
				t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
			}
		})
	}
}

// Every operation the dispatch table promises has an entry spelled the way the
// SOAPAction maps it: the same operation named in the header and in the body
// must reach the same handler.
func TestDispatchCoversTheSupportedOperations(t *testing.T) {
	e := newTestServer()

	for operation := range operations {
		req := soapPost(operation)
		req.Header.Set("SOAPAction",
			"http://schemas.microsoft.com/exchange/services/2006/messages/"+operation)
		rr := httptest.NewRecorder()

		e.Handler().ServeHTTP(rr, req)

		// The transport answered at all -- a header-named operation that misses
		// the table would be a 501. Whether the request itself is valid is the
		// operation's own business; the point is the dispatch.
		if rr.Code == http.StatusNotImplemented {
			t.Errorf("%s: the dispatched operation is not recognized", operation)
		}
	}
}

func soapRequest(operation string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
  <s:Body>
    <m:` + operation + ` xmlns:m="http://schemas.microsoft.com/exchange/services/2006/messages"/>
  </s:Body>
</s:Envelope>`
}

// The identity of a calendar item must name a calendar, not a mail folder. The
// kinds share the first byte of the identifier, and a renderer that lost the
// kind would answer a SyncFolderItems on the calendar with an ItemId whose kind
// says mailbox -- an id no GetItem on a calendar folder would ever resolve.
func TestCalendarItemIdentityCarriesTheCalendarKind(t *testing.T) {
	ev := store.Event{ID: 7, CalendarID: 4, UID: "meet", Summary: "Planning", Status: "CONFIRMED"}
	v := &itemView{
		kind:      "CalendarItem",
		container: 4,
		id:        ev.ID,
		event:     &ev,
		class:     itemClassAppointment,
		occurrence: &calendar.View{
			EventID:    ev.ID,
			CalendarID: ev.CalendarID,
			UID:        ev.UID,
			Summary:    ev.Summary,
			Status:     ev.Status,
			Start:      time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC),
			End:        time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
		},
	}

	var b strings.Builder
	renderCalendarItem(&b, v, parseShape(&shapeRequest{Base: "IdOnly"}))

	// Pull the ItemId out of the rendered element and decode it: the first byte
	// is the kind, and a calendar item's must be kindCalendar.
	const prefix = `<t:ItemId Id="`
	open := strings.Index(b.String(), prefix)
	if open < 0 {
		t.Fatalf("no ItemId in the render: %s", b.String())
	}
	rest := b.String()[open+len(prefix):]
	close := strings.Index(rest, `"`)
	if close < 0 {
		t.Fatalf("unterminated ItemId: %s", b.String())
	}
	kind, container, item, err := decodeItemID(rest[:close])
	if err != nil {
		t.Fatalf("decoding the rendered identifier: %v", err)
	}
	if kind != kindCalendar || container != 4 || item != 7 {
		t.Errorf("identifier = kind %d container %d item %d, want %d %d %d",
			kind, container, item, kindCalendar, 4, 7)
	}
}

// IdOnly with additional properties is not identity only: the extras are the
// exception that makes IdOnly usable, and a renderer that treats the base shape
// as "identifiers only" whenever extras are named drops exactly the properties
// the client asked for beyond the identifier.
func TestIDOnlyShapeKeepsItsAdditionalProperties(t *testing.T) {
	sh := parseShape(&shapeRequest{
		Base: "IdOnly",
		Additional: &shapeFields{Fields: []fieldURI{
			{FieldURI: "item:subject"},
			{PropertySet: "message", Property: "IsRead"},
		}},
	})
	if sh.identity() {
		t.Errorf("a shape that names additional properties is not identity-only")
	}
	if !sh.asked("item:subject", defaultItemFields) || !sh.asked("message:isread", defaultItemFields) {
		t.Errorf("the named properties are not asked for: extra = %v", sh.extra)
	}

	msg := &webmail.Message{
		ID:       3,
		FolderID: 1,
		UID:      9,
		Subject:  "Quarterly report",
		Size:     1234,
		Date:     time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC),
	}
	v := &itemView{kind: "Message", container: 1, id: 3, message: msg, class: "IPM.Note"}

	var b strings.Builder
	renderMessageItem(&b, v, sh)
	out := b.String()
	if !strings.Contains(out, "<t:Subject>Quarterly report</t:Subject>") {
		t.Errorf("the render dropped the requested Subject: %s", out)
	}
	if strings.Contains(out, "<t:Size>") {
		t.Errorf("the render included a property nobody asked for: %s", out)
	}

	// A bare IdOnly still renders identity and nothing else.
	b.Reset()
	renderMessageItem(&b, v, parseShape(&shapeRequest{Base: "IdOnly"}))
	if strings.Contains(b.String(), "<t:Subject>") {
		t.Errorf("a bare IdOnly shape rendered more than identity: %s", b.String())
	}
}

// A field update arrives wrapped: the schema puts the new value in the item
// element it belongs to, <t:Message><t:IsRead>true</t:IsRead></t:Message>, and
// the reader that looks for the value has to look through the wrapper rather
// than at the siblings of the wrapper.
func TestInnerBoolReadsThroughTheWrapperElement(t *testing.T) {
	const fragment = `<t:FieldURI FieldURI="message:IsRead"/><t:Message><t:IsRead>true</t:IsRead></t:Message>`
	v, ok := innerBool(fragment, "IsRead")
	if !ok || !v {
		t.Fatalf("innerBool(%q, %q) = %v %v, want true true", fragment, "IsRead", v, ok)
	}

	// A wrapper element without the target inside does not stop a later,
	// deeper match from being found.
	const guarded = `<t:Message><t:IsFlagged>false</t:IsFlagged></t:Message><t:Message><t:IsRead>true</t:IsRead></t:Message>`
	v, ok = innerBool(guarded, "IsRead")
	if !ok || !v {
		t.Fatalf("innerBool(%q, %q) = %v %v, want true true", guarded, "IsRead", v, ok)
	}
}

// The explicit false is distinct from an absent element: a client that clears
// the read flag sends IsRead false, and an update that reads "false, but there"
// as "false, not there" would clear nothing while insisting it had.
func TestInnerBoolDistinguishesExplicitFalseFromAbsent(t *testing.T) {
	const setFalse = `<t:Message><t:IsRead>false</t:IsRead></t:Message>`
	v, ok := innerBool(setFalse, "IsRead")
	if !ok || v {
		t.Fatalf("innerBool(%q, %q) = %v %v, want false true", setFalse, "IsRead", v, ok)
	}

	const absent = `<t:Message><t:IsFlagged>true</t:IsFlagged></t:Message>`
	if _, ok := innerBool(absent, "IsRead"); ok {
		t.Fatalf("innerBool(%q, %q) reported an IsRead that is not there", absent, "IsRead")
	}
}

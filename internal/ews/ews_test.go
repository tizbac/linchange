package ews

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
)

// The fault body quotes back the operation name, which the caller supplies in
// the SOAPAction header. Reflecting that verbatim puts the caller's markup
// inside the response, so the escaping has to be real.
func TestUnsupportedOperationIsEscapedInTheFault(t *testing.T) {
	e := New(config.New(), policy.Policy{})

	req := httptest.NewRequest(http.MethodPost, "/exchange.asmx",
		strings.NewReader(soapRequest("FindItem")))
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
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
	e := New(config.New(), policy.Policy{})

	for _, operation := range []string{"FindItem", "GetItem", "CreateItem", "ResolveNames"} {
		req := httptest.NewRequest(http.MethodPost, "/exchange.asmx",
			strings.NewReader(soapRequest(operation)))
		req.Header.Set("Content-Type", "text/xml; charset=utf-8")
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
	e := New(config.New(), policy.Policy{})

	req := httptest.NewRequest(http.MethodPost, "/exchange.asmx",
		strings.NewReader(soapRequest("FindItem")))
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
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
	e := New(config.New(), policy.Policy{})

	req := httptest.NewRequest(http.MethodPost, "/ews/exchange.asmx",
		strings.NewReader(soapRequest("FindItem")))
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
	e := New(config.New(), policy.Policy{})

	req := httptest.NewRequest(http.MethodPost, "/exchange.asmx", strings.NewReader(soapRequest("FindItem")))
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
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
	e := New(config.New(), policy.Policy{})

	req := httptest.NewRequest(http.MethodPost, "/ews/exchange.asmx", strings.NewReader(soapRequest("FindItem")))
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	rr := httptest.NewRecorder()

	e.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("POST /ews/exchange.asmx = %d, want 404; the mount point supplies the prefix", rr.Code)
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

package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Exchange endpoints are addressed by path, and every one of them is
// mounted by the server at a prefix. If a handler also spells out that prefix
// internally the endpoint moves: EWS answered at /ews/ews/exchange.asmx, which
// is neither what MS-OXECWS specifies nor the URL Autodiscover reports, so no
// client could reach it.
func TestExchangeEndpointsAnswerAtTheirDocumentedPaths(t *testing.T) {
	db := testMigratedDB(t)
	// Autodiscover answers 404 for a domain it does not host, which is the same
	// status as an unrouted path, so the domain has to exist for this test to
	// say anything about routing.
	exec(t, db, `INSERT INTO domains (name) VALUES ('localhost') ON CONFLICT DO NOTHING`)
	srv := newServer(t, db)

	for _, tc := range []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{
			name:       "autodiscover",
			method:     http.MethodGet,
			path:       "/autodiscover/autodiscover.xml?email=admin@localhost",
			wantStatus: http.StatusOK,
		},
		{
			name:   "ews",
			method: http.MethodPost,
			path:   "/ews/exchange.asmx",
			body:   soapEnvelope,
			// Reaching the service at all is the point; whether this particular
			// envelope is well formed is the service's business, and it answers a
			// malformed one in SOAP like any other request.
			wantStatus: 0,
		},
		{
			name:       "active sync",
			method:     http.MethodPost,
			path:       "/eas/",
			body:       wbxmlOptions,
			wantStatus: 0,
		},
		{
			name:       "mapi over http",
			method:     http.MethodPost,
			path:       "/mapi/emsmdb",
			wantStatus: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.body != "" {
				req.Header.Set("Content-Type", "text/xml; charset=utf-8")
			}
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code == http.StatusNotFound {
				t.Fatalf("%s %s returned 404; the endpoint is not at this path", tc.method, tc.path)
			}
			if tc.wantStatus != 0 && rr.Code != tc.wantStatus {
				t.Errorf("%s %s = %d, want %d; body: %s", tc.method, tc.path, rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

// wbxmlOptions is a WBXML Options request: an ActiveSync client sends it first
// to negotiate the protocol version, so it needs no credentials.
const wbxmlOptions = `<?xml version="1.0"?>
<Options xmlns="http://schemas.microsoft.com/exchange/services/2006/messages"/>`

// soapEnvelope is a minimal EWS request: a ResolveNames call, which is the
// cheapest operation the service implements.
const soapEnvelope = `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
  <s:Body>
    <m:ResolveNames xmlns:m="http://schemas.microsoft.com/exchange/services/2006/messages"
                    xmlns:t="http://schemas.microsoft.com/exchange/services/2006/types">
      <m:UnresolvedEntry>admin</m:UnresolvedEntry>
    </m:ResolveNames>
  </s:Body>
</s:Envelope>`

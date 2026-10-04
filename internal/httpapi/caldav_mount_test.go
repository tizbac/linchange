package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CalDAV is not part of the REST surface. A calendar client is handed
// /.well-known/caldav or /caldav and follows it, so the endpoint has to answer
// on those paths; an earlier version mounted it inside /api/v1 and answered 404
// to both, which is a server no calendar client can configure.
func TestCalDAVIsMountedAtTheWellKnownPaths(t *testing.T) {
	srv := newServer(t, testMigratedDB(t))

	for _, path := range []string{"/caldav/", "/caldav"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)

		if rr.Code == http.StatusNotFound {
			t.Errorf("GET %s returned 404; CalDAV must answer at this path", path)
		}
		if rr.Code == http.StatusOK && !strings.Contains(rr.Body.String(), "multistatus") {
			t.Errorf("GET %s answered %d without a CalDAV multistatus body: %s",
				path, rr.Code, rr.Body.String())
		}
	}
}

// The well-known URI is what a client follows when it is given only a domain
// name. RFC 6764 has it redirect to the service prefix, so that is what it has
// to do: a 200 with an unrelated body would be worse than no answer.
func TestCalDAVWellKnownResolves(t *testing.T) {
	srv := newServer(t, testMigratedDB(t))

	req := httptest.NewRequest(http.MethodGet, "/.well-known/caldav", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusMovedPermanently && rr.Code != http.StatusFound {
		t.Fatalf("GET /.well-known/caldav = %d, want a redirect to the CalDAV home; body: %s",
			rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/caldav/" {
		t.Errorf("redirect target = %q, want /caldav/", got)
	}
}

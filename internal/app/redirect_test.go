package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The redirect target must come from configuration, never from the request's
// Host header, or the plaintext listener becomes an open redirect.
func TestHTTPSRedirectUsesConfiguredHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/path?q=1", nil)
	req.Host = "evil.example"
	rr := httptest.NewRecorder()

	httpsRedirectHandler("mail.example.com").ServeHTTP(rr, req)

	if rr.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rr.Code)
	}
	if got, want := rr.Header().Get("Location"), "https://mail.example.com/path?q=1"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The SPA file server must never answer a path that escapes the web root. A
// path like /../secret.txt is not normalised by net/http or chi, and the
// no-locale branch used to join it straight onto the root.
func TestServeSPARejectsTraversal(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "web")
	if err := os.MkdirAll(filepath.Join(root, "en"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "en", "index.html"), []byte("shell"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "en", "app.js"), []byte("app"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "secret.txt"), []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{staticDir: root}
	for _, path := range []string{
		"/../secret.txt",
		"/../../secret.txt",
		"/en/../../secret.txt",
		"/..%2fsecret.txt",
		"/en/..%2f..%2fsecret.txt",
	} {
		req := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
		req.URL.Path = path
		rr := httptest.NewRecorder()
		s.serveSPA(rr, req)
		if rr.Code == http.StatusOK && rr.Body.String() == "TOP-SECRET" {
			t.Fatalf("GET %q served a file outside the web root", path)
		}
	}
}

func TestServeSPAServesNestedAssetsAndShell(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "en"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "en", "index.html"), []byte("shell"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "en", "app.js"), []byte("app"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{staticDir: root}
	for path, want := range map[string]string{
		"/en/app.js":     "app",
		"/en/client/url": "shell",
	} {
		req := httptest.NewRequest(http.MethodGet, "http://example.test"+path, nil)
		rr := httptest.NewRecorder()
		s.serveSPA(rr, req)
		if rr.Code != http.StatusOK || rr.Body.String() != want {
			t.Fatalf("GET %q = %d %q, want 200 %q", path, rr.Code, rr.Body.String(), want)
		}
	}
}

// A link without a locale is redirected to the language the browser asked for.
// The order matters: the cookie is an explicit choice, Accept-Language is a
// guess, and English is what everything falls back to.
func TestPreferredLanguage(t *testing.T) {
	cases := []struct {
		cookie string
		accept string
		want   string
	}{
		{"", "", "en"},
		{"", "de-DE,de;q=0.9,en;q=0.8", "de"},
		{"", "pl-PL", "pl"},
		{"", "it-IT,it;q=0.9", "it"},
		{"", "fr,de;q=0.5", "de"},
		{"", "zh;q=0.9,ja;q=0.8", "en"},
		{"de", "en-US", "de"},
		{"pl", "de-DE", "pl"},
		{"it", "en", "it"},
		{"xx", "de", "de"}, // an unknown cookie is not a language
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "http://example.test/mail", nil)
		req.Header.Set("Accept-Language", c.accept)
		if c.cookie != "" {
			req.AddCookie(&http.Cookie{Name: "linchange_lang", Value: c.cookie})
		}
		if got := preferredLanguage(req); got != c.want {
			t.Errorf("cookie %q accept %q = %q, want %q", c.cookie, c.accept, got, c.want)
		}
	}
}

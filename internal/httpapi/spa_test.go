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

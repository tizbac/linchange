package httpapi

import (
	"archive/zip"
	"bytes"
	"testing"
)

func testZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("creating %q: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("writing %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes()
}

func TestThemeZipPrefix(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		want    string
		wantErr bool
	}{
		{
			name:  "stylesheet at the root",
			files: map[string]string{"theme.css": "body{}", "logo.png": "x"},
			want:  "",
		},
		{
			name:  "stylesheet in one top-level folder",
			files: map[string]string{"mytheme/theme.css": "body{}", "mytheme/logo.png": "x"},
			want:  "mytheme/",
		},
		{
			name:    "no stylesheet",
			files:   map[string]string{"style.css": "body{}"},
			wantErr: true,
		},
		{
			name:    "two top-level folders",
			files:   map[string]string{"a/theme.css": "body{}", "b/logo.png": "x"},
			wantErr: true,
		},
		{
			name:    "path escape",
			files:   map[string]string{"../theme.css": "body{}"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := themeZipPrefix(testZip(t, tc.files))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("themeZipPrefix() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("themeZipPrefix() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("themeZipPrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestThemeZipPrefixRejectsNonZip(t *testing.T) {
	if _, err := themeZipPrefix([]byte("not a zip")); err == nil {
		t.Fatal("themeZipPrefix() accepted a non-ZIP")
	}
}

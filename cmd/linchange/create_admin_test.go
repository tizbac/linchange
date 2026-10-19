package main

import (
	"testing"
)

func TestNormaliseAddress(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "Admin@Example.COM", want: "admin@example.com"},
		{in: "  bob@example.com  ", want: "bob@example.com"},
		{in: `"Bob the Admin" <bob@example.com>`, want: "bob@example.com"},
		{in: "", wantErr: true},
		{in: "bobexample.com", wantErr: true},
		{in: "@example.com", wantErr: true},
		{in: "bob@", wantErr: true},
		// Two addresses must not collapse into one silently.
		{in: "bob@example.com, carol@example.com", wantErr: true},
	}
	for _, tc := range tests {
		got, err := normaliseAddress(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normaliseAddress(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normaliseAddress(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normaliseAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSplitAddress(t *testing.T) {
	local, domain := splitAddress("admin@example.com")
	if local != "admin" || domain != "example.com" {
		t.Errorf("splitAddress = (%q, %q), want (admin, example.com)", local, domain)
	}
}

func TestPickName(t *testing.T) {
	tests := []struct {
		requested, local, want string
	}{
		{requested: "Tiziano", local: "tiziano", want: "Tiziano"},
		{requested: "  ", local: "tiziano", want: "Tiziano"},
		{requested: "", local: "tiziano", want: "Tiziano"},
		// A multi-byte local part must not be split mid-rune.
		{requested: "", local: "élodie", want: "Élodie"},
		{requested: "", local: "", want: ""},
	}
	for _, tc := range tests {
		if got := pickName(tc.requested, tc.local); got != tc.want {
			t.Errorf("pickName(%q, %q) = %q, want %q", tc.requested, tc.local, got, tc.want)
		}
	}
}

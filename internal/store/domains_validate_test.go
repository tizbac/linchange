package store

import (
	"strings"
	"testing"
)

func TestValidateDomainName(t *testing.T) {
	valid := []string{"example.com", "mail.example.com", "localhost", "a-b.example"}
	invalid := []string{
		"", "..", "../etc", "example..com", ".example.com", "example.com.",
		"exa mple.com", "example.com/path", `example\com`, "example.com\x00",
		"-example.com", "example-.com", strings.Repeat("a", 64) + ".com",
	}
	for _, name := range valid {
		if err := ValidateDomainName(name); err != nil {
			t.Errorf("ValidateDomainName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range invalid {
		if err := ValidateDomainName(name); err == nil {
			t.Errorf("ValidateDomainName(%q) accepted an unsafe name", name)
		}
	}
}

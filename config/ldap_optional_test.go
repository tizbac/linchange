package config_test

import (
	"strings"
	"testing"

	"github.com/linchange/linchange/config"
)

// A deployment with no directory is a supported mode of operation, so the
// whole configuration must be valid without an `ldap:` section at all. This is
// worth pinning: LDAP is entirely optional, and a required-field check or a
// validation rule added carelessly would quietly turn it into a startup
// requirement for everyone.
func TestNoLDAPIsAValidModeOfOperation(t *testing.T) {
	cfg := config.New()
	cfg.Server.Hostname = "mail.example.com"
	cfg.Server.PublicIP = "203.0.113.10"
	cfg.Database.URL = "postgres://localhost/linchange"
	cfg.Auth.JWT.Secret = strings.Repeat("x", 40)
	cfg.TLS.CertFile = "/tmp/cert.pem"
	cfg.TLS.KeyFile = "/tmp/key.pem"
	cfg.Exchange.Organization = "Example Ltd"
	cfg.Mail.Outbound.DSN.FromAddress = "postmaster@example.com"
	cfg.Mail.DkimPrivateKeyEncryptionKey = strings.Repeat("k", 32)
	cfg.Auth.EncryptionKey = strings.Repeat("e", 32)

	if len(cfg.LDAP.Providers) != 0 {
		t.Errorf("providers = %d, want 0 by default", len(cfg.LDAP.Providers))
	}
	if cfg.LDAP.Sync.Enabled {
		t.Error("the reconcile must default to disabled")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a config with no directory must validate: %v", err)
	}
}

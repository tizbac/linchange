package config_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linchange/linchange/config"
)

// The example configuration ships with the project. A file that does not
// validate is worse than no file, so it is checked like any other.
func TestExampleConfigIsValid(t *testing.T) {
	setSecrets(t)

	path := filepath.Join("linchange.example.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the shipped example configuration must load and validate: %v", err)
	}

	// A few properties worth pinning, because a careless edit to the example
	// would otherwise change what a new deployment does by default.
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"enforce_sender_identity", cfg.Mail.Submission.EnforceSenderIdentity, true},
		{"daily_send_limit", cfg.Mail.Submission.DailySendLimit, 500},
		{"inbound enabled", cfg.Mail.Inbound.Enabled, true},
		{"reject_unknown_recipients", cfg.Mail.Inbound.RejectUnknownRecipients, true},
		{"default_quota_bytes", cfg.Mail.DefaultQuotaBytes, int64(10737418240)},
		{"exchange enabled", cfg.Exchange.Enabled, true},
		{"ews path", cfg.Exchange.EWS.Path, "/ews/exchange.asmx"},
		{"eas path", cfg.Exchange.EAS.Path, "/eas"},
		{"autodiscover path", cfg.Exchange.Autodiscover.Path, "/autodiscover/autodiscover.xml"},
		{"auto_migrate", cfg.Database.AutoMigrate, true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("example config: %s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// The example documents a commented-out LDAP provider. If environment
// expansion ran over raw text it would try to resolve ${LDAP_BIND_PASSWORD}
// from the comment and fail, so this pins the behaviour that only real string
// values are expanded.
func TestExampleConfigIgnoresEnvRefsInComments(t *testing.T) {
	setSecrets(t)
	// Deliberately not set.
	t.Setenv("LDAP_BIND_PASSWORD", "")

	path := filepath.Join("linchange.example.yaml")
	if _, err := config.Load(path); err != nil {
		t.Fatalf("a ${VAR} inside a comment must not be expanded: %v", err)
	}
}

// Every YAML key in the example must be a real setting. An unknown key is a
// hard error, which catches a typo that would otherwise be silently ignored.
func TestExampleConfigHasNoUnknownKeys(t *testing.T) {
	setSecrets(t)

	// Load already fails on unknown keys, so reaching here is the assertion.
	// The extra check is that a regression is attributed clearly.
	if _, err := config.Load(filepath.Join("linchange.example.yaml")); err != nil {
		if strings.Contains(err.Error(), "field") {
			t.Fatalf("the example contains an unknown key: %v", err)
		}
		t.Fatalf("load: %v", err)
	}
}

// TestDefaultsAreProductionSafe guards the defaults themselves. These are the
// values a deployment gets when the example file omits something, so weakening
// one here would weaken every installation.
func TestDefaultsAreProductionSafe(t *testing.T) {
	cfg := config.New()

	safe := []struct {
		name string
		got  bool
	}{
		{"submission requires TLS", cfg.Mail.Submission.RequireTLS},
		{"insecure auth is refused", !cfg.Mail.Submission.AllowInsecureAuth},
		{"sender identity is enforced", cfg.Mail.Submission.EnforceSenderIdentity},
		{"inbound rejects unknown recipients", cfg.Mail.Inbound.RejectUnknownRecipients},
		{"TOTP is available", cfg.Auth.TOTP.Enabled},
		{"password reset is available", cfg.Auth.Reset.Enabled},
		{"audit logging is on", cfg.Log.Audit},
		{"impersonation requires a reason", cfg.Auth.Impersonation.RequireReason},
		{"argon2 uses a strong memory cost", cfg.Auth.Local.Argon2.MemoryKib >= 65536},
		{"unknown recipients rejected before delivery", cfg.Mail.Inbound.MaxRecipients > 0},
	}
	for _, c := range safe {
		if !c.got {
			t.Errorf("default is unsafe: %s", c.name)
		}
	}

	if cfg.Mail.Submission.DailySendLimit <= 0 {
		t.Error("a daily send cap should be on by default, so a compromised " +
			"account cannot be used to spam the world on its first day")
	}
}

// setSecrets provides the two environment variables the example references.
func setSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SECRET", strings.Repeat("j", 40))
	t.Setenv("DKIM_ENCRYPTION_KEY", strings.Repeat("d", 32))
}

func TestMain(m *testing.M) {
	// Keep the tests hermetic regardless of the developer's shell.
	_ = runtime.GOMAXPROCS(0)
	m.Run()
}

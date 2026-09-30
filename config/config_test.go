package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// minimalConfig is the smallest document that passes validation. Tests build
// on it so they exercise one concern at a time.
const minimalConfig = `
database:
  url: postgres://localhost/linchange
auth:
  encryption_key: 0123456789abcdef0123456789abcdef
  jwt:
    secret: 0123456789abcdef0123456789abcdef0123456789
tls:
  cert_file: /tmp/cert.pem
  key_file: /tmp/key.pem
exchange:
  organization: Example Ltd
mail:
  dkim_private_key_encryption_key: 0123456789abcdef0123456789abcdef
  outbound:
    dsn:
      from_address: postmaster@example.com
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "linchange.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestLoadRejectsMissingPath(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Fatal("expected an error when no path is given")
	} else if !strings.Contains(err.Error(), "--config") {
		t.Errorf("error should mention the --config flag, got: %v", err)
	}
}

func TestLoadRejectsUnreadableFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil || !strings.Contains(err.Error(), "reading") {
		t.Fatalf("expected a read error, got: %v", err)
	}
}

// The whole point of decoding onto a pre-populated default document is that an
// explicit `false` must win over a default of `true`, and an absent key must
// not.
func TestLoadDistinguishesExplicitFalseFromAbsent(t *testing.T) {
	t.Setenv("TEST_SECRET", "0123456789abcdef0123456789abcdef0123456789")

	path := writeConfig(t, `
database:
  url: postgres://localhost/linchange
auth:
  jwt:
    secret: ${TEST_SECRET}
  totp:
    enabled: false
tls:
  cert_file: /tmp/cert.pem
  key_file: /tmp/key.pem
exchange:
  organization: Example Ltd
mail:
  dkim_private_key_encryption_key: 0123456789abcdef0123456789abcdef
  outbound:
    dsn:
      from_address: postmaster@example.com
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Auth.TOTP.Enabled {
		t.Error("auth.totp.enabled was explicitly false but came back true")
	}
	// Absent keys must still be defaulted.
	if !cfg.Mail.Submission.EnforceSenderIdentity {
		t.Error("an absent bool should have taken its default of true")
	}
	if !cfg.Database.AutoMigrate {
		t.Error("an absent bool should have taken its default of true")
	}
}

func TestDefaultsAppliedWhenOmitted(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"server.hostname", cfg.Server.Hostname, "localhost"},
		{"listeners.smtp_inbound", cfg.Listener.SMTPInbound.Addr, ":25"},
		{"listeners.imaps", cfg.Listener.IMAPS.Addr, ":993"},
		{"listeners.lmtp", cfg.Listener.LMTP.Addr, ""},
		{"auth.jwt.issuer", cfg.Auth.JWT.Issuer, "linchange"},
		{"auth.local.argon2.memory_kib", cfg.Auth.Local.Argon2.MemoryKib, uint32(65536)},
		{"mail.dkim.key_bits", cfg.Mail.DKIM.KeyBits, 2048},
		{"exchange.eas.path", cfg.Exchange.EAS.Path, "/eas"},
		{"logging.level", cfg.Log.Level, "info"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("CFG_TEST_PRESENT", "hello")

	t.Run("substitutes set variables", func(t *testing.T) {
		got, err := ExpandEnv("a: ${CFG_TEST_PRESENT}", "test")
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		if got != "a: hello" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("substitutes an unset variable when a fallback is given", func(t *testing.T) {
		got, err := ExpandEnv("a: ${CFG_TEST_ABSENT:-fallback}", "test")
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		if got != "a: fallback" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("an unset variable with no fallback is an error", func(t *testing.T) {
		_, err := ExpandEnv("a: ${CFG_TEST_ABSENT}", "test")
		if err == nil {
			t.Fatal("expected an error for a missing secret")
		}
		if !strings.Contains(err.Error(), "CFG_TEST_ABSENT") {
			t.Errorf("error should name the variable, got: %v", err)
		}
	})

	t.Run("an empty variable counts as unset", func(t *testing.T) {
		t.Setenv("CFG_TEST_EMPTY", "")
		if _, err := ExpandEnv("a: ${CFG_TEST_EMPTY}", "test"); err == nil {
			t.Error("an empty value should be treated as unset so a missing secret fails loudly")
		}
	})
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	cfg := New()
	cfg.Database.URL = ""
	cfg.Auth.JWT.Secret = "short"
	cfg.Listener.IMAP.Addr = ":143"
	cfg.Listener.IMAPS.Addr = ":143" // duplicate port
	cfg.Log.Level = "chatty"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation to fail")
	}

	var ve *ValidationError
	if !asValidationError(err, &ve) {
		t.Fatalf("expected a ValidationError, got %T", err)
	}
	if len(ve.Problems) < 4 {
		t.Errorf("expected several problems at once, got %d: %v", len(ve.Problems), ve.Problems)
	}

	joined := strings.Join(ve.Problems, "\n")
	for _, want := range []string{
		"database.url is required",
		"auth.jwt.secret must be at least 32",
		"bound to :143",
		"logging.level",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a problem mentioning %q, got:\n%s", want, joined)
		}
	}
}

func TestValidateTLSRequirements(t *testing.T) {
	t.Run("cert without key", func(t *testing.T) {
		cfg := validConfig()
		cfg.TLS.KeyFile = ""
		assertProblem(t, cfg, "cert_file and tls.key_file must be set together")
	})

	t.Run("TLS-only listener without a certificate", func(t *testing.T) {
		cfg := validConfig()
		cfg.TLS.CertFile, cfg.TLS.KeyFile = "", ""
		assertProblem(t, cfg, "no TLS certificate is available")
	})

	t.Run("ACME and static cert together", func(t *testing.T) {
		cfg := validConfig()
		cfg.TLS.ACME = ACME{Enabled: true, Email: "a@example.com", Domains: []string{"example.com"}}
		assertProblem(t, cfg, "mutually exclusive")
	})
}

func TestValidateLDAPProviderRequirements(t *testing.T) {
	t.Run("domain_admins_group_dn is required", func(t *testing.T) {
		cfg := validConfig()
		cfg.LDAP.Providers = []LDAPProvider{{
			Name: "corp", URL: "ldaps://dc.example.com", BaseDN: "dc=example,dc=com",
			UserFilter: "(&(objectClass=user)(mail={{username}}))",
		}}
		assertProblem(t, cfg, "domain_admins_group_dn is required")
	})

	t.Run("user_filter must contain the placeholder", func(t *testing.T) {
		cfg := validConfig()
		cfg.LDAP.Providers = []LDAPProvider{{
			Name: "corp", URL: "ldaps://dc.example.com", BaseDN: "dc=example,dc=com",
			UserFilter: "(objectClass=user)",
		}}
		assertProblem(t, cfg, "{{username}}")
	})

	t.Run("bad url scheme", func(t *testing.T) {
		cfg := validConfig()
		cfg.LDAP.Providers = []LDAPProvider{{
			Name: "corp", URL: "https://dc.example.com", BaseDN: "dc=example,dc=com",
			UserFilter: "(mail={{username}})",
		}}
		assertProblem(t, cfg, "ldap:// or ldaps://")
	})

	t.Run("duplicate provider names", func(t *testing.T) {
		cfg := validConfig()
		p := LDAPProvider{
			Name: "corp", URL: "ldaps://dc.example.com", BaseDN: "dc=example,dc=com",
			UserFilter: "(mail={{username}})", DomainAdminsGroupDN: "cn=admins",
		}
		cfg.LDAP.Providers = []LDAPProvider{p, p}
		assertProblem(t, cfg, "is duplicated")
	})
}

func TestValidateRejectsInsecureProductionCombinations(t *testing.T) {
	t.Run("DKIM signing without an encryption key", func(t *testing.T) {
		cfg := validConfig()
		cfg.Mail.DKIM.SignAll = true
		cfg.Mail.DkimPrivateKeyEncryptionKey = ""
		assertProblem(t, cfg, "dkim_private_key_encryption_key is required")
	})

	t.Run("DSN without a bounce sender", func(t *testing.T) {
		cfg := validConfig()
		cfg.Mail.Outbound.DSN.Enabled = true
		cfg.Mail.Outbound.DSN.FromAddress = ""
		assertProblem(t, cfg, "dsn.from_address is required")
	})

	t.Run("refresh TTL shorter than access TTL", func(t *testing.T) {
		cfg := validConfig()
		cfg.Auth.JWT.AccessTTL = time.Hour
		cfg.Auth.JWT.RefreshTTL = time.Minute
		assertProblem(t, cfg, "must be longer than access_ttl")
	})

	t.Run("weak argon2 memory cost", func(t *testing.T) {
		cfg := validConfig()
		cfg.Auth.Local.Argon2.MemoryKib = 1024
		assertProblem(t, cfg, "memory_kib must be at least 8192")
	})

	t.Run("idle timeout beyond absolute timeout", func(t *testing.T) {
		cfg := validConfig()
		cfg.Auth.Session.IdleTimeout = 24 * time.Hour
		cfg.Auth.Session.AbsoluteTimeout = time.Hour
		assertProblem(t, cfg, "must not exceed absolute_timeout")
	})
}

// A default public IP of loopback combined with a real hostname produces mail
// that no remote MTA will accept, so it is worth refusing.
func TestValidateFlagsLoopbackWithRealHostname(t *testing.T) {
	cfg := validConfig()
	cfg.Server.Hostname = "mail.example.com"
	cfg.Server.PublicIP = "127.0.0.1"
	assertProblem(t, cfg, "rejected by remote MTAs")
}

func TestValidateRejectsUnknownKeys(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+"\nnot_a_section: true\n"))
	if err == nil {
		t.Fatal("an unknown key should be an error, not silently ignored")
	}
	if !strings.Contains(err.Error(), "not_a_section") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

func TestEmptyDocumentIsAllDefaults(t *testing.T) {
	path := writeConfig(t, "# nothing but a comment\n")
	// An empty document fails validation because database.url and the JWT
	// secret are mandatory, which is the intended behaviour.
	if _, err := Load(path); err == nil {
		t.Fatal("an empty document should fail validation")
	}
}

func TestDirJoinsPathSafely(t *testing.T) {
	cfg := New()
	cfg.Server.DataDir = "/var/lib/linchange"
	if got, want := cfg.Dir("blobs", "ab"), "/var/lib/linchange/blobs/ab"; got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
	if got, want := cfg.Dir("blobs/"), "/var/lib/linchange/blobs"; got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestTLSEnabled(t *testing.T) {
	cfg := New()
	if cfg.TLSEnabled() {
		t.Error("no TLS material means TLS is not enabled")
	}
	cfg.TLS.CertFile, cfg.TLS.KeyFile = "c", "k"
	if !cfg.TLSEnabled() {
		t.Error("a cert and key pair means TLS is enabled")
	}
}

func validConfig() *Config {
	cfg := New()
	cfg.Server.Hostname = "mail.example.com"
	cfg.Server.PublicIP = "203.0.113.10"
	cfg.Database.URL = "postgres://localhost/linchange"
	cfg.Auth.JWT.Secret = strings.Repeat("x", 40)
	cfg.TLS.CertFile = "/tmp/cert.pem"
	cfg.TLS.KeyFile = "/tmp/key.pem"
	cfg.Exchange.Organization = "Example Ltd"
	cfg.Mail.Outbound.DSN.FromAddress = "postmaster@example.com"
	cfg.Mail.DkimPrivateKeyEncryptionKey = strings.Repeat("k", 32)
	return cfg
}

func assertProblem(t *testing.T, cfg *Config, substr string) {
	t.Helper()
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected a validation problem mentioning %q", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("expected a problem mentioning %q, got:\n%v", substr, err)
	}
}

func asValidationError(err error, target **ValidationError) bool {
	ve, ok := err.(*ValidationError)
	if ok {
		*target = ve
	}
	return ok
}

// Enrolment stores a secret it cannot encrypt, so the server must refuse to
// start rather than letting the first user to enrol discover the problem.
func TestTOTPRequiresAnEncryptionKey(t *testing.T) {
	const jwtSecret = "0123456789abcdef0123456789abcdef0123456789"

	// Each case is a whole document: appending a second `auth:` block to the
	// minimal config would be a duplicate YAML key, not an override.
	doc := func(auth string) string {
		return `
database:
  url: postgres://localhost/linchange
auth:
` + auth + `
tls:
  cert_file: /tmp/cert.pem
  key_file: /tmp/key.pem
exchange:
  organization: Example Ltd
mail:
  dkim_private_key_encryption_key: 0123456789abcdef0123456789abcdef
  outbound:
    dsn:
      from_address: postmaster@example.com
`
	}

	tests := []struct {
		name    string
		auth    string
		wantErr string
	}{
		{"key present", "  encryption_key: 0123456789abcdef0123456789abcdef\n  jwt:\n    secret: " + jwtSecret, ""},
		{"key absent", "  jwt:\n    secret: " + jwtSecret, "auth.encryption_key is required"},
		{"key too short", "  encryption_key: too-short\n  jwt:\n    secret: " + jwtSecret, "at least 32 bytes"},
		// Turning TOTP off removes the need, which is the escape hatch for an
		// operator who has not generated a key yet.
		{"totp disabled needs no key", "  jwt:\n    secret: " + jwtSecret + "\n  totp:\n    enabled: false", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, doc(tc.auth)))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

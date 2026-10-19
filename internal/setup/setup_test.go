package setup

import (
	"os"
	"strings"
	"testing"

	"github.com/linchange/linchange/config"
)

// validAnswers is a complete set of answers, so that a test only has to state
// the one thing it is actually about.
func validAnswers() Answers {
	a := Defaults()
	a.Hostname = "mail.example.com"
	a.PublicIP = "203.0.113.10"
	a.Domain = "example.com"
	a.AdminEmail = "admin@example.com"
	a.AdminPassword = "correct horse battery staple"
	a.TLSMode = TLSACME
	a.ACMEEmail = "ops@example.com"
	a.Organization = "Example Ltd"
	a.Exchange = true
	return a
}

func TestBuildConfigDisablesTLSListenersWithoutCertificate(t *testing.T) {
	// Regression: the base configuration carries the TLS listeners bound by
	// default, so turning TLS off has to clear them rather than leave them
	// advertised. A leftover :993 is refused by the validator, which is how
	// this was caught, but the deeper problem is a server that would bind a
	// TLS port with no certificate to serve on it.
	a := validAnswers()
	a.TLSMode = TLSNone
	a.IMAP, a.IMAPS = true, true
	a.POP3, a.POP3S = true, true
	a.SMTPSubmission = true
	a.HTTPSPort = 443

	cfg, warnings, err := BuildConfig(config.New(), a)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	for name, addr := range map[string]string{
		"imaps":               cfg.Listener.IMAPS.Addr,
		"pop3s":               cfg.Listener.POP3S.Addr,
		"smtp_submission_tls": cfg.Listener.SMTPSubmissionTLS.Addr,
		"https":               cfg.Listener.HTTPS.Addr,
	} {
		if addr != "" {
			t.Errorf("listeners.%s = %q, want empty when no certificate is configured", name, addr)
		}
	}
	if cfg.Listener.IMAP.Addr != ":143" {
		t.Errorf("listeners.imap = %q, want :143", cfg.Listener.IMAP.Addr)
	}
	// The operator asked for the TLS ports and did not get them, so they have
	// to be told rather than left to discover it when a client fails.
	if len(warnings) < 2 {
		t.Errorf("warnings = %v, want one per refused TLS listener", warnings)
	}
}

func TestBuildConfigEnablesTLSListenersWithACME(t *testing.T) {
	a := validAnswers()
	a.IMAP, a.IMAPS = true, true
	a.POP3, a.POP3S = true, true
	a.SMTPSubmission = true
	a.HTTPSPort = 443

	cfg, warnings, err := BuildConfig(config.New(), a)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	for name, want := range map[string]string{
		"imaps":               ":993",
		"pop3s":               ":995",
		"smtp_submission_tls": ":465",
		"https":               ":443",
	} {
		var got string
		switch name {
		case "imaps":
			got = cfg.Listener.IMAPS.Addr
		case "pop3s":
			got = cfg.Listener.POP3S.Addr
		case "smtp_submission_tls":
			got = cfg.Listener.SMTPSubmissionTLS.Addr
		case "https":
			got = cfg.Listener.HTTPS.Addr
		}
		if got != want {
			t.Errorf("listeners.%s = %q, want %q", name, got, want)
		}
	}
	if !cfg.TLS.ACME.Enabled || cfg.TLS.ACME.Email != "ops@example.com" {
		t.Errorf("acme = %+v, want enabled with ops@example.com", cfg.TLS.ACME)
	}
	if got := cfg.TLS.ACME.Domains; len(got) != 1 || got[0] != "mail.example.com" {
		t.Errorf("acme domains = %v, want [mail.example.com]", got)
	}
	if !cfg.TLS.RedirectHTTP {
		t.Error("tls.redirect_http = false, want true so the ACME challenge is reachable")
	}
}

func TestBuildConfigSetsDSNFromAddress(t *testing.T) {
	// A generated bounce claims to come from this installation; an empty or
	// foreign envelope sender is rejected by the receivers it is aimed at.
	cfg, _, err := BuildConfig(config.New(), validAnswers())
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if got := cfg.Mail.Outbound.DSN.FromAddress; got != "postmaster@example.com" {
		t.Errorf("dsn.from_address = %q, want postmaster@example.com", got)
	}
}

func TestBuildConfigGeneratesSecretsAndKeepsExistingOnes(t *testing.T) {
	base := config.New()
	base.Auth.JWT.Secret = "an-existing-jwt-secret-that-must-survive-0123456789"
	cfg, _, err := BuildConfig(base, validAnswers())
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if cfg.Auth.JWT.Secret != base.Auth.JWT.Secret {
		t.Errorf("auth.jwt.secret was regenerated; re-running the wizard must not sign every user out")
	}
	if cfg.Auth.EncryptionKey == "" {
		t.Error("auth.encryption_key was not generated; TOTP enrolment would fail")
	}
	if cfg.Mail.DkimPrivateKeyEncryptionKey == "" {
		t.Error("mail.dkim_private_key_encryption_key was not generated; outbound signing would fail")
	}
	// All three have to be long enough to be real keys, not placeholders.
	for name, value := range map[string]string{
		"auth.jwt.secret":                      cfg.Auth.JWT.Secret,
		"auth.encryption_key":                  cfg.Auth.EncryptionKey,
		"mail.dkim_private_key_encryption_key": cfg.Mail.DkimPrivateKeyEncryptionKey,
	} {
		if len(value) < 32 {
			t.Errorf("%s is %d characters, want at least 32", name, len(value))
		}
	}
}

func TestBuildConfigDoesNotMutateBase(t *testing.T) {
	// The base is the configuration already on disk. A wizard run that ends
	// in a refusal must leave it as it was.
	base := config.New()
	before := base.Listener.IMAPS.Addr
	a := validAnswers()
	a.TLSMode = TLSNone
	a.IMAPS = true
	if _, _, err := BuildConfig(base, a); err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if base.Listener.IMAPS.Addr != before {
		t.Errorf("base listeners.imaps changed from %q to %q", before, base.Listener.IMAPS.Addr)
	}
}

func TestAnswersCheck(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Answers)
		want   string
	}{
		{"complete", func(*Answers) {}, ""},
		{"missing domain", func(a *Answers) { a.Domain = "" }, "domain"},
		{"domain with at sign", func(a *Answers) { a.Domain = "user@example.com" }, "bare name"},
		{"missing admin password", func(a *Answers) { a.AdminPassword = "" }, "administrator password"},
		{"missing admin email", func(a *Answers) { a.AdminEmail = "" }, "administrator address"},
		{"bad sslmode", func(a *Answers) { a.DBSSLMode = "sometimes" }, "sslmode"},
		{"port out of range", func(a *Answers) { a.DBPort = 70000 }, "port"},
		{"no protocol", func(a *Answers) {
			a.IMAP, a.POP3, a.SMTPInbound, a.SMTPSubmission = false, false, false, false
		}, "at least one mail protocol"},
		{"acme without email", func(a *Answers) { a.ACMEEmail = "" }, "acme-email"},
		{"certificates without paths", func(a *Answers) { a.TLSMode = TLSCert }, "--cert-file"},
		{"exchange without organization", func(a *Answers) { a.Organization = "" }, "organization"},
		{"bad log level", func(a *Answers) { a.LogLevel = "chatty" }, "log level"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := validAnswers()
			tc.mutate(&a)
			err := a.Check()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Check = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Check = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Check = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDBURLEscapesThePassword(t *testing.T) {
	a := validAnswers()
	a.DBUser, a.DBPassword = "linchange", "p@ss:w/rd?x"
	a.DBHost, a.DBPort, a.DBName, a.DBSSLMode = "db.internal", 5433, "mail", "verify-full"

	got := a.DBURL()
	// The @ and : of the password must not be read as the host separator, or
	// the connection silently goes somewhere else entirely.
	if !strings.HasPrefix(got, "postgres://linchange:p%40ss%3Aw%2Frd%3Fx@db.internal:5433/mail?") {
		t.Errorf("DBURL = %q, want the password escaped and the host intact", got)
	}
	if !strings.Contains(got, "sslmode=verify-full") {
		t.Errorf("DBURL = %q, want the sslmode carried", got)
	}
}

func TestWriteFilePermissionsBackupAndReload(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/linchange.yaml"

	cfg, _, err := BuildConfig(config.New(), validAnswers())
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	backup, err := WriteFile(path, cfg)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if backup != "" {
		t.Errorf("backup = %q, want empty on a first write", backup)
	}

	// The file holds the database password and the signing keys.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("the written configuration does not load: %v", err)
	}
	if loaded.Server.Hostname != "mail.example.com" {
		t.Errorf("hostname = %q, want mail.example.com", loaded.Server.Hostname)
	}

	// A second write must keep the first file beside it: the operator is
	// about to restart a mail server on this configuration.
	loaded.Server.Hostname = "changed.example.com"
	backup, err = WriteFile(path, loaded)
	if err != nil {
		t.Fatalf("WriteFile (second): %v", err)
	}
	if backup == "" {
		t.Fatal("backup = empty, want the previous file to have been saved")
	}
	saved, err := config.Load(backup)
	if err != nil {
		t.Fatalf("the backup does not load: %v", err)
	}
	if saved.Server.Hostname != "mail.example.com" {
		t.Errorf("backup hostname = %q, want the previous value mail.example.com", saved.Server.Hostname)
	}
}

func TestWriteFileKeepsHeaderNotes(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/linchange.yaml"
	cfg, _, err := BuildConfig(config.New(), validAnswers())
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if _, err := WriteFile(path, cfg); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// A generated file nobody explained is a file nobody dares to edit.
	for _, want := range []string{"linchange setup", "linchange.example.yaml", "auth.jwt.secret"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the written file does not mention %q", want)
		}
	}
}

func TestGuessPublicIPReturnsSomethingUsable(t *testing.T) {
	got := GuessPublicIP()
	if got == "" {
		t.Fatal("GuessPublicIP = empty")
	}
	if got == "0.0.0.0" {
		t.Error("GuessPublicIP = 0.0.0.0, which is not an address to publish")
	}
}

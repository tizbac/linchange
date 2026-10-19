package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linchange/linchange/internal/setup"
)

func TestParseDatabaseURL(t *testing.T) {
	tests := []struct {
		in   string
		host string
		port int
		name string
		user string
		ssl  string
		ok   bool
	}{
		{in: "postgres://linchange:secret@127.0.0.1:5432/linchange?sslmode=disable",
			host: "127.0.0.1", port: 5432, name: "linchange", user: "linchange", ssl: "disable", ok: true},
		{in: "postgres://db.internal/mail", host: "db.internal", port: 5432, name: "mail", user: "postgres", ssl: "disable", ok: true},
		{in: "postgresql://u:p@db:55432/x?sslmode=verify-full&application_name=linchange",
			host: "db", port: 55432, name: "x", user: "u", ssl: "verify-full", ok: true},
		// An IPv6 literal has colons of its own, and the port follows the
		// bracketed host rather than the last colon in the string.
		{in: "postgres://linchange:pw@[::1]:5432/linchange?sslmode=disable",
			host: "[::1]", port: 5432, name: "linchange", user: "linchange", ssl: "disable", ok: true},
		{in: "", ok: false},
		{in: "mysql://localhost/db", ok: false},
	}
	for _, tc := range tests {
		host, port, name, user, ssl, ok := parseDatabaseURL(tc.in)
		if ok != tc.ok {
			t.Errorf("parseDatabaseURL(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if !tc.ok {
			continue
		}
		if host != tc.host || port != tc.port || name != tc.name || user != tc.user || ssl != tc.ssl {
			t.Errorf("parseDatabaseURL(%q) = (%q, %d, %q, %q, %q), want (%q, %d, %q, %q, %q)",
				tc.in, host, port, name, user, ssl, tc.host, tc.port, tc.name, tc.user, tc.ssl)
		}
	}
}

func TestPortNumber(t *testing.T) {
	if got := portNumber(":8080", 9999); got != 8080 {
		t.Errorf("portNumber(:8080) = %d", got)
	}
	if got := portNumber("", 9999); got != 9999 {
		t.Errorf("portNumber() = %d, want the fallback", got)
	}
	if got := portNumber(":http", 9999); got != 9999 {
		t.Errorf("portNumber(:http) = %d, want the fallback", got)
	}
}

func TestApplyProtocolsStartsFromOff(t *testing.T) {
	// Passing --protocols is the complete statement of what should listen, so
	// it cannot be an addition to whatever the defaults happened to be.
	a := setup.Defaults()
	a.IMAP = true
	if err := applyProtocols(&a, "smtp-submit,imap"); err != nil {
		t.Fatalf("applyProtocols: %v", err)
	}
	if !a.SMTPSubmission || !a.IMAP {
		t.Errorf("protocols = %+v, want submission and imap on", a)
	}
	if a.SMTPInbound || a.POP3 || a.IMAPS || a.POP3S {
		t.Errorf("protocols = %+v, want everything else off", a)
	}
}

func TestApplyProtocolsAliases(t *testing.T) {
	a := setup.Defaults()
	if err := applyProtocols(&a, "smtp,submit,pop3s"); err != nil {
		t.Fatalf("applyProtocols: %v", err)
	}
	if !a.SMTPInbound || !a.SMTPSubmission || !a.POP3 || !a.POP3S {
		t.Errorf("protocols = %+v, want every alias honoured", a)
	}
}

func TestApplyProtocolsRejectsUnknownNames(t *testing.T) {
	a := setup.Defaults()
	// "smtp" is an accepted alias for smtp-in; "mttp" is not a protocol.
	err := applyProtocols(&a, "imap,mttp")
	if err == nil {
		t.Fatal("applyProtocols = nil, want the misspelling to be refused")
	}
	// The error has to name the valid options, or a typo costs a lookup.
	if !strings.Contains(err.Error(), "smtp-in") {
		t.Errorf("error = %v, want it to list the valid names", err)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" IMAP, ,pop3 ,EAS ")
	want := []string{"imap", "pop3", "eas"}
	if len(got) != len(want) {
		t.Fatalf("splitList = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAnswersFromConfigRoundTripsTheWizardOutput(t *testing.T) {
	// A second run of the wizard must show what is deployed, not the
	// defaults; this is the mapping that makes that true.
	answers := setup.Defaults()
	answers.Hostname = "mail.example.com"
	answers.PublicIP = "203.0.113.10"
	answers.Domain = "example.com"
	answers.TLSMode = setup.TLSACME
	answers.ACMEEmail = "ops@example.com"
	answers.IMAP, answers.IMAPS = true, true
	answers.SMTPInbound, answers.SMTPSubmission = true, true
	// POP3 is off in this deployment, so the round trip must not bring it
	// back on.
	answers.POP3, answers.POP3S = false, false
	answers.Exchange = true
	answers.Organization = "Example Ltd"
	answers.AdminEmail = "admin@example.com"
	answers.AdminPassword = "correct horse battery staple"

	cfg, _, err := setup.BuildConfig(nil, answers)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}

	got := answersFromConfig(cfg, setup.Defaults())
	for _, check := range []struct {
		name      string
		got, want any
	}{
		{"hostname", got.Hostname, answers.Hostname},
		{"public ip", got.PublicIP, answers.PublicIP},
		{"db host", got.DBHost, answers.DBHost},
		{"db port", got.DBPort, answers.DBPort},
		{"db name", got.DBName, answers.DBName},
		{"db user", got.DBUser, answers.DBUser},
		{"db sslmode", got.DBSSLMode, answers.DBSSLMode},
		{"imap", got.IMAP, true},
		{"imaps", got.IMAPS, true},
		{"smtp inbound", got.SMTPInbound, true},
		{"smtp submission", got.SMTPSubmission, true},
		{"web port", got.WebPort, answers.WebPort},
		{"https port", got.HTTPSPort, 0},
		{"tls mode", got.TLSMode, setup.TLSACME},
		{"acme email", got.ACMEEmail, answers.ACMEEmail},
		{"exchange", got.Exchange, true},
		{"organization", got.Organization, answers.Organization},
		{"log level", got.LogLevel, answers.LogLevel},
	} {
		if check.got != check.want {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}
	// POP3 was never enabled, so it must not come back enabled.
	if got.POP3 || got.POP3S {
		t.Errorf("pop3 = %v/%v, want both off", got.POP3, got.POP3S)
	}
}

func TestIsNotExist(t *testing.T) {
	_, err := os.Stat(filepath.Join(t.TempDir(), "definitely-not-here.yaml"))
	if !isNotExist(err) {
		t.Errorf("isNotExist = false for a missing file: %v", err)
	}
	if isNotExist(nil) {
		t.Error("isNotExist = true for nil")
	}
	if isNotExist(errors.New("connection refused")) {
		t.Error("isNotExist = true for an unrelated error")
	}
}

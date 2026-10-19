package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/setup"
	"github.com/linchange/linchange/internal/tui"
)

// defaultConfigPath is where the Debian package installs the configuration,
// and what the postinst passes to the wizard.
const defaultConfigPath = "/etc/linchange/linchange.yaml"

// runSetup is the interactive first-run wizard, and its unattended twin.
//
// With a terminal it asks everything worth asking. With --non-interactive (or
// without a terminal on stdin) the answers come from flags and the same code
// path writes the file, which is what lets the Debian postinst offer the
// wizard without ever blocking an automated install.
func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `linchange setup - first-run configuration wizard

Usage:
  linchange setup --config <path.yaml> [flags]

With no flags and a terminal, the wizard asks each question in turn. Every
flag below pre-fills the corresponding answer, and with --non-interactive no
question is asked at all: the flags and the built-in defaults are the answers,
and anything required that is missing is reported as an error.

Examples:
  # Interactive
  linchange setup --config /etc/linchange/linchange.yaml

  # Unattended, as the Debian package does when there is no terminal
  linchange setup --config /etc/linchange/linchange.yaml --non-interactive \
      --hostname mail.example.com --public-ip 203.0.113.10 \
      --db-user linchange --db-password "$DB_PASSWORD" \
      --domain example.com --tls acme --acme-email me@example.com \
      --admin-email admin@example.com --admin-password-stdin < passwords.txt

Flags:
`)
		fs.PrintDefaults()
	}

	var (
		configPath  = fs.String("config", defaultConfigPath, "configuration file to write")
		headsUp     = fs.Bool("non-interactive", false, "ask nothing; use the flags and defaults (implied when stdin is not a terminal)")
		noProvision = fs.Bool("no-provision", false, "write the configuration but leave the database alone")

		hostname = fs.String("hostname", "", "server hostname, e.g. mail.example.com")
		publicIP = fs.String("public-ip", "", "public IPv4 address of this server")
		dataDir  = fs.String("data-dir", "", "data directory (default /var/lib/linchange)")

		dbHost     = fs.String("db-host", "", "PostgreSQL host")
		dbPort     = fs.Int("db-port", 0, "PostgreSQL port")
		dbName     = fs.String("db-name", "", "PostgreSQL database name")
		dbUser     = fs.String("db-user", "", "PostgreSQL role")
		dbPassword = fs.String("db-password", "", "password of the PostgreSQL role")
		dbPassStd  = fs.Bool("db-password-stdin", false, "read the database password from the first line of stdin")
		dbSSLMode  = fs.String("db-sslmode", "", "disable, require or verify-full")
		noMigrate  = fs.Bool("no-migrate", false, "do not apply database migrations")

		domain    = fs.String("domain", "", "mail domain to host, e.g. example.com")
		relayOnly = fs.Bool("relay-only", false, "do not deliver mail for the domain here (another system holds the mailboxes)")

		protocols = fs.String("protocols", "", "comma list: smtp-in,smtp-submit,imap,imaps,pop3,pop3s (default: smtp-in,smtp-submit,imap)")
		webPort   = fs.Int("web-port", 0, "web interface port")
		httpsPort = fs.Int("https-port", 0, "HTTPS port; 0 disables direct HTTPS")
		noHTTPS   = fs.Bool("no-https", false, "do not serve HTTPS on 443 even with certificates")

		tlsMode   = fs.String("tls", "", "none, acme or certificates")
		acmeEmail = fs.String("acme-email", "", "email for Let's Encrypt expiry warnings")
		certFile  = fs.String("cert-file", "", "path to a PEM certificate")
		keyFile   = fs.String("key-file", "", "path to the matching PEM private key")

		exchange     = fs.String("exchange", "", "enable the Exchange endpoints: true or false")
		endpoints    = fs.String("exchange-endpoints", "", "comma list: ews,autodiscover,eas")
		organization = fs.String("organization", "", "organization name reported to Exchange clients")

		adminEmail    = fs.String("admin-email", "", "address of the first administrator")
		adminName     = fs.String("admin-name", "", "display name of the first administrator")
		adminPassword = fs.String("admin-password", "", "password of the first administrator")
		adminPassStd  = fs.Bool("admin-password-stdin", false, "read the administrator password from the first line of stdin")
		mustChange    = fs.Bool("must-change-password", false, "require the administrator to change the password at first login")

		logLevel = fs.String("log-level", "", "debug, info, warn or error")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	// Pre-fill from what is already on disk, so that re-running the wizard
	// shows last time's answers instead of the built-in defaults.
	answers := setup.Defaults()
	base := config.New()
	if existing, err := config.Load(*configPath); err == nil {
		base = existing
		answers = answersFromConfig(existing, answers)
	} else if !isNotExist(err) {
		// A file that is there but broken is worth saying out loud: the wizard
		// is about to write a new one over a backed-up copy of it.
		fmt.Fprintf(os.Stderr, "note: %s exists but does not load (%v); starting from the defaults\n", *configPath, err)
	}

	if err := applyFlags(&answers, flagValues{
		hostname: *hostname, publicIP: *publicIP, dataDir: *dataDir,
		dbHost: *dbHost, dbPort: *dbPort, dbName: *dbName, dbUser: *dbUser,
		dbPassword: *dbPassword, dbPassStdin: *dbPassStd, dbSSLMode: *dbSSLMode,
		domain: *domain, relayOnly: *relayOnly,
		protocols: *protocols, webPort: *webPort, httpsPort: *httpsPort, noHTTPS: *noHTTPS,
		tlsMode: *tlsMode, acmeEmail: *acmeEmail, certFile: *certFile, keyFile: *keyFile,
		exchange: *exchange, endpoints: *endpoints, organization: *organization,
		adminEmail: *adminEmail, adminName: *adminName,
		adminPassword: *adminPassword, adminPassStdin: *adminPassStd, mustChange: *mustChange,
		logLevel:  *logLevel,
		noMigrate: *noMigrate,
	}); err != nil {
		return err
	}

	opts := setup.Options{
		Path:      *configPath,
		Base:      base,
		Answers:   answers,
		Provision: !*noProvision,
	}

	// Without a terminal there is nobody to ask. That is not an error in
	// itself - it is what an unattended install looks like - but it is an
	// error to reach it without having supplied everything the wizard would
	// have asked.
	if *headsUp || !isTerminal(os.Stdin) {
		if err := answers.Check(); err != nil {
			return fmt.Errorf("these answers are incomplete, and there is no terminal to ask:\n  - %w\n\n"+
				"Pass the missing flags, or run the wizard from a terminal.", err)
		}
		ui := tui.New(os.Stdin, os.Stdout)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_, err := setup.Apply(ctx, ui, opts)
		return err
	}

	ui := tui.New(os.Stdin, os.Stdout)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	_, err := setup.Run(ctx, ui, opts)
	if errors.Is(err, tui.ErrInterrupt) {
		// An interrupted wizard is a cancelled one, not a failed install.
		fmt.Fprintln(os.Stderr, "\nsetup cancelled; nothing further was changed")
		return nil
	}
	return err
}

// flagValues carries the parsed flags into applyFlags, which is separate only
// to keep runSetup readable: the mapping from flag to answer is mechanical and
// long, and it reads better away from the argument list.
type flagValues struct {
	hostname, publicIP, dataDir                   string
	dbHost, dbName, dbUser, dbPassword, dbSSLMode string
	dbPort                                        int
	dbPassStdin                                   bool
	domain                                        string
	relayOnly                                     bool
	protocols                                     string
	webPort, httpsPort                            int
	noHTTPS                                       bool
	tlsMode, acmeEmail, certFile, keyFile         string
	exchange, endpoints, organization             string
	adminEmail, adminName, adminPassword          string
	adminPassStdin, mustChange                    bool
	logLevel                                      string
	noMigrate                                     bool
}

// applyFlags overlays the flags the operator actually passed onto the answers.
// A flag left at its zero value must not overwrite a default, which is why
// every string flag is tested for emptiness and every number for zero.
func applyFlags(a *setup.Answers, v flagValues) error {
	set := func(dst *string, value string) {
		if value != "" {
			*dst = value
		}
	}

	set(&a.Hostname, v.hostname)
	set(&a.PublicIP, v.publicIP)
	set(&a.DataDir, v.dataDir)

	set(&a.DBHost, v.dbHost)
	if v.dbPort != 0 {
		a.DBPort = v.dbPort
	}
	set(&a.DBName, v.dbName)
	set(&a.DBUser, v.dbUser)
	set(&a.DBSSLMode, v.dbSSLMode)
	switch {
	case v.dbPassStdin:
		line, err := readSecret(os.Stdin, "")
		if err != nil {
			return fmt.Errorf("reading the database password from stdin: %w", err)
		}
		a.DBPassword = line
	case v.dbPassword != "":
		a.DBPassword = v.dbPassword
	}
	if v.noMigrate {
		a.AutoMigr = false
	}

	set(&a.Domain, v.domain)
	if v.relayOnly {
		a.AcceptMail = false
	}

	if v.protocols != "" {
		if err := applyProtocols(a, v.protocols); err != nil {
			return err
		}
	}
	if v.webPort != 0 {
		a.WebPort = v.webPort
	}
	if v.noHTTPS {
		a.HTTPSPort = 0
	} else if v.httpsPort != 0 {
		a.HTTPSPort = v.httpsPort
	}

	if v.tlsMode != "" {
		switch v.tlsMode {
		case setup.TLSNone, setup.TLSACME, setup.TLSCert:
			a.TLSMode = v.tlsMode
		default:
			return fmt.Errorf("--tls %q must be one of none, acme, certificates", v.tlsMode)
		}
	}
	set(&a.ACMEEmail, v.acmeEmail)
	set(&a.CertFile, v.certFile)
	set(&a.KeyFile, v.keyFile)

	if v.exchange != "" {
		on, err := strconv.ParseBool(v.exchange)
		if err != nil {
			return fmt.Errorf("--exchange %q must be true or false", v.exchange)
		}
		a.Exchange = on
	}
	if v.endpoints != "" {
		for _, name := range splitList(v.endpoints) {
			switch name {
			case "ews":
				a.EWS = true
			case "autodiscover":
				a.Autodiscover = true
			case "eas", "activesync":
				a.EAS = true
			default:
				return fmt.Errorf("--exchange-endpoints: unknown endpoint %q (ews, autodiscover, eas)", name)
			}
		}
	}
	set(&a.Organization, v.organization)

	set(&a.AdminEmail, v.adminEmail)
	set(&a.AdminName, v.adminName)
	switch {
	case v.adminPassStdin:
		line, err := readSecret(os.Stdin, "")
		if err != nil {
			return fmt.Errorf("reading the administrator password from stdin: %w", err)
		}
		a.AdminPassword = line
	case v.adminPassword != "":
		a.AdminPassword = v.adminPassword
	}
	if v.mustChange {
		a.MustChangePassword = true
	}

	set(&a.LogLevel, v.logLevel)
	return nil
}

// applyProtocols sets the protocol switches from a comma list.
func applyProtocols(a *setup.Answers, list string) error {
	// Start from off, so that passing the flag is the complete statement of
	// what should listen rather than an addition to an unknown default.
	a.SMTPInbound, a.SMTPSubmission, a.IMAP, a.IMAPS, a.POP3, a.POP3S = false, false, false, false, false, false
	for _, name := range splitList(list) {
		switch name {
		case "smtp-in", "smtp":
			a.SMTPInbound = true
		case "smtp-submit", "submission", "submit":
			a.SMTPSubmission = true
		case "imap":
			a.IMAP = true
		case "imaps":
			a.IMAP, a.IMAPS = true, true
		case "pop3":
			a.POP3 = true
		case "pop3s":
			a.POP3, a.POP3S = true, true
		case "all":
			a.SMTPInbound, a.SMTPSubmission, a.IMAP, a.IMAPS, a.POP3, a.POP3S = true, true, true, true, true, true
		default:
			return fmt.Errorf("--protocols: unknown protocol %q (smtp-in, smtp-submit, imap, imaps, pop3, pop3s, all)", name)
		}
	}
	return nil
}

// splitList trims a comma list and drops empty entries.
func splitList(list string) []string {
	var out []string
	for _, part := range strings.Split(list, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, strings.ToLower(part))
		}
	}
	return out
}

// answersFromConfig pre-fills the wizard from an existing configuration, so a
// second run shows what is actually deployed instead of the defaults.
func answersFromConfig(cfg *config.Config, fallback setup.Answers) setup.Answers {
	a := fallback
	a.Hostname = cfg.Server.Hostname
	a.PublicIP = cfg.Server.PublicIP
	a.DataDir = cfg.Server.DataDir

	if host, port, name, user, ssl, ok := parseDatabaseURL(cfg.Database.URL); ok {
		a.DBHost, a.DBPort, a.DBName, a.DBUser, a.DBSSLMode = host, port, name, user, ssl
	}
	a.AutoMigr = cfg.Database.AutoMigrate

	a.IMAP = cfg.Listener.IMAP.Addr != ""
	a.IMAPS = cfg.Listener.IMAPS.Addr != ""
	a.POP3 = cfg.Listener.POP3.Addr != ""
	a.POP3S = cfg.Listener.POP3S.Addr != ""
	a.SMTPInbound = cfg.Listener.SMTPInbound.Addr != ""
	a.SMTPSubmission = cfg.Listener.SMTPSubmission.Addr != ""
	a.WebPort = portNumber(cfg.Listener.HTTP.Addr, fallback.WebPort)
	a.HTTPSPort = portNumber(cfg.Listener.HTTPS.Addr, 0)

	switch {
	case cfg.TLS.ACME.Enabled:
		a.TLSMode, a.ACMEEmail = setup.TLSACME, cfg.TLS.ACME.Email
	case cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "":
		a.TLSMode, a.CertFile, a.KeyFile = setup.TLSCert, cfg.TLS.CertFile, cfg.TLS.KeyFile
	default:
		a.TLSMode = setup.TLSNone
	}

	a.Exchange = cfg.Exchange.Enabled
	a.EWS = cfg.Exchange.EWS.Enabled
	a.Autodiscover = cfg.Exchange.Autodiscover.Enabled
	a.EAS = cfg.Exchange.EAS.Enabled
	a.Organization = cfg.Exchange.Organization

	a.LogLevel = cfg.Log.Level
	return a
}

// parseDatabaseURL pulls the parts the wizard asks about out of a libpq URL.
// Anything it cannot read is reported as such, and the wizard then asks.
func parseDatabaseURL(raw string) (host string, port int, name, user, ssl string, ok bool) {
	// Deliberately not parsed with net/url: the URL may carry query
	// parameters in libpq's own syntax, and the wizard only needs the five
	// values it already knows how to ask for.
	if !strings.HasPrefix(raw, "postgres") {
		return "", 0, "", "", "", false
	}
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		if strings.HasPrefix(rest[i:], "?") {
			for _, param := range strings.Split(rest[i+1:], "&") {
				if strings.HasPrefix(param, "sslmode=") {
					ssl = strings.TrimPrefix(param, "sslmode=")
				}
			}
		}
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		if u := rest[:i]; u != "" {
			user = u
			if j := strings.Index(u, ":"); j >= 0 {
				user = u[:j]
			}
		}
		rest = rest[i+1:]
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		name = rest[i+1:]
		rest = rest[:i]
	}
	if strings.HasPrefix(rest, "[") {
		// An IPv6 literal is bracketed, and its colons are part of the
		// address rather than a host/port separator.
		if end := strings.Index(rest, "]"); end >= 0 {
			host = rest[:end+1]
			if tail := strings.TrimPrefix(rest[end+1:], ":"); tail != rest[end+1:] {
				if p, err := strconv.Atoi(tail); err == nil {
					port = p
				}
			}
		}
	} else if i := strings.Index(rest, ":"); i >= 0 {
		host = rest[:i]
		if p, err := strconv.Atoi(rest[i+1:]); err == nil {
			port = p
		}
	} else {
		host = rest
	}
	if host == "" || name == "" {
		return "", 0, "", "", "", false
	}
	if user == "" {
		user = "postgres"
	}
	if port == 0 {
		port = 5432
	}
	if ssl == "" {
		ssl = "disable"
	}
	return host, port, name, user, ssl, true
}

// portNumber reads the port out of a ":8080" listener address.
func portNumber(addr string, fallback int) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return fallback
	}
	n, err := strconv.Atoi(addr[i+1:])
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// isNotExist reports whether err is the file being absent. config.Load wraps
// the os error, so the string is checked as well as the unwrapped chain.
func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	if os.IsNotExist(err) {
		return true
	}
	return strings.Contains(err.Error(), "no such file or directory")
}

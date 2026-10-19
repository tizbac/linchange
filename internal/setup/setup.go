// Package setup is the first-run configuration wizard.
//
// A mail server has a long tail of settings that all have to agree with each
// other before anything starts: a hostname that matches a public address, a
// database that answers, a domain to deliver to, listeners that are only bound
// where a certificate exists, and three separate encryption keys that are
// required before the first user can log in. Writing that file by hand is the
// part of running your own mail server that people get wrong, so this package
// asks for the few things only an operator knows and derives the rest.
//
// The wizard is split in three so that each part can be tested on its own:
// Answers collects the choices, BuildConfig turns them into a validated
// configuration, and Provision makes the database match it. The UI is an
// interface so the flow can be driven by a script in tests and by a human (or
// nobody) in production.
package setup

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/linchange/linchange/config"
)

// Steps is the number of screens the wizard shows. It is used in the
// "Step 2 of 7" headings and has to match the flow below.
const Steps = 7

// TLS modes offered by the wizard.
const (
	TLSNone = "none"
	TLSACME = "acme"
	TLSCert = "certificates"
)

// Answers is everything the wizard decides. It is a flat struct on purpose:
// the questions are grouped on screen, but a flat value is what the command
// line flags and the tests fill in, and nested structs would only add
// boilerplate at every call site.
type Answers struct {
	// Identity, as the rest of the world will see it.
	Hostname string
	PublicIP string
	DataDir  string

	// PostgreSQL. The URL is assembled from these, because asking a newcomer
	// for a connection string up front gets them a copy-paste of somebody
	// else's.
	DBHost     string
	DBPort     int
	DBName     string
	DBUser     string
	DBPassword string
	DBSSLMode  string
	AutoMigr   bool

	// The mail domain hosted here, and whether mail for it is delivered by
	// this server rather than merely relayed.
	Domain     string
	AcceptMail bool

	// Listeners and protocols.
	WebPort        int
	HTTPSPort      int
	IMAP           bool
	IMAPS          bool
	POP3           bool
	POP3S          bool
	SMTPInbound    bool
	SMTPSubmission bool
	Exchange       bool
	EWS            bool
	Autodiscover   bool
	EAS            bool
	Organization   string

	// TLS.
	TLSMode   string
	ACMEEmail string
	CertFile  string
	KeyFile   string

	// The first administrator.
	AdminEmail         string
	AdminName          string
	AdminPassword      string
	MustChangePassword bool

	LogLevel string
}

// Defaults answers the wizard would give if the operator pressed enter at
// every prompt. They are safe on a private network: nothing is exposed that
// needs a certificate, and the database stays on loopback.
func Defaults() Answers {
	host, _ := os.Hostname()
	return Answers{
		Hostname: host,
		PublicIP: GuessPublicIP(),
		DataDir:  "/var/lib/linchange",

		DBHost:    "127.0.0.1",
		DBPort:    5432,
		DBName:    "linchange",
		DBUser:    "linchange",
		DBSSLMode: "disable",
		AutoMigr:  true,

		AcceptMail: true,

		WebPort:      8080,
		IMAP:         true,
		POP3:         true,
		EWS:          true,
		Autodiscover: true,
		EAS:          true,

		TLSMode: TLSNone,

		LogLevel: "info",
	}
}

// DBURL assembles the PostgreSQL connection string from the parts.
//
// The password is escaped because a generated or pasted one regularly contains
// characters that have meaning in a URL, and an unescaped @ silently changes
// which host is contacted.
func (a Answers) DBURL() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(a.DBUser, a.DBPassword),
		Host:   net.JoinHostPort(a.DBHost, strconv.Itoa(a.DBPort)),
		Path:   "/" + a.DBName,
	}
	q := url.Values{}
	if a.DBSSLMode != "" {
		q.Set("sslmode", a.DBSSLMode)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// TLSEnabled reports whether the wizard chose a certificate mode.
func (a Answers) TLSEnabled() bool { return a.TLSMode != TLSNone }

// Check reports the answers that are missing or contradictory, before
// anything is written. It is what makes `--non-interactive` honest: a flag
// forgotten is an error here rather than an invalid configuration file later.
func (a Answers) Check() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if strings.TrimSpace(a.Hostname) == "" {
		add("server hostname is required (--hostname)")
	}
	if strings.TrimSpace(a.PublicIP) == "" {
		add("public IP address is required (--public-ip)")
	}
	if strings.TrimSpace(a.DataDir) == "" {
		add("data directory is required (--data-dir)")
	}
	if strings.TrimSpace(a.Domain) == "" {
		add("mail domain is required (--domain)")
	} else if strings.ContainsAny(a.Domain, " /\\@:") {
		add("mail domain %q must be a bare name such as example.com", a.Domain)
	}
	if a.DBHost == "" || a.DBName == "" || a.DBUser == "" {
		add("database host, name and user are required (--db-host, --db-name, --db-user)")
	}
	if a.DBPort < 1 || a.DBPort > 65535 {
		add("database port %d is out of range", a.DBPort)
	}
	switch a.DBSSLMode {
	case "disable", "require", "verify-ca", "verify-full", "prefer":
	default:
		add("database sslmode %q must be one of disable, require, verify-ca, verify-full, prefer", a.DBSSLMode)
	}
	if !a.SMTPInbound && !a.SMTPSubmission && !a.IMAP && !a.POP3 {
		add("at least one mail protocol must be enabled")
	}
	if a.TLSMode == TLSACME && strings.TrimSpace(a.ACMEEmail) == "" {
		add("ACME needs an email address for expiry warnings (--acme-email)")
	}
	if a.TLSMode == TLSCert && (a.CertFile == "" || a.KeyFile == "") {
		add("certificate mode needs --cert-file and --key-file")
	}
	if strings.TrimSpace(a.AdminEmail) == "" {
		add("administrator address is required (--admin-email)")
	}
	if strings.TrimSpace(a.AdminPassword) == "" {
		add("administrator password is required (--admin-password or --admin-password-stdin)")
	}
	if a.Exchange && strings.TrimSpace(a.Organization) == "" {
		add("the Exchange endpoints need an organization name (--organization)")
	}
	switch a.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("log level %q must be one of debug, info, warn, error", a.LogLevel)
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "\n  - "))
}

// BuildConfig turns answers into a complete, validated configuration. base is
// the configuration already on disk, or config.New() for a fresh install; the
// answers overwrite only the settings the wizard asks about, so re-running it
// does not quietly revert an edit made by hand afterwards.
func BuildConfig(base *config.Config, a Answers) (*config.Config, []string, error) {
	if base == nil {
		base = config.New()
	}
	cfg := *base
	// Slices are shared with base after the struct copy; the wizard never
	// appends to them, but copying keeps the two documents independent.
	cfg.Server.TrustedProxies = append([]string(nil), base.Server.TrustedProxies...)
	cfg.LDAP.Providers = append([]config.LDAPProvider(nil), base.LDAP.Providers...)

	cfg.Server.Hostname = a.Hostname
	cfg.Server.PublicIP = a.PublicIP
	// The data directory is owned by the linchange user; a relative default
	// would land in whatever directory the service happened to start from.
	cfg.Server.DataDir = a.DataDir

	cfg.Database.URL = a.DBURL()
	cfg.Database.AutoMigrate = a.AutoMigr

	cfg.Listener.HTTP.Addr = port(a.WebPort)
	cfg.Listener.SMTPInbound.Addr = choose(a.SMTPInbound, ":25")
	cfg.Listener.SMTPSubmission.Addr = choose(a.SMTPSubmission, ":587")
	cfg.Listener.IMAP.Addr = choose(a.IMAP, ":143")
	cfg.Listener.POP3.Addr = choose(a.POP3, ":110")

	// The four TLS-only listeners are only bound when there is a certificate
	// to serve, and the validator refuses them otherwise: binding 993 without
	// one would advertise TLS and then fail the handshake. They are cleared
	// first, because the base configuration may be a previous run that did
	// have a certificate and the operator has just turned TLS off.
	var warnings []string
	cfg.Listener.IMAPS.Addr = ""
	cfg.Listener.POP3S.Addr = ""
	cfg.Listener.SMTPSubmissionTLS.Addr = ""
	cfg.Listener.HTTPS.Addr = ""
	if a.TLSEnabled() {
		cfg.Listener.IMAPS.Addr = choose(a.IMAP && a.IMAPS, ":993")
		cfg.Listener.POP3S.Addr = choose(a.POP3 && a.POP3S, ":995")
		cfg.Listener.SMTPSubmissionTLS.Addr = choose(a.SMTPSubmission, ":465")
		cfg.Listener.HTTPS.Addr = choose(a.HTTPSPort > 0, port(a.HTTPSPort))
	} else {
		if a.IMAPS {
			warnings = append(warnings, "IMAPS (993) is off: it needs a certificate, and none is configured")
		}
		if a.POP3S {
			warnings = append(warnings, "POP3S (995) is off: it needs a certificate, and none is configured")
		}
		if a.HTTPSPort > 0 {
			warnings = append(warnings, "HTTPS is off: it needs a certificate, and none is configured")
		}
	}

	cfg.Mail.Inbound.Enabled = a.SMTPInbound

	// Submission over 587 starts in plaintext so that a client can upgrade
	// with STARTTLS; require_tls then refuses to accept a password on a
	// connection that did not.
	cfg.Mail.Submission.RequireTLS = a.TLSEnabled()
	cfg.Mail.Submission.AllowInsecureAuth = false

	cfg.TLS.CertFile = ""
	cfg.TLS.KeyFile = ""
	cfg.TLS.ACME = config.ACME{Enabled: false}
	switch a.TLSMode {
	case TLSCert:
		cfg.TLS.CertFile = a.CertFile
		cfg.TLS.KeyFile = a.KeyFile
	case TLSACME:
		cfg.TLS.ACME = config.ACME{
			Enabled:  true,
			Email:    a.ACMEEmail,
			Domains:  []string{a.Hostname},
			CacheDir: cfg.Dir("acme"),
		}
		// Redirecting HTTP to HTTPS is what makes an ACME HTTP-01 challenge
		// and a client that guessed http:// both work.
		cfg.TLS.RedirectHTTP = true
	default:
		cfg.TLS.RedirectHTTP = false
	}

	cfg.Exchange.Enabled = a.Exchange
	cfg.Exchange.EWS.Enabled = a.EWS
	cfg.Exchange.Autodiscover.Enabled = a.Autodiscover
	cfg.Exchange.EAS.Enabled = a.EAS
	cfg.Exchange.Organization = a.Organization

	cfg.Log.Level = a.LogLevel

	// Bounces are generated by the server itself, and the envelope sender of
	// a generated bounce has to be an address at a domain this installation
	// controls: remote servers reject a DSN that claims to come from
	// somewhere else. postmaster is the conventional choice, and RFC 5321
	// requires it to be monitored anyway, so nothing is silently dropped.
	if cfg.Mail.Outbound.DSN.Enabled && cfg.Mail.Outbound.DSN.FromAddress == "" && a.Domain != "" {
		cfg.Mail.Outbound.DSN.FromAddress = "postmaster@" + a.Domain
	}

	if err := EnsureSecrets(&cfg); err != nil {
		return nil, warnings, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, warnings, err
	}
	return &cfg, warnings, nil
}

// EnsureSecrets fills in the three keys a deployment cannot invent for
// itself. Every one of them is required before the first login or the first
// outbound message, and all three are the sort of thing a tutorial tells you
// to generate with openssl; a wizard that does it with the system CSPRNG has
// removed a whole category of copy-paste mistake.
func EnsureSecrets(cfg *config.Config) error {
	generate := func() (string, error) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", fmt.Errorf("generating a random key: %w", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw), nil
	}
	if cfg.Auth.JWT.Secret == "" {
		secret, err := generate()
		if err != nil {
			return err
		}
		cfg.Auth.JWT.Secret = secret
	}
	if cfg.Auth.EncryptionKey == "" {
		key, err := generate()
		if err != nil {
			return err
		}
		cfg.Auth.EncryptionKey = key
	}
	if cfg.Mail.DKIM.SignAll && cfg.Mail.DkimPrivateKeyEncryptionKey == "" {
		key, err := generate()
		if err != nil {
			return err
		}
		cfg.Mail.DkimPrivateKeyEncryptionKey = key
	}
	return nil
}

// GuessPublicIP returns the address this host would send from, which is the
// answer most people want for server.public_ip. It is a guess, not a
// discovery: behind NAT it will be a private address, which the wizard says
// out loud rather than writing into the configuration unchallenged.
func GuessPublicIP() string {
	conn, err := net.DialTimeout("udp", "8.8.8.8:53", 1500*time.Millisecond)
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil || addr.IP.IsUnspecified() {
		return "127.0.0.1"
	}
	return addr.IP.String()
}

// choose returns addr when on, and the empty listener otherwise. An empty addr
// is how this configuration disables a listener, so the choice of protocol is
// expressed as a bound address rather than a separate flag.
func choose(on bool, addr string) string {
	if on {
		return addr
	}
	return ""
}

// port renders a port number as a listener address bound on every interface.
func port(n int) string {
	if n <= 0 {
		return ""
	}
	return ":" + strconv.Itoa(n)
}

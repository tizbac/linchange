package setup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/admin"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/logging"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/tui"
)

// UI is what the wizard needs from a terminal. The real implementation is
// internal/tui; tests substitute a scripted one, and an unattended install
// never gets here at all.
type UI interface {
	Section(step, total int, title, blurb string)
	Hint(text string)
	Info(format string, args ...any)
	Detail(format string, args ...any)
	Success(format string, args ...any)
	Warn(format string, args ...any)
	Error(format string, args ...any)
	KeyValue(key, value string)
	Progress(label string) func(error)
	Input(q string, o tui.InputOpts) (string, error)
	Confirm(q string, def bool, hint string) (bool, error)
	Select(q string, choices []tui.Choice, def int, hint string) (int, error)
	Multi(q string, choices []tui.Choice, def []bool, hint string) ([]bool, error)
}

// Options configure one wizard run.
type Options struct {
	// Path is the configuration file to write.
	Path string
	// Base is the configuration already at Path, or config.New() when there
	// is none. Answers pre-fill from it, so re-running the wizard keeps the
	// values it wrote last time.
	Base *config.Config
	// Answers are the starting values, typically from the command line.
	Answers Answers
	// Provision makes the database match the configuration: migrations, the
	// domain and the first administrator.
	Provision bool
}

// Result reports what a run did, for the caller to print or assert on.
type Result struct {
	ConfigPath string
	BackupPath string
	Answers    Answers
	// Provisioned is true when the database was touched.
	Provisioned bool
	// AdminCreated is false when the administrator already existed and was
	// left alone.
	AdminCreated bool
}

// Run drives the whole wizard.
//
// An invalid answer loops on the spot, and nothing is written until the last
// screen is confirmed, so interrupting the wizard at any point costs at most
// the typing.
func Run(ctx context.Context, ui UI, opts Options) (*Result, error) {
	if opts.Base == nil {
		opts.Base = config.New()
	}
	a := opts.Answers

	ui.Section(0, 0, "linchange setup",
		"This wizard writes "+opts.Path+" and prepares the database. Every "+
			"question has a sensible default: press Enter to accept it. Run "+
			"it again at any time and it keeps what it wrote before.")

	steps := []func() error{
		func() error { return stepIdentity(ui, &a) },
		func() error { return stepDatabase(ctx, ui, &a, opts.Provision) },
		func() error { return stepDomain(ui, &a) },
		func() error { return stepTLS(ui, &a) },
		func() error { return stepProtocols(ui, &a) },
		func() error { return stepAdmin(ui, &a, opts.Base.Auth.Local.MinPasswordLength) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}

	cfg, warnings, err := BuildConfig(opts.Base, a)
	if err != nil {
		ui.Error("those answers do not make a valid configuration:")
		for _, line := range strings.Split(err.Error(), "\n") {
			if trimmed := strings.TrimPrefix(line, "  - "); trimmed != "" {
				ui.Detail("%s", trimmed)
			}
		}
		return nil, err
	}

	if err := confirmReview(ui, cfg, a, warnings, opts); err != nil {
		return nil, err
	}

	result, err := Apply(ctx, ui, Options{
		Path:      opts.Path,
		Base:      opts.Base,
		Answers:   a,
		Provision: opts.Provision,
	})
	if err != nil {
		return result, err
	}

	ui.Section(0, 0, "Done", "")
	ui.KeyValue("Web interface", webURL(a))
	ui.KeyValue("Configuration", opts.Path)
	ui.KeyValue("Logs", "journalctl -u linchange -f")
	ui.Info("start the service with: systemctl enable --now linchange")
	return result, nil
}

// Apply writes the configuration for the given answers and optionally makes
// the database match it. It is the unattended half of the wizard: the command
// uses it directly when every answer came from a flag, so that an automated
// install and a human one cannot drift apart in what they produce.
func Apply(ctx context.Context, ui UI, opts Options) (*Result, error) {
	if opts.Base == nil {
		opts.Base = config.New()
	}
	cfg, warnings, err := BuildConfig(opts.Base, opts.Answers)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		ui.Warn("%s", w)
	}

	backup, err := WriteFile(opts.Path, cfg)
	if err != nil {
		ui.Error("could not write %s: %v", opts.Path, err)
		return nil, err
	}
	ui.Success("wrote %s", opts.Path)
	if backup != "" {
		ui.Detail("the previous file was saved as %s", backup)
	}

	result := &Result{ConfigPath: opts.Path, BackupPath: backup, Answers: opts.Answers}
	if opts.Provision {
		done := ui.Progress("Connecting to the database")
		db, err := store.OpenFromConfig(ctx, cfg, "linchange-setup")
		if err != nil {
			done(err)
			return result, err
		}
		defer db.Close()
		if err := store.Health(ctx, db); err != nil {
			done(err)
			return result, err
		}
		done(nil)

		if err := Provision(ctx, ui, db, cfg, opts.Answers, result); err != nil {
			return result, err
		}
		result.Provisioned = true
	}
	return result, nil
}

func webURL(a Answers) string {
	if a.TLSEnabled() && a.HTTPSPort > 0 {
		suffix := ""
		if a.HTTPSPort != 443 {
			suffix = fmt.Sprintf(":%d", a.HTTPSPort)
		}
		return "https://" + a.Hostname + suffix + "/"
	}
	return fmt.Sprintf("http://%s:%d/", a.Hostname, a.WebPort)
}

// asker collects the answers of one step and remembers the first failure, so
// that a step can be written as a straight sequence of questions without every
// call site repeating the error handling. Ctrl-C arrives here as an error and
// unwinds the whole step.
type asker struct {
	ui  UI
	err error
}

func (k *asker) input(q string, o tui.InputOpts, hint string) string {
	if k.err != nil {
		return ""
	}
	if hint != "" {
		k.ui.Hint(hint)
	}
	v, err := k.ui.Input(q, o)
	if err != nil {
		k.err = err
		return ""
	}
	return v
}

func (k *asker) confirm(q string, def bool, hint string) bool {
	if k.err != nil {
		return def
	}
	v, err := k.ui.Confirm(q, def, hint)
	if err != nil {
		k.err = err
		return def
	}
	return v
}

func (k *asker) selectOne(q string, choices []tui.Choice, def int, hint string) int {
	if k.err != nil {
		return def
	}
	v, err := k.ui.Select(q, choices, def, hint)
	if err != nil {
		k.err = err
		return def
	}
	return v
}

func (k *asker) multi(q string, choices []tui.Choice, def []bool, hint string) []bool {
	if k.err != nil {
		return def
	}
	v, err := k.ui.Multi(q, choices, def, hint)
	if err != nil {
		k.err = err
		return def
	}
	return v
}

// stepIdentity asks what the rest of the world will call this server.
func stepIdentity(ui UI, a *Answers) error {
	k := asker{ui: ui}
	ui.Section(1, Steps, "Server identity",
		"Other mail servers judge this one by its name and address, and reject "+
			"mail from a host whose name and address do not match.")

	a.Hostname = k.input("Mail server hostname", tui.InputOpts{
		Default: a.Hostname,
		Validate: func(v string) error {
			if strings.ContainsAny(v, " /\\@:") {
				return fmt.Errorf("%q is not a bare hostname; leave off the scheme, port and path", v)
			}
			return nil
		},
	}, "The name in your DNS **A record**, for example mail.example.com. It "+
		"must resolve to the public address below, and that address must "+
		"resolve back to it, or most receivers will drop your mail as spam.")

	defIP := a.PublicIP
	if defIP == "" {
		defIP = "127.0.0.1"
	}
	a.PublicIP = k.input("Public IPv4 address", tui.InputOpts{
		Default: defIP,
		Validate: func(v string) error {
			if _, err := netip.ParseAddr(v); err != nil {
				return fmt.Errorf("%q is not an IP address", v)
			}
			return nil
		},
	}, "The address other servers connect to. I looked up the address this "+
		"machine goes out to the internet with, which is **"+defIP+"**. "+
		"Behind a home router or a cloud load balancer? Use the public address "+
		"it is published as, not this private one.")
	if addr, err := netip.ParseAddr(a.PublicIP); err == nil {
		switch {
		case addr.IsLoopback() && a.Hostname != "localhost":
			ui.Warn("%s is a loopback address; remote servers will refuse mail from %s", a.PublicIP, a.Hostname)
		case addr.IsPrivate():
			ui.Warn("%s is a private address; mail works only if this host is reached directly", a.PublicIP)
		}
	}

	a.DataDir = k.input("Data directory", tui.InputOpts{
		Default: a.DataDir,
		Validate: func(v string) error {
			if !strings.HasPrefix(v, "/") {
				return errors.New("use an absolute path")
			}
			return nil
		},
	}, "Where message bodies, DKIM keys and the ACME cache live. **Back this "+
		"directory up**: it is the whole mailbox store. The packaged default "+
		"/var/lib/linchange is created for you.")
	return k.err
}

// stepDatabase collects the PostgreSQL connection and proves it answers.
// stepDatabase collects the PostgreSQL connection, and - when the database is
// about to be provisioned - proves that it answers. Skipping the check when it
// is not means a configuration can be written while the database is still
// being installed, which is the normal order on a fresh machine.
func stepDatabase(ctx context.Context, ui UI, a *Answers, check bool) error {
	ui.Section(2, Steps, "Database",
		"linchange stores accounts, mailboxes and calendar data in PostgreSQL.")

	ui.Hint("PostgreSQL is not installed by the package, because many servers " +
		"already run one. On Debian: **sudo apt install postgresql**, then " +
		"**sudo -u postgres createdb linchange** and " +
		"**sudo -u postgres psql -c \"CREATE USER linchange WITH PASSWORD '...';\"**. " +
		"MySQL and MariaDB are not supported: the schema uses PostgreSQL " +
		"features such as citext and triggers.")

	for {
		k := asker{ui: ui}
		a.DBHost = k.input("Database host", tui.InputOpts{
			Default:  a.DBHost,
			Validate: notEmpty("host"),
		}, "The machine running PostgreSQL. **127.0.0.1** when it is this one, "+
			"which is also the safe answer: a database exposed to the network "+
			"has to be defended separately.")
		a.DBPort = atoi(k.input("Database port", tui.InputOpts{
			Default:  strconv.Itoa(a.DBPort),
			Validate: validatePort,
		}, "The PostgreSQL port, **5432** unless you changed it."), a.DBPort)
		a.DBName = k.input("Database name", tui.InputOpts{
			Default:  a.DBName,
			Validate: notEmpty("name"),
		}, "Created by the commands above, for example **linchange**.")
		a.DBUser = k.input("Database user", tui.InputOpts{
			Default:  a.DBUser,
			Validate: notEmpty("user"),
		}, "The role that owns the tables, for example **linchange**.")
		a.DBPassword = k.input("Database password", tui.InputOpts{
			Default:  a.DBPassword,
			Mask:     true,
			Optional: true,
		}, "Of the role above, if it has one. It is written to the "+
			"configuration file, which is made **readable by root only**.")
		if k.err != nil {
			return k.err
		}

		a.DBSSLMode = []string{"disable", "require", "verify-full"}[k.selectOne(
			"Encryption towards the database",
			[]tui.Choice{
				{Label: "None (sslmode=disable)",
					Hint: "Same machine, or a private network you trust. The usual first choice."},
				{Label: "Required (sslmode=require)",
					Hint: "Encrypted, the certificate is not checked. Right for a managed database on the same cloud."},
				{Label: "Fully verified (sslmode=verify-full)",
					Hint: "Encrypted and the server certificate is checked against its name. Right for a database over the public internet."},
			}, sslChoice(a.DBSSLMode),
			"A local PostgreSQL usually has SSL off by default; a hosted one "+
				"always has it on.")]

		if !check {
			ui.Info("the connection is not tested here; it is when the database is prepared")
			return nil
		}

		testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		done := ui.Progress("Testing the database connection")
		err := testDatabase(testCtx, a.DBURL())
		cancel()
		done(err)
		if err == nil {
			ui.Success("the database answers")
			return nil
		}
		ui.Warn("could not reach the database")
		again, err := ui.Confirm("Try these values again?", true, "")
		if err != nil {
			return err
		}
		if !again {
			return fmt.Errorf("the database did not answer: %w", err)
		}
	}
}

// testDatabase opens a pool and performs one round trip.
func testDatabase(ctx context.Context, url string) error {
	db, err := store.Open(ctx, store.Options{
		URL:             url,
		MaxOpen:         2,
		ConnectTimeout:  10 * time.Second,
		ApplicationName: "linchange-setup",
	})
	if err != nil {
		return err
	}
	defer db.Close()
	return store.Health(ctx, db)
}

func sslChoice(mode string) int {
	switch mode {
	case "require":
		return 1
	case "verify-full", "verify-ca":
		return 2
	default:
		return 0
	}
}

// stepDomain asks which mail domain is hosted here.
func stepDomain(ui UI, a *Answers) error {
	k := asker{ui: ui}
	ui.Section(3, Steps, "Mail domain",
		"The domain whose addresses this server will hold.")

	def := a.Domain
	if def == "" && strings.Count(a.Hostname, ".") >= 2 {
		// mail.example.com usually hosts example.com; offering it saves the
		// most common typing.
		def = a.Hostname[strings.Index(a.Hostname, ".")+1:]
	}
	a.Domain = k.input("Domain name", tui.InputOpts{
		Default: def,
		Validate: func(v string) error {
			if strings.ContainsAny(v, " /\\@:") || !strings.Contains(v, ".") {
				return fmt.Errorf("%q must be a bare domain such as example.com", v)
			}
			return nil
		},
	}, "Users get addresses like **alice@"+or(def, "example.com")+"**. The MX "+
		"record of this domain must point at the hostname from step 1 for mail "+
		"to arrive; without it everything else still works, and only inbound "+
		"mail does not.")
	if k.err != nil {
		return k.err
	}

	a.AcceptMail = k.confirm(
		fmt.Sprintf("Should this server deliver mail for %s itself?", a.Domain),
		a.AcceptMail,
		"Answer **yes** when this server is the final destination for the "+
			"domain, which is the normal case. Answer no when another mail "+
			"system keeps the mailboxes and this one only relays, such as a "+
			"migration running both at once.")
	return k.err
}

// stepTLS asks how transport security is provided.
func stepTLS(ui UI, a *Answers) error {
	k := asker{ui: ui}
	ui.Section(4, Steps, "TLS certificates",
		"Encryption in transit, for mail submission, mail reading and the web interface.")

	def := 2
	switch a.TLSMode {
	case TLSACME:
		def = 0
	case TLSCert:
		def = 1
	default:
		// A real hostname means ACME is likely to work, so recommend it there
		// and leave the cautious default on a machine with no public name.
		if a.Hostname != "" && !strings.Contains(a.Hostname, "localhost") {
			def = 0
		}
	}
	idx := k.selectOne("How should certificates be obtained?",
		[]tui.Choice{
			{Label: "Automatic, from Let's Encrypt (ACME)",
				Hint: "Best choice for a server with a public hostname. Port 80 must be reachable from the internet for the challenge, and renewal is automatic."},
			{Label: "I already have certificate files",
				Hint: "Your own CA, or a wildcard certificate. Both the certificate and its private key are needed."},
			{Label: "No TLS for now",
				Hint: "Only sensible on a test machine. Mail moves in the clear, clients refuse to send passwords, and the TLS ports stay closed."},
		}, def, "Not sure? Choose **Let's Encrypt** if this hostname is "+
			"publicly reachable, and **No TLS** to try things out on a private "+
			"machine first.")
	if k.err != nil {
		return k.err
	}

	switch idx {
	case 0:
		a.TLSMode = TLSACME
		a.ACMEEmail = k.input("Email for certificate warnings", tui.InputOpts{
			Default:  a.AdminEmail,
			Optional: true,
			Validate: func(v string) error {
				if v == "" {
					return nil
				}
				if _, err := mail.ParseAddress(v); err != nil {
					return fmt.Errorf("%q is not an email address", v)
				}
				return nil
			},
		}, "Let's Encrypt uses it to warn you before a certificate expires. "+
			"It ends up in the certificate's public metadata.")
		if strings.Contains(a.Hostname, "localhost") {
			ui.Warn("the hostname is localhost: the ACME challenge cannot succeed until it is a public name")
		}
	case 1:
		a.TLSMode = TLSCert
		a.CertFile = k.input("Certificate path", tui.InputOpts{
			Default:  a.CertFile,
			Validate: absolutePath,
		}, "A PEM certificate, usually fullchain.pem. The linchange service "+
			"user must be able to read it.")
		a.KeyFile = k.input("Private key path", tui.InputOpts{
			Default:  a.KeyFile,
			Validate: absolutePath,
		}, "The matching PEM private key. Keep it readable by root only: a "+
			"leaked key lets anybody impersonate your server.")
	default:
		a.TLSMode = TLSNone
		a.ACMEEmail, a.CertFile, a.KeyFile = "", "", ""
		ui.Warn("without TLS, passwords cross the network in the clear and " +
			"Exchange and mobile clients will refuse to connect")
	}
	return k.err
}

// stepProtocols asks which services are enabled.
func stepProtocols(ui UI, a *Answers) error {
	k := asker{ui: ui}
	ui.Section(5, Steps, "Protocols",
		"Which services this server offers. Anything switched off here simply does not listen.")

	ui.Info("Webmail, calendar (CalDAV and CardDAV) and the admin panel are " +
		"always part of the web interface; there is nothing to enable for them.")

	// Each entry is a label, the default, and the field it writes back to.
	type proto struct {
		choice tui.Choice
		get    func(*Answers) *bool
	}
	plain := []proto{
		{tui.Choice{Label: "SMTP receiving (port 25)",
			Hint: "Accept mail addressed to your users from other servers. Only enable it when this server is the destination for the domain; sending also needs outbound port 25, which many hosts block."},
			func(a *Answers) *bool { return &a.SMTPInbound }},
		{tui.Choice{Label: "SMTP submission (port 587)",
			Hint: "What mail clients use to send. Thunderbird, Outlook and phones all talk to this; without it nobody can send anything."},
			func(a *Answers) *bool { return &a.SMTPSubmission }},
		{tui.Choice{Label: "IMAP (port 143)",
			Hint: "Reading mail. The protocol every modern client uses, phones included."},
			func(a *Answers) *bool { return &a.IMAP }},
		{tui.Choice{Label: "POP3 (port 110)",
			Hint: "The older alternative to IMAP, which downloads and usually deletes. Only for clients that still need it."},
			func(a *Answers) *bool { return &a.POP3 }},
	}
	// The implicit-TLS variants are only worth asking about when there is a
	// certificate to serve them with, so they join the list conditionally and
	// are matched back by index.
	var extra []proto
	if a.TLSEnabled() {
		if a.IMAP {
			extra = append(extra, proto{tui.Choice{Label: "IMAPS (port 993)",
				Hint: "IMAP over implicit TLS. What phones and Outlook expect by default."},
				func(a *Answers) *bool { return &a.IMAPS }})
		}
		if a.POP3 {
			extra = append(extra, proto{tui.Choice{Label: "POP3S (port 995)",
				Hint: "POP3 over implicit TLS."},
				func(a *Answers) *bool { return &a.POP3S }})
		}
	}

	all := append(append([]proto{}, plain...), extra...)
	labels := make([]tui.Choice, len(all))
	current := make([]bool, len(all))
	for i, p := range all {
		labels[i] = p.choice
		current[i] = *p.get(a)
	}
	checked := k.multi("Which protocols should be enabled?", labels, current,
		"Space toggles a line, Enter accepts. The ports in brackets are the "+
			"standard ones; every one of them can be changed later in the "+
			"configuration file.")
	if k.err != nil {
		return k.err
	}
	for i, p := range all {
		*p.get(a) = checked[i]
	}
	if len(extra) == 0 {
		// Without certificates the TLS listeners cannot be bound, whatever
		// the previous run chose.
		a.IMAPS, a.POP3S = false, false
	}

	a.WebPort = atoi(k.input("Web interface port", tui.InputOpts{
		Default:  strconv.Itoa(a.WebPort),
		Validate: validatePort,
	}, "Serves webmail, the calendar, ActiveSync, EWS and Autodiscover. "+
		"**8080** is the default; put a reverse proxy with TLS in front of it "+
		"if you already have one."), a.WebPort)

	if a.TLSEnabled() {
		on := k.confirm("Serve HTTPS directly on port 443?", a.HTTPSPort > 0,
			"**Yes** unless a reverse proxy already terminates TLS on this "+
				"host: two things listening on 443 will not start. The ACME "+
				"challenge is answered on the plain HTTP port either way.")
		if on {
			a.HTTPSPort = 443
		} else {
			a.HTTPSPort = 0
		}
	} else {
		a.HTTPSPort = 0
	}

	a.Exchange = k.confirm("Enable the Microsoft Exchange endpoints (EWS, Autodiscover, ActiveSync)?", a.Exchange,
		"Needed for Outlook connecting directly, and for ActiveSync phones. "+
			"They are served on the web port above, and Outlook in particular "+
			"expects **HTTPS**, so leave this off until TLS works if you chose "+
			"no certificates.")
	if k.err != nil {
		return k.err
	}
	if a.Exchange {
		endpoints := k.multi("Which Exchange endpoints?",
			[]tui.Choice{
				{Label: "EWS (Exchange Web Services)", Hint: "Calendar, contacts and mail for Outlook and macOS Mail."},
				{Label: "Autodiscover", Hint: "Lets a client find its settings from an email address alone. Outlook asks for it constantly when it is missing."},
				{Label: "ActiveSync (EAS)", Hint: "Mail and calendars on phones, and on Outlook when configured for it."},
			}, []bool{a.EWS, a.Autodiscover, a.EAS},
			"When unsure, keep all three: they share the web listener and "+
				"cost nothing to have available.")
		if k.err != nil {
			return k.err
		}
		a.EWS, a.Autodiscover, a.EAS = endpoints[0], endpoints[1], endpoints[2]
		a.Organization = k.input("Organization name", tui.InputOpts{
			Default:  or(a.Organization, a.Domain),
			Validate: notEmpty("organization"),
		}, "Reported to clients in Autodiscover and EWS responses, for "+
			"example **Example Ltd**. It appears in Outlook's account settings.")
	}
	return k.err
}

// stepAdmin creates the account that will log in first.
func stepAdmin(ui UI, a *Answers, minLength int) error {
	k := asker{ui: ui}
	ui.Section(6, Steps, "Administrator account",
		"The first account, which also holds the administration grant.")

	def := a.AdminEmail
	if def == "" && a.Domain != "" {
		def = "admin@" + a.Domain
	}
	a.AdminEmail = k.input("Administrator address", tui.InputOpts{
		Default: def,
		Validate: func(v string) error {
			addr, err := mail.ParseAddress(v)
			if err != nil || !strings.Contains(addr.Address, "@") {
				return fmt.Errorf("%q is not an email address", v)
			}
			return nil
		},
	}, "This is where the account lives, so it should be inside the domain "+
		"from step 3. It receives mail like any other account and can "+
		"administer everything.")
	if k.err != nil {
		return k.err
	}
	if a.Domain != "" && !strings.HasSuffix(strings.ToLower(a.AdminEmail), "@"+strings.ToLower(a.Domain)) {
		ui.Warn("%s is outside %s: the account is created, but mail for it is "+
			"only delivered here if that domain is hosted too", a.AdminEmail, a.Domain)
	}

	a.AdminName = k.input("Display name", tui.InputOpts{
		Default:  or(a.AdminName, "Administrator"),
		Optional: true,
	}, "Shown in the web interface and in the From: header of mail this "+
		"account sends.")

	for k.err == nil {
		pw, err := ui.Input("Password", tui.InputOpts{Mask: true})
		if err != nil {
			k.err = err
			break
		}
		if minLength > 0 && len(pw) < minLength {
			ui.Warn("at least %d characters are required; four random words "+
				"from a password manager beat a short jumble", minLength)
			continue
		}
		if pw == "" {
			ui.Warn("the password must not be empty")
			continue
		}
		confirm, err := ui.Input("Password, once more", tui.InputOpts{Mask: true})
		if err != nil {
			k.err = err
			break
		}
		if pw != confirm {
			ui.Warn("the two passwords differ; type them again")
			continue
		}
		a.AdminPassword = pw
		break
	}
	if k.err != nil {
		return k.err
	}

	a.MustChangePassword = k.confirm("Require a password change at the first login?", a.MustChangePassword,
		"**No** is right when you chose this password yourself and will keep "+
			"using it. Say yes when you are setting the account up for "+
			"somebody else.")
	return k.err
}

// confirmReview shows what will be written and asks for the last yes.
func confirmReview(ui UI, cfg *config.Config, a Answers, warnings []string, opts Options) error {
	ui.Section(7, Steps, "Review",
		"This is what will be written. Nothing has been changed yet.")

	protocols := []string{}
	add := func(on bool, name string) {
		if on {
			protocols = append(protocols, name)
		}
	}
	add(a.SMTPInbound, "SMTP in :25")
	add(a.SMTPSubmission, "submission :587")
	add(a.IMAP, "IMAP :143")
	add(a.IMAPS, "IMAPS :993")
	add(a.POP3, "POP3 :110")
	add(a.POP3S, "POP3S :995")
	add(true, fmt.Sprintf("web %d", a.WebPort))
	add(a.HTTPSPort > 0, fmt.Sprintf("https %d", a.HTTPSPort))
	if a.Exchange {
		exchange := []string{}
		if a.EWS {
			exchange = append(exchange, "EWS")
		}
		if a.Autodiscover {
			exchange = append(exchange, "Autodiscover")
		}
		if a.EAS {
			exchange = append(exchange, "ActiveSync")
		}
		protocols = append(protocols, "Exchange ("+strings.Join(exchange, ", ")+")")
	}

	tlsDesc := "none (plaintext)"
	switch a.TLSMode {
	case TLSACME:
		tlsDesc = "Let's Encrypt, automatic"
	case TLSCert:
		tlsDesc = a.CertFile
	}

	ui.KeyValue("Hostname", a.Hostname)
	ui.KeyValue("Public address", a.PublicIP)
	ui.KeyValue("Data directory", a.DataDir)
	ui.KeyValue("Database", fmt.Sprintf("%s@%s:%d/%s", a.DBUser, a.DBHost, a.DBPort, a.DBName))
	ui.KeyValue("Domain", fmt.Sprintf("%s (%s)", a.Domain,
		map[bool]string{true: "mail delivered here", false: "relay only"}[a.AcceptMail]))
	ui.KeyValue("Protocols", strings.Join(protocols, ", "))
	ui.KeyValue("TLS", tlsDesc)
	ui.KeyValue("Administrator", a.AdminEmail)
	ui.KeyValue("Password", strings.Repeat("*", 8)+" (stored as an argon2 hash)")

	for _, w := range warnings {
		ui.Warn("%s", w)
	}
	if !cfg.Database.AutoMigrate {
		ui.Warn("automatic migrations are off; the schema must be applied another way")
	}
	if _, err := os.Stat(opts.Path); err == nil {
		ui.Warn("%s already exists; the old file is backed up beside the new one", opts.Path)
	}

	ok, err := ui.Confirm("Write this configuration and prepare the database?", true,
		"Answering **no** leaves everything untouched, and you can run "+
			"**linchange setup** again later.")
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("aborted; nothing was written")
	}
	return nil
}

// Provision makes the database match the configuration: schema, domain and the
// administrator account. It is written to be re-runnable, because the wizard
// can be run again on a live server and "already exists" is a normal answer,
// not a failure.
func Provision(ctx context.Context, ui UI, db *sqlx.DB, cfg *config.Config, a Answers, result *Result) error {
	if db == nil {
		return errors.New("setup: provisioning needs a database handle")
	}

	if cfg.Database.AutoMigrate {
		apply := ui.Progress("Applying the database schema")
		log, err := provisioningLogger()
		if err != nil {
			apply(err)
			return err
		}
		if err := store.Migrate(ctx, db, log); err != nil {
			apply(err)
			return err
		}
		apply(nil)
	}

	svc, err := admin.New(admin.Options{
		DB:                db,
		Argon2:            cfg.Auth.Local.Argon2,
		MinPasswordLength: cfg.Auth.Local.MinPasswordLength,
	})
	if err != nil {
		return err
	}

	// The actor is the account being created: on a first run there is nothing
	// else to act as, and this is precisely an administrator acting on their
	// own installation.
	actor := policy.Principal{
		Address:        strings.ToLower(a.AdminEmail),
		Source:         policy.SourceLocal,
		Enabled:        true,
		MailboxEnabled: true,
		IsDomainAdmin:  true,
		Authenticated:  true,
	}

	createDomain := ui.Progress(fmt.Sprintf("Creating domain %s", a.Domain))
	switch _, err := store.NewDomains().Get(ctx, db, a.Domain); {
	case err == nil:
		createDomain(nil)
		ui.Detail("already present, left as it is")
	case !errors.Is(err, store.ErrNotFound):
		createDomain(err)
		return err
	default:
		_, err := svc.CreateDomain(ctx, actor, store.CreateDomainRequest{
			Name:    a.Domain,
			IsLocal: a.AcceptMail,
		})
		if err != nil {
			createDomain(err)
			return fmt.Errorf("creating domain %s: %w", a.Domain, err)
		}
		createDomain(nil)
	}

	createUser := ui.Progress(fmt.Sprintf("Creating administrator %s", a.AdminEmail))
	_, err = svc.CreateUser(ctx, actor, admin.CreateUserRequest{
		Address:            strings.ToLower(a.AdminEmail),
		Source:             policy.SourceLocal,
		DisplayName:        or(a.AdminName, "Administrator"),
		Password:           a.AdminPassword,
		IsDomainAdmin:      true,
		MustChangePassword: &a.MustChangePassword,
	})
	switch {
	case errors.Is(err, store.ErrConflict):
		createUser(nil)
		ui.Detail("that account already exists, left as it is")
		result.AdminCreated = false
	case err != nil:
		createUser(err)
		return fmt.Errorf("creating the administrator: %w", err)
	default:
		createUser(nil)
		result.AdminCreated = true
	}
	return nil
}

// provisioningLogger is the logger the migrator writes through. The wizard's
// own output is the terminal; this only carries migration records, at warn
// level so a normal run is silent.
func provisioningLogger() (*slog.Logger, error) {
	return logging.New(config.Log{Level: "warn", Format: "text"}, "linchange-setup")
}

// --- small helpers --------------------------------------------------------

func notEmpty(what string) func(string) error {
	return func(v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", what)
		}
		return nil
	}
}

func absolutePath(v string) error {
	if !strings.HasPrefix(v, "/") {
		return errors.New("use an absolute path")
	}
	return nil
}

func validatePort(v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q is not a port number between 1 and 65535", v)
	}
	return nil
}

func atoi(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fallback
	}
	return n
}

func or(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// _ is a compile-time check that the real terminal UI satisfies what the
// wizard asks for. A drift would otherwise surface as a panic in the command.
var _ UI = (*tui.Term)(nil)

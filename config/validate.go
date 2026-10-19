package config

import (
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"strconv"
	"strings"
)

// ValidationError collects every problem found in the configuration so that an
// operator fixing a deployment sees all of them at once instead of one per
// restart.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "  - " + e.Problems[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d problems:\n", len(e.Problems))
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	return b.String()
}

type validator struct {
	cfg      *Config
	problems []string
}

func (v *validator) addf(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

// Validate checks the configuration for internal consistency. It reports every
// problem it finds rather than stopping at the first.
func (c *Config) Validate() error {
	v := &validator{cfg: c}

	v.validateServer()
	v.validateDatabase()
	v.validateListeners()
	v.validateTLS()
	v.validateAuth()
	v.validateLDAP()
	v.validateMail()
	v.validateExchange()
	v.validateMeetings()
	v.validateLog()

	if len(v.problems) > 0 {
		return &ValidationError{Problems: v.problems}
	}
	return nil
}

func (v *validator) validateServer() {
	c := v.server()
	if c.Server.Hostname == "" {
		v.addf("server.hostname is required")
	} else if !validHostname(c.Server.Hostname) {
		v.addf("server.hostname %q is not a valid hostname", c.Server.Hostname)
	}

	if c.Server.PublicIP == "" {
		v.addf("server.public_ip is required for correct EHLO advertisement")
	} else if _, err := netip.ParseAddr(c.Server.PublicIP); err != nil {
		v.addf("server.public_ip %q is not an IP address", c.Server.PublicIP)
	}

	if c.Server.DataDir == "" {
		v.addf("server.data_dir is required")
	}

	for _, cidr := range c.Server.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			v.addf("server.trusted_proxies: %q is not a CIDR block", cidr)
		}
	}

	// A bare hostname with the default loopback address will advertise
	// "localhost" to the outside world, which no remote MTA will accept.
	if c.Server.PublicIP == "127.0.0.1" && c.Server.Hostname != "localhost" {
		v.addf("server.public_ip is 127.0.0.1 but server.hostname is %q; "+
			"outbound mail will be rejected by remote MTAs", c.Server.Hostname)
	}
}

func (v *validator) validateDatabase() {
	c := v.server()
	if c.Database.URL == "" {
		v.addf("database.url is required")
	}
	if c.Database.MaxOpen < 1 {
		v.addf("database.max_open must be at least 1, got %d", c.Database.MaxOpen)
	}
	if c.Database.MaxIdle < 0 {
		v.addf("database.max_idle must not be negative, got %d", c.Database.MaxIdle)
	}
	if c.Database.MaxIdle > c.Database.MaxOpen {
		v.addf("database.max_idle (%d) must not exceed database.max_open (%d)",
			c.Database.MaxIdle, c.Database.MaxOpen)
	}
	if c.Database.ConnectTimeout < 0 {
		v.addf("database.connect_timeout must not be negative")
	}
}

func (v *validator) validateListeners() {
	c := v.server()
	seen := map[string]string{}

	type entry struct {
		name string
		addr string
	}
	all := []entry{
		{"listeners.smtp_loopback", c.Listener.SMTPLoopback.Addr},
		{"listeners.smtp_inbound", c.Listener.SMTPInbound.Addr},
		{"listeners.smtp_submission", c.Listener.SMTPSubmission.Addr},
		{"listeners.smtp_submission_tls", c.Listener.SMTPSubmissionTLS.Addr},
		{"listeners.imap", c.Listener.IMAP.Addr},
		{"listeners.imaps", c.Listener.IMAPS.Addr},
		{"listeners.pop3", c.Listener.POP3.Addr},
		{"listeners.pop3s", c.Listener.POP3S.Addr},
		{"listeners.lmtp", c.Listener.LMTP.Addr},
		{"listeners.http", c.Listener.HTTP.Addr},
		{"listeners.https", c.Listener.HTTPS.Addr},
	}

	for _, e := range all {
		if e.addr == "" {
			continue
		}
		if _, port, err := net.SplitHostPort(e.addr); err != nil {
			v.addf("%s: %q is not a host:port address", e.name, e.addr)
			continue
		} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			v.addf("%s: %q has an invalid port", e.name, e.addr)
			continue
		}
		if other, dup := seen[e.addr]; dup {
			v.addf("%s and %s are both bound to %s", e.name, other, e.addr)
			continue
		}
		seen[e.addr] = e.name
	}

	if c.Listener.SMTPInboundMaxBytes < 1 {
		v.addf("listeners.smtp_inbound_max_bytes must be positive, got %d", c.Listener.SMTPInboundMaxBytes)
	}
	if c.Mail.MaxMessageBytes > 0 && c.Listener.SMTPInboundMaxBytes < c.Mail.MaxMessageBytes {
		// Not fatal, but the smaller cap silently wins and is confusing, so
		// it is worth surfacing.
		v.addf("listeners.smtp_inbound_max_bytes (%d) is below mail.max_message_bytes (%d); "+
			"the smaller value will apply", c.Listener.SMTPInboundMaxBytes, c.Mail.MaxMessageBytes)
	}

	if !c.TLSEnabled() {
		for name, addr := range map[string]string{
			"listeners.imaps":               c.Listener.IMAPS.Addr,
			"listeners.pop3s":               c.Listener.POP3S.Addr,
			"listeners.smtp_submission_tls": c.Listener.SMTPSubmissionTLS.Addr,
			"listeners.https":               c.Listener.HTTPS.Addr,
		} {
			if addr != "" {
				v.addf("%s is configured but no TLS certificate is available; "+
					"set tls.cert_file/tls.key_file or enable tls.acme", name)
			}
		}
	}
}

func (v *validator) validateTLS() {
	c := v.server()
	hasCert := c.TLS.CertFile != ""
	hasKey := c.TLS.KeyFile != ""

	if hasCert != hasKey {
		v.addf("tls.cert_file and tls.key_file must be set together")
	}
	if c.TLS.ACME.Enabled {
		if c.TLS.ACME.Email == "" {
			v.addf("tls.acme.email is required when ACME is enabled")
		} else if _, err := mail.ParseAddress(c.TLS.ACME.Email); err != nil {
			v.addf("tls.acme.email %q is not a valid address", c.TLS.ACME.Email)
		}
		if len(c.TLS.ACME.Domains) == 0 {
			v.addf("tls.acme.domains must list at least one name")
		}
		if hasCert {
			v.addf("tls.acme and tls.cert_file are mutually exclusive")
		}
	}
}

func (v *validator) validateAuth() {
	c := v.server()

	if c.Auth.JWT.Secret == "" {
		v.addf("auth.jwt.secret is required")
	} else if len(c.Auth.JWT.Secret) < 32 {
		v.addf("auth.jwt.secret must be at least 32 characters, got %d", len(c.Auth.JWT.Secret))
	}
	if c.Auth.JWT.AccessTTL <= 0 {
		v.addf("auth.jwt.access_ttl must be positive")
	}
	if c.Auth.JWT.RefreshTTL <= c.Auth.JWT.AccessTTL {
		v.addf("auth.jwt.refresh_ttl (%s) must be longer than access_ttl (%s)",
			c.Auth.JWT.RefreshTTL, c.Auth.JWT.AccessTTL)
	}

	a := c.Auth.Local.Argon2
	if a.MemoryKib < 8*1024 {
		v.addf("auth.local.argon2.memory_kib must be at least 8192, got %d", a.MemoryKib)
	}
	if a.Iterations < 1 {
		v.addf("auth.local.argon2.iterations must be at least 1, got %d", a.Iterations)
	}
	if a.Parallelism < 1 || a.Parallelism > 16 {
		v.addf("auth.local.argon2.parallelism must be between 1 and 16, got %d", a.Parallelism)
	}
	if a.SaltLength < 16 {
		v.addf("auth.local.argon2.salt_len must be at least 16, got %d", a.SaltLength)
	}
	if a.KeyLength < 16 {
		v.addf("auth.local.argon2.key_len must be at least 16, got %d", a.KeyLength)
	}
	if c.Auth.Local.MinPasswordLength < 8 {
		v.addf("auth.local.min_password_length must be at least 8, got %d", c.Auth.Local.MinPasswordLength)
	}

	if c.Auth.TOTP.Enabled {
		if c.Auth.TOTP.Issuer == "" {
			v.addf("auth.totp.issuer is required when TOTP is enabled")
		}
		if c.Auth.TOTP.Window > 5 {
			v.addf("auth.totp.window must not exceed 5, got %d", c.Auth.TOTP.Window)
		}
		if c.Auth.TOTP.RecoveryCodes < 0 || c.Auth.TOTP.RecoveryCodes > 32 {
			v.addf("auth.totp.recovery_codes must be between 0 and 32, got %d", c.Auth.TOTP.RecoveryCodes)
		}
		// Enrolment cannot store a secret it cannot encrypt, so refuse to
		// start rather than let the first user to enrol fail.
		if c.Auth.EncryptionKey == "" {
			v.addf("auth.encryption_key is required when TOTP is enabled, " +
				"because second-factor secrets are stored encrypted")
		} else if err := validateKeyMaterial("auth.encryption_key", c.Auth.EncryptionKey); err != nil {
			v.addf("%v", err)
		}
	}

	if c.Auth.Reset.Enabled && c.Auth.Reset.TokenTTL <= 0 {
		v.addf("auth.reset_password.token_ttl must be positive")
	}

	if c.Auth.Challenge.TTL <= 0 {
		v.addf("auth.challenge.ttl must be positive")
	}

	v.lockoutScope("auth.lockout.account", c.Auth.Lockout.Account)
	v.lockoutScope("auth.lockout.ip", c.Auth.Lockout.IP)
	if c.Auth.Session.IdleTimeout > c.Auth.Session.AbsoluteTimeout {
		v.addf("auth.session.idle_timeout (%s) must not exceed absolute_timeout (%s)",
			c.Auth.Session.IdleTimeout, c.Auth.Session.AbsoluteTimeout)
	}

	if !c.Auth.Impersonation.Allowed && c.Auth.Impersonation.RequireReason {
		v.addf("auth.impersonation.require_reason is meaningless when impersonation is disabled")
	}
}

func (v *validator) validateLDAP() {
	c := v.server()
	names := map[string]struct{}{}

	for i, p := range c.LDAP.Providers {
		label := fmt.Sprintf("ldap.providers[%d]", i)
		if p.Name == "" {
			v.addf("%s.name is required", label)
		} else {
			if _, dup := names[p.Name]; dup {
				v.addf("%s.name %q is duplicated", label, p.Name)
			}
			names[p.Name] = struct{}{}
			label = "ldap.providers." + p.Name
		}

		if p.URL == "" {
			v.addf("%s.url is required", label)
		} else if !strings.HasPrefix(p.URL, "ldap://") && !strings.HasPrefix(p.URL, "ldaps://") {
			v.addf("%s.url %q must use the ldap:// or ldaps:// scheme", label, p.URL)
		}
		if p.BaseDN == "" {
			v.addf("%s.base_dn is required", label)
		}
		if p.UserFilter == "" {
			v.addf("%s.user_filter is required", label)
		} else if !strings.Contains(p.UserFilter, "{{username}}") {
			v.addf("%s.user_filter must contain the {{username}} placeholder", label)
		}
		if (p.BindDN == "") != (p.BindPassword == "") {
			v.addf("%s.bind_dn and %s.bind_password must be set together", label, label)
		}
		if p.DefaultDomain != "" && !validHostname(p.DefaultDomain) {
			v.addf("%s.default_domain %q is not a valid domain", label, p.DefaultDomain)
		}
		if p.MailboxQuotaBytes < 0 {
			v.addf("%s.mailbox_quota_bytes must not be negative", label)
		}
		if c.Mail.MaxQuotaBytes > 0 && p.MailboxQuotaBytes > c.Mail.MaxQuotaBytes {
			v.addf("%s.mailbox_quota_bytes (%d) exceeds mail.max_quota_bytes (%d)",
				label, p.MailboxQuotaBytes, c.Mail.MaxQuotaBytes)
		}
		// The off-domain AD rule depends on this group, so a provider that
		// can import off-domain users without it is a misconfiguration that
		// would silently refuse every off-domain domain admin.
		if p.DomainAdminsGroupDN == "" {
			v.addf("%s.domain_admins_group_dn is required so that members of it "+
				"can be given a domain_admin account when their address is off-domain", label)
		}
	}

	if c.LDAP.Sync.Enabled && len(c.LDAP.Providers) == 0 {
		v.addf("ldap.sync.enabled is true but no ldap.providers are configured")
	}
	if c.LDAP.Sync.Enabled && c.LDAP.Sync.Interval <= 0 {
		v.addf("ldap.sync.interval must be positive")
	}
	if c.LDAP.Sync.PageSize < 1 {
		v.addf("ldap.sync.page_size must be at least 1")
	}
	if c.LDAP.CacheTTL < 0 {
		v.addf("ldap.cache_ttl must not be negative")
	}
}

func (v *validator) validateMail() {
	c := v.server()

	if c.Mail.Submission.DailySendLimit < 0 {
		v.addf("mail.submission.daily_send_limit must not be negative, got %d", c.Mail.Submission.DailySendLimit)
	}
	if c.Mail.Submission.AllowInsecureAuth && c.Mail.Submission.RequireTLS {
		v.addf("mail.submission.allow_insecure_auth and require_tls are both set; " +
			"require_tls wins, so allow_insecure_auth has no effect")
	}

	if c.Mail.DefaultQuotaBytes < 0 {
		v.addf("mail.default_quota_bytes must not be negative")
	}
	if c.Mail.MaxQuotaBytes > 0 && c.Mail.DefaultQuotaBytes > c.Mail.MaxQuotaBytes {
		v.addf("mail.default_quota_bytes (%d) exceeds mail.max_quota_bytes (%d)",
			c.Mail.DefaultQuotaBytes, c.Mail.MaxQuotaBytes)
	}
	if c.Mail.MaxMessageBytes < 1024 {
		v.addf("mail.max_message_bytes must be at least 1024, got %d", c.Mail.MaxMessageBytes)
	}
	if c.Mail.RecipientsPerMessage < 1 {
		v.addf("mail.recipients_per_message must be at least 1, got %d", c.Mail.RecipientsPerMessage)
	}
	if c.Mail.DKIM.KeyBits != 2048 && c.Mail.DKIM.KeyBits != 4096 {
		v.addf("mail.dkim.key_bits must be 2048 or 4096, got %d", c.Mail.DKIM.KeyBits)
	}
	if c.Mail.DKIM.SelectorPrefix == "" {
		v.addf("mail.dkim.selector_prefix is required")
	}
	if c.Mail.DKIM.SignAll {
		if c.Mail.DkimPrivateKeyEncryptionKey == "" {
			v.addf("mail.dkim_private_key_encryption_key is required when mail.dkim.sign_all is on, " +
				"so that private keys are not stored in plaintext on disk")
		} else if err := validateKeyMaterial("mail.dkim_private_key_encryption_key", c.Mail.DkimPrivateKeyEncryptionKey); err != nil {
			v.addf("%v", err)
		}
	}

	if c.Mail.Inbound.Spam.Enabled {
		if c.Mail.Inbound.Spam.JunkThreshold <= 0 {
			v.addf("mail.inbound.spam.junk_threshold must be positive")
		}
		if c.Mail.Inbound.Spam.RejectThreshold <= c.Mail.Inbound.Spam.JunkThreshold {
			v.addf("mail.inbound.spam.reject_threshold (%g) must exceed junk_threshold (%g)",
				c.Mail.Inbound.Spam.RejectThreshold, c.Mail.Inbound.Spam.JunkThreshold)
		}
	}
	if c.Mail.Inbound.Greylisting.Enabled {
		if c.Mail.Inbound.Greylisting.Delay <= 0 {
			v.addf("mail.inbound.greylisting.delay must be positive")
		}
		if c.Mail.Inbound.Greylisting.RetryWindow <= 0 {
			v.addf("mail.inbound.greylisting.retry_window must be positive")
		}
		if c.Mail.Inbound.Greylisting.Expiry <= c.Mail.Inbound.Greylisting.Delay {
			v.addf("mail.inbound.greylisting.expiry (%s) must exceed delay (%s)",
				c.Mail.Inbound.Greylisting.Expiry, c.Mail.Inbound.Greylisting.Delay)
		}
	}
	if c.Mail.Inbound.MaxRecipients < 1 {
		v.addf("mail.inbound.max_recipients must be at least 1, got %d", c.Mail.Inbound.MaxRecipients)
	}

	o := c.Mail.Outbound
	if o.Concurrency < 1 {
		v.addf("mail.outbound.concurrency must be at least 1, got %d", o.Concurrency)
	}
	if o.PerDomainConcurrency < 1 {
		v.addf("mail.outbound.per_domain_concurrency must be at least 1, got %d", o.PerDomainConcurrency)
	}
	if o.Retry.MaxAttempts < 1 {
		v.addf("mail.outbound.retry.max_attempts must be at least 1, got %d", o.Retry.MaxAttempts)
	}
	if o.Retry.BackoffBase <= 0 {
		v.addf("mail.outbound.retry.backoff_base must be positive")
	}
	if o.Retry.BackoffMax < o.Retry.BackoffBase {
		v.addf("mail.outbound.retry.backoff_max (%s) must not be below backoff_base (%s)",
			o.Retry.BackoffMax, o.Retry.BackoffBase)
	}
	if o.Retry.MaxLifetime > 0 && o.Retry.MaxLifetime < o.Retry.BackoffBase {
		v.addf("mail.outbound.retry.max_lifetime (%s) must not be below backoff_base (%s)",
			o.Retry.MaxLifetime, o.Retry.BackoffBase)
	}
	if o.DSN.Enabled {
		if o.DSN.FromAddress == "" {
			v.addf("mail.outbound.dsn.from_address is required when DSN is enabled, " +
				"so that bounces have a valid envelope sender")
		} else if _, err := mail.ParseAddress(o.DSN.FromAddress); err != nil {
			v.addf("mail.outbound.dsn.from_address %q is not a valid address", o.DSN.FromAddress)
		}
	}
}

func (v *validator) validateExchange() {
	c := v.server()
	if !c.Exchange.Enabled {
		return
	}
	if c.Exchange.Organization == "" {
		v.addf("exchange.organization is required when the Exchange endpoints are enabled; " +
			"Autodiscover and EWS report it to clients")
	}
	if c.Exchange.Autodiscover.Enabled && !strings.HasPrefix(c.Exchange.Autodiscover.Path, "/") {
		v.addf("exchange.autodiscover.path must start with /")
	}
	if c.Exchange.EWS.Enabled && !strings.HasPrefix(c.Exchange.EWS.Path, "/") {
		v.addf("exchange.ews.path must start with /")
	}
	v.validateAdvertisedPorts(c.Exchange.Advertised)
	if c.Exchange.EAS.Enabled {
		if !strings.HasPrefix(c.Exchange.EAS.Path, "/") {
			v.addf("exchange.eas.path must start with /")
		}
		if c.Exchange.EAS.MaxPingInterval < 60 {
			v.addf("exchange.eas.max_ping_interval must be at least 60 seconds, got %d",
				c.Exchange.EAS.MaxPingInterval)
		}
		if c.Exchange.EAS.MaxSyncItems < 1 {
			v.addf("exchange.eas.max_sync_items must be at least 1, got %d", c.Exchange.EAS.MaxSyncItems)
		}
	}
	if !c.Listener.HTTP.Enabled() && !c.Listener.HTTPS.Enabled() {
		v.addf("exchange endpoints are enabled but neither listeners.http nor listeners.https is set")
	}
	if c.Listener.HTTPS.Enabled() && !c.TLSEnabled() {
		v.addf("listeners.https is set but no TLS certificate is configured")
	}
}

// validateMeetings checks the meeting integration. Everything is optional: a
// deployment that says nothing about meetings simply has no meeting UI, and
// one that enables OpenIM without pointing at a server is refused at startup
// rather than at the first invitation.
func (v *validator) validateMeetings() {
	c := v.server()
	m := c.Meetings
	if m.PublicURL != "" {
		if !strings.HasPrefix(m.PublicURL, "http://") && !strings.HasPrefix(m.PublicURL, "https://") {
			v.addf("meetings.public_url %q must start with http:// or https://", m.PublicURL)
		}
		if strings.HasSuffix(m.PublicURL, "/") {
			v.addf("meetings.public_url %q must not end with /", m.PublicURL)
		}
	}
	o := m.OpenIM
	if !o.Enabled {
		return
	}
	if o.APIAddr == "" {
		v.addf("meetings.openim.api_addr is required when OpenIM is enabled")
	}
	if o.WSAddr == "" {
		v.addf("meetings.openim.ws_addr is required when OpenIM is enabled; it is the address the browser SDK connects to")
	}
	if o.AdminSecret == "" {
		v.addf("meetings.openim.admin_secret is required when OpenIM is enabled")
	}
	if !c.Listener.HTTP.Enabled() && !c.Listener.HTTPS.Enabled() {
		v.addf("meetings are enabled but neither listeners.http nor listeners.https is set")
	}
}

// validateAdvertisedPorts rejects advertised ports outside the valid range. A
// port of zero is not an error: it is how a deployment says "the standard port
// for this protocol", which is the common case.
func (v *validator) validateAdvertisedPorts(p Ports) {
	for _, f := range []struct {
		name string
		port int
	}{
		{"imap", p.IMAP},
		{"imaps", p.IMAPS},
		{"pop3", p.POP3},
		{"pop3s", p.POP3S},
		{"smtp", p.SMTP},
		{"smtps", p.SMTPS},
		{"submission", p.Submit},
		{"http", p.HTTP},
		{"https", p.HTTPS},
	} {
		if f.port < 0 || f.port > 65535 {
			v.addf("exchange.advertised.%s port %d is out of range (0 means the standard port)", f.name, f.port)
		}
	}
}

func (v *validator) validateLog() {
	c := v.server()
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		v.addf("logging.level %q must be one of debug, info, warn, error", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		v.addf("logging.format %q must be json or text", c.Log.Format)
	}
	if c.Log.AuditRetain <= 0 {
		v.addf("logging.audit_retain must be positive")
	}
}

// server gives the validator access to the configuration under validation. It
// exists so the methods can be written against a single receiver type.
func (v *validator) server() *Config { return v.cfg }

// validHostname performs a conservative RFC 1123 host check. It intentionally
// rejects anything with a scheme, port or path so that a misconfigured
// hostname is caught here rather than producing malformed EHLO responses.
func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	h = strings.TrimSuffix(h, ".")
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
			if !isAlnum && c != '-' {
				return false
			}
		}
	}
	return true
}

// minKeyBytes is the shortest key accepted for an at-rest encryption key.
// AES-256 is the target, so anything shorter is not really protecting the
// secrets it is named for.
const minKeyBytes = 32

// validateKeyMaterial checks that a configured encryption key is present and
// long enough to be meaningful.
//
// The key is accepted as an arbitrary string rather than requiring base64,
// because an operator generating one with openssl should not have to know which
// encoding this program happens to want. The TOTP package derives a fixed-size
// key from it.
func validateKeyMaterial(name, key string) error {
	if key == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len([]byte(key)) < minKeyBytes {
		return fmt.Errorf("%s must be at least %d bytes, got %d; generate one with "+
			"`openssl rand -base64 32`", name, minKeyBytes, len([]byte(key)))
	}
	return nil
}

// lockoutScope checks one of the two authentication lockout scopes.
//
// A scope with max_attempts of 0 or less is switched off, and the rest of its
// settings are then ignored rather than required, so an operator who disables a
// limit does not have to invent a window and a duration that will never be read.
//
// Nothing is required about the relationship between window and duration. Both
// orderings are coherent: a lock longer than its window is simply a long
// throttle, and a lock shorter than its window clears the counter when it is
// served rather than re-locking on the next attempt. An earlier version of this
// file refused duration > window on the reasoning that the window would then be
// the real limit, which is not how the policy works, and it rejected the shipped
// per-address default of 20 attempts per 15 minutes with a one hour lock.
func (v *validator) lockoutScope(name string, s LockoutScope) {
	if s.MaxAttempts <= 0 {
		return
	}
	if s.Window <= 0 {
		v.addf("%s.window must be positive when max_attempts is set", name)
	}
	if s.Duration <= 0 {
		v.addf("%s.duration must be positive when max_attempts is set", name)
	}
}

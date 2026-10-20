// Package config loads and validates the linchange server configuration.
//
// Configuration is a single YAML document supplied on the command line. Every
// value can be overridden with ${VAR} environment interpolation so that
// secrets need not be committed to the file. A configuration that fails to
// load or validate is a fatal startup error: the server never runs on
// partially-applied defaults.
package config

import "time"

// Config is the root configuration document.
type Config struct {
	Server   Server    `yaml:"server"`
	Database Database  `yaml:"database"`
	Listener Listeners `yaml:"listeners"`
	TLS      TLS       `yaml:"tls"`
	Auth     Auth      `yaml:"auth"`
	LDAP     LDAP      `yaml:"ldap"`
	Mail     Mail      `yaml:"mail"`
	Exchange Exchange  `yaml:"exchange"`
	Meetings Meetings  `yaml:"meetings"`
	Log      Log       `yaml:"logging"`
}

// Server holds identity and filesystem settings shared by every subsystem.
type Server struct {
	// Hostname is the name this server announces in SMTP HELO/EHLO responses
	// and uses in Received headers and DKIM checks. It should have a matching
	// DNS A record pointing at PublicIP.
	Hostname string `yaml:"hostname"`
	// PublicIP is the externally visible address of the host, used to
	// advertise the correct EHLO name and to detect connections arriving on
	// the wrong interface.
	PublicIP string `yaml:"public_ip"`
	// DataDir holds the message blob store, DKIM key material, TLS state and
	// any other on-disk state.
	DataDir string `yaml:"data_dir"`
	// TrustedProxies are CIDR ranges of reverse proxies in front of the HTTP
	// listener. X-Forwarded-For is honoured only from these ranges.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// Database describes the PostgreSQL connection and pool.
type Database struct {
	// URL is a libpq/pgx connection string. It is the only mandatory value in
	// the entire document.
	URL string `yaml:"url"`
	// MaxOpen bounds the connection pool.
	MaxOpen int `yaml:"max_open"`
	// MaxIdle bounds idle connections in the pool.
	MaxIdle int `yaml:"max_idle"`
	// MaxLifetime recycles connections to spread server-side load.
	MaxLifetime time.Duration `yaml:"max_lifetime"`
	// ConnectTimeout bounds the initial connection and each dial.
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	// AutoMigrate applies pending embedded migrations at startup.
	AutoMigrate bool `yaml:"auto_migrate"`
}

// Addr is an optional listener address. A zero Addr disables the listener
// entirely, which is how a deployment runs, for example, IMAP behind a proxy
// while still exposing SMTP.
type Addr struct {
	Addr string `yaml:"addr"`
}

// Enabled reports whether this listener should be started.
func (a Addr) Enabled() bool { return a.Addr != "" }

// Listeners enumerates every network listener the server may start.
type Listeners struct {
	SMTPLoopback        Addr  `yaml:"smtp_loopback"`
	SMTPInbound         Addr  `yaml:"smtp_inbound"`
	SMTPSubmission      Addr  `yaml:"smtp_submission"`
	SMTPSubmissionTLS   Addr  `yaml:"smtp_submission_tls"`
	IMAP                Addr  `yaml:"imap"`
	IMAPS               Addr  `yaml:"imaps"`
	POP3                Addr  `yaml:"pop3"`
	POP3S               Addr  `yaml:"pop3s"`
	LMTP                Addr  `yaml:"lmtp"`
	HTTP                Addr  `yaml:"http"`
	HTTPS               Addr  `yaml:"https"`
	SMTPInboundMaxBytes int64 `yaml:"smtp_inbound_max_bytes"`
}

// TLS configures transport security. Exactly one of the static file pair or
// the ACME block is used; see Validate.
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// RedirectHTTP makes the plain HTTP listener answer every request with a
	// permanent redirect to the same URL on HTTPS, so clients that guess
	// http:// end up where they meant to go. ACME HTTP-01 challenges are
	// still served on HTTP: without them no certificate could ever be
	// issued. Defaults to on; set to false to keep serving plain HTTP.
	RedirectHTTP bool `yaml:"redirect_http"`
	ACME         ACME `yaml:"acme"`
}

// ACME enables automatic certificate issuance.
type ACME struct {
	Enabled  bool     `yaml:"enabled"`
	Email    string   `yaml:"email"`
	Domains  []string `yaml:"domains"`
	CacheDir string   `yaml:"cache_dir"`
}

// Auth covers every credential and session policy.
type Auth struct {
	Local LocalAuth `yaml:"local"`
	// EncryptionKey protects second-factor secrets at rest. It is separate
	// from the DKIM key on purpose: those have nothing to do with each other,
	// and sharing one key would mean rotating the DKIM key invalidates every
	// enrolled second factor, or the reverse.
	EncryptionKey string        `yaml:"encryption_key"`
	JWT           JWTAuth       `yaml:"jwt"`
	TOTP          TOTPAuth      `yaml:"totp"`
	Reset         ResetAuth     `yaml:"reset_password"`
	Lockout       Lockout       `yaml:"lockout"`
	Challenge     Challenge     `yaml:"challenge"`
	Session       SessionPolicy `yaml:"session"`
	Impersonation Impersonation `yaml:"impersonation"`
}

// LocalAuth configures stored credentials for local users.
type LocalAuth struct {
	Argon2 Argon2Params `yaml:"argon2"`
	// MinPasswordLength is enforced on every password write.
	MinPasswordLength int `yaml:"min_password_length"`
}

// Argon2Params holds the argon2id cost parameters. The defaults follow the
// OWASP password storage recommendation.
type Argon2Params struct {
	MemoryKib   uint32 `yaml:"memory_kib"`
	Iterations  uint32 `yaml:"iterations"`
	Parallelism uint8  `yaml:"parallelism"`
	SaltLength  uint32 `yaml:"salt_len"`
	KeyLength   uint32 `yaml:"key_len"`
}

// JWTAuth configures the access and refresh tokens used by the web UI and the
// Exchange endpoints.
type JWTAuth struct {
	// Secret signs access tokens. Must be supplied explicitly; a generated
	// value would invalidate every session on restart.
	Secret     string        `yaml:"secret"`
	Issuer     string        `yaml:"issuer"`
	AccessTTL  time.Duration `yaml:"access_ttl"`
	RefreshTTL time.Duration `yaml:"refresh_ttl"`
}

// TOTPAuth configures optional two-factor authentication. TOTP secrets are
// held locally for every user including LDAP users, because the directory does
// not provide them.
type TOTPAuth struct {
	Enabled bool   `yaml:"enabled"`
	Issuer  string `yaml:"issuer"`
	// Window is the number of periods either side of the current one that
	// also verify, to tolerate clock skew.
	Window        uint `yaml:"window"`
	RecoveryCodes int  `yaml:"recovery_codes"`
}

// ResetAuth configures self-service password reset. Available to local users
// only; an LDAP user's password is changed in the directory.
type ResetAuth struct {
	Enabled  bool          `yaml:"enabled"`
	TokenTTL time.Duration `yaml:"token_ttl"`
}

// Lockout throttles repeated authentication failures.
//
// Both scopes are enforced together, because either one alone leaves an obvious
// way through. A per-account limit cannot see one host spraying many accounts;
// a per-IP limit cannot see a slow guess at one account from many hosts.
type Lockout struct {
	Account LockoutScope `yaml:"account"`
	IP      LockoutScope `yaml:"ip"`
}

// LockoutScope is the threshold set for one scope.
type LockoutScope struct {
	// MaxAttempts is how many failures are tolerated inside Window before the
	// subject is locked. Zero or less disables this scope, and a disabled scope
	// records nothing at all.
	MaxAttempts int `yaml:"max_attempts"`
	// Window is the period a run of failures covers. A failure arriving after
	// this has elapsed starts a new run rather than joining the current one.
	Window time.Duration `yaml:"window"`
	// Duration is how long a lock lasts once the threshold is reached.
	Duration time.Duration `yaml:"duration"`
}

// Enabled reports whether this scope does anything.
func (s LockoutScope) Enabled() bool {
	return s.MaxAttempts > 0 && s.Window > 0 && s.Duration > 0
}

// Challenge bounds the gap between a correct password and a second factor.
//
// A user with TOTP enrolled is issued an opaque challenge instead of tokens
// once their password checks out, and the challenge is what the second factor
// step exchanges for a session. Nothing usable exists until the code is
// presented, so an intercepted first response carries no access.
//
// The window is short because it is a pause in one person's login, not a
// resumable session: it has to survive typing a code off a phone, and not much
// more.
type Challenge struct {
	TTL time.Duration `yaml:"ttl"`
}

// SessionPolicy bounds the lifetime of a web session.
type SessionPolicy struct {
	IdleTimeout     time.Duration `yaml:"idle_timeout"`
	AbsoluteTimeout time.Duration `yaml:"absolute_timeout"`
}

// Impersonation controls the domain admins' ability to act as a user.
type Impersonation struct {
	Allowed bool `yaml:"allowed"`
	// RequireReason forces the admin to type a justification, which is
	// recorded in the audit log.
	RequireReason bool `yaml:"require_reason"`
	// RequireGrant makes a specific impersonation_grants row a precondition,
	// so the admin can act as one named user rather than any of them.
	//
	// It is off by default because with it off, Allowed is a blanket power:
	// any admin can act as any user. Turning it on narrows that to the pairs
	// somebody deliberately created, and the grant can then be expired or
	// revoked without touching the configuration.
	RequireGrant bool `yaml:"require_grant"`
}

// LDAP holds directory integration.
type LDAP struct {
	Providers []LDAPProvider `yaml:"providers"`
	Sync      LDAPSync       `yaml:"sync"`
	// CacheTTL bounds how long a successful directory lookup is reused
	// before the live directory is consulted again.
	CacheTTL time.Duration `yaml:"cache_ttl"`
}

// LDAPProvider is one directory server.
type LDAPProvider struct {
	// Name identifies the provider in ldap_links and audit records.
	Name string `yaml:"name"`
	// URL is an ldap:// or ldaps:// address.
	URL string `yaml:"url"`
	// StartTLS upgrades a plaintext ldap:// connection.
	StartTLS bool `yaml:"start_tls"`
	// BindDN and BindPassword are the service account used for searches.
	// A direct user bind is used for authentication when they are empty.
	BindDN       string `yaml:"bind_dn"`
	BindPassword string `yaml:"bind_password"`
	// BaseDN is the search root.
	BaseDN string `yaml:"base_dn"`
	// UserFilter locates a user. {{username}} is substituted with the
	// escaped login name.
	UserFilter string         `yaml:"user_filter"`
	Attributes LDAPAttributes `yaml:"attributes"`
	// DomainAdminsGroupDN is the directory group whose members may log in
	// without mail functionality when their address is off-domain. An AD
	// user whose mail attribute is not in a hosted domain gets a
	// domain_admin account if and only if they are a member of this group;
	// every other off-domain user is refused.
	DomainAdminsGroupDN string `yaml:"domain_admins_group_dn"`
	// MailboxQuotaBytes is applied to mailboxes provisioned by sync.
	MailboxQuotaBytes int64 `yaml:"mailbox_quota_bytes"`
	// DefaultDomain is used when the directory has no mail attribute at all.
	DefaultDomain string `yaml:"default_domain"`
	// Timeout bounds each directory operation.
	Timeout time.Duration `yaml:"timeout"`
}

// LDAPAttributes maps directory fields onto the ones we consume. The first
// attribute present on the entry wins.
type LDAPAttributes struct {
	Mail        []string `yaml:"mail"`
	DisplayName []string `yaml:"display_name"`
	MemberOf    []string `yaml:"member_of"`
}

// LDAPSync configures the periodic reconcile of directory users.
type LDAPSync struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often to reconcile.
	Interval time.Duration `yaml:"interval"`
	// DisableMissingAfter disables a local user that has vanished from the
	// directory for this long, rather than immediately.
	DisableMissingAfter time.Duration `yaml:"disable_missing_after"`
	// PageSize is the directory paged-search size.
	PageSize uint32 `yaml:"page_size"`
}

// Mail holds delivery, policy and quota settings.
type Mail struct {
	Submission Submission `yaml:"submission"`
	Inbound    Inbound    `yaml:"inbound"`
	Outbound   Outbound   `yaml:"outbound"`
	DKIM       DKIM       `yaml:"dkim"`
	// DefaultQuotaBytes is applied to new mailboxes that do not inherit a
	// domain-specific value.
	DefaultQuotaBytes int64 `yaml:"default_quota_bytes"`
	// MaxQuotaBytes caps what a domain admin may grant.
	MaxQuotaBytes int64 `yaml:"max_quota_bytes"`
	// RecipientsPerMessage caps the RCPT TO count on inbound mail.
	RecipientsPerMessage int `yaml:"recipients_per_message"`
	// MaxMessageBytes caps a single message at delivery time.
	MaxMessageBytes int64 `yaml:"max_message_bytes"`
	// DkimPrivateKeyEncryptionKey encrypts DKIM private keys at rest. It is
	// required whenever outbound signing is enabled.
	DkimPrivateKeyEncryptionKey string `yaml:"dkim_private_key_encryption_key"`
}

// Submission configures the authenticated submission listeners.
type Submission struct {
	// RequireTLS refuses AUTH on a connection that has not been upgraded.
	RequireTLS bool `yaml:"require_tls"`
	// AllowInsecureAuth permits AUTH before STARTTLS. It exists for
	// debugging against a plaintext sink and must stay false in production.
	AllowInsecureAuth bool `yaml:"allow_insecure_auth"`
	// EnforceSenderIdentity rejects any MAIL FROM that is not the
	// authenticated principal or one of its aliases.
	EnforceSenderIdentity bool `yaml:"enforce_sender_identity"`
	// DailySendLimit caps messages sent per user per UTC day. Zero disables
	// the cap.
	DailySendLimit int `yaml:"daily_send_limit"`
	// DailySendLimitOverridable lets a domain admin raise or lower the cap
	// for individual users.
	DailySendLimitOverridable bool `yaml:"daily_send_limit_overridable"`
}

// Inbound configures the :25 MTA listener.
type Inbound struct {
	Enabled bool `yaml:"enabled"`
	// RejectUnknownRecipients refuses mail for addresses with no matching
	// user, alias or shared mailbox, rather than accepting and bouncing.
	RejectUnknownRecipients bool `yaml:"reject_unknown_recipients"`
	// MaxRecipients caps RCPT TO on a single transaction.
	MaxRecipients int         `yaml:"max_recipients"`
	Greylisting   Greylisting `yaml:"greylisting"`
	Spam          Spam        `yaml:"spam"`
}

// Greylisting defers first contact from an unknown sender, which most spam
// does not retry.
type Greylisting struct {
	Enabled bool `yaml:"enabled"`
	// Delay is how long a greylisted sender must wait.
	Delay time.Duration `yaml:"delay"`
	// Expiry discards a greylist entry after this long.
	Expiry time.Duration `yaml:"expiry"`
	// RetryWindow is how long past the delay a retry is still accepted.
	RetryWindow time.Duration `yaml:"retry_window"`
}

// Spam controls scoring-based routing.
type Spam struct {
	Enabled bool `yaml:"enabled"`
	// JunkThreshold is the score at or above which mail lands in Junk.
	JunkThreshold float64 `yaml:"junk_threshold"`
	// RejectThreshold is the score at or above which mail is refused at RCPT.
	RejectThreshold float64 `yaml:"reject_threshold"`
}

// Outbound configures the delivery queue and its workers.
type Outbound struct {
	Concurrency int `yaml:"concurrency"`
	// PerDomainConcurrency limits simultaneous connections to one remote
	// domain, which is what remote MTAs expect.
	PerDomainConcurrency int   `yaml:"per_domain_concurrency"`
	Retry                Retry `yaml:"retry"`
	DSN                  DSN   `yaml:"dsn"`
	// ConnectTimeout and CommandTimeout bound each delivery attempt.
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	CommandTimeout time.Duration `yaml:"command_timeout"`
	// SMTPHost is the default smart host for outbound delivery. If empty,
	// the worker resolves MX records for the destination domain.
	SMTPHost string `yaml:"smtp_host"`
	// SMTPPort is the port for the smart host. Default is 587.
	SMTPPort int `yaml:"smtp_port"`
	// SMTPStartTLS asks for STARTTLS when delivering through the smart host.
	// A server whose EHLO advertises no STARTTLS is contacted in the clear
	// rather than refused — an MTA that accepts mail without TLS still
	// accepts the mail — while one that offers STARTTLS and cannot complete
	// the handshake is reported as a failure. When false, the session is in
	// the clear from the first byte.
	SMTPStartTLS bool `yaml:"smtp_starttls"`
	// SMTPUsername and SMTPPassword are used for authentication.
	SMTPUsername string `yaml:"smtp_username"`
	SMTPPassword string `yaml:"smtp_password"`
}

// Retry controls queue backoff.
type Retry struct {
	MaxAttempts int           `yaml:"max_attempts"`
	BackoffBase time.Duration `yaml:"backoff_base"`
	BackoffMax  time.Duration `yaml:"backoff_max"`
	// MaxLifetime abandons a message that has been retrying this long.
	MaxLifetime time.Duration `yaml:"max_lifetime"`
}

// DSN controls delayed bounce message generation.
type DSN struct {
	Enabled bool `yaml:"enabled"`
	// Delay is how long after the final failure a bounce is generated, giving
	// a transiently broken remote host time to recover.
	Delay time.Duration `yaml:"delay"`
	// FromAddress is the envelope sender of generated bounces.
	FromAddress string `yaml:"from_address"`
}

// DKIM configures outbound signing.
type DKIM struct {
	SelectorPrefix string `yaml:"selector_prefix"`
	KeyBits        int    `yaml:"key_bits"`
	// SignAll signs mail from local domains even when a domain has no key
	// yet, generating one on first use.
	SignAll bool `yaml:"sign_all"`
}

// Exchange holds the Microsoft compatibility endpoints.
type Exchange struct {
	Enabled      bool         `yaml:"enabled"`
	Autodiscover Autodiscover `yaml:"autodiscover"`
	EWS          EWS          `yaml:"ews"`
	EAS          EAS          `yaml:"eas"`
	// Organization is reported in Autodiscover and EWS responses.
	Organization string `yaml:"organization"`
	// Advertised is the port set Autodiscover reports to clients. A zero value
	// means the standard port for the protocol, which is what a server on the
	// public internet wants. Anything else - a container publishing 143 as 5143,
	// a reverse proxy on 8443 - has to be stated here, because a client that
	// believes the standard port is talking to a mail server that is not there.
	Advertised Ports `yaml:"advertised"`
}

// Ports are the externally visible ports of the mail and web listeners. Each
// zero entry means the standard port for that service.
type Ports struct {
	IMAP   int `yaml:"imap"`
	IMAPS  int `yaml:"imaps"`
	POP3   int `yaml:"pop3"`
	POP3S  int `yaml:"pop3s"`
	SMTP   int `yaml:"smtp"`
	SMTPS  int `yaml:"smtps"`
	Submit int `yaml:"submission"`
	HTTP   int `yaml:"http"`
	HTTPS  int `yaml:"https"`
}

// Or returns the advertised port for a service, falling back to standard when
// the deployment did not override it.
func (p Ports) Or(advertised, standard int) int {
	if advertised != 0 {
		return advertised
	}
	return standard
}

// Autodiscover serves the MS-OXDISCO POX endpoint.
type Autodiscover struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// EWS serves the Exchange Web Services SOAP endpoint.
type EWS struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// EAS serves Exchange ActiveSync.
type EAS struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
	// MaxPingInterval is the longest interval a device may request between
	// PING calls, in seconds.
	MaxPingInterval int `yaml:"max_ping_interval"`
	// MaxSyncItems caps the number of changes returned by one Sync command.
	MaxSyncItems int `yaml:"max_sync_items"`
}

// Meetings configures scheduled meetings: the invitation emails, the join and
// RSVP links they carry, and the OpenIM deployment that provides each
// meeting's chat room.
type Meetings struct {
	// PublicURL is the base address join and RSVP links point at, for example
	// https://mail.example.com. It is what an invitee outside this network
	// types into a browser, so it has to be the address they can actually
	// reach, not the bind address. Empty derives it from server.hostname with
	// https.
	PublicURL string `yaml:"public_url"`
	// OpenIM is the chat backend. A meeting is still a meeting without it:
	// invitations go out and RSVPs come back either way, and only the chat
	// room is missing.
	OpenIM OpenIM `yaml:"openim"`
}

// OpenIM is an OpenIM server deployment (github.com/openimsdk/open-im-server).
//
// linchange is the identity source: every OpenIM user is a linchange account,
// provisioned on first use and given SDK tokens here, so nobody maintains
// users in two places and the administrator token never leaves this process.
type OpenIM struct {
	Enabled bool `yaml:"enabled"`
	// APIAddr is the OpenIM Platform API base, e.g. http://127.0.0.1:10002.
	APIAddr string `yaml:"api_addr"`
	// WSAddr is the WebSocket gateway the SDK logs in through, e.g.
	// ws://127.0.0.1:10001.
	WSAddr string `yaml:"ws_addr"`
	// AdminUserID and AdminSecret identify linchange to the OpenIM admin API.
	AdminUserID string `yaml:"admin_user_id"`
	AdminSecret string `yaml:"admin_secret"`
}

// MeetingsPublicURL is the base address join and RSVP links are built from.
//
// A deployment that states one gets it verbatim. One that does not gets
// https://<server.hostname>, which is the address an invitee on the public
// internet would use; a server reached over plain HTTP has to say so, because
// there is no way to derive a scheme from a hostname.
func (c *Config) MeetingsPublicURL() string {
	if c.Meetings.PublicURL != "" {
		return c.Meetings.PublicURL
	}
	return "https://" + c.Server.Hostname
}

// Log configures logging and audit.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string `yaml:"level"`
	// Format is json or text.
	Format string `yaml:"format"`
	// File writes logs to this path in addition to stderr. Empty means
	// stderr only.
	File string `yaml:"file"`
	// Audit enables writing an audit_log row for every privileged action.
	Audit bool `yaml:"audit"`
	// AuditRetain bounds audit log growth.
	AuditRetain time.Duration `yaml:"audit_retain"`
}

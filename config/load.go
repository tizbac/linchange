package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrNoPath is returned when no configuration path was supplied.
var ErrNoPath = errors.New("config: no configuration file specified; pass --config <path.yaml>")

// envPattern matches ${VAR} and ${VAR:-fallback} references. A reference with
// no fallback that is unset is an error: silently substituting an empty string
// turns a missing secret into a confusing authentication failure much later.
var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// Load reads, expands, decodes, defaults and validates the configuration
// document at path. Every failure is fatal to the caller: the server never
// runs on a partially-applied configuration.
func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrNoPath
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	// Expansion runs on the parsed tree rather than on the raw text, so that
	// a ${VAR} written inside a comment documenting a provider that is not
	// configured does not turn into a startup failure. Commented-out examples
	// are the normal way to ship a config file.
	var tree yaml.Node
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if err := expandNode(&tree, path); err != nil {
		return nil, err
	}

	// Start from a fully-populated default document, then let YAML overlay
	// only the keys the operator actually wrote. Decoding into a populated
	// struct is what makes an explicit `false` distinguishable from an absent
	// key, which matters for every flag that defaults to on.
	cfg := New()

	dec := yaml.NewDecoder(bytes.NewReader(mustMarshal(&tree)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		// An entirely empty document yields io.EOF, which is not a failure:
		// the defaults are the configuration.
		if !errors.Is(err, errEmptyDocument) {
			return nil, fmt.Errorf("config: parsing %s: %w", path, err)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s is invalid: %w", path, err)
	}
	return cfg, nil
}

var errEmptyDocument = errors.New("EOF")

// mustMarshal re-serialises the expanded tree. A failure here would mean the
// tree is not a valid YAML document, which cannot happen for a tree that was
// just parsed, so panicking is the honest response: returning a nil document
// would silently discard the operator's configuration.
func mustMarshal(n *yaml.Node) []byte {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		panic(fmt.Sprintf("config: re-encoding configuration tree: %v", err))
	}
	enc.Close()
	return buf.Bytes()
}

// expandNode walks a YAML tree and expands ${VAR} references in every string
// value. Keys, non-string scalars and comments are left alone: expanding a key
// would rename a setting, and expanding a comment would make a
// documented-but-unused example a startup failure.
func expandNode(n *yaml.Node, path string) error {
	if n == nil {
		return nil
	}

	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag != "!!str" || !strings.Contains(n.Value, "${") {
			return nil
		}
		expanded, err := ExpandEnv(n.Value, path)
		if err != nil {
			return err
		}
		n.Value = expanded
		return nil

	case yaml.MappingNode:
		// Content alternates key, value, key, value. Only the values are
		// expanded; the keys are setting names, not user data.
		for i := 1; i < len(n.Content); i += 2 {
			if err := expandNode(n.Content[i], path); err != nil {
				return err
			}
		}
		return nil

	case yaml.DocumentNode, yaml.SequenceNode, yaml.AliasNode:
		for _, child := range n.Content {
			if err := expandNode(child, path); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

// ExpandEnv replaces ${VAR} and ${VAR:-fallback} with values from the process
// environment. A bare ${VAR} that is unset is an error.
func ExpandEnv(in, path string) (string, error) {
	var missing []string

	out := envPattern.ReplaceAllStringFunc(in, func(match string) string {
		groups := envPattern.FindStringSubmatch(match)
		name := groups[1]
		hasFallback := strings.Contains(match, ":-")

		if value, ok := os.LookupEnv(name); ok && value != "" {
			return value
		}
		if hasFallback {
			return groups[2]
		}
		missing = append(missing, name)
		return ""
	})

	if len(missing) > 0 {
		return "", fmt.Errorf("config: %s references unset environment variable(s): %s",
			path, strings.Join(dedupe(missing), ", "))
	}
	return out, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// New returns the default configuration. Every field is populated, so callers
// that construct a Config in code (tests, tooling) never have to remember to
// set something.
func New() *Config {
	return &Config{
		Server: Server{
			Hostname:       "localhost",
			PublicIP:       "127.0.0.1",
			DataDir:        "./data",
			TrustedProxies: []string{},
		},
		Database: Database{
			URL:            "",
			MaxOpen:        25,
			MaxIdle:        5,
			MaxLifetime:    30 * time.Minute,
			ConnectTimeout: 10 * time.Second,
			AutoMigrate:    true,
		},
		Listener: Listeners{
			SMTPLoopback:        Addr{Addr: ""},
			SMTPInbound:         Addr{Addr: ":25"},
			SMTPSubmission:      Addr{Addr: ":587"},
			SMTPSubmissionTLS:   Addr{Addr: ":465"},
			IMAP:                Addr{Addr: ":143"},
			IMAPS:               Addr{Addr: ":993"},
			POP3:                Addr{Addr: ":110"},
			POP3S:               Addr{Addr: ":995"},
			LMTP:                Addr{Addr: ""},
			HTTP:                Addr{Addr: ":8080"},
			HTTPS:               Addr{Addr: ""},
			SMTPInboundMaxBytes: 52_428_800,
		},
		TLS: TLS{
			RedirectHTTP: true,
			ACME: ACME{
				Enabled:  false,
				CacheDir: "./data/acme",
			},
		},
		Auth: Auth{
			Local: LocalAuth{
				Argon2: Argon2Params{
					MemoryKib:   65_536,
					Iterations:  3,
					Parallelism: 4,
					SaltLength:  16,
					KeyLength:   32,
				},
				MinPasswordLength: 12,
			},
			JWT: JWTAuth{
				Issuer:     "linchange",
				AccessTTL:  15 * time.Minute,
				RefreshTTL: 720 * time.Hour,
			},
			TOTP: TOTPAuth{
				Enabled:       true,
				Issuer:        "linchange",
				Window:        1,
				RecoveryCodes: 10,
			},
			Reset: ResetAuth{
				Enabled:  true,
				TokenTTL: 30 * time.Minute,
			},
			Lockout: Lockout{
				// The account limit is the strict one: five is the usual
				// recommendation, and it is the limit that actually protects a
				// single mailbox.
				Account: LockoutScope{
					MaxAttempts: 5,
					Window:      15 * time.Minute,
					Duration:    15 * time.Minute,
				},
				// The address limit is looser than the account limit, but only
				// by a factor of two, and the lock is half as long.
				//
				// A school or an office shares one egress address, so this
				// threshold is the one that can lock out a building on a burst
				// of mistyped passwords. It is kept at twice the account limit
				// rather than an order of magnitude above it because the count
				// is failures only: ten genuine failures in fifteen minutes
				// from one address is a spray, not a busy morning, whereas a
				// threshold set high enough to be certain of never catching one
				// would be no limit at all.
				//
				// The half-hour lock follows from the lower threshold. An hour
				// would mean ten typos cost a whole office an hour of mail,
				// which is a worse outage than the one the limit prevents, and
				// the account limit is what actually protects a single mailbox.
				IP: LockoutScope{
					MaxAttempts: 10,
					Window:      15 * time.Minute,
					Duration:    30 * time.Minute,
				},
			},
			Challenge: Challenge{
				// Long enough to find a phone, unlock it and read a code;
				// short enough that a challenge left open in a browser tab is
				// worthless by the time anyone comes back to it.
				TTL: 5 * time.Minute,
			},
			Session: SessionPolicy{
				IdleTimeout:     30 * time.Minute,
				AbsoluteTimeout: 12 * time.Hour,
			},
			Impersonation: Impersonation{
				Allowed:       true,
				RequireReason: true,
				RequireGrant:  true,
			},
		},
		LDAP: LDAP{
			Providers: []LDAPProvider{},
			Sync: LDAPSync{
				Enabled:             false,
				Interval:            15 * time.Minute,
				DisableMissingAfter: 72 * time.Hour,
				PageSize:            500,
			},
			CacheTTL: 5 * time.Minute,
		},
		Mail: Mail{
			Submission: Submission{
				RequireTLS:                true,
				AllowInsecureAuth:         false,
				EnforceSenderIdentity:     true,
				DailySendLimit:            500,
				DailySendLimitOverridable: true,
			},
			Inbound: Inbound{
				Enabled:                 true,
				RejectUnknownRecipients: true,
				MaxRecipients:           100,
				Greylisting: Greylisting{
					Enabled:     true,
					Delay:       5 * time.Minute,
					Expiry:      4 * time.Hour,
					RetryWindow: 30 * time.Minute,
				},
				Spam: Spam{
					Enabled:         true,
					JunkThreshold:   5.0,
					RejectThreshold: 12.0,
				},
			},
			Outbound: Outbound{
				Concurrency:          4,
				PerDomainConcurrency: 2,
				Retry: Retry{
					MaxAttempts: 12,
					BackoffBase: 5 * time.Minute,
					BackoffMax:  12 * time.Hour,
					MaxLifetime: 7 * 24 * time.Hour,
				},
				DSN: DSN{
					Enabled: true,
					Delay:   4 * time.Hour,
				},
				ConnectTimeout: 10 * time.Second,
				CommandTimeout: 2 * time.Minute,
				SMTPHost:       "",
				SMTPPort:       587,
				SMTPStartTLS:   true,
				SMTPUsername:   "",
				SMTPPassword:   "",
			},
			DKIM: DKIM{
				SelectorPrefix: "s",
				KeyBits:        2048,
				SignAll:        true,
			},
			DefaultQuotaBytes:           10_737_418_240,
			MaxQuotaBytes:               107_374_182_400,
			RecipientsPerMessage:        100,
			MaxMessageBytes:             52_428_800,
			DkimPrivateKeyEncryptionKey: "",
		},
		Exchange: Exchange{
			Enabled:      true,
			Organization: "",
			Autodiscover: Autodiscover{Enabled: true, Path: "/autodiscover/autodiscover.xml"},
			EWS:          EWS{Enabled: true, Path: "/ews/exchange.asmx"},
			EAS:          EAS{Enabled: true, Path: "/eas", MaxPingInterval: 3540, MaxSyncItems: 512},
		},
		// Calls default to the server's own STUN/TURN so that a single install
		// works without a second service. The relay range is kept to 100 ports:
		// every one of them has to be opened in a firewall, and a hundred is
		// enough for dozens of simultaneous calls.
		WebRTC: WebRTC{
			TURN: TURN{
				Enabled:       true,
				Realm:         "linchange",
				Listen:        Addr{Addr: ":3478"},
				RelayMinPort:  49152,
				RelayMaxPort:  49251,
				CredentialTTL: 2 * time.Hour,
			},
		},
		Log: Log{
			Level:       "info",
			Format:      "json",
			Audit:       true,
			AuditRetain: 365 * 24 * time.Hour,
		},
	}
}

// TLSEnabled reports whether any TLS terminator is configured.
func (c *Config) TLSEnabled() bool {
	return (c.TLS.CertFile != "" && c.TLS.KeyFile != "") || c.TLS.ACME.Enabled
}

// Dir returns a path inside the configured data directory.
func (c *Config) Dir(parts ...string) string {
	all := append([]string{c.Server.DataDir}, parts...)
	return filepath.Join(all...)
}

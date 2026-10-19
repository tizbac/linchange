package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/mail"
	"os"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/admin"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/logging"
	"github.com/linchange/linchange/internal/store"
)

// runCreateAdmin provisions an account without a web session.
//
// It exists for the first administrator, the one account that cannot be
// created through the admin panel because the admin panel is guarded by an
// administrator. Everything else is better done in the panel, which records
// who asked; this path deliberately reuses the same service so that the
// password policy, the audit entry and the transactional ordering of
// "account and password or neither" are identical to the panel's.
func runCreateAdmin(args []string) error {
	fs := flag.NewFlagSet("create-admin", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `linchange create-admin - provision an account from the command line

Usage:
  linchange create-admin --config <path.yaml> --email <address> [flags]

Examples:
  # The first administrator, holding mail as well as administration rights
  linchange create-admin --config /etc/linchange/linchange.yaml \
      --email admin@example.com --password 'correct horse battery staple'

  # The password read from stdin, so it stays out of the shell history
  printf '%%s\\n' "$PASSWORD" | linchange create-admin \
      --config /etc/linchange/linchange.yaml --email admin@example.com --password -

Flags:
`)
		fs.PrintDefaults()
	}

	var (
		configPath  = fs.String("config", "", "path to the YAML configuration file (required)")
		email       = fs.String("email", "", "address of the account to create (required)")
		password    = fs.String("password", "", "password for the account; reads stdin when set to '-', prompts on a terminal when empty")
		displayName = fs.String("name", "", "display name shown in the web UI (defaults to the local part)")
		mustChange  = fs.Bool("must-change-password", false, "require a password change at the next login")
		noDomain    = fs.Bool("no-create-domain", false, "fail when the address's domain does not exist instead of creating it")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *configPath == "" {
		fs.Usage()
		return config.ErrNoPath
	}

	addr, err := normaliseAddress(*email)
	if err != nil {
		return err
	}
	local, domainName := splitAddress(addr)

	secret, err := resolvePassword(os.Stdin, *password)
	if err != nil {
		return err
	}

	// The configuration is loaded, not merely parsed: a first-run mistake in
	// the database section must be reported here, where the operator is
	// watching, rather than at the next service start.
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	// Command-line output is the operator's console. The configured log file
	// belongs to the service, and may not even exist yet on a fresh install,
	// so opening it here would fail for no good reason.
	cfg.Log.File = ""

	log, err := logging.New(cfg.Log, "linchange")
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db, err := store.OpenFromConfig(ctx, cfg, "linchange")
	if err != nil {
		return err
	}
	defer db.Close()

	if cfg.Database.AutoMigrate {
		if err := store.Migrate(ctx, db, log); err != nil {
			return fmt.Errorf("applying database migrations: %w", err)
		}
	} else {
		// Without this the INSERT below fails with a relation-does-not-exist
		// error that says nothing about the setting that would fix it.
		if err := store.Health(ctx, db); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "note: database.auto_migrate is off, assuming the schema is already applied")
	}

	svc, err := admin.New(admin.Options{
		DB:                db,
		Argon2:            cfg.Auth.Local.Argon2,
		MinPasswordLength: cfg.Auth.Local.MinPasswordLength,
		Policy:            policyFromConfig(cfg),
	})
	if err != nil {
		return err
	}

	// The actor is the account being created. Nothing else exists yet to act
	// as, and an administrator acting on their own row is exactly what
	// happened: this command is run by whoever holds the server.
	actor := policy.Principal{
		Address:        addr,
		Source:         policy.SourceLocal,
		Enabled:        true,
		MailboxEnabled: true,
		IsDomainAdmin:  true,
		Authenticated:  true,
	}

	if !*noDomain {
		if err := ensureDomain(ctx, svc, db, actor, domainName); err != nil {
			return err
		}
	}

	user, err := svc.CreateUser(ctx, actor, admin.CreateUserRequest{
		Address:            addr,
		Source:             policy.SourceLocal,
		DisplayName:        pickName(*displayName, local),
		Password:           secret,
		IsDomainAdmin:      true,
		MustChangePassword: mustChange,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Created administrator %s\n", user.Address)
	if !user.MustChangePassword {
		// Said explicitly because the panel's default is the opposite, and an
		// operator who expected to be forced to change it should learn now
		// rather than at the first login.
		fmt.Println("The password just set is kept as it is; pass --must-change-password to force a change at first login.")
	}
	fmt.Printf("\nNext: start the service with  sudo systemctl start linchange\n")
	return nil
}

// normaliseAddress validates an address and returns it in canonical form.
//
// The service validates addresses too, but only against the schema's rules;
// catching a shell-typo such as a missing @ here gives a message about the
// argument rather than about a database constraint.
func normaliseAddress(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("create-admin: --email is required")
	}
	parsed, err := mail.ParseAddress(raw)
	if err != nil {
		return "", fmt.Errorf("create-admin: --email %q is not an address: %w", raw, err)
	}
	addr := strings.ToLower(strings.TrimSpace(parsed.Address))
	if strings.Count(addr, "@") != 1 || strings.HasPrefix(addr, "@") || strings.HasSuffix(addr, "@") {
		return "", fmt.Errorf("create-admin: --email %q must be a single user@domain address", raw)
	}
	return addr, nil
}

// splitAddress splits "user@example.com" into its local and domain parts. The
// address has already been checked to contain exactly one @.
func splitAddress(addr string) (local, domain string) {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[:i], addr[i+1:]
	}
	return addr, ""
}

// pickName falls back to the address's local part, capitalised. An empty
// display name is legal but shows as a blank in every recipient list, and the
// local part is almost always what the person is called.
func pickName(requested, local string) string {
	if strings.TrimSpace(requested) != "" {
		return strings.TrimSpace(requested)
	}
	// Rune-safe capitalisation: local[:1] would split a multi-byte name.
	r := []rune(local)
	if len(r) == 0 {
		return ""
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// resolvePassword returns the password from the flag, from stdin, or from a
// terminal prompt, in that order of precedence.
func resolvePassword(in *os.File, flagValue string) (string, error) {
	switch {
	case flagValue == "-":
		return readSecret(in, "")
	case flagValue != "":
		// Accepted for scripted installs. A password on the command line is
		// visible to every process on the machine for as long as it runs, so
		// the alternative is documented rather than refused.
		fmt.Fprintln(os.Stderr, "note: reading the password from the command line; prefer --password - to keep it out of the process list")
		return flagValue, nil
	default:
		secret, err := readSecret(in, "Password: ")
		if err != nil {
			return "", fmt.Errorf("create-admin: reading the password: %w", err)
		}
		if secret == "" {
			return "", errors.New("create-admin: the password must not be empty")
		}
		return secret, nil
	}
}

// ensureDomain creates the address's domain when it is missing, so that a
// first administrator is reachable without a second manual step. A domain is
// what makes mail for the address deliverable at all: without the row, the
// account can log in but has no home for inbound mail.
func ensureDomain(ctx context.Context, svc *admin.Service, db *sqlx.DB, actor policy.Principal, name string) error {
	if name == "" {
		return nil
	}
	_, err := store.NewDomains().Get(ctx, db, name)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("create-admin: looking up domain %s: %w", name, err)
	}

	created, err := svc.CreateDomain(ctx, actor, store.CreateDomainRequest{
		Name: name,
		// Local means this server accepts mail for the domain; a relay-only
		// domain is the exception, and is one admin-panel edit away.
		IsLocal:     true,
		Description: "created by linchange create-admin",
	})
	if err != nil {
		return fmt.Errorf("create-admin: creating domain %s: %w", name, err)
	}
	fmt.Printf("Created domain %s (accepts mail locally)\n", created.Name)
	return nil
}

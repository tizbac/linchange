package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/admin"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
)

// runCreateAdmin provisions an administrator account from the command line, for
// the moment before any web interface is reachable: a fresh deployment has no
// account to sign in with, and the account that can create the others is the
// one that has to be created out of band.
//
// The account is a domain admin holding a mailbox, the usual shape of a small
// deployment's administrator: one person who both receives mail and runs the
// server. When no password is given, a random one is printed once and the
// account must change it at first sign-in, because an administrator who did not
// choose their own password cannot be trusted to keep it.
func runCreateAdmin(args []string) error {
	fs := flag.NewFlagSet("linchange create-admin", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `linchange create-admin - provision an administrator account

Usage:
  linchange create-admin --config <path.yaml> --email <address> [flags]

Flags:
`)
		fs.PrintDefaults()
	}

	var (
		configPath = fs.String("config", "", "path to the YAML configuration file (required)")
		email      = fs.String("email", "", "address of the account to create (required)")
		password   = fs.String("password", "", "password for the account; a random one is generated and printed when empty")
		name       = fs.String("name", "", "display name for the account")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *configPath == "" || *email == "" {
		fs.Usage()
		return errors.New("--config and --email are both required")
	}
	if !strings.Contains(*email, "@") {
		return fmt.Errorf("%q is not an address", *email)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	db, err := store.OpenFromConfig(ctx, cfg, "linchange-create-admin")
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer db.Close()

	if cfg.Database.AutoMigrate {
		if err := store.Migrate(ctx, db, nil); err != nil {
			return fmt.Errorf("applying migrations: %w", err)
		}
	}

	// The account's domain has to exist before the account does, and a first
	// administrator is usually the domain's first account too. An existing
	// domain is left alone.
	at := strings.LastIndex(*email, "@")
	domain := (*email)[at+1:]
	if _, err := db.ExecContext(ctx,
		`INSERT INTO domains (name) VALUES ($1) ON CONFLICT DO NOTHING`, domain); err != nil {
		return fmt.Errorf("ensuring the domain %s exists: %w", domain, err)
	}

	adm, err := admin.New(admin.Options{
		DB:                db,
		Argon2:            cfg.Auth.Local.Argon2,
		MinPasswordLength: cfg.Auth.Local.MinPasswordLength,
	})
	if err != nil {
		return err
	}

	pw := *password
	generated := false
	if pw == "" {
		pw = generatePassword()
		generated = true
	}
	// A password the operator typed is one they chose for that account; a
	// generated one is a secret nobody can guess a second time, so the account
	// is told to replace it before it does anything else.
	mustChange := generated

	// The command line is the operator, not one of the accounts it creates, so
	// the audit entry says where the change came from. The grant it carries is
	// what admin.Service checks before it writes anything. Reaching this
	// command means holding the configuration file and the shell it points
	// at, which is the authentication this grant rests on.
	actor := policy.Principal{
		Address:        "create-admin@" + cfg.Server.Hostname,
		Source:         policy.SourceLocal,
		Enabled:        true,
		MailboxEnabled: true,
		IsDomainAdmin:  true,
		Authenticated:  true,
	}
	user, err := adm.CreateUser(ctx, actor, admin.CreateUserRequest{
		Address:            *email,
		Source:             policy.SourceLocal,
		DisplayName:        *name,
		Password:           pw,
		MustChangePassword: &mustChange,
		IsDomainAdmin:      true,
	})
	if err != nil {
		return err
	}

	// The standard folders are part of a usable mailbox: IMAP, EWS and EAS
	// all address them by name, so an account without them cannot sync.
	if err := imapd.NewMailboxRepo().EnsureFolders(ctx, db, user.Address); err != nil {
		return fmt.Errorf("creating the standard folders: %w", err)
	}

	fmt.Printf("created %s as a domain administrator\n", user.Address)
	if generated {
		fmt.Printf("password: %s\n", pw)
		fmt.Print("(printed once; the account must change it at first sign-in)\n")
	}
	return nil
}

// generatePassword answers with crypto/rand's own text: random, long enough
// for any minimum, and spelled without the characters that are easy to mistype
// or read aloud wrongly.
func generatePassword() string {
	return rand.Text()
}

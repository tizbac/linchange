// Command seed provisions a disposable linchange database for the browser
// end-to-end tests. It loads the same configuration file the server runs on,
// creates a fresh database, applies the migrations, and inserts a domain, a
// local user, their standard folders and a known password, so the tests can
// sign in through the real login screen.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
)

// dbNamePattern is what PostgreSQL accepts unquoted, so the name can be
// spliced into CREATE DATABASE without escaping.
var dbNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfgPath = flag.String("config", "", "path to the linchange configuration (required)")
		name    = flag.String("database", "", "name of the database to create (required)")
		user    = flag.String("user", "user@example.com", "address of the user to create")
		pass    = flag.String("password", "correct horse battery staple", "password for the user")
	)
	flag.Parse()
	if *cfgPath == "" || *name == "" {
		return fmt.Errorf("-config and -database are both required")
	}
	if !dbNamePattern.MatchString(*name) {
		return fmt.Errorf("%q is not a usable database name", *name)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := store.OpenFromConfig(ctx, cfg, "linchange-seed-admin")
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer admin.Close()

	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+*name+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("dropping %s: %w", *name, err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+*name); err != nil {
		return fmt.Errorf("creating %s: %w", *name, err)
	}

	db, err := store.Open(ctx, store.Options{
		URL: replaceDatabase(cfg.Database.URL, *name), MaxOpen: 4,
		ConnectTimeout: 10 * time.Second, ApplicationName: "linchange-seed",
	})
	if err != nil {
		return fmt.Errorf("opening %s: %w", *name, err)
	}
	defer db.Close()

	if err := store.Migrate(ctx, db, nil); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}

	domain, local, found := splitAddress(*user)
	if !found {
		return fmt.Errorf("%q is not an address", *user)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO domains (name) VALUES ($1)", domain); err != nil {
		return fmt.Errorf("creating domain %s: %w", domain, err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (address, address_local, source, mailbox_enabled, enabled)
		 VALUES ($1, $2, 'local', true, true)`, *user, local); err != nil {
		return fmt.Errorf("creating user %s: %w", *user, err)
	}
	if err := imapd.NewMailboxRepo().EnsureFolders(ctx, db, *user); err != nil {
		return fmt.Errorf("creating folders for %s: %w", *user, err)
	}

	hash, err := password.Hash(*pass, cfg.Auth.Local.Argon2)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}
	if err := store.NewCredentials().Set(ctx, db, *user, hash); err != nil {
		return fmt.Errorf("storing password: %w", err)
	}

	fmt.Printf("seeded %s in database %q\n", *user, *name)
	return nil
}

func splitAddress(address string) (domain, local string, ok bool) {
	i := strings.LastIndex(address, "@")
	if i < 0 || i == 0 || i == len(address)-1 {
		return "", "", false
	}
	return address[i+1:], address[:i], true
}

func replaceDatabase(dsn, name string) string {
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+1:]
	if q := strings.Index(rest, "?"); q >= 0 {
		rest = rest[:q]
	}
	return dsn[:i+1] + name + dsn[i+1+len(rest):]
}

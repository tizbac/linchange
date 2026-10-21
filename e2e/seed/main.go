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
	"github.com/linchange/linchange/internal/auth/policy"
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

	// A shared mailbox the seeded user belongs to: the end-to-end suites check
	// that a member sees the team mailbox in the folder tree, reads its mail,
	// and can send under its address, and that somebody without a grant sees
	// none of it. That needs the mailbox, a granted member, and an ungranted
	// second user to exist before the first request is made.
	shared := "team@example.com"
	sharedLocal, _, ok := splitAddress(shared)
	if !ok {
		return fmt.Errorf("shared address %q is not an address", shared)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (address, address_local, source, mailbox_enabled, enabled)
		 VALUES ($1, $2, 'shared', true, true)`, shared, sharedLocal); err != nil {
		return fmt.Errorf("creating shared principal %s: %w", shared, err)
	}
	if err := store.NewSharedMailboxes().Declare(ctx, db, store.CreateSharedMailboxRequest{
		Address:     shared,
		DisplayName: "Team Mailbox",
		Description: "Shared mailbox exercised by the end-to-end suites",
	}); err != nil {
		return fmt.Errorf("declaring shared mailbox %s: %w", shared, err)
	}
	if err := store.NewSharedMailboxGrants().Grant(ctx, db, shared, *user, policy.GrantEditor, nil); err != nil {
		return fmt.Errorf("granting %s access to %s: %w", *user, shared, err)
	}
	if err := imapd.NewMailboxRepo().EnsureFolders(ctx, db, shared); err != nil {
		return fmt.Errorf("creating folders for %s: %w", shared, err)
	}

	// The outsider has a mailbox and a password but no grant on the team
	// mailbox, so the suites can ask what a non-member gets and expect nothing.
	outsider := "e2e-other@example.com"
	outsiderLocal, _, ok := splitAddress(outsider)
	if !ok {
		return fmt.Errorf("outsider address %q is not an address", outsider)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (address, address_local, source, mailbox_enabled, enabled)
		 VALUES ($1, $2, 'local', true, true)`, outsider, outsiderLocal); err != nil {
		return fmt.Errorf("creating user %s: %w", outsider, err)
	}
	if err := imapd.NewMailboxRepo().EnsureFolders(ctx, db, outsider); err != nil {
		return fmt.Errorf("creating folders for %s: %w", outsider, err)
	}
	if err := store.NewCredentials().Set(ctx, db, outsider, hash); err != nil {
		return fmt.Errorf("storing password for %s: %w", outsider, err)
	}

	// The organization the seeded domain belongs to, with a group inside it.
	// Membership is by domain, so nobody is added by hand; the suites need the
	// group because a calendar shared with one has to be offered by the name
	// people say out loud rather than by the number its row carries.
	wg, err := store.NewWorkgroups().Create(ctx, db, store.CreateWorkgroupRequest{
		Name:        "Example",
		Domain:      domain,
		DisplayName: "Example",
		Description: "Organization seeded for the end-to-end suites",
	})
	if err != nil {
		return fmt.Errorf("creating organization: %w", err)
	}
	group, err := store.NewGroups().Create(ctx, db, wg.ID, "Team", "The seeded group")
	if err != nil {
		return fmt.Errorf("creating group: %w", err)
	}
	// One member, so the group reads as a set of people wherever it is offered
	// rather than as an empty name with nothing behind it.
	if err := store.NewGroups().AddMember(ctx, db, group.ID, outsider); err != nil {
		return fmt.Errorf("adding %s to the seeded group: %w", outsider, err)
	}

	fmt.Printf("seeded %s in database %q (shared mailbox %s, outsider %s)\n",
		*user, *name, shared, outsider)
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

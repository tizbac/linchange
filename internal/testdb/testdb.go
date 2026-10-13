// Package testdb hands each test its own database.
//
// It exists so that the connection details a test needs are written once. The
// alternative is a copy of this harness in every package that touches the
// database, and copies drift: a test suite that quietly points at a different
// instance than the Makefile does passes while testing the wrong thing.
//
// The behaviour that matters is that a test which fails or is interrupted does
// not leave a database behind, and that a test which leaves rows behind cannot
// affect any other test. Both are per-database, so both hold.
package testdb

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/store"
)

// URLEnv names the environment variable that overrides where tests connect.
const URLEnv = "LINCHANGE_TEST_DATABASE_URL"

// defaultURL mirrors docker-compose.yml and the Makefile. Keeping one source
// of truth per file is not possible in Go, which is why the DSN lives here
// rather than being restated by each suite.
const defaultURL = "postgres://linchange:linchange@127.0.0.1:55432/linchange?sslmode=disable"

// URL reports where tests are pointed, and whether that was chosen by the
// operator rather than defaulted.
func URL() (string, bool) {
	if base := os.Getenv(URLEnv); base != "" {
		return base, true
	}
	return defaultURL, false
}

// New returns a handle to an empty database belonging to this test alone.
//
// When the operator has named a database and it cannot be reached, that is a
// failure rather than a skip: a suite that skips its own database tests still
// reports "ok", which is how a broken environment goes unnoticed.
func New(t *testing.T) *sqlx.DB {
	t.Helper()

	base, operatorSet := URL()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := store.Open(ctx, store.Options{
		URL:             base,
		MaxOpen:         2,
		ConnectTimeout:  10 * time.Second,
		ApplicationName: "linchange-test-admin",
	})
	if err != nil {
		if operatorSet {
			t.Fatalf("%s is set to %q but is unreachable: %v", URLEnv, base, err)
		}
		t.Skipf("no test database at %s (%v); run `make up` or set %s", base, err, URLEnv)
	}

	// Isolated from every other test, so rows left behind by a failing test
	// cannot be seen by the next one.
	name := fmt.Sprintf("lc_test_%d_%d", time.Now().UnixNano(), rand.Int63())
	t.Logf("Creating test database: %s", name)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("creating test database: %v", err)
	}

	newDSN := replaceDatabase(base, name)
	t.Logf("Connecting to test database: %s", newDSN)

	db, err := store.Open(ctx, store.Options{
		URL:             newDSN,
		MaxOpen:         8,
		MaxIdle:         4,
		ConnectTimeout:  10 * time.Second,
		ApplicationName: "linchange-test",
	})
	if err != nil {
		admin.Close()
		t.Fatalf("opening test database: %v", err)
	}

	t.Cleanup(func() {
		db.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			// A leftover test database is untidy but not a test failure.
			t.Logf("cleanup: dropping %s: %v", name, err)
		}
		admin.Close()
	})

	return db
}

func Migrated(t *testing.T) *sqlx.DB {
	t.Helper()

	db := New(t)
	t.Logf("Connected to database, checking tables...")
	var tables []string
	err := db.SelectContext(context.Background(), &tables, `
		SELECT table_name FROM information_schema.tables 
		WHERE table_schema = 'public' AND table_name IN ('workgroups', 'schema_migrations')
	`)
	if err != nil {
		t.Logf("Error checking tables: %v", err)
	} else {
		t.Logf("Tables found: %v", tables)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := store.Migrate(ctx, db, nil); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return db
}

func replaceDatabase(dsn, name string) string {
	// Deliberately simple: the test DSN is expected to be a plain
	// postgres://host:port/db form, and anything richer is a local setup
	// problem worth surfacing loudly.
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

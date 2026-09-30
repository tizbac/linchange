package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/store"
)

// testDatabaseURL is where the store tests expect a PostgreSQL instance.
// Override it in the environment to point at a different instance, for example
// a throwaway container on CI.
const testDatabaseURLEnv = "LINCHANGE_TEST_DATABASE_URL"

// defaultTestURL mirrors docker-compose.yml and the Makefile. Keeping one
// source of truth per file is not possible in Go, so this must be updated
// alongside any change to the development credentials.
const defaultTestURL = "postgres://linchange:linchange@127.0.0.1:55432/linchange?sslmode=disable"

// Each test gets its own database so that a test which leaves rows behind, or
// a migration which is deliberately re-run, cannot affect any other.
func testDB(t *testing.T) *sqlx.DB {
	t.Helper()

	base := os.Getenv(testDatabaseURLEnv)
	if base == "" {
		// Matches the credentials in docker-compose.yml and the Makefile, so
		// that `make up` followed by `go test` exercises the database rather
		// than quietly skipping. A mismatch here used to be invisible: the
		// connection error was turned into a skip, and the package still
		// reported "ok".
		base = defaultTestURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := store.Open(ctx, store.Options{
		URL:             base,
		MaxOpen:         2,
		ConnectTimeout:  10 * time.Second,
		ApplicationName: "linchange-test-admin",
	})
	if err != nil {
		if os.Getenv(testDatabaseURLEnv) != "" {
			// The operator pointed us at a database. If we cannot reach it,
			// that is a failure, not a reason to skip.
			t.Fatalf("%s is set to %q but is unreachable: %v",
				testDatabaseURLEnv, base, err)
		}
		t.Skipf("no test database at %s (%v); run `make up` or set %s",
			testDatabaseURLEnv, err, testDatabaseURLEnv)
	}
	defer admin.Close()

	// Isolate this test in its own database.
	name := fmt.Sprintf("lc_test_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}

	dsn := replaceDatabase(base, name)
	db, err := store.Open(ctx, store.Options{
		URL:             dsn,
		MaxOpen:         8,
		MaxIdle:         4,
		ConnectTimeout:  10 * time.Second,
		ApplicationName: "linchange-test",
	})
	if err != nil {
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

func TestOpenRequiresURL(t *testing.T) {
	if _, err := store.Open(context.Background(), store.Options{}); err == nil {
		t.Fatal("an empty URL should be rejected")
	}
}

func TestOpenFailsFastOnBadDSN(t *testing.T) {
	// Open must verify the connection, so a bad DSN fails here rather than on
	// the first inbound mail delivery.
	_, err := store.Open(context.Background(), store.Options{
		URL:            "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable",
		MaxOpen:        1,
		ConnectTimeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected a connection failure")
	}
}

func TestOpenSetsApplicationName(t *testing.T) {
	db := testDB(t)

	var name string
	if err := db.GetContext(context.Background(), &name, "SHOW application_name"); err != nil {
		t.Fatalf("reading application_name: %v", err)
	}
	if name != "linchange-test" {
		t.Errorf("application_name = %q, want %q", name, "linchange-test")
	}
}

func TestHealth(t *testing.T) {
	db := testDB(t)
	if err := store.Health(context.Background(), db); err != nil {
		t.Errorf("health on a live database: %v", err)
	}

	db.Close()
	if err := store.Health(context.Background(), db); err == nil {
		t.Error("health on a closed database should fail")
	}
}

func TestMigrateAppliesSchema(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// A fresh database starts at version 0 with no migration table at all.
	if _, err := db.ExecContext(ctx, "SELECT 1 FROM schema_migrations"); err == nil {
		t.Error("schema_migrations should not exist before migrating")
	}

	if err := store.Migrate(ctx, db, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	version, err := store.SchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version == 0 {
		t.Error("schema version should be non-zero after migrating")
	}

	if err := store.VerifySchema(ctx, db); err != nil {
		t.Errorf("verify: %v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := store.Migrate(ctx, db, nil); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	first, err := store.SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	// Running again must be a no-op, not an error and not a re-apply.
	for i := 0; i < 3; i++ {
		if err := store.Migrate(ctx, db, nil); err != nil {
			t.Fatalf("repeat migrate %d: %v", i, err)
		}
	}

	second, err := store.SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("schema version changed on re-run: %d then %d", first, second)
	}

	var applied int
	if err := db.GetContext(ctx, &applied, "SELECT count(*) FROM schema_migrations"); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Errorf("expected 1 recorded migration, got %d", applied)
	}
}

// The checksum guard is what stops an applied migration from being edited in
// place, which would otherwise leave the database and the binary silently
// disagreeing about the schema.
func TestMigrateDetectsEditedMigration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := store.Migrate(ctx, db, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Simulate an operator having changed the recorded checksum.
	_, err := db.ExecContext(ctx, "UPDATE schema_migrations SET checksum = '\\x00'")
	if err != nil {
		t.Fatal(err)
	}

	err = store.Migrate(ctx, db, nil)
	if err == nil {
		t.Fatal("expected an error when a recorded checksum no longer matches")
	}
	if !strings.Contains(err.Error(), "modified after being applied") {
		t.Errorf("error should explain the drift, got: %v", err)
	}
}

func TestVerifySchemaFailsOnOldDatabase(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Pretend the database was migrated to a state this binary knows nothing
	// about.
	_, err := db.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			version bigint PRIMARY KEY, name text NOT NULL,
			checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now(),
			duration interval NOT NULL DEFAULT interval '0');
		INSERT INTO schema_migrations (version, name, checksum) VALUES (0, 'none', '\\x00')`)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.VerifySchema(ctx, db); err == nil {
		t.Fatal("VerifySchema should fail when the database is behind")
	}
}

func TestInTxCommitsOnSuccess(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	mustMigrate(t, db)

	if err := store.InTx(ctx, db, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO domains (name) VALUES ($1)", "commit.example.com")
		return err
	}); err != nil {
		t.Fatalf("InTx: %v", err)
	}

	var name string
	err := db.GetContext(ctx, &name, "SELECT name::text FROM domains WHERE name = $1", "commit.example.com")
	if err != nil {
		t.Fatalf("row should have been committed: %v", err)
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	mustMigrate(t, db)

	sentinel := errors.New("deliberate failure")
	err := store.InTx(ctx, db, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO domains (name) VALUES ($1)", "rollback.example.com"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx should return the callback error, got %v", err)
	}

	var count int
	if err := db.GetContext(ctx, &count, "SELECT count(*) FROM domains"); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected the insert to be rolled back, found %d rows", count)
	}
}

// A panic must not be swallowed into a clean rollback, or a bug would look
// like a successful no-op.
func TestInTxReraisesPanic(t *testing.T) {
	db := testDB(t)
	mustMigrate(t, db)

	panicked := func() (was bool) {
		defer func() {
			if r := recover(); r != nil {
				was = true
			}
		}()
		_ = store.InTx(context.Background(), db, func(*sqlx.Tx) error {
			panic("bug in the callback")
		})
		return false
	}()

	if !panicked {
		t.Error("a panic in the callback should propagate")
	}
}

func TestInTxRollsBackOnPanic(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	mustMigrate(t, db)

	func() {
		defer func() { _ = recover() }()
		_ = store.InTx(ctx, db, func(tx *sqlx.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO domains (name) VALUES ($1)", "panic.example.com")
			if err != nil {
				return err
			}
			panic("boom")
		})
	}()

	var count int
	if err := db.GetContext(ctx, &count, "SELECT count(*) FROM domains"); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("the insert should have been rolled back, found %d rows", count)
	}
}

func TestIsNotFound(t *testing.T) {
	if !store.IsNotFound(sql.ErrNoRows) {
		t.Error("sql.ErrNoRows should be recognised as not-found")
	}
	if !store.IsNotFound(store.ErrNotFound) {
		t.Error("store.ErrNotFound should be recognised as not-found")
	}
	if store.IsNotFound(errors.New("some other problem")) {
		t.Error("an unrelated error should not be treated as not-found")
	}
	if store.IsNotFound(nil) {
		t.Error("nil should not be treated as not-found")
	}
}

func TestIsConflict(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := store.Migrate(ctx, db, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, "INSERT INTO domains (name) VALUES ('dup.example.com')"); err != nil {
		t.Fatal(err)
	}
	_, err := db.ExecContext(ctx, "INSERT INTO domains (name) VALUES ('dup.example.com')")
	if err == nil {
		t.Fatal("expected a unique violation")
	}
	if !store.IsConflict(err) {
		t.Errorf("a unique violation should be recognised as a conflict, got: %v", err)
	}
}

// A test-only check that the schema's central invariant holds at the database
// level rather than only in application code.
func TestSchemaEnforcesCaseInsensitiveUniqueness(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := store.Migrate(ctx, db, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx,
		"INSERT INTO users (address, source, address_local) VALUES ('Alice@Example.com','local','alice@example.com')"); err != nil {
		t.Fatal(err)
	}
	_, err := db.ExecContext(ctx,
		"INSERT INTO users (address, source, address_local) VALUES ('ALICE@EXAMPLE.COM','local','alice@example.com')")
	if err == nil {
		t.Fatal("citext uniqueness should treat differently-cased addresses as equal")
	}
	if !store.IsConflict(err) {
		t.Errorf("expected a conflict, got: %v", err)
	}
}

// mustMigrate brings a test database to the current schema, failing the test
// if it cannot.
func mustMigrate(t *testing.T, db *sqlx.DB) {
	t.Helper()
	if err := store.Migrate(context.Background(), db, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

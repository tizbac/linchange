package store_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
	"github.com/linchange/linchange/migrations"
)

// Each test gets its own database so that a test which leaves rows behind, or
// a migration which is deliberately re-run, cannot affect any other. The
// harness itself lives in internal/testdb, so the connection details these
// tests rely on are written down once rather than copied per package.
func testDB(t *testing.T) *sqlx.DB {
	t.Helper()
	return testdb.New(t)
}

// Tests that assert on schema behaviour use this rather than testDB, because
// the rules they check live in the migrations, not in the connection layer.
func testMigratedDB(t *testing.T) *sqlx.DB {
	t.Helper()
	return testdb.Migrated(t)
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

	// Compared against the embedded set rather than a literal, so that adding a
	// migration does not turn this into a test that has to be edited in step
	// with the schema.
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("reading embedded migrations: %v", err)
	}

	var applied int
	if err := db.GetContext(ctx, &applied, "SELECT count(*) FROM schema_migrations"); err != nil {
		t.Fatal(err)
	}
	if applied != len(all) {
		t.Errorf("recorded %d migrations, want %d", applied, len(all))
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

// testMigratedDB returns a test database with the current schema applied.

// A database that is ahead of the binary is the other half of the checksum
// guard. The checksum loop only walks migrations this build carries, so a
// version it has never heard of is invisible: pending comes back empty and the
// schema is reported as up to date while the code is older than the data. This
// was not theoretical, it is what a stale binary does after a migration is
// added.
func TestMigrateRefusesADatabaseAheadOfTheBinary(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := store.Migrate(ctx, db, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Pretend a newer build applied a migration this one does not carry.
	latest, err := migrations.LatestVersion()
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, duration)
		 VALUES ($1, 'from_the_future', $2, interval '0')`,
		latest+1, []byte("not a real checksum")); err != nil {
		t.Fatalf("recording a future migration: %v", err)
	}

	err = store.Migrate(ctx, db, nil)
	if err == nil {
		t.Fatal("an older binary started against a newer schema")
	}
	if !strings.Contains(err.Error(), "only knows up to") {
		t.Errorf("error should explain the version mismatch, got: %v", err)
	}
}

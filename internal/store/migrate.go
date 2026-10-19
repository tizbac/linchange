package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/migrations"
)

// migrationTable records which migrations have been applied.
//
// It is created on demand rather than being part of a migration, because the
// table has to exist before any migration can consult it.
const migrationTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     bigint      PRIMARY KEY,
    name        text        NOT NULL,
    checksum    bytea       NOT NULL,
    applied_at  timestamptz NOT NULL DEFAULT now(),
    -- How long the migration took, which is how a bad migration is found.
    duration    interval    NOT NULL DEFAULT interval '0'
)`

// Migrate brings the schema up to the latest embedded version.
//
// Each migration runs in its own transaction together with the insert that
// records it, so a failure cannot leave a version marked as applied that was
// not. Migrations already applied are skipped by version; a change to the
// contents of an applied migration is reported as an error rather than
// ignored, because that means the database and the binary disagree.
func Migrate(ctx context.Context, db *sqlx.DB, log *slog.Logger) error {
	if _, err := db.ExecContext(ctx, migrationTable); err != nil {
		return fmt.Errorf("store: creating migration table: %w", err)
	}

	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return err
	}

	all, err := migrations.All()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}

	pending := make([]migrations.Migration, 0, len(all))
	for _, m := range all {
		if _, done := applied[m.Version]; done {
			continue
		}
		pending = append(pending, m)
	}

	// Refuse to run when the database is ahead of this binary. The loop below
	// only walks the migrations this binary carries, so a version applied here
	// that the binary has never heard of is invisible to it: pending comes back
	// empty and the schema is reported as up to date while the code is older
	// than the data. For a mail server that is a downgrade that runs happily
	// and fails later, in the worst place, so it is stopped at startup.
	if newest := all[len(all)-1].Version; hasNewer(applied, newest) {
		return fmt.Errorf(
			"store: the database is at schema version %d but this binary only "+
				"knows up to %d; refusing to start an older binary against a "+
				"newer schema, so deploy the newer build first",
			newestApplied(applied), newest)
	}

	// Verify that nothing already applied has been edited. This runs before
	// the no-op short circuit below: a database that is fully migrated is
	// exactly the case where an edited migration would otherwise go unnoticed
	// forever, because there would be no pending work to trip over.
	for _, m := range all {
		prev, ok := applied[m.Version]
		if !ok {
			continue
		}
		if !bytes.Equal(prev.checksum, checksum(m.SQL)) {
			return fmt.Errorf(
				"store: migration %04d_%s was modified after being applied "+
					"(recorded checksum %x, file checksum %x); "+
					"write a new migration instead of editing an applied one",
				m.Version, m.Name, prev.checksum, checksum(m.SQL))
		}
	}

	if len(pending) == 0 {
		if log != nil {
			log.Info("schema up to date", "version", all[len(all)-1].Version)
		}
		return nil
	}

	sort.Slice(pending, func(i, j int) bool { return pending[i].Version < pending[j].Version })

	if log != nil {
		log.Info("applying migrations", "count", len(pending), "from", pending[0].Version)
	}

	for _, m := range pending {
		started := time.Now()
		err := applyMigration(ctx, db, m)
		if err != nil {
			return fmt.Errorf("store: applying migration %04d_%s: %w", m.Version, m.Name, err)
		}
		if log != nil {
			log.Info("migration applied",
				"version", m.Version,
				"name", m.Name,
				"duration", time.Since(started).Round(time.Millisecond).String())
		}
	}

	return nil
}

// applyMigration runs one migration and records it atomically.
func applyMigration(ctx context.Context, db *sqlx.DB, m migrations.Migration) error {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// CREATE INDEX CONCURRENTLY and a few other statements cannot run inside a
	// transaction. A future migration needing that must be marked, so this
	// stays strict here rather than quietly running outside a transaction.
	started := time.Now()
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("executing: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO schema_migrations (version, name, checksum, duration)
		VALUES ($1, $2, $3, $4)`,
		m.Version, m.Name, checksum(m.SQL), time.Since(started))
	if err != nil {
		return fmt.Errorf("recording migration: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	return nil
}

type appliedMigration struct {
	name     string
	checksum []byte
}

// hasNewer reports whether any applied version is past the newest this binary
// carries.
func hasNewer(applied map[int64]appliedMigration, newest int64) bool {
	return newestApplied(applied) > newest
}

// newestApplied returns the highest applied version, or 0 when nothing has been
// applied.
func newestApplied(applied map[int64]appliedMigration) int64 {
	var newest int64
	for version := range applied {
		if version > newest {
			newest = version
		}
	}
	return newest
}

// checksum is the SHA-256 of a migration's SQL text. It detects an applied
// migration being edited in place, which would otherwise silently leave the
// database and the binary describing different schemas.
func checksum(sqlText string) []byte {
	sum := sha256.Sum256([]byte(sqlText))
	return sum[:]
}

func appliedMigrations(ctx context.Context, db *sqlx.DB) (map[int64]appliedMigration, error) {
	rows, err := db.QueryxContext(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: reading applied migrations: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]appliedMigration)
	for rows.Next() {
		var (
			version int64
			rec     appliedMigration
		)
		if err := rows.Scan(&version, &rec.name, &rec.checksum); err != nil {
			return nil, fmt.Errorf("store: scanning applied migrations: %w", err)
		}
		out[version] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating applied migrations: %w", err)
	}
	return out, nil
}

// SchemaVersion returns the highest applied migration version.
func SchemaVersion(ctx context.Context, db *sqlx.DB) (int64, error) {
	var version sql.NullInt64
	err := db.GetContext(ctx, &version, `SELECT max(version) FROM schema_migrations`)
	if err != nil {
		if IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: reading schema version: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return version.Int64, nil
}

// VerifySchema reports whether the database is at the version this binary
// expects. It is used by non-migrating entrypoints, such as a CLI running
// against a database managed by the server.
func VerifySchema(ctx context.Context, db *sqlx.DB) error {
	current, err := SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	expected, err := migrations.LatestVersion()
	if err != nil {
		return err
	}
	if current < expected {
		return fmt.Errorf("store: database schema is at version %d but this binary requires %d; "+
			"start the server once with database.auto_migrate enabled, or run the migration step",
			current, expected)
	}
	return nil
}

// Package store holds the PostgreSQL access layer.
//
// Every repository takes a *sqlx.DB (or a *sqlx.Tx) so that a caller can
// compose several repositories into one transaction. Nothing in this package
// opens a connection of its own.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	// Registers the "pgx" driver with database/sql, which is how sqlx reaches
	// PostgreSQL. The import must live in the package that calls sqlx.Open,
	// otherwise the driver is missing at runtime and Open fails with an
	// "unknown driver" error that no compiler catches.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
)

// Errors returned by repositories. Callers distinguish these rather than
// string-matching driver errors.
var (
	// ErrNotFound is returned when a lookup matches no row.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned when a unique constraint would be violated.
	ErrConflict = errors.New("store: already exists")
	// ErrQuotaExceeded is returned when an operation would push a mailbox
	// over its owner's quota.
	ErrQuotaExceeded = errors.New("store: quota exceeded")
	// ErrNoMailbox is returned when a principal exists but may not hold mail.
	ErrNoMailbox = errors.New("store: principal may not hold mail")
)

// Options are the knobs for opening the database.
type Options struct {
	URL            string
	MaxOpen        int
	MaxIdle        int
	MaxLifetime    time.Duration
	ConnectTimeout time.Duration
	// ApplicationName is reported to PostgreSQL in pg_stat_activity, which
	// makes it possible to attribute a busy database to this server.
	ApplicationName string
}

// Open connects to PostgreSQL and verifies the connection.
//
// The verification is deliberate: returning a *sqlx.DB that has not yet
// reached the server would defer a misconfigured DSN to the first request,
// which for a mail server means the first inbound connection decides whether
// the deployment works.
func Open(ctx context.Context, opts Options) (*sqlx.DB, error) {
	if opts.URL == "" {
		return nil, errors.New("store: database URL is empty")
	}

	if opts.ApplicationName != "" {
		opts.URL = injectApplicationName(opts.URL, opts.ApplicationName)
	}

	db, err := sqlx.Open("pgx", opts.URL)
	if err != nil {
		return nil, fmt.Errorf("store: opening database: %w", err)
	}

	db.SetMaxOpenConns(opts.MaxOpen)
	db.SetMaxIdleConns(opts.MaxIdle)
	db.SetConnMaxLifetime(opts.MaxLifetime)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx := ctx
	if opts.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		pingCtx, cancel = context.WithTimeout(ctx, opts.ConnectTimeout)
		defer cancel()
	}

	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connecting to database: %w", err)
	}

	return db, nil
}

// OpenFromConfig connects using the database section of the configuration.
func OpenFromConfig(ctx context.Context, cfg *config.Config, appName string) (*sqlx.DB, error) {
	return Open(ctx, Options{
		URL:             cfg.Database.URL,
		MaxOpen:         cfg.Database.MaxOpen,
		MaxIdle:         cfg.Database.MaxIdle,
		MaxLifetime:     cfg.Database.MaxLifetime,
		ConnectTimeout:  cfg.Database.ConnectTimeout,
		ApplicationName: appName,
	})
}

// injectApplicationName adds or replaces the application_name parameter so
// that connections are identifiable in pg_stat_activity.
func injectApplicationName(rawURL, name string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		// Not a URL we can parse; let the driver report the real problem
		// rather than guessing here.
		return rawURL
	}
	q := u.Query()
	q.Set("application_name", name)
	u.RawQuery = q.Encode()
	return u.String()
}

// Health runs a trivial round trip to confirm the pool is usable.
func Health(ctx context.Context, db *sqlx.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

// InTx runs fn inside a transaction, committing on success and rolling back on
// error or panic.
//
// A panic is re-raised after the rollback so that a genuine bug is not
// silently converted into a clean rollback; swallowing it would let a partially
// applied operation look like a success.
func InTx(ctx context.Context, db *sqlx.DB, fn func(*sqlx.Tx) error) error {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			// Best effort: a failed rollback on a broken connection is not
			// actionable, and the original error is more useful.
			_ = tx.Rollback()
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing transaction: %w", err)
	}
	committed = true
	return nil
}

// InQuerierTx runs fn inside a transaction, or directly when q already is one.
//
// Every repository takes a Querier so that a caller can compose several into one
// transaction, which creates a problem for a repository that needs atomicity of
// its own: it cannot open a nested transaction, and it must not insist the
// caller opened one, because "these writes are atomic" is a property of the
// repository rather than of whoever happened to call it.
//
// So a repository that needs a transaction asks for one here. Given a *sqlx.DB
// it opens one; given a *sqlx.Tx it simply runs, because the caller's
// transaction already covers it and PostgreSQL has no nested transaction to
// nest into. Guessing wrong and opening a second connection would let the
// repository's writes commit outside the caller's transaction, which is a
// correctness bug that only appears under concurrency.
func InQuerierTx(ctx context.Context, q Querier, fn func(*sqlx.Tx) error) error {
	if tx, ok := q.(*sqlx.Tx); ok {
		return fn(tx)
	}
	db, ok := q.(*sqlx.DB)
	if !ok {
		return fmt.Errorf("store: %T is neither a *sqlx.DB nor a *sqlx.Tx", q)
	}
	return InTx(ctx, db, fn)
}

// IsNotFound reports whether err represents a missing row, in the forms sqlx
// and the pgx stdlib driver produce.
func IsNotFound(err error) bool {
	if errors.Is(err, ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return true
	}
	return false
}

// IsConflict reports whether err is a unique or foreign key violation.
func IsConflict(err error) bool {
	if errors.Is(err, ErrConflict) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", // unique_violation
			"23503", // foreign_key_violation
			"23514": // check_violation
			return true
		}
	}
	return false
}

// IsConnectionError reports whether err looks like a connectivity problem
// rather than a query problem. Callers use it to decide whether an operation
// should be retried.
func IsConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Class 08 is connection exception, 53 is insufficient resources,
		// 57P01 and 57P02 are admin shutdown and crash shutdown, 57P03 is
		// cannot connect now.
		switch {
		case pgErr.Code == "53300", pgErr.Code == "53400":
			return true
		case len(pgErr.Code) >= 2 && pgErr.Code[:2] == "08":
			return true
		case pgErr.Code == "57P01", pgErr.Code == "57P02", pgErr.Code == "57P03":
			return true
		}
	}
	return false
}

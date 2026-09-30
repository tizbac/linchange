// Package app wires the subsystems together and owns the process lifecycle.
//
// The wiring lives here rather than in main so that a future CLI subcommand,
// and the integration tests, can start the same application the server runs.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/store"
)

// App holds the long-lived resources shared by every listener.
type App struct {
	Config    *config.Config
	Logger    *slog.Logger
	DB        *sqlx.DB
	Blobs     *blobstore.Store
	StartedAt time.Time

	shutdownOnce sync.Once
	closers      []func(context.Context) error
}

// Options are the inputs to New.
type Options struct {
	Config *config.Config
	Logger *slog.Logger
}

// New opens the database and the blob store, applies migrations, and prepares
// the process-wide resources.
//
// It returns a fully wired App or nothing: a partially initialised App is never
// returned, so a caller cannot forget to close what was already opened.
func New(ctx context.Context, opts Options) (*App, error) {
	cfg := opts.Config
	log := opts.Logger
	if cfg == nil {
		return nil, errors.New("app: config is required")
	}
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}

	a := &App{
		Config:    cfg,
		Logger:    log,
		StartedAt: time.Now(),
	}

	db, err := store.OpenFromConfig(ctx, cfg, "linchange")
	if err != nil {
		return nil, err
	}
	a.DB = db
	a.closers = append(a.closers, func(context.Context) error { return db.Close() })

	if cfg.Database.AutoMigrate {
		if err := store.Migrate(ctx, db, log); err != nil {
			a.Close(context.Background())
			return nil, err
		}
	} else {
		// Without auto-migrate the schema must already be current, or the
		// server would start and then fail every query.
		if err := store.VerifySchema(ctx, db); err != nil {
			a.Close(context.Background())
			return nil, err
		}
	}

	blobs, err := blobstore.Open(blobstore.Options{
		Root: cfg.Dir("blobs"),
	})
	if err != nil {
		a.Close(context.Background())
		return nil, err
	}
	a.Blobs = blobs

	if err := ensureDirs(cfg); err != nil {
		a.Close(context.Background())
		return nil, err
	}

	log.Info("initialised",
		"hostname", cfg.Server.Hostname,
		"public_ip", cfg.Server.PublicIP,
		"data_dir", cfg.Server.DataDir,
		"postgres", db.PingContext(ctx) == nil,
	)

	return a, nil
}

// ensureDirs creates the data directories the server writes into.
//
// This is done at startup rather than lazily on first use so that a
// permissions problem surfaces immediately, not on the first inbound message.
func ensureDirs(cfg *config.Config) error {
	dirs := []string{
		cfg.Dir("blobs"),
		cfg.Dir("dkim"),
		cfg.Dir("spool"),
		cfg.Dir("tmp"),
	}
	if cfg.TLS.ACME.Enabled {
		dirs = append(dirs, cfg.TLS.ACME.CacheDir)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("app: creating %s: %w", d, err)
		}
	}
	return nil
}

// OnShutdown registers a cleanup function to run when the app closes.
// Functions run in reverse registration order.
func (a *App) OnShutdown(fn func(context.Context) error) {
	a.closers = append(a.closers, fn)
}

// Close releases every resource the app holds. It is safe to call more than
// once, which matters because a failed startup and a normal shutdown both end
// up here.
func (a *App) Close(ctx context.Context) {
	a.shutdownOnce.Do(func() {
		for i := len(a.closers) - 1; i >= 0; i-- {
			if err := a.closers[i](ctx); err != nil {
				a.Logger.Error("closing resource", "error", err)
			}
		}
	})
}

// Uptime reports how long the app has been running.
func (a *App) Uptime() time.Duration { return time.Since(a.StartedAt) }

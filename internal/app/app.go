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
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/delivery"
	"github.com/linchange/linchange/internal/dkim"
	"github.com/linchange/linchange/internal/httpapi"
	imapd "github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/inbound"
	"github.com/linchange/linchange/internal/pop3d"
	"github.com/linchange/linchange/internal/smtpd"
	"github.com/linchange/linchange/internal/store"
)

// App holds the long-lived resources shared by every listener.
type App struct {
	Config     *config.Config
	Logger     *slog.Logger
	DB         *sqlx.DB
	Blobs      *blobstore.Store
	HTTPServer *httpapi.Server
	HTTPListen *http.Server

	deliveryPool *delivery.Pool
	inboundSMTPS *inbound.Server
	smtpServer   *smtpd.Server
	imapServer   *imapd.Server
	pop3Server   *pop3d.Server
	dkimKeys     *dkim.Keys
	StartedAt    time.Time

	shutdownOnce sync.Once
	closers      []func(context.Context) error
}

// Options are the inputs to New.
type Options struct {
	Config *config.Config
	Logger *slog.Logger
	// StaticDir is the directory holding the built web app, served alongside the
	// API. Empty means API-only, for API clients and the protocol listeners.
	StaticDir string
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

	if cfg.Mail.Outbound.Concurrency > 0 {
		pool := delivery.FromConfig(cfg, log, db)
		poolCfg := pool.GetConfig()
		poolCfg.BlobStore = blobs
		pool.ApplyConfig(poolCfg)
		a.deliveryPool = pool

		// Register shutdown so the pool stops processing. The pool's Stop
		// waits for in-flight deliveries to finish, so it must be called
		// from the shutdown path (OnShutdown) rather than from Close: the
		// DB connection pool is released before the pool shuts down.
		a.OnShutdown(func(ctx context.Context) error {
			pool.Stop()
			log.Info("delivery pool stopped")
			return nil
		})
	}

	// Wire the inbound MTA on :25. It takes mail from the internet, so it runs
	// before submission: everything it accepts lands in a local mailbox.
	{
		backend := inbound.NewBackend(db, *cfg, log, blobs)
		inboundSrv, err := inbound.New(*cfg, backend, log)
		if err != nil {
			a.Close(context.Background())
			return nil, fmt.Errorf("inbound: %w", err)
		}
		a.inboundSMTPS = inboundSrv

		if inboundSrv.Enabled() {
			a.OnShutdown(func(ctx context.Context) error {
				log.Info("smtp inbound listener stopped")
				return nil
			})
		}
	}

	// Wire the SMTP submission backend and server.
	{
		backend := smtpd.NewBackend(
			db,
			cfg.Mail.Submission,
			store.NewUsers(),
			store.NewAliases(),
			store.NewCredentials(),
			a.deliveryPool.GetConfig().Outbox,
			blobs,
			log,
			store.NewSendCounters(),
		)
		smtpSrv, err := smtpd.New(*cfg, backend, log)
		if err != nil {
			a.Close(context.Background())
			return nil, fmt.Errorf("smtpd: %w", err)
		}
		a.smtpServer = smtpSrv

		if smtpSrv.Enabled() {
			a.OnShutdown(func(ctx context.Context) error {
				log.Info("smtp submission listeners stopped")
				return nil
			})
		}
	}

	// Wire the IMAP server.
	{
		imapSrv, err := imapd.New(
			db,
			blobs,
			log,
			store.NewUsers(),
			store.NewAliases(),
			store.NewCredentials(),
			store.NewSharedMailboxes(),
			store.NewSharedMailboxGrants(),
			cfg,
		)
		if err != nil {
			a.Close(context.Background())
			return nil, fmt.Errorf("imapd: %w", err)
		}
		a.imapServer = imapSrv

		if imapSrv.Enabled() {
			a.OnShutdown(func(ctx context.Context) error {
				log.Info("imap listeners stopped")
				return nil
			})
		}
	}

	// Wire the POP3 server.
	{
		pop3Srv, err := pop3d.New(
			db,
			blobs,
			log,
			store.NewUsers(),
			store.NewAliases(),
			store.NewCredentials(),
			store.NewSharedMailboxes(),
			store.NewSharedMailboxGrants(),
			cfg,
		)
		if err != nil {
			a.Close(context.Background())
			return nil, fmt.Errorf("pop3d: %w", err)
		}
		a.pop3Server = pop3Srv

		if pop3Srv.Enabled() {
			a.OnShutdown(func(ctx context.Context) error {
				log.Info("pop3 listeners stopped")
				return nil
			})
		}
	}

	// Initialise DKIM key manager.
	if cfg.Mail.DKIM.SelectorPrefix != "" {
		a.dkimKeys = dkim.New(cfg.Dir("dkim"))
		poolCfg := a.deliveryPool.GetConfig()
		poolCfg.DKIMKeys = a.dkimKeys
		poolCfg.DKIMSelectorPrefix = cfg.Mail.DKIM.SelectorPrefix
		a.deliveryPool.ApplyConfig(poolCfg)
	}

	if err := ensureDirs(cfg); err != nil {
		a.Close(context.Background())
		return nil, err
	}

	// The HTTP listener serves the REST API and, when a web build is present,
	// the Angular app from the same origin. An empty address means this process
	// is not the HTTP front end, in which case nothing is bound.
	if addr := cfg.Listener.HTTP.Addr; addr != "" {
		api, err := httpapi.New(httpapi.Options{
			DB: db, Config: cfg, Logger: log, StaticDir: opts.StaticDir,
		})
		if err != nil {
			a.Close(context.Background())
			return nil, err
		}
		a.HTTPServer = api
		a.HTTPListen = &http.Server{
			Addr:              addr,
			Handler:           api.Handler(),
			ReadHeaderTimeout: 15 * time.Second,
		}
		a.closers = append(a.closers, a.HTTPListen.Shutdown)
	}

	log.Info("initialised",
		"hostname", cfg.Server.Hostname,
		"public_ip", cfg.Server.PublicIP,
		"data_dir", cfg.Server.DataDir,
		"postgres", db.PingContext(ctx) == nil,
		"http", cfg.Listener.HTTP.Addr,
		"smtp_submission", cfg.Listener.SMTPSubmission.Addr,
		"smtp_submission_tls", cfg.Listener.SMTPSubmissionTLS.Addr,
		"imap", cfg.Listener.IMAP.Addr,
		"imaps", cfg.Listener.IMAPS.Addr,
		"pop3", cfg.Listener.POP3.Addr,
		"pop3s", cfg.Listener.POP3S.Addr,
	)

	return a, nil
}

// Serve runs the HTTP listener until ctx is cancelled. A clean shutdown returns
// nil; a bind or accept error is returned to the caller.
func (a *App) Serve(ctx context.Context) error {
	if a.deliveryPool != nil {
		a.deliveryPool.Start(ctx)
		go a.runDSNProcessor(ctx)
	}

	// Start the inbound MTA listener.
	var inboundErrCh chan error
	if a.inboundSMTPS != nil && a.inboundSMTPS.Enabled() {
		inboundErrCh = make(chan error, 1)
		go func() {
			if err := a.inboundSMTPS.Start(ctx); err != nil {
				inboundErrCh <- fmt.Errorf("smtp inbound: %w", err)
			}
		}()
	}

	// Start SMTP submission listeners.
	var smtpErrCh chan error
	if a.smtpServer != nil {
		smtpErrCh = make(chan error, 2)
		go func() {
			if err := a.smtpServer.StartHTTP(ctx); err != nil {
				smtpErrCh <- fmt.Errorf("smtp submission: %w", err)
			}
		}()
		go func() {
			if err := a.smtpServer.StartTLS(ctx); err != nil {
				smtpErrCh <- fmt.Errorf("smtp submission-tls: %w", err)
			}
		}()
	}

	// Start IMAP listeners.
	var imapErrCh chan error
	if a.imapServer != nil && a.imapServer.Enabled() {
		imapErrCh = make(chan error, 2)
		go func() {
			if err := a.imapServer.StartPlain(ctx); err != nil {
				imapErrCh <- fmt.Errorf("imap: %w", err)
			}
		}()
		go func() {
			if err := a.imapServer.StartTLS(ctx); err != nil {
				imapErrCh <- fmt.Errorf("imaps: %w", err)
			}
		}()
	}

	// Start POP3 listeners.
	var pop3ErrCh chan error
	if a.pop3Server != nil && a.pop3Server.Enabled() {
		pop3ErrCh = make(chan error, 2)
		go func() {
			if err := a.pop3Server.StartPlain(ctx); err != nil {
				pop3ErrCh <- fmt.Errorf("pop3: %w", err)
			}
		}()
		go func() {
			if err := a.pop3Server.StartTLS(ctx); err != nil {
				pop3ErrCh <- fmt.Errorf("pop3s: %w", err)
			}
		}()
	}

	if a.HTTPListen == nil {
		if inboundErrCh != nil {
			return <-inboundErrCh
		}
		if smtpErrCh != nil {
			return <-smtpErrCh
		}
		if imapErrCh != nil {
			return <-imapErrCh
		}
		if pop3ErrCh != nil {
			return <-pop3ErrCh
		}
		<-ctx.Done()
		return nil
	}

	httpErrCh := make(chan error, 1)
	go func() { httpErrCh <- a.HTTPListen.ListenAndServe() }()

	if smtpErrCh == nil && imapErrCh == nil && pop3ErrCh == nil && inboundErrCh == nil {
		// Only HTTP.
		select {
		case <-ctx.Done():
			return nil
		case err := <-httpErrCh:
			return err
		}
	}

	// HTTP with inbound, SMTP, IMAP, and/or POP3.
	select {
	case <-ctx.Done():
		return nil
	case err := <-httpErrCh:
		return err
	case err := <-inboundErrCh:
		return err
	case err := <-smtpErrCh:
		return err
	case err := <-imapErrCh:
		return err
	case err := <-pop3ErrCh:
		return err
	}
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

// runDSNProcessor runs the DSN processor in the background. It enqueues bounce
// messages for failed outbox entries whose retry lifetime has expired.
func (a *App) runDSNProcessor(ctx context.Context) {
	if a.deliveryPool == nil {
		return
	}
	dsn := delivery.NewDSNProcessor(a.deliveryPool)
	err := dsn.Process(ctx, a.Blobs, func(submittedBy, envelopeFrom string, blob []byte, recipients []delivery.OutboxRecipient) (int64, error) {
		digest, size, err := a.Blobs.Put(blob)
		if err != nil {
			return 0, fmt.Errorf("dsn blob put: %w", err)
		}
		return a.deliveryPool.GetConfig().Outbox.InsertMessage(ctx, nil, submittedBy, envelopeFrom, digest[:], size, recipients)
	})
	if err != nil {
		a.Logger.Error("dsn processor", "error", err)
	}
}

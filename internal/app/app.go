// Package app wires the subsystems together and owns the process lifecycle.
//
// The wiring lives here rather than in main so that a future CLI subcommand,
// and the integration tests, can start the same application the server runs.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/acme"
	ldapauth "github.com/linchange/linchange/internal/auth/ldap"
	"github.com/linchange/linchange/internal/auth/mailauth"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/delivery"
	"github.com/linchange/linchange/internal/dkim"
	"github.com/linchange/linchange/internal/httpapi"
	imapd "github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/imapimport"
	"github.com/linchange/linchange/internal/inbound"
	"github.com/linchange/linchange/internal/ldapsync"
	"github.com/linchange/linchange/internal/pop3d"
	"github.com/linchange/linchange/internal/smtpd"
	"github.com/linchange/linchange/internal/store"
)

// App holds the long-lived resources shared by every listener.
type App struct {
	Config         *config.Config
	Logger         *slog.Logger
	DB             *sqlx.DB
	Blobs          *blobstore.Store
	HTTPServer     *httpapi.Server
	HTTPListen     *http.Server
	HTTPSListen    *http.Server
	ACMEManager    *acme.Manager
	ACMEHTTPListen *http.Server

	deliveryPool *delivery.Pool
	inboundSMTPS *inbound.Server
	smtpServer   *smtpd.Server
	imapServer   *imapd.Server
	pop3Server   *pop3d.Server
	dkimKeys     *dkim.Keys
	// syncer reconciles the directory-backed principals. It is always built and
	// is inert unless the configuration enables it.
	syncer *ldapsync.Syncer
	// imports runs remote-IMAP import jobs claimed from the database.
	imports   *imapimport.Service
	StartedAt time.Time

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

	// Initialize ACME manager if enabled (but defer Start until HTTP server is running).
	if cfg.TLS.ACME.Enabled {
		a.ACMEManager = acme.NewManager(cfg.TLS.ACME.Email, cfg.TLS.ACME.Domains, cfg.TLS.ACME.CacheDir)
		a.OnShutdown(func(context.Context) error {
			a.Logger.Info("acme manager stopped")
			return nil
		})
	}

	// The directory reconcile keeps the directory-backed principals in step
	// with the directory. It runs on its own clock and is inert when the
	// configuration does not enable it. Its Stop is registered here, after the
	// database closer, because closers run in reverse: the reconcile must be
	// finished writing before the connection it writes through is released.
	a.syncer = ldapsync.New(cfg.LDAP, db, log, nil)
	a.OnShutdown(func(context.Context) error {
		a.syncer.Stop()
		log.Info("directory reconcile stopped")
		return nil
	})

	// The remote-IMAP import runner is stopped before the database closer
	// for the same reason as the reconcile: closers run in reverse order, so
	// registering it here means its Stop runs first and any in-flight job
	// finishes writing before the connection is released.
	imports := imapimport.New(db, blobs, log, cfg.Auth.EncryptionKey)
	a.imports = imports
	a.OnShutdown(func(context.Context) error {
		imports.Stop()
		log.Info("imap import runner stopped")
		return nil
	})

	// One directory client answers for every listener, so a provider is dialed
	// and a DN cache is kept once rather than per surface. It is also given to
	// the web login path by httpapi below, which is what keeps a browser and a
	// mail client agreeing about who an account is.
	directory := ldapauth.New(cfg.LDAP, store.NewLDAPLinks(), db, nil)
	// One verifier answers for all three mail protocols. Sharing it is what
	// stops them from drifting apart on who may log in and how the password is
	// checked.
	mailVerifier := mailauth.New(db, log, directory)

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
		inboundSrv, err := inbound.New(*cfg, backend, log, a.ACMEManager)
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
			store.NewAliases(),
			mailVerifier,
			a.deliveryPool.GetConfig().Outbox,
			blobs,
			log,
			store.NewSendCounters(),
		)
		smtpSrv, err := smtpd.New(*cfg, backend, log, a.ACMEManager)
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
			store.NewAliases(),
			mailVerifier,
			store.NewSharedMailboxes(),
			store.NewSharedMailboxGrants(),
			cfg,
			a.ACMEManager,
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
			store.NewAliases(),
			mailVerifier,
			store.NewSharedMailboxes(),
			store.NewSharedMailboxGrants(),
			cfg,
			a.ACMEManager,
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

	// The HTTP and HTTPS listeners both serve the REST API and, when a web
	// build is present, the Angular app. They are two independent servers:
	// HTTPListen is always plain HTTP, HTTPSListen always speaks TLS. An
	// empty address for either means that listener is not started.
	var apiHandler http.Handler
	if cfg.Listener.HTTP.Addr != "" || cfg.Listener.HTTPS.Addr != "" {
		api, err := httpapi.New(httpapi.Options{
			DB: db, Config: cfg, Logger: log, Blobs: blobs, StaticDir: opts.StaticDir,
			// The same directory instance the mail listeners use, so one
			// provider is dialed and one DN cache is kept for the process.
			Directory: directory,
			// The same verifier, so a CalDAV client and a mail client cannot
			// come to disagree about who an account is or what its password is.
			MailAuth: mailVerifier,
			// The import runner, so the HTTP API can enqueue and cancel jobs.
			Imports: imports,
		})
		if err != nil {
			a.Close(context.Background())
			return nil, err
		}
		a.HTTPServer = api
		apiHandler = api.Handler()
	}

	// ACME HTTP-01 challenge handler on port 80 (required for Let's Encrypt
	// HTTP challenge). Only start a separate ACME HTTP server if the main
	// HTTP listener is not already on port 80; if it is, that server handles
	// challenges through its wrapped handler below.
	if a.ACMEManager != nil && !isPort80(cfg.Listener.HTTP.Addr) {
		a.ACMEHTTPListen = &http.Server{
			Addr:              ":80",
			Handler:           a.ACMEManager.HTTPHandler(nil),
			ReadHeaderTimeout: 15 * time.Second,
		}
		a.closers = append(a.closers, a.ACMEHTTPListen.Shutdown)
	}

	if addr := cfg.Listener.HTTP.Addr; addr != "" {
		handler := apiHandler
		if cfg.Listener.HTTPS.Addr != "" && cfg.TLS.RedirectHTTP {
			// ACME challenges must keep working over plain HTTP, so when
			// both are in play the challenge handler sits in front and the
			// redirect is its fallback; without ACME the redirect is the
			// whole handler chain.
			handler = httpsRedirectHandler()
			if a.ACMEManager != nil {
				handler = a.ACMEManager.HTTPHandler(handler)
			}
		} else if a.ACMEManager != nil && isPort80(addr) {
			handler = a.ACMEManager.HTTPHandler(handler)
		}
		a.HTTPListen = &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 15 * time.Second,
		}
		a.closers = append(a.closers, a.HTTPListen.Shutdown)
	}

	if addr := cfg.Listener.HTTPS.Addr; addr != "" {
		var tlsConfig *tls.Config
		if a.ACMEManager != nil {
			tlsConfig = a.ACMEManager.TLSConfig()
		} else if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
			if err != nil {
				a.Close(context.Background())
				return nil, fmt.Errorf("https: load tls certificate: %w", err)
			}
			tlsConfig = &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			}
		} else {
			a.Close(context.Background())
			return nil, errors.New("https: listeners.https is set but no TLS certificate is available")
		}
		a.HTTPSListen = &http.Server{
			Addr:              addr,
			Handler:           apiHandler,
			ReadHeaderTimeout: 15 * time.Second,
			TLSConfig:         tlsConfig,
		}
		a.closers = append(a.closers, a.HTTPSListen.Shutdown)
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
	// The reconcile runs its first pass in the background, so starting the
	// server is not delayed by a directory that is slow to answer.
	if a.syncer != nil {
		a.syncer.Start(ctx)
	}

	if a.imports != nil {
		a.imports.Start(ctx)
	}

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

	// Start the ACME HTTP-01 challenge server on port 80, if it is needed
	// (ACME enabled and the main HTTP listener is not itself on port 80).
	if a.ACMEHTTPListen != nil {
		go func() {
			if err := a.ACMEHTTPListen.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				a.Logger.Error("acme http server", "error", err)
			}
		}()
	}

	// Pre-fetch ACME certificates now that the HTTP-01 challenge handler is
	// reachable (either the dedicated :80 server above or the wrapped main
	// HTTP handler).
	if a.ACMEManager != nil {
		if err := a.ACMEManager.Start(ctx); err != nil {
			a.Logger.Error("acme pre-fetch failed", "error", err)
		}
	}

	var httpErrCh, httpsErrCh chan error
	if a.HTTPListen != nil {
		httpErrCh = make(chan error, 1)
		go func() {
			httpErrCh <- a.HTTPListen.ListenAndServe()
		}()
	}
	if a.HTTPSListen != nil {
		httpsErrCh = make(chan error, 1)
		go func() {
			httpsErrCh <- a.HTTPSListen.ListenAndServeTLS("", "")
		}()
	}

	hasOtherListeners := smtpErrCh != nil || imapErrCh != nil || pop3ErrCh != nil || inboundErrCh != nil
	if httpErrCh == nil && httpsErrCh == nil {
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

	if !hasOtherListeners {
		// Only HTTP and/or HTTPS.
		select {
		case <-ctx.Done():
			return nil
		case err := <-httpErrCh:
			return err
		case err := <-httpsErrCh:
			return err
		}
	}

	// HTTP(S) with inbound, SMTP, IMAP, and/or POP3.
	select {
	case <-ctx.Done():
		return nil
	case err := <-httpErrCh:
		return err
	case err := <-httpsErrCh:
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

// isPort80 reports whether addr binds TCP port 80, in any of the forms a
// deployment may write it (":80", "0.0.0.0:80", "[::]:80", ":80/").
func isPort80(addr string) bool {
	return strings.HasSuffix(addr, ":80") || strings.HasSuffix(addr, ":80/")
}

// httpsRedirectHandler answers every request with a permanent redirect to the
// same URL on HTTPS, keeping the original host and request path. It sits
// behind the ACME challenge handler (if any), which serves HTTP-01
// challenges and passes everything else here.
func httpsRedirectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
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

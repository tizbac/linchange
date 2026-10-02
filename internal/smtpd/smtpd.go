// Package smtpd provides the authenticated SMTP submission listener on :587
// and :465. It handles AUTH (PLAIN and LOGIN), enforces sender identity,
// checks daily send limits, stores the message blob, and queues it in the
// outbox for delivery.
package smtpd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	pwd "github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/delivery"
	"github.com/linchange/linchange/internal/logging"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
)

// Backend implements smtp.Backend for authenticated submission.
type Backend struct {
	db             *sqlx.DB
	cfg            config.Submission
	userStore      *store.Users
	aliasStore     *store.Aliases
	credStore      *store.Credentials
	outboxRepo     *delivery.OutboxRepository
	blobStore      *blobstore.Store
	logger         *slog.Logger
	dailySendLimit int
	sendRepo       *store.SendCounters
}

// NewBackend creates the SMTP backend from the application configuration.
func NewBackend(
	db *sqlx.DB,
	cfg config.Submission,
	users *store.Users,
	aliases *store.Aliases,
	credStore *store.Credentials,
	outboxRepo *delivery.OutboxRepository,
	blobStore *blobstore.Store,
	log *slog.Logger,
	sendRepo *store.SendCounters,
) *Backend {
	return &Backend{
		db:             db,
		cfg:            cfg,
		userStore:      users,
		aliasStore:     aliases,
		credStore:      credStore,
		outboxRepo:     outboxRepo,
		blobStore:      blobStore,
		logger:         log,
		dailySendLimit: cfg.DailySendLimit,
		sendRepo:       sendRepo,
	}
}

// session holds the state for one SMTP connection.
type session struct {
	conn          *smtp.Conn
	backend       *Backend
	authenticated bool
	from          string
	recipients    []string
}

// NewSession is called when a new connection is established.
func (b *Backend) NewSession(conn *smtp.Conn) (smtp.Session, error) {
	s := &session{
		conn:       conn,
		backend:    b,
		from:       "",
		recipients: []string{},
	}
	return s, nil
}

// recipientAddrsFromHeaders returns the To and Cc addresses of a message.
//
// A message whose headers cannot be parsed yields nothing rather than an
// error: the envelope recipients are already known, and refusing mail because a
// header is malformed would reject messages that any other MTA delivers.
func recipientAddrsFromHeaders(msg []byte) []string {
	parsed, err := mail.Parse(msg)
	if err != nil {
		return nil
	}

	seen := make(map[string]bool)
	var addrs []string
	for _, a := range parsed.Recipients() {
		if a.Address == "" || seen[a.Address] {
			continue
		}
		seen[a.Address] = true
		addrs = append(addrs, a.Address)
	}
	return addrs
}

// AuthMechanisms returns the list of supported authentication mechanisms.
func (s *session) AuthMechanisms() []string {
	return []string{sasl.Plain, sasl.Login}
}

// Auth handles authentication.
func (s *session) Auth(mech string) (sasl.Server, error) {
	// Check if connection is TLS.
	_, isTLS := s.conn.TLSConnectionState()
	if !s.backend.cfg.AllowInsecureAuth && !isTLS {
		return nil, smtp.ErrAuthUnsupported
	}

	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			return s.authenticate(username, password)
		}), nil
	case sasl.Login:
		return &loginServer{session: s}, nil
	default:
		return nil, smtp.ErrAuthUnsupported
	}
}

// authenticate validates credentials against the user store.
func (s *session) authenticate(username string, password string) error {
	usr, err := s.backend.userStore.Get(s.context(), s.backend.db, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			pwd.DummyVerify(password)
			return errors.New("invalid credentials")
		}
		return err
	}

	if !usr.Enabled {
		return errors.New("account disabled")
	}
	if !usr.MailboxEnabled {
		return errors.New("mail disabled")
	}

	// Check credentials.
	cred, err := s.backend.credStore.Get(s.context(), s.backend.db, username)
	if err != nil {
		// No stored credential (LDAP user or similar).
		if errors.Is(err, store.ErrNoCredential) || errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}

	// Compare password.
	if strings.HasPrefix(cred.Hash, "$argon2") {
		match, err := pwd.Verify(password, cred.Hash)
		if err != nil {
			return fmt.Errorf("verify password: %w", err)
		}
		if !match {
			return errors.New("invalid credentials")
		}
	}

	s.authenticated = true
	s.from = username
	return nil
}

// Mail begins a new mail transaction.
func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	if !s.authenticated && !s.backend.cfg.AllowInsecureAuth {
		return smtp.ErrAuthRequired
	}

	if s.backend.cfg.EnforceSenderIdentity {
		if from != s.from {
			// Check if from is an alias of the authenticated user.
			alias, err := s.backend.aliasStore.Get(s.context(), s.backend.db, from)
			if err != nil {
				return smtp.ErrAuthFailed
			}
			if alias.TargetAddress != nil && *alias.TargetAddress != s.from {
				return smtp.ErrAuthFailed
			}
		}
	}

	s.from = from
	return nil
}

// Rcpt accepts a recipient address.
func (s *session) Rcpt(to string, opts *smtp.RcptOptions) error {
	s.recipients = append(s.recipients, to)
	return nil
}

// Data receives the message body, stores the blob, and queues it in the outbox.
func (s *session) Data(r io.Reader) error {
	msg, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read message: %w", err)
	}

	// Store the blob.
	digest, size, err := s.backend.blobStore.Put(msg)
	if err != nil {
		return fmt.Errorf("blob store: %w", err)
	}

	// Check daily send limit.
	if s.backend.dailySendLimit > 0 {
		counter, err := s.backend.sendRepo.Today(s.context(), s.backend.db, s.from)
		if err != nil {
			return fmt.Errorf("send counter: %w", err)
		}
		if counter.Count >= s.backend.dailySendLimit {
			return &smtp.SMTPError{
				Code:         552,
				EnhancedCode: smtp.EnhancedCode{5, 2, 2},
				Message:      "Daily send limit exceeded",
			}
		}
	}

	// Extract recipient addresses from headers. The envelope is what actually
	// gets delivered to, so it stays the source of truth; the headers only add
	// per-recipient header kinds for the delivery report.
	headerAddrs := recipientAddrsFromHeaders(msg)
	if len(headerAddrs) == 0 {
		headerAddrs = append(headerAddrs, s.recipients...)
	}

	// Build recipient list with deduplication.
	seen := make(map[string]bool)
	var recipients []delivery.OutboxRecipient
	for i, addr := range headerAddrs {
		if seen[addr] {
			continue
		}
		seen[addr] = true
		recipients = append(recipients, delivery.OutboxRecipient{
			Position: i,
			Address:  addr,
			Kind:     delivery.To,
		})
	}

	if len(recipients) == 0 {
		// Fallback to envelope recipients if headers had no To/Cc.
		for i, addr := range s.recipients {
			if seen[addr] {
				continue
			}
			seen[addr] = true
			recipients = append(recipients, delivery.OutboxRecipient{
				Position: i,
				Address:  addr,
				Kind:     delivery.To,
			})
		}
	}

	if len(recipients) == 0 {
		return &smtp.SMTPError{
			Code:         550,
			EnhancedCode: smtp.EnhancedCode{5, 1, 1},
			Message:      "No valid recipients",
		}
	}

	// Insert into outbox.
	_, err = s.backend.outboxRepo.InsertMessage(s.context(), nil, s.from, s.from, digest[:], size, recipients)
	if err != nil {
		return fmt.Errorf("outbox insert: %w", err)
	}

	s.backend.logger.Info("message queued",
		"from", s.from,
		"recipients", len(recipients),
		"size", size)

	return nil
}

// Reset resets the session state.
func (s *session) Reset() {
	s.from = ""
	s.recipients = []string{}
}

// Logout is called when the connection is closed.
func (s *session) Logout() error {
	return nil
}

// context returns a context for database operations.
func (s *session) context() context.Context {
	return context.Background()
}

// loginServer implements sasl.Server for the LOGIN mechanism.
type loginServer struct {
	step     int
	username string
	session  *session
}

// Next handles LOGIN challenge-response.
func (l *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	switch l.step {
	case 0:
		// First response is username.
		l.username = string(response)
		l.step = 1
		return []byte("Password:"), false, nil
	case 1:
		// Second response is password.
		pw := string(response)
		if err := l.session.authenticate(l.username, pw); err != nil {
			return nil, true, err
		}
		return nil, true, nil
	default:
		return nil, true, errors.New("authentication failed")
	}
}

// Server wraps the SMTP server listeners.
type Server struct {
	httpServer *smtp.Server
	tlsServer  *smtp.Server
}

// New creates the SMTP submission listeners.
func New(cfg config.Config, backend *Backend, logger *slog.Logger) (*Server, error) {
	var httpServer, tlsServer *smtp.Server

	if cfg.Listener.SMTPSubmission.Enabled() {
		httpServer = smtp.NewServer(backend)
		httpServer.Addr = cfg.Listener.SMTPSubmission.Addr
		httpServer.LMTP = false
		httpServer.Domain = ""
		httpServer.MaxRecipients = 100
		httpServer.MaxMessageBytes = int64(cfg.Listener.SMTPInboundMaxBytes)
		httpServer.MaxLineLength = 998
		httpServer.AllowInsecureAuth = cfg.Mail.Submission.AllowInsecureAuth
		httpServer.Debug = nil
		httpServer.ErrorLog = logging.NewPrintf(logger)
		httpServer.ReadTimeout = 30 * time.Second
		httpServer.WriteTimeout = 30 * time.Second
		httpServer.EnableSMTPUTF8 = true
	}

	if cfg.Listener.SMTPSubmissionTLS.Enabled() {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
			if err == nil {
				tlsCfg.Certificates = []tls.Certificate{cert}
			} else {
				logger.Warn("failed to load TLS cert", "cert", cfg.TLS.CertFile, "err", err)
			}
		}
		tlsServer = smtp.NewServer(backend)
		tlsServer.Addr = cfg.Listener.SMTPSubmissionTLS.Addr
		tlsServer.LMTP = false
		tlsServer.Domain = ""
		tlsServer.MaxRecipients = 100
		tlsServer.MaxMessageBytes = int64(cfg.Listener.SMTPInboundMaxBytes)
		tlsServer.MaxLineLength = 998
		tlsServer.AllowInsecureAuth = false
		tlsServer.Debug = nil
		tlsServer.ErrorLog = logging.NewPrintf(logger)
		tlsServer.ReadTimeout = 30 * time.Second
		tlsServer.WriteTimeout = 30 * time.Second
		tlsServer.EnableSMTPUTF8 = true
		tlsServer.TLSConfig = tlsCfg
	}

	return &Server{
		httpServer: httpServer,
		tlsServer:  tlsServer,
	}, nil
}

// StartHTTP starts the plaintext submission listener.
func (sv *Server) StartHTTP(ctx context.Context) error {
	if sv.httpServer == nil {
		return nil
	}
	ln, err := net.Listen("tcp", sv.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("smtp submission: %w", err)
	}
	return sv.httpServer.Serve(ln)
}

// StartTLS starts the TLS submission listener.
func (sv *Server) StartTLS(ctx context.Context) error {
	if sv.tlsServer == nil {
		return nil
	}
	ln, err := net.Listen("tcp", sv.tlsServer.Addr)
	if err != nil {
		return fmt.Errorf("smtp submission-tls: %w", err)
	}
	return sv.tlsServer.Serve(ln)
}

// Enabled reports whether any SMTP submission listener is configured.
func (sv *Server) Enabled() bool {
	return sv.httpServer != nil || sv.tlsServer != nil
}

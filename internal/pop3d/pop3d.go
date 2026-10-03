// Package pop3d provides a POP3 server on :110 and :995 backed by PostgreSQL.
//
// Features:
//   - SASL PLAIN authentication
//   - STAT, LIST, RETR, TOP, DELE, RSET, NOOP, QUIT
//   - CAPA extension mechanism (RFC 2449)
//   - STLS (RFC 2595)
//
// The implementation follows RFC 1939 (POP3) and RFC 2449 (CAPA).
package pop3d

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/jmoiron/sqlx"
	"github.com/migadu/go-pop3/pop3"
	"github.com/migadu/go-pop3/pop3server"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/mailauth"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
)

// Server wraps the POP3 server listeners.
type Server struct {
	plainServer *pop3server.Server
	tlsServer   *pop3server.Server
	backend     *backend
	logger      *slog.Logger
	enabled     bool
	plainAddr   string
	tlsAddr     string
}

// Enabled reports whether any POP3 listener is configured.
func (s *Server) Enabled() bool { return s.enabled }

// PlainServer returns the underlying pop3server.Server for plain connections.
// Tests use this to bind to a random port.
func (s *Server) PlainServer() *pop3server.Server { return s.plainServer }

// PlainAddr returns the configured plain listener address.
func (s *Server) PlainAddr() string { return s.plainAddr }

// SetPlainAddr changes the plain listener address.
func (s *Server) SetPlainAddr(addr string) { s.plainAddr = addr }

type backend struct {
	db              *sqlx.DB
	blobStore       *blobstore.Store
	aliasStore      *store.Aliases
	auth            *mailauth.Verifier
	mailboxStore    *imapd.MailboxRepo
	messageStore    *imapd.MessageRepo
	quotaRepo       *store.Quotas
	sharedMailboxes *store.SharedMailboxes
	sharedGrants    *store.SharedMailboxGrants
	logger          *slog.Logger
}

type session struct {
	backend     *backend
	logger      *slog.Logger
	username    string
	password    string
	mailboxID   int64
	deletedUids map[uint32]struct{}
}

// New creates the POP3 server from application configuration.
// New creates the POP3 server.
//
// The listener addresses come from cfg, so that the ports the operator
// configured are the ports that are bound.
func New(db *sqlx.DB, blobs *blobstore.Store,
	log *slog.Logger, aliases *store.Aliases,
	auth *mailauth.Verifier,
	sharedMailboxes *store.SharedMailboxes, sharedGrants *store.SharedMailboxGrants,
	cfg *config.Config,
) (*Server, error) {

	var plainServer, tlsServer *pop3server.Server
	enabled := false

	b := &backend{
		db:              db,
		blobStore:       blobs,
		aliasStore:      aliases,
		auth:            auth,
		mailboxStore:    imapd.NewMailboxRepo(),
		messageStore:    imapd.NewMessageRepo(),
		quotaRepo:       store.NewQuotas(),
		sharedMailboxes: sharedMailboxes,
		sharedGrants:    sharedGrants,
		logger:          log,
	}

	plainAddr := cfg.Listener.POP3.Addr
	tlsAddr := cfg.Listener.POP3S.Addr

	if plainAddr != "" {
		plainServer = pop3server.New(pop3server.Options{
			NewSession: func(conn *pop3server.Conn) (pop3server.Session, error) {
				sess, err := b.newSession()
				if err != nil {
					return nil, err
				}
				return sess, nil
			},
			Greeting:     fmt.Sprintf("%s POP3 ready", "linchange"),
			InsecureAuth: true,
			Logger:       log,
		})
		enabled = true
	}

	if tlsAddr != "" {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
			if err == nil {
				tlsCfg.Certificates = []tls.Certificate{cert}
			} else {
				log.Warn("failed to load TLS cert", "cert", cfg.TLS.CertFile, "err", err)
			}
		}
		tlsServer = pop3server.New(pop3server.Options{
			NewSession: func(conn *pop3server.Conn) (pop3server.Session, error) {
				sess, err := b.newSession()
				if err != nil {
					return nil, err
				}
				return sess, nil
			},
			Greeting:     fmt.Sprintf("%s POP3 ready", "linchange"),
			TLSConfig:    tlsCfg,
			InsecureAuth: false,
			Logger:       log,
		})
		enabled = true
	}

	return &Server{
		plainServer: plainServer,
		tlsServer:   tlsServer,
		backend:     b,
		logger:      log,
		enabled:     enabled,
		plainAddr:   plainAddr,
		tlsAddr:     tlsAddr,
	}, nil
}

// StartPlain starts the plaintext POP3 listener.
func (s *Server) StartPlain(ctx context.Context) error {
	if s.plainServer == nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.plainAddr)
	if err != nil {
		return fmt.Errorf("pop3: %w", err)
	}
	s.logger.Info("pop3 listening", "addr", s.plainAddr)
	return s.plainServer.Serve(ln)
}

// StartTLS starts the TLS POP3 listener.
func (s *Server) StartTLS(ctx context.Context) error {
	if s.tlsServer == nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.tlsAddr)
	if err != nil {
		return fmt.Errorf("pop3s: %w", err)
	}
	s.logger.Info("pop3s listening", "addr", s.tlsAddr)
	return s.tlsServer.Serve(ln)
}

func (b *backend) newSession() (*session, error) {
	return &session{
		backend:     b,
		deletedUids: make(map[uint32]struct{}),
	}, nil
}

func (s *session) Close() error {
	return nil
}

func (s *session) Login(ctx context.Context, username, password string) error {
	principal, err := s.backend.auth.Authenticate(ctx, username, password, policy.ActionPOP3)
	if err != nil {
		// Every failure is reported the same way. A POP3 client is told only
		// that the authentication failed; the reason belongs in the server log,
		// and reporting it here would let a caller map refusals to account
		// state.
		if !errors.Is(err, mailauth.ErrRejected) && !errors.Is(err, mailauth.ErrUnavailable) {
			s.backend.logger.Error("pop3 login failed unexpectedly",
				"user", username, "error", err)
		}
		return &pop3server.Error{Code: "AUTH", Message: "authentication failed"}
	}

	// The canonical address, not what the client typed: the INBOX lookup below
	// and every later stat are keyed on it.
	s.username = principal.Address

	// Find the user's INBOX. This is what makes the session usable — a login
	// that succeeded but resolved no mailbox would report an empty inbox for
	// ever rather than saying anything was wrong.
	mailbox, err := s.backend.mailboxStore.GetByName(ctx, s.backend.db, s.username, "INBOX")
	if err != nil {
		return fmt.Errorf("pop3: find inbox: %w", err)
	}
	s.mailboxID = mailbox.ID

	s.backend.logger.Info("pop3 login", "user", s.username)
	return nil
}

func (s *session) Stat(ctx context.Context) (count int, size int64, err error) {
	var stats struct {
		Count int   `db:"count"`
		Size  int64 `db:"size"`
	}
	query := `
		SELECT COUNT(*)::int as count, COALESCE(SUM(size), 0)::bigint as size
		FROM messages
		WHERE mailbox_id = $1
		AND NOT '\Deleted' = ANY(flags)
	`
	if err := s.backend.db.GetContext(ctx, &stats, query, s.mailboxID); err != nil {
		return 0, 0, fmt.Errorf("stat: %w", err)
	}
	return stats.Count, stats.Size, nil
}

func (s *session) List(ctx context.Context, msgNum int) ([]pop3.MessageInfo, error) {
	if msgNum != 0 {
		// Single message list.
		var msg struct {
			UID  uint32 `db:"uid"`
			Size int64  `db:"size"`
		}
		query := `
			SELECT uid, size FROM messages
			WHERE mailbox_id = $1
		AND NOT '\Deleted' = ANY(flags)
			ORDER BY uid
			LIMIT 1 OFFSET $2
		`
		if err := s.backend.db.GetContext(ctx, &msg, query, s.mailboxID, msgNum-1); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, &pop3server.Error{Message: "no such message"}
			}
			return nil, fmt.Errorf("list msg %d: %w", msgNum, err)
		}
		return []pop3.MessageInfo{{
			Num:  msgNum,
			Size: msg.Size,
		}}, nil
	}

	// All messages list.
	query := `
		SELECT uid, size FROM messages
		WHERE mailbox_id = $1
		AND NOT '\Deleted' = ANY(flags)
		ORDER BY uid
	`
	var msgs []struct {
		UID  uint32 `db:"uid"`
		Size int64  `db:"size"`
	}
	if err := s.backend.db.SelectContext(ctx, &msgs, query, s.mailboxID); err != nil {
		return nil, fmt.Errorf("list all: %w", err)
	}

	result := make([]pop3.MessageInfo, len(msgs))
	for i, m := range msgs {
		result[i] = pop3.MessageInfo{
			Num:  i + 1,
			Size: m.Size,
		}
	}
	return result, nil
}

func (s *session) Uidl(ctx context.Context, msgNum int) ([]pop3.MessageUidl, error) {
	if msgNum != 0 {
		var msg struct {
			UID uint32 `db:"uid"`
		}
		query := `
			SELECT uid FROM messages
			WHERE mailbox_id = $1
		AND NOT '\Deleted' = ANY(flags)
			ORDER BY uid
			LIMIT 1 OFFSET $2
		`
		if err := s.backend.db.GetContext(ctx, &msg, query, s.mailboxID, msgNum-1); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, &pop3server.Error{Message: "no such message"}
			}
			return nil, fmt.Errorf("uidl msg %d: %w", msgNum, err)
		}
		return []pop3.MessageUidl{{
			Num:      msgNum,
			UniqueID: fmt.Sprintf("%d", msg.UID),
		}}, nil
	}

	query := `
		SELECT uid FROM messages
		WHERE mailbox_id = $1
		AND NOT '\Deleted' = ANY(flags)
		ORDER BY uid
	`
	var uids []uint32
	if err := s.backend.db.SelectContext(ctx, &uids, query, s.mailboxID); err != nil {
		return nil, fmt.Errorf("uidl all: %w", err)
	}

	result := make([]pop3.MessageUidl, len(uids))
	for i, uid := range uids {
		result[i] = pop3.MessageUidl{
			Num:      i + 1,
			UniqueID: fmt.Sprintf("%d", uid),
		}
	}
	return result, nil
}

func (s *session) Retr(ctx context.Context, msgNum int) (io.ReadCloser, error) {
	query := `
		SELECT blob_sha256 FROM messages
		WHERE mailbox_id = $1
		AND NOT '\Deleted' = ANY(flags)
		ORDER BY uid
		LIMIT 1 OFFSET $2
	`
	var msg struct {
		BlobSHA256 []byte `db:"blob_sha256"`
	}
	if err := s.backend.db.GetContext(ctx, &msg, query, s.mailboxID, msgNum-1); err != nil {
		if err == sql.ErrNoRows {
			return nil, &pop3server.Error{Message: "no such message"}
		}
		return nil, fmt.Errorf("retr msg %d: %w", msgNum, err)
	}

	var digest blobstore.Digest
	copy(digest[:], msg.BlobSHA256)
	data, err := s.backend.blobStore.Get(digest)
	if err != nil {
		return nil, fmt.Errorf("blob get: %w", err)
	}

	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *session) Top(ctx context.Context, msgNum, lines int) (io.ReadCloser, error) {
	query := `
		SELECT blob_sha256 FROM messages
		WHERE mailbox_id = $1
		AND NOT '\Deleted' = ANY(flags)
		ORDER BY uid
		LIMIT 1 OFFSET $2
	`
	var msg struct {
		BlobSHA256 []byte `db:"blob_sha256"`
	}
	if err := s.backend.db.GetContext(ctx, &msg, query, s.mailboxID, msgNum-1); err != nil {
		if err == sql.ErrNoRows {
			return nil, &pop3server.Error{Message: "no such message"}
		}
		return nil, fmt.Errorf("top msg %d: %w", msgNum, err)
	}

	var digest blobstore.Digest
	copy(digest[:], msg.BlobSHA256)
	data, err := s.backend.blobStore.Get(digest)
	if err != nil {
		return nil, fmt.Errorf("blob get: %w", err)
	}

	// Extract just the headers.
	reader := bytes.NewReader(data)
	scanner := bufioScanner{reader: reader}
	var header bytes.Buffer
	for {
		line, err := scanner.nextLine()
		if err != nil {
			break
		}
		if line == "" {
			break // end of headers
		}
		header.WriteString(line)
		header.WriteString("\r\n")
	}

	return io.NopCloser(bytes.NewReader(header.Bytes())), nil
}

type bufioScanner struct {
	reader *bytes.Reader
}

func (s *bufioScanner) nextLine() (string, error) {
	var line bytes.Buffer
	for {
		b, err := s.reader.ReadByte()
		if err != nil {
			if line.Len() > 0 {
				return line.String(), nil
			}
			return "", err
		}
		if b == '\n' {
			return line.String(), nil
		}
		if b != '\r' {
			line.WriteByte(b)
		}
	}
}

func (s *session) Dele(ctx context.Context, msgNum int) error {
	query := `
		SELECT uid FROM messages
		WHERE mailbox_id = $1
		AND NOT '\Deleted' = ANY(flags)
		ORDER BY uid
		LIMIT 1 OFFSET $2
	`
	var uid uint32
	if err := s.backend.db.GetContext(ctx, &uid, query, s.mailboxID, msgNum-1); err != nil {
		if err == sql.ErrNoRows {
			return &pop3server.Error{Message: "no such message"}
		}
		return fmt.Errorf("dele msg %d: %w", msgNum, err)
	}

	// Mark as deleted by adding the flag to the array.
	_, err := s.backend.db.ExecContext(ctx, `
		UPDATE messages SET flags = array_append(flags, '\Deleted')
		WHERE mailbox_id = $1 AND uid = $2 AND NOT '\Deleted' = ANY(flags)
	`, s.mailboxID, uid)
	if err != nil {
		return fmt.Errorf("mark deleted: %w", err)
	}

	s.deletedUids[uid] = struct{}{}
	return nil
}

func (s *session) Rset(ctx context.Context) error {
	// Remove deleted flag from all messages.
	_, err := s.backend.db.ExecContext(ctx, `
		UPDATE messages SET flags = array_remove(flags, '\Deleted')
		WHERE mailbox_id = $1 AND '\Deleted' = ANY(flags)
	`, s.mailboxID)
	if err != nil {
		return fmt.Errorf("rset: %w", err)
	}

	s.deletedUids = make(map[uint32]struct{})
	return nil
}

func (s *session) Noop(ctx context.Context) error {
	return nil
}

func (s *session) Quit(ctx context.Context) (int, error) {
	// Commit deletions by actually removing messages from the mailbox.
	if len(s.deletedUids) > 0 {
		uids := make([]int, 0, len(s.deletedUids))
		for uid := range s.deletedUids {
			uids = append(uids, int(uid))
		}
		_, err := s.backend.db.ExecContext(ctx, `
			DELETE FROM messages WHERE mailbox_id = $1 AND uid = ANY($2::int[])
		`, s.mailboxID, uids)
		if err != nil {
			return 0, fmt.Errorf("quit commit deletions: %w", err)
		}
	}

	return 0, nil
}

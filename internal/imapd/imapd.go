// Package imapd provides an IMAP server on :143 and :993 backed by PostgreSQL.
//
// Features:
//   - SASL authentication (PLAIN + LOGIN via a custom mechanism)
//   - Personal namespace (INBOX + user-created folders)
//   - Shared mailbox namespace (user#sharedmailbox)
//   - FETCH, SEARCH, STORE, UID FETCH, UID SEARCH, UID STORE
//   - APPEND with quota enforcement
//   - CREATE, DELETE, RENAME, LIST, LSUB, STATUS, SUBSCRIBE/UNSUBSCRIBE
//   - IDLE via pg_notify
//   - EXPUNGE
//
// The implementation follows RFC 3501 (IMAP4rev1) and RFC 2177 (IDLE).
package imapd

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/mailauth"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/acme"
	"github.com/linchange/linchange/internal/store"
)

var (
	ErrMailboxExists   = errors.New("mailbox already exists")
	ErrMailboxNotFound = errors.New("mailbox not found")
	ErrMailboxReadOnly = errors.New("mailbox is read only")
	ErrMailboxSelected = errors.New("no mailbox is selected")
)

// Server wraps the IMAP server listeners.
type Server struct {
	plainServer *imapserver.Server
	tlsServer   *imapserver.Server
	backend     *backend
	logger      *slog.Logger
	enabled     bool
	plainAddr   string
	tlsAddr     string
}

// Enabled reports whether any IMAP listener is configured.
func (s *Server) Enabled() bool { return s.enabled }

// PlainServer returns the underlying imapserver.Server for plain connections.
// Tests use this to bind to a random port.
func (s *Server) PlainServer() *imapserver.Server { return s.plainServer }

// PlainAddr returns the configured plain listener address.
func (s *Server) PlainAddr() string { return s.plainAddr }

// SetPlainAddr changes the plain listener address.
func (s *Server) SetPlainAddr(addr string) { s.plainAddr = addr }

type backend struct {
	db              *sqlx.DB
	blobStore       *blobstore.Store
	aliasStore      *store.Aliases
	auth            *mailauth.Verifier
	mailboxStore    *MailboxRepo
	messageStore    *MessageRepo
	quotaRepo       *store.Quotas
	sharedMailboxes *store.SharedMailboxes
	sharedGrants    *store.SharedMailboxGrants
	logger          *slog.Logger
}

// New creates the IMAP server.
//
// The listener addresses come from cfg, so that the ports the operator
// configured are the ports that are bound.
func New(db *sqlx.DB, blobs *blobstore.Store,
	log *slog.Logger, aliases *store.Aliases,
	auth *mailauth.Verifier,
	sharedMailboxes *store.SharedMailboxes, sharedGrants *store.SharedMailboxGrants,
	cfg *config.Config,
	acmeManager *acme.Manager,
) (*Server, error) {

	var plainServer, tlsServer *imapserver.Server
	enabled := false

	b := &backend{
		db:              db,
		blobStore:       blobs,
		aliasStore:      aliases,
		auth:            auth,
		mailboxStore:    NewMailboxRepo(),
		messageStore:    NewMessageRepo(),
		quotaRepo:       store.NewQuotas(),
		sharedMailboxes: sharedMailboxes,
		sharedGrants:    sharedGrants,
		logger:          log,
	}

	plainAddr := cfg.Listener.IMAP.Addr
	tlsAddr := cfg.Listener.IMAPS.Addr

	if plainAddr != "" {
		plainServer = imapserver.New(&imapserver.Options{
			NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				sess, err := b.NewSession()
				if err != nil {
					return nil, nil, err
				}
				return sess, nil, nil
			},
			Caps: imap.CapSet{
				imap.CapIMAP4rev1:    {},
				imap.CapIMAP4rev2:    {},
				imap.CapIdle:         {},
				imap.CapNamespace:    {},
				imap.CapUIDPlus:      {},
				imap.CapSASLIR:       {},
				imap.CapLiteralMinus: {},
				imap.CapUnselect:     {},
				imap.CapEnable:       {},
				imap.CapID:           {},
				imap.CapESearch:      {},
				imap.CapSearchRes:    {},
				imap.CapListExtended: {},
				imap.CapListStatus:   {},
				imap.CapMove:         {},
				imap.CapStatusSize:   {},
				imap.CapChildren:     {},
			},
			InsecureAuth: true,
			Logger:       imapLogger{log: log},
		})
		_ = plainAddr // Addr is set via the server's internal config
		enabled = true
	}

	if tlsAddr != "" {
		var tlsCfg *tls.Config
		if acmeManager != nil {
			tlsCfg = acmeManager.TLSConfig()
		} else {
			tlsCfg = &tls.Config{
				MinVersion: tls.VersionTLS12,
			}
			if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
				cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
				if err != nil {
					return nil, fmt.Errorf("imapd: failed to load TLS cert %s: %w", cfg.TLS.CertFile, err)
				}
				tlsCfg.Certificates = []tls.Certificate{cert}
				} else {
					return nil, fmt.Errorf("imapd: TLS listener configured but no cert_file/key_file provided")
				}
		}

		tlsServer = imapserver.New(&imapserver.Options{
			NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				sess, err := b.NewSession()
				if err != nil {
					return nil, nil, err
				}
				return sess, nil, nil
			},
			Caps: imap.CapSet{
				imap.CapIMAP4rev1:    {},
				imap.CapIMAP4rev2:    {},
				imap.CapIdle:         {},
				imap.CapNamespace:    {},
				imap.CapUIDPlus:      {},
				imap.CapSASLIR:       {},
				imap.CapLiteralMinus: {},
				imap.CapUnselect:     {},
				imap.CapEnable:       {},
				imap.CapID:           {},
				imap.CapESearch:      {},
				imap.CapSearchRes:    {},
				imap.CapListExtended: {},
				imap.CapListStatus:   {},
				imap.CapMove:         {},
				imap.CapStatusSize:   {},
				imap.CapChildren:     {},
			},
			TLSConfig: tlsCfg,
			Logger:    imapLogger{log: log},
		})
		_ = tlsAddr
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

// StartPlain starts the plaintext IMAP listener.
func (s *Server) StartPlain(ctx context.Context) error {
	if s.plainServer == nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.plainAddr)
	if err != nil {
		return fmt.Errorf("imap: %w", err)
	}
	s.logger.Info("imap listening", "addr", s.plainAddr)
	return s.plainServer.Serve(ln)
}

// StartTLS starts the TLS IMAP listener.
func (s *Server) StartTLS(ctx context.Context) error {
	if s.tlsServer == nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.tlsAddr)
	if err != nil {
		return fmt.Errorf("imaps: %w", err)
	}
	s.logger.Info("imaps listening", "addr", s.tlsAddr)
	return s.tlsServer.Serve(ln)
}

// imapLogger adapts slog.Logger to imapserver.Logger.
type imapLogger struct {
	log *slog.Logger
}

func (l imapLogger) Printf(format string, args ...interface{}) {
	l.log.Info(fmt.Sprintf("imap: "+format, args...))
}

// ---------------------------------------------------------------------------
// Session implementation
// ---------------------------------------------------------------------------

// session holds the state for one IMAP connection.
type session struct {
	backend       *backend
	user          string
	authenticated bool
	selected      *selectedMailbox
	readonly      bool
	idleDone      chan struct{}
}

type selectedMailbox struct {
	name         string
	mailboxID    int64
	uidValidity  uint32
	uidNext      uint32
	messageCount uint32
}

// Login handles SASL LOGIN authentication.
func (b *backend) NewSession() (imapserver.Session, error) {
	return &session{
		backend:  b,
		idleDone: make(chan struct{}),
	}, nil
}

// Login authenticates a user via username/password.
func (s *session) Login(username, password string) error {
	principal, err := s.backend.auth.Authenticate(
		context.Background(), username, password, policy.ActionIMAP)
	if err != nil {
		// Every failure is reported the same way. An IMAP client is told only
		// that the login failed; the reason belongs in the server log, and
		// telling it here would let a caller map refusals to account state.
		if !errors.Is(err, mailauth.ErrRejected) && !errors.Is(err, mailauth.ErrUnavailable) {
			s.backend.logger.Error("imap login failed unexpectedly",
				"user", username, "error", err)
		}
		return imapserver.ErrAuthFailed
	}

	// The canonical address, not whatever the client typed. Every mailbox and
	// quota lookup below is keyed on it, and so is a shared mailbox grant, so
	// this must be the stored spelling.
	s.user = principal.Address
	s.authenticated = true

	s.backend.logger.Info("imap login", "user", principal.Address)
	return nil
}

// Close cleans up session resources.
func (s *session) Close() error {
	close(s.idleDone)
	return nil
}

// AuthenticateMechanisms returns the supported SASL mechanisms.
func (s *session) AuthenticateMechanisms() []string {
	return []string{"PLAIN", "LOGIN"}
}

// ---------------------------------------------------------------------------
// Namespace
// ---------------------------------------------------------------------------

// Namespace returns the server namespace.
func (s *session) Namespace() (*imap.NamespaceData, error) {
	return &imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{
			{Prefix: "", Delim: '/'},
		},
	}, nil
}

// ---------------------------------------------------------------------------
// Mailbox operations
// ---------------------------------------------------------------------------

// List returns the mailbox list.
func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	ctx := context.Background()

	mailboxes, err := s.backend.mailboxStore.ListForUser(ctx, s.backend.db, s.user)
	if err != nil {
		return fmt.Errorf("imap: list mailboxes: %w", err)
	}

	s.backend.logger.Info("imap: list mailboxes", "count", len(mailboxes), "patterns", patterns)

	matched := make([]Mailbox, 0, len(mailboxes))
	for _, mb := range mailboxes {
		if options != nil && options.ReturnSubscribed && !mb.Subscribed {
			continue
		}
		if !s.matchPatterns(mb.Name, patterns) {
			continue
		}
		matched = append(matched, mb)
	}

	for _, mb := range matched {
		var attrs []imap.MailboxAttr
		if mb.SpecialUse != nil && *mb.SpecialUse != "" {
			attrs = append(attrs, imap.MailboxAttrMarked)
		}
		if mb.Subscribed {
			attrs = append(attrs, imap.MailboxAttrSubscribed)
		}

		data := &imap.ListData{
			Attrs:   attrs,
			Delim:   '/',
			Mailbox: mb.Name,
		}
		if err := w.WriteList(data); err != nil {
			return fmt.Errorf("imap: list write: %w", err)
		}
	}

	// Add shared mailboxes the user has access to.
	shared, err := s.backend.sharedGrants.ListForMember(ctx, s.backend.db, s.user)
	if err != nil {
		// Ignore errors on shared mailbox listing.
		return nil
	}

	for _, sg := range shared {
		sharedAddr := sg.SharedAddress
		// Parse the shared mailbox name.
		name := sharedAddr
		if idx := strings.IndexByte(sharedAddr, '#'); idx >= 0 {
			name = sharedAddr[:idx]
		}
		if !s.matchPatterns(name, patterns) {
			continue
		}
		data := &imap.ListData{
			Delim:   '/',
			Mailbox: name,
		}
		if err := w.WriteList(data); err != nil {
			return fmt.Errorf("imap: list shared: %w", err)
		}
	}

	return nil
}

func (s *session) matchPatterns(name string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if s.matchPattern(name, pattern) {
			return true
		}
	}
	return false
}

func (s *session) matchPattern(name, pattern string) bool {
	// Simple wildcard matching: * matches any sequence of non-delimiter chars
	// % matches any sequence of chars
	// For simplicity, convert % to * and normalize multiple asterisks
	pattern = strings.ReplaceAll(pattern, "%", "*")
	pattern = normalizeAsterisks(pattern)

	if pattern == "*" {
		return true
	}

	if strings.Contains(pattern, "*") {
		parts := strings.SplitN(pattern, "*", 2)
		if len(parts) == 2 {
			if strings.HasPrefix(pattern, "*") && strings.HasSuffix(pattern, "*") {
				return strings.Contains(name, parts[1])
			}
			if strings.HasPrefix(pattern, "*") {
				return strings.HasSuffix(name, parts[1])
			}
			if strings.HasSuffix(pattern, "*") {
				return strings.HasPrefix(name, parts[0])
			}
			return strings.HasPrefix(name, parts[0]) && strings.HasSuffix(name, parts[1])
		}
	}
	return name == pattern
}

func normalizeAsterisks(s string) string {
	var result strings.Builder
	prevStar := false
	for _, r := range s {
		if r == '*' {
			if !prevStar {
				result.WriteRune(r)
			}
			prevStar = true
		} else {
			result.WriteRune(r)
			prevStar = false
		}
	}
	return result.String()
}

// Create creates a new mailbox.
func (s *session) Create(mailbox string, options *imap.CreateOptions) error {
	ctx := context.Background()

	// Resolve the mailbox name.
	name := strings.Trim(mailbox, "/")
	if name == "" || name == "INBOX" {
		return ErrMailboxExists
	}

	err := s.backend.mailboxStore.Create(ctx, s.backend.db, s.user, name, nil)
	if err != nil {
		if err == store.ErrConflict {
			return ErrMailboxExists
		}
		return fmt.Errorf("imap: create mailbox: %w", err)
	}

	return nil
}

// Delete removes a mailbox.
func (s *session) Delete(mailbox string) error {
	ctx := context.Background()

	name := strings.Trim(mailbox, "/")
	if name == "" || name == "INBOX" {
		return ErrMailboxReadOnly
	}

	// Check if the user owns this mailbox.
	mb, err := s.backend.mailboxStore.GetByName(ctx, s.backend.db, s.user, name)
	if err != nil {
		return ErrMailboxNotFound
	}

	err = s.backend.mailboxStore.Delete(ctx, s.backend.db, mb.ID)
	if err != nil {
		return fmt.Errorf("imap: delete mailbox: %w", err)
	}

	return nil
}

// Rename renames a mailbox.
func (s *session) Rename(oldName, newName string, options *imap.RenameOptions) error {
	ctx := context.Background()

	oldName = strings.Trim(oldName, "/")
	newName = strings.Trim(newName, "/")

	mb, err := s.backend.mailboxStore.GetByName(ctx, s.backend.db, s.user, oldName)
	if err != nil {
		return ErrMailboxNotFound
	}

	err = s.backend.mailboxStore.Rename(ctx, s.backend.db, mb.ID, newName)
	if err != nil {
		return fmt.Errorf("imap: rename mailbox: %w", err)
	}

	return nil
}

// Select selects a mailbox for read/write access.
func (s *session) Select(mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	err := s.selectMailbox(mailbox, false)
	if err != nil {
		return nil, err
	}

	return &imap.SelectData{
		NumMessages: s.selected.messageCount,
		UIDNext:     imap.UID(s.selected.uidNext),
		UIDValidity: s.selected.uidValidity,
	}, nil
}

// UIDNext returns the next UID for the selected mailbox.
func (s *session) UIDNext() (uint32, error) {
	if s.selected == nil {
		return 0, ErrMailboxSelected
	}
	return s.selected.uidNext, nil
}

// Unselect deselects the current mailbox.
func (s *session) Unselect() error {
	s.selected = nil
	return nil
}

// Subscribe subscribes to a mailbox.
func (s *session) Subscribe(mailbox string) error {
	ctx := context.Background()

	name := strings.Trim(mailbox, "/")
	mb, err := s.backend.mailboxStore.GetByName(ctx, s.backend.db, s.user, name)
	if err != nil {
		return fmt.Errorf("imap: subscribe: %w", err)
	}

	err = s.backend.mailboxStore.SetSubscribed(ctx, s.backend.db, mb.ID, true)
	if err != nil {
		return fmt.Errorf("imap: set subscribed: %w", err)
	}

	return nil
}

// Unsubscribe unsubscribes from a mailbox.
func (s *session) Unsubscribe(mailbox string) error {
	ctx := context.Background()

	name := strings.Trim(mailbox, "/")
	mb, err := s.backend.mailboxStore.GetByName(ctx, s.backend.db, s.user, name)
	if err != nil {
		return fmt.Errorf("imap: unsubscribe: %w", err)
	}

	err = s.backend.mailboxStore.SetSubscribed(ctx, s.backend.db, mb.ID, false)
	if err != nil {
		return fmt.Errorf("imap: set unsubscribed: %w", err)
	}

	return nil
}

// Status returns mailbox status.
func (s *session) Status(mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	ctx := context.Background()

	name := strings.Trim(mailbox, "/")
	if name == "" || name == "INBOX" {
		name = "INBOX"
	}

	mb, err := s.backend.mailboxStore.GetByName(ctx, s.backend.db, s.user, name)
	if err != nil {
		return nil, ErrMailboxNotFound
	}

	msgCount, err := s.backend.messageStore.CountByMailbox(ctx, s.backend.db, mb.ID)
	if err != nil {
		return nil, fmt.Errorf("imap: status count: %w", err)
	}

	msgCountU32 := uint32(msgCount)
	return &imap.StatusData{
		Mailbox:     name,
		NumMessages: &msgCountU32,
	}, nil
}

// Append appends a message to a mailbox.
func (s *session) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	ctx := context.Background()

	name := strings.Trim(mailbox, "/")
	if name == "" {
		name = "INBOX"
	}

	// Check if it's a shared mailbox.
	isShared := strings.Contains(name, "#")
	if isShared {
		// Parse shared mailbox name: user#sharedmailbox
		parts := strings.SplitN(name, "#", 2)
		if len(parts) != 2 {
			return nil, ErrMailboxNotFound
		}
		mailboxName := parts[1]

		// Check if user has access to this shared mailbox.
		access, err := s.backend.sharedGrants.Access(ctx, s.backend.db, parts[0], s.user)
		if err != nil || !access.AtLeast(policy.GrantRead) {
			return nil, ErrMailboxNotFound
		}

		// Get the shared mailbox owner's mailboxes.
		mbs, err := s.backend.mailboxStore.ListForUser(ctx, s.backend.db, parts[0])
		if err != nil {
			return nil, ErrMailboxNotFound
		}

		var found bool
		for _, m := range mbs {
			if m.Name == mailboxName {
				found = true
				break
			}
		}
		if !found {
			return nil, ErrMailboxNotFound
		}
	}

	mb, err := s.backend.mailboxStore.GetByName(ctx, s.backend.db, s.user, name)
	if err != nil {
		return nil, ErrMailboxNotFound
	}

	_ = mb

	// Read the full message.
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("imap: read append data: %w", err)
	}

	// Check quota.
	quota, err := s.backend.quotaRepo.Get(ctx, s.backend.db, s.user)
	if err != nil {
		return nil, fmt.Errorf("imap: get quota: %w", err)
	}

	limit, err := getQuotaLimit(ctx, s.backend.db, s.user)
	if err != nil {
		return nil, fmt.Errorf("imap: get quota limit: %w", err)
	}

	if limit > 0 && quota.UsedBytes+int64(len(data)) > limit {
		return nil, errors.New("quota exceeded")
	}

	// Store the blob.
	digest, size, err := s.backend.blobStore.Put(data)
	if err != nil {
		return nil, fmt.Errorf("imap: store blob: %w", err)
	}

	// Describe the message once, for both the row and the address indexes.
	meta, err := mail.Describe(data)
	if err != nil {
		return nil, fmt.Errorf("imap: describe message: %w", err)
	}

	// Insert reserves the UID and advances uidnext itself, so there is no
	// second uidnext write here to race against.
	internalDate := meta.Date
	if options.Time.IsZero() {
		internalDate = time.Now().UTC()
	}

	// The client names the flags it wants, and an appended message is Recent
	// until this session has seen it.
	flags := []string{`\Recent`}
	for _, f := range options.Flags {
		flags = append(flags, string(f))
	}

	result, err := s.backend.messageStore.Insert(ctx, s.backend.db, mb.ID, digest[:], size, internalDate, flags, meta)
	if err != nil {
		return nil, fmt.Errorf("imap: insert message: %w", err)
	}

	// Adjust quota.
	_, err = s.backend.quotaRepo.Adjust(ctx, s.backend.db, s.user, size, 1)
	if err != nil {
		return nil, fmt.Errorf("imap: adjust quota: %w", err)
	}

	return &imap.AppendData{
		UID:         imap.UID(result.UID),
		UIDValidity: s.selected.uidValidity,
	}, nil
}

// Poll polls for mailbox changes.
func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	ctx := context.Background()

	db := s.backend.db

	// Simple polling: check for new/changed messages.
	if s.selected == nil {
		return nil
	}

	// Get current message count
	var currentCount int
	const countQuery = `SELECT count(*) FROM messages WHERE mailbox_id = $1`
	if err := db.GetContext(ctx, &currentCount, countQuery, s.selected.mailboxID); err != nil {
		return fmt.Errorf("imap: poll count: %w", err)
	}

	// Update message count and send EXISTS if there are new messages
	if currentCount > int(s.selected.messageCount) {
		s.selected.messageCount = uint32(currentCount)
		w.WriteNumMessages(s.selected.messageCount)
	}

	return nil
}

// Idle enters idle mode.
func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	ctx := context.Background()

	// Set up notification listening.
	listener := &pgListener{
		db:     s.backend.db,
		logger: s.backend.logger,
	}

	if err := listener.Listen(ctx, "message_change"); err != nil {
		return fmt.Errorf("imap: listen: %w", err)
	}
	defer listener.Unlisten(ctx)

	for {
		select {
		case <-stop:
			return nil
		case <-listener.Notify:
			if s.selected == nil {
				return nil
			}
			// Send untagged EXISTS response.
			count, err := s.backend.messageStore.CountByMailbox(ctx, s.backend.db, s.selected.mailboxID)
			if err == nil {
				_ = w.WriteNumMessages(uint32(count))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Message operations
// ---------------------------------------------------------------------------

// Search searches messages in the selected mailbox.
func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	ctx := context.Background()

	if s.selected == nil {
		return nil, ErrMailboxSelected
	}

	var ids []uint32
	var err error

	switch kind {
	case imapserver.NumKindSeq:
		ids, err = s.backend.messageStore.SearchSeq(ctx, s.backend.db, s.selected.mailboxID, criteria)
	default:
		ids, err = s.backend.messageStore.SearchUID(ctx, s.backend.db, s.selected.mailboxID, criteria)
	}

	if err != nil {
		return nil, fmt.Errorf("imap: search: %w", err)
	}

	return &imap.SearchData{
		All: idsToNumSet(kind, ids),
	}, nil
}

func idsToNumSet(kind imapserver.NumKind, ids []uint32) imap.NumSet {
	switch kind {
	case imapserver.NumKindSeq:
		ss := make(imap.SeqSet, len(ids))
		for i, id := range ids {
			ss[i] = imap.SeqRange{Start: id, Stop: id}
		}
		return ss
	default:
		us := make(imap.UIDSet, len(ids))
		for i, id := range ids {
			us[i] = imap.UIDRange{Start: imap.UID(id), Stop: imap.UID(id)}
		}
		return us
	}
}

// Fetch fetches messages.
func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	ctx := context.Background()

	if s.selected == nil {
		return ErrMailboxSelected
	}

	messages, err := s.backend.messageStore.GetByUIDSet(ctx, s.backend.db, s.selected.mailboxID, numSet)
	if err != nil {
		return fmt.Errorf("imap: fetch: %w", err)
	}

	for _, msg := range messages {
		// Get the full message blob.
		var digest blobstore.Digest
		copy(digest[:], msg.BlobSHA256)
		blob, err := s.backend.blobStore.Get(digest)
		if err != nil {
			s.backend.logger.Warn("imap: fetch blob", "message_id", msg.ID, "error", err)
			continue
		}

		// Build the envelope.
		envelope := buildEnvelope(msg)

		// Write the response using the UID as the sequence number.
		// When fetching by sequence set, the sequence numbers correspond to the UIDs
		// since we query by UID.
		seqNumU32 := uint32(msg.UID)
		wr := w.CreateMessage(seqNumU32)
		if envelope != nil {
			wr.WriteEnvelope(envelope)
		}
		wr.WriteFlags(convertFlags(msg.Flags))
		wr.WriteInternalDate(msg.InternalDate)
		wr.WriteUID(imap.UID(msg.UID))

		// Write the body section.
		if len(options.BodySection) > 0 {
			for _, bs := range options.BodySection {
				lr := wr.WriteBodySection(bs, int64(len(blob)))
				if _, err := lr.Write(blob); err != nil {
					s.backend.logger.Warn("imap: write body section", "error", err)
					return fmt.Errorf("imap: write body section: %w", err)
				}
				lr.Close()
			}
		}

		wr.Close()
	}

	return nil
}

// Store stores flags on messages.
func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	ctx := context.Background()

	if s.selected == nil {
		return ErrMailboxSelected
	}

	if flags == nil {
		return nil
	}

	flagList := make([]string, len(flags.Flags))
	for i, f := range flags.Flags {
		flagList[i] = string(f)
	}

	messages, err := s.backend.messageStore.GetByUIDSet(ctx, s.backend.db, s.selected.mailboxID, numSet)
	if err != nil {
		return fmt.Errorf("imap: store: %w", err)
	}

	for _, msg := range messages {
		var newFlags []string

		// Start with existing flags.
		newFlags = append(newFlags, msg.Flags...)

		switch flags.Op {
		case imap.StoreFlagsAdd:
			for _, f := range flagList {
				newFlags = addFlag(newFlags, f)
			}
		case imap.StoreFlagsDel:
			for _, f := range flagList {
				newFlags = removeFlag(newFlags, f)
			}
		default:
			// Replace all flags.
			newFlags = append([]string{}, flagList...)
		}

		err = s.backend.messageStore.UpdateFlags(ctx, s.backend.db, msg.ID, newFlags)
		if err != nil {
			return fmt.Errorf("imap: update flags: %w", err)
		}

		// Write the updated flags if not silent.
		if !flags.Silent {
			seqNumU32 := uint32(msg.UID)
			wr := w.CreateMessage(seqNumU32)
			wr.WriteFlags(convertFlags(newFlags))
			wr.WriteUID(imap.UID(msg.UID))
			wr.Close()
		}
	}

	return nil
}

// Expunge expunges messages.
func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	ctx := context.Background()

	if s.selected == nil {
		return ErrMailboxSelected
	}

	if uids != nil {
		// UID EXPUNGE - delete specific UIDs
		for _, uidRange := range *uids {
			uid := uidRange.Start
			err := s.backend.messageStore.DeleteByUID(ctx, s.backend.db, s.selected.mailboxID, uint32(uid))
			if err != nil {
				return fmt.Errorf("imap: expunge: %w", err)
			}
			s.selected.messageCount--
			s.backend.logger.Info("imap: expunge uid", "uid", uid, "seqNum", s.selected.messageCount+1)
			w.WriteExpunge(s.selected.messageCount + 1)
		}
	} else {
		// EXPUNGE without UIDs - expunge all messages with \Deleted flag
		messages, err := s.backend.messageStore.GetDeletedMessages(ctx, s.backend.db, s.selected.mailboxID)
		if err != nil {
			return fmt.Errorf("imap: expunge get messages: %w", err)
		}

		allMessages, _ := s.backend.messageStore.GetAllMessages(ctx, s.backend.db, s.selected.mailboxID)
		s.backend.logger.Info("imap: expunge", "deletedCount", len(messages), "mailboxID", s.selected.mailboxID, "totalCount", len(allMessages))
		for i := range allMessages {
			s.backend.logger.Info("imap: expunge all message", "uid", allMessages[i].UID, "flags", allMessages[i].Flags)
		}

		for _, msg := range messages {
			s.backend.logger.Info("imap: expunge msg", "uid", msg.UID, "seqNumBefore", s.selected.messageCount)
			err := s.backend.messageStore.DeleteByUID(ctx, s.backend.db, s.selected.mailboxID, msg.UID)
			if err != nil {
				return fmt.Errorf("imap: expunge delete: %w", err)
			}
			s.selected.messageCount--
			s.backend.logger.Info("imap: expunge msg done", "uid", msg.UID, "seqNumAfter", s.selected.messageCount+1)
			w.WriteExpunge(s.selected.messageCount + 1)
		}
	}

	return nil
}

// Copy copies messages to another mailbox.
func (s *session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	ctx := context.Background()

	if s.selected == nil {
		return nil, ErrMailboxSelected
	}

	destName := strings.Trim(dest, "/")
	destMB, err := s.backend.mailboxStore.GetByName(ctx, s.backend.db, s.user, destName)
	if err != nil {
		return nil, ErrMailboxNotFound
	}

	messages, err := s.backend.messageStore.GetByUIDSet(ctx, s.backend.db, s.selected.mailboxID, numSet)
	if err != nil {
		return nil, fmt.Errorf("imap: copy: %w", err)
	}

	var uids []uint32
	for _, msg := range messages {
		// Read the blob.
		var digest blobstore.Digest
		copy(digest[:], msg.BlobSHA256)
		_, err := s.backend.blobStore.Get(digest)
		if err != nil {
			continue
		}

		// A copy carries the source message's identity into the new folder, so
		// that both copies of a conversation still thread together.
		meta := msg.metadata()
		result, err := s.backend.messageStore.Insert(ctx, s.backend.db, destMB.ID, digest[:], msg.Size, msg.InternalDate, msg.Flags, meta)
		if err != nil {
			continue
		}
		uids = append(uids, result.UID)
	}

	srcUIDs := make(imap.UIDSet, len(messages))
	for i, msg := range messages {
		srcUIDs[i] = imap.UIDRange{Start: imap.UID(msg.UID), Stop: imap.UID(msg.UID)}
	}

	destUIDs := make(imap.UIDSet, len(uids))
	for i, uid := range uids {
		destUIDs[i] = imap.UIDRange{Start: imap.UID(uid), Stop: imap.UID(uid)}
	}
	return &imap.CopyData{
		UIDValidity: s.selected.uidValidity,
		SourceUIDs:  srcUIDs,
		DestUIDs:    destUIDs,
	}, nil
}

// Move moves messages to another mailbox.
func (s *session) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	ctx := context.Background()

	if s.selected == nil {
		return ErrMailboxSelected
	}

	destName := strings.Trim(dest, "/")
	destMB, err := s.backend.mailboxStore.GetByName(ctx, s.backend.db, s.user, destName)
	if err != nil {
		return ErrMailboxNotFound
	}

	messages, err := s.backend.messageStore.GetByUIDSet(ctx, s.backend.db, s.selected.mailboxID, numSet)
	if err != nil {
		return fmt.Errorf("imap: move: %w", err)
	}

	srcUIDs := make(imap.UIDSet, len(messages))
	moving := make([]uint32, 0, len(messages))
	for i, msg := range messages {
		srcUIDs[i] = imap.UIDRange{Start: imap.UID(msg.UID), Stop: imap.UID(msg.UID)}
		moving = append(moving, msg.UID)
	}

	// The rows are relocated, not reinserted, so a message keeps its message
	// ID, thread key and address indexes across the move.
	destUIDs, err := s.backend.messageStore.MoveTo(ctx, s.backend.db, s.selected.mailboxID, moving, destMB.ID)
	if err != nil {
		return fmt.Errorf("imap: move: %w", err)
	}

	for range messages {
		s.selected.messageCount--
		w.WriteExpunge(s.selected.messageCount + 1)
	}

	w.WriteCopyData(&imap.CopyData{
		UIDValidity: s.selected.uidValidity,
		SourceUIDs:  srcUIDs,
		DestUIDs:    imap.UIDSet(toUIDSet(destUIDs)),
	})

	return nil
}

// toUIDSet turns a UID list into the set form the IMAP response needs.
func toUIDSet(uids []uint32) []imap.UIDRange {
	ranges := make([]imap.UIDRange, 0, len(uids))
	for _, uid := range uids {
		ranges = append(ranges, imap.UIDRange{Start: imap.UID(uid), Stop: imap.UID(uid)})
	}
	return ranges
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// selectMailbox opens and selects a mailbox by name.
func (s *session) selectMailbox(name string, readOnly bool) error {
	ctx := context.Background()

	name = strings.Trim(name, "/")
	if name == "" {
		name = "INBOX"
	}

	mb, err := s.backend.mailboxStore.GetByName(ctx, s.backend.db, s.user, name)
	if err != nil {
		return ErrMailboxNotFound
	}

	count, err := s.backend.messageStore.CountByMailbox(ctx, s.backend.db, mb.ID)
	if err != nil {
		return fmt.Errorf("imap: count: %w", err)
	}

	s.selected = &selectedMailbox{
		name:         mb.Name,
		mailboxID:    mb.ID,
		uidValidity:  mb.UIDValidity,
		uidNext:      mb.UIDNext,
		messageCount: uint32(count),
	}
	s.readonly = readOnly

	return nil
}

// ---------------------------------------------------------------------------
// Message header parsing
// ---------------------------------------------------------------------------

type messageHeaders struct {
	fromAddr     string
	fromName     string
	subject      string
	internalDate time.Time
	flags        []string
}

// ---------------------------------------------------------------------------
// Envelope building
// ---------------------------------------------------------------------------

func buildEnvelope(msg Message) *imap.Envelope {
	if msg.FromAddress == "" {
		return nil
	}

	env := &imap.Envelope{
		From: []imap.Address{
			{
				Name:    msg.FromName,
				Mailbox: msg.FromAddress,
			},
		},
		Subject: msg.Subject,
		Date:    msg.InternalDate,
	}

	return env
}

// ---------------------------------------------------------------------------
// Flag helpers
// ---------------------------------------------------------------------------

func convertFlags(strFlags []string) []imap.Flag {
	flags := make([]imap.Flag, len(strFlags))
	for i, f := range strFlags {
		flags[i] = imap.Flag(f)
	}
	return flags
}

func addFlag(flags []string, flag string) []string {
	for _, f := range flags {
		if f == flag {
			return flags
		}
	}
	return append(flags, flag)
}

func removeFlag(flags []string, flag string) []string {
	result := make([]string, 0, len(flags))
	for _, f := range flags {
		if f != flag {
			result = append(result, f)
		}
	}
	return result
}

func hasFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Quota
// ---------------------------------------------------------------------------

func getQuotaLimit(ctx context.Context, db *sqlx.DB, address string) (int64, error) {
	// First check user-specific quota.
	usr, err := store.NewUsers().Get(ctx, db, address)
	if err != nil {
		return 0, err
	}

	if usr.QuotaBytes != nil {
		if *usr.QuotaBytes == 0 {
			return 0, nil // unlimited
		}
		return *usr.QuotaBytes, nil
	}

	// Fall back to default 10GB.
	return int64(10_737_418_240), nil
}

// ---------------------------------------------------------------------------
// Digest helpers
// ---------------------------------------------------------------------------

func digestFromHex(hexStr string) blobstore.Digest {
	var d blobstore.Digest
	b, err := hex.DecodeString(hexStr)
	if err == nil && len(b) == len(d) {
		copy(d[:], b)
	}
	return d
}

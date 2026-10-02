package inbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	sasl "github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/blobstore"
	imapd "github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/logging"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
)

// defaultQuotaBytes applies when neither the user nor the domain sets a quota,
// matching the IMAP append path.
const defaultQuotaBytes int64 = 10 * 1024 * 1024 * 1024

// errUnknownRecipient means no user, alias or shared mailbox accepts the
// address. It is a definitive answer, distinct from a database error.
var errUnknownRecipient = errors.New("inbound: unknown recipient")

// Backend implements smtp.Backend for the :25 inbound listener. It resolves
// recipients, checks SPF/DKIM/DMARC, greylists first contacts and routes
// messages into INBOX or Junk by spam score.
type Backend struct {
	db        *sqlx.DB
	cfg       config.Config
	log       *slog.Logger
	blobs     *blobstore.Store
	dns       Resolver
	greylist  *Greylist
	rates     *Rates
	users     *store.Users
	aliases   *store.Aliases
	quotas    *store.Quotas
	mailboxes *imapd.MailboxRepo
	messages  *imapd.MessageRepo
}

// NewBackend builds the inbound backend.
func NewBackend(db *sqlx.DB, cfg config.Config, log *slog.Logger, blobs *blobstore.Store) *Backend {
	return &Backend{
		db:        db,
		cfg:       cfg,
		log:       log,
		blobs:     blobs,
		dns:       NewResolver(),
		greylist:  NewGreylist(cfg.Mail.Inbound.Greylisting),
		rates:     NewRates(),
		users:     store.NewUsers(),
		aliases:   store.NewAliases(),
		quotas:    store.NewQuotas(),
		mailboxes: imapd.NewMailboxRepo(),
		messages:  imapd.NewMessageRepo(),
	}
}

// Server wraps the inbound SMTP listener.
type Server struct {
	srv *smtp.Server
}

// New builds the inbound listener, or a disabled Server when the address is
// not configured.
func New(cfg config.Config, backend *Backend, log *slog.Logger) (*Server, error) {
	if !cfg.Listener.SMTPInbound.Enabled() {
		return &Server{}, nil
	}
	s := smtp.NewServer(backend)
	s.Addr = cfg.Listener.SMTPInbound.Addr
	s.Domain = cfg.Server.Hostname
	s.MaxRecipients = cfg.Mail.Inbound.MaxRecipients
	s.MaxMessageBytes = cfg.Listener.SMTPInboundMaxBytes
	// MaxLineLength is left at the go-smtp default: the limit also applies to
	// body lines, and the RFC 5322 figure of 998 would reject real messages.
	s.ReadTimeout = 60 * time.Second
	s.WriteTimeout = 60 * time.Second
	s.EnableSMTPUTF8 = true
	s.ErrorLog = logging.NewPrintf(log)
	return &Server{srv: s}, nil
}

// Enabled reports whether a listener will be started.
func (sv *Server) Enabled() bool { return sv.srv != nil }

// Start listens and serves until the process is done.
func (sv *Server) Start(ctx context.Context) error {
	if sv.srv == nil {
		return nil
	}
	ln, err := net.Listen("tcp", sv.srv.Addr)
	if err != nil {
		return fmt.Errorf("inbound smtp: %w", err)
	}
	return sv.srv.Serve(ln)
}

// session holds the state for one inbound connection.
type session struct {
	conn      *smtp.Conn
	backend   *Backend
	ip        netip.Addr
	envFrom   string
	envDomain string
	// owners are the mailboxes this transaction delivers to, one per RCPT TO.
	owners  []string
	signals Signals
}

// NewSession is called when a client connects and issues EHLO/HELO.
func (b *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	s := &session{conn: c, backend: b}
	if r, ok := c.Conn().RemoteAddr().(*net.TCPAddr); ok {
		if a, ok := netip.AddrFromSlice(r.IP); ok {
			s.ip = a
		}
	}
	// Opportunistic maintenance: drop greylist entries that expired without a
	// valid retry.
	if _, err := b.greylist.Prune(context.Background(), b.db); err != nil {
		b.log.Warn("greylist prune failed", "error", err)
	}
	return s, nil
}

// AuthMechanisms reports that authentication is not offered. Inbound mail is
// never authenticated; it is identified by its source address instead.
func (s *session) AuthMechanisms() []string { return nil }

// Auth is never called because no mechanisms are advertised.
func (s *session) Auth(mech string) (sasl.Server, error) {
	return nil, smtp.ErrAuthUnsupported
}

// Mail records the envelope sender and runs the connection-level checks.
func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	ctx := s.context()
	s.envFrom = strings.TrimSpace(from)
	if s.envFrom == "<>" {
		s.envFrom = ""
	}
	if s.envFrom != "" {
		s.envDomain = domainOf(s.envFrom)
		res, err := CheckSPF(ctx, s.ip, s.envDomain, s.backend.dns)
		if err != nil {
			s.backend.log.Warn("SPF evaluation failed", "error", err)
			res = SPFTempError
		}
		s.signals.SPF = res
	}

	// Only the content-level signals can reject a message, because the DKIM and
	// DMARC verdicts need the body. The connection signals collected here are
	// scored together in Data.
	if s.backend.cfg.Mail.Inbound.Spam.Enabled {
		if name := s.conn.Hostname(); name != "" {
			if a, err := netip.ParseAddr(name); err == nil && (a.Is4() || a.Is6()) {
				s.signals.HELOIsIP = true
			}
		}
		if ptr, err := s.backend.dns.PTR(ctx, s.ip); err != nil || ptr == "" {
			s.signals.PTRPresent = false
		} else {
			s.signals.PTRPresent = true
		}
		if n, err := s.backend.rates.WindowCount(ctx, s.backend.db, "inbound", s.ip.String(), RateWindow); err != nil {
			s.backend.log.Warn("rate count failed", "error", err)
		} else {
			s.signals.RateCount = n
		}
	}

	action, err := s.backend.greylist.Check(ctx, s.backend.db, s.ip, s.envFrom, time.Now().UTC())
	if err != nil {
		s.backend.log.Warn("greylist check failed; accepting", "error", err)
	}
	if action == GreyDefer {
		return &smtp.SMTPError{
			Code:         450,
			EnhancedCode: smtp.EnhancedCode{4, 7, 0},
			Message:      "greylisted, please retry later",
		}
	}
	return nil
}

// Rcpt records a resolved recipient for this transaction.
func (s *session) Rcpt(to string, opts *smtp.RcptOptions) error {
	to = strings.ToLower(strings.TrimSpace(to))
	owner, err := s.backend.resolveRecipient(s.context(), to)
	if err != nil {
		if errors.Is(err, errUnknownRecipient) {
			if s.backend.cfg.Mail.Inbound.RejectUnknownRecipients {
				return &smtp.SMTPError{
					Code:         550,
					EnhancedCode: smtp.EnhancedCode{5, 1, 1},
					Message:      "no such mailbox",
				}
			}
			return nil
		}
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message:      "unable to look up recipient",
		}
	}
	s.owners = append(s.owners, owner)
	return nil
}

// Data receives the message, applies the content-level checks and delivers it.
func (s *session) Data(r io.Reader) error {
	ctx := s.context()
	maxBytes := s.backend.cfg.Listener.SMTPInboundMaxBytes
	limit := int64(0)
	if maxBytes > 0 {
		limit = maxBytes + 1
	}
	data, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		// The server's own size limit trips first when it is set, and its
		// error is already the right permanent answer.
		if errors.Is(err, smtp.ErrDataTooLarge) {
			return err
		}
		s.backend.log.Warn("reading message failed", "error", err)
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message:      "error reading message",
		}
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return smtp.ErrDataTooLarge
	}

	// Every recipient was unknown and unknown recipients are accepted: the
	// message is discarded silently rather than bounced, so that the setting
	// does not turn the listener into an address oracle.
	if len(s.owners) == 0 {
		s.backend.log.Debug("inbound message discarded, no valid recipients",
			"from", s.envFrom, "ip", s.ip.String())
		return nil
	}

	spam := s.backend.cfg.Mail.Inbound.Spam
	junk := false
	if spam.Enabled {
		verifs := DKIMVerify(ctx, data, s.backend.dns)
		s.signals.HasDKIM = len(verifs) > 0
		s.signals.DKIMValid = DKIMValid(verifs)
		s.fillDMARCSignals(ctx, data, verifs)
		sc := ScoreMessage(s.signals)
		if sc.Total >= spam.RejectThreshold {
			s.backend.log.Warn("inbound message rejected",
				"from", s.envFrom, "score", sc.Total, "parts", sc.Parts)
			return &smtp.SMTPError{
				Code:         550,
				EnhancedCode: smtp.EnhancedCode{5, 7, 1},
				Message:      "message rejected: " + scoreSummary(sc),
			}
		}
		junk = sc.Total >= spam.JunkThreshold
	}

	digest, size, err := s.backend.blobs.Put(data)
	if err != nil {
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message:      "unable to store message",
		}
	}

	meta, err := mail.Describe(data)
	if err != nil {
		return &smtp.SMTPError{
			Code:         554,
			EnhancedCode: smtp.EnhancedCode{5, 6, 0},
			Message:      "message could not be parsed",
		}
	}
	internalDate := meta.Date
	if internalDate.IsZero() {
		internalDate = time.Now().UTC()
	}

	var flags []string
	if junk {
		flags = []string{"\\Junk"}
	}

	if err := s.deliver(ctx, digest, size, flags, meta, internalDate); err != nil {
		return err
	}

	if spam.Enabled {
		if err := s.backend.rates.Increment(ctx, s.backend.db, "inbound", s.ip.String(), RateWindow); err != nil {
			s.backend.log.Warn("rate increment failed", "error", err)
		}
	}
	s.backend.log.Info("inbound message delivered",
		"from", s.envFrom,
		"to", len(s.owners),
		"bytes", size,
		"junk", junk,
	)
	return nil
}

// deliver stores the message in every recipient's INBOX.
//
// Quota is checked for all recipients first so a full mailbox is reported
// before anything is written; a failure between the check and the insert is
// possible in principle and would cost the sender a retry.
func (s *session) deliver(ctx context.Context, digest blobstore.Digest, size int64,
	flags []string, meta mail.Metadata, internalDate time.Time,
) error {
	owners := make([]string, 0, len(s.owners))
	seen := map[string]bool{}
	for _, owner := range s.owners {
		if !seen[owner] {
			seen[owner] = true
			owners = append(owners, owner)
		}
	}
	for _, owner := range owners {
		mbID, err := s.backend.ensureInbox(ctx, owner)
		if err != nil {
			return &smtp.SMTPError{
				Code:         451,
				EnhancedCode: smtp.EnhancedCode{4, 3, 0},
				Message:      "unable to access mailbox",
			}
		}
		limit := s.backend.quotaLimit(ctx, owner)
		if limit > 0 {
			usage, err := s.backend.quotas.Get(ctx, s.backend.db, owner)
			if err != nil {
				return &smtp.SMTPError{
					Code:         451,
					EnhancedCode: smtp.EnhancedCode{4, 3, 0},
					Message:      "unable to check quota",
				}
			}
			if usage.UsedBytes+size > limit {
				return &smtp.SMTPError{
					Code:         452,
					EnhancedCode: smtp.EnhancedCode{4, 3, 1},
					Message:      "mailbox over quota",
				}
			}
		}
		if _, err := s.backend.messages.Insert(ctx, s.backend.db, mbID, digest[:], size,
			internalDate, flags, meta); err != nil {
			// The sender gets a temporary failure and will retry, but the
			// operator needs to know why: without this the only symptom is a
			// mailbox that never fills up.
			s.backend.log.Error("store message failed",
				"owner", owner, "error", err)
			return &smtp.SMTPError{
				Code:         451,
				EnhancedCode: smtp.EnhancedCode{4, 3, 0},
				Message:      "unable to store message",
			}
		}
		if _, err := s.backend.quotas.Adjust(ctx, s.backend.db, owner, size, 1); err != nil {
			return &smtp.SMTPError{
				Code:         451,
				EnhancedCode: smtp.EnhancedCode{4, 3, 0},
				Message:      "unable to update quota",
			}
		}
	}
	return nil
}

// Reset clears the transaction state.
func (s *session) Reset() {
	s.envFrom = ""
	s.envDomain = ""
	s.owners = nil
	s.signals = Signals{}
}

// Logout is called when the connection closes.
func (s *session) Logout() error { return nil }

func (s *session) context() context.Context { return context.Background() }

// fillDMARCSignals evaluates DMARC for the message and records the outcome on
// the session signals.
func (s *session) fillDMARCSignals(ctx context.Context, data []byte, verifs []*dkim.Verification) {
	meta, describeErr := mail.Describe(data)
	fromAddr := ""
	if describeErr == nil {
		fromAddr = meta.FromAddress
	}
	if fromAddr == "" {
		return
	}
	res, err := DMARCEvaluate(ctx, s.backend.dns, DMARCInput{
		FromDomain:     domainOf(fromAddr),
		EnvelopeDomain: s.envDomain,
		SPF:            s.signals.SPF,
		DKIM:           verifs,
	})
	if err != nil {
		s.backend.log.Warn("DMARC lookup failed", "error", err)
		return
	}
	if res.Record == nil {
		return
	}
	policy := res.Policy
	s.signals.DMARCPolicy = &policy
	if policy != dmarc.PolicyNone && !res.Aligned {
		s.signals.DMARCFails = true
	}
}

// resolveRecipient maps a recipient address to the mailbox that will hold the
// message.
//
// A users row is the only kind of mailbox there is: a shared mailbox is a user
// with source 'shared', which is why shared mailboxes need no separate lookup
// here. An alias names such a row, and the schema forbids an alias pointing at
// itself, so one hop always reaches a mailbox.
func (b *Backend) resolveRecipient(ctx context.Context, to string) (string, error) {
	if to == "" {
		return "", errUnknownRecipient
	}
	if address, ok, err := b.mailbox(ctx, to); err != nil || ok {
		return address, err
	}

	alias, err := b.aliases.Get(ctx, b.db, to)
	if err != nil {
		if store.IsNotFound(err) {
			return "", errUnknownRecipient
		}
		return "", err
	}
	if !alias.Enabled || alias.TargetAddress == nil {
		// A workgroup alias, which has no delivery path of its own yet.
		return "", errUnknownRecipient
	}
	address, ok, err := b.mailbox(ctx, strings.ToLower(*alias.TargetAddress))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errUnknownRecipient
	}
	return address, nil
}

// mailbox reports the address of the mailbox at address, and whether it may
// receive mail at all.
func (b *Backend) mailbox(ctx context.Context, address string) (string, bool, error) {
	usr, err := b.users.Get(ctx, b.db, address)
	if err != nil {
		if store.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if !usr.Enabled || !usr.MailboxEnabled {
		return "", false, nil
	}
	return usr.Address, true, nil
}

// ensureInbox returns the id of the owner's INBOX, creating it on first use.
func (b *Backend) ensureInbox(ctx context.Context, owner string) (int64, error) {
	mb, err := b.mailboxes.GetByName(ctx, b.db, owner, "INBOX")
	if err == nil {
		return mb.ID, nil
	}
	if !store.IsNotFound(err) {
		return 0, err
	}
	if err := b.mailboxes.Create(ctx, b.db, owner, "INBOX", nil); err != nil {
		if store.IsConflict(err) {
			mb, gerr := b.mailboxes.GetByName(ctx, b.db, owner, "INBOX")
			if gerr != nil {
				return 0, gerr
			}
			return mb.ID, nil
		}
		return 0, err
	}
	mb, err = b.mailboxes.GetByName(ctx, b.db, owner, "INBOX")
	if err != nil {
		return 0, err
	}
	return mb.ID, nil
}

// quotaLimit mirrors the IMAP append path: an explicit zero is unlimited, a
// nil quota inherits the default. A failed lookup falls back to the default
// rather than failing the message, since refusing mail over a quota read would
// lose it for no good reason.
func (b *Backend) quotaLimit(ctx context.Context, owner string) int64 {
	usr, err := b.users.Get(ctx, b.db, owner)
	if err != nil || usr.QuotaBytes == nil {
		return defaultQuotaBytes
	}
	if *usr.QuotaBytes == 0 {
		return 0
	}
	return *usr.QuotaBytes
}

// domainOf returns the lowercased domain part of an address.
func domainOf(address string) string {
	i := strings.LastIndexByte(address, '@')
	if i < 0 {
		return strings.ToLower(address)
	}
	return strings.ToLower(address[i+1:])
}

// scoreSummary renders the scoring parts for log and reject messages.
func scoreSummary(sc Score) string {
	parts := make([]string, 0, len(sc.Parts))
	for _, p := range sc.Parts {
		parts = append(parts, fmt.Sprintf("%s(%g)", p.Name, p.Score))
	}
	return fmt.Sprintf("score %.1f [%s]", sc.Total, strings.Join(parts, ", "))
}

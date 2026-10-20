package inbound

import (
	"context"
	"crypto/tls"
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
	"github.com/linchange/linchange/internal/acme"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/delivery"
	imapd "github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/logging"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// defaultQuotaBytes applies when neither the user nor the domain sets a quota,
// matching the IMAP append path.
const defaultQuotaBytes int64 = 10 * 1024 * 1024 * 1024

// errUnknownRecipient means no user, alias or shared mailbox accepts the
// address. It is a definitive answer, distinct from a database error.
var errUnknownRecipient = errors.New("inbound: unknown recipient")

// Backend implements smtp.Backend for the :25 inbound listener. It resolves
// recipients, checks SPF/DKIM/DMARC, greylists first contacts and routes
// messages into INBOX or Junk by spam score, running each owner's rules on the
// way in.
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
	rules     *store.MailRules
	outbox    *delivery.OutboxRepository

	// Scheduling applies an iTIP message to the owner's calendar once the
	// message is stored: the REPLY an external mail client sends when its
	// user clicks Accept, the REQUEST that files a meeting they were invited
	// to, the CANCEL that withdraws one. It is nil when nothing wires it,
	// and a nil hook leaves inbound mail exactly as it was - mail.
	//
	// It runs once per stored recipient, because the answer belongs to
	// whoever the message was addressed to, and it is called only after that
	// recipient's copy is in the mailbox. It must never fail the delivery:
	// a calendar that refuses a scheduling message is not a reason for the
	// sender's client to retry the whole delivery, and the message itself is
	// already stored and readable.
	Scheduling func(ctx context.Context, owner string, document []byte)
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
		rules:     store.NewMailRules(),
		outbox:    delivery.NewOutboxRepository(db),
	}
}

// Server wraps the inbound SMTP listener.
type Server struct {
	srv *smtp.Server
}

// New builds the inbound listener, or a disabled Server when the address is
// not configured.
//
// STARTTLS is offered on this port whenever a certificate is available, either
// from ACME or from the configured static pair. Remote senders negotiate it to
// protect the transfer; without a TLS config go-smtp does not advertise the
// extension at all, which is the honest answer when there is no certificate.
func New(cfg config.Config, backend *Backend, log *slog.Logger, acmeManager *acme.Manager) (*Server, error) {
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
	if tlsCfg := serverTLSConfig(cfg, acmeManager, log); tlsCfg != nil {
		s.TLSConfig = tlsCfg
	}
	return &Server{srv: s}, nil
}

// serverTLSConfig builds the TLS configuration used to answer STARTTLS on the
// inbound port, preferring ACME over the static certificate pair. It returns
// nil when neither is configured, which is how the caller learns that STARTTLS
// cannot be offered.
func serverTLSConfig(cfg config.Config, acmeManager *acme.Manager, log *slog.Logger) *tls.Config {
	if acmeManager != nil {
		return acmeManager.TLSConfig()
	}
	if cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		log.Warn("failed to load TLS cert; STARTTLS will not be offered",
			"cert", cfg.TLS.CertFile, "err", err)
		return nil
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
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

	// A message arriving from this machine's own loopback interface is the
	// submission path handing local mail back for local delivery, not a
	// stranger's first attempt: greylisting exists to make strangers retry,
	// and would only delay a user's own mail to their own server by the full
	// deferral window.
	if !s.ip.IsLoopback() {
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

	parsed, err := mail.Parse(data)
	if err != nil {
		return &smtp.SMTPError{
			Code:         554,
			EnhancedCode: smtp.EnhancedCode{5, 6, 0},
			Message:      "message could not be parsed",
		}
	}
	meta := parsed.Metadata()
	internalDate := meta.Date
	if internalDate.IsZero() {
		internalDate = time.Now().UTC()
	}
	facts := webmail.NewMatchContext(parsed, int64(len(data)))

	var flags []string
	if junk {
		flags = []string{"\\Junk"}
	}

	if err := s.deliver(ctx, digest, size, flags, meta, internalDate, facts, parsed); err != nil {
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

// deliver stores the message for every recipient, applying each owner's rules
// on the way in.
//
// Quota is checked per recipient before its insert so a full mailbox is
// reported before anything is written for it; a failure between the check and
// the insert is possible in principle and would cost the sender a retry.
//
// Rules decide, per owner: whether the message is discarded at all, which
// folder it is filed in, which flags it carries, and whether a copy goes out to
// another address. A rule that cannot be read degrades to plain delivery
// rather than to refusing the message.
//
// The parsed message travels alongside its digest because a scheduling
// message is handed to the calendar layer once it is stored for an owner -
// the bytes are already in hand, and re-reading them from the blob store for
// every delivery of every message would buy nothing.
func (s *session) deliver(ctx context.Context, digest blobstore.Digest, size int64,
	flags []string, meta mail.Metadata, internalDate time.Time, facts webmail.MatchContext,
	parsed *mail.Parsed,
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
		outcome, ok := s.backend.ownerOutcome(ctx, owner, facts)

		// A discarded message is not stored for this owner, but a forward asked
		// for in the same rule set still goes out: "send it to the spam trap and
		// keep nothing" is the point of combining the two.
		if ok && outcome.Discard {
			s.backend.log.Info("inbound message discarded by rule",
				"owner", owner, "rules", outcome.Matched)
			if outcome.Forward != "" {
				s.backend.queueForward(ctx, owner, digest, size, outcome.Forward)
			}
			continue
		}

		dest := "INBOX"
		if ok && outcome.Destination != "" {
			dest = outcome.Destination
		}
		mbID, err := s.backend.ensureMailbox(ctx, owner, dest)
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
			internalDate, mergeFlags(flags, outcome.Flags), meta); err != nil {
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
		// The scheduling answer is given after the recipient's copy exists,
		// so an answer the calendar layer refuses is still mail the recipient
		// can read and answer again from their client.
		s.backend.applyScheduling(ctx, owner, parsed)
		if ok {
			if len(outcome.Matched) > 0 {
				s.backend.log.Info("rules applied to inbound message",
					"owner", owner, "rules", outcome.Matched, "folder", dest)
			}
			if outcome.Forward != "" {
				s.backend.queueForward(ctx, owner, digest, size, outcome.Forward)
			}
		}
	}
	return nil
}

// applyScheduling hands a delivered message to the iTIP processor, when this
// deployment wired one.
//
// Everything about it is best effort: no hook means no scheduling and nothing
// else changes; a message that is not a scheduling message costs one scan of
// its parts; and any failure inside the calendar layer is logged rather than
// returned, because the message is already stored and the sender's client must
// not be told to retry a delivery that succeeded.
func (b *Backend) applyScheduling(ctx context.Context, owner string, parsed *mail.Parsed) {
	if b.Scheduling == nil || parsed == nil {
		return
	}
	document := parsed.ScheduleDocument()
	if len(document) == 0 {
		return
	}
	b.Scheduling(ctx, owner, document)
}

// ownerOutcome runs the owner's enabled rules against the message facts.
//
// It reports whether the decision may be trusted: a failure to read or evaluate
// the rules means the caller should deliver the ordinary way, because refusing
// inbound mail over a settings read would turn a transient fault into lost mail.
func (b *Backend) ownerOutcome(ctx context.Context, owner string, facts webmail.MatchContext) (webmail.RuleOutcome, bool) {
	rows, err := b.rules.ListEnabled(ctx, b.db, owner)
	if err != nil {
		b.log.Warn("rules could not be read; delivering without them",
			"owner", owner, "error", err)
		return webmail.RuleOutcome{}, false
	}
	out, err := webmail.ApplyRules(rows, facts)
	if err != nil {
		b.log.Warn("rule evaluation failed; delivering without it",
			"owner", owner, "error", err)
		return webmail.RuleOutcome{}, false
	}
	return out, true
}

// queueForward enqueues a copy of the message for a rule-requested forward.
//
// The blob already holds the full message, so the forward reuses its digest and
// the recipient gets exactly the bytes the owner received. The rule names the
// recipient as an RFC 5322 address, possibly with a display name, so the bare
// address is extracted before it is queued: the outbox stores what goes into an
// RCPT TO, and a display name there would only break the delivery. A forward
// that fails to queue is logged rather than fatal: the owner still has the
// message, and a temporary SMTP failure would otherwise bounce mail that was
// accepted.
func (b *Backend) queueForward(ctx context.Context, owner string, digest blobstore.Digest, size int64, to string) {
	addrs, err := mail.ParseAddressList(to)
	if err != nil || len(addrs) == 0 || strings.TrimSpace(addrs[0].Address) == "" {
		b.log.Warn("rule forward named no usable address", "owner", owner, "to", to, "error", err)
		return
	}
	to = strings.ToLower(strings.TrimSpace(addrs[0].Address))
	_, err = b.outbox.InsertMessage(ctx, nil, owner, owner, digest[:], size,
		[]delivery.OutboxRecipient{{Position: 0, Address: to, Kind: delivery.To}})
	if err != nil {
		b.log.Warn("could not queue rule forward", "owner", owner, "to", to, "error", err)
		return
	}
	b.log.Info("rule forward queued", "owner", owner, "to", to)
}

// mergeFlags joins the delivery flags (junk) with the rule flags, repeating
// neither.
func mergeFlags(base, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra))
	seen := map[string]bool{}
	for _, group := range [][]string{base, extra} {
		for _, f := range group {
			if seen[f] {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
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
	if !alias.Enabled {
		return "", errUnknownRecipient
	}
	// Try each target in order until one succeeds.
	for _, t := range alias.Targets {
		if t.TargetAddress == nil {
			// Workgroup target - skip for now (would need workgroup member expansion)
			continue
		}
		address, ok, err := b.mailbox(ctx, strings.ToLower(*t.TargetAddress))
		if err != nil {
			return "", err
		}
		if ok {
			return address, nil
		}
		// Target doesn't have a mailbox, try next target
	}
	return "", errUnknownRecipient
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

// ensureMailbox returns the id of the owner's mailbox named name, creating it
// on first use.
func (b *Backend) ensureMailbox(ctx context.Context, owner, name string) (int64, error) {
	mb, err := b.mailboxes.GetByName(ctx, b.db, owner, name)
	if err == nil {
		return mb.ID, nil
	}
	if !store.IsNotFound(err) {
		return 0, err
	}
	if err := b.mailboxes.Create(ctx, b.db, owner, name, nil); err != nil {
		if store.IsConflict(err) {
			mb, gerr := b.mailboxes.GetByName(ctx, b.db, owner, name)
			if gerr != nil {
				return 0, gerr
			}
			return mb.ID, nil
		}
		return 0, err
	}
	mb, err = b.mailboxes.GetByName(ctx, b.db, owner, name)
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

package inbound

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

func boolPtr(b bool) *bool { return &b }

// inboundFixture is a running :25 listener over a migrated test database.
type inboundFixture struct {
	db    *sqlx.DB
	cfg   *config.Config
	dns   *fixtureResolver
	blobs *blobstore.Store
	addr  string
}

// setupInbound starts a listener on a random port and seeds the directory with
// one enabled user, one disabled user, one user that may not hold mail, one
// shared mailbox and an alias.
func setupInbound(t *testing.T, tune func(*config.Config)) *inboundFixture {
	t.Helper()

	db := testdb.Migrated(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO domains (name) VALUES ('example.com')`); err != nil {
		t.Fatalf("seeding domain: %v", err)
	}
	users := store.NewUsers()
	for _, u := range []struct {
		address string
		source  policy.Source
		enabled *bool
		mailbox *bool
	}{
		{address: "alice@example.com", source: policy.SourceLocal},
		{address: "disabled@example.com", source: policy.SourceLocal, enabled: boolPtr(false)},
		{address: "nomailbox@example.com", source: policy.SourceLocal, mailbox: boolPtr(false)},
		{address: "support@example.com", source: policy.SourceShared},
	} {
		if err := users.Create(ctx, db, store.CreateUserRequest{
			Address:        u.address,
			Source:         u.source,
			Enabled:        u.enabled,
			MailboxEnabled: u.mailbox,
		}); err != nil {
			t.Fatalf("creating %s: %v", u.address, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address) VALUES ('support@example.com')`); err != nil {
		t.Fatalf("declaring shared mailbox: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO aliases (address, target_kind, target_address, enabled)
		VALUES ('team@example.com', 'user', 'alice@example.com', true)`); err != nil {
		t.Fatalf("creating alias: %v", err)
	}
	// An alias onto a principal that may not hold mail, to prove the alias does
	// not launder it.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO aliases (address, target_kind, target_address, enabled)
		VALUES ('togroup@example.com', 'user', 'nomailbox@example.com', true)`); err != nil {
		t.Fatalf("creating alias: %v", err)
	}
	var workgroupID int64
	if err := db.GetContext(ctx, &workgroupID,
		`INSERT INTO workgroups (name, domain) VALUES ('crew', 'example.com') RETURNING id`); err != nil {
		t.Fatalf("seeding workgroup: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO aliases (address, target_kind, workgroup_id, enabled)
		VALUES ('crew@example.com', 'workgroup', $1, true)`, workgroupID); err != nil {
		t.Fatalf("creating workgroup alias: %v", err)
	}

	blobs, err := blobstore.Open(blobstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("opening blobstore: %v", err)
	}

	dbURL, _ := testdb.URL()
	cfg := config.New()
	cfg.Database.URL = dbURL
	cfg.Listener.SMTPInbound.Addr = "127.0.0.1:0"
	cfg.Listener.SMTPInboundMaxBytes = 52_428_800
	cfg.Mail.Inbound.Greylisting.Enabled = false
	if tune != nil {
		tune(cfg)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	backend := NewBackend(db, *cfg, log, blobs)
	// A known-good sender by default: SPF passes for the loopback client and
	// reverse DNS exists, so the score stays below the junk threshold.
	dns := newFixtureResolver()
	dns.txt["example.com"] = []string{"v=spf1 ip4:127.0.0.1 -all"}
	dns.ptr["127.0.0.1"] = "mail.example.com"
	backend.dns = dns

	srv, err := New(*cfg, backend, log)
	if err != nil {
		t.Fatalf("creating inbound server: %v", err)
	}
	if !srv.Enabled() {
		t.Fatal("inbound server reported itself disabled")
	}
	ln, err := net.Listen("tcp", cfg.Listener.SMTPInbound.Addr)
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go func() { _ = srv.srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.srv.Close()
		ln.Close()
	})

	return &inboundFixture{db: db, cfg: cfg, dns: dns, blobs: blobs, addr: ln.Addr().String()}
}

// storedMessage is one row of the messages table joined to its mailbox.
type storedMessage struct {
	Owner   string `db:"owner"`
	Subject string `db:"subject"`
	From    string `db:"from_address"`
	Flags   []string
	Size    int64  `db:"size"`
	Digest  []byte `db:"blob_sha256"`
}

func (f *inboundFixture) messages(t *testing.T, owner string) []storedMessage {
	t.Helper()
	var out []storedMessage
	err := f.db.SelectContext(context.Background(), &out, `
		SELECT mb.owner, m.subject, m.from_address, m.flags, m.size, m.blob_sha256
		FROM messages m JOIN mailboxes mb ON mb.id = m.mailbox_id
		WHERE mb.owner = $1 ORDER BY m.uid`, owner)
	if err != nil {
		t.Fatalf("reading messages of %s: %v", owner, err)
	}
	return out
}

func (f *inboundFixture) quotaUsed(t *testing.T, owner string) int64 {
	t.Helper()
	var used int64
	err := f.db.GetContext(context.Background(), &used,
		`SELECT used_bytes FROM quota_usage WHERE owner = $1`, owner)
	if err != nil {
		t.Fatalf("reading quota usage of %s: %v", owner, err)
	}
	return used
}

// inboundMessage builds a small RFC 5322 message.
func inboundMessage(from, to, subject, body string) string {
	return strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"Date: Mon, 02 Jan 2006 15:04:05 +0000",
		"",
		body,
		"",
	}, "\r\n")
}

// wrappedBody returns a body of about n bytes made of realistic short lines,
// because the listener refuses message lines longer than its line limit.
func wrappedBody(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(strings.Repeat("x", 200))
		b.WriteString("\r\n")
	}
	return b.String()
}

// smtpSession is a minimal SMTP client that exposes the exact status codes, so
// that the tests assert on protocol answers rather than on error strings.
type smtpSession struct {
	t  *testing.T
	tp *textproto.Conn
}

func dialSMTP(t *testing.T, addr string) *smtpSession {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dialing %s: %v", addr, err)
	}
	s := &smtpSession{t: t, tp: textproto.NewConn(conn)}
	t.Cleanup(func() { conn.Close() })
	if code, _, err := s.tp.ReadResponse(220); err != nil {
		t.Fatalf("reading greeting: %d %v", code, err)
	}
	s.tp.PrintfLine("EHLO client.example.com")
	if code, msg, err := s.tp.ReadResponse(250); err != nil {
		t.Fatalf("EHLO: %d %s: %v", code, msg, err)
	}
	return s
}

// cmd sends a command and returns the status code and text.
func (s *smtpSession) cmd(format string, args ...any) (int, string) {
	s.t.Helper()
	if err := s.tp.PrintfLine(format, args...); err != nil {
		s.t.Fatalf("writing %s: %v", fmt.Sprintf(format, args...), err)
	}
	code, msg, err := s.tp.ReadResponse(0)
	if err != nil {
		s.t.Fatalf("reading response to %s: %v", fmt.Sprintf(format, args...), err)
	}
	return code, msg
}

// data sends DATA with msg and returns the final status code.
func (s *smtpSession) data(msg string) (int, string) {
	s.t.Helper()
	if err := s.tp.PrintfLine("DATA"); err != nil {
		s.t.Fatalf("writing DATA: %v", err)
	}
	code, reply, err := s.tp.ReadResponse(354)
	if err != nil {
		s.t.Fatalf("DATA: %d %s: %v", code, reply, err)
	}
	w := s.tp.DotWriter()
	if _, err := w.Write([]byte(msg)); err != nil {
		s.t.Fatalf("writing message: %v", err)
	}
	if err := w.Close(); err != nil {
		s.t.Fatalf("closing message: %v", err)
	}
	code, reply, err = s.tp.ReadResponse(0)
	if err != nil {
		s.t.Fatalf("end of DATA: %d %s: %v", code, reply, err)
	}
	return code, reply
}

// ---------------------------------------------------------------------------
// Delivery
// ---------------------------------------------------------------------------

// A message that scores below the junk threshold lands in INBOX, with its
// headers parsed, its bytes stored and its owner's quota updated.
func TestInboundDeliversMessageToInbox(t *testing.T) {
	f := setupInbound(t, nil)
	c := dialSMTP(t, f.addr)

	body := "the quick brown fox"
	if code, msg := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatalf("MAIL FROM = %d %s", code, msg)
	}
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	code, msg := c.data(inboundMessage("Sender <sender@example.com>", "alice@example.com", "Hello", body))
	if code != 250 {
		t.Fatalf("DATA = %d %s", code, msg)
	}

	msgs := f.messages(t, "alice@example.com")
	if len(msgs) != 1 {
		t.Fatalf("alice has %d messages, want 1", len(msgs))
	}
	got := msgs[0]
	if got.Subject != "Hello" {
		t.Errorf("subject = %q, want %q", got.Subject, "Hello")
	}
	if got.From != "sender@example.com" {
		t.Errorf("from_address = %q, want %q", got.From, "sender@example.com")
	}
	if len(got.Flags) != 0 {
		t.Errorf("flags = %v, want none", got.Flags)
	}
	if got.Size <= 0 {
		t.Errorf("size = %d, want > 0", got.Size)
	}
	if used := f.quotaUsed(t, "alice@example.com"); used != got.Size {
		t.Errorf("quota used = %d, want %d", used, got.Size)
	}

	// The stored blob must be the message that was sent.
	stored, err := f.blobs.Get(blobstore.Digest(got.Digest))
	if err != nil {
		t.Fatalf("reading blob: %v", err)
	}
	if !strings.Contains(string(stored), body) {
		t.Errorf("stored blob does not contain the body")
	}
}

// INBOX is created on demand, so a user who has never received mail has one.
func TestInboundCreatesInbox(t *testing.T) {
	f := setupInbound(t, nil)
	c := dialSMTP(t, f.addr)

	if code, _ := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatal("MAIL FROM rejected")
	}
	if code, _ := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatal("RCPT TO rejected")
	}
	if code, msg := c.data(inboundMessage("s@example.com", "alice@example.com", "First", "hi")); code != 250 {
		t.Fatalf("DATA = %d %s", code, msg)
	}

	var name string
	if err := f.db.GetContext(context.Background(), &name,
		`SELECT name FROM mailboxes WHERE owner = 'alice@example.com'`); err != nil {
		t.Fatalf("reading mailbox: %v", err)
	}
	if name != "INBOX" {
		t.Errorf("mailbox = %q, want INBOX", name)
	}
}

// An alias delivers to its target, and a recipient named twice is stored once.
func TestInboundFollowsAliasAndDeduplicates(t *testing.T) {
	f := setupInbound(t, nil)
	c := dialSMTP(t, f.addr)

	if code, _ := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatal("MAIL FROM rejected")
	}
	for _, to := range []string{"team@example.com", "alice@example.com", "TEAM@example.com"} {
		if code, msg := c.cmd("RCPT TO:<%s>", to); code != 250 {
			t.Fatalf("RCPT TO:%s = %d %s", to, code, msg)
		}
	}
	if code, msg := c.data(inboundMessage("s@example.com", "team@example.com", "Once", "hi")); code != 250 {
		t.Fatalf("DATA = %d %s", code, msg)
	}

	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 1 {
		t.Fatalf("alice has %d messages, want 1: duplicate recipients must store once", len(msgs))
	}
}

// A shared mailbox receives mail under its own address and quota.
func TestInboundDeliversToSharedMailbox(t *testing.T) {
	f := setupInbound(t, nil)
	c := dialSMTP(t, f.addr)

	if code, _ := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatal("MAIL FROM rejected")
	}
	if code, msg := c.cmd("RCPT TO:<support@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	if code, msg := c.data(inboundMessage("s@example.com", "support@example.com", "Ticket", "help")); code != 250 {
		t.Fatalf("DATA = %d %s", code, msg)
	}

	msgs := f.messages(t, "support@example.com")
	if len(msgs) != 1 || msgs[0].Subject != "Ticket" {
		t.Fatalf("shared mailbox messages = %+v, want one Ticket", msgs)
	}
	if used := f.quotaUsed(t, "support@example.com"); used != msgs[0].Size {
		t.Errorf("shared quota used = %d, want %d", used, msgs[0].Size)
	}
}

// ---------------------------------------------------------------------------
// Recipients
// ---------------------------------------------------------------------------

// A recipient that is unknown, disabled or may not hold mail is refused
// definitively, before any message body is accepted.
func TestInboundRejectsUndeliverableRecipients(t *testing.T) {
	f := setupInbound(t, nil)
	c := dialSMTP(t, f.addr)

	if code, _ := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatal("MAIL FROM rejected")
	}
	for _, to := range []string{
		"nobody@example.com",
		"disabled@example.com",
		"nomailbox@example.com",
	} {
		code, msg := c.cmd("RCPT TO:<%s>", to)
		if code != 550 {
			t.Errorf("RCPT TO:%s = %d %s, want 550", to, code, msg)
		}
	}
}

// With unknown recipients accepted, the transaction succeeds and nothing is
// stored: the listener must not act as an address oracle.
func TestInboundSilentlyDropsUnknownRecipients(t *testing.T) {
	f := setupInbound(t, func(c *config.Config) {
		c.Mail.Inbound.RejectUnknownRecipients = false
	})
	c := dialSMTP(t, f.addr)

	if code, _ := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatal("MAIL FROM rejected")
	}
	if code, msg := c.cmd("RCPT TO:<nobody@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s, want 250", code, msg)
	}
	code, msg := c.data(inboundMessage("s@example.com", "nobody@example.com", "Probe", "hi"))
	if code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 0 {
		t.Fatalf("alice has %d messages, want 0", len(msgs))
	}
}

// A bounce with a null envelope sender is accepted; greylisting has nothing to
// defer on, because a retried bounce would be dropped anyway.
func TestInboundAcceptsNullSender(t *testing.T) {
	f := setupInbound(t, func(c *config.Config) {
		c.Mail.Inbound.Greylisting = config.Greylisting{
			Enabled:     true,
			Delay:       time.Hour,
			Expiry:      4 * time.Hour,
			RetryWindow: 30 * time.Minute,
		}
	})
	c := dialSMTP(t, f.addr)

	if code, msg := c.cmd("MAIL FROM:<>"); code != 250 {
		t.Fatalf("MAIL FROM:<> = %d %s, want 250", code, msg)
	}
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	if code, msg := c.data(inboundMessage("postmaster@example.com", "alice@example.com", "Undeliverable", "bounced")); code != 250 {
		t.Fatalf("DATA = %d %s", code, msg)
	}
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 1 {
		t.Fatalf("alice has %d messages, want 1", len(msgs))
	}
}

// An alias does not launder an undeliverable target: it resolves to the
// mailbox it names, and that mailbox's own rules still apply. A workgroup alias
// is refused as well, since no delivery path expands one yet.
func TestInboundAliasRespectsTargetRules(t *testing.T) {
	f := setupInbound(t, nil)
	c := dialSMTP(t, f.addr)

	if code, _ := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatal("MAIL FROM rejected")
	}
	for _, to := range []string{"togroup@example.com", "crew@example.com"} {
		code, msg := c.cmd("RCPT TO:<%s>", to)
		if code != 550 {
			t.Errorf("RCPT TO:%s = %d %s, want 550", to, code, msg)
		}
	}
}

// ---------------------------------------------------------------------------
// Greylisting
// ---------------------------------------------------------------------------

// The first contact from a sender is deferred, and once the retry falls inside
// the window the message is accepted.
func TestInboundGreylistsFirstContact(t *testing.T) {
	f := setupInbound(t, func(c *config.Config) {
		c.Mail.Inbound.Greylisting = config.Greylisting{
			Enabled:     true,
			Delay:       time.Hour,
			Expiry:      4 * time.Hour,
			RetryWindow: 30 * time.Minute,
		}
	})

	c := dialSMTP(t, f.addr)
	code, msg := c.cmd("MAIL FROM:<sender@example.com>")
	if code != 450 {
		t.Fatalf("first MAIL FROM = %d %s, want 450", code, msg)
	}

	// Pretend the sender came back after the delay.
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE greylist SET first_seen_at = now() - interval '2 hours', accepted_at = now()
		 WHERE envelope_from = 'sender@example.com'`); err != nil {
		t.Fatalf("ageing greylist entry: %v", err)
	}

	c2 := dialSMTP(t, f.addr)
	if code, msg := c2.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatalf("retried MAIL FROM = %d %s, want 250", code, msg)
	}
	if code, msg := c2.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	if code, msg := c2.data(inboundMessage("s@example.com", "alice@example.com", "Retry", "hi")); code != 250 {
		t.Fatalf("DATA = %d %s", code, msg)
	}
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 1 {
		t.Fatalf("alice has %d messages, want 1", len(msgs))
	}
}

// ---------------------------------------------------------------------------
// Limits
// ---------------------------------------------------------------------------

// A message larger than the configured maximum is refused with a permanent
// size error.
func TestInboundRejectsOversizedMessage(t *testing.T) {
	f := setupInbound(t, func(c *config.Config) {
		c.Listener.SMTPInboundMaxBytes = 512
	})
	c := dialSMTP(t, f.addr)

	if code, _ := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatal("MAIL FROM rejected")
	}
	if code, _ := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatal("RCPT TO rejected")
	}
	code, msg := c.data(inboundMessage("s@example.com", "alice@example.com", "Big", wrappedBody(1024)))
	if code != 552 {
		t.Fatalf("DATA = %d %s, want 552", code, msg)
	}
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 0 {
		t.Fatalf("alice has %d messages, want 0", len(msgs))
	}
}

// A mailbox with no room left is deferred so the sender retries later, and the
// message is not stored.
func TestInboundDefersWhenOverQuota(t *testing.T) {
	f := setupInbound(t, nil)
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE users SET quota_bytes = 100 WHERE address = 'alice@example.com'`); err != nil {
		t.Fatalf("setting quota: %v", err)
	}
	c := dialSMTP(t, f.addr)

	if code, _ := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatal("MAIL FROM rejected")
	}
	if code, _ := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatal("RCPT TO rejected")
	}
	code, msg := c.data(inboundMessage("s@example.com", "alice@example.com", "Too much", wrappedBody(500)))
	if code != 452 {
		t.Fatalf("DATA = %d %s, want 452", code, msg)
	}
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 0 {
		t.Fatalf("alice has %d messages, want 0", len(msgs))
	}
}

// An unlimited quota (zero) is not read as "no room".
func TestInboundUnlimitedQuotaAccepts(t *testing.T) {
	f := setupInbound(t, nil)
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE users SET quota_bytes = 0 WHERE address = 'alice@example.com'`); err != nil {
		t.Fatalf("setting quota: %v", err)
	}
	c := dialSMTP(t, f.addr)

	if code, _ := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatal("MAIL FROM rejected")
	}
	if code, _ := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatal("RCPT TO rejected")
	}
	code, msg := c.data(inboundMessage("s@example.com", "alice@example.com", "Big", wrappedBody(5000)))
	if code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}
}

// The recipient cap is enforced before the body is accepted.
func TestInboundRejectsTooManyRecipients(t *testing.T) {
	f := setupInbound(t, func(c *config.Config) {
		c.Mail.Inbound.MaxRecipients = 2
	})
	c := dialSMTP(t, f.addr)

	if code, _ := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatal("MAIL FROM rejected")
	}
	if code, _ := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatal("RCPT TO rejected")
	}
	if code, _ := c.cmd("RCPT TO:<support@example.com>"); code != 250 {
		t.Fatal("RCPT TO rejected")
	}
	if code, _ := c.cmd("RCPT TO:<alice@example.com>"); code != 452 {
		t.Errorf("third RCPT TO = %d, want 452", code)
	}
}

// ---------------------------------------------------------------------------
// Spam scoring
// ---------------------------------------------------------------------------

// A message with no SPF pass, no DKIM and no reverse DNS reaches the junk
// threshold and is flagged as such rather than rejected.
func TestInboundRoutesScoredMessageToJunk(t *testing.T) {
	f := setupInbound(t, nil)
	f.dns.txt["spammer.test"] = []string{"v=spf1 -all"}
	delete(f.dns.ptr, "127.0.0.1")

	c := dialSMTP(t, f.addr)
	if code, msg := c.cmd("MAIL FROM:<boss@spammer.test>"); code != 250 {
		t.Fatalf("MAIL FROM = %d %s", code, msg)
	}
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	code, msg := c.data(inboundMessage("Boss <boss@spammer.test>", "alice@example.com", "Cheap watches", "buy now"))
	if code != 250 {
		t.Fatalf("DATA = %d %s", code, msg)
	}

	msgs := f.messages(t, "alice@example.com")
	if len(msgs) != 1 {
		t.Fatalf("alice has %d messages, want 1", len(msgs))
	}
	if len(msgs[0].Flags) != 1 || msgs[0].Flags[0] != "\\Junk" {
		t.Errorf("flags = %v, want [\\Junk]", msgs[0].Flags)
	}
}

// The same message is refused outright once the reject threshold is below the
// score the message reaches, but only after the body has been read: the
// connection-level signals alone stay under it.
func TestInboundRejectsAtRejectThreshold(t *testing.T) {
	f := setupInbound(t, func(c *config.Config) {
		c.Mail.Inbound.Spam.RejectThreshold = 4.5
	})
	f.dns.txt["spammer.test"] = []string{"v=spf1 -all"}
	delete(f.dns.ptr, "127.0.0.1")

	c := dialSMTP(t, f.addr)
	if code, msg := c.cmd("MAIL FROM:<boss@spammer.test>"); code != 250 {
		t.Fatalf("MAIL FROM = %d %s", code, msg)
	}
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	code, msg := c.data(inboundMessage("Boss <boss@spammer.test>", "alice@example.com", "Cheap watches", "buy now"))
	if code != 550 {
		t.Fatalf("DATA = %d %s, want 550", code, msg)
	}
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 0 {
		t.Fatalf("alice has %d messages, want 0", len(msgs))
	}
}

// With spam scoring off, every message is delivered to INBOX.
func TestInboundSpamDisabledSkipsScoring(t *testing.T) {
	f := setupInbound(t, func(c *config.Config) {
		c.Mail.Inbound.Spam.Enabled = false
	})
	f.dns.txt["spammer.test"] = []string{"v=spf1 -all"}
	delete(f.dns.ptr, "127.0.0.1")

	c := dialSMTP(t, f.addr)
	if code, _ := c.cmd("MAIL FROM:<boss@spammer.test>"); code != 250 {
		t.Fatal("MAIL FROM rejected")
	}
	if code, _ := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatal("RCPT TO rejected")
	}
	code, msg := c.data(inboundMessage("Boss <boss@spammer.test>", "alice@example.com", "Cheap watches", "buy now"))
	if code != 250 {
		t.Fatalf("DATA = %d %s", code, msg)
	}
	msgs := f.messages(t, "alice@example.com")
	if len(msgs) != 1 || len(msgs[0].Flags) != 0 {
		t.Fatalf("messages = %+v, want one without flags", msgs)
	}
}

// ---------------------------------------------------------------------------
// Protocol
// ---------------------------------------------------------------------------

// The listener advertises no authentication: inbound mail is identified by its
// source address, and a client may not attempt to log in.
func TestInboundOffersNoAuth(t *testing.T) {
	f := setupInbound(t, nil)

	conn, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()
	tp := textproto.NewConn(conn)
	if _, _, err := tp.ReadResponse(220); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	tp.PrintfLine("EHLO client.example.com")
	_, ext, err := tp.ReadResponse(250)
	if err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if strings.Contains(ext, "AUTH") {
		t.Errorf("EHLO advertised AUTH:\n%s", ext)
	}
	// A client may not authenticate: AUTH is refused, not merely unadvertised.
	tp.PrintfLine("AUTH PLAIN AHUAcA==")
	code, _, err := tp.ReadResponse(0)
	if err != nil {
		t.Fatalf("AUTH: %v", err)
	}
	if code/100 != 5 || code == 235 {
		t.Errorf("AUTH = %d, want a permanent 5xx refusal", code)
	}
}

// A disabled listener does not listen at all.
func TestInboundDisabledDoesNotListen(t *testing.T) {
	dbURL, _ := testdb.URL()
	cfg := config.New()
	cfg.Database.URL = dbURL
	cfg.Listener.SMTPInbound.Addr = ""

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	blobs, err := blobstore.Open(blobstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("opening blobstore: %v", err)
	}
	srv, err := New(*cfg, NewBackend(nil, *cfg, log, blobs), log)
	if err != nil {
		t.Fatalf("creating inbound server: %v", err)
	}
	if srv.Enabled() {
		t.Error("server reported itself enabled with no configured address")
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Errorf("Start on a disabled server = %v, want nil", err)
	}
}

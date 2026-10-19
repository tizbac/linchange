package inbound

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// backend is exposed so a test can wire the optional hooks the
	// application wires in production - scheduling, today.
	backend *Backend
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
		INSERT INTO aliases (address, display_name, description, enabled, updated_by)
		VALUES ('team@example.com', 'Team alias', 'Delivers to Alice', true, 'system')`); err != nil {
		t.Fatalf("creating alias: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO alias_targets (alias_address, target_kind, target_address, workgroup_id, position)
		VALUES ('team@example.com', 'user', 'alice@example.com', NULL, 0)`); err != nil {
		t.Fatalf("creating alias target: %v", err)
	}
	// An alias onto a principal that may not hold mail, to prove the alias does
	// not launder it.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO aliases (address, display_name, description, enabled, updated_by)
		VALUES ('togroup@example.com', 'To group', 'Delivers to nomailbox', true, 'system')`); err != nil {
		t.Fatalf("creating alias: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO alias_targets (alias_address, target_kind, target_address, workgroup_id, position)
		VALUES ('togroup@example.com', 'user', 'nomailbox@example.com', NULL, 0)`); err != nil {
		t.Fatalf("creating alias target: %v", err)
	}
	var workgroupID int64
	if err := db.GetContext(ctx, &workgroupID,
		`INSERT INTO workgroups (name, domain) VALUES ('crew', 'example.com') RETURNING id`); err != nil {
		t.Fatalf("seeding workgroup: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO aliases (address, display_name, description, enabled, updated_by)
		VALUES ('crew@example.com', 'Crew alias', 'Workgroup alias', true, 'system')`); err != nil {
		t.Fatalf("creating alias: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO alias_targets (alias_address, target_kind, workgroup_id, position)
		VALUES ('crew@example.com', 'workgroup', $1, 0)`, workgroupID); err != nil {
		t.Fatalf("creating workgroup alias target: %v", err)
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

	srv, err := New(*cfg, backend, log, nil)
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

	return &inboundFixture{db: db, cfg: cfg, dns: dns, blobs: blobs, addr: ln.Addr().String(), backend: backend}
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

// seedRule stores one rule for owner, encoded the way the webmail API stores
// rules: conditions and actions as JSON arrays the engine reads as a unit.
func (f *inboundFixture) seedRule(t *testing.T, owner, name, conditions, actions string, stop bool) {
	t.Helper()
	if _, err := store.NewMailRules().Create(context.Background(), f.db, store.MailRule{
		Owner:          owner,
		Name:           name,
		Enabled:        true,
		Priority:       100,
		Conditions:     json.RawMessage(conditions),
		Actions:        json.RawMessage(actions),
		StopProcessing: stop,
	}); err != nil {
		t.Fatalf("seeding rule %s: %v", name, err)
	}
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
	return dialSMTPAs(t, addr, nil)
}

// dialSMTPAs connects with an explicit source address, so that a test can
// appear to come from somewhere other than this machine's own loopback
// interface. A nil source dials from loopback, which the server treats as its
// own submission path.
func dialSMTPAs(t *testing.T, addr string, src *net.TCPAddr) *smtpSession {
	t.Helper()
	conn, err := (&net.Dialer{LocalAddr: src}).Dial("tcp", addr)
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

// A rule that matches files the message in the folder it names instead of
// INBOX, and the folder is created on the way.
func TestInboundFilesMessageInRuleFolder(t *testing.T) {
	f := setupInbound(t, nil)
	f.seedRule(t, "alice@example.com", "Newsletters",
		`[{"field":"subject","operator":"contains","value":"newsletter"}]`,
		`[{"type":"move","target":"Newsletters"}]`, true)
	c := dialSMTP(t, f.addr)

	if code, msg := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatalf("MAIL FROM = %d %s", code, msg)
	}
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	if code, msg := c.data(inboundMessage("s@example.com", "alice@example.com", "The Monthly Newsletter", "hi")); code != 250 {
		t.Fatalf("DATA = %d %s", code, msg)
	}

	var name string
	if err := f.db.GetContext(context.Background(), &name,
		`SELECT mb.name FROM messages m JOIN mailboxes mb ON mb.id = m.mailbox_id
		 WHERE mb.owner = 'alice@example.com'`); err != nil {
		t.Fatalf("reading mailbox: %v", err)
	}
	if name != "Newsletters" {
		t.Errorf("mailbox = %q, want Newsletters", name)
	}
}

// A discard rule keeps the message out of the mailbox but the DATA still
// succeeds: a refusal here would only prove the address exists, so the drop is
// silent.
func TestInboundDiscardsMessageByRule(t *testing.T) {
	f := setupInbound(t, nil)
	f.seedRule(t, "alice@example.com", "Spam trap",
		`[{"field":"from","operator":"contains","value":"spambot"}]`,
		`[{"type":"discard"}]`, true)
	c := dialSMTP(t, f.addr)

	if code, msg := c.cmd("MAIL FROM:<spambot@elsewhere.example>"); code != 250 {
		t.Fatalf("MAIL FROM = %d %s", code, msg)
	}
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	if code, msg := c.data(inboundMessage("Spambot <spambot@elsewhere.example>", "alice@example.com", "Deal", "hi")); code != 250 {
		t.Fatalf("DATA = %d %s, want 250 (discarded, not refused)", code, msg)
	}
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 0 {
		t.Fatalf("alice has %d messages, want 0", len(msgs))
	}
}

// A forward rule sends a copy to another address while the message is still
// filed for the owner: a forward is a copy, not a move.
func TestInboundForwardsByRule(t *testing.T) {
	f := setupInbound(t, nil)
	f.seedRule(t, "alice@example.com", "Copy me",
		`[{"field":"subject","operator":"contains","value":"copy"}]`,
		`[{"type":"forward","target":"Copy <copy@elsewhere.example>"}]`, true)
	c := dialSMTP(t, f.addr)

	if code, msg := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatalf("MAIL FROM = %d %s", code, msg)
	}
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	if code, msg := c.data(inboundMessage("s@example.com", "alice@example.com", "Please Copy This", "hi")); code != 250 {
		t.Fatalf("DATA = %d %s", code, msg)
	}

	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 1 {
		t.Fatalf("alice has %d messages, want 1", len(msgs))
	}
	var count int
	if err := f.db.GetContext(context.Background(), &count, `
		SELECT count(*) FROM outbox o
		JOIN outbox_recipients r ON r.outbox_id = o.id
		WHERE o.submitted_by = 'alice@example.com'
		  AND r.address = 'copy@elsewhere.example'`); err != nil {
		t.Fatalf("reading outbox: %v", err)
	}
	if count != 1 {
		t.Errorf("queued forwards = %d, want 1", count)
	}
}

// A rule that marks the message flagged and read files it with both flags, on
// top of whatever the delivery path chose (nothing here, the sender is clean).
func TestInboundAppliesRuleFlags(t *testing.T) {
	f := setupInbound(t, nil)
	f.seedRule(t, "alice@example.com", "Urgent",
		`[{"field":"subject","operator":"contains","value":"urgent"}]`,
		`[{"type":"mark_flagged"},{"type":"mark_read"}]`, true)
	c := dialSMTP(t, f.addr)

	if code, msg := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatalf("MAIL FROM = %d %s", code, msg)
	}
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	if code, msg := c.data(inboundMessage("s@example.com", "alice@example.com", "Urgent: sign this", "hi")); code != 250 {
		t.Fatalf("DATA = %d %s", code, msg)
	}

	msgs := f.messages(t, "alice@example.com")
	if len(msgs) != 1 {
		t.Fatalf("alice has %d messages, want 1", len(msgs))
	}
	got := map[string]bool{}
	for _, fl := range msgs[0].Flags {
		got[fl] = true
	}
	if !got["\\Flagged"] || !got["\\Seen"] {
		t.Errorf("flags = %v, want both \\Flagged and \\Seen", msgs[0].Flags)
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
// remoteSource finds a local address that is not loopback, so that a test can
// appear to arrive from another machine. It returns nil when the host has only
// loopback, and the tests that need it skip rather than lie.
func remoteSource(t *testing.T) *net.TCPAddr {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("listing interfaces: %v", err)
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || !ipnet.IP.IsGlobalUnicast() || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
				continue
			}
			return &net.TCPAddr{IP: ipnet.IP}
		}
	}
	return nil
}

func TestInboundGreylistsFirstContact(t *testing.T) {
	f := setupInbound(t, func(c *config.Config) {
		c.Mail.Inbound.Greylisting = config.Greylisting{
			Enabled:     true,
			Delay:       time.Hour,
			Expiry:      4 * time.Hour,
			RetryWindow: 30 * time.Minute,
		}
	})

	src := remoteSource(t)
	if src == nil {
		t.Skip("this host has only loopback addresses; the greylist exempts loopback")
	}

	c := dialSMTPAs(t, f.addr, src)
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

	c2 := dialSMTPAs(t, f.addr, src)
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

// A sender on this machine's own loopback interface is the server handing its
// local mail back for local delivery, and is not greylisted: the deferral
// window exists to make strangers' MTAs retry, and applying it to the server's
// own submissions would delay a user's own mail by the full delay.
func TestInboundDoesNotGreylistLoopback(t *testing.T) {
	f := setupInbound(t, func(c *config.Config) {
		c.Mail.Inbound.Greylisting = config.Greylisting{
			Enabled:     true,
			Delay:       time.Hour,
			Expiry:      4 * time.Hour,
			RetryWindow: 30 * time.Minute,
		}
	})

	c := dialSMTP(t, f.addr) // dials from 127.0.0.1
	if code, msg := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatalf("MAIL FROM from loopback = %d %s, want 250 without greylist deferral", code, msg)
	}
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	if code, msg := c.data(inboundMessage("s@example.com", "alice@example.com", "Local", "hi")); code != 250 {
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

// writeSelfSignedCert generates a self-signed certificate for name and writes
// the PEM cert and key to two files, returning their paths. It is the fixture
// for any test that needs a real TLS handshake without a CA.
func writeSelfSignedCert(t *testing.T, name string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	certFile = filepath.Join(t.TempDir(), "cert.pem")
	keyFile = filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("writing cert: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}
	return certFile, keyFile
}

// With a certificate configured, the inbound port advertises STARTTLS and the
// upgrade succeeds, after which the session is re-established over TLS.
func TestInboundOffersStartTLS(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t, "mail.example.com")
	f := setupInbound(t, func(cfg *config.Config) {
		cfg.TLS.CertFile = certFile
		cfg.TLS.KeyFile = keyFile
	})

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
	if !strings.Contains(ext, "STARTTLS") {
		t.Fatalf("EHLO did not advertise STARTTLS:\n%s", ext)
	}

	tp.PrintfLine("STARTTLS")
	if _, _, err := tp.ReadResponse(220); err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	// The certificate is self-signed, so the handshake is verified for liveness
	// rather than against a CA.
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "mail.example.com"})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}

	// A second EHLO over the encrypted channel must also succeed, proving the
	// session was reset correctly by the upgrade.
	tp = textproto.NewConn(tlsConn)
	tp.PrintfLine("EHLO client.example.com")
	_, ext, err = tp.ReadResponse(250)
	if err != nil {
		t.Fatalf("EHLO over TLS: %v", err)
	}
	if strings.Contains(ext, "STARTTLS") {
		t.Errorf("EHLO over TLS re-advertised STARTTLS:\n%s", ext)
	}
}

// Without a certificate there is no TLS config, so STARTTLS is neither
// advertised nor accepted.
func TestInboundStartTLSOfferedOnlyWithCertificate(t *testing.T) {
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
	if strings.Contains(ext, "STARTTLS") {
		t.Errorf("EHLO advertised STARTTLS with no certificate:\n%s", ext)
	}
	tp.PrintfLine("STARTTLS")
	code, _, err := tp.ReadResponse(0)
	if err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	if code/100 != 5 {
		t.Errorf("STARTTLS without a certificate = %d, want a 5xx refusal", code)
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
	srv, err := New(*cfg, NewBackend(nil, *cfg, log, blobs), log, nil)
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

// schedulingMessage builds an invitation as it arrives from another server:
// a part a human reads and a calendar part a client answers from. The METHOD
// travels in the part's body - its copy in the Content-Type is the decoration
// clients read first, never the source of truth.
func schedulingMessage(method string) string {
	ics := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//other//EN",
		"METHOD:" + method,
		"BEGIN:VEVENT",
		"UID:abc123@other",
		"SEQUENCE:0",
		"DTSTAMP:20261021T120000Z",
		"DTSTART:20261022T140000Z",
		"DTEND:20261022T150000Z",
		"SUMMARY:Design review",
		"ORGANIZER:mailto:organizer@example.com",
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:alice@example.com",
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")
	return strings.Join([]string{
		"From: Organizer <organizer@example.com>",
		"To: alice@example.com",
		"Subject: Invitation: Design review",
		"Date: Mon, 02 Jan 2006 15:04:05 +0000",
		"MIME-Version: 1.0",
		`Content-Type: multipart/mixed; boundary="calboundary"`,
		"",
		"--calboundary",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"You are invited.",
		"",
		"--calboundary",
		"Content-Type: text/calendar; method=" + method + "; charset=utf-8",
		"Content-Disposition: attachment; filename=invitation.ics",
		"",
		ics,
		"--calboundary--",
		"",
	}, "\r\n")
}

// pastedCalendarMessage carries a calendar file with no METHOD at all: what a
// forward of somebody's appointment looks like. No question is asked by it,
// so nothing should be handed to the calendar layer to answer.
func pastedCalendarMessage() string {
	return strings.Join([]string{
		"From: Forwarder <sender@example.com>",
		"To: alice@example.com",
		"Subject: FW: the thing",
		"Date: Mon, 02 Jan 2006 15:04:05 +0000",
		"MIME-Version: 1.0",
		`Content-Type: multipart/mixed; boundary="calboundary"`,
		"",
		"--calboundary",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"as promised",
		"",
		"--calboundary",
		"Content-Type: text/calendar; charset=utf-8",
		"Content-Disposition: attachment; filename=appointment.ics",
		"",
		"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:pasted@other\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		"--calboundary--",
		"",
	}, "\r\n")
}

// A scheduling message in the inbound mail is handed to the calendar layer
// once, for the recipient whose copy was stored - which is the path an
// external mail client's Accept travels when its user answers an invitation
// from this server. Ordinary mail and a pasted calendar file are not handed
// to anything, and every message is stored either way.
func TestInboundHandsSchedulingMessagesToTheCalendar(t *testing.T) {
	f := setupInbound(t, nil)

	type handed struct {
		owner    string
		document string
	}
	var (
		mu    sync.Mutex
		hands []handed
	)
	f.backend.Scheduling = func(_ context.Context, owner string, document []byte) {
		mu.Lock()
		defer mu.Unlock()
		hands = append(hands, handed{owner: owner, document: string(document)})
	}
	handedTo := func() []handed {
		mu.Lock()
		defer mu.Unlock()
		return append([]handed(nil), hands...)
	}

	send := func(msg string) {
		t.Helper()
		c := dialSMTP(t, f.addr)
		if code, reply := c.cmd("MAIL FROM:<organizer@example.com>"); code != 250 {
			t.Fatalf("MAIL FROM = %d %s", code, reply)
		}
		if code, reply := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
			t.Fatalf("RCPT TO = %d %s", code, reply)
		}
		if code, reply := c.data(msg); code != 250 {
			t.Fatalf("DATA = %d %s", code, reply)
		}
	}

	send(schedulingMessage("REQUEST"))
	got := handedTo()
	if len(got) != 1 {
		t.Fatalf("scheduling calls = %d, want 1: %+v", len(got), got)
	}
	if got[0].owner != "alice@example.com" {
		t.Errorf("owner = %q, want the recipient whose copy was stored", got[0].owner)
	}
	if !strings.Contains(got[0].document, "METHOD:REQUEST") {
		t.Errorf("handed document is not the iTIP document:\n%s", got[0].document)
	}

	// Ordinary mail costs one scan of its parts and no call at all.
	send(inboundMessage("Sender <sender@example.com>", "alice@example.com", "Hello", "plain body"))
	if n := len(handedTo()); n != 1 {
		t.Errorf("scheduling calls after a plain message = %d, want 1", n)
	}

	// A calendar file that asks nothing is not a scheduling message: the
	// method lives in the body, and a forward carries none.
	send(pastedCalendarMessage())
	if n := len(handedTo()); n != 1 {
		t.Errorf("scheduling calls after a pasted calendar file = %d, want 1", n)
	}

	// The hand-off replaced nothing: all three are mail.
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 3 {
		t.Errorf("alice has %d messages, want 3", len(msgs))
	}
}

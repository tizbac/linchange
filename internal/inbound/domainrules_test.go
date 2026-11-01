package inbound

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// setNetworks names the sources a domain will accept mail from.
func (f *inboundFixture) setNetworks(t *testing.T, domain string, networks []string) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE domains SET antispam_networks = $1 WHERE name = $2`, networks, domain); err != nil {
		t.Fatalf("setting the allowed networks of %s: %v", domain, err)
	}
}

// seedHeaderRule adds one header rule to a domain the way the administration
// API would.
func (f *inboundFixture) seedHeaderRule(t *testing.T, domain, header, value, match, action string) {
	t.Helper()
	_, err := store.NewDomainHeaderRules().Create(context.Background(), f.db, domain,
		store.DomainHeaderRuleRequest{Header: header, Value: value, Match: match, Action: action})
	if err != nil {
		t.Fatalf("seeding a header rule on %s: %v", domain, err)
	}
}

func (f *inboundFixture) addMailbox(t *testing.T, address string) {
	t.Helper()
	if err := store.NewUsers().Create(context.Background(), f.db, store.CreateUserRequest{
		Address: address,
		Source:  policy.SourceLocal,
	}); err != nil {
		t.Fatalf("creating mailbox %s: %v", address, err)
	}
}

// messageWithHeader is inboundMessage with one extra header, which is what an
// anti-spam service appends before it delivers mail.
func messageWithHeader(from, to, subject, body, header string) string {
	return strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"Date: Mon, 02 Jan 2006 15:04:05 +0000",
		header,
		"",
		body,
		"",
	}, "\r\n")
}

// A domain that names the sources it accepts refuses every other one at RCPT
// time, before the message body is even sent.
func TestInboundRefusesSourcesADomainDoesNotAllow(t *testing.T) {
	src := remoteSource(t)
	if src == nil {
		t.Skip("this host has only loopback addresses; there is no stranger to refuse")
	}

	f := setupInbound(t, nil)
	f.setNetworks(t, "example.com", []string{"192.0.2.0/24"})

	c := dialSMTPAs(t, f.addr, src)
	if code, msg := c.cmd("MAIL FROM:<sender@example.com>"); code != 250 {
		t.Fatalf("MAIL FROM = %d %s, want 250", code, msg)
	}
	code, msg := c.cmd("RCPT TO:<alice@example.com>")
	if code != 550 {
		t.Fatalf("RCPT TO from an unlisted source = %d %s, want 550", code, msg)
	}

	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 0 {
		t.Fatalf("a refused message still reached alice: %d messages", len(msgs))
	}
}

// The server's own loopback is the submission path handing local mail back for
// local delivery. Restricting a domain to an external filter cannot mean the
// domain's own users lose mail.
func TestInboundAllowsLocalLoopbackThroughANetworkRestriction(t *testing.T) {
	f := setupInbound(t, nil)
	f.setNetworks(t, "example.com", []string{"192.0.2.0/24"})

	c := dialSMTP(t, f.addr)
	if code, msg := c.cmd("MAIL FROM:<colleague@example.com>"); code != 250 {
		t.Fatalf("MAIL FROM = %d %s, want 250", code, msg)
	}
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO from loopback = %d %s, want 250", code, msg)
	}
	if code, msg := c.data(inboundMessage("colleague@example.com", "alice@example.com", "Internal", "hi")); code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 1 {
		t.Fatalf("alice has %d messages, want 1", len(msgs))
	}
}

// A source a domain named as its anti-spam relay is not scored against SPF,
// reverse DNS or greylisting: the relay checked those before it delivered the
// message, and re-checking them would score the relay's own address.
func TestInboundTrustedRelaySkipsTheIntegratedChecks(t *testing.T) {
	src := remoteSource(t)
	if src == nil {
		t.Skip("this host has only loopback addresses; there is no relay to name")
	}

	f := setupInbound(t, func(c *config.Config) {
		c.Mail.Inbound.Greylisting = config.Greylisting{
			Enabled:     true,
			Delay:       time.Hour,
			Expiry:      4 * time.Hour,
			RetryWindow: 30 * time.Minute,
		}
	})
	// The connecting address is the relay, and the sender is a domain with no
	// SPF record and no reverse DNS: on any other source this message would be
	// deferred by greylisting and then junked by the score.
	f.setNetworks(t, "example.com", []string{src.IP.String()})

	c := dialSMTPAs(t, f.addr, src)
	if code, msg := c.cmd("MAIL FROM:<sender@no-spf.test>"); code != 250 {
		t.Fatalf("MAIL FROM = %d %s, want 250", code, msg)
	}
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO from the named relay = %d %s, want 250", code, msg)
	}
	if code, msg := c.data(inboundMessage("sender@no-spf.test", "alice@example.com", "Through the filter", "hi")); code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}

	msgs := f.messages(t, "alice@example.com")
	if len(msgs) != 1 {
		t.Fatalf("alice has %d messages, want 1", len(msgs))
	}
	if len(msgs[0].Flags) != 0 {
		t.Errorf("a trusted relay's message was flagged %v, want no junk flag", msgs[0].Flags)
	}

	var greylistEntries int
	if err := f.db.GetContext(context.Background(), &greylistEntries,
		`SELECT count(*) FROM greylist WHERE sender_ip = $1`, src.IP.String()); err != nil {
		t.Fatalf("reading greylist: %v", err)
	}
	if greylistEntries != 0 {
		t.Errorf("a trusted relay was greylisted %d times, want 0", greylistEntries)
	}
}

// The verdict of an external filter is a header, and the domain names it.
func TestDomainHeaderRuleMarksAMessageJunk(t *testing.T) {
	f := setupInbound(t, nil)
	f.seedHeaderRule(t, "example.com", "X-Spam", "YES", store.HeaderMatchContains, store.HeaderActionJunk)

	c := dialSMTP(t, f.addr)
	c.cmd("MAIL FROM:<sender@example.com>")
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s", code, msg)
	}
	if code, msg := c.data(messageWithHeader("sender@example.com", "alice@example.com", "Buy now", "hi", "X-Spam: yes")); code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}

	msgs := f.messages(t, "alice@example.com")
	if len(msgs) != 1 {
		t.Fatalf("alice has %d messages, want 1", len(msgs))
	}
	if !containsFlag(msgs[0].Flags, `\Junk`) {
		t.Errorf("flags = %v, want the junk flag", msgs[0].Flags)
	}
}

// Matching is insensitive by default, because a filter writes "Yes", "YES" and
// "yes" as though they were the same word.
func TestDomainHeaderRuleMatchesInsensitivelyByDefault(t *testing.T) {
	f := setupInbound(t, nil)
	f.seedHeaderRule(t, "example.com", "X-Spam", "YES", store.HeaderMatchIs, store.HeaderActionJunk)

	c := dialSMTP(t, f.addr)
	c.cmd("MAIL FROM:<sender@example.com>")
	c.cmd("RCPT TO:<alice@example.com>")
	if code, msg := c.data(messageWithHeader("sender@example.com", "alice@example.com", "Hi", "hi", "X-Spam:   yes")); code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}
	msgs := f.messages(t, "alice@example.com")
	if len(msgs) != 1 || !containsFlag(msgs[0].Flags, `\Junk`) {
		t.Fatalf("the rule did not fire: flags = %v", msgs[0].Flags)
	}
}

// A rule that only asks whether a header is there matches without a value.
func TestDomainHeaderRuleCanMatchAHeaderPresence(t *testing.T) {
	f := setupInbound(t, nil)
	f.seedHeaderRule(t, "example.com", "X-Spam-Flag", "", store.HeaderMatchExists, store.HeaderActionDiscard)

	c := dialSMTP(t, f.addr)
	c.cmd("MAIL FROM:<sender@example.com>")
	c.cmd("RCPT TO:<alice@example.com>")
	if code, msg := c.data(messageWithHeader("sender@example.com", "alice@example.com", "Tagged", "hi", "X-Spam-Flag: YES")); code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 0 {
		t.Fatalf("a discarded message reached alice: %d messages", len(msgs))
	}

	// The same message without the header is delivered normally.
	c2 := dialSMTP(t, f.addr)
	c2.cmd("MAIL FROM:<sender@example.com>")
	c2.cmd("RCPT TO:<alice@example.com>")
	if code, msg := c2.data(inboundMessage("sender@example.com", "alice@example.com", "Untagged", "hi")); code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 1 {
		t.Fatalf("alice has %d messages, want the one without the header", len(msgs))
	}
}

// Rejecting answers the sender with a permanent failure, and nothing is filed.
func TestDomainHeaderRuleCanRejectAMessage(t *testing.T) {
	f := setupInbound(t, nil)
	f.seedHeaderRule(t, "example.com", "X-Virus", "detected", store.HeaderMatchContains, store.HeaderActionReject)

	c := dialSMTP(t, f.addr)
	c.cmd("MAIL FROM:<sender@example.com>")
	if code, msg := c.cmd("RCPT TO:<alice@example.com>"); code != 250 {
		t.Fatalf("RCPT TO = %d %s, want 250", code, msg)
	}
	code, msg := c.data(messageWithHeader("sender@example.com", "alice@example.com", "Your invoice", "hi", "X-Virus: detected"))
	if code != 550 {
		t.Fatalf("DATA = %d %s, want 550", code, msg)
	}
	if msgs := f.messages(t, "alice@example.com"); len(msgs) != 0 {
		t.Fatalf("a rejected message was stored: %d messages", len(msgs))
	}
}

// A rule belongs to the domain it was written for, and not to the mailbox of a
// user in another one.
func TestDomainHeaderRulesApplyOnlyToTheirOwnDomain(t *testing.T) {
	f := setupInbound(t, nil)
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO domains (name) VALUES ('other.example')`); err != nil {
		t.Fatalf("seeding the second domain: %v", err)
	}
	f.addMailbox(t, "carol@other.example")
	f.seedHeaderRule(t, "example.com", "X-Spam", "YES", store.HeaderMatchContains, store.HeaderActionJunk)

	c := dialSMTP(t, f.addr)
	c.cmd("MAIL FROM:<sender@example.com>")
	c.cmd("RCPT TO:<carol@other.example>")
	if code, msg := c.data(messageWithHeader("sender@example.com", "carol@other.example", "Shared", "hi", "X-Spam: yes")); code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}
	msgs := f.messages(t, "carol@other.example")
	if len(msgs) != 1 || containsFlag(msgs[0].Flags, `\Junk`) {
		t.Fatalf("example.com's rule changed other.example's mail: flags = %v", msgs[0].Flags)
	}
}

// The header rules run before the integrated scoring, so a message the filter
// marked as junk is junk even when the SPF and DKIM checks are clean.
func TestDomainHeaderRuleJunksAMessageThatScoresClean(t *testing.T) {
	f := setupInbound(t, func(c *config.Config) {
		c.Mail.Inbound.Spam = config.Spam{Enabled: true, JunkThreshold: 5.0, RejectThreshold: 12.0}
	})
	f.seedHeaderRule(t, "example.com", "X-Spam", "YES", store.HeaderMatchContains, store.HeaderActionJunk)

	c := dialSMTP(t, f.addr)
	c.cmd("MAIL FROM:<sender@example.com>") // passes SPF in the fixture, has reverse DNS
	c.cmd("RCPT TO:<alice@example.com>")
	if code, msg := c.data(messageWithHeader("sender@example.com", "alice@example.com", "Newsletter", "hi", "X-Spam: YES")); code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}
	msgs := f.messages(t, "alice@example.com")
	if len(msgs) != 1 || !containsFlag(msgs[0].Flags, `\Junk`) {
		t.Fatalf("flags = %v, want the junk flag from the header rule", msgs[0].Flags)
	}
}

// A header whose name differs only in case is the same header.
func TestDomainHeaderRuleHeaderNameIsMatchedCaseInsensitively(t *testing.T) {
	f := setupInbound(t, nil)
	f.seedHeaderRule(t, "example.com", "x-spam", "yes", store.HeaderMatchContains, store.HeaderActionJunk)

	c := dialSMTP(t, f.addr)
	c.cmd("MAIL FROM:<sender@example.com>")
	c.cmd("RCPT TO:<alice@example.com>")
	if code, msg := c.data(messageWithHeader("sender@example.com", "alice@example.com", "Case", "hi", "X-SPAM: Yes")); code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}
	msgs := f.messages(t, "alice@example.com")
	if len(msgs) != 1 || !containsFlag(msgs[0].Flags, `\Junk`) {
		t.Fatalf("flags = %v, want the junk flag", msgs[0].Flags)
	}
}

// A rule can be turned off without deleting it, and an off rule does nothing.
func TestDisabledDomainHeaderRuleDoesNothing(t *testing.T) {
	f := setupInbound(t, nil)
	if _, err := store.NewDomainHeaderRules().Create(context.Background(), f.db, "example.com",
		store.DomainHeaderRuleRequest{
			Header:  "X-Spam",
			Value:   "YES",
			Action:  store.HeaderActionJunk,
			Enabled: boolPtrInbound(false),
		}); err != nil {
		t.Fatalf("seeding a disabled rule: %v", err)
	}

	c := dialSMTP(t, f.addr)
	c.cmd("MAIL FROM:<sender@example.com>")
	c.cmd("RCPT TO:<alice@example.com>")
	if code, msg := c.data(messageWithHeader("sender@example.com", "alice@example.com", "Ham", "hi", "X-Spam: YES")); code != 250 {
		t.Fatalf("DATA = %d %s, want 250", code, msg)
	}
	msgs := f.messages(t, "alice@example.com")
	if len(msgs) != 1 || containsFlag(msgs[0].Flags, `\Junk`) {
		t.Fatalf("flags = %v, want a plain message", msgs[0].Flags)
	}
}

func boolPtrInbound(b bool) *bool { return &b }

// The header matcher is tested apart from the listener, because the shapes a
// filter writes are the part most likely to surprise an operator.
func TestHeaderRuleMatches(t *testing.T) {
	rule := func(header, value, match, action string, caseSensitive bool) store.DomainHeaderRule {
		return store.DomainHeaderRule{
			Header:        header,
			Value:         value,
			Match:         match,
			Action:        action,
			CaseSensitive: caseSensitive,
		}
	}
	headers := map[string][]string{
		"x-spam":        {"Yes, score=4.1"},
		"x-spam-status": {"no"},
		"received":      {"from relay-a", "from relay-b"},
	}

	cases := []struct {
		rule   store.DomainHeaderRule
		want   bool
		reason string
	}{
		{rule("X-Spam", "YES", store.HeaderMatchContains, store.HeaderActionJunk, false), true, "insensitive contains"},
		{rule("X-Spam", "yes", store.HeaderMatchIs, store.HeaderActionJunk, false), false, "the header carries a score too"},
		{rule("X-Spam", "yes", store.HeaderMatchStartsWith, store.HeaderActionJunk, false), true, "starts with"},
		{rule("X-Spam-Status", "NO", store.HeaderMatchIs, store.HeaderActionDiscard, false), true, "is, insensitive"},
		{rule("X-Spam-Status", "no", store.HeaderMatchIs, store.HeaderActionDiscard, true), true, "is, sensitive and exact"},
		{rule("X-Spam-Status", "No", store.HeaderMatchIs, store.HeaderActionDiscard, true), false, "sensitive case differs"},
		{rule("X-Spam-Flag", "", store.HeaderMatchExists, store.HeaderActionReject, false), false, "the header is absent"},
		{rule("Received", "relay-b", store.HeaderMatchEndsWith, store.HeaderActionJunk, false), true, "any of the repeated headers"},
		{rule("X-Spam", "no", store.HeaderMatchContains, store.HeaderActionJunk, false), false, "not present"},
	}
	for _, c := range cases {
		if got := headerRuleMatches(c.rule, headers); got != c.want {
			t.Errorf("%s: %v matches %q = %v, want %v", c.reason, c.rule.Header, c.rule.Value, got, c.want)
		}
	}
}

// The loopback exemption and the relay list are read from the same table the
// administration API writes, so a test that writes the column directly sees the
// same policy the listener applies.
func TestDomainPolicyReadsItsNetworks(t *testing.T) {
	f := setupInbound(t, nil)
	f.setNetworks(t, "example.com", []string{"192.0.2.0/24", "203.0.113.5"})

	policy, err := f.backend.domainPolicy(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("reading the policy: %v", err)
	}
	if !policy.restricted() {
		t.Fatal("the domain reported itself unrestricted")
	}
	for _, in := range []string{"192.0.2.7", "203.0.113.5"} {
		if !policy.trusts(netipMustParse(in)) {
			t.Errorf("%s is not recognized as an allowed source", in)
		}
	}
	for _, out := range []string{"192.0.3.1", "203.0.113.6"} {
		if policy.trusts(netipMustParse(out)) {
			t.Errorf("%s should not be allowed", out)
		}
	}
}

func netipMustParse(s string) netip.Addr { return netip.MustParseAddr(s) }

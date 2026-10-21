// Package delivery manages the persistent Postgres outbox, worker-driven retry
// scheduling, and Delivery Status Notification (DSN) bounce generation.
//
// The outbox is the source of truth for every outbound message: the delivery
// worker claims queued rows, delivers them over SMTP, updates per-recipient
// states, and defers or finalises messages when the last attempt is exhausted.
package delivery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/dkim"
	"github.com/linchange/linchange/internal/store"
)

// ---------------------------------------------------------------------------
// BlobStore is the interface for retrieving the raw message blob.
// ---------------------------------------------------------------------------

// BlobStore provides access to stored message blobs. The delivery worker
// needs the raw RFC 5322 bytes to send over SMTP.
type BlobStore interface {
	// Get retrieves a blob by its SHA-256 digest.
	Get(d blobstore.Digest) ([]byte, error)
}

// ---------------------------------------------------------------------------
// Worker pool configuration and execution.
// ---------------------------------------------------------------------------

// Config holds all configuration needed by the delivery worker pool.
type Config struct {
	Concurrency          int           // Number of concurrent delivery workers.
	PerDomainConcurrency int           // Max deliveries per destination domain.
	Retry                BackoffConfig // Exponential backoff parameters.
	DSN                  DSNConfig     // DSN bounce configuration.
	SMTP                 SMTPConfig    // SMTP connection parameters.
	BlobStore            BlobStore     // Where to fetch message blobs.
	Outbox               *OutboxRepository
	// DKIMKeys is an optional key manager for signing outbound messages.
	DKIMKeys *dkim.Keys
	// DKIMSelectorPrefix is the prefix for DKIM selectors (e.g. "mail").
	DKIMSelectorPrefix string
	// Loopback, when set, is how this server reaches its own inbound SMTP
	// listener. Recipients at domains hosted here are handed back through it
	// rather than to the relay, so a message to a colleague on the same
	// server is delivered instead of being sent to a host that was never
	// configured. Nil keeps the old relay-only behaviour.
	Loopback *SMTPConfig
	// IsLocalDomain reports whether a domain is hosted on this server. Nil
	// means the split is never made (see Loopback).
	IsLocalDomain func(ctx context.Context, domain string) (bool, error)
}

// Pool manages the delivery worker pool.
type Pool struct {
	cfg     Config
	logger  *slog.Logger
	db      *sqlx.DB
	quit    chan struct{}
	done    chan struct{}
	active  atomic.Int64
	stopped atomic.Bool

	// lookupMX and lookupHost are the DNS the exchanger search asks. They are
	// fields rather than calls made in place so that a test can answer with
	// records it wrote itself: which exchanger is tried first, and what happens
	// when the first cannot be reached, are the parts worth testing, and both
	// would otherwise be left to whatever resolver the machine happens to use.
	lookupMX   func(ctx context.Context, name string) ([]*net.MX, error)
	lookupHost func(ctx context.Context, name string) ([]string, error)
}

// NewPool creates a new delivery worker pool.
func NewPool(cfg Config, logger *slog.Logger, db *sqlx.DB) *Pool {
	return &Pool{
		cfg:        cfg,
		logger:     logger,
		db:         db,
		quit:       make(chan struct{}),
		done:       make(chan struct{}),
		lookupMX:   net.DefaultResolver.LookupMX,
		lookupHost: net.DefaultResolver.LookupHost,
	}
}

// FromConfig builds a Pool from the application configuration.
func FromConfig(appCfg *config.Config, logger *slog.Logger, db *sqlx.DB) *Pool {
	cfg := Config{
		Concurrency:          appCfg.Mail.Outbound.Concurrency,
		PerDomainConcurrency: appCfg.Mail.Outbound.PerDomainConcurrency,
		Retry: BackoffConfig{
			Base:     appCfg.Mail.Outbound.Retry.BackoffBase,
			Max:      appCfg.Mail.Outbound.Retry.BackoffMax,
			Lifetime: appCfg.Mail.Outbound.Retry.MaxLifetime,
		},
		DSN: DSNConfig{
			Enabled:     appCfg.Mail.Outbound.DSN.Enabled,
			FromAddress: appCfg.Mail.Outbound.DSN.FromAddress,
			Delay:       appCfg.Mail.Outbound.DSN.Delay,
		},
		SMTP: SMTPConfig{
			Host:           appCfg.Mail.Outbound.SMTPHost,
			Port:           appCfg.Mail.Outbound.SMTPPort,
			TLS:            appCfg.Mail.Outbound.SMTPStartTLS,
			ConnectTimeout: appCfg.Mail.Outbound.ConnectTimeout,
			CommandTimeout: appCfg.Mail.Outbound.CommandTimeout,
			Username:       appCfg.Mail.Outbound.SMTPUsername,
			Password:       appCfg.Mail.Outbound.SMTPPassword,
		},
		Outbox:    NewOutboxRepository(db),
		BlobStore: nil, // Must be set by the caller.
		DKIMKeys:  nil, // Optional: set by the caller if DKIM signing is desired.
	}

	// Local recipients are delivered through this server's own inbound
	// listener, unauthenticated and without TLS, because that is what a
	// public inbound MTA speaks. Without it the relay handles every
	// recipient, and a single-server deployment that never configured a
	// relay delivers nothing to its own domains.
	if addr := appCfg.Listener.SMTPInbound.Addr; addr != "" {
		if host, port, err := net.SplitHostPort(addr); err == nil {
			if port != "" {
				if pnum, err := strconv.Atoi(port); err == nil {
					if host == "" {
						host = "127.0.0.1"
					}
					cfg.Loopback = &SMTPConfig{
						Host:           host,
						Port:           pnum,
						ConnectTimeout: appCfg.Mail.Outbound.ConnectTimeout,
						CommandTimeout: appCfg.Mail.Outbound.CommandTimeout,
					}
				}
			}
		}
	}
	domains := store.NewDomains()
	cfg.IsLocalDomain = func(ctx context.Context, domain string) (bool, error) {
		if _, err := domains.Get(ctx, db, domain); err != nil {
			if store.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}

	return NewPool(cfg, logger, db)
}

// GetConfig returns a copy of the pool's current configuration.
func (p *Pool) GetConfig() Config { return p.cfg }

// ApplyConfig replaces the pool's configuration with cfg. It is safe to call
// before Start so that BlobStore can be set after FromConfig.
func (p *Pool) ApplyConfig(cfg Config) { p.cfg = cfg }

// Start launches the worker pool. Workers run until Stop is called or ctx is
// cancelled. Start returns after all workers have exited.
func (p *Pool) Start(ctx context.Context) {
	go func() {
		defer close(p.done)

		// Launch worker goroutines.
		var wg sync.WaitGroup
		for i := 0; i < p.cfg.Concurrency; i++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				p.worker(ctx, workerID)
			}(i)
		}

		// Wait for the quit signal or context cancellation.
		select {
		case <-ctx.Done():
		case <-p.quit:
		}

		// Give in-flight deliveries a grace period to complete.
		_ = ctx // Workers check ctx in their delivery loop.

		wg.Wait()
		p.stopped.Store(true)
	}()
}

// Stop signals the worker pool to finish in-flight work and exit.
func (p *Pool) Stop() {
	if p.stopped.Swap(true) {
		return // Already stopped.
	}
	close(p.quit)
	<-p.done
}

// IsActive returns true if the pool has been started and not yet stopped.
func (p *Pool) IsActive() bool {
	return !p.stopped.Load()
}

// ---------------------------------------------------------------------------
// Single worker loop.
// ---------------------------------------------------------------------------

func (p *Pool) worker(ctx context.Context, workerID int) {
	for {
		// Claim due messages. The claim marks them 'sending' in the same
		// statement that takes them, so a message is handed to one worker
		// however many are polling: reading the rows and marking them one
		// call apart left every worker claiming the same batch and the losers
		// logging a delivery error for mail that was going out fine.
		messages, err := p.cfg.Outbox.ClaimForSending(ctx, p.cfg.PerDomainConcurrency)
		if err != nil {
			p.logger.Error("outbox claim", "worker", workerID, "error", err)
			// Wait before retrying the claim.
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			case <-p.quit:
				return
			}
			continue
		}

		if len(messages) == 0 {
			// No work available. Sleep briefly and try again.
			select {
			case <-time.After(1 * time.Second):
			case <-ctx.Done():
				return
			case <-p.quit:
				return
			}
			continue
		}

		for i := range messages {
			// Stopping is not a delivery failure: whatever this worker
			// claimed and has not reached goes back to the queue, because
			// nothing retries a message left 'sending'.
			select {
			case <-ctx.Done():
				p.releaseClaims(workerID, messages[i:])
				return
			case <-p.quit:
				p.releaseClaims(workerID, messages[i:])
				return
			default:
			}

			p.active.Add(1)
			if err := p.deliver(ctx, workerID, messages[i]); err != nil {
				p.logger.Error("delivery failed",
					"worker", workerID,
					"message_id", messages[i].ID,
					"error", err)
			}
			p.active.Add(-1)
		}
	}
}

// releaseClaims hands claimed messages back to the queue unattempted, which
// is what the pool owes them when it is told to stop. The context the pool is
// running under is already done by then, and the release is precisely the
// thing that still has to happen, so it is given its own.
func (p *Pool) releaseClaims(workerID int, messages []OutboxMessage) {
	if len(messages) == 0 {
		return
	}
	ids := make([]int64, 0, len(messages))
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.cfg.Outbox.Release(ctx, ids); err != nil {
		p.logger.Error("releasing unattempted messages",
			"worker", workerID,
			"count", len(ids),
			"error", err)
		return
	}
	p.logger.Info("released unattempted messages back to the queue",
		"worker", workerID,
		"count", len(ids))
}

// ---------------------------------------------------------------------------
// Single message delivery.
// ---------------------------------------------------------------------------

func (p *Pool) deliver(ctx context.Context, workerID int, msg OutboxMessage) error {
	// Fetch the message blob.
	var d blobstore.Digest
	copy(d[:], msg.BlobSHA256)
	blob, err := p.cfg.BlobStore.Get(d)
	if err != nil {
		if err == ErrBlobNotFound {
			p.logger.Warn("blob not found for outbox message",
				"message_id", msg.ID,
				"blob_sha256", fmt.Sprintf("%x", msg.BlobSHA256))
			dsnDelay := time.Duration(0)
			if p.cfg.DSN.Enabled {
				dsnDelay = p.cfg.DSN.Delay
			}
			return p.cfg.Outbox.MarkFailed(ctx, nil, msg.ID, msg.Attempts, "blob not found", dsnDelay)
		}
		return fmt.Errorf("get blob: %w", err)
	}

	// DKIM signing: extract the domain from envelope_from and sign if a key exists.
	var signingDomain string
	if idx := strings.LastIndex(msg.EnvelopeFrom, "@"); idx >= 0 {
		signingDomain = msg.EnvelopeFrom[idx+1:]
	}

	if p.cfg.DKIMKeys != nil && signingDomain != "" {
		if key, pubKey, err := p.cfg.DKIMKeys.GetOrCreate(signingDomain); err == nil {
			// Check if we already have a cached key.
			signedBlob, err := dkim.Sign(blob, &dkim.SignOptions{
				Domain:     signingDomain,
				Selector:   p.cfg.DKIMSelectorPrefix,
				PrivateKey: key,
			})
			if err == nil {
				blob = signedBlob
				p.logger.Info("DKIM signed",
					"message_id", msg.ID,
					"domain", signingDomain,
					"selector", p.cfg.DKIMSelectorPrefix,
					"public_key", pubKey[:60]+"...")
			} else {
				p.logger.Warn("DKIM signing failed, sending unsigned",
					"message_id", msg.ID,
					"domain", signingDomain,
					"error", err)
			}
		} else {
			p.logger.Info("no DKIM key for domain, sending unsigned",
				"message_id", msg.ID,
				"domain", signingDomain,
				"error", err)
		}
	}

	// The message was marked 'sending' by the claim that handed it here, so
	// there is nothing to take ownership of on the way in: the rows are this
	// worker's already, and no other one was offered them.

	// Only recipients that have not settled are sent. A message whose local
	// half has already arrived is retried for the rest, and sending the
	// first half again would deliver it twice.
	var pending []OutboxRecipient
	for _, r := range msg.Recipients {
		if r.State == RecipientPending || r.State == RecipientDeferred {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return p.settle(ctx, msg, nil)
	}

	// Recipients at domains hosted here are handed back through this server's
	// own inbound listener; everything else goes to the configured relay. The
	// relay exists for other servers -- pointing it at a server that never
	// configured one delivers nothing, which is what happened to every message
	// addressed to a colleague on the same box.
	var local, remote []OutboxRecipient
	if p.cfg.Loopback != nil && p.cfg.IsLocalDomain != nil {
		for _, r := range pending {
			if p.hostsLocally(ctx, r.Address) {
				local = append(local, r)
			} else {
				remote = append(remote, r)
			}
		}
	} else {
		remote = pending
	}

	sendCtx, cancel := context.WithTimeout(ctx, p.cfg.SMTP.CommandTimeout)
	defer cancel()

	addresses := func(rs []OutboxRecipient) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.Address
		}
		return out
	}
	positionOf := make(map[string]int, len(pending))
	for _, r := range pending {
		positionOf[r.Address] = r.Position
	}
	// mark settles recipients by address, which is how a sending path reports
	// its outcome. The reason stored here is the text the bounce shows, so a
	// specific failure (a 550 from the destination) beats a generic one.
	mark := func(addrs []string, state RecipientState, reasons map[string]string, fallback string) {
		for _, a := range addrs {
			pos, ok := positionOf[a]
			if !ok {
				continue
			}
			reason := fallback
			if r := reasons[a]; r != "" {
				reason = r
			}
			if err := p.cfg.Outbox.UpdateRecipientState(ctx, nil, msg.ID, pos, state, reason); err != nil {
				p.logger.Error("mark recipient", "message_id", msg.ID,
					"recipient", a, "state", state, "error", err)
			}
		}
	}

	var lastErr error
	if len(local) > 0 {
		// The loopback speaks what a public inbound MTA speaks: plain SMTP,
		// no credentials, one transaction per recipient.
		out := (&smtpClient{cfg: *p.cfg.Loopback}).SendEach(
			sendCtx, msg.EnvelopeFrom, addresses(local), blob)
		mark(out.Delivered, RecipientDelivered, nil, "")
		mark(out.Permanent, RecipientFailed, out.Reasons, "rejected by local delivery")
		if len(out.Transient) > 0 {
			p.logger.Warn("local delivery left recipients pending",
				"message_id", msg.ID, "reasons", out.Reasons)
			if lastErr == nil {
				lastErr = fmt.Errorf("local delivery: %v", out.Reasons)
			}
		}
		// Transient local failures stay unresolved for the next claim; the
		// settle below decides what the message does meanwhile.
	}
	if len(remote) > 0 {
		if p.cfg.SMTP.Host != "" {
			// One relay handles every remote recipient: the operator said
			// where remote mail should go.
			client := NewSMTPClient(p.cfg.SMTP)
			sendErr := client.Send(sendCtx, msg.EnvelopeFrom, addresses(remote), blob)
			switch {
			case sendErr == nil:
				mark(addresses(remote), RecipientDelivered, nil, "")
			case IsPermanent(sendErr):
				mark(addresses(remote), RecipientFailed, nil, sendErr.Error())
				lastErr = sendErr
			default:
				// Stays unresolved; settle defers the message for another try.
				lastErr = sendErr
			}
		} else {
			// No relay is configured, so each destination domain is dialed at
			// its own mail exchanger, which is what the SMTP configuration
			// documents for an unset smart host. A domain that publishes no
			// mail exchanger and resolves to no address at all is written off
			// permanently; one whose DNS is merely flaky stays unresolved.
			byDomain := make(map[string][]OutboxRecipient)
			for _, r := range remote {
				_, domain, _ := strings.Cut(r.Address, "@")
				byDomain[strings.ToLower(domain)] = append(byDomain[strings.ToLower(domain)], r)
			}
			domains := make([]string, 0, len(byDomain))
			for d := range byDomain {
				domains = append(domains, d)
			}
			sort.Strings(domains)
			for _, domain := range domains {
				group := byDomain[domain]
				exchangers, err := p.mailExchangers(ctx, domain)
				if err != nil {
					if IsPermanent(err) {
						// The destination exists nowhere: nothing will change
						// that before the retry lifetime ends, so the recipient
						// is written off now and the sender gets a bounce
						// naming it.
						p.logger.Warn("no mail exchanger for domain; failing permanently",
							"message_id", msg.ID, "domain", domain, "error", err)
						mark(addresses(group), RecipientFailed, nil, err.Error())
						if lastErr == nil {
							lastErr = err
						}
						continue
					}
					p.logger.Warn("no mail exchanger for domain; deferring",
						"message_id", msg.ID, "domain", domain, "error", err)
					if lastErr == nil {
						lastErr = err
					}
					continue
				}
				// Walk the exchangers the domain published, stopping at the
				// first that takes the connection. One address that cannot be
				// reached — a route this host has no way out on, an address
				// family that is advertised but not usable — must not defer
				// the mail that the next published exchanger would take.
				out := p.deliverToExchangers(sendCtx, msg.ID, domain, exchangers,
					msg.EnvelopeFrom, addresses(group), blob)
				mark(out.Delivered, RecipientDelivered, nil, "")
				mark(out.Permanent, RecipientFailed, out.Reasons,
					"the destination mail exchanger rejected the message")
				if len(out.Transient) > 0 {
					// The reasons are logged and carried in the error rather
					// than swallowed: "could not be reached" on its own sent
					// whoever was told that to guess at the address family, the
					// port or the peer, and guessing at what the exchanger
					// actually said is not a question they could answer.
					p.logger.Warn("mail exchanger deferred recipients",
						"message_id", msg.ID, "domain", domain, "reasons", out.Reasons)
					if lastErr == nil {
						lastErr = fmt.Errorf("%s: the mail exchanger could not be reached: %s",
							domain, transientReason(out))
					}
				}
			}
		}
	}

	return p.settle(ctx, msg, lastErr)
}

// deliverToExchangers sends one message to one domain's recipients, trying the
// exchangers that domain published in order until one takes the connection.
//
// An exchanger that cannot be dialled at all is the network's answer about
// that address — a route, a filter, an address family this host cannot use —
// and not about the domain, so the next published address gets its turn. An
// exchanger that answers and then declines is the destination speaking: that
// answer stands, and dialling its neighbours would only collect the same
// refusal elsewhere.
//
// Nothing is settled here. The caller marks the recipients from what comes
// back, because an attempt that never connected delivered to nobody, so
// re-marking from an earlier, failed attempt cannot lose a delivery.
func (p *Pool) deliverToExchangers(ctx context.Context, messageID int64, domain string,
	exchangers []SMTPConfig, from string, to []string, blob []byte) RecipientOutcome {
	if len(exchangers) == 0 {
		// Unreachable rather than empty: a caller that passed nothing to try
		// must not be answered with an outcome that looks like success, since
		// a message with no transient recipient is settled as sent.
		out := RecipientOutcome{Transient: append([]string(nil), to...), Reasons: map[string]string{}}
		for _, address := range to {
			out.Reasons[address] = fmt.Sprintf("no mail exchanger is published for %s", domain)
		}
		return out
	}

	var out RecipientOutcome
	for _, exchanger := range exchangers {
		out = (&smtpClient{cfg: exchanger}).SendEach(ctx, from, to, blob)
		if out.Connected || len(out.Delivered) > 0 {
			return out
		}
		p.logger.Warn("mail exchanger could not be reached; trying the next",
			"message_id", messageID, "domain", domain,
			"host", exchanger.Host, "reasons", out.Reasons)
	}
	return out
}

// mailExchangers answers where to deliver mail for a domain when no smart host
// is configured: every mail exchanger it publishes, lowest preference first,
// falling back to the domain's own address as the RFC allows. This is what the
// SMTP configuration documents for an empty smtp_host, and without it a
// single-server deployment can only ever talk to itself.
//
// All of them are returned rather than only the first. Taking the
// lowest-preference record alone means one exchanger that cannot be reached —
// a route that disappeared, an address family this host cannot use, a filter
// that dropped port 25 to one address — defers the whole domain, although the
// domain published four more that answer. The caller walks the list and stops
// at the first that takes the connection.
//
// A domain that publishes no mail exchanger and resolves to no address at all
// is reported as a permanent failure: it exists nowhere, and retrying it for a
// week only delays the bounce that tells the sender so. A lookup that failed
// for any other reason is transient, since DNS may come back.
func (p *Pool) mailExchangers(ctx context.Context, domain string) ([]SMTPConfig, error) {
	records, err := p.lookupMX(ctx, domain)
	if err != nil {
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
			return nil, fmt.Errorf("resolving MX for %s: %w", domain, err)
		}
		// No MX published: many domains still accept mail at their address.
		if _, err := p.lookupHost(ctx, domain); err != nil {
			var hostErr *net.DNSError
			if errors.As(err, &hostErr) && hostErr.IsNotFound {
				return nil, &ErrPermanent{
					Err: fmt.Errorf("no mail exchanger or address is published for %s", domain),
				}
			}
			return nil, fmt.Errorf("resolving %s: %w", domain, err)
		}
		return []SMTPConfig{p.directTo(domain)}, nil
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Pref < records[j].Pref })

	out := make([]SMTPConfig, 0, len(records))
	seen := make(map[string]bool, len(records))
	nullMX := false
	for _, mx := range records {
		host := strings.TrimSuffix(mx.Host, ".")
		if host == "" {
			// RFC 7505: a mail exchanger of "." means the domain says it
			// accepts no mail at all. Dialing its web address would be a guess.
			nullMX = true
			continue
		}
		if seen[strings.ToLower(host)] {
			continue
		}
		seen[strings.ToLower(host)] = true
		out = append(out, p.directTo(host))
	}
	if len(out) == 0 {
		if nullMX {
			return nil, &ErrPermanent{
				Err: fmt.Errorf("%s publishes that it accepts no mail", domain),
			}
		}
		// Every record was empty of anything dialable, which is the null-MX
		// case wearing a different shape.
		return []SMTPConfig{p.directTo(domain)}, nil
	}
	return out, nil
}

// directTo builds the SMTP configuration for talking straight to a mail
// exchanger: port 25, no credentials -- relays authenticate, destinations do
// not.
func (p *Pool) directTo(host string) SMTPConfig {
	return SMTPConfig{
		Host:           host,
		Port:           25,
		ConnectTimeout: p.cfg.SMTP.ConnectTimeout,
		CommandTimeout: p.cfg.SMTP.CommandTimeout,
	}
}

// hostsLocally reports whether a recipient's domain is hosted on this server.
// A lookup that fails is answered "no" and logged: the mail still leaves by
// the relay rather than being stranded over a database hiccup.
func (p *Pool) hostsLocally(ctx context.Context, address string) bool {
	if p.cfg.IsLocalDomain == nil {
		return false
	}
	_, domain, ok := strings.Cut(address, "@")
	if !ok {
		return false
	}
	hosted, err := p.cfg.IsLocalDomain(ctx, strings.ToLower(domain))
	if err != nil {
		p.logger.Warn("hosted-domain lookup failed; relaying instead",
			"address", address, "error", err)
		return false
	}
	return hosted
}

// settle closes a delivery round by reading what became of every recipient:
// with some still unresolved the message is deferred to try only those again,
// with none unresolved it is sent, and when a recipient refused the mail it is
// failed so the bounce lists exactly the recipients that did not get it.
func (p *Pool) settle(ctx context.Context, msg OutboxMessage, lastErr error) error {
	recipients, err := p.cfg.Outbox.loadRecipients(ctx, nil, msg.ID)
	if err != nil {
		return fmt.Errorf("reload recipients: %w", err)
	}
	pending, failed := 0, 0
	for _, r := range recipients {
		switch r.State {
		case RecipientPending, RecipientDeferred:
			pending++
		case RecipientFailed:
			failed++
		}
	}

	switch {
	case pending > 0:
		text := "some recipients are still pending"
		if lastErr != nil {
			text = lastErr.Error()
		}
		nextDelay, ok := p.cfg.Retry.CalculateBackoff(msg.Attempts, msg.CreatedAt)
		if !ok {
			p.logger.Info("message lifetime exceeded, marking failed",
				"message_id", msg.ID,
				"attempts", msg.Attempts)
			return p.fail(ctx, msg, text)
		}
		if err := p.cfg.Outbox.MarkDeferred(ctx, nil, msg.ID, msg.Attempts+1,
			text, time.Now().Add(nextDelay)); err != nil {
			p.logger.Error("mark deferred", "message_id", msg.ID, "error", err)
		}
		return nil

	case failed > 0:
		text := "a recipient refused the message"
		if lastErr != nil {
			text = lastErr.Error()
		}
		return p.fail(ctx, msg, text)

	default:
		if err := p.cfg.Outbox.MarkSent(ctx, nil, msg.ID); err != nil {
			p.logger.Error("mark sent", "message_id", msg.ID, "error", err)
		}
		return nil
	}
}

// fail marks the message failed: the bounce that follows lists the recipients
// whose delivery failed, and only those.
func (p *Pool) fail(ctx context.Context, msg OutboxMessage, text string) error {
	dsnDelay := time.Duration(0)
	if p.cfg.DSN.Enabled {
		dsnDelay = p.cfg.DSN.Delay
	}
	return p.cfg.Outbox.MarkFailed(ctx, nil, msg.ID, msg.Attempts, text, dsnDelay)
}

// ---------------------------------------------------------------------------
// DSN processing.
// ---------------------------------------------------------------------------

// DSNProcessor processes messages that are due for DSN bounce generation.
// It loads failed messages, generates a bounce, inserts it into the outbox,
// and marks the original message as bounced.
type DSNProcessor struct {
	pool *Pool
}

// NewDSNProcessor creates a new DSN processor.
func NewDSNProcessor(pool *Pool) *DSNProcessor {
	return &DSNProcessor{pool: pool}
}

// Process runs the DSN processing loop. It processes messages in batches and
// returns when ctx is cancelled.
func (p *DSNProcessor) Process(ctx context.Context, blobStore BlobStore, submissionFn func(submittedBy, envelopeFrom string, blob []byte, recipients []OutboxRecipient) (int64, error)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		messages, err := p.pool.cfg.Outbox.ClaimDSNMessages(ctx, nil, 10)
		if err != nil {
			p.pool.logger.Error("claim DSN messages", "error", err)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return err
			}
			continue
		}

		if len(messages) == 0 {
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return nil
			}
			continue
		}

		for _, msg := range messages {
			// Find failed recipients.
			var failedRecipients []OutboxRecipient
			for _, r := range msg.Recipients {
				if r.State == RecipientFailed || r.State == RecipientDeferred {
					failedRecipients = append(failedRecipients, r)
				}
			}

			if len(failedRecipients) == 0 {
				if err := p.pool.cfg.Outbox.MarkBounced(ctx, nil, msg.ID); err != nil {
					p.pool.logger.Error("mark bounced", "message_id", msg.ID, "error", err)
				}
				continue
			}

			// The original message is read so the report can name it. A blob
			// that cannot be read is not a reason to withhold the bounce: the
			// report is generated without the excerpt.
			original := originalMessage(blobStore, msg)
			if original != nil && original.Header.Get("Auto-Submitted") != "" {
				// A delivery report is itself mail, and it can fail. Generating
				// a report for a report would loop forever, so automatically
				// generated messages are never bounced.
				p.pool.logger.Info("not bouncing an automatically generated message",
					"message_id", msg.ID)
				if err := p.pool.cfg.Outbox.MarkBounced(ctx, nil, msg.ID); err != nil {
					p.pool.logger.Error("mark bounced", "message_id", msg.ID, "error", err)
				}
				continue
			}

			// Generate DSN bounce.
			bounceData, err := GenerateDSN(p.pool.cfg.DSN, msg, failedRecipients, original)
			if err != nil {
				p.pool.logger.Error("generate DSN", "message_id", msg.ID, "error", err)
				continue
			}

			if bounceData == nil {
				if err := p.pool.cfg.Outbox.MarkBounced(ctx, nil, msg.ID); err != nil {
					p.pool.logger.Error("mark bounced", "message_id", msg.ID, "error", err)
				}
				continue
			}

			// Insert the DSN into the outbox for delivery. The report leaves
			// from the postmaster address rather than the original sender's
			// envelope, which a remote server would read as the sender writing
			// to itself.
			envelopeFrom := msg.EnvelopeFrom
			if p.pool.cfg.DSN.FromAddress != "" {
				envelopeFrom = p.pool.cfg.DSN.FromAddress
			}
			_, err = submissionFn(msg.SubmittedBy, envelopeFrom, bounceData, []OutboxRecipient{
				{
					Position: 0,
					Address:  msg.SubmittedBy,
					Kind:     To,
				},
			})
			if err != nil {
				p.pool.logger.Error("enqueue DSN", "message_id", msg.ID, "error", err)
				continue
			}

			if err := p.pool.cfg.Outbox.MarkBounced(ctx, nil, msg.ID); err != nil {
				p.pool.logger.Error("mark bounced", "message_id", msg.ID, "error", err)
			} else {
				p.pool.logger.Info("DSN bounced", "message_id", msg.ID)
			}
		}
	}
}

// originalMessage reads the headers of the message that failed, so the bounce
// can quote the subject it was about. Nothing here is worth losing a bounce
// over: an unreadable blob simply yields nil.
func originalMessage(blobStore BlobStore, msg OutboxMessage) *mail.Message {
	if blobStore == nil || len(msg.BlobSHA256) == 0 {
		return nil
	}
	var d blobstore.Digest
	copy(d[:], msg.BlobSHA256)
	blob, err := blobStore.Get(d)
	if err != nil {
		return nil
	}
	m, err := mail.ReadMessage(bytes.NewReader(blob))
	if err != nil {
		return nil
	}
	return m
}

// ---------------------------------------------------------------------------
// Error type helpers.
// ---------------------------------------------------------------------------

// IsTransient checks if err is a transient delivery error.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var _ *ErrTransient
	for err != nil {
		if _, ok := err.(*ErrTransient); ok {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
		} else {
			break
		}
	}
	return false
}

// IsPermanent checks if err is a permanent delivery error.
func IsPermanent(err error) bool {
	if err == nil {
		return false
	}
	for err != nil {
		if _, ok := err.(*ErrPermanent); ok {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
		} else {
			break
		}
	}
	return false
}

// transientReason is the one line a deferred delivery carries into the log and
// into the queue's last-error column: what the exchanger actually said rather
// than that something did not work.
//
// The connection-level key wins when it is set, because one of those covers
// every recipient of the attempt; otherwise the first recipient with a reason
// stands for the rest. Every address in an outcome that never connected was
// refused by the same dial, so they are one reason repeated.
func transientReason(out RecipientOutcome) string {
	if reason := out.Reasons["*"]; reason != "" {
		return reason
	}
	for _, address := range out.Transient {
		if reason := out.Reasons[address]; reason != "" {
			return reason
		}
	}
	return "the exchanger gave no reason"
}

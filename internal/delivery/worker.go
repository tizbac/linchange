// Package delivery manages the persistent Postgres outbox, worker-driven retry
// scheduling, and Delivery Status Notification (DSN) bounce generation.
//
// The outbox is the source of truth for every outbound message: the delivery
// worker claims queued rows, delivers them over SMTP, updates per-recipient
// states, and defers or finalises messages when the last attempt is exhausted.
package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/dkim"
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
}

// NewPool creates a new delivery worker pool.
func NewPool(cfg Config, logger *slog.Logger, db *sqlx.DB) *Pool {
	return &Pool{
		cfg:    cfg,
		logger: logger,
		db:     db,
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
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
		// Claim messages from the outbox.
		messages, err := p.cfg.Outbox.ClaimMessages(ctx, nil, p.cfg.PerDomainConcurrency)
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

		p.active.Add(int64(len(messages)))

		for _, msg := range messages {
			select {
			case <-ctx.Done():
				p.active.Add(-int64(len(messages)))
				return
			case <-p.quit:
				p.active.Add(-int64(len(messages)))
				return
			default:
			}

			if err := p.deliver(ctx, workerID, msg); err != nil {
				p.logger.Error("delivery failed",
					"worker", workerID,
					"message_id", msg.ID,
					"error", err)
			}

			p.active.Add(-1)
		}
	}
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

	// Mark message as "sending" to claim it.
	if err := p.cfg.Outbox.MarkSending(ctx, nil, msg.ID); err != nil {
		return fmt.Errorf("mark sending: %w", err)
	}

	// Extract recipient addresses.
	recipients := make([]string, len(msg.Recipients))
	for i, r := range msg.Recipients {
		recipients[i] = r.Address
	}

	// Send via SMTP.
	client := NewSMTPClient(p.cfg.SMTP)
	sendCtx, cancel := context.WithTimeout(ctx, p.cfg.SMTP.CommandTimeout)
	defer cancel()

	err = client.Send(sendCtx, msg.EnvelopeFrom, recipients, blob)
	if err == nil {
		// Success: mark everything as delivered.
		if err := p.cfg.Outbox.MarkSent(ctx, nil, msg.ID); err != nil {
			p.logger.Error("mark sent", "message_id", msg.ID, "error", err)
		}
		return nil
	}

	// Determine the failure outcome.
	switch {
	case IsTransient(err):
		// Transient failure: schedule retry with backoff.
		nextDelay, ok := p.cfg.Retry.CalculateBackoff(msg.Attempts, msg.CreatedAt)
		if !ok {
			p.logger.Info("message lifetime exceeded, marking failed",
				"message_id", msg.ID,
				"attempts", msg.Attempts)
			dsnDelay := time.Duration(0)
		if p.cfg.DSN.Enabled {
			dsnDelay = p.cfg.DSN.Delay
		}
		return p.cfg.Outbox.MarkFailed(ctx, nil, msg.ID, msg.Attempts, err.Error(), dsnDelay)
		}

		nextAttemptAt := time.Now().Add(nextDelay)
		if err := p.cfg.Outbox.MarkDeferred(ctx, nil, msg.ID, msg.Attempts+1, err.Error(), nextAttemptAt); err != nil {
			p.logger.Error("mark deferred", "message_id", msg.ID, "error", err)
		}
		return nil

	case IsPermanent(err):
		// Permanent failure: all recipients rejected.
		dsnDelay := time.Duration(0)
		if p.cfg.DSN.Enabled {
			dsnDelay = p.cfg.DSN.Delay
		}
		return p.cfg.Outbox.MarkFailed(ctx, nil, msg.ID, msg.Attempts, err.Error(), dsnDelay)

	default:
		// Unknown error type: treat as transient.
		nextDelay, ok := p.cfg.Retry.CalculateBackoff(msg.Attempts, msg.CreatedAt)
		if !ok {
			dsnDelay := time.Duration(0)
		if p.cfg.DSN.Enabled {
			dsnDelay = p.cfg.DSN.Delay
		}
		return p.cfg.Outbox.MarkFailed(ctx, nil, msg.ID, msg.Attempts, err.Error(), dsnDelay)
		}
		nextAttemptAt := time.Now().Add(nextDelay)
		return p.cfg.Outbox.MarkDeferred(ctx, nil, msg.ID, msg.Attempts+1, err.Error(), nextAttemptAt)
	}
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
			case <-time.After(10 * time.Second):
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

			// Generate DSN bounce.
			bounceData, err := GenerateDSN(p.pool.cfg.DSN, msg, failedRecipients)
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

			// Insert the DSN into the outbox for delivery.
			_, err = submissionFn(msg.SubmittedBy, msg.EnvelopeFrom, bounceData, []OutboxRecipient{
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

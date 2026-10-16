// Package delivery manages the persistent Postgres outbox, worker-driven retry
// scheduling, and Delivery Status Notification (DSN) bounce generation.
//
// The outbox is the source of truth for every outbound message: the delivery
// worker claims queued rows, delivers them over SMTP, updates per-recipient
// states, and defers or finalises messages when the last attempt is exhausted.
package delivery

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
)

// ---------------------------------------------------------------------------
// Outbox message representation.
// ---------------------------------------------------------------------------

// OutboxState mirrors the PostgreSQL outbox_state enum.
type OutboxState string

const (
	// Queued means the message has been submitted but not yet attempted.
	OutboxQueued OutboxState = "queued"
	// Sending means the delivery worker has started trying this message.
	OutboxSending OutboxState = "sending"
	// Sent means every recipient has been delivered successfully.
	OutboxSent OutboxState = "sent"
	// Deferred means at least one recipient could not be reached but the
	// message should be retried.
	OutboxDeferred OutboxState = "deferred"
	// Failed means all attempts are exhausted and the message will not be
	// retried. The next step is to generate a DSN bounce if configured.
	OutboxFailed OutboxState = "failed"
	// Bounced means the DSN bounce was generated.
	OutboxBounced OutboxState = "bounced"
)

// RecipientState mirrors the PostgreSQL recipient_state enum.
type RecipientState string

const (
	// RecipientPending means the recipient has not yet been attempted.
	RecipientPending RecipientState = "pending"
	// RecipientDelivered means the recipient received the message.
	RecipientDelivered RecipientState = "delivered"
	// RecipientDeferred means this recipient could not be reached but will be
	// retried on the next claim.
	RecipientDeferred RecipientState = "deferred"
	// RecipientFailed means all attempts for this recipient are exhausted.
	RecipientFailed RecipientState = "failed"
)

// OutboxRecipientKind mirrors the PostgreSQL outbox_recipient_kind enum.
type OutboxRecipientKind string

const (
	To  OutboxRecipientKind = "to"
	CC  OutboxRecipientKind = "cc"
	BCC OutboxRecipientKind = "bcc"
)

// OutboxMessage is one row of the outbox table plus its recipients.
type OutboxMessage struct {
	ID            int64             `db:"id"`
	SubmittedBy   string            `db:"submitted_by"`
	EnvelopeFrom  string            `db:"envelope_from"`
	BlobSHA256    []byte            `db:"blob_sha256"`
	Size          int64             `db:"size"`
	State         OutboxState       `db:"state"`
	Attempts      int               `db:"attempts"`
	NextAttemptAt time.Time         `db:"next_attempt_at"`
	LastError     string            `db:"last_error"`
	FailedAt      sql.NullTime      `db:"failed_at"`
	DSNAt         sql.NullTime      `db:"dsn_at"`
	SentAt        sql.NullTime      `db:"sent_at"`
	CreatedAt     time.Time         `db:"created_at"`
	Recipients    []OutboxRecipient `db:"-"`
}

// OutboxRecipient is one row of the outbox_recipients table.
type OutboxRecipient struct {
	Position    int                 `db:"position"`
	Address     string              `db:"address"`
	Kind        OutboxRecipientKind `db:"kind"`
	State       RecipientState      `db:"state"`
	Attempts    int                 `db:"attempts"`
	LastError   string              `db:"last_error"`
	DeliveredAt sql.NullTime        `db:"delivered_at"`
}

// ---------------------------------------------------------------------------
// Error types.
// ---------------------------------------------------------------------------

// ErrTransient is returned when a delivery failure is likely to succeed on
// retry. Callers defer and retry the message.
type ErrTransient struct{ Err error }

func (e *ErrTransient) Error() string { return "transient: " + e.Err.Error() }
func (e *ErrTransient) Unwrap() error { return e.Err }

// ErrPermanent is returned when a recipient address is definitively invalid.
// The delivery worker marks it failed immediately without retry.
type ErrPermanent struct{ Err error }

func (e *ErrPermanent) Error() string { return "permanent: " + e.Err.Error() }
func (e *ErrPermanent) Unwrap() error { return e.Err }

// ErrSMTP is returned when the remote server rejects the message or a command.
type ErrSMTP struct {
	Code int
	Text string
}

func (e *ErrSMTP) Error() string {
	return fmt.Sprintf("smtp %d: %s", e.Code, e.Text)
}

// ---------------------------------------------------------------------------
// SMTP client interface for decoupling delivery from the wire.
// ---------------------------------------------------------------------------

// SMTPClient sends a single message to one or more recipients. The caller owns
// the message bytes (already RFC 5322 serialised). The client opens and closes
// the network connection as needed.
type SMTPClient interface {
	// Send delivers msg to the given recipients. It must deliver to all
	// recipients or mark none; partial delivery within one call is handled
	// by the worker.
	//
	// If Send returns ErrTransient the worker should retry with backoff.
	// ErrPermanent means the address is bad and should not be retried.
	Send(ctx context.Context, from string, to []string, data []byte) error
}

// ---------------------------------------------------------------------------
// Retry backoff logic.
// ---------------------------------------------------------------------------

// BackoffConfig holds the parameters for exponential backoff.
type BackoffConfig struct {
	Base     time.Duration
	Max      time.Duration
	Lifetime time.Duration
}

// CalculateBackoff returns how long to wait before the next attempt given the
// current attempt count and the message's creation time.
//
// The delay doubles each attempt starting from Base, capped at Max. If the
// message is older than Lifetime it is considered expired.
func (c BackoffConfig) CalculateBackoff(attempts int, createdAt time.Time) (time.Duration, bool) {
	if c.Base <= 0 {
		return 0, true
	}

	// Apply exponential backoff: Base * 2^attempts.
	delay := c.Base
	for i := 0; i < attempts; i++ {
		delay *= 2
		if delay > c.Max {
			delay = c.Max
			break
		}
	}

	// Add jitter between 0% and 25% of delay to avoid thundering herd.
	jitter := time.Duration(rand.Float64() * float64(delay) * 0.25)
	delay += jitter

	// Check lifetime if configured.
	if c.Lifetime > 0 && time.Since(createdAt) > c.Lifetime {
		return 0, false // expired
	}

	return delay, true
}

// ---------------------------------------------------------------------------
// SMTP implementation using go-smtp and go-sasl.
// ---------------------------------------------------------------------------

// SMTPConfig holds the connection parameters for the SMTP client.
type SMTPConfig struct {
	Host               string
	Port               int
	TLS                bool
	ConnectTimeout     time.Duration
	CommandTimeout     time.Duration
	Username           string
	Password           string
	InsecureSkipVerify bool
}

// smtpClient wraps an SMTP client and provides the Send method.
type smtpClient struct {
	cfg SMTPConfig
}

// NewSMTPClient creates a new SMTP client from the configuration.
func NewSMTPClient(cfg SMTPConfig) SMTPClient {
	return &smtpClient{cfg: cfg}
}

// Send delivers a message using go-smtp.
func (c *smtpClient) Send(ctx context.Context, from string, to []string, data []byte) error {
	dialer := &net.Dialer{
		Timeout: c.cfg.ConnectTimeout,
	}

	addr := net.JoinHostPort(c.cfg.Host, fmt.Sprintf("%d", c.cfg.Port))

	var s *smtp.Client
	var err error

	tlsConfig := &tls.Config{
		ServerName:         c.cfg.Host,
		InsecureSkipVerify: c.cfg.InsecureSkipVerify,
	}

	if c.cfg.TLS {
		// Direct TLS (port 465 typically).
		conn, dialErr := dialer.DialContext(ctx, "tcp", addr)
		if dialErr != nil {
			return &ErrTransient{Err: fmt.Errorf("dial TLS: %w", dialErr)}
		}
		s, err = smtp.NewClientStartTLS(conn, tlsConfig)
	} else {
		// Plain connection with STARTTLS attempt.
		conn, dialErr := dialer.DialContext(ctx, "tcp", addr)
		if dialErr != nil {
			return &ErrTransient{Err: fmt.Errorf("dial: %w", dialErr)}
		}
		s = smtp.NewClient(conn)
		if err := s.Hello("linchange"); err != nil {
			s.Close()
			return &ErrTransient{Err: fmt.Errorf("EHLO: %w", err)}
		}
		// The go-smtp library does not expose a public StartTLS method,
		// so we only support STARTTLS when the whole connection is TLS.
	}

	if err != nil {
		return &ErrTransient{Err: fmt.Errorf("SMTP connect: %w", err)}
	}
	defer s.Close()

	s.CommandTimeout = c.cfg.CommandTimeout

	if c.cfg.Username != "" && c.cfg.Password != "" {
		auth := sasl.NewPlainClient("", c.cfg.Username, c.cfg.Password)
		if err := s.Auth(auth); err != nil {
			return &ErrTransient{Err: fmt.Errorf("AUTH: %w", err)}
		}
	}

	if err := s.Mail(from, nil); err != nil {
		if smtpIsPermanent(err) {
			return &ErrPermanent{Err: fmt.Errorf("MAIL FROM: %w", err)}
		}
		return &ErrTransient{Err: fmt.Errorf("MAIL FROM: %w", err)}
	}

	// Attempt each recipient, collecting results.
	var (
		delivered []string
		permanent []string
		transient []string
	)

	for _, toAddr := range to {
		if err := s.Rcpt(toAddr, nil); err != nil {
			if smtpIsPermanent(err) {
				permanent = append(permanent, toAddr)
			} else {
				transient = append(transient, toAddr)
			}
			continue
		}
		delivered = append(delivered, toAddr)
	}

	// If all recipients permanently rejected, fail immediately.
	if len(permanent) > 0 && len(delivered) == 0 && len(transient) == 0 {
		return &ErrPermanent{Err: fmt.Errorf("all recipients rejected: %v", permanent)}
	}

	// Send DATA for recipients that accepted.
	if len(delivered) > 0 {
		if err := sendMailData(s, data); err != nil {
			s.Close()
			return &ErrTransient{Err: fmt.Errorf("DATA: %w", err)}
		}
	}

	// Try to send DATA for transient recipients (re-Rcpt + DATA).
	for _, addr := range transient {
		if err := s.Rcpt(addr, nil); err != nil {
			permanent = append(permanent, addr)
			continue
		}
		if err := sendMailData(s, data); err != nil {
			permanent = append(permanent, addr)
			continue
		}
		delivered = append(delivered, addr)
	}

	s.Close()

	if len(permanent) > 0 && len(delivered) == 0 {
		return &ErrPermanent{Err: fmt.Errorf("no recipients accepted: %v", permanent)}
	}
	if len(permanent) > 0 {
		return &ErrTransient{Err: fmt.Errorf("some recipients failed: %v", permanent)}
	}
	return nil
}

// RecipientOutcome is what became of each recipient of one SendEach call:
// the three groups are disjoint, and everybody is in exactly one of them.
type RecipientOutcome struct {
	Delivered []string
	Permanent []string
	Transient []string
	// Reasons explains the failures above, keyed by recipient address (or by
	// "*" when a connection-level failure hit every recipient). Without it a
	// deferred message says only that something did not work, which is not
	// enough to find out what.
	Reasons map[string]string
}

func (o *RecipientOutcome) failTransient(rcpts []string, reason string) {
	if o.Reasons == nil {
		o.Reasons = map[string]string{}
	}
	for _, r := range rcpts {
		o.Reasons[r] = reason
	}
	o.Transient = append(o.Transient, rcpts...)
}

func (o *RecipientOutcome) failPermanent(rcpt, reason string) {
	if o.Reasons == nil {
		o.Reasons = map[string]string{}
	}
	o.Reasons[rcpt] = reason
	o.Permanent = append(o.Permanent, rcpt)
}

// SendEach delivers the message to each recipient in its own SMTP transaction
// and reports what became of that recipient, so a caller can settle one
// recipient's fate without guessing at the batch's. A failure to connect at
// all is transient for everybody: nothing was accepted and a retry is worth
// making.
func (c *smtpClient) SendEach(ctx context.Context, from string, to []string, data []byte) RecipientOutcome {
	var out RecipientOutcome
	if len(to) == 0 {
		return out
	}

	dialer := &net.Dialer{Timeout: c.cfg.ConnectTimeout}
	addr := net.JoinHostPort(c.cfg.Host, fmt.Sprintf("%d", c.cfg.Port))
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		out.failTransient(to, fmt.Sprintf("connect %s: %v", addr, err))
		return out
	}

	var s *smtp.Client
	if c.cfg.TLS {
		s, err = smtp.NewClientStartTLS(conn, &tls.Config{
			ServerName:         c.cfg.Host,
			InsecureSkipVerify: c.cfg.InsecureSkipVerify,
		})
	} else {
		s = smtp.NewClient(conn)
		if err = s.Hello("linchange"); err != nil {
			s.Close()
			out.failTransient(to, fmt.Sprintf("EHLO: %v", err))
			return out
		}
	}
	if err != nil {
		out.failTransient(to, fmt.Sprintf("STARTTLS: %v", err))
		return out
	}
	defer s.Close()
	s.CommandTimeout = c.cfg.CommandTimeout

	if c.cfg.Username != "" && c.cfg.Password != "" {
		auth := sasl.NewPlainClient("", c.cfg.Username, c.cfg.Password)
		if err := s.Auth(auth); err != nil {
			out.failTransient(to, fmt.Sprintf("AUTH: %v", err))
			return out
		}
	}

	for _, rcpt := range to {
		// Every recipient is its own transaction: MAIL FROM, RCPT, DATA. An
		// SMTP transaction covers one delivery, and reusing the envelope
		// across deliveries would make one recipient's failure the next
		// recipient's mail.
		if err := s.Mail(from, nil); err != nil {
			out.failTransient([]string{rcpt}, fmt.Sprintf("MAIL FROM: %v", err))
			continue
		}
		if err := s.Rcpt(rcpt, nil); err != nil {
			reason := fmt.Sprintf("RCPT TO: %v", err)
			if smtpIsPermanent(err) {
				out.failPermanent(rcpt, reason)
			} else {
				out.failTransient([]string{rcpt}, reason)
			}
			continue
		}
		if err := sendMailData(s, data); err != nil {
			out.failTransient([]string{rcpt}, fmt.Sprintf("DATA: %v", err))
			continue
		}
		out.Delivered = append(out.Delivered, rcpt)
	}
	return out
}

// smtpIsPermanent checks if an SMTP error indicates a permanent failure
// (5xx class).
func smtpIsPermanent(err error) bool {
	if err == nil {
		return false
	}
	var smtpErr *smtp.SMTPError
	if errors.As(err, &smtpErr) {
		return smtpErr.Code >= 500
	}
	// Connection/network errors are transient.
	var netErr net.Error
	return !errors.As(err, &netErr)
}

// sendMailData writes the message data to an SMTP connection.
func sendMailData(s *smtp.Client, data []byte) error {
	w, err := s.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// ---------------------------------------------------------------------------
// DSN (Delivery Status Notification) bounce generation.
// ---------------------------------------------------------------------------

// DSNConfig holds the parameters for generating DSN bounces.
type DSNConfig struct {
	Enabled     bool
	FromAddress string
	Delay       time.Duration
}

// GenerateDSN creates a DSN bounce message for a failed outbox message.
// The message is sent back to the original sender (SubmittedBy) explaining
// which recipients failed.
func GenerateDSN(cfg DSNConfig, msg OutboxMessage, failedRecipients []OutboxRecipient) ([]byte, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	if cfg.FromAddress == "" {
		return nil, errors.New("DSN: from_address not configured")
	}

	sender, err := mail.ParseAddress(cfg.FromAddress)
	if err != nil {
		return nil, fmt.Errorf("DSN: parse from_address %q: %w", cfg.FromAddress, err)
	}

	// Build the list of failed recipients for the DSN body.
	var failedList strings.Builder
	for i, r := range failedRecipients {
		if i > 0 {
			failedList.WriteString("\n")
		}
		fmt.Fprintf(&failedList, "  - %s: %s", r.Address, r.LastError)
	}

	// Construct a simple RFC 5322 message.
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", sender.String())
	fmt.Fprintf(&b, "To: %s\r\n", msg.SubmittedBy)
	fmt.Fprintf(&b, "Subject: Delivery Status Notification (Failure)\r\n")
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(&b, "Content-Transfer-Encoding: 7bit\r\n")
	fmt.Fprintf(&b, "\r\n")
	fmt.Fprintf(&b, "Your message was not delivered to the following recipients:\r\n")
	fmt.Fprintf(&b, "\r\n")
	fmt.Fprintf(&b, "Envelope from: %s\r\n", msg.EnvelopeFrom)
	fmt.Fprintf(&b, "\r\n")
	fmt.Fprintf(&b, "Failed recipients:\r\n")
	if failedList.Len() > 0 {
		failedList.WriteString("\r\n")
	}
	fmt.Fprint(&b, failedList.String())
	fmt.Fprintf(&b, "\r\n")
	fmt.Fprintf(&b, "Total attempts: %d\r\n", msg.Attempts)
	if msg.LastError != "" {
		fmt.Fprintf(&b, "Last error: %s\r\n", msg.LastError)
	}
	fmt.Fprintf(&b, "\r\n")

	return []byte(b.String()), nil
}

// ---------------------------------------------------------------------------
// Blob helpers.
// ---------------------------------------------------------------------------

// BlobSHA256Hex returns the hex encoding of a SHA-256 digest.
func BlobSHA256Hex(digest []byte) string {
	return base64.StdEncoding.EncodeToString(digest)
}

// ErrBlobNotFound is returned when a blob is not found in storage.
var ErrBlobNotFound = errors.New("delivery: blob not found")

// ErrBlobCorrupt is returned when a blob's SHA-256 does not match.
var ErrBlobCorrupt = errors.New("delivery: blob SHA-256 mismatch")

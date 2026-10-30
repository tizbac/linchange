package imapimport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
)

type Service struct {
	db       *sqlx.DB
	blobs    *blobstore.Store
	log      *slog.Logger
	key      []byte
	quotas   *store.Quotas
	mailbox  *imapd.MailboxRepo
	messages *imapd.MessageRepo
	users    *store.Users
	// allowPrivateHosts permits loopback and RFC 1918 import sources. Off by
	// default; see SetAllowPrivateHosts.
	allowPrivateHosts bool

	quit    chan struct{}
	done    chan struct{}
	stopped atomic.Bool
	wg      sync.WaitGroup

	mu      sync.Mutex
	cancels map[int64]context.CancelFunc
}

func New(db *sqlx.DB, blobs *blobstore.Store, log *slog.Logger, encryptionKey string) *Service {
	return &Service{
		db:       db,
		blobs:    blobs,
		log:      log,
		key:      deriveKey(encryptionKey),
		quotas:   store.NewQuotas(),
		mailbox:  imapd.NewMailboxRepo(),
		messages: imapd.NewMessageRepo(),
		users:    store.NewUsers(),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
		cancels:  make(map[int64]context.CancelFunc),
	}
}

// SetAllowPrivateHosts permits imports from loopback and RFC 1918 addresses.
// It is set once during construction from configuration and defaults to false.
func (s *Service) SetAllowPrivateHosts(allow bool) { s.allowPrivateHosts = allow }

func (s *Service) Start(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		close(s.done)

		if err := s.requeueOrphans(ctx); err != nil {
			s.log.Error("imapimport: startup requeue", "err", err)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-s.quit:
				return
			default:
			}

			job, err := s.claimNext(ctx)
			if err != nil {
				s.log.Error("imapimport: claim", "err", err)
				if !sleepOrStop(ctx, s.quit, 2*time.Second) {
					return
				}
				continue
			}
			if job == nil {
				if !sleepOrStop(ctx, s.quit, 2*time.Second) {
					return
				}
				continue
			}
			s.runJob(ctx, *job)
		}
	}()
}

func (s *Service) Stop() {
	if s.stopped.Swap(true) {
		return
	}
	close(s.quit)
	s.mu.Lock()
	for _, cancel := range s.cancels {
		cancel()
	}
	s.mu.Unlock()
	<-s.done
	s.wg.Wait()
}

func sleepOrStop(ctx context.Context, quit chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-quit:
		return false
	case <-timer.C:
		return true
	}
}

const (
	maxSyncRetries = 5
	retryBaseDelay = 5 * time.Second
	retryMaxDelay  = 2 * time.Minute
)

// runJob executes a claimed job. When the IMAP connection drops mid-sync it
// retries with backoff: every imported message is recorded in
// imap_import_items and deduplicated by blob_sha256, so restarting the sync
// skips what already landed instead of importing it twice.
func (s *Service) runJob(parent context.Context, job Job) {
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.cancels[job.ID] = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.cancels, job.ID)
		s.mu.Unlock()
	}()

	var err error
	for attempt := 0; ; attempt++ {
		err = s.sync(ctx, job)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if isPermanentSyncError(err) {
			break
		}
		if attempt >= maxSyncRetries {
			err = fmt.Errorf("imapimport: giving up after %d attempts: %w", attempt+1, err)
			break
		}
		delay := backoffDelay(attempt)
		s.log.Warn("imapimport: sync interrupted, retrying",
			"job", job.ID, "attempt", attempt+1, "delay", delay, "err", err)
		if !sleepOrStop(ctx, s.quit, delay) {
			err = ctx.Err()
			break
		}
	}

	if err == nil {
		s.finishJob(ctx, job.ID, StatusCompleted, nil)
		s.log.Info("imapimport: job finished", "job", job.ID, "owner", job.Owner)
		return
	}
	if ctx.Err() != nil {
		current, getErr := s.getJob(context.Background(), job.ID)
		if getErr == nil && current.CancelRequested {
			s.finishJob(context.Background(), job.ID, StatusCancelled, nil)
			s.log.Info("imapimport: job cancelled", "job", job.ID)
			return
		}
		s.requeueJob(context.Background(), job.ID)
		s.log.Info("imapimport: job requeued", "job", job.ID)
		return
	}
	s.finishJob(context.Background(), job.ID, StatusFailed, err)
	s.log.Error("imapimport: job failed", "job", job.ID, "err", err)
}

// isPermanentSyncError reports errors that retrying cannot fix: the quota is
// full or the stored password cannot be decrypted. Everything else (network
// drops, IMAP protocol errors, transient DB issues) is worth another attempt.
func isPermanentSyncError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "quota exceeded") ||
		strings.Contains(msg, "decrypting password")
}

func backoffDelay(attempt int) time.Duration {
	d := retryBaseDelay << attempt
	if d > retryMaxDelay {
		d = retryMaxDelay
	}
	return d
}

type RemoteParams struct {
	Host             string
	Port             int
	TLS              string
	Username         string
	Password         string
	AcceptSelfSigned bool
}

func (s *Service) ListRemoteFolders(ctx context.Context, params RemoteParams) ([]string, error) {
	if strings.TrimSpace(params.Host) == "" {
		return nil, fmt.Errorf("%w: remote host is required", ErrInvalid)
	}
	if err := validateRemoteHost(ctx, params.Host, s.allowPrivateHosts); err != nil {
		return nil, err
	}
	client, err := s.dial(ctx, params)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	if err := client.Login(params.Username, params.Password).Wait(); err != nil {
		return nil, fmt.Errorf("imapimport: remote login: %w", err)
	}

	cmd := client.List("", "*", nil)
	entries, err := cmd.Collect()
	if err != nil {
		return nil, fmt.Errorf("imapimport: listing remote folders: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if hasAttr(entry.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		names = append(names, entry.Mailbox)
	}
	return names, nil
}

func (s *Service) dial(ctx context.Context, params RemoteParams) (*imapclient.Client, error) {
	addr := net.JoinHostPort(params.Host, fmt.Sprintf("%d", params.Port))
	tlsCfg := &tls.Config{
		InsecureSkipVerify: params.AcceptSelfSigned,
		ServerName:         params.Host,
	}

	var (
		client *imapclient.Client
		err    error
	)
	switch params.TLS {
	case "plain":
		client, err = imapclient.DialInsecure(addr, &imapclient.Options{TLSConfig: tlsCfg})
	case "starttls":
		client, err = imapclient.DialStartTLS(addr, &imapclient.Options{TLSConfig: tlsCfg})
	default:
		client, err = imapclient.DialTLS(addr, &imapclient.Options{TLSConfig: tlsCfg})
	}
	if err != nil {
		return nil, fmt.Errorf("imapimport: dialing %s: %w", addr, err)
	}
	if err := client.WaitGreeting(); err != nil {
		client.Close()
		return nil, fmt.Errorf("imapimport: greeting: %w", err)
	}
	return client, nil
}

func hasAttr(attrs []imap.MailboxAttr, want imap.MailboxAttr) bool {
	for _, attr := range attrs {
		if attr == want {
			return true
		}
	}
	return false
}

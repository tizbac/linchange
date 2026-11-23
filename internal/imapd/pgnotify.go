package imapd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

const (
	// notifyChannel is the PostgreSQL channel the message triggers publish on.
	notifyChannel = "message_change"

	// notifyRetryMin and notifyRetryMax bound the reconnect backoff. A database
	// restart must not turn the listener into a reconnect loop.
	notifyRetryMin = time.Second
	notifyRetryMax = 30 * time.Second
)

// changeNotifier fans PostgreSQL LISTEN/NOTIFY out to the IMAP sessions that
// are idling.
//
// It owns exactly one pooled connection, and only while at least one session is
// subscribed. Notifications are a broadcast, so giving every IDLE a connection
// of its own multiplies the database's connection count by the number of
// clients and turns idle mailboxes into a busy poll. The previous
// implementation did that: it queried SELECT 1 in a tight loop on a pooled
// connection, and its goroutine outlived IDLE, so each past IDLE permanently
// took a connection out of the pool until none were left.
type changeNotifier struct {
	db     *sqlx.DB
	logger *slog.Logger

	mu      sync.Mutex
	subs    map[*changeSubscriber]struct{}
	cancel  context.CancelFunc
	running bool
	closed  bool
}

type changeSubscriber struct {
	mailboxID int64
	ch        chan struct{}
}

func newChangeNotifier(db *sqlx.DB, logger *slog.Logger) *changeNotifier {
	return &changeNotifier{
		db:     db,
		logger: logger,
		subs:   make(map[*changeSubscriber]struct{}),
	}
}

// subscribe registers interest in changes to mailboxID. A mailboxID of zero
// means the caller has no mailbox selected and wants every notification.
//
// The returned function must be called when the caller stops caring; it is safe
// to call more than once.
func (n *changeNotifier) subscribe(mailboxID int64) (<-chan struct{}, func()) {
	sub := &changeSubscriber{mailboxID: mailboxID, ch: make(chan struct{}, 1)}

	n.mu.Lock()
	n.subs[sub] = struct{}{}
	if !n.closed && !n.running {
		n.startLocked()
	}
	n.mu.Unlock()

	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			n.mu.Lock()
			delete(n.subs, sub)
			if len(n.subs) == 0 && n.cancel != nil {
				n.cancel()
			}
			n.mu.Unlock()
		})
	}
}

// Close stops the listener and future subscriptions.
func (n *changeNotifier) Close() {
	n.mu.Lock()
	n.closed = true
	if n.cancel != nil {
		n.cancel()
		n.cancel = nil
	}
	n.mu.Unlock()
}

// startLocked starts the listener goroutine. The caller must hold n.mu.
func (n *changeNotifier) startLocked() {
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.running = true
	go n.run(ctx)
}

func (n *changeNotifier) run(ctx context.Context) {
	defer func() {
		n.mu.Lock()
		n.running = false
		n.cancel = nil
		// A subscriber may have arrived while the previous connection was
		// failing and this goroutine was on its way out. Keep the listener
		// alive for it.
		if !n.closed && len(n.subs) > 0 {
			n.startLocked()
		}
		n.mu.Unlock()
	}()

	backoff := notifyRetryMin
	for {
		if ctx.Err() != nil {
			return
		}

		err := n.listenOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		n.logger.Warn("imap: notification listener failed", "error", err)
		if !sleepContext(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// listenOnce blocks on one LISTEN connection until the context is cancelled or
// the connection fails.
//
// Raw is the sanctioned way to reach the driver connection: database/sql has no
// API for waiting on a notification, and polling is what caused the outage this
// replaces.
func (n *changeNotifier) listenOnce(ctx context.Context) error {
	conn, err := n.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("imap: get conn for listen: %w", err)
	}
	defer conn.Close()

	err = conn.Raw(func(driverConn any) error {
		sc, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("imap: listen conn is %T, want *stdlib.Conn", driverConn)
		}
		pgxConn := sc.Conn()
		if pgxConn == nil {
			return fmt.Errorf("imap: listen conn has no pgx connection")
		}

		if _, err := pgxConn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
			return fmt.Errorf("imap: listen %s: %w", notifyChannel, err)
		}

		for {
			notification, err := pgxConn.WaitForNotification(ctx)
			if err != nil {
				return fmt.Errorf("imap: wait for notification: %w", err)
			}
			n.broadcast(mailboxIDFromPayload(notification.Payload))
		}
	})

	// The connection goes back to the pool, so clear the LISTEN state.
	// Otherwise it keeps receiving notifications while it serves unrelated
	// queries, and the driver buffers them for a reader that never comes.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, uerr := conn.ExecContext(cleanupCtx, "UNLISTEN "+notifyChannel); uerr != nil && err == nil {
		err = fmt.Errorf("imap: unlisten %s: %w", notifyChannel, uerr)
	}

	return err
}

// broadcast wakes the subscribers interested in mailboxID. A zero id means the
// payload could not be understood, so every subscriber is woken: a spurious
// count query is cheaper than a mailbox that never sees new mail while idling.
func (n *changeNotifier) broadcast(mailboxID int64) {
	n.mu.Lock()
	targets := make([]chan struct{}, 0, len(n.subs))
	for sub := range n.subs {
		if mailboxID == 0 || sub.mailboxID == 0 || sub.mailboxID == mailboxID {
			targets = append(targets, sub.ch)
		}
	}
	n.mu.Unlock()

	for _, ch := range targets {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// mailboxIDFromPayload reads the mailbox id from the trigger's JSON payload.
func mailboxIDFromPayload(payload string) int64 {
	var p struct {
		MailboxID int64 `json:"mailbox_id"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return 0
	}
	return p.MailboxID
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > notifyRetryMax {
		return notifyRetryMax
	}
	return d
}

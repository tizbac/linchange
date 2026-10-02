package imapd

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/emersion/go-imap/v2/imapserver"
)

// pgListener handles pg_notify for IDLE support.
type pgListener struct {
	db     *sqlx.DB
	logger *slog.Logger
	Notify chan struct{}
	conn   *sql.Conn
}

// Listen starts listening for message_change notifications.
func (l *pgListener) Listen(ctx context.Context, channel string) error {
	// Get a raw connection for LISTEN.
	var err error
	l.conn, err = l.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("imap: get conn for listen: %w", err)
	}

	l.Notify = make(chan struct{}, 1)

	// Start a goroutine to handle notifications.
	go l.listenLoop(ctx, channel)

	return nil
}

// Unlisten stops listening.
func (l *pgListener) Unlisten(ctx context.Context) error {
	if l.conn != nil {
		_, err := l.conn.ExecContext(ctx, "UNLISTEN message_change")
		if err != nil {
			l.logger.Warn("imap: unlisten", "error", err)
		}
		l.conn.Close()
	}
	return nil
}

func (l *pgListener) listenLoop(ctx context.Context, channel string) {
	// Execute LISTEN.
	_, err := l.conn.ExecContext(ctx, "LISTEN "+strings.ReplaceAll(channel, " ", "_"))
	if err != nil {
		l.logger.Error("imap: listen", "channel", channel, "error", err)
		return
	}

	// Use the connection to wait for notifications.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Poll for notifications.
		rows, err := l.conn.QueryContext(ctx, "SELECT 1")
		if err != nil {
			// Check if the connection is still alive.
			if ctx.Err() != nil {
				return
			}
			l.logger.Warn("imap: notification poll", "error", err)
			// Try to reconnect.
			l.conn.Close()
			l.conn, err = l.db.Conn(ctx)
			if err != nil {
				continue
			}
			l.conn.ExecContext(ctx, "LISTEN "+strings.ReplaceAll(channel, " ", "_"))
			continue
		}
		rows.Close()

		// Trigger the notify channel.
		select {
		case l.Notify <- struct{}{}:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// Update writer helper
// ---------------------------------------------------------------------------

// messageCountWriter writes untagged EXISTS responses for IDLE.
type messageCountWriter struct {
	w *imapserver.UpdateWriter
}

func (w *messageCountWriter) MessageCount(count uint32) error {
	return w.w.WriteNumMessages(count)
}

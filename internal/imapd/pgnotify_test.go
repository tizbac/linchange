package imapd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/testdb"
)

func testNotifier(t *testing.T, db *sqlx.DB) *changeNotifier {
	t.Helper()
	n := newChangeNotifier(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(n.Close)
	return n
}

// notifyUntil publishes a notification for mailboxID until the subscriber
// observes one. pg_notify is not queued for sessions that have not issued
// LISTEN yet, so the first iterations of a fresh listener legitimately miss.
func notifyUntil(t *testing.T, db *sqlx.DB, ch <-chan struct{}, mailboxID int64) {
	t.Helper()

	payload := fmt.Sprintf(`{"mailbox_id":%d}`, mailboxID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := db.ExecContext(context.Background(),
			"SELECT pg_notify($1, $2)", notifyChannel, payload); err != nil {
			t.Fatalf("pg_notify: %v", err)
		}
		select {
		case <-ch:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("no notification for mailbox %d", mailboxID)
}

func TestChangeNotifierDeliversOnlyMatchingMailbox(t *testing.T) {
	db := testdb.Migrated(t)
	n := testNotifier(t, db)

	ch42, unsub42 := n.subscribe(42)
	defer unsub42()
	ch7, unsub7 := n.subscribe(7)
	defer unsub7()

	notifyUntil(t, db, ch42, 42)

	// The notification for mailbox 42 must not wake a session idling on 7.
	select {
	case <-ch7:
		t.Fatal("mailbox 7 was woken by a change to mailbox 42")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestChangeNotifierReleasesConnectionAfterLastSubscriber(t *testing.T) {
	db := testdb.Migrated(t)
	n := testNotifier(t, db)

	ch, unsubscribe := n.subscribe(1)
	notifyUntil(t, db, ch, 1)

	if inUse := db.Stats().InUse; inUse == 0 {
		t.Fatal("listener did not hold a connection while subscribed")
	}

	unsubscribe()

	deadline := time.Now().Add(5 * time.Second)
	for db.Stats().InUse != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("listener still holds %d connection(s) after the last unsubscribe", db.Stats().InUse)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestChangeNotifierRestartsForANewSubscriber(t *testing.T) {
	db := testdb.Migrated(t)
	n := testNotifier(t, db)

	ch1, unsub1 := n.subscribe(1)
	notifyUntil(t, db, ch1, 1)
	unsub1()

	// Let the previous listener goroutine observe the cancellation.
	time.Sleep(100 * time.Millisecond)

	ch2, unsub2 := n.subscribe(2)
	defer unsub2()
	notifyUntil(t, db, ch2, 2)
}

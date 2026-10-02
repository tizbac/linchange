package inbound

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/testdb"
)

func testGreylist() *Greylist {
	return NewGreylist(config.Greylisting{
		Enabled:     true,
		Delay:       10 * time.Minute,
		Expiry:      24 * time.Hour,
		RetryWindow: 12 * time.Hour,
	})
}

const greyIP = "192.0.2.66"

func TestGreylistFirstContactDefers(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	g := testGreylist()
	ip := netip.MustParseAddr(greyIP)

	action, err := g.Check(ctx, db, ip, "mailer@spam.example", time.Now().UTC())
	if err != nil {
		t.Fatalf("first contact: %v", err)
	}
	if action != GreyDefer {
		t.Errorf("first contact = %s, want defer", action)
	}
}

func TestGreylistRetryCycle(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	g := testGreylist()
	ip := netip.MustParseAddr(greyIP)
	from := "retry@spam.example"
	start := time.Now().UTC().Add(-time.Minute)

	mustCheck := func(at time.Time, want GreylistAction) {
		t.Helper()
		action, err := g.Check(ctx, db, ip, from, at)
		if err != nil {
			t.Fatalf("check at %s: %v", at, err)
		}
		if action != want {
			t.Errorf("check at %s = %s, want %s", at, action, want)
		}
	}

	// First contact.
	mustCheck(start, GreyDefer)
	// Retries before the delay elapses keep deferring.
	mustCheck(start.Add(4*time.Minute), GreyDefer)
	mustCheck(start.Add(8*time.Minute), GreyDefer)
	// After the delay, within the retry window: accepted.
	mustCheck(start.Add(11*time.Minute), GreyAccept)
	mustCheck(start.Add(12*time.Minute), GreyAccept)
	// Long after, the sender is known.
	mustCheck(start.Add(6*time.Hour), GreyAccept)
}

func TestGreylistExpiredRestart(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	g := testGreylist()
	ip := netip.MustParseAddr(greyIP)

	// Seed a contact from 48h ago: it expired (24h expiry) without ever
	// being accepted, so the next contact must start the cycle over.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO greylist (sender_ip, envelope_from, first_seen_at, last_seen_at, expires_at)
		VALUES ($1, 'expired@spam.example', now() - $2::interval, now() - $2::interval, now() - $3::interval)`,
		ip.String(), "48 hours", "24 hours"); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	action, err := g.Check(ctx, db, ip, "expired@spam.example", time.Now().UTC())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if action != GreyDefer {
		t.Errorf("expired entry = %s, want defer", action)
	}
}

func TestGreylistKnownSenderAccepts(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	g := testGreylist()
	ip := netip.MustParseAddr(greyIP)

	// A sender with an accepted history is always fine.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO greylist (sender_ip, envelope_from, first_seen_at, last_seen_at, expires_at, accepted_at)
		VALUES ($1, $2, now(), now(), now() + '24 hours', now())`,
		ip.String(), "known@friend.example"); err != nil {
		t.Fatalf("seeding known sender: %v", err)
	}
	action, err := g.Check(ctx, db, ip, "known@friend.example", time.Now().UTC())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if action != GreyAccept {
		t.Errorf("known sender = %s, want accept", action)
	}
}

func TestGreylistNullSenderSkips(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	g := testGreylist()
	ip := netip.MustParseAddr(greyIP)

	action, err := g.Check(ctx, db, ip, "", time.Now().UTC())
	if err != nil {
		t.Fatalf("null sender: %v", err)
	}
	if action != GreyAccept {
		t.Errorf("null sender = %s, want accept", action)
	}

	var cnt int
	if err := db.GetContext(ctx, &cnt, `SELECT count(*) FROM greylist`); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 0 {
		t.Errorf("null sender created %d greylist rows", cnt)
	}
}

func TestGreylistPrune(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	g := testGreylist()
	ip := netip.MustParseAddr(greyIP)

	// One expired and never-accepted entry, one still live.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO greylist (sender_ip, envelope_from, first_seen_at, last_seen_at, expires_at)
		VALUES ($1, 'gone@spam.example', now() - $2::interval, now() - $2::interval, now() - $3::interval)`,
		ip.String(), "48 hours", "24 hours"); err != nil {
		t.Fatalf("seeding expired: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO greylist (sender_ip, envelope_from, first_seen_at, last_seen_at, expires_at)
		VALUES ($1, 'live@spam.example', now(), now(), now() + $2::interval)`,
		ip.String(), "24 hours"); err != nil {
		t.Fatalf("seeding live: %v", err)
	}

	n, err := g.Prune(ctx, db)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1", n)
	}
	var remaining int
	if err := db.GetContext(ctx, &remaining, `SELECT count(*) FROM greylist`); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Errorf("%d rows remain, want 1", remaining)
	}
}

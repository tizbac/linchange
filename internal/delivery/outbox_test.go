package delivery_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/linchange/linchange/internal/delivery"
	"github.com/linchange/linchange/internal/testdb"
)

func TestDeferredMessageIsClaimedAgainWhenDue(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	repo := delivery.NewOutboxRepository(db)

	var id int64
	if err := db.GetContext(ctx, &id, `
		INSERT INTO outbox (submitted_by, envelope_from, blob_sha256, size)
		VALUES ('alice@example.com', 'alice@example.com', '\x01'::bytea, 1)
		RETURNING id`); err != nil {
		t.Fatalf("inserting a message: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO outbox_recipients (outbox_id, position, address, kind)
		VALUES ($1, 0, 'bob@example.net', 'to')`, id); err != nil {
		t.Fatalf("inserting a recipient: %v", err)
	}

	future := time.Now().Add(time.Hour)
	if err := repo.MarkDeferred(ctx, nil, id, 1, "temporary failure", future); err != nil {
		t.Fatalf("MarkDeferred: %v", err)
	}
	claimed, err := repo.ClaimForSending(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimForSending: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("a deferred message due in an hour was claimed: %+v", claimed)
	}

	// Make it due. The next claim must pick it up and hand the delivery path
	// the deferred recipient rather than dropping it as already settled.
	if _, err := db.ExecContext(ctx, `
		UPDATE outbox SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatalf("making the message due: %v", err)
	}
	claimed, err = repo.ClaimForSending(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimForSending: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != id {
		t.Fatalf("due deferred message = %+v, want id %d", claimed, id)
	}
	if claimed[0].State != delivery.OutboxSending {
		t.Errorf("claim left state %q, want sending", claimed[0].State)
	}
	if len(claimed[0].Recipients) != 1 || claimed[0].Recipients[0].State != delivery.RecipientDeferred {
		t.Fatalf("claimed recipients = %+v, want one deferred recipient", claimed[0].Recipients)
	}

	// A second deferral moves the due time forward again, which is the part
	// that stops a broken destination being hammered every pass.
	if err := repo.MarkDeferred(ctx, nil, id, 2, "still unreachable", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("second MarkDeferred: %v", err)
	}
	claimed, err = repo.ClaimForSending(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimForSending after second deferral: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("a message deferred again was claimed immediately: %+v", claimed)
	}
}

func TestQueueHidesSentAndRetentionDeletesSettledRows(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	repo := delivery.NewOutboxRepository(db)

	insert := func(state string, createdAt time.Time, sentAt, failedAt, bouncedAt *time.Time) int64 {
		t.Helper()
		nullable := func(value *time.Time) any {
			if value == nil {
				return nil
			}
			return *value
		}
		var id int64
		if err := db.GetContext(ctx, &id, `
			INSERT INTO outbox (submitted_by, envelope_from, blob_sha256, size, state,
			                    created_at, sent_at, failed_at, dsn_at)
			VALUES ('alice@example.com', 'alice@example.com', '\x01'::bytea, 1,
			        $1::outbox_state, $2, $3, $4, $5)
			RETURNING id`, state, createdAt, nullable(sentAt), nullable(failedAt), nullable(bouncedAt)); err != nil {
			t.Fatalf("inserting %s message: %v", state, err)
		}
		return id
	}

	now := time.Now().UTC()
	oldSentTime := now.Add(-2 * time.Hour)
	oldSent := insert("sent", oldSentTime, &oldSentTime, nil, nil)
	recentSentTime := now.Add(-10 * time.Minute)
	recentSent := insert("sent", recentSentTime, &recentSentTime, nil, nil)
	oldFailedTime := now.Add(-20 * 24 * time.Hour)
	_ = insert("failed", oldFailedTime, nil, &oldFailedTime, nil)
	recentFailedTime := now.Add(-time.Hour)
	_ = insert("failed", recentFailedTime, nil, &recentFailedTime, nil)
	oldBounceTime := now.Add(-20 * 24 * time.Hour)
	oldBounce := insert("bounced", oldBounceTime, nil, &oldBounceTime, &oldBounceTime)
	queued := insert("queued", now.Add(-30*24*time.Hour), nil, nil, nil)

	// Sent is not part of the default queue view.
	items, err := repo.ListQueue(ctx, nil, delivery.QueueFilter{})
	if err != nil {
		t.Fatalf("ListQueue: %v", err)
	}
	for _, item := range items {
		if item.State == delivery.OutboxSent {
			t.Errorf("a sent message appeared in the default queue: %+v", item)
		}
	}
	withSent, err := repo.ListQueue(ctx, nil, delivery.QueueFilter{IncludeSent: true})
	if err != nil {
		t.Fatalf("ListQueue with sent: %v", err)
	}
	if len(withSent) != len(items)+2 {
		t.Errorf("including sent returned %d rows, want %d", len(withSent), len(items)+2)
	}

	deleted, err := repo.DeleteFiltered(ctx, delivery.QueueFilter{States: []delivery.OutboxState{delivery.OutboxFailed}})
	if err != nil {
		t.Fatalf("DeleteFiltered: %v", err)
	}
	if deleted != 2 {
		t.Errorf("DeleteFiltered deleted %d, want the two failures", deleted)
	}
	var remaining int
	if err := db.GetContext(ctx, &remaining, `SELECT count(*) FROM outbox WHERE state = 'failed'`); err != nil {
		t.Fatalf("counting failures: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d failed rows survived the filtered delete", remaining)
	}

	// The filtered delete removed both failures above, so seed one of each age
	// again to check that retention keeps the recent failure and removes the
	// old one.
	oldFailedAgainTime := now.Add(-20 * 24 * time.Hour)
	_ = insert("failed", oldFailedAgainTime, nil, &oldFailedAgainTime, nil)
	recentFailedTime = now.Add(-time.Hour)
	recentFailed := insert("failed", recentFailedTime, nil, &recentFailedTime, nil)

	purged, err := repo.DeleteExpired(ctx, delivery.SentRetention, delivery.FailedRetention)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if purged != 3 {
		t.Errorf("DeleteExpired removed %d rows, want the old sent (%d), old bounce (%d) and old failure", purged, oldSent, oldBounce)
	}
	var survivors []string
	if err := db.SelectContext(ctx, &survivors, `SELECT id::text || ':' || state::text FROM outbox ORDER BY id`); err != nil {
		t.Fatalf("reading the survivors: %v", err)
	}
	want := map[string]bool{
		rowKey(recentSent, "sent"):     true,
		rowKey(recentFailed, "failed"): true,
		rowKey(queued, "queued"):       true,
	}
	if len(survivors) != len(want) {
		t.Fatalf("survivors = %v, want %d rows", survivors, len(want))
	}
	for _, row := range survivors {
		if !want[row] {
			t.Errorf("unexpected survivor %s", row)
		}
	}
}

func rowKey(id int64, state string) string {
	return strconv.FormatInt(id, 10) + ":" + state
}

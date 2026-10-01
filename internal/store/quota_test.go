package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// seedAdminFixtures provisions a person, a shared mailbox and a domain admin,
// which is the cast every test in this file needs.
func seedAdminFixtures(t *testing.T, db *sqlx.DB) (person, admin string) {
	t.Helper()
	person, _ = seedAliasRepoFixtures(t, db)
	ctx := context.Background()

	admin = "root@example.com"
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: admin, Source: policy.SourceDomainAdmin, DisplayName: "Root",
	}); err != nil {
		t.Fatalf("seeding administrator: %v", err)
	}
	return person, admin
}

func TestQuotaUsageStartsEmpty(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAdminFixtures(t, db)

	// An owner with no counter row has used nothing. Reporting that as an
	// error would make every new mailbox look like a database failure.
	got, err := store.NewQuotas().Get(context.Background(), db, person)
	if err != nil {
		t.Fatalf("reading usage for a fresh owner: %v", err)
	}
	if got.UsedBytes != 0 || got.MessageCount != 0 {
		t.Errorf("usage = %d bytes in %d messages, want zero", got.UsedBytes, got.MessageCount)
	}
}

func TestQuotaUsageAdjustAccumulates(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAdminFixtures(t, db)
	quotas := store.NewQuotas()
	ctx := context.Background()

	if _, err := quotas.Adjust(ctx, db, person, 1000, 1); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	got, err := quotas.Adjust(ctx, db, person, 2500, 2)
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if got.UsedBytes != 3500 {
		t.Errorf("used_bytes = %d, want 3500", got.UsedBytes)
	}
	if got.MessageCount != 3 {
		t.Errorf("message_count = %d, want 3", got.MessageCount)
	}

	// A deletion has to come off the same counter, or the quota would creep
	// upwards every time somebody cleared a message.
	got, err = quotas.Adjust(ctx, db, person, -1000, -1)
	if err != nil {
		t.Fatalf("deleting a message: %v", err)
	}
	if got.UsedBytes != 2500 || got.MessageCount != 2 {
		t.Errorf("usage = %d bytes in %d messages, want 2500 in 2", got.UsedBytes, got.MessageCount)
	}
}

// The counter is the one thing standing between a runaway sender and a full
// disk, so it must refuse a negative total rather than let a bug drive it
// below zero and open the door.
func TestQuotaUsageRefusesToGoNegative(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAdminFixtures(t, db)
	ctx := context.Background()

	if _, err := store.NewQuotas().Adjust(ctx, db, person, 100, 1); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	_, err := store.NewQuotas().Adjust(ctx, db, person, -500, -1)
	if err == nil {
		t.Error("subtracting more than was ever added = nil, want the check constraint to refuse it")
	}
}

// A deletion arriving for an owner with no counter is a no-op rather than a
// failure: a restored mailbox has mail to remove before it has a counter, and
// refusing there would strand the mail instead of the count.
func TestQuotaUsageDeletionBeforeAnyDelivery(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAdminFixtures(t, db)

	got, err := store.NewQuotas().Adjust(context.Background(), db, person, -1000, -1)
	if err != nil {
		t.Fatalf("subtracting from an owner with no counter: %v", err)
	}
	if got.UsedBytes != 0 || got.MessageCount != 0 {
		t.Errorf("usage = %d bytes in %d messages, want zero", got.UsedBytes, got.MessageCount)
	}
}

func TestQuotaUsageExceeded(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAdminFixtures(t, db)
	quotas := store.NewQuotas()
	ctx := context.Background()

	if _, err := quotas.Adjust(ctx, db, person, 900, 1); err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	// Landing exactly on the limit is allowed; passing it is not, so a user
	// can fill their quota to the byte and not be surprised by one more mail.
	over, err := quotas.Exceeded(ctx, db, person, 1000, 100)
	if err != nil {
		t.Fatalf("checking the limit: %v", err)
	}
	if over {
		t.Error("Exceeded at exactly the limit, want false")
	}
	over, err = quotas.Exceeded(ctx, db, person, 1000, 101)
	if err != nil {
		t.Fatalf("checking the limit: %v", err)
	}
	if !over {
		t.Error("Exceeded one byte over the limit, want true")
	}

	// A limit of zero means unlimited rather than "no mail at all".
	over, err = quotas.Exceeded(ctx, db, person, 0, 1<<40)
	if err != nil {
		t.Fatalf("checking an unlimited quota: %v", err)
	}
	if over {
		t.Error("Exceeded against a zero limit, want false: zero means unlimited")
	}
}

func TestQuotaUsageReset(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAdminFixtures(t, db)
	quotas := store.NewQuotas()
	ctx := context.Background()

	if _, err := quotas.Adjust(ctx, db, person, 5000, 5); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := quotas.Reset(ctx, db, person, 12, 1); err != nil {
		t.Fatalf("resetting: %v", err)
	}
	got, err := quotas.Get(ctx, db, person)
	if err != nil {
		t.Fatalf("reading usage: %v", err)
	}
	if got.UsedBytes != 12 || got.MessageCount != 1 {
		t.Errorf("usage = %d bytes in %d messages, want the reset values", got.UsedBytes, got.MessageCount)
	}
}

func TestSendCounterIncrementsPerDay(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAdminFixtures(t, db)
	counters := store.NewSendCounters()
	ctx := context.Background()

	if n, err := counters.Increment(ctx, db, person, 1); err != nil || n != 1 {
		t.Fatalf("first send = %d, %v; want 1", n, err)
	}
	if n, err := counters.Increment(ctx, db, person, 4); err != nil || n != 5 {
		t.Fatalf("second send = %d, %v; want 5", n, err)
	}

	today, err := counters.Today(ctx, db, person)
	if err != nil {
		t.Fatalf("reading today's count: %v", err)
	}
	if today.Count != 5 {
		t.Errorf("count = %d, want 5", today.Count)
	}
}

func TestSendCounterIsPerPrincipal(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAdminFixtures(t, db)
	ctx := context.Background()
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: "carol@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("seeding carol: %v", err)
	}
	counters := store.NewSendCounters()

	if _, err := counters.Increment(ctx, db, person, 3); err != nil {
		t.Fatalf("person send: %v", err)
	}
	n, err := counters.Increment(ctx, db, "carol@example.com", 1)
	if err != nil {
		t.Fatalf("carol send: %v", err)
	}
	if n != 1 {
		t.Errorf("carol's count = %d, want 1: the cap is per user", n)
	}
}

func TestSendCounterPruneRemovesOldDays(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAdminFixtures(t, db)
	ctx := context.Background()
	counters := store.NewSendCounters()

	if _, err := counters.Increment(ctx, db, person, 2); err != nil {
		t.Fatalf("sending: %v", err)
	}
	// Yesterday's counter, planted so there is something old to remove.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO send_counters (owner, day, count)
		VALUES ($1, current_date - 7, 99)`, person); err != nil {
		t.Fatalf("planting an old counter: %v", err)
	}

	removed, err := counters.Prune(ctx, db, time.Now().AddDate(0, 0, -1))
	if err != nil {
		t.Fatalf("pruning: %v", err)
	}
	if removed != 1 {
		t.Errorf("pruned %d counters, want 1", removed)
	}
	today, err := counters.Today(ctx, db, person)
	if err != nil {
		t.Fatalf("reading today's count: %v", err)
	}
	if today.Count != 2 {
		t.Errorf("today's count = %d, want 2: pruning is not allowed to touch today", today.Count)
	}
}

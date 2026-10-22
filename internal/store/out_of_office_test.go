package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

func TestOutOfOfficeScheduleAndOneReplyPerSender(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: "alice@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("creating alice: %v", err)
	}

	settings := store.NewOutOfOffice()
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	end := now.Add(time.Hour)
	setting, err := settings.Set(ctx, db, "alice@example.com", store.OutOfOffice{
		Enabled: true,
		Start:   &start,
		End:     &end,
		Subject: "Away",
		Message: "I am away.",
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !setting.Enabled || setting.Start == nil || setting.End == nil {
		t.Fatalf("setting = %+v, want enabled with a window", setting)
	}
	if !setting.Active(now) {
		t.Error("the setting is not active inside its window")
	}
	if setting.Active(end) {
		t.Error("the setting is active at its exclusive end")
	}
	if setting.Active(start.Add(-time.Minute)) {
		t.Error("the setting is active before its start")
	}

	// A sender is answered once, not once per message.
	if claimed, err := settings.ClaimReply(ctx, db, "alice@example.com", "bob@example.com"); err != nil || !claimed {
		t.Fatalf("first ClaimReply = %v, %v, want true", claimed, err)
	}
	for i := 0; i < 4; i++ {
		if claimed, err := settings.ClaimReply(ctx, db, "alice@example.com", "bob@example.com"); err != nil || claimed {
			t.Fatalf("later ClaimReply = %v, %v, want false", claimed, err)
		}
	}

	// Switching it off and on again starts a new period, so the same sender
	// receives the new message rather than being suppressed by the old one.
	off := setting
	off.Enabled = false
	if _, err := settings.Set(ctx, db, "alice@example.com", off); err != nil {
		t.Fatalf("disabling: %v", err)
	}
	if _, err := settings.Set(ctx, db, "alice@example.com", setting); err != nil {
		t.Fatalf("re-enabling: %v", err)
	}
	if claimed, err := settings.ClaimReply(ctx, db, "alice@example.com", "bob@example.com"); err != nil || !claimed {
		t.Fatalf("ClaimReply after re-enabling = %v, %v, want true", claimed, err)
	}
}

func TestOutOfOfficeRejectsAnEndBeforeItsStart(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: "alice@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("creating alice: %v", err)
	}
	start := time.Now().Add(time.Hour)
	end := start.Add(-time.Minute)
	if _, err := store.NewOutOfOffice().Set(ctx, db, "alice@example.com", store.OutOfOffice{
		Enabled: true, Start: &start, End: &end, Message: "Away",
	}); err == nil {
		t.Fatal("an end before the start was accepted")
	}
}

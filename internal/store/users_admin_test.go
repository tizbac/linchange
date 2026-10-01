package store_test

import (
	"context"
	"testing"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

func TestUsersCreateAndGet(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	err := users.Create(ctx, db, store.CreateUserRequest{
		Address:     "Alice@Example.com",
		Source:      policy.SourceLocal,
		DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}

	got, err := users.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("getting user (case-insensitive): %v", err)
	}
	if got.Address != "Alice@Example.com" {
		t.Errorf("address = %q, want the case as created", got.Address)
	}
	if got.Source != policy.SourceLocal {
		t.Errorf("source = %q, want local", got.Source)
	}
	if !got.Enabled {
		t.Error("enabled = false, want the default true")
	}
	if !got.MailboxEnabled {
		t.Error("mailbox_enabled = false, want the default true for a local user")
	}
	if got.AddressLocal != "alice" {
		t.Errorf("address_local = %q, want alice (lowercased local part)", got.AddressLocal)
	}
	if got.Timezone != "UTC" {
		t.Errorf("timezone = %q, want the default UTC", got.Timezone)
	}
}

func TestUsersCreateRejectsDuplicate(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "alice@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "ALICE@example.com", Source: policy.SourceLocal,
	})
	if err != store.ErrConflict {
		t.Fatalf("re-creating the same address = %v, want ErrConflict", err)
	}
}

func TestUsersCreateRejectsUnknownSource(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	if err := users.Create(context.Background(), db, store.CreateUserRequest{
		Address: "alice@example.com", Source: policy.Source("carrier-pigeon"),
	}); err == nil {
		t.Fatal("expected an unknown source to be refused")
	}
}

// A domain admin administers mail and never receives it. The repository forces
// mailbox_enabled false whether the caller asks for it or not, so the pairing
// is a rule and cannot be opted out of.
func TestUsersCreateDomainAdminCannotHoldMail(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	mailbox := true
	err := users.Create(ctx, db, store.CreateUserRequest{
		Address:        "admin@example.com",
		Source:         policy.SourceDomainAdmin,
		MailboxEnabled: &mailbox,
	})
	if err != nil {
		t.Fatalf("creating domain admin: %v", err)
	}
	got, err := users.Get(ctx, db, "admin@example.com")
	if err != nil {
		t.Fatalf("getting user: %v", err)
	}
	if got.MailboxEnabled {
		t.Error("a domain admin came out able to hold mail; it must not")
	}
}

func TestUsersGetMissing(t *testing.T) {
	db := testMigratedDB(t)
	if _, err := store.NewUsers().Get(context.Background(), db, "nobody@nowhere.invalid"); !store.IsNotFound(err) {
		t.Fatalf("getting a missing user = %v, want ErrNotFound", err)
	}
}

func TestUsersListExcludesDeleted(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	for _, a := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if err := users.Create(ctx, db, store.CreateUserRequest{
			Address: a, Source: policy.SourceLocal,
		}); err != nil {
			t.Fatalf("creating %s: %v", a, err)
		}
	}
	if err := users.Delete(ctx, db, "b@example.com"); err != nil {
		t.Fatalf("deleting b: %v", err)
	}

	got, err := users.List(ctx, db)
	if err != nil {
		t.Fatalf("listing users: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d users, want 2 (the deleted one excluded)", len(got))
	}
	for _, u := range got {
		if u.Address == "b@example.com" {
			t.Fatal("a deleted user appeared in the list")
		}
	}
}

func TestUsersUpdate(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "alice@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("creating user: %v", err)
	}

	quota := int64(1 << 30)
	err := users.Update(ctx, db, "alice@example.com", store.UpdateUserRequest{
		DisplayName:    "Alice A.",
		Enabled:        true,
		MailboxEnabled: true,
		QuotaBytes:     &quota,
		Timezone:       "Europe/Rome",
		Language:       "it",
	})
	if err != nil {
		t.Fatalf("updating user: %v", err)
	}

	got, err := users.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("getting user: %v", err)
	}
	if got.DisplayName != "Alice A." {
		t.Errorf("display_name = %q", got.DisplayName)
	}
	if got.QuotaBytes == nil || *got.QuotaBytes != 1<<30 {
		t.Errorf("quota_bytes = %v, want 1 GiB", got.QuotaBytes)
	}
	if got.Timezone != "Europe/Rome" || got.Language != "it" {
		t.Errorf("timezone/language = %q/%q", got.Timezone, got.Language)
	}
}

func TestUsersUpdateKeepsADomainAdminMailboxFree(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "admin@example.com", Source: policy.SourceDomainAdmin,
	}); err != nil {
		t.Fatalf("creating domain admin: %v", err)
	}

	// An update that asks for a mailbox must still come out without one.
	err := users.Update(ctx, db, "admin@example.com", store.UpdateUserRequest{
		DisplayName: "Admin", Enabled: true, MailboxEnabled: true,
	})
	if err != nil {
		t.Fatalf("updating domain admin: %v", err)
	}
	got, err := users.Get(ctx, db, "admin@example.com")
	if err != nil {
		t.Fatalf("getting user: %v", err)
	}
	if got.MailboxEnabled {
		t.Error("an update re-granted mail to a domain admin; it must stay mailbox-free")
	}
}

func TestUsersSetEnabled(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "alice@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	if err := users.SetEnabled(ctx, db, "alice@example.com", false); err != nil {
		t.Fatalf("disabling user: %v", err)
	}
	got, err := users.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("getting user: %v", err)
	}
	if got.Enabled {
		t.Error("enabled = true after SetEnabled(false)")
	}
}

func TestUsersSetQuota(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "alice@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("creating user: %v", err)
	}

	quota := int64(1 << 30)
	if err := users.SetQuota(ctx, db, "alice@example.com", &quota); err != nil {
		t.Fatalf("setting quota: %v", err)
	}
	if got, _ := users.Get(ctx, db, "alice@example.com"); got.QuotaBytes == nil || *got.QuotaBytes != 1<<30 {
		t.Errorf("quota_bytes = %v, want 1 GiB", got.QuotaBytes)
	}

	// Clearing the override makes the user inherit the domain default again.
	if err := users.SetQuota(ctx, db, "alice@example.com", nil); err != nil {
		t.Fatalf("clearing quota: %v", err)
	}
	if got, _ := users.Get(ctx, db, "alice@example.com"); got.QuotaBytes != nil {
		t.Errorf("quota_bytes = %v, want nil (inherit)", got.QuotaBytes)
	}
}

// Deleting a user is soft: the principal disappears from Get and List, but the
// row (and any mail it owns) survives, so a hard wipe is never one call away.
func TestUsersDeleteIsSoft(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "alice@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	if err := users.Delete(ctx, db, "alice@example.com"); err != nil {
		t.Fatalf("deleting user: %v", err)
	}
	if _, err := users.Get(ctx, db, "alice@example.com"); !store.IsNotFound(err) {
		t.Fatalf("getting a deleted user = %v, want ErrNotFound", err)
	}
	// The row is still there, just deleted: a raw count proves the soft delete.
	var n int
	if err := db.GetContext(ctx, &n, `SELECT count(*) FROM users WHERE address = 'alice@example.com'`); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if n != 1 {
		t.Errorf("the deleted user left %d rows, want the one surviving row", n)
	}
}

package store_test

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// seedAliasRepoFixtures provisions the shape an alias needs to point at: a domain,
// a person to deliver into, and a shared principal to deliver into.
func seedAliasRepoFixtures(t *testing.T, db *sqlx.DB) (person, shared string) {
	t.Helper()
	ctx := context.Background()

	if err := store.NewDomains().Create(ctx, db, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("seeding domain: %v", err)
	}
	users := store.NewUsers()
	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "alice@example.com", Source: policy.SourceLocal, DisplayName: "Alice",
	}); err != nil {
		t.Fatalf("seeding person: %v", err)
	}
	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "support@example.com", Source: policy.SourceShared, DisplayName: "Support",
	}); err != nil {
		t.Fatalf("seeding shared principal: %v", err)
	}
	return "alice@example.com", "support@example.com"
}

func TestAliasCreateAndGet(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAliasRepoFixtures(t, db)
	aliases := store.NewAliases()
	ctx := context.Background()

	if err := aliases.Create(ctx, db, store.CreateAliasRequest{
		Address:       "a.alice@example.com",
		TargetKind:    store.AliasTargetUser,
		TargetAddress: &person,
		DisplayName:   "Alice on her second address",
		Description:   "used by an old signup",
	}); err != nil {
		t.Fatalf("creating alias: %v", err)
	}

	got, err := aliases.Get(ctx, db, "a.alice@example.com")
	if err != nil {
		t.Fatalf("getting alias: %v", err)
	}
	if got.TargetKind != store.AliasTargetUser {
		t.Errorf("target_kind = %q, want user", got.TargetKind)
	}
	if got.TargetAddress == nil || *got.TargetAddress != person {
		t.Errorf("target_address = %v, want %q", got.TargetAddress, person)
	}
	// A user alias carries no workgroup, and the check constraint agrees.
	if got.WorkgroupID != nil {
		t.Errorf("workgroup_id = %v, want nil for a user alias", got.WorkgroupID)
	}
	if !got.Enabled {
		t.Error("enabled = false, want aliases to start enabled")
	}
	if got.DisplayName != "Alice on her second address" {
		t.Errorf("display_name = %q", got.DisplayName)
	}
}

// Addresses are citext, so a second alias at a different case is the same
// alias and must conflict rather than silently fork delivery.
func TestAliasAddressesAreCaseInsensitive(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAliasRepoFixtures(t, db)
	aliases := store.NewAliases()
	ctx := context.Background()

	if err := aliases.Create(ctx, db, store.CreateAliasRequest{
		Address: "a.alice@example.com", TargetKind: store.AliasTargetUser, TargetAddress: &person,
	}); err != nil {
		t.Fatalf("creating alias: %v", err)
	}
	err := aliases.Create(ctx, db, store.CreateAliasRequest{
		Address: "A.Alice@Example.com", TargetKind: store.AliasTargetUser, TargetAddress: &person,
	})
	if err != store.ErrConflict {
		t.Fatalf("re-creating at another case = %v, want ErrConflict", err)
	}
}

// An alias cannot name a target that does not exist: that is how a typo would
// otherwise become an address that silently swallows every message.
func TestAliasRejectsUnknownTarget(t *testing.T) {
	db := testMigratedDB(t)
	seedAliasRepoFixtures(t, db)
	aliases := store.NewAliases()
	ctx := context.Background()

	ghost := "nobody@example.com"
	err := aliases.Create(ctx, db, store.CreateAliasRequest{
		Address: "a.nobody@example.com", TargetKind: store.AliasTargetUser, TargetAddress: &ghost,
	})
	if err != store.ErrConflict {
		t.Fatalf("aliasing an unknown user = %v, want ErrConflict", err)
	}
}

// The kind decides which field carries the target, and a caller that fills in
// both must not trip the check constraint over it. Create drops the field that
// does not belong to the kind.
func TestAliasIgnoresTheTargetFieldThatDoesNotMatchTheKind(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAliasRepoFixtures(t, db)
	aliases := store.NewAliases()
	ctx := context.Background()

	stray := int64(9999)
	if err := aliases.Create(ctx, db, store.CreateAliasRequest{
		Address:       "a.alice@example.com",
		TargetKind:    store.AliasTargetUser,
		TargetAddress: &person,
		WorkgroupID:   &stray, // nonsense for a user alias
	}); err != nil {
		t.Fatalf("creating alias with a stray workgroup id: %v", err)
	}

	got, err := aliases.Get(ctx, db, "a.alice@example.com")
	if err != nil {
		t.Fatalf("getting alias: %v", err)
	}
	if got.WorkgroupID != nil {
		t.Errorf("workgroup_id = %v, want it dropped for a user alias", got.WorkgroupID)
	}
	if got.TargetAddress == nil || *got.TargetAddress != person {
		t.Errorf("target_address = %v, want %q", got.TargetAddress, person)
	}
}

func TestAliasCreateRejectsUnknownKind(t *testing.T) {
	db := testMigratedDB(t)
	seedAliasRepoFixtures(t, db)
	aliases := store.NewAliases()
	ctx := context.Background()

	err := aliases.Create(ctx, db, store.CreateAliasRequest{
		Address: "a.alice@example.com", TargetKind: store.AliasTargetKind("mailbox"),
	})
	if err == nil {
		t.Fatal("creating an alias with an unknown target kind = nil, want an error")
	}
}

func TestAliasCreateRejectsEmptyAddress(t *testing.T) {
	db := testMigratedDB(t)
	seedAliasRepoFixtures(t, db)
	aliases := store.NewAliases()
	ctx := context.Background()

	if err := aliases.Create(ctx, db, store.CreateAliasRequest{Address: ""}); err == nil {
		t.Fatal("creating an alias with no address = nil, want an error")
	}
}

// A disabled alias is kept, not deleted: it still has to be recognisable to
// whoever owns it, it simply delivers nowhere.
func TestAliasSetEnabledKeepsTheRow(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAliasRepoFixtures(t, db)
	aliases := store.NewAliases()
	ctx := context.Background()

	if err := aliases.Create(ctx, db, store.CreateAliasRequest{
		Address: "a.alice@example.com", TargetKind: store.AliasTargetUser, TargetAddress: &person,
	}); err != nil {
		t.Fatalf("creating alias: %v", err)
	}
	if err := aliases.SetEnabled(ctx, db, "a.alice@example.com", false); err != nil {
		t.Fatalf("disabling alias: %v", err)
	}

	got, err := aliases.Get(ctx, db, "a.alice@example.com")
	if err != nil {
		t.Fatalf("a disabled alias must still be readable: %v", err)
	}
	if got.Enabled {
		t.Error("enabled = true, want false")
	}

	if err := aliases.SetEnabled(ctx, db, "nobody@example.com", false); err != store.ErrNotFound {
		t.Errorf("disabling an unknown alias = %v, want ErrNotFound", err)
	}
}

// The reverse lookup is the delivery path's question: given a mailbox, which
// addresses resolve to it?
func TestAliasListForTarget(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAliasRepoFixtures(t, db)
	aliases := store.NewAliases()
	ctx := context.Background()

	for _, address := range []string{"a.alice@example.com", "b.alice@example.com"} {
		if err := aliases.Create(ctx, db, store.CreateAliasRequest{
			Address: address, TargetKind: store.AliasTargetUser, TargetAddress: &person,
		}); err != nil {
			t.Fatalf("creating alias %s: %v", address, err)
		}
	}
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: "carol@example.com", Source: policy.SourceLocal, DisplayName: "Carol",
	}); err != nil {
		t.Fatalf("seeding an unrelated person: %v", err)
	}
	other := "carol@example.com"
	if err := aliases.Create(ctx, db, store.CreateAliasRequest{
		Address: "a.carol@example.com", TargetKind: store.AliasTargetUser, TargetAddress: &other,
	}); err != nil {
		t.Fatalf("creating unrelated alias: %v", err)
	}

	got, err := aliases.ListForTarget(ctx, db, person)
	if err != nil {
		t.Fatalf("resolving aliases: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d aliases for %s, want 2: %+v", len(got), person, got)
	}
	if got[0].Address != "a.alice@example.com" || got[1].Address != "b.alice@example.com" {
		t.Errorf("aliases = %q, %q, want them ordered by address", got[0].Address, got[1].Address)
	}
}

func TestAliasListAndDelete(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedAliasRepoFixtures(t, db)
	aliases := store.NewAliases()
	ctx := context.Background()

	if err := aliases.Create(ctx, db, store.CreateAliasRequest{
		Address: "a.alice@example.com", TargetKind: store.AliasTargetUser, TargetAddress: &person,
	}); err != nil {
		t.Fatalf("creating user alias: %v", err)
	}
	if err := aliases.Create(ctx, db, store.CreateAliasRequest{
		Address: "help@example.com", TargetKind: store.AliasTargetShared, TargetAddress: &shared,
	}); err != nil {
		t.Fatalf("creating shared alias: %v", err)
	}

	all, err := aliases.List(ctx, db)
	if err != nil {
		t.Fatalf("listing aliases: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d aliases, want 2: %+v", len(all), all)
	}
	if all[0].Address != "a.alice@example.com" {
		t.Errorf("first alias = %q, want ordering by address", all[0].Address)
	}

	// Deleting an alias must not touch the principal it pointed at, which is
	// the whole difference between an alias and a shared mailbox.
	if err := aliases.Delete(ctx, db, "a.alice@example.com"); err != nil {
		t.Fatalf("deleting alias: %v", err)
	}
	if _, err := aliases.Get(ctx, db, "a.alice@example.com"); err != store.ErrNotFound {
		t.Errorf("getting a deleted alias = %v, want ErrNotFound", err)
	}
	if _, err := store.NewUsers().Get(ctx, db, person); err != nil {
		t.Errorf("deleting an alias removed its target: %v", err)
	}
	if err := aliases.Delete(ctx, db, "a.alice@example.com"); err != store.ErrNotFound {
		t.Errorf("deleting a missing alias = %v, want ErrNotFound", err)
	}
}

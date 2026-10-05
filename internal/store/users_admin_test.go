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

// A local user may be granted administration while keeping its mailbox. This is
// the ordinary administrator of a small deployment, and it is the shape the demo
// user has: without it the admin pages are unreachable for anyone who receives
// mail.
func TestUsersCreateGrantsAdministrationToAMailboxHolder(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "root@example.com", Source: policy.SourceLocal,
		DisplayName: "Root", IsDomainAdmin: true,
	}); err != nil {
		t.Fatalf("creating an administrator: %v", err)
	}

	got, err := users.Get(ctx, db, "root@example.com")
	if err != nil {
		t.Fatalf("getting user: %v", err)
	}
	if !got.IsDomainAdmin {
		t.Error("is_domain_admin = false, want the grant to have been stored")
	}
	if !got.MailboxEnabled {
		t.Error("mailbox_enabled = false; an administrator who holds mail kept its mailbox")
	}

	// And the grant has to reach the policy identity, because that is what
	// every protocol and route actually reads. LoadPrincipal reports who
	// somebody is and not that they have proved it, so the credential paths
	// are the ones that set Authenticated and this test stands in for one.
	principal, err := store.LoadPrincipal(ctx, db, "root@example.com")
	if err != nil {
		t.Fatalf("loading principal: %v", err)
	}
	if !principal.DomainAdmin() {
		t.Error("LoadPrincipal did not report the administrator")
	}
	principal.Authenticated = true
	if err := principal.Can(policy.ActionAdminister); err != nil {
		t.Errorf("the administrator was refused administration: %v", err)
	}
	if err := principal.Can(policy.ActionIMAP); err != nil {
		t.Errorf("the administrator was refused IMAP: %v", err)
	}
}

// An ordinary local user is not an administrator until somebody says so.
func TestUsersCreateLeavesAnOrdinaryUserWithoutAdministration(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()

	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: "alice@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	principal, err := store.LoadPrincipal(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("loading principal: %v", err)
	}
	if principal.DomainAdmin() {
		t.Error("an ordinary user was granted administration by default")
	}
	if err := principal.Can(policy.ActionAdminister); !policy.IsDenied(err, policy.CodeNotDomainAdmin) {
		t.Errorf("an ordinary user was allowed to administer: %v", err)
	}
}

// The two sources that cannot hold the grant get the answer from the store
// rather than a check violation, and the repository refuses to write the
// combination in the first place.
func TestUsersRefusesTheAdministrationGrantWhereItCannotApply(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "shared@example.com", Source: policy.SourceShared,
		IsDomainAdmin: true,
	}); err != nil {
		t.Fatalf("creating a shared mailbox: %v", err)
	}
	got, err := users.Get(ctx, db, "shared@example.com")
	if err != nil {
		t.Fatalf("getting the shared mailbox: %v", err)
	}
	if got.IsDomainAdmin {
		t.Error("a shared mailbox was granted administration; it can never authenticate")
	}

	// The same on an update, since update replaces the whole row.
	if err := users.Update(ctx, db, "shared@example.com", store.UpdateUserRequest{
		Enabled: true, MailboxEnabled: true, IsDomainAdmin: true,
	}); err != nil {
		t.Fatalf("updating the shared mailbox: %v", err)
	}
	if got, err = users.Get(ctx, db, "shared@example.com"); err != nil {
		t.Fatalf("getting the shared mailbox: %v", err)
	}
	if got.IsDomainAdmin {
		t.Error("an update granted administration to a shared mailbox")
	}
}

// The grant follows the domain_admin source in both directions, so a domain
// admin can be neither demoted nor turned into a shared mailbox by a write.
func TestUsersKeepsADomainAdminAnAdministrator(t *testing.T) {
	db := testMigratedDB(t)
	users := store.NewUsers()
	ctx := context.Background()

	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address: "admin@example.com", Source: policy.SourceDomainAdmin,
	}); err != nil {
		t.Fatalf("creating domain admin: %v", err)
	}
	got, err := users.Get(ctx, db, "admin@example.com")
	if err != nil {
		t.Fatalf("getting user: %v", err)
	}
	if !got.IsDomainAdmin {
		t.Error("a domain_admin row was created without the administration grant")
	}

	if err := users.Update(ctx, db, "admin@example.com", store.UpdateUserRequest{
		Enabled: true, MailboxEnabled: false,
	}); err != nil {
		t.Fatalf("updating domain admin: %v", err)
	}
	if got, err = users.Get(ctx, db, "admin@example.com"); err != nil {
		t.Fatalf("getting user: %v", err)
	}
	if !got.IsDomainAdmin {
		t.Error("an update revoked a domain admin's administration")
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

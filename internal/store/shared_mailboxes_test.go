package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// declareSharedMailbox is the shorthand every test below wants: a shared
// principal exists, so declare it.
func declareSharedMailbox(t *testing.T, db *sqlx.DB, address, name string) {
	t.Helper()
	if err := store.NewSharedMailboxes().Declare(context.Background(), db, store.CreateSharedMailboxRequest{
		Address: address, DisplayName: name,
	}); err != nil {
		t.Fatalf("declaring shared mailbox %s: %v", address, err)
	}
}

func TestSharedMailboxDeclareAndGet(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedAliasRepoFixtures(t, db)
	ctx := context.Background()

	colour := "#0b7285"
	if err := store.NewSharedMailboxes().Declare(ctx, db, store.CreateSharedMailboxRequest{
		Address:     shared,
		DisplayName: "Support",
		Description: "the public helpdesk",
		Colour:      &colour,
		OpenAccess:  true,
		UpdatedBy:   &person,
	}); err != nil {
		t.Fatalf("declaring shared mailbox: %v", err)
	}

	got, err := store.NewSharedMailboxes().Get(ctx, db, shared)
	if err != nil {
		t.Fatalf("getting shared mailbox: %v", err)
	}
	if got.Address != shared {
		t.Errorf("address = %q, want %q", got.Address, shared)
	}
	if got.DisplayName != "Support" {
		t.Errorf("display_name = %q", got.DisplayName)
	}
	if got.Colour == nil || *got.Colour != colour {
		t.Errorf("colour = %v, want %q", got.Colour, colour)
	}
	if !got.OpenAccess {
		t.Error("open_access = false, want true")
	}
	if got.UpdatedBy == nil || *got.UpdatedBy != person {
		t.Errorf("updated_by = %v, want %q", got.UpdatedBy, person)
	}
}

// The declaration is what makes a mailbox a team mailbox, and a guard refuses
// to hang it on a person. That is the difference between sharing a mailbox and
// sharing somebody's mail.
func TestSharedMailboxCannotBeDeclaredForAPerson(t *testing.T) {
	db := testMigratedDB(t)
	person, _ := seedAliasRepoFixtures(t, db)
	ctx := context.Background()

	err := store.NewSharedMailboxes().Declare(ctx, db, store.CreateSharedMailboxRequest{
		Address: person, DisplayName: "Alice and everyone",
	})
	if err != store.ErrConflict {
		t.Fatalf("declaring a person's mailbox shared = %v, want ErrConflict", err)
	}
}

func TestSharedMailboxCannotBeDeclaredTwice(t *testing.T) {
	db := testMigratedDB(t)
	_, shared := seedAliasRepoFixtures(t, db)
	declareSharedMailbox(t, db, shared, "Support")

	err := store.NewSharedMailboxes().Declare(context.Background(), db, store.CreateSharedMailboxRequest{
		Address: shared, DisplayName: "Support again",
	})
	if err != store.ErrConflict {
		t.Fatalf("declaring twice = %v, want ErrConflict", err)
	}
}

func TestSharedMailboxUpdate(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedAliasRepoFixtures(t, db)
	declareSharedMailbox(t, db, shared, "Support")

	subject := "away until Monday"
	body := "We are closed for the holiday."
	expires := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	colour := "#c2255c"

	if err := store.NewSharedMailboxes().Update(context.Background(), db, shared, store.UpdateSharedMailboxRequest{
		DisplayName:        "Support (EMEA)",
		Description:        "moved to the Berlin office",
		Colour:             &colour,
		OpenAccess:         false,
		AutoReplySubject:   &subject,
		AutoReplyBody:      &body,
		AutoReplyExpiresAt: &expires,
		UpdatedBy:          &person,
	}); err != nil {
		t.Fatalf("updating shared mailbox: %v", err)
	}

	got, err := store.NewSharedMailboxes().Get(context.Background(), db, shared)
	if err != nil {
		t.Fatalf("getting shared mailbox: %v", err)
	}
	if got.DisplayName != "Support (EMEA)" {
		t.Errorf("display_name = %q", got.DisplayName)
	}
	if got.OpenAccess {
		t.Error("open_access = true, want false after the update")
	}
	if got.AutoReplySubject == nil || *got.AutoReplySubject != subject {
		t.Errorf("auto_reply_subject = %v, want %q", got.AutoReplySubject, subject)
	}
	if got.AutoReplyExpiresAt == nil || !got.AutoReplyExpiresAt.UTC().Truncate(time.Second).Equal(expires) {
		t.Errorf("auto_reply_expires_at = %v, want %v", got.AutoReplyExpiresAt, expires)
	}

	err = store.NewSharedMailboxes().Update(context.Background(), db, "nobody@example.com", store.UpdateSharedMailboxRequest{})
	if err != store.ErrNotFound {
		t.Errorf("updating an unknown shared mailbox = %v, want ErrNotFound", err)
	}
}

// Undeclare strips the shared semantics without touching the mail, which is the
// one case where the principal survives its declaration.
func TestSharedMailboxUndeclareKeepsThePrincipal(t *testing.T) {
	db := testMigratedDB(t)
	_, shared := seedAliasRepoFixtures(t, db)
	declareSharedMailbox(t, db, shared, "Support")
	ctx := context.Background()

	if err := store.NewSharedMailboxes().Undeclare(ctx, db, shared); err != nil {
		t.Fatalf("undeclaring: %v", err)
	}
	if _, err := store.NewSharedMailboxes().Get(ctx, db, shared); err != store.ErrNotFound {
		t.Errorf("getting an undeclared mailbox = %v, want ErrNotFound", err)
	}
	if _, err := store.NewUsers().Get(ctx, db, shared); err != nil {
		t.Errorf("undeclaring removed the principal: %v", err)
	}
	if err := store.NewSharedMailboxes().Undeclare(ctx, db, shared); err != store.ErrNotFound {
		t.Errorf("undeclaring twice = %v, want ErrNotFound", err)
	}
}

func TestSharedMailboxList(t *testing.T) {
	db := testMigratedDB(t)
	_, shared := seedAliasRepoFixtures(t, db)
	ctx := context.Background()
	boxes := store.NewSharedMailboxes()

	for _, u := range []struct{ address, local string }{
		{"sales@example.com", "sales"},
	} {
		if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
			Address: u.address, Source: policy.SourceShared,
		}); err != nil {
			t.Fatalf("seeding shared principal %s: %v", u.address, err)
		}
		if err := boxes.Declare(ctx, db, store.CreateSharedMailboxRequest{Address: u.address}); err != nil {
			t.Fatalf("declaring %s: %v", u.address, err)
		}
	}
	// The fixture already provisioned a second shared principal.
	if err := boxes.Declare(ctx, db, store.CreateSharedMailboxRequest{Address: shared}); err != nil {
		t.Fatalf("declaring support: %v", err)
	}

	got, err := boxes.List(ctx, db)
	if err != nil {
		t.Fatalf("listing shared mailboxes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d shared mailboxes, want 2: %+v", len(got), got)
	}
	if got[0].Address != "sales@example.com" || got[1].Address != "support@example.com" {
		t.Errorf("order = %q, %q, want ordering by address", got[0].Address, got[1].Address)
	}
}

func TestSharedMailboxGrant(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedAliasRepoFixtures(t, db)
	declareSharedMailbox(t, db, shared, "Support")
	ctx := context.Background()
	grants := store.NewSharedMailboxGrants()

	if err := grants.Grant(ctx, db, shared, person, policy.GrantOwner, &person); err != nil {
		t.Fatalf("granting access: %v", err)
	}

	access, err := grants.Access(ctx, db, shared, person)
	if err != nil {
		t.Fatalf("reading access: %v", err)
	}
	if access.Level != policy.GrantOwner {
		t.Errorf("level = %q, want %q", access.Level, policy.GrantOwner)
	}
	if access.Open {
		t.Error("open = true, want false")
	}

	// Re-granting at a lower level replaces the old one rather than failing:
	// an admin page changing a level should just work.
	if err := grants.Grant(ctx, db, shared, person, policy.GrantRead, &person); err != nil {
		t.Fatalf("re-granting: %v", err)
	}
	access, err = grants.Access(ctx, db, shared, person)
	if err != nil {
		t.Fatalf("reading access after re-grant: %v", err)
	}
	if access.Level != policy.GrantRead {
		t.Errorf("level = %q, want %q after re-granting", access.Level, policy.GrantRead)
	}

	list, err := grants.ListForShared(ctx, db, shared)
	if err != nil {
		t.Fatalf("listing grants: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d grants, want 1 after re-granting: %+v", len(list), list)
	}
}

// A domain admin administers mail but never receives it, so a grant is refused.
// Administrators read other people's mail through audited impersonation, which
// is a deliberate and recorded act rather than a standing permission.
func TestSharedMailboxGrantRefusesAnAdministrator(t *testing.T) {
	db := testMigratedDB(t)
	_, shared := seedAliasRepoFixtures(t, db)
	declareSharedMailbox(t, db, shared, "Support")
	ctx := context.Background()

	admin := "root@example.com"
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: admin, Source: policy.SourceDomainAdmin, DisplayName: "Root",
	}); err != nil {
		t.Fatalf("seeding administrator: %v", err)
	}

	err := store.NewSharedMailboxGrants().Grant(ctx, db, shared, admin, policy.GrantOwner, nil)
	if err != store.ErrConflict {
		t.Fatalf("granting an administrator access = %v, want ErrConflict", err)
	}
}

// A grant names a shared mailbox, and the foreign key is what makes that real:
// a shared principal that was never declared is not a mailbox to grant against.
func TestSharedMailboxGrantToAnUndeclaredPrincipalFails(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedAliasRepoFixtures(t, db)
	ctx := context.Background()

	err := store.NewSharedMailboxGrants().Grant(ctx, db, shared, person, policy.GrantRead, nil)
	if err != store.ErrConflict {
		t.Fatalf("granting against an undeclared shared mailbox = %v, want ErrConflict", err)
	}
}

// Access against a mailbox that does not exist is ErrNotFound, while a mailbox
// that exists with no grant is an empty level and no error: the policy package
// decides what an empty level allows.
func TestSharedMailboxAccessDistinguishesNoGrantFromNoMailbox(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedAliasRepoFixtures(t, db)
	declareSharedMailbox(t, db, shared, "Support")
	ctx := context.Background()
	grants := store.NewSharedMailboxGrants()

	access, err := grants.Access(ctx, db, shared, person)
	if err != nil {
		t.Fatalf("access with no grant = %v, want no error", err)
	}
	if access.Level != "" {
		t.Errorf("level = %q, want empty when no grant exists", access.Level)
	}

	if _, err := grants.Access(ctx, db, "nobody@example.com", person); err != store.ErrNotFound {
		t.Errorf("access to an undeclared mailbox = %v, want ErrNotFound", err)
	}
}

func TestSharedMailboxOpenAccessIsReported(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedAliasRepoFixtures(t, db)
	declareSharedMailbox(t, db, shared, "Support")
	ctx := context.Background()
	grants := store.NewSharedMailboxGrants()

	if err := store.NewSharedMailboxes().Update(ctx, db, shared, store.UpdateSharedMailboxRequest{
		OpenAccess: true,
	}); err != nil {
		t.Fatalf("opening the mailbox to the domain: %v", err)
	}

	access, err := grants.Access(ctx, db, shared, person)
	if err != nil {
		t.Fatalf("reading access: %v", err)
	}
	if !access.Open {
		t.Error("open = false, want true for a mailbox open to the domain")
	}
	if access.Level != "" {
		t.Errorf("level = %q, want empty: open access is not a grant", access.Level)
	}
}

func TestSharedMailboxRevoke(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedAliasRepoFixtures(t, db)
	declareSharedMailbox(t, db, shared, "Support")
	ctx := context.Background()
	grants := store.NewSharedMailboxGrants()

	if err := grants.Grant(ctx, db, shared, person, policy.GrantEditor, nil); err != nil {
		t.Fatalf("granting access: %v", err)
	}
	mine, err := grants.ListForMember(ctx, db, person)
	if err != nil {
		t.Fatalf("listing the member's mailboxes: %v", err)
	}
	if len(mine) != 1 || mine[0].SharedAddress != shared {
		t.Fatalf("ListForMember = %+v, want one grant on %s", mine, shared)
	}

	if err := grants.Revoke(ctx, db, shared, person); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	access, err := grants.Access(ctx, db, shared, person)
	if err != nil {
		t.Fatalf("reading access after revoke: %v", err)
	}
	if access.Level != "" {
		t.Errorf("level = %q, want empty after revoking", access.Level)
	}
	if err := grants.Revoke(ctx, db, shared, person); err != store.ErrNotFound {
		t.Errorf("revoking twice = %v, want ErrNotFound", err)
	}
}

// Users are deleted softly, so retiring a shared mailbox does not cascade: the
// principal disappears from every lookup and the declaration and its grants
// stay put until the principal is purged. The test pins that, because a shared
// mailbox is expected to be recoverable and a hard delete would take the mail
// with it.
func TestRetiringASharedMailboxPrincipalKeepsTheMail(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedAliasRepoFixtures(t, db)
	declareSharedMailbox(t, db, shared, "Support")
	ctx := context.Background()
	grants := store.NewSharedMailboxGrants()

	if err := grants.Grant(ctx, db, shared, person, policy.GrantEditor, nil); err != nil {
		t.Fatalf("granting access: %v", err)
	}
	if err := store.NewUsers().Delete(ctx, db, shared); err != nil {
		t.Fatalf("deleting the shared principal: %v", err)
	}

	// Gone as far as anybody can reach it.
	if _, err := store.NewUsers().Get(ctx, db, shared); err != store.ErrNotFound {
		t.Errorf("a deleted shared principal is still readable: %v", err)
	}

	// But the mail behind it is untouched, which is the point of a soft
	// delete, and so the declaration survives for a possible restore.
	if _, err := store.NewSharedMailboxes().Get(ctx, db, shared); err != nil {
		t.Errorf("soft-deleting a principal destroyed its mailbox: %v", err)
	}

	// Members are unaffected: a colleague's grant is not collateral damage.
	if _, err := store.NewUsers().Get(ctx, db, person); err != nil {
		t.Errorf("deleting a shared mailbox removed a member: %v", err)
	}
	mine, err := grants.ListForMember(ctx, db, person)
	if err != nil {
		t.Fatalf("listing the member's mailboxes: %v", err)
	}
	if len(mine) != 1 {
		t.Errorf("got %d grants for a member, want 1: %+v", len(mine), mine)
	}
}

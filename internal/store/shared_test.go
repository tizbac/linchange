package store_test

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"
)

// An alias and a shared mailbox are different things and the schema keeps them
// apart:
//
//   - An alias is a second address that delivers into a mailbox that already
//     exists. It has no folders, no messages, no quota of its own, and no
//     members. Deleting it changes nothing that was already received.
//   - A shared mailbox is a mailbox that holds mail in its own right, with its
//     own address, folders, messages, quota and member list. Deleting it
//     deletes that mail.
//
// The tests below fail if those two ever collapse into each other, which is
// the mistake that made `shared_mailbox_grants` reference `users` directly and
// allowed a person's private mailbox to be handed to a team.

func seedSharedMailboxFixtures(t *testing.T, db *sqlx.DB) (person, shared string) {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO domains (name) VALUES ('example.com')`); err != nil {
		t.Fatalf("seeding domain: %v", err)
	}
	for _, u := range []struct{ address, local, source string }{
		{"alice@example.com", "alice", "local"},
		{"bob@example.com", "bob", "local"},
		{"support@example.com", "support", "shared"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO users (address, address_local, source) VALUES ($1,$2,$3)`,
			u.address, u.local, u.source); err != nil {
			t.Fatalf("seeding user %s: %v", u.address, err)
		}
	}
	return "alice@example.com", "support@example.com"
}

// A shared mailbox is declared, not inferred. Only a principal whose source is
// 'shared' may be declared one, which is what stops a team being granted
// access to an ordinary person's private mail.
func TestOnlyASharedPrincipalCanBeASharedMailbox(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedSharedMailboxFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address) VALUES ($1)`, shared); err != nil {
		t.Fatalf("declaring a shared mailbox for a shared principal: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address) VALUES ($1)`, person); err == nil {
		t.Fatal("expected a person to be refused as a shared mailbox, " +
			"otherwise their private mail becomes the team's")
	}
}

// The same protection from the other side: a real shared mailbox cannot be
// quietly relabelled as an ordinary person, because its grants and the mail
// already delivered to it would then be unexplainable.
func TestASharedMailboxCannotBeRelabelledAsAPerson(t *testing.T) {
	db := testMigratedDB(t)
	_, shared := seedSharedMailboxFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address) VALUES ($1)`, shared); err != nil {
		t.Fatalf("declaring shared mailbox: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE users SET source = 'local' WHERE address = $1`, shared); err == nil {
		t.Fatal("expected the shared source to be pinned while a shared mailbox is declared")
	}
}

// A shared mailbox holds mail of its own. This is the property that separates
// it from an alias, so it is asserted directly: folders, messages and a quota
// all belong to the shared mailbox itself.
func TestASharedMailboxHoldsItsOwnMail(t *testing.T) {
	db := testMigratedDB(t)
	_, shared := seedSharedMailboxFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address, display_name) VALUES ($1,'Support')`, shared); err != nil {
		t.Fatalf("declaring shared mailbox: %v", err)
	}
	// The quota belongs to the shared mailbox, as its own principal's.
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET quota_bytes = 1073741824 WHERE address = $1`, shared); err != nil {
		t.Fatalf("setting shared mailbox quota: %v", err)
	}

	var mailboxID int64
	if err := db.GetContext(ctx, &mailboxID, `
		INSERT INTO mailboxes (owner, name, uidvalidity)
		VALUES ($1,'INBOX',1) RETURNING id`, shared); err != nil {
		t.Fatalf("a shared mailbox must be able to own an INBOX: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO messages (mailbox_id, uid, blob_sha256, size, subject, internal_date, thread_key)
		VALUES ($1,1,'\x01'::bytea,42,'Ticket 12345',now(),'t1')`, mailboxID); err != nil {
		t.Fatalf("a shared mailbox must be able to receive mail: %v", err)
	}

	// The quota is the shared mailbox's, and is separate from any member's.
	var quota int64
	if err := db.GetContext(ctx, &quota,
		`SELECT quota_bytes FROM users WHERE address = $1`, shared); err != nil {
		t.Fatalf("reading quota: %v", err)
	}
	if quota != 1073741824 {
		t.Errorf("shared mailbox quota = %d, want 1073741824", quota)
	}

	// Mail sits with the shared mailbox, not with any member.
	var messageOwner string
	if err := db.GetContext(ctx, &messageOwner, `
		SELECT m.owner FROM messages ms JOIN mailboxes m ON m.id = ms.mailbox_id
		WHERE ms.id = 1`); err != nil {
		t.Fatalf("reading message owner: %v", err)
	}
	if messageOwner != shared {
		t.Errorf("message owner = %q, want the shared mailbox %q", messageOwner, shared)
	}
}

// Deleting a shared mailbox takes its mail with it. An alias behaves the
// opposite way, which is exactly why they are not the same feature.
func TestDeletingASharedMailboxDeletesItsMail(t *testing.T) {
	db := testMigratedDB(t)
	_, shared := seedSharedMailboxFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address) VALUES ($1)`, shared); err != nil {
		t.Fatalf("declaring shared mailbox: %v", err)
	}
	var mailboxID int64
	if err := db.GetContext(ctx, &mailboxID, `
		INSERT INTO mailboxes (owner, name, uidvalidity) VALUES ($1,'INBOX',1) RETURNING id`,
		shared); err != nil {
		t.Fatalf("seeding mailbox: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO messages (mailbox_id, uid, blob_sha256, size, subject, internal_date, thread_key)
		VALUES ($1,1,'\x01'::bytea,42,'Ticket 12345',now(),'t1')`, mailboxID); err != nil {
		t.Fatalf("seeding message: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO shared_mailbox_grants (shared_address, member_address, level)
		VALUES ($1,'alice@example.com','owner')`, shared); err != nil {
		t.Fatalf("seeding grant: %v", err)
	}

	// A shared mailbox is removed by deleting its principal, which cascades
	// to the declaration, the grants, the folders and the messages. Dropping
	// only the shared_mailboxes row would leave the mail behind.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM users WHERE address = $1`, shared); err != nil {
		t.Fatalf("deleting shared mailbox: %v", err)
	}

	for _, tc := range []struct {
		name, query string
		args        []any
	}{
		{"messages", `SELECT count(*) FROM messages WHERE mailbox_id = $1`, []any{mailboxID}},
		{"grants", `SELECT count(*) FROM shared_mailbox_grants WHERE shared_address = $1`, []any{shared}},
		{"mailboxes", `SELECT count(*) FROM mailboxes WHERE owner = $1`, []any{shared}},
	} {
		var n int
		if err := db.GetContext(ctx, &n, tc.query, tc.args...); err != nil {
			t.Fatalf("counting %s: %v", tc.name, err)
		}
		if n != 0 {
			t.Errorf("%d %s survived the deletion of the shared mailbox", n, tc.name)
		}
	}
}

// Grants may only be made against a declared shared mailbox. This is the
// constraint that the old foreign key onto users() was missing.
func TestGrantsRequireADeclaredSharedMailbox(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedSharedMailboxFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address) VALUES ($1)`, shared); err != nil {
		t.Fatalf("declaring shared mailbox: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO shared_mailbox_grants (shared_address, member_address, level)
		VALUES ($1,'alice@example.com','editor')`, shared); err != nil {
		t.Fatalf("granting against a real shared mailbox: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO shared_mailbox_grants (shared_address, member_address, level)
		VALUES ($1,'bob@example.com','editor')`, person); err == nil {
		t.Fatal("expected a grant against an ordinary person's mailbox to be refused")
	}
}

// A mailbox cannot be a member of itself, which would otherwise recurse in the
// access check and let a shared mailbox be its own owner.
func TestASharedMailboxCannotBeItsOwnMember(t *testing.T) {
	db := testMigratedDB(t)
	_, shared := seedSharedMailboxFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address) VALUES ($1)`, shared); err != nil {
		t.Fatalf("declaring shared mailbox: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO shared_mailbox_grants (shared_address, member_address, level)
		VALUES ($1,$1,'owner')`, shared); err == nil {
		t.Fatal("expected a self-grant to be refused")
	}
}

// A domain admin administers mail but does not receive it, so it must not be
// able to become a member of a shared mailbox. It uses impersonation instead,
// which is audited.
func TestADomainAdminCannotBecomeASharedMailboxMember(t *testing.T) {
	db := testMigratedDB(t)
	_, shared := seedSharedMailboxFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (address, address_local, source, mailbox_enabled)
		VALUES ('admin@example.com','admin','domain_admin',false)`); err != nil {
		t.Fatalf("seeding admin: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address) VALUES ($1)`, shared); err != nil {
		t.Fatalf("declaring shared mailbox: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO shared_mailbox_grants (shared_address, member_address, level)
		VALUES ($1,'admin@example.com','editor')`, shared); err == nil {
		t.Fatal("expected a domain admin to be refused a shared mailbox grant, " +
			"since it administers mail rather than receiving it")
	}
}

// The two features compose: an alias delivers into a shared mailbox, which is
// how a team gets an address that reaches a distribution of people without
// giving anyone a second personal address.
func TestAnAliasCanDeliverIntoASharedMailbox(t *testing.T) {
	db := testMigratedDB(t)
	person, shared := seedSharedMailboxFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address) VALUES ($1)`, shared); err != nil {
		t.Fatalf("declaring shared mailbox: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO shared_mailbox_grants (shared_address, member_address, level)
		VALUES ($1,$2,'owner')`, shared, "alice@example.com"); err != nil {
		t.Fatalf("seeding grant: %v", err)
	}

	// helpdesk@ is an alias; it has no folders and no members of its own.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO aliases (address, target_kind, target_address)
		VALUES ('helpdesk@example.com','shared',$1)`, shared); err != nil {
		t.Fatalf("an alias must be able to target a shared mailbox: %v", err)
	}

	// It resolves to the shared mailbox, not to a person.
	var targetKind, target string
	if err := db.GetContext(ctx, &targetKind,
		`SELECT target_kind::text FROM aliases WHERE address = 'helpdesk@example.com'`); err != nil {
		t.Fatalf("reading alias kind: %v", err)
	}
	if err := db.GetContext(ctx, &target,
		`SELECT target_address FROM aliases WHERE address = 'helpdesk@example.com'`); err != nil {
		t.Fatalf("reading alias target: %v", err)
	}
	if targetKind != "shared" {
		t.Errorf("alias kind = %q, want shared", targetKind)
	}
	if target != shared {
		t.Errorf("alias target = %q, want %q", target, shared)
	}

	// The alias owns nothing. If it had folders or grants it would be a
	// shared mailbox wearing a different name.
	var aliasFolders int
	if err := db.GetContext(ctx, &aliasFolders,
		`SELECT count(*) FROM mailboxes WHERE owner = 'helpdesk@example.com'`); err != nil {
		t.Fatalf("counting alias folders: %v", err)
	}
	if aliasFolders != 0 {
		t.Errorf("the alias owns %d mailboxes; an alias must hold no mail of its own", aliasFolders)
	}
	var aliasGrants int
	if err := db.GetContext(ctx, &aliasGrants, `
		SELECT count(*) FROM shared_mailbox_grants
		WHERE shared_address = 'helpdesk@example.com' OR member_address = 'helpdesk@example.com'`); err != nil {
		t.Fatalf("counting alias grants: %v", err)
	}
	if aliasGrants != 0 {
		t.Errorf("the alias holds %d grants; an alias must have no members", aliasGrants)
	}

	// And the person's own mailbox is untouched by all of this.
	var personFolders int
	if err := db.GetContext(ctx, &personFolders,
		`SELECT count(*) FROM mailboxes WHERE owner = $1`, person); err != nil {
		t.Fatalf("counting person folders: %v", err)
	}
	if personFolders != 0 {
		t.Errorf("the person's mailbox gained %d folders from shared mailbox setup", personFolders)
	}
}

// An alias resolves to a mailbox, and that mailbox is a real user, so the
// foreign key holds the line: an alias cannot dangle.
func TestAliasToASharedMailboxRequiresItToExist(t *testing.T) {
	db := testMigratedDB(t)
	seedSharedMailboxFixtures(t, db)
	ctx := context.Background()

	// A 'shared' principal with no shared_mailboxes row is not a shared
	// mailbox, so an alias cannot claim to target one.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO aliases (address, target_kind, target_address)
		VALUES ('nowhere@example.com','shared','undeclared@example.com')`); err == nil {
		t.Fatal("expected an alias to an undeclared shared mailbox to be refused")
	}
}

// Renaming a shared mailbox is an ordinary edit and has to work. The guard that
// protects a shared principal used to read `source` and `mailbox_enabled` off
// the shared_mailboxes row, where neither column exists, so every update to the
// table failed with "record old has no field source" and the mailbox could be
// declared but never edited.
func TestSharedMailboxCanBeRenamed(t *testing.T) {
	db := testMigratedDB(t)
	_, shared := seedSharedMailboxFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address) VALUES ($1)`, shared); err != nil {
		t.Fatalf("declaring a shared mailbox: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE shared_mailboxes SET display_name = 'Support (EMEA)' WHERE address = $1`,
		shared); err != nil {
		t.Fatalf("renaming a shared mailbox: %v", err)
	}

	var name string
	if err := db.GetContext(ctx, &name,
		`SELECT display_name FROM shared_mailboxes WHERE address = $1`, shared); err != nil {
		t.Fatalf("reading the new name: %v", err)
	}
	if name != "Support (EMEA)" {
		t.Errorf("display_name = %q, want the new name", name)
	}
}

// The rules that used to be written against the wrong table are enforced from
// the users side, where `source` and `mailbox_enabled` actually live.
func TestASharedMailboxPrincipalIsPinned(t *testing.T) {
	db := testMigratedDB(t)
	_, shared := seedSharedMailboxFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO shared_mailboxes (address) VALUES ($1)`, shared); err != nil {
		t.Fatalf("declaring a shared mailbox: %v", err)
	}

	// Relabelling a declared shared mailbox as a person would leave its
	// grants and folders pointing at somebody.
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET source = 'local' WHERE address = $1`, shared); err == nil {
		t.Error("expected the source of a declared shared mailbox to be pinned")
	}

	// And it may not be switched off while it holds mail, which would strand
	// everything already delivered to it.
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET mailbox_enabled = false WHERE address = $1`, shared); err == nil {
		t.Error("expected a declared shared mailbox to refuse being disabled")
	}
}

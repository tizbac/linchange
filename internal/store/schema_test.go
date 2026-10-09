package store_test

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"
)

// These tests pin the schema invariants that the rest of the server relies on
// to be enforced by PostgreSQL rather than by application code. The rules
// they cover were originally wrong in ways no compile-time check could catch,
// and which only showed up as broken behaviour once repositories were written
// against them.

// seedAliasFixtures creates one domain, a person, a shared mailbox and a
// workgroup, and returns them for the alias tests.
type aliasFixtures struct {
	person     string
	shared     string
	workgroup  int64
	otherGroup int64
}

func seedAliasFixtures(t *testing.T, db *sqlx.DB) aliasFixtures {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `INSERT INTO domains (name) VALUES ('example.com')`); err != nil {
		t.Fatalf("seeding domain: %v", err)
	}

	person := "alice@example.com"
	shared := "shared@example.com#shared"
	for _, u := range []struct{ address, local, source string }{
		{person, "alice", "local"},
		{shared, "shared", "shared"},
	} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO users (address, address_local, source) VALUES ($1, $2, $3)`,
			u.address, u.local, u.source)
		if err != nil {
			t.Fatalf("seeding user %s: %v", u.address, err)
		}
	}

	var sales, support int64
	if err := db.GetContext(ctx, &sales,
		`INSERT INTO workgroups (name, domain) VALUES ('sales','example.com') RETURNING id`); err != nil {
		t.Fatalf("seeding workgroup: %v", err)
	}
	if err := db.GetContext(ctx, &support,
		`INSERT INTO workgroups (name, domain) VALUES ('support','example.com') RETURNING id`); err != nil {
		t.Fatalf("seeding workgroup: %v", err)
	}

	return aliasFixtures{person: person, shared: shared, workgroup: sales, otherGroup: support}
}

// An alias must be able to point at a person. The original schema had a single
// bigint target column, which made "point this address at Alice" impossible to
// express, so the only alias you could create was one to a workgroup.
func TestAliasPointsAtAMailboxAddress(t *testing.T) {
	db := testMigratedDB(t)
	f := seedAliasFixtures(t, db)
	ctx := context.Background()

	for _, tc := range []struct {
		name, kind, address, target string
	}{
		{"person", "user", "a.smith@example.com", f.person},
		{"shared mailbox", "shared", "support@example.com", f.shared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx,
				`INSERT INTO aliases (address, target_kind, target_address) VALUES ($1,$2,$3)`,
				tc.address, tc.kind, tc.target)
			if err != nil {
				t.Fatalf("an alias must be able to target a %s: %v", tc.name, err)
			}
		})
	}
}

func TestAliasPointsAtAWorkgroup(t *testing.T) {
	db := testMigratedDB(t)
	f := seedAliasFixtures(t, db)
	ctx := context.Background()

	var name string
	err := db.GetContext(ctx, &name, `
		INSERT INTO aliases (address, target_kind, workgroup_id)
		VALUES ('squad@example.com','workgroup',$1)
		RETURNING (SELECT w.name FROM workgroups w WHERE w.id = aliases.workgroup_id)`,
		f.workgroup)
	if err != nil {
		t.Fatalf("an alias must be able to target a workgroup: %v", err)
	}
	if name != "sales" {
		t.Errorf("workgroup alias resolved to %q, want sales", name)
	}
}

// The declared kind has to agree with the column that carries the target, or
// the resolution code has to guess what the operator meant.
func TestAliasKindMustMatchItsTarget(t *testing.T) {
	db := testMigratedDB(t)
	f := seedAliasFixtures(t, db)
	ctx := context.Background()

	bad := []struct {
		name string
		sql  string
		args []any
	}{
		{
			"person alias carrying a workgroup id",
			`INSERT INTO alias_targets (alias_address, target_kind, target_address, workgroup_id, position)
			 VALUES ('bad1@example.com','user',$1,$2)`,
			[]any{f.person, f.workgroup},
		},
		{
			"workgroup alias carrying an address",
			`INSERT INTO alias_targets (alias_address, target_kind, target_address, workgroup_id, position)
			 VALUES ('bad2@example.com','workgroup',$1,$2)`,
			[]any{f.person, f.workgroup},
		},
		{
			"workgroup alias with no target at all",
			`INSERT INTO alias_targets (alias_address, target_kind, position) VALUES ('bad3@example.com','workgroup',0)`,
			nil,
		},
		{
			"person alias with no target at all",
			`INSERT INTO alias_targets (alias_address, target_kind, position) VALUES ('bad4@example.com','user',0)`,
			nil,
		},
	}

	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, tc.sql, tc.args...); err == nil {
				t.Fatal("expected the database to reject this row")
			}
		})
	}
}

// An alias that targets an address with no user behind it is a configuration
// mistake that silently swallows mail, so it must not be creatable.
func TestAliasTargetMustExist(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx,
		`INSERT INTO aliases (address,target_kind,target_address) VALUES ($1,'user',$2)`,
		"ghost@example.com", "nobody@example.com")
	if err == nil {
		t.Fatal("expected a dangling alias to be rejected")
	}
}

// Alias expansion recurses. An alias pointing at itself would loop forever in
// the MTA as well as in the web UI, so the database refuses it outright.
func TestAliasCannotPointAtItself(t *testing.T) {
	db := testMigratedDB(t)
	f := seedAliasFixtures(t, db)
	ctx := context.Background()

	_, err := db.ExecContext(ctx,
		`INSERT INTO aliases (address,target_kind,target_address) VALUES ($1,'user',$1)`,
		f.person)
	if err == nil {
		t.Fatal("expected a self-referencing alias to be rejected")
	}
}

// Deleting a mailbox must take its aliases with it, or every subsequent
// delivery to that address fails on a dangling reference.
func TestDeletingAUserRemovesTheirAliases(t *testing.T) {
	db := testMigratedDB(t)
	f := seedAliasFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO aliases (address,target_kind,target_address) VALUES ('a.smith@example.com','user',$1)`,
		f.person); err != nil {
		t.Fatalf("seeding alias: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO aliases (address,target_kind,workgroup_id) VALUES ('squad@example.com','workgroup',$1)`,
		f.workgroup); err != nil {
		t.Fatalf("seeding workgroup alias: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE address = $1`, f.person); err != nil {
		t.Fatalf("deleting user: %v", err)
	}

	var dangling int
	if err := db.GetContext(ctx, &dangling,
		`SELECT count(*) FROM aliases WHERE target_address = $1`, f.person); err != nil {
		t.Fatalf("counting aliases: %v", err)
	}
	if dangling != 0 {
		t.Errorf("%d aliases still reference the deleted user", dangling)
	}

	// The workgroup alias is unrelated and must survive.
	var survivors int
	if err := db.GetContext(ctx, &survivors,
		`SELECT count(*) FROM aliases WHERE address = 'squad@example.com'`); err != nil {
		t.Fatalf("counting survivors: %v", err)
	}
	if survivors != 1 {
		t.Errorf("unrelated workgroup alias was removed by the cascade")
	}
}

// This is the lookup that decides where an inbound message lands, so it is on
// the hot path of every delivery.
func TestAliasReverseResolution(t *testing.T) {
	db := testMigratedDB(t)
	f := seedAliasFixtures(t, db)
	ctx := context.Background()

	for _, addr := range []string{"a.smith@example.com", "asmith@example.com"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO aliases (address,target_kind,target_address) VALUES ($1,'user',$2)`,
			addr, f.person); err != nil {
			t.Fatalf("seeding alias: %v", err)
		}
	}

	var found []string
	if err := db.SelectContext(ctx, &found,
		`SELECT address FROM aliases WHERE target_address = $1 ORDER BY address`, f.person); err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if len(found) != 2 {
		t.Errorf("reverse resolution found %d aliases, want 2: %v", len(found), found)
	}
}

// Addresses are compared case-insensitively, so an alias and its target must
// not be creatable with differing case in a way that produces two spellings of
// one mailbox.
func TestAliasAddressIsCaseInsensitive(t *testing.T) {
	db := testMigratedDB(t)
	f := seedAliasFixtures(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO aliases (address,target_kind,target_address) VALUES ('A.Smith@Example.com','user',$1)`,
		f.person); err != nil {
		t.Fatalf("seeding alias: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO aliases (address,target_kind,target_address) VALUES ('a.smith@example.com','user',$1)`,
		f.person); err == nil {
		t.Fatal("expected a differently-cased duplicate alias to be rejected")
	}
}

// The outbox stores one row per recipient rather than parallel arrays, because
// a message where some recipients were accepted and others were not needs a
// bounce naming the ones that failed.
func TestOutboxTracksEachRecipientSeparately(t *testing.T) {
	db := testMigratedDB(t)
	f := seedAliasFixtures(t, db)
	ctx := context.Background()

	var outboxID int64
	if err := db.GetContext(ctx, &outboxID, `
		INSERT INTO outbox (submitted_by, envelope_from, blob_sha256, size)
		VALUES ($1,$2,'\x00'::bytea,10) RETURNING id`, f.person, f.person); err != nil {
		t.Fatalf("seeding outbox: %v", err)
	}

	// The same address in two different headers is legal, and must be two
	// rows, not one row with an ambiguous kind.
	recipients := []struct {
		position int
		address  string
		kind     string
	}{
		{0, "a@remote.test", "to"},
		{1, "b@remote.test", "cc"},
		{2, "a@remote.test", "bcc"},
	}
	for _, r := range recipients {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO outbox_recipients (outbox_id, position, address, kind)
			VALUES ($1,$2,$3,$4)`, outboxID, r.position, r.address, r.kind); err != nil {
			t.Fatalf("seeding recipient %d: %v", r.position, err)
		}
	}

	if _, err := db.ExecContext(ctx, `
		UPDATE outbox_recipients SET state='delivered', delivered_at=now()
		WHERE outbox_id=$1 AND position=0`, outboxID); err != nil {
		t.Fatalf("marking delivered: %v", err)
	}

	type row struct {
		Address string `db:"address"`
		Kind    string `db:"kind"`
		State   string `db:"state"`
	}
	var got []row
	if err := db.SelectContext(ctx, &got, `
		SELECT address, kind, state FROM outbox_recipients
		WHERE outbox_id=$1 ORDER BY position`, outboxID); err != nil {
		t.Fatalf("reading recipients: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d recipients, want 3", len(got))
	}
	if got[0].State != "delivered" {
		t.Errorf("recipient 0 state = %q, want delivered", got[0].State)
	}
	for i, r := range got[1:] {
		if r.State != "pending" {
			t.Errorf("recipient %d state = %q, want pending", i+1, r.State)
		}
	}
	// The bcc recipient's kind must not be lost just because its address
	// repeats one in the To header.
	if got[2].Kind != "bcc" || got[0].Kind != "to" {
		t.Errorf("per-recipient header kind was not preserved: %+v", got)
	}

	// A message counts as delivered only once every recipient is.
	var pending int
	if err := db.GetContext(ctx, &pending,
		`SELECT count(*) FROM outbox_recipients WHERE outbox_id=$1 AND state='pending'`, outboxID); err != nil {
		t.Fatalf("counting pending: %v", err)
	}
	if pending != 2 {
		t.Errorf("%d recipients still pending, want 2", pending)
	}
}

func TestOutboxRecipientRowsAreValidated(t *testing.T) {
	db := testMigratedDB(t)
	f := seedAliasFixtures(t, db)
	ctx := context.Background()

	var outboxID int64
	if err := db.GetContext(ctx, &outboxID, `
		INSERT INTO outbox (submitted_by, envelope_from, blob_sha256, size)
		VALUES ($1,$2,'\x00'::bytea,10) RETURNING id`, f.person, f.person); err != nil {
		t.Fatalf("seeding outbox: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO outbox_recipients (outbox_id, position, address, kind)
		VALUES ($1,0,'a@remote.test','to')`, outboxID); err != nil {
		t.Fatalf("seeding recipient: %v", err)
	}

	bad := []struct {
		name string
		sql  string
		args []any
	}{
		{
			"duplicate header position",
			`INSERT INTO outbox_recipients (outbox_id,position,address,kind) VALUES ($1,0,'b@remote.test','cc')`,
			[]any{outboxID},
		},
		{
			"negative position",
			`INSERT INTO outbox_recipients (outbox_id,position,address,kind) VALUES ($1,-1,'b@remote.test','cc')`,
			[]any{outboxID},
		},
		{
			"empty address",
			`INSERT INTO outbox_recipients (outbox_id,position,address,kind) VALUES ($1,1,'','cc')`,
			[]any{outboxID},
		},
		{
			"negative attempts",
			`INSERT INTO outbox_recipients (outbox_id,position,address,kind,attempts) VALUES ($1,1,'b@remote.test','cc',-1)`,
			[]any{outboxID},
		},
		{
			"recipient of a message that does not exist",
			`INSERT INTO outbox_recipients (outbox_id,position,address,kind) VALUES (999999,0,'b@remote.test','cc')`,
			nil,
		},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, tc.sql, tc.args...); err == nil {
				t.Fatal("expected the database to reject this row")
			}
		})
	}
}

// Deleting a queued message must take its recipients with it, or the delivery
// worker keeps chasing work for a message that is gone.
func TestDeletingAMessageCascadesToRecipients(t *testing.T) {
	db := testMigratedDB(t)
	f := seedAliasFixtures(t, db)
	ctx := context.Background()

	var outboxID int64
	if err := db.GetContext(ctx, &outboxID, `
		INSERT INTO outbox (submitted_by, envelope_from, blob_sha256, size)
		VALUES ($1,$2,'\x00'::bytea,10) RETURNING id`, f.person, f.person); err != nil {
		t.Fatalf("seeding outbox: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO outbox_recipients (outbox_id, position, address, kind)
		VALUES ($1,0,'a@remote.test','to'),($1,1,'b@remote.test','cc')`, outboxID); err != nil {
		t.Fatalf("seeding recipients: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM outbox WHERE id = $1`, outboxID); err != nil {
		t.Fatalf("deleting outbox row: %v", err)
	}

	var orphans int
	if err := db.GetContext(ctx, &orphans,
		`SELECT count(*) FROM outbox_recipients WHERE outbox_id = $1`, outboxID); err != nil {
		t.Fatalf("counting orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d recipient rows outlived their message", orphans)
	}
}

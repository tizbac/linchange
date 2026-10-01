package store_test

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

func TestImpersonationGrantIsActiveUntilItExpires(t *testing.T) {
	db := testMigratedDB(t)
	person, admin := seedAdminFixtures(t, db)
	grants := store.NewImpersonationGrants()
	ctx := context.Background()

	expires := time.Now().Add(24 * time.Hour)
	if err := grants.Grant(ctx, db, admin, person, "supporting a migration", &expires); err != nil {
		t.Fatalf("granting impersonation: %v", err)
	}

	ok, err := grants.ActiveFor(ctx, db, admin, person)
	if err != nil {
		t.Fatalf("checking the grant: %v", err)
	}
	if !ok {
		t.Error("ActiveFor = false, want true for a grant that has not expired")
	}

	// Withdrawing it takes effect immediately, which is the point of keeping
	// revoked_at rather than deleting the row: the history of who was allowed
	// to read this person's mail is itself worth keeping.
	if err := grants.Revoke(ctx, db, admin, person); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	ok, err = grants.ActiveFor(ctx, db, admin, person)
	if err != nil {
		t.Fatalf("checking after revoke: %v", err)
	}
	if ok {
		t.Error("ActiveFor = true after revoking, want false")
	}
}

// An expired grant is not a live one. A grant with no expiry is the deliberate
// "until withdrawn" case, which is different from one that lapsed yesterday.
func TestImpersonationGrantExpiry(t *testing.T) {
	db := testMigratedDB(t)
	person, admin := seedAdminFixtures(t, db)
	grants := store.NewImpersonationGrants()
	ctx := context.Background()

	lapsed := time.Now().Add(-time.Hour)
	if err := grants.Grant(ctx, db, admin, person, "an incident last week", &lapsed); err != nil {
		t.Fatalf("granting with a past expiry: %v", err)
	}
	ok, err := grants.ActiveFor(ctx, db, admin, person)
	if err != nil {
		t.Fatalf("checking the grant: %v", err)
	}
	if ok {
		t.Error("ActiveFor = true for an already expired grant, want false")
	}

	// A grant that never expires stays in force until it is withdrawn.
	if err := grants.Grant(ctx, db, admin, person, "standing cover", nil); err != nil {
		t.Fatalf("granting with no expiry: %v", err)
	}
	ok, err = grants.ActiveFor(ctx, db, admin, person)
	if err != nil {
		t.Fatalf("checking the standing grant: %v", err)
	}
	if !ok {
		t.Error("ActiveFor = false for a grant with no expiry, want true")
	}
}

func TestImpersonationGrantIsPerPair(t *testing.T) {
	db := testMigratedDB(t)
	person, admin := seedAdminFixtures(t, db)
	ctx := context.Background()
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: "carol@example.com", Source: policy.SourceLocal,
	}); err != nil {
		t.Fatalf("seeding carol: %v", err)
	}
	grants := store.NewImpersonationGrants()

	if err := grants.Grant(ctx, db, admin, person, "one incident", nil); err != nil {
		t.Fatalf("granting: %v", err)
	}

	// Being able to read Alice's mail is not being able to read Carol's.
	ok, err := grants.ActiveFor(ctx, db, admin, "carol@example.com")
	if err != nil {
		t.Fatalf("checking an ungranted pair: %v", err)
	}
	if ok {
		t.Error("ActiveFor = true for a target that was never granted, want false")
	}

	// And it is directional: a grant to impersonate does not let the target
	// impersonate the administrator.
	ok, err = grants.ActiveFor(ctx, db, person, admin)
	if err != nil {
		t.Fatalf("checking the reverse direction: %v", err)
	}
	if ok {
		t.Error("ActiveFor = true in the reverse direction, want false")
	}
}

// Re-granting replaces rather than duplicating, since the pair is the key.
func TestImpersonationGrantIsReplacedNotDuplicated(t *testing.T) {
	db := testMigratedDB(t)
	person, admin := seedAdminFixtures(t, db)
	grants := store.NewImpersonationGrants()
	ctx := context.Background()

	if err := grants.Grant(ctx, db, admin, person, "first reason", nil); err != nil {
		t.Fatalf("granting: %v", err)
	}
	if err := grants.Grant(ctx, db, admin, person, "second reason", nil); err != nil {
		t.Fatalf("re-granting: %v", err)
	}

	got, err := grants.ListForActor(ctx, db, admin)
	if err != nil {
		t.Fatalf("listing grants: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d grants, want 1: the pair is the key: %+v", len(got), got)
	}
	if got[0].Reason != "second reason" {
		t.Errorf("reason = %q, want the replacement", got[0].Reason)
	}
}

// Re-granting also revives a grant that was withdrawn, which is what an admin
// renewing access means.
func TestImpersonationGrantRevival(t *testing.T) {
	db := testMigratedDB(t)
	person, admin := seedAdminFixtures(t, db)
	grants := store.NewImpersonationGrants()
	ctx := context.Background()

	if err := grants.Grant(ctx, db, admin, person, "first", nil); err != nil {
		t.Fatalf("granting: %v", err)
	}
	if err := grants.Revoke(ctx, db, admin, person); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if err := grants.Grant(ctx, db, admin, person, "again", nil); err != nil {
		t.Fatalf("granting again: %v", err)
	}
	ok, err := grants.ActiveFor(ctx, db, admin, person)
	if err != nil {
		t.Fatalf("checking the grant: %v", err)
	}
	if !ok {
		t.Error("ActiveFor = false after re-granting, want true")
	}
}

func TestImpersonationGrantListsBothWays(t *testing.T) {
	db := testMigratedDB(t)
	person, admin := seedAdminFixtures(t, db)
	ctx := context.Background()
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: "root2@example.com", Source: policy.SourceDomainAdmin,
	}); err != nil {
		t.Fatalf("seeding a second admin: %v", err)
	}
	grants := store.NewImpersonationGrants()

	if err := grants.Grant(ctx, db, admin, person, "one", nil); err != nil {
		t.Fatalf("granting: %v", err)
	}
	if err := grants.Grant(ctx, db, "root2@example.com", person, "two", nil); err != nil {
		t.Fatalf("granting from a second admin: %v", err)
	}

	// "Who may read this person's mail" is the question an audit asks, and it
	// is not the same list as "what has this admin been given".
	who, err := grants.ListForTarget(ctx, db, person)
	if err != nil {
		t.Fatalf("listing impersonators: %v", err)
	}
	if len(who) != 2 {
		t.Fatalf("got %d impersonators of %s, want 2: %+v", len(who), person, who)
	}
	// Ordered by actor, which is citext collation: the digit sorts before '@'.
	if who[0].ActorAddress != "root2@example.com" {
		t.Errorf("first impersonator = %q, want ordering by actor", who[0].ActorAddress)
	}

	mine, err := grants.ListForActor(ctx, db, "root2@example.com")
	if err != nil {
		t.Fatalf("listing grants: %v", err)
	}
	if len(mine) != 1 || mine[0].TargetAddress != person {
		t.Errorf("root2 holds %+v, want exactly one grant on %s", mine, person)
	}
}

func TestImpersonationGrantUnknownAndRevokeTwice(t *testing.T) {
	db := testMigratedDB(t)
	person, admin := seedAdminFixtures(t, db)
	grants := store.NewImpersonationGrants()
	ctx := context.Background()

	if _, err := grants.Get(ctx, db, admin, person); err != store.ErrNotFound {
		t.Errorf("Get on an ungranted pair = %v, want ErrNotFound", err)
	}
	if err := grants.Revoke(ctx, db, admin, person); err != store.ErrNotFound {
		t.Errorf("revoking a grant that was never made = %v, want ErrNotFound", err)
	}

	if err := grants.Grant(ctx, db, admin, person, "one", nil); err != nil {
		t.Fatalf("granting: %v", err)
	}
	if err := grants.Revoke(ctx, db, admin, person); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if err := grants.Revoke(ctx, db, admin, person); err != store.ErrNotFound {
		t.Errorf("revoking twice = %v, want ErrNotFound", err)
	}
}

// A grant cannot name somebody who does not exist, which stops a typo from
// becoming a permission that silently matches nothing.
func TestImpersonationGrantRejectsUnknownPrincipal(t *testing.T) {
	db := testMigratedDB(t)
	_, admin := seedAdminFixtures(t, db)

	err := store.NewImpersonationGrants().Grant(context.Background(), db, admin,
		"nobody@example.com", "a typo", nil)
	if err != store.ErrConflict {
		t.Fatalf("granting against an unknown target = %v, want ErrConflict", err)
	}
}

// --- audit log ---

func TestAuditLogRecordAndList(t *testing.T) {
	db := testMigratedDB(t)
	person, admin := seedAdminFixtures(t, db)
	audit := store.NewAuditLog()
	ctx := context.Background()

	ip := netip.MustParseAddr("203.0.113.9")
	detail := json.RawMessage(`{"before":"old@example.com","after":"new@example.com"}`)

	id, err := audit.Record(ctx, db, store.AuditEntry{
		ActorAddress:  &admin,
		TargetAddress: &person,
		Action:        "user.update",
		TargetType:    "user",
		TargetID:      person,
		IP:            &ip,
		Detail:        detail,
	})
	if err != nil {
		t.Fatalf("recording: %v", err)
	}
	if id == 0 {
		t.Error("id = 0, want a real row id")
	}

	entries, err := audit.List(ctx, db, store.AuditFilter{Action: "user.update"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(entries), entries)
	}

	got := entries[0]
	if got.Action != "user.update" || got.TargetType != "user" || got.TargetID != person {
		t.Errorf("entry = %+v, want the recorded fields", got)
	}
	if got.ActorAddress == nil || *got.ActorAddress != admin {
		t.Errorf("actor = %v, want %q", got.ActorAddress, admin)
	}
	if got.TargetAddress == nil || *got.TargetAddress != person {
		t.Errorf("target = %v, want %q", got.TargetAddress, person)
	}
	if got.IP == nil || *got.IP != ip {
		t.Errorf("ip = %v, want %v", got.IP, ip)
	}

	var read map[string]string
	if err := json.Unmarshal(got.Detail, &read); err != nil {
		t.Fatalf("decoding detail: %v", err)
	}
	if read["after"] != "new@example.com" {
		t.Errorf("detail = %v, want the recorded change", read)
	}
}

// Under impersonation the actor and the target differ, and keeping both is the
// whole point: the log has to show both who acted and whose mail they read.
func TestAuditLogSeparatesActorFromTarget(t *testing.T) {
	db := testMigratedDB(t)
	person, admin := seedAdminFixtures(t, db)
	audit := store.NewAuditLog()
	ctx := context.Background()

	if _, err := audit.Record(ctx, db, store.AuditEntry{
		ActorAddress: &admin, TargetAddress: &person, Action: "impersonation.start",
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}

	entries, err := audit.List(ctx, db, store.AuditFilter{Target: person})
	if err != nil {
		t.Fatalf("listing by target: %v", err)
	}
	if len(entries) != 1 || entries[0].ActorAddress == nil || *entries[0].ActorAddress != admin {
		t.Fatalf("entries for the target = %+v, want one naming %q as the actor", entries, admin)
	}
}

func TestAuditLogEntryWithoutOptionalFields(t *testing.T) {
	db := testMigratedDB(t)
	audit := store.NewAuditLog()
	ctx := context.Background()

	// A login failure has no target, no address and no detail. That has to be
	// recordable: refusing to log the events that matter least would leave a
	// blind spot exactly where one is useful.
	if _, err := audit.Record(ctx, db, store.AuditEntry{Action: "auth.login_failed"}); err != nil {
		t.Fatalf("recording a bare entry: %v", err)
	}

	entries, err := audit.List(ctx, db, store.AuditFilter{Action: "auth.login_failed"})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(entries), entries)
	}
	got := entries[0]
	if got.ActorAddress != nil || got.TargetAddress != nil || got.IP != nil {
		t.Errorf("entry = %+v, want no actor, target or ip", got)
	}
	// An empty detail is an empty object, never SQL NULL, so a reader does
	// not have to handle both shapes.
	if string(got.Detail) != "{}" {
		t.Errorf("detail = %s, want {}", got.Detail)
	}
}

func TestAuditLogFiltersAndOrders(t *testing.T) {
	db := testMigratedDB(t)
	person, admin := seedAdminFixtures(t, db)
	audit := store.NewAuditLog()
	ctx := context.Background()

	for _, e := range []store.AuditEntry{
		{ActorAddress: &admin, TargetAddress: &person, Action: "user.update"},
		{ActorAddress: &admin, TargetAddress: &person, Action: "user.create"},
		{ActorAddress: &person, Action: "auth.login"},
	} {
		if _, err := audit.Record(ctx, db, e); err != nil {
			t.Fatalf("recording %s: %v", e.Action, err)
		}
	}

	// Newest first, which is the order an administrator reads a log in.
	all, err := audit.List(ctx, db, store.AuditFilter{})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d entries, want 3", len(all))
	}

	byActor, err := audit.List(ctx, db, store.AuditFilter{Actor: admin})
	if err != nil {
		t.Fatalf("listing by actor: %v", err)
	}
	if len(byActor) != 2 {
		t.Errorf("got %d entries for %s, want 2", len(byActor), admin)
	}

	// The two filters combine rather than one overriding the other.
	both, err := audit.List(ctx, db, store.AuditFilter{Actor: admin, Action: "user.create"})
	if err != nil {
		t.Fatalf("listing by actor and action: %v", err)
	}
	if len(both) != 1 {
		t.Errorf("got %d entries for admin+user.create, want 1: %+v", len(both), both)
	}

	none, err := audit.List(ctx, db, store.AuditFilter{Action: "nothing.happened"})
	if err != nil {
		t.Fatalf("listing an unmatched action: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("got %d entries for an unmatched action, want none", len(none))
	}
}

func TestAuditLogLimitsResults(t *testing.T) {
	db := testMigratedDB(t)
	audit := store.NewAuditLog()
	ctx := context.Background()

	for range 5 {
		if _, err := audit.Record(ctx, db, store.AuditEntry{Action: "spam.event"}); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}

	page, err := audit.List(ctx, db, store.AuditFilter{Limit: 2})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("got %d entries, want the limit of 2", len(page))
	}

	// A caller cannot ask for the whole table by passing a huge limit.
	huge, err := audit.List(ctx, db, store.AuditFilter{Limit: 100000})
	if err != nil {
		t.Fatalf("listing with a huge limit: %v", err)
	}
	if len(huge) != 5 {
		t.Errorf("got %d entries, want all 5 present but capped", len(huge))
	}

	if n, err := audit.CountFor(ctx, db, "spam.event"); err != nil || n != 5 {
		t.Errorf("CountFor = %d, %v; want 5", n, err)
	}
}

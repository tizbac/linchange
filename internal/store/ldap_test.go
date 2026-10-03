package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linchange/linchange/internal/store"
)

func TestLDAPLinkUpsertAndGet(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	links := store.NewLDAPLinks()
	now := time.Now().UTC().Round(time.Second)

	const dn = "CN=Alice,OU=Users,DC=corp,DC=example,DC=com"
	err := links.Upsert(ctx, db, store.LDAPLink{
		Address: "alice@example.com", Provider: "corp", DN: dn,
		DisplayName: "Alice Example", MemberOf: []string{"CN=Users,DC=corp"},
	}, now)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := links.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Provider != "corp" || got.DN != dn {
		t.Errorf("Get = %+v, want the corp link we stored", got)
	}
	if got.DisplayName != "Alice Example" {
		t.Errorf("DisplayName = %q, want the directory value", got.DisplayName)
	}
	if len(got.MemberOf) != 1 || got.MemberOf[0] != "CN=Users,DC=corp" {
		t.Errorf("MemberOf = %v, want the group we stored", got.MemberOf)
	}
	if !got.LastSeenAt.Equal(now) {
		t.Errorf("LastSeenAt = %v, want %v", got.LastSeenAt, now)
	}

	// A second sync of the same entry refreshes the row, it does not duplicate.
	again := now.Add(time.Minute)
	if err := links.Upsert(ctx, db, store.LDAPLink{
		Address: "alice@example.com", Provider: "corp", DN: dn,
		DisplayName: "Alice A. Example", MemberOf: []string{"CN=Users,DC=corp", "CN=Admins,DC=corp"},
	}, again); err != nil {
		t.Fatalf("Upsert again: %v", err)
	}
	var count int
	if err := db.GetContext(ctx, &count, `SELECT count(*) FROM ldap_links WHERE address = 'alice@example.com'`); err != nil {
		t.Fatalf("counting links: %v", err)
	}
	if count != 1 {
		t.Fatalf("link count = %d, want 1 (upsert must not duplicate)", count)
	}
	got, err = links.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get after re-upsert: %v", err)
	}
	if !got.LastSeenAt.Equal(again) || got.DisplayName != "Alice A. Example" {
		t.Errorf("re-upsert did not refresh the row: %+v", got)
	}
}

func TestLDAPLinkOneProviderPerAddress(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	links := store.NewLDAPLinks()
	now := time.Now().UTC().Round(time.Second)

	if err := links.Upsert(ctx, db, store.LDAPLink{Address: "bob@example.com", Provider: "corp", DN: "CN=Bob"}, now); err != nil {
		t.Fatalf("Upsert corp: %v", err)
	}
	// A later sync that sees the entry under a different provider repoints the
	// link; the address stays the key.
	if err := links.Upsert(ctx, db, store.LDAPLink{Address: "bob@example.com", Provider: "ad2", DN: "CN=Bob,DC=ad2"}, now); err != nil {
		t.Fatalf("Upsert ad2: %v", err)
	}
	got, err := links.Get(ctx, db, "bob@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Provider != "ad2" || got.DN != "CN=Bob,DC=ad2" {
		t.Errorf("link = %+v, want it repointed at ad2", got)
	}
}

func TestLDAPLinkListForProvider(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	links := store.NewLDAPLinks()
	now := time.Now().UTC().Round(time.Second)

	if err := links.Upsert(ctx, db, store.LDAPLink{Address: "alice@example.com", Provider: "corp", DN: "CN=Alice"}, now); err != nil {
		t.Fatalf("Upsert alice: %v", err)
	}
	if err := links.Upsert(ctx, db, store.LDAPLink{Address: "bob@example.com", Provider: "corp", DN: "CN=Bob"}, now); err != nil {
		t.Fatalf("Upsert bob: %v", err)
	}
	if err := links.Upsert(ctx, db, store.LDAPLink{Address: "support@example.com", Provider: "other", DN: "CN=Support"}, now); err != nil {
		t.Fatalf("Upsert other: %v", err)
	}

	all, err := links.ListForProvider(ctx, db, "corp")
	if err != nil {
		t.Fatalf("ListForProvider: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListForProvider returned %d links, want 2", len(all))
	}
	for _, l := range all {
		if l.Provider != "corp" {
			t.Errorf("link %v has provider %q, want corp", l.Address, l.Provider)
		}
	}
}

func TestLDAPLinkDelete(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	links := store.NewLDAPLinks()
	now := time.Now().UTC().Round(time.Second)

	if err := links.Upsert(ctx, db, store.LDAPLink{Address: "alice@example.com", Provider: "corp", DN: "CN=Alice"}, now); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := links.Delete(ctx, db, "alice@example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := links.Get(ctx, db, "alice@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
	if err := links.Delete(ctx, db, "alice@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}

func TestLDAPLinkFollowsPrincipal(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	links := store.NewLDAPLinks()
	now := time.Now().UTC().Round(time.Second)

	if err := links.Upsert(ctx, db, store.LDAPLink{Address: "alice@example.com", Provider: "corp", DN: "CN=Alice"}, now); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Deleting the principal removes its link.
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE address = 'alice@example.com'`); err != nil {
		t.Fatalf("deleting principal: %v", err)
	}
	if _, err := links.Get(ctx, db, "alice@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after principal deletion = %v, want ErrNotFound (cascade)", err)
	}
}

func TestLDAPSyncStateRecordAndGet(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()
	state := store.NewLDAPSyncState()

	// A provider that has never run reads as a zero state, not an error.
	zero, err := state.Get(ctx, db, "corp")
	if err != nil {
		t.Fatalf("Get for an unseen provider: %v", err)
	}
	if zero.LastRunAt != nil || zero.LastSuccessAt != nil || zero.LastError != "" {
		t.Errorf("unseen provider state = %+v, want zero", zero)
	}

	ran := time.Now().UTC().Round(time.Second)
	if err := state.RecordRun(ctx, db, "corp", ran, &ran, "", 10, 2, 3, 0); err != nil {
		t.Fatalf("RecordRun success: %v", err)
	}
	got, err := state.Get(ctx, db, "corp")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(ran) {
		t.Errorf("LastRunAt = %v, want %v", got.LastRunAt, ran)
	}
	if got.LastSuccessAt == nil || !got.LastSuccessAt.Equal(ran) {
		t.Errorf("LastSuccessAt = %v, want %v", got.LastSuccessAt, ran)
	}
	if got.EntriesSeen != 10 || got.EntriesAdded != 2 || got.EntriesUpdated != 3 {
		t.Errorf("counters = %+v, want seen=10 added=2 updated=3", got)
	}

	// A failed run records its error but keeps the last success.
	again := ran.Add(time.Minute)
	if err := state.RecordRun(ctx, db, "corp", again, nil, "ldap: server down", 0, 0, 0, 0); err != nil {
		t.Fatalf("RecordRun failure: %v", err)
	}
	got, err = state.Get(ctx, db, "corp")
	if err != nil {
		t.Fatalf("Get after failure: %v", err)
	}
	if got.LastError != "ldap: server down" {
		t.Errorf("LastError = %q, want the recorded error", got.LastError)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(again) {
		t.Errorf("LastRunAt = %v, want the later run", got.LastRunAt)
	}
	if got.LastSuccessAt == nil || !got.LastSuccessAt.Equal(ran) {
		t.Errorf("LastSuccessAt = %v, want the earlier successful run kept", got.LastSuccessAt)
	}
	if got.EntriesSeen != 0 {
		t.Errorf("EntriesSeen = %d, want the counters from the latest run", got.EntriesSeen)
	}
}

package store_test

import (
	"context"
	"testing"

	"github.com/linchange/linchange/internal/store"
)

func TestDomainsCreateAndGet(t *testing.T) {
	db := testMigratedDB(t)
	domains := store.NewDomains()
	ctx := context.Background()

	err := domains.Create(ctx, db, store.CreateDomainRequest{
		Name:        "example.com",
		IsLocal:     true,
		Description: "the primary domain",
	})
	if err != nil {
		t.Fatalf("creating domain: %v", err)
	}

	got, err := domains.Get(ctx, db, "example.com")
	if err != nil {
		t.Fatalf("getting domain: %v", err)
	}
	if got.Name != "example.com" {
		t.Errorf("name = %q, want example.com", got.Name)
	}
	if got.Status != store.DomainStatusActive {
		t.Errorf("status = %q, want the default active", got.Status)
	}
	if !got.IsLocal {
		t.Error("is_local = false, want true")
	}
	if got.Description != "the primary domain" {
		t.Errorf("description = %q", got.Description)
	}
	if got.DefaultQuotaBytes != store.DefaultDomainQuotaBytes {
		t.Errorf("default_quota_bytes = %d, want the schema default", got.DefaultQuotaBytes)
	}
}

// Names are citext: Example.COM and example.com are the same domain, so a
// second create at a different case must conflict, not add a row.
func TestDomainNamesAreCaseInsensitive(t *testing.T) {
	db := testMigratedDB(t)
	domains := store.NewDomains()
	ctx := context.Background()

	if err := domains.Create(ctx, db, store.CreateDomainRequest{Name: "Example.com"}); err != nil {
		t.Fatalf("creating domain: %v", err)
	}
	if err := domains.Create(ctx, db, store.CreateDomainRequest{Name: "EXAMPLE.COM"}); err != store.ErrConflict {
		t.Fatalf("re-creating at another case = %v, want ErrConflict", err)
	}
}

func TestDomainsCreateRejectsEmptyName(t *testing.T) {
	db := testMigratedDB(t)
	domains := store.NewDomains()
	if err := domains.Create(context.Background(), db, store.CreateDomainRequest{}); err == nil {
		t.Fatal("expected an empty domain name to be refused")
	}
}

func TestDomainsGetMissing(t *testing.T) {
	db := testMigratedDB(t)
	domains := store.NewDomains()
	if _, err := domains.Get(context.Background(), db, "nowhere.invalid"); !store.IsNotFound(err) {
		t.Fatalf("getting a missing domain = %v, want ErrNotFound", err)
	}
}

func TestDomainsListIsOrdered(t *testing.T) {
	db := testMigratedDB(t)
	domains := store.NewDomains()
	ctx := context.Background()

	for _, name := range []string{"zeta.com", "alpha.com", "mid.com"} {
		if err := domains.Create(ctx, db, store.CreateDomainRequest{Name: name}); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}

	got, err := domains.List(ctx, db)
	if err != nil {
		t.Fatalf("listing domains: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("listed %d domains, want 3", len(got))
	}
	want := []string{"alpha.com", "mid.com", "zeta.com"}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("domain[%d] = %q, want %q", i, got[i].Name, name)
		}
	}
}

func TestDomainsSetStatus(t *testing.T) {
	db := testMigratedDB(t)
	domains := store.NewDomains()
	ctx := context.Background()

	if err := domains.Create(ctx, db, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("creating domain: %v", err)
	}
	if err := domains.SetStatus(ctx, db, "example.com", store.DomainStatusSuspended); err != nil {
		t.Fatalf("suspending domain: %v", err)
	}
	got, err := domains.Get(ctx, db, "example.com")
	if err != nil {
		t.Fatalf("getting domain: %v", err)
	}
	if got.Status != store.DomainStatusSuspended {
		t.Errorf("status = %q, want suspended", got.Status)
	}
	if err := domains.SetStatus(ctx, db, "nowhere.invalid", store.DomainStatusActive); !store.IsNotFound(err) {
		t.Errorf("suspending a missing domain = %v, want ErrNotFound", err)
	}
}

func TestDomainsUpdate(t *testing.T) {
	db := testMigratedDB(t)
	domains := store.NewDomains()
	ctx := context.Background()

	if err := domains.Create(ctx, db, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("creating domain: %v", err)
	}

	limit := 200
	dkim := "mail"
	err := domains.Update(ctx, db, "example.com", store.UpdateDomainRequest{
		Status:            store.DomainStatusActive,
		IsLocal:           true,
		Description:       "updated",
		DefaultQuotaBytes: 5 * 1024 * 1024 * 1024,
		DailySendLimit:    &limit,
		DKIMSelector:      &dkim,
	})
	if err != nil {
		t.Fatalf("updating domain: %v", err)
	}

	got, err := domains.Get(ctx, db, "example.com")
	if err != nil {
		t.Fatalf("getting domain: %v", err)
	}
	if got.Description != "updated" {
		t.Errorf("description = %q, want updated", got.Description)
	}
	if got.DefaultQuotaBytes != 5*1024*1024*1024 {
		t.Errorf("default_quota_bytes = %d", got.DefaultQuotaBytes)
	}
	if got.DailySendLimit == nil || *got.DailySendLimit != 200 {
		t.Errorf("daily_send_limit = %v, want 200", got.DailySendLimit)
	}
	if got.DKIMSelector == nil || *got.DKIMSelector != "mail" {
		t.Errorf("dkim_selector = %v, want mail", got.DKIMSelector)
	}
}

func TestDomainsDelete(t *testing.T) {
	db := testMigratedDB(t)
	domains := store.NewDomains()
	ctx := context.Background()

	if err := domains.Create(ctx, db, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("creating domain: %v", err)
	}
	if err := domains.Delete(ctx, db, "example.com"); err != nil {
		t.Fatalf("deleting domain: %v", err)
	}
	if _, err := domains.Get(ctx, db, "example.com"); !store.IsNotFound(err) {
		t.Errorf("getting a deleted domain = %v, want ErrNotFound", err)
	}
	if err := domains.Delete(ctx, db, "example.com"); !store.IsNotFound(err) {
		t.Errorf("deleting a missing domain = %v, want ErrNotFound", err)
	}
}

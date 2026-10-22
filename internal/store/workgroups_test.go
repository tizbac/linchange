package store_test

import (
	"context"
	"testing"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// An organization is defined by the domains it owns rather than by a list of
// members, so these are the lookups everything else is built on: which group a
// domain belongs to, which domains a group owns, and who is in one of them.
func TestOrganizationResolvesFromItsDomains(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()
	domains := store.NewDomains()
	groups := store.NewWorkgroups()

	for _, name := range []string{"example.com", "example.org", "other.test"} {
		if err := domains.Create(ctx, db, store.CreateDomainRequest{Name: name}); err != nil {
			t.Fatalf("creating domain %s: %v", name, err)
		}
	}

	wg, err := groups.Create(ctx, db, store.CreateWorkgroupRequest{
		Name:        "engineering",
		Domain:      "example.com",
		DisplayName: "Engineering",
	})
	if err != nil {
		t.Fatalf("creating workgroup: %v", err)
	}

	// The home domain resolves even though nothing has attached it to a row yet:
	// a group created before domains.workgroup_id existed still owns its domain.
	got, err := groups.ForDomain(ctx, db, "example.com")
	if err != nil {
		t.Fatalf("for domain home: %v", err)
	}
	if got.ID != wg.ID {
		t.Errorf("for domain home = %d, want %d", got.ID, wg.ID)
	}

	// The second domain is attached through domains.workgroup_id.
	if err := domains.Update(ctx, db, "example.org", store.UpdateDomainRequest{
		Status:      store.DomainStatusActive,
		WorkgroupID: &wg.ID,
	}); err != nil {
		t.Fatalf("attaching domain: %v", err)
	}
	got, err = groups.ForDomain(ctx, db, "example.org")
	if err != nil {
		t.Fatalf("for domain attached: %v", err)
	}
	if got.ID != wg.ID {
		t.Errorf("for domain attached = %d, want %d", got.ID, wg.ID)
	}

	// A domain nobody owns belongs to no organization, which is a missing thing
	// rather than a failed lookup.
	if _, err := groups.ForDomain(ctx, db, "other.test"); !store.IsNotFound(err) {
		t.Errorf("for domain unattached = %v, want not found", err)
	}

	// Addresses resolve by their domain part, at any case, with no trailing
	// surprise from whoever typed it.
	got, err = groups.ForAddress(ctx, db, "Someone@EXAMPLE.org")
	if err != nil {
		t.Fatalf("for address: %v", err)
	}
	if got.ID != wg.ID {
		t.Errorf("for address = %d, want %d", got.ID, wg.ID)
	}
	if _, err := groups.ForAddress(ctx, db, "someone-without-a-domain"); !store.IsNotFound(err) {
		t.Errorf("for address without a domain = %v, want not found", err)
	}

	// The home domain comes first, then the rest in name order, because that is
	// the order the address book shows them in.
	owned, err := groups.Domains(ctx, db, wg.ID)
	if err != nil {
		t.Fatalf("domains: %v", err)
	}
	if len(owned) != 2 || owned[0] != "example.com" || owned[1] != "example.org" {
		t.Errorf("domains = %v, want [example.com example.org]", owned)
	}
}

// Membership in an organization is not a list of members: everyone with an
// address in one of its domains is a contact, and a disabled mailbox is not one.
func TestOrganizationPeopleAreEveryoneInTheDomains(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()
	users := store.NewUsers()
	domains := store.NewDomains()
	groups := store.NewWorkgroups()

	wg, err := groups.Create(ctx, db, store.CreateWorkgroupRequest{
		Name:   "engineering",
		Domain: "example.com",
	})
	if err != nil {
		t.Fatalf("creating workgroup: %v", err)
	}

	// The second domain belongs to the same organization, so people on it count
	// as contacts too.
	for _, name := range []string{"example.com", "example.org", "other.test"} {
		if err := domains.Create(ctx, db, store.CreateDomainRequest{Name: name}); err != nil {
			t.Fatalf("creating domain %s: %v", name, err)
		}
	}
	if err := domains.Update(ctx, db, "example.org", store.UpdateDomainRequest{
		Status:      store.DomainStatusActive,
		WorkgroupID: &wg.ID,
	}); err != nil {
		t.Fatalf("attaching domain: %v", err)
	}

	people := []store.CreateUserRequest{
		{Address: "alice@example.com", Source: policy.SourceLocal, DisplayName: "Alice"},
		{Address: "bob@example.org", Source: policy.SourceLocal, DisplayName: "Bob"},
		{Address: "carol@other.test", Source: policy.SourceLocal, DisplayName: "Carol"},
		{Address: "dave@example.com", Source: policy.SourceLocal, DisplayName: "Dave"},
		// Erin is outside every domain but belongs through a member row.
		{Address: "erin@contractor.test", Source: policy.SourceLocal, DisplayName: "Erin"},
	}
	for _, p := range people {
		if err := users.Create(ctx, db, p); err != nil {
			t.Fatalf("creating %s: %v", p.Address, err)
		}
	}
	if err := groups.AddMember(ctx, db, wg.ID, "erin@contractor.test", store.WorkgroupRoleMember); err != nil {
		t.Fatalf("adding erin: %v", err)
	}
	if err := users.SetEnabled(ctx, db, "dave@example.com", false); err != nil {
		t.Fatalf("disabling dave: %v", err)
	}

	got, err := groups.People(ctx, db, wg.ID)
	if err != nil {
		t.Fatalf("people: %v", err)
	}
	want := []string{"alice@example.com", "bob@example.org", "erin@contractor.test"}
	if len(got) != len(want) {
		t.Fatalf("people = %v, want %v", got, want)
	}
	for i, address := range want {
		if got[i].Address != address {
			t.Errorf("people[%d] = %q, want %q", i, got[i].Address, address)
		}
	}

	// The member row is also enough to resolve the organization for an address
	// outside its domains.
	if wg, err := groups.ForAddress(ctx, db, "erin@contractor.test"); err != nil || wg.ID == 0 {
		t.Errorf("ForAddress for an explicit member = %+v, %v", wg, err)
	}
}

package store_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/linchange/linchange/internal/store"
)

// A domain created without saying anything about sources accepts mail from
// anywhere, which is what every domain did before this setting existed.
func TestDomainsDefaultToNoSourceRestriction(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()

	if err := store.NewDomains().Create(ctx, db, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("creating domain: %v", err)
	}

	got, err := store.NewDomains().Get(ctx, db, "example.com")
	if err != nil {
		t.Fatalf("getting domain: %v", err)
	}
	if len(got.AntispamNetworks) != 0 {
		t.Errorf("antispam_networks = %v, want empty", got.AntispamNetworks)
	}
}

// Networks are stored in the form the inbound listener matches against, so
// "203.0.113.5" and "203.0.113.5/32" are the same entry rather than two.
func TestDomainsStoreNetworksInCanonicalForm(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()

	err := store.NewDomains().Create(ctx, db, store.CreateDomainRequest{
		Name:             "example.com",
		AntispamNetworks: []string{"203.0.113.5", "203.0.113.5/32", "2001:db8::/32", ""},
	})
	if err != nil {
		t.Fatalf("creating domain: %v", err)
	}

	got, err := store.NewDomains().Get(ctx, db, "example.com")
	if err != nil {
		t.Fatalf("getting domain: %v", err)
	}
	want := []string{"203.0.113.5/32", "2001:db8::/32"}
	if len(got.AntispamNetworks) != len(want) {
		t.Fatalf("antispam_networks = %v, want %v", got.AntispamNetworks, want)
	}
	for i, network := range got.AntispamNetworks {
		if network != want[i] {
			t.Errorf("network %d = %q, want %q", i, network, want[i])
		}
	}
}

func TestParseAntispamNetwork(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"192.0.2.5", "192.0.2.5/32"},
		{" 192.0.2.0/24 ", "192.0.2.0/24"},
		{"2001:db8::1", "2001:db8::1/128"},
		{"2001:db8::/32", "2001:db8::/32"},
	}
	for _, c := range cases {
		prefix, err := store.ParseAntispamNetwork(c.raw)
		if err != nil {
			t.Fatalf("ParseAntispamNetwork(%q) = %v", c.raw, err)
		}
		if prefix.String() != c.want {
			t.Errorf("ParseAntispamNetwork(%q) = %s, want %s", c.raw, prefix, c.want)
		}
	}

	for _, bad := range []string{"", "not an address", "192.0.2.0/33", "192.0.2.", "203.0.113.5/"} {
		if _, err := store.ParseAntispamNetwork(bad); err == nil {
			t.Errorf("ParseAntispamNetwork(%q) succeeded, want an error", bad)
		}
	}
}

// Contains is the point of the check: a network holds its own addresses and not
// the ones next to it.
func TestAntispamNetworkContains(t *testing.T) {
	network, err := store.ParseAntispamNetwork("192.0.2.0/24")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	for _, in := range []string{"192.0.2.1", "192.0.2.255", "192.0.2.0"} {
		if !network.Contains(netip.MustParseAddr(in)) {
			t.Errorf("%s is not in %s", in, network)
		}
	}
	for _, out := range []string{"192.0.3.1", "192.1.2.1"} {
		if network.Contains(netip.MustParseAddr(out)) {
			t.Errorf("%s should not be in %s", out, network)
		}
	}
}

// A domain's header rules belong to that domain only: an update addressed to
// one domain must not reach another domain's rule.
func TestDomainHeaderRulesAreScopedToTheirDomain(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()
	domains := store.NewDomains()
	rules := store.NewDomainHeaderRules()

	for _, name := range []string{"alpha.example", "beta.example"} {
		if err := domains.Create(ctx, db, store.CreateDomainRequest{Name: name}); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}

	alpha, err := rules.Create(ctx, db, "alpha.example", store.DomainHeaderRuleRequest{
		Header: "X-Spam",
		Value:  "YES",
		Action: store.HeaderActionJunk,
	})
	if err != nil {
		t.Fatalf("creating rule: %v", err)
	}

	if err := rules.Update(ctx, db, "beta.example", alpha.ID, store.DomainHeaderRuleUpdate{
		Enabled: boolPtr(false),
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("updating another domain's rule = %v, want ErrNotFound", err)
	}

	if _, err := rules.GetForDomain(ctx, db, "beta.example", alpha.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("getting another domain's rule = %v, want ErrNotFound", err)
	}

	got, err := rules.GetForDomain(ctx, db, "alpha.example", alpha.ID)
	if err != nil {
		t.Fatalf("getting the rule: %v", err)
	}
	if !got.Enabled {
		t.Error("the rule was disabled by another domain's update")
	}
}

func TestDomainHeaderRuleDefaultsAndValidation(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()
	rules := store.NewDomainHeaderRules()

	if err := store.NewDomains().Create(ctx, db, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("creating domain: %v", err)
	}

	// "X-Spam YES means junk" needs no other words: contains and junk are the
	// defaults, and matching is insensitive unless the administrator says
	// otherwise.
	rule, err := rules.Create(ctx, db, "example.com", store.DomainHeaderRuleRequest{
		Header: "X-Spam",
		Value:  "YES",
	})
	if err != nil {
		t.Fatalf("creating rule: %v", err)
	}
	if rule.Match != store.HeaderMatchContains || rule.Action != store.HeaderActionJunk {
		t.Errorf("defaults = match %q action %q", rule.Match, rule.Action)
	}
	if rule.CaseSensitive {
		t.Error("case_sensitive = true, want the insensitive default")
	}
	if !rule.Enabled {
		t.Error("enabled = false, want the default true")
	}

	for _, bad := range []store.DomainHeaderRuleRequest{
		{Header: "", Value: "YES"},
		{Header: "X-Spam", Value: ""},
		{Header: "X-Spam", Value: "YES", Match: "sounds_like"},
		{Header: "X-Spam", Value: "YES", Action: "quarantine"},
	} {
		if _, err := rules.Create(ctx, db, "example.com", bad); err == nil {
			t.Errorf("creating %v succeeded, want a refusal", bad)
		}
	}

	// "exists" is a rule about the header being there at all, so an empty value
	// is the whole rule rather than a missing one.
	if _, err := rules.Create(ctx, db, "example.com", store.DomainHeaderRuleRequest{
		Header: "X-Spam-Flag",
		Match:  store.HeaderMatchExists,
	}); err != nil {
		t.Fatalf("creating an exists rule: %v", err)
	}
}

func TestDomainHeaderRuleListAndDelete(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()
	rules := store.NewDomainHeaderRules()

	if err := store.NewDomains().Create(ctx, db, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("creating domain: %v", err)
	}

	for _, req := range []store.DomainHeaderRuleRequest{
		{Header: "X-Spam", Value: "YES"},
		{Header: "X-Spam-Status", Value: "no", Action: store.HeaderActionDiscard},
		{Header: "X-Virus", Value: "found", Action: store.HeaderActionReject, Enabled: boolPtr(false)},
	} {
		if _, err := rules.Create(ctx, db, "example.com", req); err != nil {
			t.Fatalf("creating rule %s: %v", req.Header, err)
		}
	}

	all, err := rules.List(ctx, db, "example.com")
	if err != nil {
		t.Fatalf("listing rules: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("list returned %d rules, want 3", len(all))
	}

	enabled, err := rules.ListEnabled(ctx, db, "example.com")
	if err != nil {
		t.Fatalf("listing enabled rules: %v", err)
	}
	if len(enabled) != 2 {
		t.Fatalf("enabled returned %d rules, want 2", len(enabled))
	}

	if err := rules.Delete(ctx, db, "example.com", all[0].ID); err != nil {
		t.Fatalf("deleting a rule: %v", err)
	}
	if err := rules.Delete(ctx, db, "example.com", all[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleting a rule twice = %v, want ErrNotFound", err)
	}
}

// A rule is deleted with its domain, so a domain removed from the server does
// not leave rules behind pointing at it.
func TestDomainHeaderRulesGoWithTheirDomain(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()
	rules := store.NewDomainHeaderRules()

	if err := store.NewDomains().Create(ctx, db, store.CreateDomainRequest{Name: "gone.example"}); err != nil {
		t.Fatalf("creating domain: %v", err)
	}
	rule, err := rules.Create(ctx, db, "gone.example", store.DomainHeaderRuleRequest{Header: "X-Spam", Value: "yes"})
	if err != nil {
		t.Fatalf("creating rule: %v", err)
	}
	if err := store.NewDomains().Delete(ctx, db, "gone.example"); err != nil {
		t.Fatalf("deleting domain: %v", err)
	}

	var remaining int
	if err := db.GetContext(ctx, &remaining, `SELECT count(*) FROM domain_header_rules WHERE id = $1`, rule.ID); err != nil {
		t.Fatalf("counting rules: %v", err)
	}
	if remaining != 0 {
		t.Errorf("rule %d survived its domain", rule.ID)
	}
}

func boolPtr(b bool) *bool { return &b }

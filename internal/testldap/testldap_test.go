package testldap_test

import (
	"strings"
	"testing"

	ldap "github.com/go-ldap/ldap/v3"

	"github.com/linchange/linchange/internal/testldap"
)

const (
	baseDN    = "dc=corp,dc=example,dc=com"
	adminDN   = "cn=admins,ou=groups," + baseDN
	aliceDN   = "cn=alice,ou=users," + baseDN
	bobDN     = "cn=bob,ou=users," + baseDN
	serviceDN = "cn=svc,ou=users," + baseDN
)

func seed(t *testing.T) *testldap.Server {
	t.Helper()
	s := testldap.Start(t)
	s.AddUser(testldap.Entry{
		DN:       serviceDN,
		Password: "svc-password",
		Attributes: map[string][]string{
			"objectclass": {"user"},
			"mail":        {"svc@corp.example.com"},
			"displayname": {"Service"},
			"memberof":    {adminDN},
		},
	})
	s.AddUser(testldap.Entry{
		DN:       aliceDN,
		Password: "alice-password",
		Attributes: map[string][]string{
			"objectclass": {"user"},
			"mail":        {"Alice@Corp.Example.com"},
			"displayname": {"Alice Johnson"},
			"memberof":    {adminDN},
		},
	})
	s.AddUser(testldap.Entry{
		DN:       bobDN,
		Password: "bob-password",
		Attributes: map[string][]string{
			"objectclass": {"user"},
			"mail":        {"bob@corp.example.com"},
			"displayname": {"Bob Roberts"},
			"memberof":    {},
		},
	})
	s.AddUser(testldap.Entry{
		DN: adminDN,
		Attributes: map[string][]string{
			"objectclass": {"group"},
			"member":      {aliceDN},
		},
	})
	return s
}

func dial(t *testing.T, s *testldap.Server) *ldap.Conn {
	t.Helper()
	conn, err := ldap.DialURL(s.URL())
	if err != nil {
		t.Fatalf("dialing fixture: %v", err)
	}
	t.Cleanup(func() { conn.Unbind() })
	return conn
}

func TestBind(t *testing.T) {
	s := seed(t)
	conn := dial(t, s)

	if err := conn.Bind(aliceDN, "alice-password"); err != nil {
		t.Fatalf("alice bind: %v", err)
	}
	err := conn.Bind(aliceDN, "wrong")
	if err == nil {
		t.Fatal("wrong password was accepted")
	}
	if !ldap.IsErrorAnyOf(err, ldap.LDAPResultInvalidCredentials) {
		t.Fatalf("wrong password should be invalidCredentials, got %v", err)
	}
	if err := conn.Bind("cn=nobody,ou=users,"+baseDN, "whatever"); err == nil {
		t.Fatal("unknown DN was accepted")
	}
	// The group entry carries no password: nobody can bind as it, whatever
	// they type.
	if err := conn.Bind(adminDN, "whatever"); err == nil {
		t.Fatal("passwordless entry was bindable")
	}
	if got := s.Binds(); got != 4 {
		t.Fatalf("binds seen by server = %d, want 4", got)
	}
}

func TestSearchSubtreeAndFilters(t *testing.T) {
	s := seed(t)
	conn := dial(t, s)
	if err := conn.Bind(serviceDN, "svc-password"); err != nil {
		t.Fatalf("service bind: %v", err)
	}

	res, err := conn.Search(&ldap.SearchRequest{
		BaseDN:     baseDN,
		Scope:      ldap.ScopeWholeSubtree,
		Filter:     "(&(objectClass=user)(mail=*@corp.example.com))",
		Attributes: []string{"dn", "mail", "displayName", "memberOf"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("entries = %d, want 3 (service, alice, bob)", len(res.Entries))
	}
	var alice *ldap.Entry
	for _, e := range res.Entries {
		if e.DN == aliceDN {
			alice = e
		}
	}
	if alice == nil {
		t.Fatalf("alice not in results: %v", res.Entries)
	}
	// Attribute values come back as stored.
	if got := alice.GetAttributeValue("mail"); !strings.EqualFold(got, "Alice@Corp.Example.com") {
		t.Fatalf("alice mail = %q", got)
	}
	if got := alice.GetEqualFoldAttributeValue("displayName"); got != "Alice Johnson" {
		t.Fatalf("alice displayName = %q", got)
	}
	groups := alice.GetAttributeValues("memberOf")
	if len(groups) != 1 || !strings.EqualFold(groups[0], adminDN) {
		t.Fatalf("alice memberOf = %v", groups)
	}

	// An equality filter that nothing satisfies returns no entries, not an error.
	res, err = conn.Search(&ldap.SearchRequest{
		BaseDN: baseDN,
		Scope:  ldap.ScopeWholeSubtree,
		Filter: "(mail=nobody@corp.example.com)",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Fatalf("expected no entries, got %d", len(res.Entries))
	}

	if got := s.Searches(); got != 2 {
		t.Fatalf("searches seen by server = %d, want 2", got)
	}
}

func TestSearchScopes(t *testing.T) {
	s := seed(t)
	conn := dial(t, s)
	if err := conn.Bind(serviceDN, "svc-password"); err != nil {
		t.Fatalf("service bind: %v", err)
	}

	run := func(scope int, base, filter string) int {
		t.Helper()
		res, err := conn.Search(&ldap.SearchRequest{
			BaseDN: base,
			Scope:  scope,
			Filter: filter,
		})
		if err != nil {
			t.Fatalf("search scope=%d base=%s: %v", scope, base, err)
		}
		return len(res.Entries)
	}

	if n := run(ldap.ScopeBaseObject, aliceDN, "(objectClass=user)"); n != 1 {
		t.Fatalf("base scope on alice = %d, want 1", n)
	}
	// svc, alice and bob all sit directly under ou=users.
	if n := run(ldap.ScopeSingleLevel, "ou=users,"+baseDN, "(objectClass=user)"); n != 3 {
		t.Fatalf("one-level scope on ou=users = %d, want 3", n)
	}
	if n := run(ldap.ScopeWholeSubtree, baseDN, "(objectClass=user)"); n != 3 {
		t.Fatalf("subtree scope = %d, want 3", n)
	}
	// The group entry is not objectClass user.
	if n := run(ldap.ScopeWholeSubtree, baseDN, "(objectClass=group)"); n != 1 {
		t.Fatalf("group search = %d, want 1", n)
	}
}

// TestSearchPresentWildcard checks that "(attr=*)" matches an entry when and
// only when the attribute is present, whatever it holds.
func TestSearchPresentWildcard(t *testing.T) {
	s := seed(t)
	conn := dial(t, s)
	if err := conn.Bind(serviceDN, "svc-password"); err != nil {
		t.Fatalf("service bind: %v", err)
	}

	// svc, alice, bob and the group all carry an objectClass.
	res, err := conn.Search(&ldap.SearchRequest{
		BaseDN: baseDN,
		Scope:  ldap.ScopeWholeSubtree,
		Filter: "(objectClass=*)",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(res.Entries))
	}

	// An attribute nobody carries matches nothing, not an error.
	res, err = conn.Search(&ldap.SearchRequest{
		BaseDN: baseDN,
		Scope:  ldap.ScopeWholeSubtree,
		Filter: "(seeAlso=*)",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(res.Entries))
	}
}

// TestMultipleMessagesPerConnection makes sure a connection survives several
// round trips, which is what both the login client and the syncer do.
func TestMultipleMessagesPerConnection(t *testing.T) {
	s := seed(t)
	conn := dial(t, s)
	if err := conn.Bind(serviceDN, "svc-password"); err != nil {
		t.Fatalf("service bind: %v", err)
	}
	for i := 0; i < 3; i++ {
		res, err := conn.Search(&ldap.SearchRequest{
			BaseDN: baseDN,
			Scope:  ldap.ScopeWholeSubtree,
			Filter: "(objectClass=user)",
		})
		if err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
		if len(res.Entries) != 3 {
			t.Fatalf("search %d: %d entries, want 3", i, len(res.Entries))
		}
	}
	if err := conn.Bind(aliceDN, "alice-password"); err != nil {
		t.Fatalf("rebind as alice: %v", err)
	}
}

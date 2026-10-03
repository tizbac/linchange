package ldap_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	ldapauth "github.com/linchange/linchange/internal/auth/ldap"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
	"github.com/linchange/linchange/internal/testldap"
)

const (
	baseDN    = "dc=corp,dc=example,dc=com"
	svcDN     = "cn=svc-linchange,ou=service," + baseDN
	aliceDN   = "cn=alice,ou=users," + baseDN
	bobDN     = "cn=bob,ou=users," + baseDN
	adminsDN  = "cn=domain admins,ou=groups," + baseDN
	aliceAddr = "alice@corp.example.com"
	bobAddr   = "bob@corp.example.com"
)

func seed(t *testing.T) *testldap.Server {
	t.Helper()
	srv := testldap.Start(t)
	srv.AddUser(testldap.Entry{
		DN:       svcDN,
		Password: "svc-pass",
		Attributes: map[string][]string{
			"objectclass": {"user"},
		},
	})
	srv.AddUser(testldap.Entry{
		DN:       aliceDN,
		Password: "alice-pass",
		Attributes: map[string][]string{
			"objectclass": {"user"},
			"mail":        {aliceAddr},
			"displayname": {"Alice"},
			"memberof":    {adminsDN},
		},
	})
	srv.AddUser(testldap.Entry{
		DN:       bobDN,
		Password: "bob-pass",
		Attributes: map[string][]string{
			"objectclass": {"user"},
			"mail":        {bobAddr},
		},
	})
	srv.AddUser(testldap.Entry{
		DN: adminsDN,
		Attributes: map[string][]string{
			"objectclass": {"group"},
			"member":      {aliceDN},
		},
	})
	return srv
}

func searchProvider(srv *testldap.Server) config.LDAP {
	return config.LDAP{
		Providers: []config.LDAPProvider{{
			Name:                "corp",
			URL:                 srv.URL(),
			BindDN:              svcDN,
			BindPassword:        "svc-pass",
			BaseDN:              baseDN,
			UserFilter:          "(&(objectClass=user)(mail={{username}}))",
			DomainAdminsGroupDN: adminsDN,
			Timeout:             5 * time.Second,
		}},
	}
}

func newClient(t *testing.T, lcfg config.LDAP, now func() time.Time) (*ldapauth.Client, *store.LDAPLinks, *sqlx.DB) {
	t.Helper()
	db := testdb.Migrated(t)
	links := store.NewLDAPLinks()
	return ldapauth.New(lcfg, links, db, now), links, db
}

func addLink(t *testing.T, links *store.LDAPLinks, q *sqlx.DB, address, provider, dn string) {
	t.Helper()
	ctx := context.Background()
	// The link's address is a foreign key into users, so the principal must
	// exist first: this is the same order the reconcile writes them in.
	if err := store.NewUsers().Create(ctx, q, store.CreateUserRequest{
		Address: address,
		Source:  policy.SourceLDAP,
	}); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	if err := links.Upsert(ctx, q,
		store.LDAPLink{Address: address, Provider: provider, DN: dn}, time.Now()); err != nil {
		t.Fatalf("upserting link: %v", err)
	}
}

func TestAuthenticateSearchMode(t *testing.T) {
	srv := seed(t)
	client, links, db := newClient(t, searchProvider(srv), nil)
	addLink(t, links, db, aliceAddr, "corp", aliceDN)

	ok, err := client.Authenticate(context.Background(), aliceAddr, "alice-pass")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok {
		t.Fatal("expected the directory to accept the password")
	}
	if n := srv.Binds(); n != 2 {
		t.Fatalf("binds = %d, want 2 (service + user)", n)
	}
	if n := srv.Searches(); n != 1 {
		t.Fatalf("searches = %d, want 1", n)
	}

	ok, err = client.Authenticate(context.Background(), aliceAddr, "wrong")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok {
		t.Fatal("expected the directory to refuse the password")
	}
	if n := srv.Binds(); n != 4 {
		t.Fatalf("binds = %d, want 4", n)
	}
	if n := srv.Searches(); n != 2 {
		t.Fatalf("searches = %d, want 2", n)
	}
}

func TestAuthenticateDirectMode(t *testing.T) {
	srv := seed(t)
	lcfg := searchProvider(srv)
	lcfg.Providers[0].BindDN = ""
	lcfg.Providers[0].BindPassword = ""
	client, links, db := newClient(t, lcfg, nil)
	addLink(t, links, db, bobAddr, "corp", bobDN)

	ok, err := client.Authenticate(context.Background(), bobAddr, "bob-pass")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok {
		t.Fatal("expected the directory to accept the password")
	}
	if n := srv.Binds(); n != 1 {
		t.Fatalf("binds = %d, want 1 (no service account in direct mode)", n)
	}
	if n := srv.Searches(); n != 0 {
		t.Fatalf("searches = %d, want 0", n)
	}

	ok, err = client.Authenticate(context.Background(), bobAddr, "nope")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok {
		t.Fatal("expected the directory to refuse the password")
	}
	if n := srv.Binds(); n != 2 {
		t.Fatalf("binds = %d, want 2", n)
	}
}

func TestAuthenticateCachesLookup(t *testing.T) {
	srv := seed(t)
	lcfg := searchProvider(srv)
	lcfg.CacheTTL = time.Hour
	client, links, db := newClient(t, lcfg, nil)
	addLink(t, links, db, aliceAddr, "corp", aliceDN)

	for i := 0; i < 2; i++ {
		ok, err := client.Authenticate(context.Background(), aliceAddr, "alice-pass")
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if !ok {
			t.Fatal("expected the directory to accept the password")
		}
	}
	if n := srv.Searches(); n != 1 {
		t.Fatalf("searches = %d, want 1 (second lookup served from cache)", n)
	}
	if n := srv.Binds(); n != 3 {
		t.Fatalf("binds = %d, want 3 (service, user, user)", n)
	}
}

func TestAuthenticateCacheExpires(t *testing.T) {
	srv := seed(t)
	lcfg := searchProvider(srv)
	lcfg.CacheTTL = time.Minute

	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	client, links, db := newClient(t, lcfg, clock)
	addLink(t, links, db, aliceAddr, "corp", aliceDN)

	if ok, err := client.Authenticate(context.Background(), aliceAddr, "alice-pass"); err != nil || !ok {
		t.Fatalf("Authenticate = %v, %v; want success", ok, err)
	}
	now = now.Add(2 * time.Minute) // past the TTL

	if ok, err := client.Authenticate(context.Background(), aliceAddr, "alice-pass"); err != nil || !ok {
		t.Fatalf("Authenticate = %v, %v; want success", ok, err)
	}
	if n := srv.Searches(); n != 2 {
		t.Fatalf("searches = %d, want 2 after the cache expired", n)
	}
}

func TestAuthenticateUnlinkedAddress(t *testing.T) {
	srv := seed(t)
	client, _, _ := newClient(t, searchProvider(srv), nil)

	ok, err := client.Authenticate(context.Background(), aliceAddr, "alice-pass")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok {
		t.Fatal("expected refusal for an address the directory does not back")
	}
	if n := srv.Binds(); n != 0 {
		t.Fatalf("binds = %d, want 0: no directory was consulted", n)
	}
}

func TestAuthenticateUnknownProvider(t *testing.T) {
	srv := seed(t)
	client, links, db := newClient(t, searchProvider(srv), nil)
	addLink(t, links, db, aliceAddr, "ghost", aliceDN)

	ok, err := client.Authenticate(context.Background(), aliceAddr, "alice-pass")
	if err == nil {
		t.Fatal("expected an error for a provider the configuration no longer has")
	}
	if ok {
		t.Fatal("expected refusal")
	}
}

func TestAuthenticateDirectoryDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	lcfg := config.LDAP{Providers: []config.LDAPProvider{{
		Name:         "corp",
		URL:          "ldap://" + addr,
		BindDN:       svcDN,
		BindPassword: "svc-pass",
		BaseDN:       baseDN,
		UserFilter:   "(&(objectClass=user)(mail={{username}}))",
		Timeout:      2 * time.Second,
	}}}
	client, links, db := newClient(t, lcfg, nil)
	addLink(t, links, db, aliceAddr, "corp", aliceDN)

	start := time.Now()
	ok, err := client.Authenticate(context.Background(), aliceAddr, "alice-pass")
	if err == nil {
		t.Fatal("expected an error while the directory is down")
	}
	if ok {
		t.Fatal("expected refusal")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("gave up after %v; the dial must honour the timeout", d)
	}
}

func TestAuthenticateServiceAccountBroken(t *testing.T) {
	srv := seed(t)
	lcfg := searchProvider(srv)
	lcfg.Providers[0].BindPassword = "stale"
	client, links, db := newClient(t, lcfg, nil)
	addLink(t, links, db, aliceAddr, "corp", aliceDN)

	ok, err := client.Authenticate(context.Background(), aliceAddr, "alice-pass")
	if err == nil {
		t.Fatal("expected an error for a service account the directory no longer accepts")
	}
	if ok {
		t.Fatal("expected refusal")
	}
}

func TestAuthenticateEntryGone(t *testing.T) {
	srv := seed(t)
	srv.RemoveUser(aliceDN)
	client, links, db := newClient(t, searchProvider(srv), nil)
	addLink(t, links, db, aliceAddr, "corp", aliceDN)

	ok, err := client.Authenticate(context.Background(), aliceAddr, "alice-pass")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ok {
		t.Fatal("expected refusal for an entry the directory no longer has")
	}
}

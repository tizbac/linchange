package ldapsync_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/ldapsync"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
	"github.com/linchange/linchange/internal/testldap"
)

const (
	baseDN   = "dc=corp,dc=example,dc=com"
	svcDN    = "cn=svc-linchange,ou=service," + baseDN
	aliceDN  = "cn=alice,ou=users," + baseDN
	bobDN    = "cn=bob,ou=users," + baseDN
	carolDN  = "cn=carol,ou=users," + baseDN
	daveDN   = "cn=dave,ou=users," + baseDN
	erinDN   = "cn=erin,ou=users," + baseDN
	adminsDN = "cn=domain admins,ou=groups," + baseDN

	// One domain this server hosts, and the addresses inside and outside it.
	hostedDomain = "corp.example.com"
	aliceAddr    = "alice@corp.example.com"
	erinAddr     = "erin@corp.example.com"
	bobAddr      = "bob@elsewhere.example.net"
	carolAddr    = "carol@elsewhere.example.net"
	daveAddr     = "dave@elsewhere.example.net"
)

// seed builds a directory holding one hosted user, two off-domain admins who
// arrive by different routes, and one off-domain stranger.
func seed(t *testing.T) *testldap.Server {
	t.Helper()
	srv := testldap.Start(t)

	// The service account is deliberately not an objectClass=user, so the
	// reconcile's user filter does not read it as a person to provision.
	srv.AddUser(testldap.Entry{
		DN: svcDN, Password: "svc-pass",
		Attributes: map[string][]string{"objectclass": {"service"}},
	})
	srv.AddUser(testldap.Entry{
		DN: aliceDN, Password: "alice-pass",
		Attributes: map[string][]string{
			"objectclass": {"user"}, "mail": {aliceAddr}, "displayname": {"Alice"},
		},
	})
	// Off-domain, an admin by its own memberOf claim.
	srv.AddUser(testldap.Entry{
		DN: bobDN, Password: "bob-pass",
		Attributes: map[string][]string{
			"objectclass": {"user"}, "mail": {bobAddr},
			"displayname": {"Bob"}, "memberof": {adminsDN},
		},
	})
	// Off-domain, an admin only because the group lists it.
	srv.AddUser(testldap.Entry{
		DN: carolDN, Password: "carol-pass",
		Attributes: map[string][]string{
			"objectclass": {"user"}, "mail": {carolAddr}, "displayname": {"Carol"},
		},
	})
	// Off-domain and in no group: refused.
	srv.AddUser(testldap.Entry{
		DN: daveDN, Password: "dave-pass",
		Attributes: map[string][]string{
			"objectclass": {"user"}, "mail": {daveAddr}, "displayname": {"Dave"},
		},
	})
	srv.AddUser(testldap.Entry{
		DN: adminsDN,
		Attributes: map[string][]string{
			"objectclass": {"group"},
			// bob is absent here on purpose: his admin claim comes from
			// memberOf, which is the other direction a directory reports.
			"member": {carolDN},
		},
	})
	return srv
}

// corpConfig is the provider a hosted example.com domain is reconciled from.
func corpConfig(srv *testldap.Server) config.LDAP {
	return config.LDAP{
		Providers: []config.LDAPProvider{{
			Name:                "corp",
			URL:                 srv.URL(),
			BindDN:              svcDN,
			BindPassword:        "svc-pass",
			BaseDN:              baseDN,
			UserFilter:          "(&(objectClass=user)(mail={{username}}))",
			DomainAdminsGroupDN: adminsDN,
			// The default domain has to be one this server hosts, or an entry
			// with no mail attribute lands off-domain and is refused.
			DefaultDomain:     hostedDomain,
			MailboxQuotaBytes: 1 << 30,
			Timeout:           5 * time.Second,
			Attributes: config.LDAPAttributes{
				Mail:        []string{"mail"},
				DisplayName: []string{"displayName"},
				MemberOf:    []string{"memberOf"},
			},
		}},
		Sync: config.LDAPSync{
			Enabled:             true,
			Interval:            time.Hour,
			DisableMissingAfter: time.Hour,
		},
	}
}

// newSyncer wires a syncer over a migrated database that hosts exactly one
// domain, which is what decides who gets mail.
func newSyncer(t *testing.T, cfg config.LDAP, now func() time.Time) (*ldapsync.Syncer, *sqlx.DB) {
	t.Helper()
	db := testdb.Migrated(t)
	if err := store.NewDomains().Create(context.Background(), db,
		store.CreateDomainRequest{Name: hostedDomain}); err != nil {
		t.Fatalf("creating the hosted domain: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return ldapsync.New(cfg, db, log, now), db
}

var at = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func state(t *testing.T, db *sqlx.DB) store.SyncState {
	t.Helper()
	st, err := store.NewLDAPSyncState().Get(context.Background(), db, "corp")
	if err != nil {
		t.Fatalf("reading the sync state: %v", err)
	}
	return st
}

func user(t *testing.T, db *sqlx.DB, address string) store.User {
	t.Helper()
	u, err := store.NewUsers().Get(context.Background(), db, address)
	if err != nil {
		t.Fatalf("reading %s: %v", address, err)
	}
	return u
}

// TestReconcileProvision is the ordinary case: a directory user in a domain
// this server hosts becomes a mailbox, and stays one.
func TestReconcileProvision(t *testing.T) {
	srv := seed(t)
	cfg := corpConfig(srv)
	s, db := newSyncer(t, cfg, func() time.Time { return at })
	ctx := context.Background()

	if err := s.Reconcile(ctx, cfg.Providers[0]); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	u := user(t, db, aliceAddr)
	if u.Source != policy.SourceLDAP {
		t.Errorf("source = %q, want %q", u.Source, policy.SourceLDAP)
	}
	if !u.MailboxEnabled {
		t.Error("a hosted directory user must have a mailbox")
	}
	if !u.Enabled {
		t.Error("a provisioned user must be enabled")
	}
	if u.DisplayName != "Alice" {
		t.Errorf("display name = %q, want Alice", u.DisplayName)
	}
	if u.QuotaBytes == nil || *u.QuotaBytes != 1<<30 {
		t.Errorf("quota = %v, want %d", u.QuotaBytes, 1<<30)
	}

	link, err := store.NewLDAPLinks().Get(ctx, db, aliceAddr)
	if err != nil {
		t.Fatalf("reading the link: %v", err)
	}
	if link.Provider != "corp" || link.DN != aliceDN {
		t.Errorf("link = %+v, want provider corp and dn %s", link, aliceDN)
	}

	// The first pass provisions everyone it should: alice in the hosted domain,
	// bob and carol as domain admins off it. Dave is refused.
	st := state(t, db)
	if st.EntriesAdded != 3 {
		t.Errorf("entries added = %d, want 3 (alice, bob, carol)", st.EntriesAdded)
	}
	if st.EntriesUpdated != 0 {
		t.Errorf("entries updated = %d, want 0", st.EntriesUpdated)
	}
	if st.LastSuccessAt == nil {
		t.Error("a successful run must record a success time")
	}
	if st.LastError != "" {
		t.Errorf("last error = %q, want empty", st.LastError)
	}

	// A second pass over an unchanged directory creates nobody and disables
	// nobody. Only alice counts as updated: the off-domain admins are already
	// enabled, so refreshing them is a no-op rather than a change.
	if err := s.Reconcile(ctx, cfg.Providers[0]); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	st = state(t, db)
	if st.EntriesAdded != 0 {
		t.Errorf("entries added on the second run = %d, want 0", st.EntriesAdded)
	}
	if st.EntriesUpdated != 1 {
		t.Errorf("entries updated on the second run = %d, want 1 (only alice)", st.EntriesUpdated)
	}
	if st.EntriesDisabled != 0 {
		t.Errorf("entries disabled = %d, want 0", st.EntriesDisabled)
	}
}

// TestReconcileOffDomainAdmin covers the two directions a directory reports
// group membership in: the entry claims it, or the group lists it. Both land on
// a domain_admin, which administers mail and never receives it.
func TestReconcileOffDomainAdmin(t *testing.T) {
	srv := seed(t)
	cfg := corpConfig(srv)
	s, db := newSyncer(t, cfg, func() time.Time { return at })

	if err := s.Reconcile(context.Background(), cfg.Providers[0]); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, addr := range []string{bobAddr, carolAddr} {
		u := user(t, db, addr)
		if u.Source != policy.SourceDomainAdmin {
			t.Errorf("%s source = %q, want %q", addr, u.Source, policy.SourceDomainAdmin)
		}
		if u.MailboxEnabled {
			t.Errorf("%s must not receive mail", addr)
		}
		if !u.Enabled {
			t.Errorf("%s must be enabled", addr)
		}
	}
}

// TestReconcileOffDomainStranger is the refusal case. An off-domain user in no
// admin group gets nothing at all, and one that an earlier, laxer configuration
// linked loses both its access and its link.
func TestReconcileOffDomainStranger(t *testing.T) {
	srv := seed(t)
	cfg := corpConfig(srv)
	s, db := newSyncer(t, cfg, func() time.Time { return at })
	ctx := context.Background()

	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: daveAddr, Source: policy.SourceLDAP,
	}); err != nil {
		t.Fatalf("creating the pre-existing user: %v", err)
	}
	if err := store.NewLDAPLinks().Upsert(ctx, db, store.LDAPLink{
		Address: daveAddr, Provider: "corp", DN: daveDN,
	}, at.Add(-2*time.Hour)); err != nil {
		t.Fatalf("creating the pre-existing link: %v", err)
	}

	if err := s.Reconcile(ctx, cfg.Providers[0]); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if u := user(t, db, daveAddr); u.Enabled {
		t.Error("an off-domain non-admin must be disabled")
	}
	if _, err := store.NewLDAPLinks().Get(ctx, db, daveAddr); err == nil {
		t.Error("the link of a refused user must be removed")
	}
	if st := state(t, db); st.EntriesDisabled != 1 {
		t.Errorf("entries disabled = %d, want 1", st.EntriesDisabled)
	}
}

// TestReconcileOffDomainStrangerUnlinked proves the refusal creates nothing: a
// stranger with no prior link is simply not provisioned.
func TestReconcileOffDomainStrangerUnlinked(t *testing.T) {
	srv := seed(t)
	cfg := corpConfig(srv)
	s, db := newSyncer(t, cfg, func() time.Time { return at })
	ctx := context.Background()

	if err := s.Reconcile(ctx, cfg.Providers[0]); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := store.NewUsers().Get(ctx, db, daveAddr); err == nil {
		t.Error("an off-domain non-admin must not be provisioned")
	}
	if st := state(t, db); st.EntriesAdded != 3 {
		t.Errorf("entries added = %d, want 3 (alice, bob, carol)", st.EntriesAdded)
	}
}

// TestReconcileMissingGrace is the liveness property: a principal is only
// disabled once it has been absent long enough, keeps its link so the reason is
// recorded, and is restored when the directory reports it again.
func TestReconcileMissingGrace(t *testing.T) {
	srv := seed(t)
	srv.AddUser(testldap.Entry{
		DN: erinDN, Password: "erin-pass",
		Attributes: map[string][]string{
			"objectclass": {"user"}, "mail": {erinAddr}, "displayname": {"Erin"},
		},
	})

	clock := at
	cfg := corpConfig(srv)
	s, db := newSyncer(t, cfg, func() time.Time { return clock })
	ctx := context.Background()

	if err := s.Reconcile(ctx, cfg.Providers[0]); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if u := user(t, db, erinAddr); !u.Enabled {
		t.Fatal("erin should have been provisioned and enabled")
	}

	srv.RemoveUser(erinDN)

	// Still inside the grace: nothing may change.
	clock = at.Add(30 * time.Minute)
	if err := s.Reconcile(ctx, cfg.Providers[0]); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if u := user(t, db, erinAddr); !u.Enabled {
		t.Fatal("erin must not be disabled inside the grace period")
	}

	// Past the grace: disabled, with the link retained.
	clock = at.Add(2 * time.Hour)
	if err := s.Reconcile(ctx, cfg.Providers[0]); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if u := user(t, db, erinAddr); u.Enabled {
		t.Error("erin must be disabled once the grace has passed")
	}
	if _, err := store.NewLDAPLinks().Get(ctx, db, erinAddr); err != nil {
		t.Errorf("the link of a disabled principal must be kept, so the reason is recorded: %v", err)
	}

	// The directory reports her again: back in business.
	srv.AddUser(testldap.Entry{
		DN: erinDN, Password: "erin-pass",
		Attributes: map[string][]string{
			"objectclass": {"user"}, "mail": {erinAddr}, "displayname": {"Erin"},
		},
	})
	if err := s.Reconcile(ctx, cfg.Providers[0]); err != nil {
		t.Fatalf("fourth reconcile: %v", err)
	}
	if u := user(t, db, erinAddr); !u.Enabled {
		t.Error("erin must be enabled again once the directory reports her")
	}
}

// TestReconcileFailureIsRecorded is the operator-facing half: a directory that
// cannot be read leaves a trace and does not erase the memory of the last good
// run.
func TestReconcileFailureIsRecorded(t *testing.T) {
	srv := seed(t)
	cfg := corpConfig(srv)
	s, db := newSyncer(t, cfg, func() time.Time { return at })
	ctx := context.Background()

	if err := s.Reconcile(ctx, cfg.Providers[0]); err != nil {
		t.Fatalf("the good run should succeed: %v", err)
	}

	cfg.Providers[0].BindPassword = "stale"
	if err := s.Reconcile(ctx, cfg.Providers[0]); err == nil {
		t.Fatal("a bad bind password must fail the reconcile")
	}

	st := state(t, db)
	if st.LastError == "" {
		t.Error("a failed run must record its error")
	}
	if st.LastSuccessAt == nil || !st.LastSuccessAt.Equal(at) {
		t.Errorf("last success = %v, want the earlier good run at %v", st.LastSuccessAt, at)
	}
	if !st.LastRunAt.Equal(at) {
		t.Errorf("last run = %v, want %v", st.LastRunAt, at)
	}
}

// TestReconcileWithoutBindDN is the configuration guard: a provider that binds
// people as themselves cannot be enumerated, and says so.
func TestReconcileWithoutBindDN(t *testing.T) {
	srv := seed(t)
	cfg := corpConfig(srv)
	cfg.Providers[0].BindDN = ""
	s, _ := newSyncer(t, cfg, func() time.Time { return at })

	if err := s.Reconcile(context.Background(), cfg.Providers[0]); err == nil {
		t.Fatal("a provider with no bind DN must not pretend to reconcile")
	}
}

// TestReconcileMailAddress covers the two decisions about what an entry's
// address is: an entry with no mail attribute at all, and one that lists an
// alias alongside the address mail actually goes to.
func TestReconcileMailAddress(t *testing.T) {
	srv := testldap.Start(t)
	srv.AddUser(testldap.Entry{
		DN: svcDN, Password: "svc-pass",
		Attributes: map[string][]string{"objectclass": {"service"}},
	})
	// No mail attribute at all: the DN leaf under the provider's default
	// domain, which is what lets a directory that publishes no mail attribute
	// still provision anybody.
	srv.AddUser(testldap.Entry{
		DN:       "cn=nomail,ou=users," + baseDN,
		Password: "nomail-pass",
		Attributes: map[string][]string{
			"objectclass": {"user"}, "displayname": {"No Mail"},
		},
	})
	// An alias and the prefixed primary. The prefixed one is the address mail
	// goes to, so it is the principal that should exist.
	srv.AddUser(testldap.Entry{
		DN:       "cn=aliases,ou=users," + baseDN,
		Password: "aliases-pass",
		Attributes: map[string][]string{
			"objectclass": {"user"},
			"mail":        {"alias@corp.example.com", "SMTP:primary@corp.example.com"},
		},
	})

	cfg := corpConfig(srv)
	s, db := newSyncer(t, cfg, func() time.Time { return at })
	ctx := context.Background()
	if err := s.Reconcile(ctx, cfg.Providers[0]); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if u := user(t, db, "nomail@"+hostedDomain); !u.MailboxEnabled {
		t.Error("an entry with no mail attribute should fall back to the DN leaf and the default domain")
	}
	if u := user(t, db, "primary@corp.example.com"); u.Address != "primary@corp.example.com" {
		t.Errorf("address = %q", u.Address)
	}
	if _, err := store.NewUsers().Get(ctx, db, "alias@corp.example.com"); err == nil {
		t.Error("a bare alias must not become a principal of its own when an SMTP: primary is listed")
	}
}

// TestStripUsernameAssertion is the filter surgery the reconcile depends on.
func TestStripUsernameAssertion(t *testing.T) {
	cases := []struct {
		name, in, want, wantErr string
	}{
		{
			name: "an and keeps its other assertions",
			in:   "(&(objectClass=user)(mail={{username}}))",
			want: "(&(objectClass=user))",
		},
		{
			name: "an or drops only the named assertion",
			in:   "(|(mail={{username}})(objectClass=group))",
			want: "(|(objectClass=group))",
		},
		{
			name: "a nested group survives whole",
			in:   "(&(objectClass=user)(|(a=1)(b=2))(mail={{username}}))",
			want: "(&(objectClass=user)(|(a=1)(b=2)))",
		},
		{
			name: "a nested group holding the placeholder goes with it",
			in:   "(&(objectClass=user)(!(disabled={{username}})))",
			want: "(&(objectClass=user))",
		},
		{
			name: "a filter with no placeholder is untouched",
			in:   "(objectClass=user)",
			want: "(objectClass=user)",
		},
		{
			name:    "a bare assertion has no shape to search with",
			in:      "(mail={{username}})",
			wantErr: "filter is not an AND or OR group",
		},
		{
			name:    "a lone placeholder leaves nothing behind",
			in:      "(&(mail={{username}}))",
			wantErr: "no assertion survives the removal of {{username}}",
		},
		{
			name:    "no surrounding group at all",
			in:      "mail={{username}}",
			wantErr: "filter is not a group",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ldapsync.StripUsernameAssertion(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("got %q, want the error %q", got, tc.wantErr)
				}
				if err.Error() != tc.wantErr {
					t.Errorf("error = %q, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStartStop is the scheduler: starting runs once and keeps running, and
// stopping is idempotent and waits for the pass in flight.
func TestStartStop(t *testing.T) {
	srv := seed(t)
	cfg := corpConfig(srv)
	cfg.Sync.Interval = 200 * time.Millisecond
	s, db := newSyncer(t, cfg, nil)
	ctx := context.Background()

	s.Start(ctx)
	time.Sleep(700 * time.Millisecond)
	s.Stop()
	s.Stop()

	if _, err := store.NewUsers().Get(ctx, db, aliceAddr); err != nil {
		t.Errorf("the scheduled reconcile should have provisioned the user: %v", err)
	}
}

// TestStartWithoutSync is the disabled case: nothing runs, and stopping a
// syncer that never started is not an error.
func TestStartWithoutSync(t *testing.T) {
	srv := seed(t)
	cfg := corpConfig(srv)
	cfg.Sync.Enabled = false
	s, db := newSyncer(t, cfg, nil)

	s.Start(context.Background())
	s.Stop()

	if _, err := store.NewUsers().Get(context.Background(), db, aliceAddr); err == nil {
		t.Error("a disabled sync must not reconcile")
	}
}

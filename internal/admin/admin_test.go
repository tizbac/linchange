package admin_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/admin"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

const testPassword = "correct-horse-battery"

// testArgon2 is deliberately the cheapest parameter set the package allows, so
// a suite that creates accounts does not spend its time hashing.
func testArgon2() config.Argon2Params {
	return config.Argon2Params{
		MemoryKib:   8 * 1024,
		Iterations:  1,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

func newService(t *testing.T) (*admin.Service, *sqlx.DB) {
	t.Helper()
	db := testdb.Migrated(t)
	svc, err := admin.New(admin.Options{
		DB:                db,
		Argon2:            testArgon2(),
		MinPasswordLength: 12,
		Policy: policy.Policy{
			Impersonation: policy.Impersonation{Allowed: true, RequireReason: true},
		},
	})
	if err != nil {
		t.Fatalf("building the admin service: %v", err)
	}
	return svc, db
}

// adminPrincipal is a signed-in domain admin, which is the only kind of caller
// this service accepts.
func adminPrincipal(address string) policy.Principal {
	return policy.Principal{
		Address:       address,
		Source:        policy.SourceDomainAdmin,
		Enabled:       true,
		Authenticated: true,
	}
}

func personPrincipal(address string) policy.Principal {
	return policy.Principal{
		Address:        address,
		Source:         policy.SourceLocal,
		Enabled:        true,
		MailboxEnabled: true,
		Authenticated:  true,
	}
}

func sharedPrincipal(address string) policy.Principal {
	return policy.Principal{
		Address:        address,
		Source:         policy.SourceShared,
		Enabled:        true,
		MailboxEnabled: true,
		Authenticated:  true,
	}
}

// seedDomainAndAdmin creates the domain and the administrator that every test
// signs in as, because an administrator without a domain has nothing to
// administer.
func seedDomainAndAdmin(t *testing.T, svc *admin.Service) policy.Principal {
	t.Helper()
	ctx := context.Background()
	adm := adminPrincipal("root@example.com")
	if _, err := svc.CreateDomain(ctx, adm, store.CreateDomainRequest{Name: "example.com", IsLocal: true}); err != nil {
		t.Fatalf("creating the domain: %v", err)
	}
	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{Address: adm.Address, Source: policy.SourceDomainAdmin, DisplayName: "Root"}); err != nil {
		t.Fatalf("creating the administrator: %v", err)
	}
	return adm
}

// --- authorization ---

// An ordinary user cannot reach the administration surface. This is the single
// most important property of the package, so it is checked against every kind
// of principal rather than once.
func TestAdministrationRefusesNonAdmins(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{Address: "bob@example.com", Source: policy.SourceLocal, Password: testPassword}); err != nil {
		t.Fatalf("seeding a user: %v", err)
	}

	cases := []struct {
		name  string
		actor policy.Principal
	}{
		{"an ordinary user", personPrincipal("bob@example.com")},
		{"a shared principal", sharedPrincipal("support@example.com")},
		{"an unauthenticated caller", policy.Principal{Address: "bob@example.com", Source: policy.SourceLocal}},
		{"a disabled admin", policy.Principal{
			Address: "root@example.com", Source: policy.SourceDomainAdmin, Authenticated: true,
		}},
		{"nobody at all", policy.Principal{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := svc.ListUsers(ctx, c.actor); !errors.Is(err, admin.ErrDenied) {
				t.Errorf("ListUsers = %v, want ErrDenied", err)
			}
			if _, err := svc.ListDomains(ctx, c.actor); !errors.Is(err, admin.ErrDenied) {
				t.Errorf("ListDomains = %v, want ErrDenied", err)
			}
			if _, err := svc.ListAliases(ctx, c.actor); !errors.Is(err, admin.ErrDenied) {
				t.Errorf("ListAliases = %v, want ErrDenied", err)
			}
			if _, err := svc.ListSharedMailboxes(ctx, c.actor); !errors.Is(err, admin.ErrDenied) {
				t.Errorf("ListSharedMailboxes = %v, want ErrDenied", err)
			}
			if _, err := svc.ListAudit(ctx, c.actor, store.AuditFilter{}); !errors.Is(err, admin.ErrDenied) {
				t.Errorf("ListAudit = %v, want ErrDenied", err)
			}
			// And a mutation, which is the one that would matter.
			_, err := svc.CreateUser(ctx, c.actor, admin.CreateUserRequest{
				Address: "intruder@example.com", Source: policy.SourceLocal, Password: testPassword,
			})
			if !errors.Is(err, admin.ErrDenied) {
				t.Errorf("CreateUser = %v, want ErrDenied", err)
			}
		})
	}
}

// A denied call must not have written anything, or the refusal is only half
// true.
func TestDeniedAdministrationWritesNothing(t *testing.T) {
	svc, db := newService(t)
	ctx := context.Background()
	seedDomainAndAdmin(t, svc)

	person := personPrincipal("bob@example.com")
	if _, err := svc.CreateUser(ctx, person, admin.CreateUserRequest{
		Address: "intruder@example.com", Source: policy.SourceLocal, Password: testPassword,
	}); !errors.Is(err, admin.ErrDenied) {
		t.Fatalf("CreateUser by a user = %v, want ErrDenied", err)
	}

	var n int
	if err := db.GetContext(ctx, &n,
		`SELECT count(*) FROM users WHERE address = 'intruder@example.com'`); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if n != 0 {
		t.Errorf("a refused call created %d users, want none", n)
	}
}

// --- user provisioning ---

func TestCreateUserStoresThePasswordAndRecordsTheChange(t *testing.T) {
	svc, db := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	user, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "bob@example.com", Source: policy.SourceLocal, DisplayName: "Bob", Password: testPassword,
	})
	if err != nil {
		t.Fatalf("creating a user: %v", err)
	}
	if user.Source != policy.SourceLocal {
		t.Errorf("source = %q, want local", user.Source)
	}
	// An administrator choosing somebody else's password has no way to know
	// whether that user can keep it, so the account must change it.
	if !user.MustChangePassword {
		t.Error("must_change_password = false, want true for a new local account")
	}

	has, err := store.NewCredentials().Has(ctx, db, "bob@example.com")
	if err != nil {
		t.Fatalf("checking for a password: %v", err)
	}
	if !has {
		t.Error("the account has no password")
	}

	entries := mustAudit(t, db, "user.create", "bob@example.com")
	if len(entries) != 1 {
		t.Fatalf("got %d user.create entries, want 1", len(entries))
	}
	if entries[0].ActorAddress == nil || *entries[0].ActorAddress != adm.Address {
		t.Errorf("actor = %v, want %q", entries[0].ActorAddress, adm.Address)
	}
	if entries[0].TargetAddress == nil || *entries[0].TargetAddress != "bob@example.com" {
		t.Errorf("target = %v, want the new user", entries[0].TargetAddress)
	}
}

// The password must be stored hashed, and the plaintext must not be anywhere
// the audit detail or the response can carry it.
func TestCreateUserDoesNotLeakThePassword(t *testing.T) {
	svc, db := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "bob@example.com", Source: policy.SourceLocal, Password: testPassword,
	}); err != nil {
		t.Fatalf("creating a user: %v", err)
	}

	var hash string
	if err := db.GetContext(ctx, &hash,
		`SELECT password_hash FROM credentials WHERE address = 'bob@example.com'`); err != nil {
		t.Fatalf("reading the stored hash: %v", err)
	}
	if hash == testPassword {
		t.Fatal("the password was stored in the clear")
	}
	for _, entry := range mustAudit(t, db, "user.create", "bob@example.com") {
		if strings.Contains(string(entry.Detail), testPassword) {
			t.Errorf("the password appears in the audit detail: %s", entry.Detail)
		}
	}
}

// A local account with no usable password is not a state this can produce: the
// hash and the row are written together or not at all.
func TestCreateUserRejectsABadPassword(t *testing.T) {
	svc, db := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	for _, pw := range []string{"", "short"} {
		_, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
			Address: "bob@example.com", Source: policy.SourceLocal, Password: pw,
		})
		if !errors.Is(err, admin.ErrInvalid) {
			t.Errorf("CreateUser with password %q = %v, want ErrInvalid", pw, err)
		}
	}

	// Nothing was left behind by either attempt.
	var n int
	if err := db.GetContext(ctx, &n,
		`SELECT count(*) FROM users WHERE address = 'bob@example.com'`); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if n != 0 {
		t.Errorf("a refused create left %d users behind", n)
	}
}

// A directory principal is authenticated elsewhere, so demanding a password
// would be wrong in the other direction.
func TestCreateDirectoryUserNeedsNoPassword(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "ldap@example.com", Source: policy.SourceLDAP, Password: "",
	}); err != nil {
		t.Fatalf("creating a directory user without a password: %v", err)
	}
}

func TestCreateUserRejectsAMalformedAddress(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	for _, address := range []string{"", "   ", "not-an-address", "bob@example.com "} {
		_, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
			Address: address, Source: policy.SourceLocal, Password: testPassword,
		})
		if !errors.Is(err, admin.ErrInvalid) {
			t.Errorf("CreateUser with address %q = %v, want ErrInvalid", address, err)
		}
	}
}

// Disabling an account has to end its live sessions, or an access token already
// in flight keeps working and the switch is not a control.
func TestDisablingAUserEndsTheirSessions(t *testing.T) {
	svc, db := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "bob@example.com", Source: policy.SourceLocal, Password: testPassword,
	}); err != nil {
		t.Fatalf("creating a user: %v", err)
	}
	session := seedLiveSession(t, db, "bob@example.com")

	if err := svc.SetUserEnabled(ctx, adm, "bob@example.com", false); err != nil {
		t.Fatalf("disabling the user: %v", err)
	}
	if _, err := store.NewSessions().Load(ctx, db, session); err == nil {
		t.Error("the session survived the account being disabled")
	}
	if len(mustAudit(t, db, "user.disable", "bob@example.com")) != 1 {
		t.Error("disabling an account was not recorded")
	}
}

// Changing somebody's password must invalidate what was signed with the old
// one, for the same reason.
func TestResetPasswordEndsSessions(t *testing.T) {
	svc, db := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "bob@example.com", Source: policy.SourceLocal, Password: testPassword,
	}); err != nil {
		t.Fatalf("creating a user: %v", err)
	}
	session := seedLiveSession(t, db, "bob@example.com")

	if err := svc.ResetUserPassword(ctx, adm, "bob@example.com", "a-brand-new-password"); err != nil {
		t.Fatalf("resetting the password: %v", err)
	}
	if _, err := store.NewSessions().Load(ctx, db, session); err == nil {
		t.Error("the session survived the password being reset")
	}
	if len(mustAudit(t, db, "user.reset_password", "bob@example.com")) != 1 {
		t.Error("the password reset was not recorded")
	}
}

func TestSetUserQuotaRecordsTheChange(t *testing.T) {
	svc, db := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "bob@example.com", Source: policy.SourceLocal, Password: testPassword,
	}); err != nil {
		t.Fatalf("creating a user: %v", err)
	}

	quota := int64(1024 * 1024)
	if err := svc.SetUserQuota(ctx, adm, "bob@example.com", &quota); err != nil {
		t.Fatalf("setting the quota: %v", err)
	}
	view, err := svc.GetUser(ctx, adm, "bob@example.com")
	if err != nil {
		t.Fatalf("reading the user: %v", err)
	}
	if view.QuotaBytes == nil || *view.QuotaBytes != quota {
		t.Errorf("quota = %v, want %d", view.QuotaBytes, quota)
	}
	if len(mustAudit(t, db, "user.quota", "bob@example.com")) != 1 {
		t.Error("the quota change was not recorded")
	}
}

func TestListUsersReportsUsage(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "bob@example.com", Source: policy.SourceLocal, Password: testPassword,
	}); err != nil {
		t.Fatalf("creating a user: %v", err)
	}
	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "carol@example.com", Source: policy.SourceLocal, Password: testPassword,
	}); err != nil {
		t.Fatalf("creating a second user: %v", err)
	}

	users, err := svc.ListUsers(ctx, adm)
	if err != nil {
		t.Fatalf("listing users: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("got %d users, want the two seeded plus the administrator: %+v", len(users), users)
	}
	for _, u := range users {
		if u.QuotaUsedBytes != 0 || u.QuotaMessageCount != 0 {
			t.Errorf("%s reports %d bytes in %d messages, want zero",
				u.Address, u.QuotaUsedBytes, u.QuotaMessageCount)
		}
	}
}

// --- shared mailboxes and impersonation ---

func TestGrantSharedMailboxAccessRefusesASelfGrant(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "support@example.com", Source: policy.SourceShared,
	}); err != nil {
		t.Fatalf("creating the shared principal: %v", err)
	}
	if _, err := svc.DeclareSharedMailbox(ctx, adm, store.CreateSharedMailboxRequest{Address: "support@example.com"}); err != nil {
		t.Fatalf("declaring the shared mailbox: %v", err)
	}

	err := svc.GrantSharedMailboxAccess(ctx, adm, "support@example.com", "support@example.com", policy.GrantOwner)
	if !errors.Is(err, admin.ErrInvalid) {
		t.Fatalf("self-grant = %v, want ErrInvalid", err)
	}
}

func TestDeclareSharedMailboxRejectsABadColour(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "support@example.com", Source: policy.SourceShared,
	}); err != nil {
		t.Fatalf("creating the shared principal: %v", err)
	}

	for _, colour := range []string{"red", "#fff", "#gggggg"} {
		_, err := svc.DeclareSharedMailbox(ctx, adm, store.CreateSharedMailboxRequest{
			Address: "support@example.com", Colour: &colour,
		})
		if !errors.Is(err, admin.ErrInvalid) {
			t.Errorf("colour %q = %v, want ErrInvalid", colour, err)
		}
	}

	good := "#0b7285"
	if _, err := svc.DeclareSharedMailbox(ctx, adm, store.CreateSharedMailboxRequest{
		Address: "support@example.com", Colour: &good,
	}); err != nil {
		t.Errorf("a valid colour was refused: %v", err)
	}
}

func TestGrantImpersonationRequiresAReason(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "bob@example.com", Source: policy.SourceLocal, Password: testPassword,
	}); err != nil {
		t.Fatalf("creating a user: %v", err)
	}

	err := svc.GrantImpersonation(ctx, adm, admin.GrantImpersonationRequest{
		Actor: adm.Address, Target: "bob@example.com",
	})
	if !errors.Is(err, admin.ErrInvalid) {
		t.Fatalf("a grant with no reason = %v, want ErrInvalid", err)
	}

	ttl := 24 * time.Hour
	if err := svc.GrantImpersonation(ctx, adm, admin.GrantImpersonationRequest{
		Actor: adm.Address, Target: "bob@example.com", Reason: "a support incident", TTL: &ttl,
	}); err != nil {
		t.Fatalf("granting with a reason: %v", err)
	}

	grants, err := svc.ListImpersonationGrants(ctx, adm, adm.Address)
	if err != nil {
		t.Fatalf("listing grants: %v", err)
	}
	if len(grants) != 1 || !grants[0].Active {
		t.Fatalf("grants = %+v, want one live grant", grants)
	}
}

// The power being granted is the power to administer, so it cannot be handed
// to somebody who does not hold it.
func TestGrantImpersonationRefusesANonAdmin(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "bob@example.com", Source: policy.SourceLocal, Password: testPassword,
	}); err != nil {
		t.Fatalf("creating a user: %v", err)
	}

	err := svc.GrantImpersonation(ctx, adm, admin.GrantImpersonationRequest{
		Actor: "bob@example.com", Target: "bob@example.com", Reason: "self service",
	})
	if !errors.Is(err, admin.ErrInvalid) {
		t.Fatalf("granting an ordinary user impersonation = %v, want ErrInvalid", err)
	}
}

func TestWhoCanImpersonate(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "bob@example.com", Source: policy.SourceLocal, Password: testPassword,
	}); err != nil {
		t.Fatalf("creating a user: %v", err)
	}
	if err := svc.GrantImpersonation(ctx, adm, admin.GrantImpersonationRequest{
		Actor: adm.Address, Target: "bob@example.com", Reason: "an incident",
	}); err != nil {
		t.Fatalf("granting: %v", err)
	}

	who, err := svc.WhoCanImpersonate(ctx, adm, "bob@example.com")
	if err != nil {
		t.Fatalf("listing impersonators: %v", err)
	}
	if len(who) != 1 || who[0].ActorAddress != adm.Address {
		t.Errorf("who = %+v, want just the administrator", who)
	}
}

func TestImpersonationGrantRevocation(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	adm := seedDomainAndAdmin(t, svc)

	if _, err := svc.CreateUser(ctx, adm, admin.CreateUserRequest{
		Address: "bob@example.com", Source: policy.SourceLocal, Password: testPassword,
	}); err != nil {
		t.Fatalf("creating a user: %v", err)
	}
	if err := svc.GrantImpersonation(ctx, adm, admin.GrantImpersonationRequest{
		Actor: adm.Address, Target: "bob@example.com", Reason: "an incident",
	}); err != nil {
		t.Fatalf("granting: %v", err)
	}
	if err := svc.RevokeImpersonation(ctx, adm, adm.Address, "bob@example.com"); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	grants, err := svc.ListImpersonationGrants(ctx, adm, adm.Address)
	if err != nil {
		t.Fatalf("listing grants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("got %d grants, want the revoked one still on record: %+v", len(grants), grants)
	}
	if grants[0].Active {
		t.Error("a revoked grant reports active")
	}
}

// --- helpers ---

// mustAudit returns the entries for an action that were recorded against one
// specific target. Scoping by target matters because provisioning the fixtures
// writes entries of its own, and a test asserting "exactly one" should be
// about its own change rather than about everything that has ever happened.
func mustAudit(t *testing.T, db *sqlx.DB, action, target string) []store.AuditEntry {
	t.Helper()
	entries, err := store.NewAuditLog().List(context.Background(), db, store.AuditFilter{
		Action: action, Target: target,
	})
	if err != nil {
		t.Fatalf("listing %s entries: %v", action, err)
	}
	return entries
}

// seedLiveSession opens a session for an address, so a test can prove that
// disabling an account or resetting its password ended the sessions that were
// already open.
func seedLiveSession(t *testing.T, db *sqlx.DB, address string) string {
	t.Helper()
	created, err := store.NewSessions().Create(context.Background(), db, store.CreateSessionRequest{
		Address:     address,
		AbsoluteTTL: time.Hour,
		IdleTTL:     time.Hour,
	})
	if err != nil {
		t.Fatalf("seeding a session for %s: %v", address, err)
	}
	return created.Session.ID
}

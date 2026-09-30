package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/secret"
	"github.com/linchange/linchange/internal/store"
)

// A real argon2 hash is used throughout rather than a placeholder string, so the
// guard that refuses to store anything else is exercised against a value that
// looks like the thing it is supposed to accept.
func testHash(t *testing.T, plaintext string) string {
	t.Helper()
	hash, err := password.Hash(plaintext, config.Argon2Params{
		MemoryKib: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	})
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	return hash
}

func TestCredentialsSetAndGet(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	creds := store.NewCredentials()

	hash := testHash(t, "correct horse battery staple")
	if err := creds.Set(ctx, db, "alice@example.com", hash); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := creds.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Hash != hash {
		t.Error("the stored hash did not come back unchanged")
	}
	if !got.ChangedAt.Valid {
		t.Error("changed_at was not set by a password write")
	}

	// The hash has to be a usable argon2 verification, not just a string that
	// survived a round trip.
	ok, err := password.Verify("correct horse battery staple", got.Hash)
	if err != nil || !ok {
		t.Errorf("Verify against the stored hash = %v, %v", ok, err)
	}
	ok, err = password.Verify("wrong password", got.Hash)
	if err != nil || ok {
		t.Errorf("Verify with a wrong password = %v, %v", ok, err)
	}
}

func TestCredentialsSetRejectsAPlaintextPassword(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	creds := store.NewCredentials()

	// This is the mistake the check exists for: a caller that hands over a
	// plaintext password would otherwise store it silently, and the only symptom
	// would be a plaintext password sitting in the database.
	for _, bad := range []string{
		"correct horse battery staple",
		"$argon2",
		"$argon2id$v=19$m=65536",
		"",
	} {
		if err := creds.Set(ctx, db, "alice@example.com", bad); err == nil {
			t.Errorf("stored %q as a credential", bad)
		}
	}

	// Nothing was written, so the address still has no credential.
	if _, err := creds.Get(ctx, db, "alice@example.com"); !errors.Is(err, store.ErrNoCredential) {
		t.Errorf("Get after a rejected store = %v, want ErrNoCredential", err)
	}
}

func TestCredentialsSetRejectsAnAddressWithNoUser(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)

	err := store.NewCredentials().Set(context.Background(), db, "nobody@example.com",
		testHash(t, "whatever"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Set for an address with no user = %v, want ErrNotFound", err)
	}
}

func TestCredentialsSetForAnLDAPUser(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (address, address_local, source) VALUES ('dana@example.com','dana','ldap')`); err != nil {
		t.Fatalf("seeding an LDAP user: %v", err)
	}

	// An LDAP user normally has no local password, but a domain can still hold
	// one for an account that migrated in from a local deployment, and the
	// schema has to allow it.
	if err := store.NewCredentials().Set(ctx, db, "dana@example.com", testHash(t, "migrated")); err != nil {
		t.Fatalf("Set for an LDAP user: %v", err)
	}
}

// An LDAP user and an address that does not exist both report the same thing,
// because telling them apart would make this a way to enumerate accounts.
func TestGetReportsMissingAndUnknownAlike(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	creds := store.NewCredentials()

	for _, address := range []string{"dana@example.com", "nobody@example.com", ""} {
		if _, err := creds.Get(ctx, db, address); !errors.Is(err, store.ErrNoCredential) {
			t.Errorf("Get(%q) = %v, want ErrNoCredential", address, err)
		}
	}
}

func TestCredentialsSetReplacesAnExistingHash(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	creds := store.NewCredentials()

	first := testHash(t, "first password")
	second := testHash(t, "second password")
	if err := creds.Set(ctx, db, "alice@example.com", first); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := creds.Set(ctx, db, "alice@example.com", second); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := creds.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Hash != second {
		t.Error("the second write did not replace the hash")
	}
	// One row, not two.
	var count int
	if err := db.GetContext(ctx, &count,
		`SELECT count(*) FROM credentials WHERE address = 'alice@example.com'`); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != 1 {
		t.Errorf("credential rows = %d, want 1", count)
	}
}

func TestCredentialsHas(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	creds := store.NewCredentials()

	for _, address := range []string{"alice@example.com", "dana@example.com", "nobody@example.com", ""} {
		has, err := creds.Has(ctx, db, address)
		if err != nil {
			t.Fatalf("Has(%q): %v", address, err)
		}
		if has {
			t.Errorf("Has(%q) = true, want false", address)
		}
	}

	if err := creds.Set(ctx, db, "alice@example.com", testHash(t, "password")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if has, err := creds.Has(ctx, db, "alice@example.com"); err != nil || !has {
		t.Errorf("Has after a store = %v, %v; want true", has, err)
	}
}

func TestCredentialsDeleteLeavesTheUser(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	creds := store.NewCredentials()

	if err := creds.Set(ctx, db, "alice@example.com", testHash(t, "password")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Deleting the password leaves the user, which is how a local account
	// becomes directory-backed.
	if err := creds.Delete(ctx, db, "alice@example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := creds.Get(ctx, db, "alice@example.com"); !errors.Is(err, store.ErrNoCredential) {
		t.Errorf("Get after Delete = %v, want ErrNoCredential", err)
	}
	if _, err := store.LoadPrincipal(ctx, db, "alice@example.com"); err != nil {
		t.Errorf("deleting a credential removed the user too: %v", err)
	}
	if err := creds.Delete(ctx, db, "alice@example.com"); err != nil {
		t.Errorf("deleting a credential twice: %v", err)
	}
}

func TestTOTPStoreAndGet(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	totp := store.NewTOTP()

	digests := [][]byte{secret.Hash("alpha"), secret.Hash("beta"), secret.Hash("gamma")}
	sealed := []byte("nonce-and-ciphertext")

	if err := totp.Store(ctx, db, "alice@example.com", sealed, digests, true); err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, err := totp.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.SecretEncrypted) != string(sealed) {
		t.Errorf("secret = %q, want %q", got.SecretEncrypted, sealed)
	}
	if !got.Enabled {
		t.Error("an enrolment stored as enabled came back disabled")
	}
	if len(got.RecoveryDigests) != 3 {
		t.Fatalf("recovery digests = %d, want 3", len(got.RecoveryDigests))
	}
	// The digests have to survive the text[] encoding byte for byte, or a
	// recovery code would be issued that can never be redeemed.
	for i, want := range digests {
		if string(got.RecoveryDigests[i]) != string(want) {
			t.Errorf("digest %d did not survive the round trip", i)
		}
	}
	if got.LastUsedAt.Valid {
		t.Error("a fresh enrolment reports a spent step")
	}
}

// An unconfirmed enrolment must be storable and readable, and must come back
// disabled. Otherwise an administrator could enrol a factor on a user's phone
// without their knowledge and the flag would have nothing to say.
func TestTOTPStoresAnUnconfirmedEnrolment(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	totp := store.NewTOTP()

	if err := totp.Store(ctx, db, "alice@example.com", []byte("sealed"), nil, false); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := totp.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Enabled {
		t.Error("an unconfirmed enrolment came back enabled")
	}
	if len(got.RecoveryDigests) != 0 {
		t.Errorf("recovery digests = %d, want 0", len(got.RecoveryDigests))
	}
}

func TestTOTPStoreRejectsAnIncompleteEnrolment(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	totp := store.NewTOTP()

	if err := totp.Store(ctx, db, "", []byte("sealed"), nil, true); err == nil {
		t.Error("stored an enrolment with no address")
	}
	if err := totp.Store(ctx, db, "alice@example.com", nil, nil, true); err == nil {
		t.Error("stored an enrolment with no secret")
	}
	if err := totp.Store(ctx, db, "nobody@example.com", []byte("sealed"), nil, true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Store for an address with no user = %v, want ErrNotFound", err)
	}
}

func TestTOTPGetForAnAbsentEnrolment(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)

	for _, address := range []string{"alice@example.com", "nobody@example.com", ""} {
		if _, err := store.NewTOTP().Get(context.Background(), db, address); !errors.Is(err, store.ErrNoTOTP) {
			t.Errorf("Get(%q) = %v, want ErrNoTOTP", address, err)
		}
	}
}

// MarkUsed is what actually makes a code single use, so the step has to advance
// and must never move backwards.
func TestTOTPMarkUsedAdvancesTheStep(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	totp := store.NewTOTP()

	if err := totp.Store(ctx, db, "alice@example.com", []byte("sealed"), nil, true); err != nil {
		t.Fatalf("Store: %v", err)
	}

	const step = int64(1_700_000_000 / 30)
	if err := totp.MarkUsed(ctx, db, "alice@example.com", step); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	got, err := totp.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.LastUsedAt.Valid {
		t.Fatal("the spent step was not recorded")
	}
	want := time.Unix(step*30, 0)
	if diff := got.LastUsedAt.Time.Sub(want); diff > time.Second || diff < -time.Second {
		t.Errorf("last_used_at = %v, want %v", got.LastUsedAt.Time, want)
	}

	// A later step advances it.
	if err := totp.MarkUsed(ctx, db, "alice@example.com", step+1); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	got, _ = totp.Get(ctx, db, "alice@example.com")
	if !got.LastUsedAt.Time.After(want) {
		t.Errorf("last_used_at = %v, want something after %v", got.LastUsedAt.Time, want)
	}
}

// Two requests carrying the same code can pass verification at the same moment.
// Without a guard on the write, the second would move the recorded step backwards
// and the code would become usable again, so this is the property that makes
// verification's own check meaningful.
func TestTOTPMarkUsedNeverMovesBackwards(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	totp := store.NewTOTP()

	if err := totp.Store(ctx, db, "alice@example.com", []byte("sealed"), nil, true); err != nil {
		t.Fatalf("Store: %v", err)
	}

	const later = int64(1_700_000_000 / 30)
	if err := totp.MarkUsed(ctx, db, "alice@example.com", later); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	// A step behind, which is what a late duplicate request would carry.
	if err := totp.MarkUsed(ctx, db, "alice@example.com", later-2); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	got, err := totp.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.LastUsedAt.Time.Equal(time.Unix(later*30, 0)) {
		t.Errorf("last_used_at = %v, want it left at %v", got.LastUsedAt.Time, time.Unix(later*30, 0))
	}

	// Re-marking the same step is a no-op rather than an error, because two
	// requests carrying the same code is normal rather than exceptional.
	if err := totp.MarkUsed(ctx, db, "alice@example.com", later); err != nil {
		t.Errorf("re-marking the same step: %v", err)
	}
}

// A spent code is removed by digest value, so two concurrent uses of the same
// code cannot delete each other or an unrelated one.
func TestTOTPConsumeRecoveryCodeIsSingleUse(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	totp := store.NewTOTP()

	first := secret.Hash("first-code")
	second := secret.Hash("second-code")
	third := secret.Hash("third-code")
	if err := totp.Store(ctx, db, "alice@example.com", []byte("sealed"),
		[][]byte{first, second, third}, true); err != nil {
		t.Fatalf("Store: %v", err)
	}

	spent, err := totp.ConsumeRecoveryCode(ctx, db, "alice@example.com", first)
	if err != nil {
		t.Fatalf("ConsumeRecoveryCode: %v", err)
	}
	if !spent {
		t.Error("a live recovery code was reported as already spent")
	}

	// A replay of the same code finds nothing.
	spent, err = totp.ConsumeRecoveryCode(ctx, db, "alice@example.com", first)
	if err != nil {
		t.Fatalf("ConsumeRecoveryCode: %v", err)
	}
	if spent {
		t.Error("the same recovery code was spent twice")
	}

	// The other two are untouched: removing by value rather than by position is
	// what keeps a replay from destroying an unrelated code.
	got, err := totp.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.RecoveryDigests) != 2 {
		t.Fatalf("remaining codes = %d, want 2", len(got.RecoveryDigests))
	}
	for _, want := range [][]byte{second, third} {
		found := false
		for _, have := range got.RecoveryDigests {
			if string(have) == string(want) {
				found = true
			}
		}
		if !found {
			t.Error("an unspent recovery code was consumed by a replay of a different one")
		}
	}
}

func TestTOTPConsumeRecoveryCodeRejectsEmptyInput(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	totp := store.NewTOTP()

	for _, c := range []struct {
		address string
		digest  []byte
	}{
		{"", []byte("x")},
		{"alice@example.com", nil},
	} {
		spent, err := totp.ConsumeRecoveryCode(ctx, db, c.address, c.digest)
		if err != nil {
			t.Errorf("ConsumeRecoveryCode(%q, %d bytes): %v", c.address, len(c.digest), err)
		}
		if spent {
			t.Errorf("ConsumeRecoveryCode(%q, %d bytes) reported a spend", c.address, len(c.digest))
		}
	}
}

// Re-enrolling replaces the secret and issues new codes, and must not inherit
// the spent step of the old enrolment or the old codes would stay spent.
func TestTOTPStoreReplacesAnEnrolment(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	totp := store.NewTOTP()

	if err := totp.Store(ctx, db, "alice@example.com", []byte("first-seal"),
		[][]byte{secret.Hash("old-code")}, true); err != nil {
		t.Fatalf("Store: %v", err)
	}
	const step = int64(1_700_000_000 / 30)
	if err := totp.MarkUsed(ctx, db, "alice@example.com", step); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	if err := totp.Store(ctx, db, "alice@example.com", []byte("second-seal"),
		[][]byte{secret.Hash("new-code")}, true); err != nil {
		t.Fatalf("re-Store: %v", err)
	}
	got, err := totp.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.SecretEncrypted) != "second-seal" {
		t.Errorf("secret = %q, want the replacement", got.SecretEncrypted)
	}
	if got.LastUsedAt.Valid {
		t.Error("a new enrolment inherited the spent step of the old one")
	}
	if len(got.RecoveryDigests) != 1 || string(got.RecoveryDigests[0]) != string(secret.Hash("new-code")) {
		t.Errorf("recovery codes = %v, want only the newly issued one", got.RecoveryDigests)
	}
}

func TestTOTPDelete(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	totp := store.NewTOTP()

	if err := totp.Store(ctx, db, "alice@example.com", []byte("sealed"), nil, true); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := totp.Delete(ctx, db, "alice@example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := totp.Get(ctx, db, "alice@example.com"); !errors.Is(err, store.ErrNoTOTP) {
		t.Errorf("Get after Delete = %v, want ErrNoTOTP", err)
	}
	if err := totp.Delete(ctx, db, "alice@example.com"); err != nil {
		t.Errorf("deleting an enrolment twice: %v", err)
	}
	// Unenrolling a factor must not touch the user's password or their account.
	if _, err := store.LoadPrincipal(ctx, db, "alice@example.com"); err != nil {
		t.Errorf("unenrolling removed the user: %v", err)
	}
}

// The digests are compared by the auth package, so what matters here is that the
// digests the package hands back are the ones it was given.
func TestStoredDigestsAreUsableByTheRecoveryCodeVerifier(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()
	totp := store.NewTOTP()

	codes, err := secret.GenerateRecoveryCodes(5)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	typed := codes[2].Code
	digests := make([][]byte, 0, len(codes))
	for _, c := range codes {
		digests = append(digests, c.Digest)
	}

	if err := totp.Store(ctx, db, "alice@example.com", []byte("sealed"), digests, true); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := totp.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	index, ok := secret.VerifyRecoveryCode(typed, got.RecoveryDigests)
	if !ok {
		t.Fatal("an issued recovery code did not verify against the stored digests")
	}
	if index != 2 {
		t.Errorf("matched index %d, want 2", index)
	}

	// And it can then be spent exactly once.
	spent, err := totp.ConsumeRecoveryCode(ctx, db, "alice@example.com", got.RecoveryDigests[index])
	if err != nil || !spent {
		t.Errorf("spending the matched code = %v, %v", spent, err)
	}
	after, err := totp.Get(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := secret.VerifyRecoveryCode(typed, after.RecoveryDigests); ok {
		t.Error("a spent recovery code still verified")
	}
	if len(after.RecoveryDigests) != 4 {
		t.Errorf("remaining codes = %d, want 4", len(after.RecoveryDigests))
	}
}

// The forced-change flag lives on users, so it has to be settable there and have
// LoadPrincipal report the change. This is the path a reset or an
// administrative "must change" takes.
func TestSetMustChangePassword(t *testing.T) {
	db := testMigratedDB(t)
	seedSessionFixtures(t, db)
	ctx := context.Background()

	p, err := store.LoadPrincipal(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("LoadPrincipal: %v", err)
	}
	if p.MustChangePassword {
		t.Fatal("setup: a new user should not be required to change a password")
	}

	if err := store.SetMustChangePassword(ctx, db, "alice@example.com", true); err != nil {
		t.Fatalf("SetMustChangePassword: %v", err)
	}
	p, err = store.LoadPrincipal(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("LoadPrincipal: %v", err)
	}
	if !p.MustChangePassword {
		t.Error("a session would not know a password change is required")
	}

	if err := store.SetMustChangePassword(ctx, db, "alice@example.com", false); err != nil {
		t.Fatalf("SetMustChangePassword: %v", err)
	}
	p, _ = store.LoadPrincipal(ctx, db, "alice@example.com")
	if p.MustChangePassword {
		t.Error("clearing the flag did not clear it")
	}

	if err := store.SetMustChangePassword(ctx, db, "nobody@example.com", true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetMustChangePassword for an unknown address = %v, want ErrNotFound", err)
	}
}

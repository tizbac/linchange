package password_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/password"
)

// fast keeps the tests quick. Production cost is covered by the OWASP
// recommendation being the configured default, not by being slow here.
func fast() config.Argon2Params {
	return config.Argon2Params{
		MemoryKib:   8 * 1024,
		Iterations:  1,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

func TestHashAndVerify(t *testing.T) {
	p := fast()
	const pw = "correct horse battery staple"

	hash, err := password.Hash(pw, p)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	ok, err := password.Verify(pw, hash)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Error("the correct password did not verify")
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	p := fast()
	hash, err := password.Hash("correct horse battery staple", p)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	for _, wrong := range []string{
		"correct horse battery stapl",
		"Correct horse battery staple",
		"",
		"correct horse battery staple ",
		"a completely different password",
	} {
		ok, err := password.Verify(wrong, hash)
		if err != nil {
			t.Fatalf("Verify(%q) returned an error rather than a mismatch: %v", wrong, err)
		}
		if ok {
			t.Errorf("Verify(%q) accepted a wrong password", wrong)
		}
	}
}

func TestHashIsSaltedSoIdenticalPasswordsDiffer(t *testing.T) {
	p := fast()
	const pw = "correct horse battery staple"

	first, err := password.Hash(pw, p)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	second, err := password.Hash(pw, p)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	if first == second {
		t.Error("hashing the same password twice produced identical output, " +
			"so the hashes are rainbow-tableable")
	}

	// Both must still verify: the salt is stored, not discarded.
	for _, h := range []string{first, second} {
		ok, err := password.Verify(pw, h)
		if err != nil || !ok {
			t.Errorf("salted hash failed to verify: ok=%v err=%v", ok, err)
		}
	}
}

func TestHashUsesTheConfiguredParameters(t *testing.T) {
	p := fast()
	hash, err := password.Hash("correct horse battery staple", p)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	for _, want := range []string{
		"$argon2id$",
		"v=19",
		"m=8192",
		"t=1",
		"p=1",
	} {
		if !strings.Contains(hash, want) {
			t.Errorf("hash %q does not record %q; the parameters must be stored "+
				"so the cost can be raised without invalidating existing passwords", hash, want)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	weak := fast()
	strong := fast()
	strong.MemoryKib = 64 * 1024
	strong.Iterations = 3

	hash, err := password.Hash("correct horse battery staple", weak)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	// Cost unchanged: no upgrade needed.
	needs, err := password.NeedsRehash(hash, weak)
	if err != nil {
		t.Fatalf("NeedsRehash: %v", err)
	}
	if needs {
		t.Error("NeedsRehash asked for an upgrade at unchanged parameters, " +
			"which would rewrite every hash on every login for no gain")
	}

	// Cost raised: the old hash must still verify, then be upgraded.
	needs, err = password.NeedsRehash(hash, strong)
	if err != nil {
		t.Fatalf("NeedsRehash: %v", err)
	}
	if !needs {
		t.Error("NeedsRehash did not request an upgrade after the cost was raised")
	}

	ok, err := password.Verify("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Errorf("a hash at the old cost stopped verifying after the cost was raised: ok=%v err=%v", ok, err)
	}
}

// A hash produced at a *higher* cost must not be downgraded: a login must not
// weaken a stored hash just because the configuration was lowered.
func TestNeedsRehashDoesNotDowngrade(t *testing.T) {
	strong := fast()
	strong.Iterations = 4

	hash, err := password.Hash("correct horse battery staple", strong)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	weak := fast()
	needs, err := password.NeedsRehash(hash, weak)
	if err != nil {
		t.Fatalf("NeedsRehash: %v", err)
	}
	if needs {
		t.Error("NeedsRehash asked to replace a stronger hash with a weaker one")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	// Each of these would panic or silently succeed if parsing were careless.
	bad := []string{
		"",
		"not-a-hash",
		"$argon2i$v=19$m=8192,t=1,p=1$c2FsdA$aGFzaA",  // wrong variant
		"$argon2id$v=16$m=8192,t=1,p=1$c2FsdA$aGFzaA", // wrong version
		"$argon2id$v=19$c2FsdA$aGFzaA",                // missing parameters
		"$argon2id$v=19$m=8192,t=1,p=1$c2FsdA",        // missing digest
		"$argon2id$v=19$m=0,t=1,p=1$c2FsdA$aGFzaA",    // zero memory
		"$argon2id$v=19$m=8192,t=0,p=1$c2FsdA$aGFzaA", // zero iterations
		"$argon2id$v=19$m=8192,t=1,p=0$c2FsdA$aGFzaA", // zero parallelism
		"$argon2id$v=19$m=8192,t=1,p=1$$aGFzaA",       // empty salt
		"$argon2id$v=19$m=8192,t=1,p=1$c2FsdA$YQ",     // truncated digest
		"$argon2id$v=19$m=8192,t=1,p=1$!!!$aGFzaA",    // undecodable salt
		"$argon2id$v=19$m=8192,t=1,p=1$c2FsdA$!!!",    // undecodable digest
		"$argon2id$v=99$m=8192,t=1,p=1$c2FsdA$aGFzaA", // future version
	}

	for _, stored := range bad {
		ok, err := password.Verify("correct horse battery staple", stored)
		if err == nil {
			t.Errorf("Verify accepted the malformed hash %q", stored)
		}
		if ok {
			t.Errorf("Verify reported a match for the malformed hash %q", stored)
		}
	}
}

// The whole point of the dummy hash: an unknown account must cost the same as
// a known one, or a login form becomes a user enumeration oracle.
func TestDummyVerifyTakesComparableTime(t *testing.T) {
	const pw = "correct horse battery staple"

	// Measure the real path first so a wildly different dummy cost is caught.
	hash, err := password.Hash(pw, fast())
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	const rounds = 3
	var real, dummy time.Duration
	for i := 0; i < rounds; i++ {
		start := time.Now()
		_, _ = password.Verify(pw, hash)
		real += time.Since(start)

		start = time.Now()
		password.DummyVerify(pw)
		dummy += time.Since(start)
	}

	// Argon2 timings are noisy, so allow a generous band. The failure this
	// guards against is a dummy that returns instantly, which is orders of
	// magnitude off rather than marginally different.
	if dummy < real/4 {
		t.Errorf("DummyVerify took %v against a real verification of %v; "+
			"an unknown account is measurably faster, which leaks whether it exists",
			dummy/3, real/3)
	}
}

func TestDummyVerifyAlwaysFailsAndNeverPanics(t *testing.T) {
	// It returns nothing by design: there is no account to match.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("DummyVerify panicked: %v", r)
		}
	}()

	for _, pw := range []string{"", "anything", strings.Repeat("x", 10000)} {
		password.DummyVerify(pw)
	}
}

func TestHashRejectsEmptyPassword(t *testing.T) {
	if _, err := password.Hash("", fast()); !errors.Is(err, password.ErrEmpty) {
		t.Errorf("Hash(\"\") = %v, want ErrEmpty", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		pw      string
		min     int
		wantErr error
	}{
		{"long enough", "correct horse battery staple", 12, nil},
		{"exactly the minimum", "123456789012", 12, nil},
		{"one under the minimum", "12345678901", 12, password.ErrTooShort},
		{"empty", "", 12, password.ErrEmpty},
		{"unicode counts as runes not bytes", strings.Repeat("é", 12), 12, nil},
		{"unicode just under the limit", strings.Repeat("é", 11), 12, password.ErrTooShort},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := password.Validate(tc.pw, tc.min)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Validate = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// The shipped default cost should satisfy the OWASP recommendation, because
// that is what every deployment gets without configuring anything.
func TestConfiguredDefaultsMeetTheOWASPRecommendation(t *testing.T) {
	p := config.New().Auth.Local.Argon2

	if p.MemoryKib < 64*1024 {
		t.Errorf("default memory cost is %d KiB, want at least 65536", p.MemoryKib)
	}
	if p.Iterations < 3 {
		t.Errorf("default iterations = %d, want at least 3", p.Iterations)
	}
	if p.SaltLength < 16 {
		t.Errorf("default salt length = %d, want at least 16", p.SaltLength)
	}
	if p.KeyLength < 32 {
		t.Errorf("default key length = %d, want at least 32", p.KeyLength)
	}
}

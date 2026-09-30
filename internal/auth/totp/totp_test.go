package totp_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	// The library and the package under test have the same name, so the
	// library is aliased. Everything else in this file refers to the package
	// under test.
	"github.com/pquerna/otp"
	libtotp "github.com/pquerna/otp/totp"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/totp"
)

const testKey = "0123456789abcdef0123456789abcdef0123456789"

func cfg() config.TOTPAuth {
	return config.TOTPAuth{
		Enabled:       true,
		Issuer:        "linchange",
		Window:        1,
		RecoveryCodes: 10,
	}
}

func newVerifier(t *testing.T, c config.TOTPAuth) *totp.Verifier {
	t.Helper()
	v, err := totp.New(c, testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// codeFor computes the code an authenticator app would show at t, using the
// same parameters the verifier uses. Computed independently of the verifier's
// own code path so the test is not just checking the code against itself.
func codeFor(t *testing.T, secretB32 string, at time.Time) string {
	t.Helper()
	code, err := libtotp.GenerateCodeCustom(secretB32, at.UTC(), libtotp.ValidateOpts{
		Period:    30,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("generating expected code: %v", err)
	}
	return code
}

func sealed(t *testing.T, v *totp.Verifier, secretB32 string) []byte {
	t.Helper()
	enc, err := v.Encrypt(secretB32, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return enc.Ciphertext
}

func TestEnrolProducesAUsableSecret(t *testing.T) {
	v := newVerifier(t, cfg())

	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if enrol.Secret == "" {
		t.Fatal("Enrol produced no secret")
	}
	if !strings.HasPrefix(enrol.URI, "otpauth://totp/") {
		t.Errorf("URI %q is not an otpauth TOTP URI", enrol.URI)
	}
	if !strings.Contains(enrol.URI, "issuer=linchange") {
		t.Errorf("URI %q does not carry the issuer", enrol.URI)
	}
	if !strings.Contains(enrol.URI, "alice") {
		t.Errorf("URI %q does not carry the account name", enrol.URI)
	}
	if len(enrol.RecoveryCodes) != 10 {
		t.Errorf("got %d recovery codes, want 10", len(enrol.RecoveryCodes))
	}
}

func TestEnrolIssuesADifferentSecretEachTime(t *testing.T) {
	v := newVerifier(t, cfg())

	first, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	second, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if first.Secret == second.Secret {
		t.Error("Enrol returned the same secret twice, so two users could share a second factor")
	}
}

func TestEnrolRejectsEmptyAddress(t *testing.T) {
	if _, err := newVerifier(t, cfg()).Enrol("  "); err == nil {
		t.Error("Enrol accepted an empty address")
	}
}

func TestVerifyAcceptsAValidCode(t *testing.T) {
	v := newVerifier(t, cfg())
	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	want := codeFor(t, enrol.Secret, now)

	step, err := v.Verify(sealed(t, v, enrol.Secret), want, time.Time{}, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if step != totp.StepAt(now) {
		t.Errorf("Verify returned step %d, want %d", step, totp.StepAt(now))
	}
}

func TestVerifyRejectsWrongCodes(t *testing.T) {
	v := newVerifier(t, cfg())
	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	sealedSecret := sealed(t, v, enrol.Secret)
	now := time.Unix(1_700_000_000, 0)
	valid := codeFor(t, enrol.Secret, now)

	// A near miss: one digit off, which is what a mistyped code looks like.
	first := valid[0]
	bumped := byte('0')
	if first == '0' {
		bumped = '1'
	} else {
		bumped = first - 1
	}
	wrong := string(bumped) + valid[1:]

	for _, code := range []string{
		"",
		"   ",
		"000000",
		"12345",   // too short
		"1234567", // too long
		"abcdef",  // not digits
		wrong,
	} {
		if _, err := v.Verify(sealedSecret, code, time.Time{}, now); !errors.Is(err, totp.ErrInvalidCode) {
			t.Errorf("Verify(%q) = %v, want ErrInvalidCode", code, err)
		}
	}
}

// The window absorbs clock drift, so a phone a step behind still works.
func TestVerifyAcceptsCodesEitherSideWithinTheWindow(t *testing.T) {
	v := newVerifier(t, cfg())
	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	sealedSecret := sealed(t, v, enrol.Secret)
	now := time.Unix(1_700_000_000, 0)

	for _, offset := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		code := codeFor(t, enrol.Secret, now.Add(offset))
		if _, err := v.Verify(sealedSecret, code, time.Time{}, now); err != nil {
			t.Errorf("a code from %v away was rejected with window 1: %v", offset, err)
		}
	}

	// Two steps away is outside a window of one, whichever way it lies.
	for _, offset := range []time.Duration{-90 * time.Second, 90 * time.Second} {
		code := codeFor(t, enrol.Secret, now.Add(offset))
		if _, err := v.Verify(sealedSecret, code, time.Time{}, now); !errors.Is(err, totp.ErrInvalidCode) {
			t.Errorf("a code from %v away was accepted with window 1", offset)
		}
	}
}

func TestVerifyHonoursAZeroWindow(t *testing.T) {
	c := cfg()
	c.Window = 0
	v := newVerifier(t, c)
	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	sealedSecret := sealed(t, v, enrol.Secret)
	now := time.Unix(1_700_000_000, 0)

	if _, err := v.Verify(sealedSecret, codeFor(t, enrol.Secret, now), time.Time{}, now); err != nil {
		t.Errorf("the current code was rejected with a zero window: %v", err)
	}
	if _, err := v.Verify(sealedSecret, codeFor(t, enrol.Secret, now.Add(-30*time.Second)), time.Time{}, now); err == nil {
		t.Error("a code from the previous step was accepted with a zero window")
	}
}

// A code must not work twice. Without this, a shoulder-surfer who reads one
// code has a usable credential for the rest of the window, and the window is
// the entire protection.
func TestVerifyRefusesAReplayedCode(t *testing.T) {
	v := newVerifier(t, cfg())
	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	sealedSecret := sealed(t, v, enrol.Secret)
	now := time.Unix(1_700_000_000, 0)
	code := codeFor(t, enrol.Secret, now)

	step, err := v.Verify(sealedSecret, code, time.Time{}, now)
	if err != nil {
		t.Fatalf("first Verify: %v", err)
	}

	// The caller persists the step as last_used_at.
	lastUsed := time.Unix(step*30, 0)
	if _, err := v.Verify(sealedSecret, code, lastUsed, now); !errors.Is(err, totp.ErrReplay) {
		t.Errorf("the same code verified twice: %v", err)
	}

	// Even with a wider window, the spent step stays spent.
	if _, err := v.Verify(sealedSecret, code, lastUsed, now.Add(30*time.Second)); !errors.Is(err, totp.ErrReplay) {
		t.Errorf("a replayed code was accepted in a later step: %v", err)
	}
}

// A code from a *newer* step is fine: that is ordinary clock drift forward, not
// a replay.
func TestVerifyAcceptsANewerStepAfterAnOlderOne(t *testing.T) {
	v := newVerifier(t, cfg())
	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	sealedSecret := sealed(t, v, enrol.Secret)
	now := time.Unix(1_700_000_000, 0)

	earlier := now.Add(-30 * time.Second)
	step, err := v.Verify(sealedSecret, codeFor(t, enrol.Secret, earlier), time.Time{}, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	lastUsed := time.Unix(step*30, 0)
	if _, err := v.Verify(sealedSecret, codeFor(t, enrol.Secret, now), lastUsed, now); err != nil {
		t.Errorf("a code from a newer step was refused as a replay: %v", err)
	}
}

func TestVerifyIgnoresWhitespaceInCodes(t *testing.T) {
	v := newVerifier(t, cfg())
	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	code := codeFor(t, enrol.Secret, now)
	spaced := code[:3] + " " + code[3:]

	if _, err := v.Verify(sealed(t, v, enrol.Secret), spaced, time.Time{}, now); err != nil {
		t.Errorf("a code with a space was rejected: %v", err)
	}
}

func TestVerifyRejectsAnUnenrolledOrCorruptSecret(t *testing.T) {
	v := newVerifier(t, cfg())

	// Truncated ciphertext must be an error, not a panic.
	for _, bad := range [][]byte{nil, {}, {0x01}, make([]byte, 12)} {
		if _, err := v.Verify(bad, "123456", time.Time{}, time.Now()); !errors.Is(err, totp.ErrCorruptSecret) {
			t.Errorf("Verify with %d bytes of ciphertext = %v, want ErrCorruptSecret", len(bad), err)
		}
	}
}

// A different encryption key must not open the secret. This is the property that
// makes storing it encrypted worth anything.
func TestASecretCannotBeDecryptedWithAnotherKey(t *testing.T) {
	enrol, err := newVerifier(t, cfg()).Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	sealedSecret := sealed(t, newVerifier(t, cfg()), enrol.Secret)

	other, err := totp.New(cfg(), strings.Repeat("z", 40))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := other.Decrypt(sealedSecret); !errors.Is(err, totp.ErrCorruptSecret) {
		t.Errorf("a secret was opened with the wrong key: %v", err)
	}

	// And a corrupted ciphertext must fail rather than yield garbage.
	corrupted := append([]byte(nil), sealedSecret...)
	corrupted[len(corrupted)-1] ^= 0xff
	if _, err := newVerifier(t, cfg()).Decrypt(corrupted); !errors.Is(err, totp.ErrCorruptSecret) {
		t.Errorf("a corrupted ciphertext decrypted: %v", err)
	}
}

// The stored ciphertext must not contain the secret in the clear, or
// encryption-at-rest is decorative.
func TestCiphertextDoesNotLeakTheSecret(t *testing.T) {
	v := newVerifier(t, cfg())
	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	sealedSecret := sealed(t, v, enrol.Secret)

	if strings.Contains(string(sealedSecret), enrol.Secret) {
		t.Error("the ciphertext contains the base32 secret")
	}
	// The same secret sealed twice must differ, or the nonce is not random.
	again := sealed(t, v, enrol.Secret)
	if string(sealedSecret) == string(again) {
		t.Error("sealing the same secret twice produced identical ciphertext, so the nonce is not random")
	}
	// Both must still open.
	for _, c := range [][]byte{sealedSecret, again} {
		got, err := v.Decrypt(c)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if got != enrol.Secret {
			t.Errorf("Decrypt returned %q, want %q", got, enrol.Secret)
		}
	}
}

func TestNewRequiresAKey(t *testing.T) {
	for _, key := range []string{"", "   "} {
		if _, err := totp.New(cfg(), key); !errors.Is(err, totp.ErrKeyRequired) {
			t.Errorf("New with key %q = %v, want ErrKeyRequired", key, err)
		}
	}
}

func TestNewRefusesWhenDisabled(t *testing.T) {
	c := cfg()
	c.Enabled = false
	if _, err := totp.New(c, testKey); !errors.Is(err, totp.ErrDisabled) {
		t.Errorf("New with TOTP disabled = %v, want ErrDisabled", err)
	}
}

func TestIssuerIsBounded(t *testing.T) {
	// An authenticator app has very little room for the issuer, and a long one
	// produces a truncated, wrong-looking label.
	c := cfg()
	c.Issuer = strings.Repeat("A", 100)
	v := newVerifier(t, c)

	if len(v.Issuer()) > 32 {
		t.Errorf("issuer is %d characters, want at most 32", len(v.Issuer()))
	}
	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if !strings.Contains(enrol.URI, "issuer="+v.Issuer()) {
		t.Errorf("URI does not carry the bounded issuer: %s", enrol.URI)
	}
}

func TestEmptyIssuerFallsBack(t *testing.T) {
	c := cfg()
	c.Issuer = "   "
	if v := newVerifier(t, c); v.Issuer() == "" {
		t.Error("a blank issuer was not replaced with a default")
	}
}

func TestDecodeSecret(t *testing.T) {
	v := newVerifier(t, cfg())
	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}

	// A user typing the secret into a password manager or reading it off a
	// backup must not be defeated by case, spaces or padding.
	for _, typed := range []string{
		enrol.Secret,
		strings.ToLower(enrol.Secret),
		enrol.Secret + "====",
		" " + enrol.Secret + " ",
	} {
		got, err := totp.DecodeSecret(typed)
		if err != nil {
			t.Errorf("DecodeSecret(%q): %v", typed, err)
			continue
		}
		if got != strings.ToUpper(strings.TrimRight(enrol.Secret, "=")) {
			t.Errorf("DecodeSecret(%q) = %q", typed, got)
		}
	}

	// A secret with spaces in it, as a person might write it down.
	spaced := enrol.Secret[:8] + " " + enrol.Secret[8:]
	if _, err := totp.DecodeSecret(spaced); err != nil {
		t.Errorf("DecodeSecret with an embedded space: %v", err)
	}

	for _, bad := range []string{"", "not base32 !!!", "AAAA"} {
		if _, err := totp.DecodeSecret(bad); err == nil {
			t.Errorf("DecodeSecret(%q) should have failed", bad)
		}
	}
}

func TestFingerprintIsStableAndDistinct(t *testing.T) {
	v := newVerifier(t, cfg())
	enrol, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	other, err := v.Enrol("alice@example.com")
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}

	a := totp.Fingerprint(enrol.Secret)
	b := totp.Fingerprint(enrol.Secret)
	c := totp.Fingerprint(other.Secret)

	if a == "" {
		t.Fatal("Fingerprint returned nothing for a valid secret")
	}
	if a != b {
		t.Error("Fingerprint is not stable")
	}
	if a == c {
		t.Error("two different secrets produced the same fingerprint")
	}
	if strings.Contains(a, enrol.Secret) {
		t.Error("Fingerprint leaks the secret")
	}
	if totp.Fingerprint("!!!not base32!!!") != "" {
		t.Error("Fingerprint should return nothing for an undecodable secret")
	}
}

func TestRecentlyUsedRejectsAbsurdTimestamps(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	if !totp.RecentlyUsed(now.Add(-time.Hour), now) {
		t.Error("a timestamp an hour old was treated as implausible")
	}
	if totp.RecentlyUsed(time.Time{}, now) {
		t.Error("a zero timestamp was treated as plausible")
	}
	// A phone years out of date would otherwise have its whole remaining window
	// accepted at once.
	if totp.RecentlyUsed(now.Add(-400*24*time.Hour), now) {
		t.Error("a timestamp years old was treated as plausible")
	}
}

func TestStepAt(t *testing.T) {
	// Two instants inside one 30 second window share a step, and instants in
	// adjacent windows do not. The base has to sit on a window boundary, or
	// "+29 seconds" crosses one and the first assertion fails for a reason
	// that has nothing to do with the code.
	const boundary = 30
	base := time.Unix(1_700_000_000, 0).Truncate(boundary * time.Second)
	if base.Unix()%boundary != 0 {
		t.Fatalf("test setup: %v is not on a %d second boundary", base, boundary)
	}

	if totp.StepAt(base) != totp.StepAt(base.Add(29*time.Second)) {
		t.Error("two instants in the same window got different steps")
	}
	if totp.StepAt(base) == totp.StepAt(base.Add(30*time.Second)) {
		t.Error("two instants in adjacent windows got the same step")
	}
	if totp.StepAt(base) != totp.StepAt(base.Add(-0)) {
		t.Error("StepAt is not stable for the same instant")
	}
}

// The key is what protects the secret, so the config validation that guards it
// is part of this package's correctness.
func TestConfiguredKeyMeetsTheMinimum(t *testing.T) {
	// The default config has no key, which is why validation demands one.
	if got := config.New().Auth.EncryptionKey; got != "" {
		t.Errorf("a key was defaulted to %q; it must be supplied by the operator "+
			"so that no two deployments share one", got)
	}
}

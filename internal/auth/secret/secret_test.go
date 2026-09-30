package secret_test

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/linchange/linchange/internal/auth/secret"
)

func TestGenerateToken(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 500; i++ {
		tok, err := secret.GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if tok == "" {
			t.Fatal("GenerateToken returned an empty token")
		}
		if seen[tok] {
			t.Fatalf("GenerateToken repeated a token after %d draws", i)
		}
		seen[tok] = true
	}
}

// Tokens travel in cookies, reset URLs and Authorization headers, so they must
// survive URL encoding untouched.
func TestGeneratedTokensAreURLSafe(t *testing.T) {
	for i := 0; i < 100; i++ {
		tok, err := secret.GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if strings.ContainsAny(tok, "+/=&?# ") {
			t.Fatalf("token %q contains a character that is unsafe in a URL", tok)
		}
	}
}

func TestGenerateClampsSize(t *testing.T) {
	// A zero or negative size must not produce a constant token.
	for _, n := range []int{0, -1, -1000} {
		tok, err := secret.Generate(n)
		if err != nil {
			t.Fatalf("Generate(%d): %v", n, err)
		}
		if len(tok) < 40 {
			t.Errorf("Generate(%d) produced a %d character token, which is far too short", n, len(tok))
		}
	}

	// An absurd size is clamped rather than allocating it.
	tok, err := secret.Generate(1 << 30)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(tok) > 128 {
		t.Errorf("Generate(1<<30) produced %d characters; the size should be clamped", len(tok))
	}
}

func TestHashAndEqual(t *testing.T) {
	tok, err := secret.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	digest := secret.Hash(tok)

	if len(digest) != 32 {
		t.Errorf("Hash returned %d bytes, want 32", len(digest))
	}
	if !secret.Equal(tok, digest) {
		t.Error("Equal rejected the token it was derived from")
	}

	// The stored form must not reveal the token.
	if strings.Contains(string(digest), tok) {
		t.Error("the digest contains the token in the clear")
	}

	other, err := secret.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if secret.Equal(other, digest) {
		t.Error("Equal accepted a different token")
	}
}

func TestEqualIsFalseForWrongLengthDigests(t *testing.T) {
	tok, err := secret.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	// A truncated or empty stored digest must not panic or match.
	for _, stored := range [][]byte{nil, {}, {0x00}, secret.Hash(tok)[:16]} {
		if secret.Equal(tok, stored) {
			t.Errorf("Equal accepted a %d byte digest", len(stored))
		}
	}
}

// Sessions look a token up by digest, so the digest has to stay stable across
// releases: changing the algorithm would invalidate every stored session and,
// worse, would be invisible until a user's next request. Pinning the value
// makes an accidental change a test failure.
func TestHashIsStable(t *testing.T) {
	const token = "a-token"
	want := sha256.Sum256([]byte(token))

	if got := secret.Hash(token); !bytes.Equal(got, want[:]) {
		t.Errorf("Hash(%q) = %x, want the plain SHA-256 %x", token, got, want)
	}
}

func TestGenerateRecoveryCodes(t *testing.T) {
	codes, err := secret.GenerateRecoveryCodes(10)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(codes) != 10 {
		t.Fatalf("got %d codes, want 10", len(codes))
	}

	seen := make(map[string]bool, len(codes))
	for i, c := range codes {
		if c.Code == "" {
			t.Fatalf("code %d is empty", i)
		}
		if len(c.Digest) == 0 {
			t.Fatalf("code %d has no digest", i)
		}
		// A code of one repeated character would be far weaker than its length
		// suggests, and would be the first thing a shoulder-surfer would guess.
		if strings.Count(c.Code, string(c.Code[0])) == len(c.Code) {
			t.Errorf("code %d is a single repeated character", i)
		}
		if seen[c.Code] {
			t.Errorf("recovery code %q was generated twice", c.Code)
		}
		seen[c.Code] = true
	}
}

func TestGenerateRecoveryCodesRejectsNonPositive(t *testing.T) {
	for _, n := range []int{0, -1} {
		if _, err := secret.GenerateRecoveryCodes(n); err == nil {
			t.Errorf("GenerateRecoveryCodes(%d) should have failed", n)
		}
	}
}

// A recovery code is retyped from paper by a person, so the formatting has to
// be readable and the alphabet has to exclude the characters people misread.
func TestRecoveryCodesAreReadable(t *testing.T) {
	codes, err := secret.GenerateRecoveryCodes(20)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}

	for _, c := range codes {
		groups := strings.Split(c.Code, "-")
		if len(groups) != 4 {
			t.Errorf("code %q is not split into four groups", c.Code)
		}
		for _, g := range groups {
			if len(g) != 4 {
				t.Errorf("group %q of %q is not four characters", g, c.Code)
			}
			for _, r := range g {
				// I, L, O and U are read as 1, 1, 0 and V.
				if strings.ContainsRune("ILOU", r) {
					t.Errorf("code %q contains the ambiguous character %q", c.Code, r)
				}
				if r >= 'a' && r <= 'z' {
					t.Errorf("code %q contains a lowercase letter", c.Code)
				}
			}
		}
	}
}

// Users paste these, retype them, and drop the dashes, so every reasonable
// spelling has to normalize to the same thing.
func TestNormalizeRecoveryCode(t *testing.T) {
	want := "ABCD2345"
	for _, typed := range []string{
		"ABCD-2345",
		"abcd-2345",
		"AbCd-2345",
		"ABCD2345",
		"  ABCD-2345  ",
		"ABCD 2345",
		"ABCD--2345",
		"ABCD-2345\n",
	} {
		if got := secret.NormalizeRecoveryCode(typed); got != want {
			t.Errorf("NormalizeRecoveryCode(%q) = %q, want %q", typed, got, want)
		}
	}
}

func TestVerifyRecoveryCode(t *testing.T) {
	codes, err := secret.GenerateRecoveryCodes(5)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	stored := make([][]byte, len(codes))
	for i, c := range codes {
		stored[i] = c.Digest
	}

	// Each code must be found at its own index, so the caller can burn exactly
	// the one that was used.
	for i, c := range codes {
		got, ok := secret.VerifyRecoveryCode(c.Code, stored)
		if !ok {
			t.Errorf("code %q did not verify", c.Code)
			continue
		}
		if got != i {
			t.Errorf("code %q matched index %d, want %d", c.Code, got, i)
		}

		// A user retyping from paper should not be punished for case or dashes.
		sloppy := strings.ToLower(strings.ReplaceAll(c.Code, "-", ""))
		if _, ok := secret.VerifyRecoveryCode(sloppy, stored); !ok {
			t.Errorf("code %q did not verify when retyped as %q", c.Code, sloppy)
		}
	}

	if _, ok := secret.VerifyRecoveryCode("ZZZZ-ZZZZ-ZZZZ-ZZZZ", stored); ok {
		t.Error("a code that was never generated verified")
	}
	if _, ok := secret.VerifyRecoveryCode("", stored); ok {
		t.Error("an empty code verified")
	}
}

func TestVerifyRecoveryCodeAgainstNoCodes(t *testing.T) {
	if _, ok := secret.VerifyRecoveryCode("ABCD-2345", nil); ok {
		t.Error("a code verified against an empty set")
	}
}

func TestFingerprint(t *testing.T) {
	a := secret.Fingerprint([]byte("token-one"))
	b := secret.Fingerprint([]byte("token-one"))
	c := secret.Fingerprint([]byte("token-two"))

	if a != b {
		t.Error("Fingerprint is not deterministic")
	}
	if a == c {
		t.Error("Fingerprint collided for different secrets")
	}
	if strings.Contains(a, "token-one") {
		t.Error("Fingerprint leaks the input")
	}
	// Short enough to read in a log, long enough not to be reversible.
	if len(a) < 6 || len(a) > 24 {
		t.Errorf("Fingerprint length is %d, want something log-sized", len(a))
	}
}

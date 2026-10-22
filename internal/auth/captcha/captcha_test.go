package captcha

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// codeFromToken opens the challenge the way the server does, so the test can
// answer it without the package exposing the answer.
func codeFromToken(t *testing.T, m *Manager, token string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("token is not base64url: %v", err)
	}
	nonce, sealed := raw[:m.aead.NonceSize()], raw[m.aead.NonceSize():]
	plain, err := m.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		t.Fatalf("token does not open: %v", err)
	}
	return string(plain[9:])
}

func TestChallengeVerifiesOnlyItsOwnAnswerBeforeExpiry(t *testing.T) {
	m, err := New(10 * time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	challenge, err := m.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !strings.HasPrefix(challenge.Image, "data:image/svg+xml;base64,") {
		t.Errorf("image is not an SVG data URL: %.40q", challenge.Image)
	}
	code := codeFromToken(t, m, challenge.Token)
	if len(code) != codeLen {
		t.Fatalf("code length = %d, want %d", len(code), codeLen)
	}

	if !m.Verify(challenge.Token, code) {
		t.Error("the right answer was refused")
	}
	if !m.Verify(challenge.Token, strings.ToLower(code)) {
		t.Error("case should not matter")
	}
	if m.Verify(challenge.Token, code+"X") {
		t.Error("a wrong answer was accepted")
	}
	if m.Verify(challenge.Token[:len(challenge.Token)-1], code) {
		t.Error("a truncated token was accepted")
	}

	now = now.Add(11 * time.Minute)
	if m.Verify(challenge.Token, code) {
		t.Error("an expired challenge was accepted")
	}
}

func TestExpiryIsSealedIntoTheToken(t *testing.T) {
	m, err := New(time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	challenge, err := m.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	raw, err := base64.RawURLEncoding.DecodeString(challenge.Token)
	if err != nil {
		t.Fatalf("token is not base64url: %v", err)
	}
	nonce, sealed := raw[:m.aead.NonceSize()], raw[m.aead.NonceSize():]
	plain, err := m.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		t.Fatalf("token does not open: %v", err)
	}
	expires := int64(binary.BigEndian.Uint64(plain[1:9]))
	if want := m.now().Add(time.Minute).Unix(); expires != want {
		t.Errorf("sealed expiry = %d, want %d", expires, want)
	}
}

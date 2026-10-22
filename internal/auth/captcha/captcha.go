// Package captcha issues and checks the short challenge shown before a
// password reset request.
//
// The challenge is stateless: the answer is sealed into the token with
// AES-GCM and the token is the only thing the browser carries back. Nothing
// is stored per visitor, so a burst of reset requests cannot grow a table,
// and a restart simply invalidates the challenges still on screen.
//
// The image is SVG rather than a bitmap because this server already renders
// SVG for organization marks, and an inline document keeps the challenge
// readable at every pixel density. The answer is never part of the SVG as
// text: each glyph is a separate rotated element, and noise lines cross it.
package captcha

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"html"
	"math/big"
	"strings"
	"time"
)

const (
	// alphabet omits characters that a reader routinely mistakes for one
	// another. A challenge that is correct but unreadable is a failed
	// challenge.
	alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	codeLen  = 5
	// tokenVersion is part of the sealed plaintext so a future format can
	// reject old tokens instead of misreading them.
	tokenVersion = 1
)

// Challenge is one issued captcha. Token is opaque to the browser; Image is a
// data URL ready for an <img src>.
type Challenge struct {
	Token     string    `json:"token"`
	Image     string    `json:"image"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Manager seals and opens challenges under one key. It is safe for concurrent
// use: the key is read-only and crypto/rand is the only source of state.
type Manager struct {
	aead cipher.AEAD
	ttl  time.Duration
	now  func() time.Time
}

// New builds a manager with a fresh random key. A restart invalidates
// challenges issued before it, which is why the token lifetime is short.
func New(ttl time.Duration) (*Manager, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("captcha: generating the key: %w", err)
	}
	return NewWithKey(key, ttl)
}

// NewWithKey builds a manager around an existing AES-256 key. It exists so a
// deployment with several front ends can share challenges, and so a test can
// open the tokens it issued without the package exposing the answer.
func NewWithKey(key []byte, ttl time.Duration) (*Manager, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("captcha: a positive lifetime is required")
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("captcha: the key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("captcha: building the cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("captcha: building the sealed box: %w", err)
	}
	return &Manager{aead: aead, ttl: ttl, now: time.Now}, nil
}

// Issue draws a code, seals it into a token and renders the image.
func (m *Manager) Issue() (Challenge, error) {
	code, err := randomCode()
	if err != nil {
		return Challenge{}, err
	}
	expiresAt := m.now().Add(m.ttl)

	plain := make([]byte, 1+8+codeLen)
	plain[0] = tokenVersion
	binary.BigEndian.PutUint64(plain[1:9], uint64(expiresAt.Unix()))
	copy(plain[9:], code)

	nonce := make([]byte, m.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Challenge{}, fmt.Errorf("captcha: generating a nonce: %w", err)
	}
	sealed := m.aead.Seal(nil, nonce, plain, nil)
	token := base64.RawURLEncoding.EncodeToString(append(nonce, sealed...))

	return Challenge{
		Token:     token,
		Image:     imageFor(code),
		ExpiresAt: expiresAt,
	}, nil
}

// Verify reports whether answer is the code sealed into token and the token
// has not expired. It never explains why a challenge failed.
func (m *Manager) Verify(token, answer string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) <= m.aead.NonceSize() {
		return false
	}
	nonce, sealed := raw[:m.aead.NonceSize()], raw[m.aead.NonceSize():]
	plain, err := m.aead.Open(nil, nonce, sealed, nil)
	if err != nil || len(plain) != 1+8+codeLen || plain[0] != tokenVersion {
		return false
	}
	expiresAt := time.Unix(int64(binary.BigEndian.Uint64(plain[1:9])), 0)
	if !expiresAt.After(m.now()) {
		return false
	}
	want := string(plain[9:])
	got := normalise(answer)
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

func normalise(answer string) string {
	return strings.ToUpper(strings.TrimSpace(answer))
}

func randomCode() (string, error) {
	out := make([]byte, codeLen)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("captcha: drawing a code: %w", err)
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}

// imageFor renders the code as SVG. Each character carries its own rotation
// and offset, and the lines behind and in front of the text make a simple
// text extractor see a document rather than a plain answer.
func imageFor(code string) string {
	var b strings.Builder
	const width, height = 220, 64
	b.WriteString(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`,
		width, height, width, height))
	b.WriteString(fmt.Sprintf(`<rect width="%d" height="%d" fill="#f8fafc"/>`, width, height))

	// Deterministic-looking noise is enough here: it has to obscure the
	// glyphs, not be a secret. crypto/rand would make each response larger
	// for no security gain.
	for i := 0; i < 7; i++ {
		x1 := 4 + (i*31)%200
		y1 := 8 + (i*17)%48
		x2 := 10 + (i*47)%210
		y2 := 6 + (i*29)%50
		b.WriteString(fmt.Sprintf(
			`<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#cbd5e1" stroke-width="1.4" opacity="0.8"/>`,
			x1, y1, x2, y2))
	}
	for i, ch := range code {
		x := 20 + i*38 + (i*11)%7
		y := 45 + (i*5)%5
		angle := -16 + (i*9)%33
		b.WriteString(fmt.Sprintf(
			`<text x="%d" y="%d" font-family="Verdana,Arial,sans-serif" font-size="36" font-weight="700" fill="#0f172a" transform="rotate(%d %d %d)">%s</text>`,
			x, y, angle, x, y, html.EscapeString(string(ch))))
	}
	for i := 0; i < 24; i++ {
		x := 4 + (i*37)%210
		y := 6 + (i*23)%52
		b.WriteString(fmt.Sprintf(`<circle cx="%d" cy="%d" r="1.1" fill="#94a3b8"/>`, x, y))
	}
	b.WriteString(`</svg>`)
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(b.String()))
}

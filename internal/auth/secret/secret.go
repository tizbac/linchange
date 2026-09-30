// Package secret generates and verifies the opaque tokens the server hands
// out: session refresh tokens, password reset tokens and TOTP recovery codes.
//
// Every one of these is a bearer credential. A database leak must not hand an
// attacker working sessions, so tokens are stored only as a keyed digest and
// compared in constant time.
package secret

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// tokenBytes is the entropy of a session or reset token. 32 bytes is 256 bits,
// which cannot be guessed and does not need to be memorable; these are only
// ever carried in a cookie or a URL the user just used.
const tokenBytes = 32

// ErrMalformed means a token could not be decoded, so it was never issued by
// this server.
var ErrMalformed = errors.New("secret: malformed token")

// Generate returns a new URL-safe token of n bytes of entropy.
//
// n is clamped to a sane range: a zero-length token is a bug that would turn a
// session cookie into a constant, and a megabyte one is a denial of service
// against anyone who has to store it.
func Generate(n int) (string, error) {
	switch {
	case n <= 0:
		n = tokenBytes
	case n > 64:
		n = 64
	}

	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secret: reading randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// GenerateToken is Generate at the default size.
func GenerateToken() (string, error) { return Generate(tokenBytes) }

// Hash returns the digest a token is stored as.
//
// This is a plain SHA-256 rather than a password hash, and that is deliberate.
// A password has low entropy and is guessed offline, so it needs argon2. A
// 256-bit token is not guessable offline, so a slow hash would only add latency
// to every authenticated request, and the database leak it protects against is
// already covered by the token's entropy.
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Equal compares a presented token against a stored digest in constant time.
//
// It returns false rather than an error for a mismatch, so a caller cannot
// accidentally branch on distinguishable failures.
func Equal(token string, stored []byte) bool {
	return subtle.ConstantTimeCompare(Hash(token), stored) == 1
}

// RecoveryCode is one single-use code for a second-factor bypass.
//
// Codes are drawn from a reduced alphabet on purpose. They have to be
// readable and writable by a person holding a printout, so ambiguity and
// transcription errors are the real risks; excluding the characters that look
// alike is worth more than the extra bits a full alphabet would provide.
type RecoveryCode struct {
	// Code is what the user types.
	Code string
	// Digest is what is stored.
	Digest []byte
}

// alphabet is Crockford-ish: digits plus unambiguous uppercase letters. I, L,
// O and U are omitted because people misread them as 1, 1, 0 and V.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// recoveryGroups is how many groups a code is split into, and recoveryGroupLen
// how long each group is. Four groups of four gives 16 characters, which is
// about 80 bits of entropy from a 32-symbol alphabet.
const (
	recoveryGroups    = 4
	recoveryGroupLen  = 4
	recoveryCodeChars = recoveryGroups * recoveryGroupLen
)

// GenerateRecoveryCodes returns n independent recovery codes.
//
// Drawing from a rejection-sampled alphabet keeps every code equally likely;
// taking len(alphabet) modulo a byte range would bias the early characters and
// quietly shrink the search space.
func GenerateRecoveryCodes(n int) ([]RecoveryCode, error) {
	if n <= 0 {
		return nil, errors.New("secret: recovery code count must be positive")
	}

	codes := make([]RecoveryCode, 0, n)
	for i := 0; i < n; i++ {
		code, err := generateRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, RecoveryCode{Code: code, Digest: Hash(NormalizeRecoveryCode(code))})
	}
	return codes, nil
}

func generateRecoveryCode() (string, error) {
	// One byte per character gives 32 symbols exactly, so no rejection is
	// needed, but keep the loop so the code stays correct if the alphabet is
	// ever changed to a size that is not a power of two.
	out := make([]byte, 0, recoveryCodeChars)
	for len(out) < recoveryCodeChars {
		buf := make([]byte, recoveryCodeChars)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("secret: reading randomness: %w", err)
		}
		for _, b := range buf {
			if len(out) == recoveryCodeChars {
				break
			}
			if int(b) < len(alphabet) {
				out = append(out, alphabet[b])
			}
		}
	}

	var b strings.Builder
	for i, c := range out {
		if i > 0 && i%recoveryGroupLen == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(c)
	}
	return b.String(), nil
}

// NormalizeRecoveryCode puts a user-typed code into the stored form: upper
// case, with the grouping dashes removed and surrounding space trimmed.
//
// Users retype these from paper, so it has to tolerate lowercase, a missing
// dash, and a trailing newline from a paste.
func NormalizeRecoveryCode(code string) string {
	var b strings.Builder
	b.Grow(len(code))
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			// Dashes, spaces and anything else are dropped.
		}
	}
	return b.String()
}

// VerifyRecoveryCode checks a typed code against stored digests and returns
// the index of the one that matched, so the caller can burn exactly that code.
//
// Every stored code is examined even after a match, and a match does not return
// early. An attacker must not be able to learn how many codes remain by timing
// a login attempt.
func VerifyRecoveryCode(typed string, stored [][]byte) (int, bool) {
	digest := Hash(NormalizeRecoveryCode(typed))

	matched := -1
	for i, candidate := range stored {
		if subtle.ConstantTimeCompare(digest, candidate) == 1 {
			matched = i
		}
	}
	return matched, matched >= 0
}

// Fingerprint returns a short, non-reversible identifier for a secret, safe to
// write to an audit log.
//
// It exists so an operator can correlate two log lines about the same token
// without the log containing anything usable.
func Fingerprint(secretValue []byte) string {
	mac := hmac.New(sha256.New, []byte("linchange-fingerprint"))
	mac.Write(secretValue)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:6])
}

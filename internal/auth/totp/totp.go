// Package totp implements the second factor: enrolment, code verification and
// recovery codes.
//
// Two things make this harder than calling a library. The secret has to be
// stored encrypted, because a database leak otherwise hands an attacker a
// permanent second factor that no password rotation can dislodge. And a code
// must not be accepted twice, or a shoulder-surfer who observes one code gets
// a window that outlasts the code itself.
package totp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/secret"
)

// Errors returned by this package. Callers map these onto user-facing messages
// and must not distinguish ErrInvalidCode from ErrReplay in a way that tells an
// attacker which codes have already been used.
var (
	// ErrInvalidCode means the code did not verify against the window.
	ErrInvalidCode = errors.New("totp: invalid code")
	// ErrReplay means the code was valid but its time step has already been
	// used. It is deliberately folded into ErrInvalidCode by the caller-facing
	// helper, because "correct but reused" is a stronger hint than "wrong".
	ErrReplay = errors.New("totp: code already used")
	// ErrNoSecret means the principal has no second factor enrolled.
	ErrNoSecret = errors.New("totp: no secret enrolled")
	// ErrDisabled means second-factor authentication is switched off.
	ErrDisabled = errors.New("totp: second factor is disabled")
	// ErrCorruptSecret means the stored secret could not be decrypted or
	// parsed, which is a key or data problem rather than a user error.
	ErrCorruptSecret = errors.New("totp: stored secret is unreadable")
	// ErrKeyRequired means no at-rest encryption key was configured.
	ErrKeyRequired = errors.New("totp: no encryption key configured")
)

// digest and digits follow RFC 6238. Anything else silently produces codes no
// authenticator app will accept.
const (
	digest = otp.AlgorithmSHA1
	digits = otp.DigitsSix
	// period is the TOTP time step in seconds.
	period = 30
	// issuerMaxLen bounds the issuer shown in an authenticator app, which has
	// very little room for it.
	issuerMaxLen = 32
)

// Verifier checks second-factor codes for one server.
type Verifier struct {
	issuer string
	// window is how many periods either side of now also verify, which is what
	// absorbs clock drift between the server and a phone.
	window int
	// recoveryCodes is how many codes to issue at enrolment.
	recoveryCodes int
	// aead encrypts secrets at rest.
	aead cipher.AEAD
	// keyID identifies the key, so the key can be rotated later without a
	// flag day.
	keyID string
}

// New builds a Verifier from configuration.
//
// The encryption key is required because the secrets cannot be stored safely
// without it, and a secret that cannot be stored is worse than no second
// factor: an operator would believe they had one.
func New(cfg config.TOTPAuth, encryptionKey string) (*Verifier, error) {
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	if strings.TrimSpace(encryptionKey) == "" {
		return nil, ErrKeyRequired
	}

	// Derive a fixed-size key from whatever the operator configured, so the
	// key can be an arbitrary passphrase or a base64 blob without this package
	// having to care which.
	sum := sha256.Sum256([]byte(encryptionKey))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("totp: building cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("totp: building AEAD: %w", err)
	}

	issuer := strings.TrimSpace(cfg.Issuer)
	if issuer == "" {
		issuer = "linchange"
	}
	if len(issuer) > issuerMaxLen {
		issuer = issuer[:issuerMaxLen]
	}

	window := int(cfg.Window)
	if window < 0 {
		window = 0
	}

	return &Verifier{
		issuer:        issuer,
		window:        window,
		recoveryCodes: cfg.RecoveryCodes,
		aead:          aead,
		keyID:         "v1",
	}, nil
}

// Issuer is the name shown in an authenticator app.
func (v *Verifier) Issuer() string { return v.issuer }

// RecoveryCodeCount is how many codes enrolment issues.
func (v *Verifier) RecoveryCodeCount() int { return v.recoveryCodes }

// Enrolment is what a user needs to set up an authenticator: the secret to
// type or scan, and a URI for a QR code.
type Enrolment struct {
	// Secret is the base32 shared secret, shown to the user once.
	Secret string
	// URI is the otpauth:// URI, for rendering as a QR code.
	URI string
	// RecoveryCodes are shown once and never again.
	RecoveryCodes []string
	// RecoveryDigests are the stored forms of RecoveryCodes, passed to
	// Encrypt when the caller persists the enrolment.
	RecoveryDigests [][]byte
}

// Enrol creates a new secret for a principal.
//
// The caller must not persist the secret until enrolment has been confirmed:
// an unconfirmed secret would let an administrator enrol a factor on a user's
// phone without their knowledge.
func (v *Verifier) Enrol(address string) (*Enrolment, error) {
	if strings.TrimSpace(address) == "" {
		return nil, errors.New("totp: address is required")
	}

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      v.issuer,
		AccountName: address,
		Period:      period,
		Digits:      digits,
		Algorithm:   digest,
		// 160 bits is the RFC 4226 recommendation and the widest every
		// authenticator app supports.
		SecretSize: 20,
	})
	if err != nil {
		return nil, fmt.Errorf("totp: generating secret: %w", err)
	}

	codes, err := secret.GenerateRecoveryCodes(v.recoveryCodes)
	if err != nil {
		return nil, err
	}
	plain := make([]string, len(codes))
	digests := make([][]byte, len(codes))
	for i, c := range codes {
		plain[i] = c.Code
		digests[i] = c.Digest
	}

	return &Enrolment{
		Secret:          key.Secret(),
		URI:             key.URL(),
		RecoveryCodes:   plain,
		RecoveryDigests: digests,
	}, nil
}

// EncryptedSecret is a secret ready for storage in user_totp.secret_encrypted.
type EncryptedSecret struct {
	// Ciphertext is the nonce followed by the sealed secret.
	Ciphertext []byte
	// RecoveryDigests are the hashes of the issued recovery codes, stored
	// alongside the secret so both are consumed together.
	RecoveryDigests [][]byte
}

// Encrypt seals a base32 secret for storage.
//
// The key ID is written into the ciphertext so a future key rotation can tell
// which key sealed a given row, and therefore which key is needed to open it.
func (v *Verifier) Encrypt(secretB32 string, recovery [][]byte) (*EncryptedSecret, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("totp: reading nonce: %w", err)
	}

	// The plaintext carries the key ID, then the secret, so that Decrypt can
	// route to the right key without a second column.
	plaintext := make([]byte, 0, len(v.keyID)+1+len(secretB32))
	plaintext = append(plaintext, v.keyID...)
	plaintext = append(plaintext, 0)
	plaintext = append(plaintext, secretB32...)

	sealed := v.aead.Seal(nonce, nonce, plaintext, nil)

	// Wipe the assembled plaintext. It is a second-factor secret sitting in
	// heap memory, and leaving it there for the garbage collector to copy is a
	// needless exposure.
	defer zero(plaintext)

	return &EncryptedSecret{Ciphertext: sealed, RecoveryDigests: recovery}, nil
}

// Decrypt opens a stored secret and reports which key sealed it.
func (v *Verifier) Decrypt(ciphertext []byte) (string, error) {
	nonceSize := v.aead.NonceSize()
	if len(ciphertext) < nonceSize+v.aead.Overhead() {
		return "", ErrCorruptSecret
	}

	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := v.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		// Either the data is damaged or the configured key is wrong. Both
		// mean the same thing to the caller.
		return "", ErrCorruptSecret
	}
	defer zero(plaintext)

	sep := indexByte(plaintext, 0)
	if sep < 0 {
		return "", ErrCorruptSecret
	}

	keyID := string(plaintext[:sep])
	if keyID != v.keyID {
		return "", fmt.Errorf("%w: sealed with key %q, this server holds %q",
			ErrCorruptSecret, keyID, v.keyID)
	}

	return string(plaintext[sep+1:]), nil
}

// Verify checks a code against a stored secret.
//
// lastUsed is the time step of the last code accepted, or the zero time. It is
// the anti-replay guard: a code from a step already spent is refused even
// though it is arithmetically correct.
//
// now is a parameter so the window and the replay guard can be tested at exact
// boundaries rather than by sleeping.
func (v *Verifier) Verify(storedCiphertext []byte, code string, lastUsed time.Time, now time.Time) (step int64, err error) {
	secretB32, err := v.Decrypt(storedCiphertext)
	if err != nil {
		return 0, err
	}

	code = strings.TrimSpace(code)
	if code == "" {
		return 0, ErrInvalidCode
	}

	ok, step, err := v.validate(secretB32, code, now)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrInvalidCode
	}

	// Refuse a code from a step already spent. A step that is only ahead of
	// the last one is fine, since that is normal clock drift.
	lastStep := lastUsed.Unix() / period
	if !lastUsed.IsZero() && step <= lastStep {
		return 0, ErrReplay
	}

	return step, nil
}

// validate checks the code against every step in the window and reports which
// step matched.
//
// The window is walked here rather than handed to the library because
// ValidateCustom answers only "is it valid", and the anti-replay guard needs to
// know precisely which step was spent. Deriving that separately would leave a
// gap: a code from the previous step and one from the current step are both
// valid, and only the caller can tell them apart.
func (v *Verifier) validate(secretB32, code string, now time.Time) (bool, int64, error) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if code == "" {
		return false, 0, ErrInvalidCode
	}

	opts := totp.ValidateOpts{
		Period:    period,
		Digits:    digits,
		Algorithm: digest,
	}

	opts.Skew = 0
	nowStep := now.Unix() / period

	for offset := -v.window; offset <= v.window; offset++ {
		step := nowStep + int64(offset)
		at := time.Unix(step*period, 0).UTC()

		candidate, err := totp.GenerateCodeCustom(secretB32, at, opts)
		if err != nil {
			// A secret the library cannot use is our problem, not the user's,
			// and must look like a wrong code from outside.
			return false, 0, ErrInvalidCode
		}

		if subtle.ConstantTimeCompare([]byte(candidate), []byte(code)) == 1 {
			return true, step, nil
		}
	}
	return false, 0, nil
}

// StepAt returns the time step for t, exposed so a caller can persist it as
// last_used_at and compare against it later.
func StepAt(t time.Time) int64 {
	return t.Unix() / period
}

// RecentlyUsed reports whether a stored last_used_at is plausible.
//
// A phone that is years out of date would otherwise have its entire remaining
// window accepted at once, and a database edit would too.
func RecentlyUsed(lastUsed time.Time, now time.Time) bool {
	return !lastUsed.IsZero() && now.Sub(lastUsed) < 365*24*time.Hour
}

// zero overwrites b. Go gives no way to guarantee a compiler or the garbage
// collector does not keep an earlier copy, so this is best effort, but leaving
// the original bytes in place is strictly worse.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// EncodeSecret is a helper for presenting a base32 secret to a user who prefers
// to type it rather than scan a QR code. The paddingless alphabet is what
// authenticator apps display.
func EncodeSecret(b32 string) string {
	return strings.ToUpper(strings.TrimRight(b32, "="))
}

// DecodeSecret accepts a user-typed base32 secret, tolerating the spaces and
// lowercase that come from a password manager or a printed backup.
func DecodeSecret(typed string) (string, error) {
	cleaned := strings.ToUpper(strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, typed))
	cleaned = strings.TrimRight(cleaned, "=")

	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(cleaned)
	if err != nil {
		return "", fmt.Errorf("totp: malformed base32 secret: %w", err)
	}
	if len(decoded) < 10 {
		return "", errors.New("totp: secret is too short to be a real one")
	}
	return cleaned, nil
}

// Fingerprint is a short, non-reversible identifier for an enrolled secret, so
// an operator can tell two enrolments apart in the audit log without the log
// holding anything usable.
func Fingerprint(secretB32 string) string {
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(
		strings.TrimRight(secretB32, "="))
	if err != nil {
		return ""
	}
	return secret.Fingerprint(decoded)
}

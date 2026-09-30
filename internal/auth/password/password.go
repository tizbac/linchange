// Package password hashes and verifies local user passwords with argon2id.
//
// Two properties matter more than the hashing itself, and both are easy to
// get wrong:
//
//   - Verification must take the same time whether or not the account exists.
//     A login endpoint that returns instantly for an unknown address leaks the
//     user list to anyone who measures. DummyVerify exists for that case.
//   - The stored parameters have to be recorded with the hash. When the
//     configured cost is raised, existing hashes must still verify and then be
//     transparently upgraded, so NeedsRehash has to be able to tell.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/linchange/linchange/config"
)

// Errors returned by Hash. Callers map these onto user-facing messages.
var (
	// ErrTooShort means the password is below the configured minimum.
	ErrTooShort = errors.New("password is too short")
	// ErrEmpty means there was no password at all.
	ErrEmpty = errors.New("password is empty")
)

// minSecretLength is the floor below which argon2 output stops being worth
// attacking, independent of the configured key length.
const minSecretLength = 16

// Hash derives a new hash for password using p, drawing a fresh random salt.
//
// The result is a PHC-format string:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>
//
// Recording the parameters alongside the hash is what allows the cost to be
// raised later without invalidating every existing password.
func Hash(password string, p config.Argon2Params) (string, error) {
	if password == "" {
		return "", ErrEmpty
	}

	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: reading salt: %w", err)
	}

	return encode(password, salt, p), nil
}

// Verify reports whether password matches the stored PHC hash.
//
// It returns a bool rather than an error for a mismatch, because a caller that
// logs or branches on the error would reintroduce the enumeration timing
// difference this package exists to avoid. A malformed hash is reported as an
// error, because that indicates corrupt data rather than a wrong password.
func Verify(password, stored string) (bool, error) {
	params, salt, want, err := decode(stored)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt, params.Iterations, params.MemoryKib, params.Parallelism, uint32(len(want)))

	// Constant time regardless of whether the digest matched.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether stored was produced with weaker parameters than
// p, and should therefore be replaced after a successful verification.
//
// This is how a cost increase reaches existing accounts without an admin
// having to reset anyone's password.
//
// The comparison is "weaker than", not "different from". A hash at a higher
// cost than currently configured must be left alone: downgrading it because an
// administrator lowered the configuration would weaken every account on the
// next login, and a hash is not something a user can be asked to redo.
func NeedsRehash(stored string, p config.Argon2Params) (bool, error) {
	used, _, _, err := decode(stored)
	if err != nil {
		return false, err
	}

	return used.MemoryKib < p.MemoryKib ||
		used.Iterations < p.Iterations ||
		used.KeyLength < p.KeyLength ||
		// Parallelism is not a security cost, so a lower value is not a
		// reason to touch a hash. It is compared only when configured
		// higher, for consistency with the rehash that follows.
		used.Parallelism < p.Parallelism, nil
}

// Validate applies the policy checks that apply before hashing. It is separate
// from Hash so a caller can validate a password it is not about to store, such
// as during a change-password form.
func Validate(password string, minLength int) error {
	if password == "" {
		return ErrEmpty
	}
	if minLength > 0 && len([]rune(password)) < minLength {
		return fmt.Errorf("%w: %d characters, need at least %d",
			ErrTooShort, len([]rune(password)), minLength)
	}
	return nil
}

// dummyHash is a real argon2id hash at the default cost, computed once. It is
// what DummyVerify runs against so that an unknown account costs the same as a
// known one.
var dummyHash = mustEncode("a password that no account has", []byte("0123456789abcdef"),
	config.Argon2Params{MemoryKib: 65536, Iterations: 3, Parallelism: 4, SaltLength: 16, KeyLength: 32})

// DummyVerify burns the same work as Verify against a hash no account can
// match.
//
// Call this whenever a login names an account that does not exist, before
// returning the same "invalid credentials" error the caller would return
// otherwise. Skipping it is the single easiest way to turn a login form into a
// user enumeration oracle.
//
// The extra work is not free, which is the point: a deployment that turns
// authentication off entirely should not call this at all.
func DummyVerify(password string) {
	_, _ = Verify(password, dummyHash)
}

func encode(password string, salt []byte, p config.Argon2Params) string {
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKib, p.Parallelism, p.KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKib, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

func mustEncode(password string, salt []byte, p config.Argon2Params) string {
	return encode(password, salt, p)
}

type encoded struct {
	MemoryKib   uint32
	Iterations  uint32
	Parallelism uint8
	KeyLength   uint32
}

// decode parses a PHC-format argon2id string. Only argon2id is accepted: the
// variants are not interchangeable, and silently falling back to a weaker one
// on a malformed value would be a downgrade.
func decode(stored string) (encoded, []byte, []byte, error) {
	var out encoded

	parts := strings.Split(stored, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[0] != "" {
		return out, nil, nil, errors.New("password: malformed hash")
	}
	if parts[1] != "argon2id" {
		return out, nil, nil, fmt.Errorf("password: unsupported hash variant %q", parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return out, nil, nil, errors.New("password: malformed hash version")
	}
	if version != argon2.Version {
		return out, nil, nil, fmt.Errorf("password: unsupported hash version %d", version)
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&out.MemoryKib, &out.Iterations, &out.Parallelism); err != nil {
		return out, nil, nil, errors.New("password: malformed hash parameters")
	}
	if out.MemoryKib == 0 || out.Iterations == 0 || out.Parallelism == 0 {
		return out, nil, nil, errors.New("password: hash parameters must be non-zero")
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return out, nil, nil, errors.New("password: malformed salt")
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return out, nil, nil, errors.New("password: malformed hash digest")
	}
	if len(salt) == 0 {
		return out, nil, nil, errors.New("password: empty salt")
	}
	if len(key) < minSecretLength {
		return out, nil, nil, fmt.Errorf("password: hash digest is only %d bytes", len(key))
	}

	out.KeyLength = uint32(len(key))
	return out, salt, key, nil
}

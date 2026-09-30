package login

import (
	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/password"
)

// dummyPassword is the plaintext behind the throwaway hash used to keep refused
// sign-ins as expensive as real ones. It is never a real password, and no
// account can be created with it, because it is only ever hashed and compared
// against in memory.
const dummyPassword = "a password that no account has"

// dummyHashFor builds a hash at the configured cost so a refused sign-in costs
// the same as an evaluated one.
//
// password.DummyVerify hashes against a fixed hash computed from the package's
// default parameters. If this deployment is configured for a higher cost than
// that default, every real verification is slower than the dummy, and the timing
// gap reappears for exactly the paths the dummy exists to close. Deriving the
// hash from the configuration is what makes the two the same work.
func dummyHashFor(p config.Argon2Params) (string, error) {
	return password.Hash(dummyPassword, p)
}

// verifyPassword checks a plaintext against a stored hash.
func verifyPassword(plaintext, stored string) (bool, error) {
	return password.Verify(plaintext, stored)
}

// rehashIfWeak returns a fresh hash for plaintext when the stored one was made
// with weaker parameters than p, and an empty string when the existing hash is
// fine.
//
// The empty string is what "nothing to do" looks like, and a caller that forgot
// to check for it would write an empty hash over a good one and lock the user
// out of their own account. Hash only fails on a random source failure, and that
// is returned rather than swallowed.
func rehashIfWeak(stored, plaintext string, p config.Argon2Params) (string, error) {
	needs, err := password.NeedsRehash(stored, p)
	if err != nil {
		return "", err
	}
	if !needs {
		return "", nil
	}
	return password.Hash(plaintext, p)
}

// dummyVerify spends the same work as a real verification of a password that
// matches nothing.
//
// It runs on every path that refuses a sign-in without checking a real hash: an
// unknown address, a locked account, a disabled account, a shared mailbox, a
// local account with no stored password. Any of those that answered faster than
// a wrong password would report which accounts exist.
func (a *Authenticator) dummyVerify(plaintext string) {
	// The bool is ignored deliberately: a dummy check that could report a match
	// would be a way to construct a credential for a user that has none.
	_, _ = password.Verify(plaintext, a.dummyHash)
}

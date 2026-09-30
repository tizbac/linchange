package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Credentials is the stored password for local users and domain admins.
//
// LDAP users have no row here, because their password is checked by binding to
// the directory and never held here. A shared mailbox has no row either: it is
// reached through grants, not a password.
type Credentials struct{}

// NewCredentials returns a repository. It is stateless, so this is free.
func NewCredentials() *Credentials { return &Credentials{} }

// Credential is one row of credentials.
type Credential struct {
	Address string `db:"address"`
	// Hash is the encoded argon2id PHC string. Parameters travel with the hash,
	// which is what lets the cost parameters be raised later without
	// invalidating anybody's password.
	Hash string `db:"password_hash"`
	// ChangedAt is when the password was last set. NULL for a user who has
	// never set one, which is normal for a directory-created account.
	ChangedAt sql.NullTime `db:"changed_at"`
}

// ErrNoCredential means the principal holds no password here. It is distinct
// from ErrNotFound because for an LDAP user it is the expected answer, and a
// caller that reports it as a missing account would be wrong about a user who
// very much exists.
var ErrNoCredential = errors.New("store: no stored credential")

// Get reads the stored credential for an address.
//
// ErrNoCredential is returned both for an address with no row and for one that
// does not exist, deliberately. The two are indistinguishable to a caller on
// purpose: telling them apart would turn this function into a way to enumerate
// accounts, and the caller cannot act on the difference anyway.
func (c *Credentials) Get(ctx context.Context, q Querier, address string) (Credential, error) {
	if address == "" {
		return Credential{}, ErrNoCredential
	}

	// must_change is not selected: it lives on users, since it is a property of
	// the principal rather than of the password. Two copies of one flag is two
	// things that can disagree, and the ways they disagree are both bad.
	const query = `
		SELECT address, password_hash, changed_at
		FROM credentials
		WHERE address = $1`

	var row Credential
	if err := q.GetContext(ctx, &row, query, address); err != nil {
		if IsNotFound(err) {
			return Credential{}, ErrNoCredential
		}
		return Credential{}, fmt.Errorf("store: reading credential: %w", err)
	}
	return row, nil
}

// Has reports whether a stored credential exists, without reading the hash.
//
// A login path that has already failed for another reason can use this to decide
// whether to spend an argon2 verification, which is the expensive part of a
// failed login. It exists to be called only when the answer does not matter: it
// is a boolean, so a caller must not turn it into a response to the client.
func (c *Credentials) Has(ctx context.Context, q Querier, address string) (bool, error) {
	const query = `SELECT 1 FROM credentials WHERE address = $1`

	var one int
	if err := q.GetContext(ctx, &one, query, address); err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: checking credential: %w", err)
	}
	return true, nil
}

// Set stores a new password hash for an address.
//
// The hash is an encoded argon2id PHC string from the password package, and
// that is checked here rather than trusted. A caller that passed a plaintext
// password would otherwise store it silently, and the only sign of that would
// be a plaintext password in the database.
//
// The forced-change flag is not set here. It is a property of the principal and
// lives on users, so a caller that needs it calls SetMustChangePassword; a
// password write and a forced change are separate decisions and this signature
// cannot express the wrong combination of them.
func (c *Credentials) Set(ctx context.Context, q Querier, address, hash string) error {
	if address == "" {
		return errors.New("store: a credential needs an address")
	}
	if err := checkPHCString(hash); err != nil {
		return err
	}

	const upsert = `
		INSERT INTO credentials (address, password_hash, changed_at)
		VALUES ($1, $2, now())
		ON CONFLICT (address) DO UPDATE
		SET password_hash = EXCLUDED.password_hash,
		    changed_at    = now()`

	if _, err := q.ExecContext(ctx, upsert, address, hash); err != nil {
		if IsConflict(err) {
			// The address has no users row, so there is nothing to hold a
			// password for.
			return ErrNotFound
		}
		return fmt.Errorf("store: setting credential: %w", err)
	}
	return nil
}

// Delete removes a stored credential, leaving the user row in place.
//
// This is how an LDAP-backed account stops being local, or how a shared mailbox
// is converted into something else: the principal survives, the password does
// not. Deleting a user is the cascade in the other direction.
func (c *Credentials) Delete(ctx context.Context, q Querier, address string) error {
	const del = `DELETE FROM credentials WHERE address = $1`
	if _, err := q.ExecContext(ctx, del, address); err != nil {
		return fmt.Errorf("store: deleting credential: %w", err)
	}
	return nil
}

// checkPHCString rejects anything that is not an encoded argon2 hash.
//
// This is a guard against a caller mistake, not a verifier: it recognises the
// shape of a PHC string and refuses everything else, including a plaintext
// password. It does not attempt to parse the parameters, so a malformed but
// plausibly shaped string would pass here and be caught at verification time.
func checkPHCString(hash string) error {
	if hash == "" {
		return errors.New("store: a credential needs a password hash")
	}
	if !strings.HasPrefix(hash, "$argon2") {
		return fmt.Errorf("store: refusing to store something that is not an "+
			"encoded argon2 hash (got %q); pass the output of password.Hash, "+
			"never a plaintext password", truncate(hash))
	}
	// A PHC string has at least the family, the parameters and the salt and
	// hash fields, so a bare "$argon2" is not one.
	if strings.Count(hash, "$") < 4 {
		return fmt.Errorf("store: %q is not a complete encoded hash", truncate(hash))
	}
	return nil
}

// truncate keeps a rejected value short enough to log without putting a
// password into a log line in full. A caller that passed a plaintext password
// will see the first few characters of it, which is a deliberate trade: the
// message is useless without showing what was wrong, and this value has already
// been refused, so it was never stored.
func truncate(s string) string {
	const limit = 12
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

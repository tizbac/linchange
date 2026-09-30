package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// TOTPRecord is one row of user_totp.
//
// Enrolments are held here for every principal that can sign in, including LDAP
// users, because a directory does not provide a second factor.
type TOTPRecord struct {
	Address string `db:"address"`
	// SecretEncrypted is the AES-256-GCM ciphertext of the base32 secret, with
	// the key id carried inside it so a rotation can tell which key sealed it.
	SecretEncrypted []byte `db:"secret_encrypted"`
	// Enabled is false for a secret that has been generated but not yet
	// confirmed. An unconfirmed secret must never be treated as a second factor,
	// or an administrator could enrol a factor on a user's phone without their
	// knowledge.
	Enabled bool `db:"enabled"`
	// RecoveryDigests are the hashes of the unused recovery codes. The column is
	// text[] while these are raw bytes, so the conversion happens in this
	// repository rather than being left to a caller to remember.
	RecoveryDigests [][]byte `db:"-"`
	// LastUsedAt is the time step of the last accepted code, which is what makes
	// a code single use.
	LastUsedAt sql.NullTime `db:"last_used_at"`
	// CreatedAt is when the enrolment row was written.
	CreatedAt time.Time `db:"created_at"`
}

// TOTP reads and writes second-factor enrolments.
type TOTP struct{}

// NewTOTP returns a repository. It is stateless, so this is free.
func NewTOTP() *TOTP { return &TOTP{} }

// ErrNoTOTP means the principal has no enrolment, which is the normal state for
// anyone who has not enrolled a factor.
var ErrNoTOTP = errors.New("store: no totp enrolment")

// Get reads the enrolment for an address.
func (t *TOTP) Get(ctx context.Context, q Querier, address string) (TOTPRecord, error) {
	if address == "" {
		return TOTPRecord{}, ErrNoTOTP
	}

	const query = `
		SELECT address, secret_encrypted, enabled, recovery_codes,
		       last_used_at, created_at
		FROM user_totp
		WHERE address = $1`

	var row TOTPRecord
	// The recovery codes live in a text[] the record does not hold, so the
	// query is scanned into a wrapper rather than adding a field to the exported
	// type that no caller wants.
	var scanned struct {
		TOTPRecord
		Recovery []string `db:"recovery_codes"`
	}
	if err := q.GetContext(ctx, &scanned, query, address); err != nil {
		if IsNotFound(err) {
			return TOTPRecord{}, ErrNoTOTP
		}
		return TOTPRecord{}, fmt.Errorf("store: reading totp enrolment: %w", err)
	}
	row = scanned.TOTPRecord

	// The column is text[] but the package produces raw digests, so the encoding
	// is this repository's business rather than the caller's. A value that will
	// not decode is an error rather than a silently dropped code, because a
	// recovery code that cannot be compared is a code that does not work.
	digests, err := decodeRecoveryDigests(scanned.Recovery)
	if err != nil {
		return TOTPRecord{}, err
	}
	row.RecoveryDigests = digests
	return row, nil
}

// Store writes an enrolment for a confirmed factor.
//
// The caller is responsible for having confirmed the factor first. Writing an
// enabled enrolment for a secret the user has not yet entered a code from is
// exactly the enrolment-without-knowledge case the enabled flag exists to
// prevent, so this does not accept a separate "confirmed" argument to get wrong:
// enabled is the only switch, and it is the caller's judgement.
func (t *TOTP) Store(ctx context.Context, q Querier, address string, secretEncrypted []byte, recoveryDigests [][]byte, enabled bool) error {
	if address == "" {
		return errors.New("store: a totp enrolment needs an address")
	}
	if len(secretEncrypted) == 0 {
		return errors.New("store: a totp enrolment needs an encrypted secret")
	}

	const upsert = `
		INSERT INTO user_totp (address, secret_encrypted, enabled, recovery_codes)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (address) DO UPDATE
		SET secret_encrypted = EXCLUDED.secret_encrypted,
		    enabled          = EXCLUDED.enabled,
		    recovery_codes   = EXCLUDED.recovery_codes,
		    -- A new enrolment starts with no spent steps, so the codes issued
		    -- with it are all live.
		    last_used_at     = NULL`

	if _, err := q.ExecContext(ctx, upsert, address, secretEncrypted, enabled,
		encodeRecoveryDigests(recoveryDigests)); err != nil {
		if IsConflict(err) {
			return ErrNotFound
		}
		return fmt.Errorf("store: storing totp enrolment: %w", err)
	}
	return nil
}

// MarkUsed records the time step a code was spent at.
//
// The step is written back rather than a wall clock reading, because what has to
// be compared is which window the previous code was spent in. Writing a plain
// timestamp and comparing it against a step boundary would be equivalent, and
// would be wrong the moment the two disagreed about where a window starts.
//
// The update refuses to move backwards. Two requests carrying the same code can
// pass verification concurrently, and without this the second would overwrite the
// first's step with an earlier one and the code would become reusable again. That
// makes this statement, not the verification, the thing that actually enforces
// single use.
func (t *TOTP) MarkUsed(ctx context.Context, q Querier, address string, step int64) error {
	const update = `
		UPDATE user_totp
		SET last_used_at = to_timestamp($2)
		WHERE address = $1
		  AND (last_used_at IS NULL OR last_used_at < to_timestamp($2))`

	if _, err := q.ExecContext(ctx, update, address, step*30); err != nil {
		return fmt.Errorf("store: marking totp used: %w", err)
	}
	return nil
}

// ConsumeRecoveryCode spends one recovery code and reports whether it was still
// live.
//
// The code is removed by digest value rather than by position, which is what
// makes it safe under concurrency. A caller verifies the typed code against the
// digests it read and gets back an index; if two requests present the same code
// at once, both verify and both arrive here. Removing by index would let the
// second one delete whichever code had shifted into that slot, silently
// destroying an unrelated code. Removing by value means the second update
// matches nothing, because the digest is already gone, and it is told so.
//
// Reporting false is therefore a real answer rather than a rare one: it means the
// code was already spent, which is what happens on a replay.
func (t *TOTP) ConsumeRecoveryCode(ctx context.Context, q Querier, address string, digest []byte) (bool, error) {
	if address == "" || len(digest) == 0 {
		return false, nil
	}

	const update = `
		UPDATE user_totp
		SET recovery_codes = array_remove(recovery_codes, $2)
		WHERE address = $1 AND $2 = ANY (recovery_codes)`

	res, err := q.ExecContext(ctx, update, address, base64.StdEncoding.EncodeToString(digest))
	if err != nil {
		return false, fmt.Errorf("store: consuming recovery code: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: consuming recovery code: %w", err)
	}
	return n > 0, nil
}

// Delete removes an enrolment, which is how a factor is unenrolled.
func (t *TOTP) Delete(ctx context.Context, q Querier, address string) error {
	const del = `DELETE FROM user_totp WHERE address = $1`
	if _, err := q.ExecContext(ctx, del, address); err != nil {
		return fmt.Errorf("store: deleting totp enrolment: %w", err)
	}
	return nil
}

// encodeRecoveryDigests renders digests for the text[] column.
//
// Base64 is used because a digest is opaque bytes with no text form of its own,
// and hex would double the size of a column holding a fixed, modest number of
// entries.
func encodeRecoveryDigests(digests [][]byte) []string {
	if len(digests) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(digests))
	for _, d := range digests {
		out = append(out, base64.StdEncoding.EncodeToString(d))
	}
	return out
}

// decodeRecoveryDigests reverses encodeRecoveryDigests.
func decodeRecoveryDigests(stored []string) ([][]byte, error) {
	out := make([][]byte, 0, len(stored))
	for i, s := range stored {
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("store: recovery code %d is not valid base64: %w", i, err)
		}
		out = append(out, raw)
	}
	return out, nil
}

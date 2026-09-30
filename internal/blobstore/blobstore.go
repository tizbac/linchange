// Package blobstore stores raw RFC 5322 message bytes on disk, addressed by
// their SHA-256 digest.
//
// Message bodies are kept out of PostgreSQL on purpose. Bodies are large,
// written once and read whole; keeping them in a content-addressed store gives
// deduplication for free (a message delivered to twenty users is stored once),
// makes quota arithmetic trivial, and keeps the database small enough to back
// up routinely.
//
// The store is deliberately minimal. There is no garbage collector here: a
// blob's lifetime is owned by the message rows that reference it, and
// collection is a separate concern that runs with full knowledge of the
// database.
package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Errors returned by the store.
var (
	// ErrNotFound is returned when no blob exists for a digest.
	ErrNotFound = errors.New("blobstore: blob not found")
	// ErrSizeMismatch is returned when a stored blob is not the length the
	// caller expected, which indicates corruption.
	ErrSizeMismatch = errors.New("blobstore: stored blob has an unexpected size")
	// ErrZeroDigest is returned when a caller supplies the unset digest. A zero
	// digest reaches here when a nullable blob_sha256 column is scanned as all
	// zeroes, and looking it up would turn a missing reference into a confusing
	// path error rather than the obvious "no blob" answer.
	ErrZeroDigest = errors.New("blobstore: digest is unset")
)

// Options configures a store.
type Options struct {
	// Root is the directory that holds the shards.
	Root string
	// FileMode is the permission on blob files. 0600 because message
	// contents are private.
	FileMode os.FileMode
	// DirMode is the permission on the shard directories.
	DirMode os.FileMode
}

// Store is a content-addressed blob store. It is safe for concurrent use.
type Store struct {
	root     string
	fileMode os.FileMode
	dirMode  os.FileMode

	// mu guards the per-shard mkdir cache, so that a burst of concurrent
	// writes to the same shard does not each perform a stat.
	mu         sync.Mutex
	madeShards map[byte]bool
}

// Open prepares the store root, creating it if necessary.
func Open(opts Options) (*Store, error) {
	if opts.Root == "" {
		return nil, errors.New("blobstore: root is required")
	}
	if opts.FileMode == 0 {
		opts.FileMode = 0o600
	}
	if opts.DirMode == 0 {
		opts.DirMode = 0o700
	}
	if err := os.MkdirAll(opts.Root, opts.DirMode); err != nil {
		return nil, fmt.Errorf("blobstore: creating root %s: %w", opts.Root, err)
	}
	return &Store{
		root:       opts.Root,
		fileMode:   opts.FileMode,
		dirMode:    opts.DirMode,
		madeShards: make(map[byte]bool, 256),
	}, nil
}

// Root returns the directory backing the store.
func (s *Store) Root() string { return s.root }

// Digest is the content address of a message, in the form stored in the
// messages table.
type Digest [32]byte

// DigestOf returns the digest of b.
func DigestOf(b []byte) Digest { return sha256.Sum256(b) }

// ParseDigest decodes a hex digest as produced by Digest.String.
func ParseDigest(s string) (Digest, error) {
	var d Digest
	raw, err := hex.DecodeString(s)
	if err != nil {
		return d, fmt.Errorf("blobstore: %q is not a hex digest: %w", s, err)
	}
	if len(raw) != len(d) {
		return d, fmt.Errorf("blobstore: digest must be %d bytes, got %d", len(d), len(raw))
	}
	copy(d[:], raw)
	if d.IsZero() {
		return d, fmt.Errorf("blobstore: %w: refusing to parse an all-zero digest", ErrZeroDigest)
	}
	return d, nil
}

// String renders the digest as lowercase hex.
func (d Digest) String() string { return hex.EncodeToString(d[:]) }

// IsZero reports whether the digest is unset.
func (d Digest) IsZero() bool { return d == Digest{} }

// path returns the on-disk location of a digest.
//
// A two-level fanout of the first byte of the digest keeps any single directory
// to roughly 1/256th of the store, which stays manageable well into the
// millions of messages.
func (s *Store) path(d Digest) string {
	return filepath.Join(s.root,
		fmt.Sprintf("%02x", d[0]),
		fmt.Sprintf("%02x", d[1]),
		d.String())
}

// ensureShard creates the directory for d's shard if it does not exist.
func (s *Store) ensureShard(d Digest) error {
	shard := d[0]
	s.mu.Lock()
	if s.madeShards[shard] {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	dir := filepath.Join(s.root, fmt.Sprintf("%02x", shard), fmt.Sprintf("%02x", d[1]))
	if err := os.MkdirAll(dir, s.dirMode); err != nil {
		return fmt.Errorf("blobstore: creating shard %s: %w", dir, err)
	}

	s.mu.Lock()
	s.madeShards[shard] = true
	s.mu.Unlock()
	return nil
}

// Put stores b and returns its digest and length.
//
// A second Put of identical bytes is a no-op that returns the same digest, so
// callers can call this unconditionally. The write goes to a temporary file in
// the same directory and is then renamed, because a rename within a filesystem
// is atomic: a reader never observes a partially written message, which would
// otherwise be a message truncated mid-header and served over IMAP.
func (s *Store) Put(b []byte) (Digest, int64, error) {
	d := DigestOf(b)
	if err := s.ensureShard(d); err != nil {
		return d, 0, err
	}

	final := s.path(d)
	if _, err := os.Stat(final); err == nil {
		return d, int64(len(b)), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return d, 0, fmt.Errorf("blobstore: checking %s: %w", final, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(final), ".tmp-*")
	if err != nil {
		return d, 0, fmt.Errorf("blobstore: creating temp file: %w", err)
	}
	tmpName := tmp.Name()

	// Every failure past this point must remove the temp file, or a crash
	// loop would slowly fill the store with partial messages.
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	if _, err := tmp.Write(b); err != nil {
		cleanup()
		return d, 0, fmt.Errorf("blobstore: writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return d, 0, fmt.Errorf("blobstore: syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return d, 0, fmt.Errorf("blobstore: closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, s.fileMode); err != nil {
		os.Remove(tmpName)
		return d, 0, fmt.Errorf("blobstore: chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return d, 0, fmt.Errorf("blobstore: renaming into place: %w", err)
	}

	// The directory entry needs to be durable too, otherwise a crash can lose
	// the blob even though the file contents were synced.
	syncDir(filepath.Dir(final))

	return d, int64(len(b)), nil
}

// Get returns the bytes of a blob.
func (s *Store) Get(d Digest) ([]byte, error) {
	if d.IsZero() {
		return nil, ErrZeroDigest
	}
	f, err := os.Open(s.path(d))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, d)
		}
		return nil, fmt.Errorf("blobstore: opening %s: %w", d, err)
	}
	defer f.Close()

	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("blobstore: reading %s: %w", d, err)
	}
	return b, nil
}

// GetSize returns the length of a stored blob without reading it, which is what
// IMAP's RFC822.SIZE and POP3's STAT and LIST need.
func (s *Store) GetSize(d Digest) (int64, error) {
	if d.IsZero() {
		return 0, ErrZeroDigest
	}
	info, err := os.Stat(s.path(d))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("%w: %s", ErrNotFound, d)
		}
		return 0, fmt.Errorf("blobstore: stat %s: %w", d, err)
	}
	return info.Size(), nil
}

// Open returns a reader over a blob. The caller closes it.
//
// This exists so that a large IMAP FETCH or POP3 RETR streams from disk rather
// than materialising the whole message in memory.
func (s *Store) Open(d Digest) (*os.File, error) {
	if d.IsZero() {
		return nil, ErrZeroDigest
	}
	f, err := os.Open(s.path(d))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, d)
		}
		return nil, fmt.Errorf("blobstore: opening %s: %w", d, err)
	}
	return f, nil
}

// Exists reports whether a blob is present.
func (s *Store) Exists(d Digest) bool {
	_, err := os.Stat(s.path(d))
	return err == nil
}

// Delete removes a blob.
//
// A missing blob is not an error: deletion is idempotent, because the caller
// is usually reacting to a database row going away and the row is the source
// of truth.
func (s *Store) Delete(d Digest) error {
	if d.IsZero() {
		return ErrZeroDigest
	}
	err := os.Remove(s.path(d))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blobstore: deleting %s: %w", d, err)
	}
	return nil
}

// syncDir flushes a directory entry. Failure is not reported: on filesystems
// that do not support it the blob is still correct, and refusing to store mail
// because of an fsync quirk would be worse than the durability gap.
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Sync()
}

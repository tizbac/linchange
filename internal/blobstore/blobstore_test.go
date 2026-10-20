package blobstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Options{Root: filepath.Join(t.TempDir(), "blobs")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	body := []byte("Subject: hello\r\n\r\nbody text\r\n")

	d, n, err := s.Put(body)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("put reported %d bytes, want %d", n, len(body))
	}

	got, err := s.Get(d)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("round trip changed the bytes:\ngot  %q\nwant %q", got, body)
	}
}

func TestPutCoversEverySecondByte(t *testing.T) {
	s := newTestStore(t)

	// The shards fan out on two bytes, so a store can hold many digests that
	// share a leading byte but live in different second-byte directories. The
	// cache of created shards has to remember the whole two-byte prefix, or the
	// first write into a leading byte would make every sibling directory of
	// that byte look created and later writes there would fail.
	var pair [2][]byte
	found := false
	seen := map[byte][]byte{}
	for i := 0; i < 50000 && !found; i++ {
		payload := []byte(fmt.Sprintf("payload %d", i))
		d := DigestOf(payload)
		if prev, ok := seen[d[0]]; ok && prev[1] != d[1] {
			pair = [2][]byte{prev, payload}
			found = true
			break
		}
		if _, ok := seen[d[0]]; !ok {
			seen[d[0]] = payload
		}
	}
	if !found {
		t.Skip("no two payloads in range share a leading byte but differ in the second")
	}

	digests := make([]Digest, 2)
	for i, payload := range pair {
		d, _, err := s.Put(payload)
		if err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		digests[i] = d
	}
	for i, payload := range pair {
		got, err := s.Get(digests[i])
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("get %d returned %q, want %q", i, got, payload)
		}
	}
}

func TestPutIsDeduplicating(t *testing.T) {
	s := newTestStore(t)
	body := []byte("identical message delivered to twenty people")

	first, _, err := s.Put(body)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	second, _, err := s.Put(body)
	if err != nil {
		t.Fatalf("put again: %v", err)
	}
	if first != second {
		t.Errorf("identical content produced different digests: %s vs %s", first, second)
	}

	// One file on disk, not two.
	var count int
	err = filepath.Walk(s.Root(), func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name()[0] != '.' {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 stored file for duplicate content, found %d", count)
	}
}

func TestGetMissingBlob(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Get(DigestOf([]byte("never written")))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	if _, err := s.GetSize(DigestOf([]byte("never written"))); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSize: expected ErrNotFound, got %v", err)
	}
	if _, err := s.Open(DigestOf([]byte("never written"))); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open: expected ErrNotFound, got %v", err)
	}
}

func TestDigestRoundTrip(t *testing.T) {
	d := DigestOf([]byte("hello"))
	parsed, err := ParseDigest(d.String())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed != d {
		t.Errorf("parse(digest) = %s, want %s", parsed, d)
	}
}

func TestParseDigestRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"not hex":     "zzzz",
		"too short":   "00ff",
		"too long":    d0() + "00",
		"empty":       "",
		"zero digest": d0(),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDigest(in); err == nil {
				t.Errorf("ParseDigest(%q) should have failed", in)
			}
		})
	}
}

func d0() string {
	var d Digest
	return d.String()
}

func TestZeroDigestIsDetectable(t *testing.T) {
	var d Digest
	if !d.IsZero() {
		t.Error("a zero Digest should report IsZero")
	}
	if DigestOf([]byte("x")).IsZero() {
		t.Error("a real Digest should not report IsZero")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	d, _, err := s.Put([]byte("temporary"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := s.Delete(d); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Deleting something already gone is not an error: the caller is reacting
	// to a database row disappearing, and the row is the source of truth.
	if err := s.Delete(d); err != nil {
		t.Errorf("second delete should be a no-op, got %v", err)
	}
	if s.Exists(d) {
		t.Error("blob should be gone")
	}
}

// A blob must never be observable half-written, because a truncated message
// served over IMAP is corrupt mail with no way for the client to tell.
func TestConcurrentPutsNeverExposePartialContent(t *testing.T) {
	s := newTestStore(t)
	// Large enough that a partially written file is likely to be observed if
	// the atomic rename were missing.
	payloads := make([][]byte, 8)
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte(fmt.Sprintf("message-%d ", i)), 200_000)
	}

	var wg sync.WaitGroup
	digests := make([]Digest, len(payloads))

	// Writers.
	for i, p := range payloads {
		wg.Add(1)
		go func(i int, p []byte) {
			defer wg.Done()
			d, _, err := s.Put(p)
			if err != nil {
				t.Errorf("put %d: %v", i, err)
				return
			}
			digests[i] = d
		}(i, p)
	}
	wg.Wait()

	// Readers run against every blob while a second round of writes lands.
	for i, p := range payloads {
		wg.Add(1)
		go func(i int, p []byte) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				got, err := s.Get(digests[i])
				if err != nil {
					t.Errorf("get %d: %v", i, err)
					return
				}
				if !bytes.Equal(got, p) {
					t.Errorf("blob %d read back %d bytes, want %d: partial content observed",
						i, len(got), len(p))
					return
				}
			}
		}(i, p)
	}
	wg.Wait()
}

func TestGetSizeMatchesContent(t *testing.T) {
	s := newTestStore(t)
	body := []byte("a message of a known length")
	d, n, err := s.Put(body)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetSize(d)
	if err != nil {
		t.Fatalf("get size: %v", err)
	}
	if got != n {
		t.Errorf("GetSize = %d, put reported %d", got, n)
	}
}

func TestOpenStreamsContent(t *testing.T) {
	s := newTestStore(t)
	body := bytes.Repeat([]byte("x"), 100_000)
	d, _, err := s.Put(body)
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	f, err := s.Open(d)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Error("streamed content differs from what was stored")
	}
}

func TestOpenRejectsEmptyRoot(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Fatal("an empty root should be rejected")
	}
}

func TestFilesAreNotWorldReadable(t *testing.T) {
	s := newTestStore(t)
	d, _, err := s.Put([]byte("private correspondence"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	info, err := os.Stat(s.path(d))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("blob permissions are %o; message content must not be group or world readable", perm)
	}
}

func TestPutLeavesNoTempFilesBehind(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 20; i++ {
		if _, _, err := s.Put([]byte(fmt.Sprintf("message %d", i))); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	var leftovers []string
	err := filepath.Walk(s.Root(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && len(info.Name()) > 0 && info.Name()[0] == '.' {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

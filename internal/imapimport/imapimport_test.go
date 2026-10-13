package imapimport

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key := deriveKey("test-encryption-key")
	sealed, err := encryptPassword(key, "s3cret-pa55")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := decryptPassword(key, sealed)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "s3cret-pa55" {
		t.Errorf("got %q, want %q", got, "s3cret-pa55")
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	key := deriveKey("key-a")
	sealed, err := encryptPassword(key, "password")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := decryptPassword(deriveKey("key-b"), sealed); err == nil {
		t.Fatal("expected error decrypting with wrong key")
	}
}

func TestUnderRoot(t *testing.T) {
	cases := []struct {
		name, root string
		delim      rune
		want       bool
	}{
		{"INBOX", "", '/', true},
		{"INBOX", "INBOX", '/', true},
		{"INBOX/Work", "INBOX", '/', true},
		{"Work", "INBOX", '/', false},
		{"INBOX.Work", "INBOX", '.', true},
		{"INBOXWork", "INBOX", '/', false},
	}
	for _, c := range cases {
		if got := underRoot(c.name, c.root, c.delim); got != c.want {
			t.Errorf("underRoot(%q, %q, %q) = %v, want %v", c.name, c.root, string(c.delim), got, c.want)
		}
	}
}

func TestTargetName(t *testing.T) {
	cases := []struct {
		sourceRoot, targetRoot, folder string
		delim                          rune
		want                           string
	}{
		{"", "", "INBOX", '/', "INBOX"},
		{"INBOX", "Imported", "INBOX", '/', "Imported"},
		{"INBOX", "Imported", "INBOX/Work", '/', "Imported/Work"},
		{"INBOX", "", "INBOX", '/', "INBOX"},
		{"Archive", "Old", "Archive", '.', "Old"},
		{"", "Imported", "INBOX", '/', "Imported/INBOX"},
		{"INBOX", "Imported", "INBOX.Trash", '.', "Imported/Trash"},
	}
	for _, c := range cases {
		got := targetName(c.sourceRoot, c.targetRoot, c.folder, c.delim)
		if got != c.want {
			t.Errorf("targetName(%q, %q, %q, %q) = %q, want %q",
				c.sourceRoot, c.targetRoot, c.folder, string(c.delim), got, c.want)
		}
	}
}

func TestImportFlagsDropsRecent(t *testing.T) {
	got := importFlags([]imap.Flag{`\Seen`, `\Flagged`, `\Recent`})
	want := []string{`\Seen`, `\Flagged`}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
			break
		}
	}
}

func TestIsPermanentSyncError(t *testing.T) {
	if !isPermanentSyncError(nil) {
		t.Error("nil should be permanent (nothing to retry)")
	}
	if !isPermanentSyncError(errString("imapimport: quota exceeded for a@b.c")) {
		t.Error("quota exceeded should be permanent")
	}
	if isPermanentSyncError(errString("connection reset by peer")) {
		t.Error("connection reset should not be permanent")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

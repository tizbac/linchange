package imapd_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	imapd "github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

// newTestMailbox creates a user with an INBOX and returns the database and the
// INBOX id.
func newTestMailbox(t *testing.T, address string) (*sqlx.DB, int64) {
	t.Helper()

	db := testdb.Migrated(t)
	ctx := context.Background()

	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: address,
		Source:  "local",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	var mailboxID int64
	if err := db.GetContext(ctx, &mailboxID,
		`INSERT INTO mailboxes (owner, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, 1) RETURNING id`, address); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	return db, mailboxID
}

func describe(t *testing.T, raw string) mail.Metadata {
	t.Helper()
	meta, err := mail.Describe([]byte(raw))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	return meta
}

const threadedRoot = "From: alice@example.com\r\n" +
	"To: bob@example.com, Carol <carol@example.com>\r\n" +
	"Cc: dave@example.com\r\n" +
	"Subject: Q3 plan\r\n" +
	"Message-ID: <root@example.com>\r\n" +
	"Date: Mon, 02 Jan 2006 15:04:05 +0000\r\n" +
	"Content-Type: multipart/mixed; boundary=\"b\"\r\n" +
	"\r\n" +
	"--b\r\nContent-Type: text/plain\r\n\r\nThe plan is in the attached deck.\r\n" +
	"--b\r\nContent-Type: application/pdf; name=\"deck.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"deck.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n" +
	"--b--\r\n"

// A message row has to carry enough to list, search and thread a mailbox
// without reading the blob again.
func TestInsertPersistsMessageMetadata(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	ctx := context.Background()
	repo := imapd.NewMessageRepo()

	meta := describe(t, threadedRoot)
	result, err := repo.Insert(ctx, db, mailboxID, []byte("digest"), 1234,
		meta.Date, []string{`\Recent`}, meta)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if result.UID != 1 {
		t.Errorf("UID = %d, want the first UID in a fresh mailbox", result.UID)
	}

	var row struct {
		Subject        string `db:"subject"`
		FromAddress    string `db:"from_address"`
		FromName       string `db:"from_name"`
		Snippet        string `db:"snippet"`
		MessageID      string `db:"message_id"`
		ThreadKey      string `db:"thread_key"`
		ThreadSubject  string `db:"thread_subject"`
		HasAttachments bool   `db:"has_attachments"`
	}
	if err := db.GetContext(ctx, &row, `
		SELECT subject, from_address, from_name, snippet, message_id,
		       thread_key, thread_subject, has_attachments
		FROM messages WHERE id = $1`, result.ID); err != nil {
		t.Fatalf("read back message: %v", err)
	}

	if row.Subject != "Q3 plan" {
		t.Errorf("subject = %q", row.Subject)
	}
	if row.FromAddress != "alice@example.com" {
		t.Errorf("from_address = %q", row.FromAddress)
	}
	if row.MessageID != "root@example.com" {
		t.Errorf("message_id = %q, want the real Message-ID, not a generated one", row.MessageID)
	}
	if row.ThreadKey != "root@example.com" {
		t.Errorf("thread_key = %q, want the root of the References chain", row.ThreadKey)
	}
	if row.ThreadSubject != "q3 plan" {
		t.Errorf("thread_subject = %q", row.ThreadSubject)
	}
	if !row.HasAttachments {
		t.Error("has_attachments = false, want true")
	}
	// The snippet has to come from the body, or full-text search cannot match
	// anything anyone wrote.
	if !strings.Contains(row.Snippet, "plan is in the attached deck") {
		t.Errorf("snippet = %q, want body text", row.Snippet)
	}
}

// A UUID in thread_key would have made every message its own thread.
func TestInsertThreadsAReplyWithItsParent(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	ctx := context.Background()
	repo := imapd.NewMessageRepo()

	rootMeta := describe(t, threadedRoot)
	root, err := repo.Insert(ctx, db, mailboxID, []byte("d1"), 100, rootMeta.Date, nil, rootMeta)
	if err != nil {
		t.Fatalf("insert root: %v", err)
	}

	const reply = "From: carol@example.com\r\nTo: bob@example.com\r\n" +
		"Subject: Re: Q3 plan\r\nMessage-ID: <reply@example.com>\r\n" +
		"In-Reply-To: <root@example.com>\r\nReferences: <root@example.com>\r\n" +
		"\r\nLooks good to me.\r\n"
	replyMeta := describe(t, reply)
	if _, err := repo.Insert(ctx, db, mailboxID, []byte("d2"), 100, replyMeta.Date, nil, replyMeta); err != nil {
		t.Fatalf("insert reply: %v", err)
	}

	// The grouping query a reader would run.
	rows, err := db.QueryxContext(ctx,
		`SELECT count(DISTINCT thread_key) FROM messages WHERE mailbox_id = $1`, mailboxID)
	if err != nil {
		t.Fatalf("count threads: %v", err)
	}
	defer rows.Close()

	rows.Next()
	var threads int
	if err := rows.Scan(&threads); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if threads != 1 {
		t.Errorf("distinct thread keys = %d, want 1: the reply must group with %d", threads, root.ID)
	}
}

// The address indexes are what IMAP SEARCH and recipient lookup read, so they
// have to be written at insert time rather than re-derived from the blob.
func TestInsertWritesAddressIndexes(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	ctx := context.Background()

	meta := describe(t, threadedRoot)
	result, err := imapd.NewMessageRepo().Insert(ctx, db, mailboxID, []byte("d"), 100, meta.Date, nil, meta)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	type row struct {
		Header string `db:"header_name"`
		Addr   string `db:"address"`
		Name   string `db:"display_name"`
	}
	var rows []row
	if err := db.SelectContext(ctx, &rows, `
		SELECT header_name, address, display_name
		FROM message_addresses WHERE message_id = $1 ORDER BY header_name, position`, result.ID); err != nil {
		t.Fatalf("read addresses: %v", err)
	}

	want := map[string][]string{
		"From": {"alice@example.com"},
		"To":   {"bob@example.com", "carol@example.com"},
		"Cc":   {"dave@example.com"},
	}
	for header, addrs := range want {
		var got []string
		for _, r := range rows {
			if r.Header == header {
				got = append(got, r.Addr)
			}
		}
		if len(got) != len(addrs) {
			t.Fatalf("%s indexed as %v, want %v", header, got, addrs)
		}
		for i := range addrs {
			if got[i] != addrs[i] {
				t.Errorf("%s[%d] = %q, want %q", header, i, got[i], addrs[i])
			}
		}
	}

	// The display name has to survive, since a list shows it next to the
	// address.
	for _, r := range rows {
		if r.Header == "To" && r.Addr == "carol@example.com" && r.Name != "Carol" {
			t.Errorf("Carol's display name = %q, want it stored", r.Name)
		}
	}
}

func TestInsertWritesReferences(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	ctx := context.Background()

	const reply = "From: b@example.com\r\nTo: a@example.com\r\nSubject: Re: x\r\n" +
		"Message-ID: <r@e.com>\r\nReferences: <root@e.com> <mid@e.com>\r\n\r\nbody\r\n"
	meta := describe(t, reply)
	result, err := imapd.NewMessageRepo().Insert(ctx, db, mailboxID, []byte("d"), 100, meta.Date, nil, meta)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var refs []string
	if err := db.SelectContext(ctx, &refs, `
		SELECT reference FROM message_references WHERE message_id = $1 ORDER BY position`, result.ID); err != nil {
		t.Fatalf("read references: %v", err)
	}
	if len(refs) != 2 || refs[0] != "root@e.com" || refs[1] != "mid@e.com" {
		t.Errorf("references = %v, want the chain in order", refs)
	}
}

// Two deliveries to one mailbox can run at once. Reading uidnext and writing it
// back would hand both the same UID and lose one of them.
func TestInsertAssignsDistinctUIDsUnderConcurrency(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	ctx := context.Background()
	repo := imapd.NewMessageRepo()

	meta := describe(t, threadedRoot)

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan uint32, workers)
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := repo.Insert(ctx, db, mailboxID, []byte("d"), 100, meta.Date, nil, meta)
			if err != nil {
				errs <- err
				return
			}
			results <- res.UID
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Insert: %v", err)
		return
	}

	seen := map[uint32]bool{}
	for uid := range results {
		if seen[uid] {
			t.Errorf("UID %d handed out twice", uid)
		}
		seen[uid] = true
	}
	if len(seen) != workers {
		t.Errorf("distinct UIDs = %d, want %d", len(seen), workers)
	}

	// uidnext has to end up past every UID handed out.
	var uidnext uint32
	if err := db.GetContext(ctx, &uidnext,
		`SELECT uidnext FROM mailboxes WHERE id = $1`, mailboxID); err != nil {
		t.Fatalf("read uidnext: %v", err)
	}
	if uidnext <= uint32(workers) {
		t.Errorf("uidnext = %d, want it past every assigned UID", uidnext)
	}
}

func TestInsertRollsBackWhenTheMessageFails(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	ctx := context.Background()
	repo := imapd.NewMessageRepo()

	// A message_id that collides with the primary key on another table is not
	// available, so instead force a failure the transaction must roll back: a
	// reference longer than the column allows would truncate silently, so use a
	// bad size that violates the CHECK constraint.
	meta := describe(t, threadedRoot)
	if _, err := repo.Insert(ctx, db, mailboxID, []byte("d"), -1, meta.Date, nil, meta); err == nil {
		t.Fatal("Insert with a negative size succeeded, want the CHECK constraint to reject it")
	}

	var count int
	if err := db.GetContext(ctx, &count,
		`SELECT count(*) FROM messages WHERE mailbox_id = $1`, mailboxID); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 0 {
		t.Errorf("messages = %d after a rolled back insert, want 0", count)
	}

	// The reserved UID must not be consumed either, or every failure would
	// leave a permanent hole in the sequence.
	var uidnext uint32
	if err := db.GetContext(ctx, &uidnext,
		`SELECT uidnext FROM mailboxes WHERE id = $1`, mailboxID); err != nil {
		t.Fatalf("read uidnext: %v", err)
	}
	if uidnext != 1 {
		t.Errorf("uidnext = %d after a rolled back insert, want it unchanged at 1", uidnext)
	}
}

// A move relocates the row. Re-inserting it instead would mint a new message
// ID and a new thread key, splitting a conversation in half.
func TestMoveToPreservesMessageIdentity(t *testing.T) {
	db, inboxID := newTestMailbox(t, "bob@example.com")
	ctx := context.Background()
	repo := imapd.NewMessageRepo()

	var projectsID int64
	if err := db.GetContext(ctx, &projectsID, `
		INSERT INTO mailboxes (owner, name, uidvalidity, uidnext)
		VALUES ('bob@example.com', 'Projects', 1, 1) RETURNING id`); err != nil {
		t.Fatalf("create Projects: %v", err)
	}

	meta := describe(t, threadedRoot)
	inserted, err := repo.Insert(ctx, db, inboxID, []byte("d"), 100, meta.Date, []string{`\Recent`}, meta)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// A copy first, so the move has something to be grouped with.
	if _, err := db.ExecContext(ctx,
		`UPDATE mailboxes SET uidnext = 2 WHERE id = $1`, inboxID); err != nil {
		t.Fatalf("bump uidnext: %v", err)
	}

	destUIDs, err := repo.MoveTo(ctx, db, inboxID, []uint32{inserted.UID}, projectsID)
	if err != nil {
		t.Fatalf("MoveTo: %v", err)
	}
	if len(destUIDs) != 1 {
		t.Fatalf("destUIDs = %v, want one", destUIDs)
	}

	var moved struct {
		ID          int64  `db:"id"`
		MailboxID   int64  `db:"mailbox_id"`
		MessageID   string `db:"message_id"`
		ThreadKey   string `db:"thread_key"`
		BlobSHA256  []byte `db:"blob_sha256"`
		AddressRows int    `db:"address_rows"`
	}
	if err := db.GetContext(ctx, &moved, `
		SELECT m.id, m.mailbox_id, COALESCE(m.message_id, '') AS message_id,
		       m.thread_key, m.blob_sha256,
		       (SELECT count(*) FROM message_addresses a WHERE a.message_id = m.id) AS address_rows
		FROM messages m WHERE m.uid = $1 AND m.mailbox_id = $2`,
		destUIDs[0], projectsID); err != nil {
		t.Fatalf("read moved message: %v", err)
	}

	if moved.ID != inserted.ID {
		t.Errorf("moved row id = %d, want the same row %d", moved.ID, inserted.ID)
	}
	if moved.MessageID != meta.MessageID {
		t.Errorf("message_id = %q after the move, want it unchanged", moved.MessageID)
	}
	if moved.ThreadKey != meta.ThreadKey {
		t.Errorf("thread_key = %q after the move, want it unchanged", moved.ThreadKey)
	}
	if string(moved.BlobSHA256) != "d" {
		t.Errorf("blob_sha256 = %q, want it unchanged", moved.BlobSHA256)
	}
	if moved.AddressRows == 0 {
		t.Error("the moved message lost its address index rows")
	}

	// \Recent must not survive a move: the client has already seen the message.
	var flags []string
	if err := db.GetContext(ctx, &flags,
		`SELECT flags FROM messages WHERE id = $1`, moved.ID); err != nil {
		t.Fatalf("read flags: %v", err)
	}
	for _, f := range flags {
		if f == `\Recent` {
			t.Error(`\Recent survived the move`)
		}
	}
}

func TestMoveToReportsAMissingMessage(t *testing.T) {
	db, inboxID := newTestMailbox(t, "bob@example.com")
	ctx := context.Background()

	var projectsID int64
	if err := db.GetContext(ctx, &projectsID, `
		INSERT INTO mailboxes (owner, name, uidvalidity, uidnext)
		VALUES ('bob@example.com', 'Projects', 1, 1) RETURNING id`); err != nil {
		t.Fatalf("create Projects: %v", err)
	}

	if _, err := imapd.NewMessageRepo().MoveTo(ctx, db, inboxID, []uint32{99}, projectsID); err == nil {
		t.Fatal("MoveTo for a UID that is not there succeeded, want an error")
	}
}

// Quota has to come back when a message goes away, or a user slowly runs out
// of space they never used.
func TestDeleteByUIDReturnsQuota(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	ctx := context.Background()

	const size = 4096
	if _, err := db.ExecContext(ctx,
		`INSERT INTO quota_usage (owner, used_bytes, message_count)
		 VALUES ('bob@example.com', 0, 0)`); err != nil {
		t.Fatalf("seed quota: %v", err)
	}

	meta := describe(t, threadedRoot)
	inserted, err := imapd.NewMessageRepo().Insert(ctx, db, mailboxID, []byte("d"), size, meta.Date, nil, meta)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := store.NewQuotas().Adjust(ctx, db, "bob@example.com", size, 1); err != nil {
		t.Fatalf("Adjust: %v", err)
	}

	if err := imapd.NewMessageRepo().DeleteByUID(ctx, db, mailboxID, inserted.UID); err != nil {
		t.Fatalf("DeleteByUID: %v", err)
	}

	usage, err := store.NewQuotas().Get(ctx, db, "bob@example.com")
	if err != nil {
		t.Fatalf("Get quota: %v", err)
	}
	if usage.UsedBytes != 0 {
		t.Errorf("used_bytes = %d after deleting a %d byte message, want 0", usage.UsedBytes, size)
	}
	if usage.MessageCount != 0 {
		t.Errorf("message_count = %d, want 0", usage.MessageCount)
	}
}

func TestDeleteByUIDReportsAMissingMessage(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	if err := imapd.NewMessageRepo().DeleteByUID(context.Background(), db, mailboxID, 42); err == nil {
		t.Fatal("DeleteByUID for a UID that is not there succeeded, want an error")
	}
}

// A message with no Date header still has to be filed under something sensible.
func TestInsertAcceptsAMessageWithNoDate(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	ctx := context.Background()

	const noDate = "From: a@example.com\r\nTo: b@example.com\r\nSubject: s\r\n\r\nbody\r\n"
	meta := describe(t, noDate)
	if !meta.Date.IsZero() {
		t.Errorf("Date = %v, want the zero time when the header is absent", meta.Date)
	}

	internal := time.Now().UTC()
	if _, err := imapd.NewMessageRepo().Insert(ctx, db, mailboxID, []byte("d"), 10, internal, nil, meta); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

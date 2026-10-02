package imapd_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	imapd "github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

// deliver builds a real message from Alice with the given subject and body and
// files it.
func deliver(t *testing.T, db *sqlx.DB, mailboxID int64, subject, body, messageID string, flags []string) imapd.MessageResult {
	t.Helper()
	return deliverFrom(t, db, mailboxID, "Alice <alice@example.com>", subject, body, messageID, flags)
}

// deliverFrom is deliver with a chosen sender, because conversation grouping
// depends on who sent a message and not on it would test nothing.
func deliverFrom(t *testing.T, db *sqlx.DB, mailboxID int64, from, subject, body, messageID string, flags []string) imapd.MessageResult {
	t.Helper()

	raw := fmt.Sprintf("From: %s\r\n"+
		"To: bob@example.com\r\n"+
		"Subject: %s\r\n"+
		"Message-ID: <%s>\r\n"+
		"Date: Mon, 02 Jan 2006 15:04:05 +0000\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", from, subject, messageID, body)

	meta, err := mail.Describe([]byte(raw))
	if err != nil {
		t.Fatalf("Describe(%q): %v", subject, err)
	}

	result, err := imapd.NewMessageRepo().Insert(ctx(), db, mailboxID,
		[]byte("digest-"+messageID), int64(len(raw)), meta.Date, flags, meta)
	if err != nil {
		t.Fatalf("Insert(%q): %v", subject, err)
	}
	return result
}

func ctx() context.Context { return context.Background() }

// A folder list is what a mail client shows on open, and it has to contain the
// standard folders. Before provisioning, an owner had INBOX and nothing else,
// so there was nowhere to file a reply.
func TestEnsureFoldersProvisionsStandardSet(t *testing.T) {
	db := testdb.Migrated(t)
	owner := "bob@example.com"

	if err := store.NewUsers().Create(context.Background(), db, store.CreateUserRequest{
		Address: owner,
		Source:  "local",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	repo := imapd.NewMailboxRepo()
	if err := repo.EnsureFolders(context.Background(), db, owner); err != nil {
		t.Fatalf("EnsureFolders: %v", err)
	}
	// Provisioning is on the delivery path, so it runs again on the next
	// message. It must not duplicate or fail.
	if err := repo.EnsureFolders(context.Background(), db, owner); err != nil {
		t.Fatalf("EnsureFolders twice: %v", err)
	}

	folders, counts, err := repo.ListWithCounts(context.Background(), db, owner)
	if err != nil {
		t.Fatalf("ListWithCounts: %v", err)
	}

	want := []string{"INBOX", "Sent", "Drafts", "Junk", "Trash", "Archive"}
	if len(folders) != len(want) {
		t.Fatalf("got %d folders %v, want %d", len(folders), names(folders), len(want))
	}
	imapd.SortFolders(folders)
	for i, name := range want {
		if folders[i].Name != name {
			t.Errorf("folder %d = %q, want %q", i, folders[i].Name, name)
		}
		if counts[i].Total != 0 || counts[i].Unread != 0 {
			t.Errorf("folder %q counts = %+v, want zero", name, counts[i])
		}
	}

	// A client distinguishes Sent from a folder the user named Sent by the
	// special-use attribute, so it has to be set.
	for _, mb := range folders {
		switch mb.Name {
		case "Sent":
			if mb.SpecialUse == nil || *mb.SpecialUse != `\sent` {
				t.Errorf("Sent special_use = %v, want \\sent", mb.SpecialUse)
			}
		case "INBOX":
			if mb.SpecialUse != nil {
				t.Errorf("INBOX special_use = %v, want none", *mb.SpecialUse)
			}
		}
	}
}

// INBOX is not a label. Renaming it would break every client that has it
// selected and every piece of state that refers to it by name.
func TestRenameRejectsINBOX(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMailboxRepo()

	if err := repo.RenameChecked(context.Background(), db, mailboxID, "Mail"); err == nil {
		t.Fatal("renaming INBOX succeeded, want an error")
	}

	var name string
	if err := db.GetContext(context.Background(), &name,
		`SELECT name FROM mailboxes WHERE id = $1`, mailboxID); err != nil {
		t.Fatalf("read mailbox: %v", err)
	}
	if name != "INBOX" {
		t.Errorf("name = %q, want it left as INBOX", name)
	}
}

func TestRenameRejectsSeparator(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMailboxRepo()

	// A slash is the IMAP hierarchy separator, so a folder named with one
	// would become a parent that does not exist and split a listing in two.
	for _, name := range []string{"a/b", "", "a\x00b"} {
		if err := repo.RenameChecked(context.Background(), db, mailboxID, name); err == nil {
			t.Errorf("renaming to %q succeeded, want an error", name)
		}
	}
}

// The list is what a user scrolls, so its order and paging have to hold.
func TestListPagesNewestFirst(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMessageRepo()

	for i := 1; i <= 7; i++ {
		deliver(t, db, mailboxID, fmt.Sprintf("Subject %d", i),
			fmt.Sprintf("body %d", i), fmt.Sprintf("m%d@example.com", i), nil)
		// internal_date is the sort key and the schema does not default it to
		// something ordered, so distinct values are given deliberately.
		if _, err := db.Exec(`UPDATE messages SET internal_date = $2 WHERE mailbox_id = $1`,
			mailboxID, time.Date(2026, 1, i, 12, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("set internal date: %v", err)
		}
	}

	page, err := repo.List(context.Background(), db, imapd.ListOptions{MailboxID: mailboxID, Limit: 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if page.Total != 7 {
		t.Errorf("Total = %d, want 7", page.Total)
	}
	if len(page.Messages) != 3 {
		t.Fatalf("got %d messages, want a full page of 3", len(page.Messages))
	}
	if !page.HasMore {
		t.Error("HasMore = false on a partial page, want true")
	}
	if page.Messages[0].Subject != "Subject 7" {
		t.Errorf("first subject = %q, want the newest", page.Messages[0].Subject)
	}

	// Walking the cursor must cover every message exactly once, which is the
	// property an offset-based page cannot promise while mail is arriving.
	seen := map[int64]bool{}
	for {
		for _, m := range page.Messages {
			if seen[m.ID] {
				t.Fatalf("message %d appeared on two pages", m.ID)
			}
			seen[m.ID] = true
		}
		if !page.HasMore {
			break
		}
		page, err = repo.List(context.Background(), db, imapd.ListOptions{
			MailboxID:  mailboxID,
			Limit:      3,
			CursorDate: page.CursorDate,
			CursorID:   page.CursorID,
		})
		if err != nil {
			t.Fatalf("List page 2: %v", err)
		}
	}
	if len(seen) != 7 {
		t.Errorf("walked %d messages, want 7", len(seen))
	}

	last, err := repo.List(context.Background(), db, imapd.ListOptions{MailboxID: mailboxID, Limit: 100})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if last.HasMore {
		t.Error("HasMore = true when everything fits on one page, want false")
	}
}

// Two messages can share a timestamp to the microsecond, so a cursor on the date
// alone would either skip one or repeat it.
func TestListCursorHandlesIdenticalTimestamps(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMessageRepo()

	same := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	for i := 1; i <= 4; i++ {
		deliver(t, db, mailboxID, fmt.Sprintf("Same %d", i), "body",
			fmt.Sprintf("same%d@example.com", i), nil)
		if _, err := db.Exec(`UPDATE messages SET internal_date = $2 WHERE mailbox_id = $1`,
			mailboxID, same); err != nil {
			t.Fatalf("set internal date: %v", err)
		}
	}

	seen := map[int64]bool{}
	page, err := repo.List(context.Background(), db, imapd.ListOptions{MailboxID: mailboxID, Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for {
		for _, m := range page.Messages {
			if seen[m.ID] {
				t.Fatalf("message %d appeared twice with an identical date", m.ID)
			}
			seen[m.ID] = true
		}
		if !page.HasMore {
			break
		}
		page, err = repo.List(context.Background(), db, imapd.ListOptions{
			MailboxID: mailboxID, Limit: 2, CursorDate: page.CursorDate, CursorID: page.CursorID,
		})
		if err != nil {
			t.Fatalf("List next: %v", err)
		}
	}
	if len(seen) != 4 {
		t.Errorf("walked %d messages, want 4", len(seen))
	}
}

func TestListUnreadFilter(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMessageRepo()

	deliver(t, db, mailboxID, "unread one", "body", "u1@example.com", nil)
	deliver(t, db, mailboxID, "read one", "body", "r1@example.com", []string{`\Seen`})
	deliver(t, db, mailboxID, "starred unread", "body", "u2@example.com", []string{`\Flagged`})

	unread, err := repo.List(context.Background(), db, imapd.ListOptions{
		MailboxID: mailboxID, UnreadOnly: true,
	})
	if err != nil {
		t.Fatalf("List unread: %v", err)
	}
	if unread.Total != 2 {
		t.Errorf("unread Total = %d, want 2", unread.Total)
	}

	starred, err := repo.List(context.Background(), db, imapd.ListOptions{
		MailboxID: mailboxID, Flagged: true,
	})
	if err != nil {
		t.Fatalf("List flagged: %v", err)
	}
	if starred.Total != 1 || starred.Messages[0].Subject != "starred unread" {
		t.Errorf("flagged listing = %+v, want only the starred message", starred.Messages)
	}
}

// Adding a flag must not clear the others. Reassigning the array would lose the
// \Seen that opening the message just set.
func TestAddFlagPreservesExistingFlags(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMessageRepo()

	result := deliver(t, db, mailboxID, "flags", "body", "f@example.com", []string{`\Seen`})
	// Adding twice is what a retried request does, and it must not duplicate.
	if err := repo.AddFlag(context.Background(), db, result.ID, `\Flagged`); err != nil {
		t.Fatalf("AddFlag: %v", err)
	}
	if err := repo.AddFlag(context.Background(), db, result.ID, `\Flagged`); err != nil {
		t.Fatalf("AddFlag again: %v", err)
	}

	msg, err := repo.GetByID(context.Background(), db, "bob@example.com", result.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(msg.Flags) != 2 {
		t.Errorf("flags = %v, want \\Seen and \\Flagged once each", msg.Flags)
	}

	if err := repo.RemoveFlag(context.Background(), db, result.ID, `\Seen`); err != nil {
		t.Fatalf("RemoveFlag: %v", err)
	}
	msg, err = repo.GetByID(context.Background(), db, "bob@example.com", result.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(msg.Flags) != 1 || msg.Flags[0] != `\Flagged` {
		t.Errorf("flags = %v, want only \\Flagged", msg.Flags)
	}

	if err := repo.AddFlag(context.Background(), db, 999999, `\Flagged`); !store.IsNotFound(err) {
		t.Errorf("AddFlag on a missing message = %v, want not found", err)
	}
}

// A message id is a bigserial, so an unscoped read is an enumeration of every
// message on the server.
func TestGetByIDIsScopedToOwner(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	result := deliver(t, db, mailboxID, "private", "body", "p@example.com", nil)

	if _, err := imapd.NewMessageRepo().GetByID(context.Background(), db, "mallory@example.com", result.ID); !store.IsNotFound(err) {
		t.Errorf("GetByID as another user = %v, want not found", err)
	}
	if _, err := imapd.NewMailboxRepo().GetFolder(context.Background(), db, "mallory@example.com", mailboxID); !store.IsNotFound(err) {
		t.Errorf("GetFolder as another user = %v, want not found", err)
	}
}

// A conversation is one row in the list, with the newest message as its preview.
func TestListThreadsGroupsByConversation(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMessageRepo()

	root := deliver(t, db, mailboxID, "Q3 plan", "the plan is attached",
		"root@example.com", []string{`\Seen`})
	reply := deliver(t, db, mailboxID, "Re: Q3 plan", "looks good",
		"reply1@example.com", nil)
	deliver(t, db, mailboxID, "Lunch", "unrelated conversation",
		"other@example.com", nil)

	// Every message above carries the same Date, which would leave the ordering
	// decided by row id rather than by age. Distinct dates make the assertions
	// about order mean what they say.
	for _, when := range []struct {
		id  int64
		day int
	}{{root.ID, 1}, {reply.ID, 2}} {
		if _, err := db.Exec(`UPDATE messages SET internal_date = $2 WHERE id = $1`,
			when.id, time.Date(2026, 2, when.day, 10, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("set internal date: %v", err)
		}
	}

	threads, err := repo.ListThreads(context.Background(), db, mailboxID, 10)
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 2 {
		t.Fatalf("got %d threads, want 2: %+v", len(threads), threads)
	}

	// Newest conversation first.
	if threads[0].ThreadKey != "root@example.com" {
		t.Errorf("first thread key = %q, want the newest conversation", threads[0].ThreadKey)
	}
	if threads[0].Latest.ID != reply.ID {
		t.Errorf("preview = message %d, want the newest of the conversation", threads[0].Latest.ID)
	}
	if threads[0].MessageCount != 2 {
		t.Errorf("MessageCount = %d, want 2", threads[0].MessageCount)
	}
	if threads[0].UnreadCount != 1 {
		t.Errorf("UnreadCount = %d, want 1", threads[0].UnreadCount)
	}

	messages, err := repo.ThreadMessages(context.Background(), db, "bob@example.com", "root@example.com")
	if err != nil {
		t.Fatalf("ThreadMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("got %d thread messages, want 2", len(messages))
	}
	if messages[0].ID != root.ID || messages[1].ID != reply.ID {
		t.Errorf("thread order = %d,%d want %d,%d", messages[0].ID, messages[1].ID, root.ID, reply.ID)
	}

	// A thread key is only unique within an owner.
	if got, err := repo.ThreadMessages(context.Background(), db, "mallory@example.com", "root@example.com"); err != nil {
		t.Fatalf("ThreadMessages as another user: %v", err)
	} else if len(got) != 0 {
		t.Errorf("got %d messages for another owner, want none", len(got))
	}
}

// A reply that carries its References chain belongs to the conversation the
// chain names, whatever the subject says.
func TestListThreadsFollowsReferencesChain(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMessageRepo()

	root := deliver(t, db, mailboxID, "Quarterly plan", "attached",
		"root@example.com", []string{`\Seen`})

	// A client that rewords the subject on reply, which several do, must still
	// land in the same conversation.
	reply := fmt.Sprintf("From: Alice <alice@example.com>\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: quarterly plan (v2)\r\n" +
		"Message-ID: <reply1@example.com>\r\n" +
		"References: <root@example.com>\r\n" +
		"In-Reply-To: <root@example.com>\r\n" +
		"Date: Tue, 03 Jan 2006 15:04:05 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nlooks good\r\n")
	meta, err := mail.Describe([]byte(reply))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if meta.ThreadKey != "root@example.com" {
		t.Errorf("reply thread key = %q, want the root's", meta.ThreadKey)
	}
	if _, err := repo.Insert(context.Background(), db, mailboxID, []byte("digest"), 99,
		meta.Date, nil, meta); err != nil {
		t.Fatalf("Insert reply: %v", err)
	}

	// The duplicate In-Reply-To is not stored twice: a conforming reply repeats
	// the last reference in both headers.
	var refs int
	if err := db.GetContext(context.Background(), &refs,
		`SELECT count(*) FROM message_references r
		 JOIN messages m ON m.id = r.message_id
		 WHERE m.mailbox_id = $1 AND r.reference = 'root@example.com'`, mailboxID); err != nil {
		t.Fatalf("count references: %v", err)
	}
	if refs != 1 {
		t.Errorf("stored %d copies of the reference, want 1", refs)
	}

	threads, err := repo.ListThreads(context.Background(), db, mailboxID, 10)
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 1 || threads[0].Latest.ID != reply2ID(t, db, mailboxID, root.ID) {
		t.Fatalf("threads = %+v, want one conversation previewed by the reply", threads)
	}
	if threads[0].MessageCount != 2 || threads[0].UnreadCount != 1 {
		t.Errorf("counts = %d/%d, want 2 messages and 1 unread",
			threads[0].MessageCount, threads[0].UnreadCount)
	}
}

// A reply with no References is a heuristic case: it joins the conversation with
// the same subject and sender, and only that one.
func TestListThreadsAdoptsConversationForReferencelessReply(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMessageRepo()

	deliver(t, db, mailboxID, "Q3 plan", "the plan is attached",
		"root@example.com", []string{`\Seen`})

	// No References, and a subject that only matches after folding the prefix.
	deliver(t, db, mailboxID, "RE: q3 plan", "looks good", "reply1@example.com", nil)

	// Same subject, a different sender: a new conversation, not a merge. This
	// is the case that keeps every unrelated "Invoice" mail separate.
	deliverFrom(t, db, mailboxID, "Carol <carol@example.com>", "Re: Q3 plan",
		"my own plan", "other@example.com", nil)

	threads, err := repo.ListThreads(context.Background(), db, mailboxID, 10)
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 2 {
		t.Fatalf("got %d threads, want 2: %+v", len(threads), threads)
	}

	adopted := 0
	for _, th := range threads {
		switch th.ThreadKey {
		case "root@example.com":
			adopted++
			if th.MessageCount != 2 {
				t.Errorf("alice's conversation has %d messages, want 2", th.MessageCount)
			}
		case "other@example.com":
		default:
			t.Errorf("unexpected thread key %q", th.ThreadKey)
		}
	}
	if adopted != 1 {
		t.Errorf("root conversation appears %d times, want once", adopted)
	}
}

// A stale subject must not collect new mail: the conversation has to be recent.
func TestListThreadsDoesNotAdoptStaleSubject(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMessageRepo()

	deliver(t, db, mailboxID, "Weekly report", "first", "old@example.com", nil)
	if _, err := db.Exec(`UPDATE messages SET internal_date = internal_date - interval '200 days'
		WHERE mailbox_id = $1`, mailboxID); err != nil {
		t.Fatalf("age the message: %v", err)
	}
	deliver(t, db, mailboxID, "Re: Weekly report", "much later", "new@example.com", nil)

	threads, err := repo.ListThreads(context.Background(), db, mailboxID, 10)
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 2 {
		t.Errorf("got %d threads, want the stale subject left alone: %+v", len(threads), threads)
	}
}

// Search text arrives in a request body, so it has to be a query, not a regex.
func TestSearchMessages(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMessageRepo()

	deliver(t, db, mailboxID, "Quarterly budget", "the attached spreadsheet shows the budget",
		"s1@example.com", nil)
	deliver(t, db, mailboxID, "Lunch", "shall we get ramen", "s2@example.com", nil)

	hits, err := repo.SearchMessages(context.Background(), db, imapd.SearchOptions{
		Owner: "bob@example.com",
		Text:  "budget",
	})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 1 || hits[0].Subject != "Quarterly budget" {
		t.Fatalf("hits = %+v, want the budget message", hits)
	}

	// A query that would be an invalid or catastrophic regex is just words.
	rows, err := repo.SearchMessages(context.Background(), db, imapd.SearchOptions{
		Owner: "bob@example.com",
		Text:  "((((.*",
	})
	if err != nil {
		t.Fatalf("SearchMessages with punctuation: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d hits for punctuation, want none", len(rows))
	}

	// And it is scoped to the owner.
	rows, err = repo.SearchMessages(context.Background(), db, imapd.SearchOptions{
		Owner: "mallory@example.com",
		Text:  "budget",
	})
	if err != nil {
		t.Fatalf("SearchMessages as another user: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d hits for another owner, want none", len(rows))
	}
}

// Trashing is a move, not a delete: the message has to be findable afterwards.
func TestTrimMovesToTrash(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMailboxRepo()

	result := deliver(t, db, mailboxID, "junk to keep", "body", "t1@example.com", nil)
	moved, err := repo.Trim(context.Background(), db, "bob@example.com", mailboxID, []uint32{uint32(result.UID)})
	if err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if moved != 1 {
		t.Errorf("moved = %d, want 1", moved)
	}

	trash, err := repo.GetByName(context.Background(), db, "bob@example.com", "Trash")
	if err != nil {
		t.Fatalf("GetByName Trash: %v", err)
	}
	if trash.ID == mailboxID {
		t.Fatal("Trim moved the message into the folder it came from")
	}

	msg, err := imapd.NewMessageRepo().GetByID(context.Background(), db, "bob@example.com", result.ID)
	if err != nil {
		t.Fatalf("GetByID after trim: %v", err)
	}
	if msg.MailboxID != trash.ID {
		t.Errorf("mailbox = %d, want the trash folder %d", msg.MailboxID, trash.ID)
	}
	if msg.MessageID != "t1@example.com" {
		t.Errorf("message id = %q, want it preserved by a move", msg.MessageID)
	}
}

// Folder names are case sensitive to the protocol, so the API must not guess a
// spelling the mailbox does not have.
func TestGetFolderByName(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMailboxRepo()

	if _, err := repo.GetFolderByName(context.Background(), db, "bob@example.com", "inbox"); err != nil {
		t.Errorf("GetFolderByName(inbox) = %v, want INBOX", err)
	}
	mb, err := repo.GetFolderByName(context.Background(), db, "bob@example.com", "INBOX")
	if err != nil || mb.ID != mailboxID {
		t.Errorf("GetFolderByName(INBOX) = %+v, %v", mb, err)
	}
	if _, err := repo.GetFolderByName(context.Background(), db, "bob@example.com", "Work"); !store.IsNotFound(err) {
		t.Errorf("GetFolderByName(Work) = %v, want not found", err)
	}
	if _, err := repo.GetFolderByName(context.Background(), db, "bob@example.com", ""); !store.IsNotFound(err) {
		t.Errorf("GetFolderByName(empty) = %v, want not found", err)
	}
}

// Counts come from one grouped scan so the numbers agree with each other and
// the list does not run a query per folder.
func TestFolderStats(t *testing.T) {
	db, mailboxID := newTestMailbox(t, "bob@example.com")
	repo := imapd.NewMailboxRepo()

	if err := repo.EnsureFolders(context.Background(), db, "bob@example.com"); err != nil {
		t.Fatalf("EnsureFolders: %v", err)
	}
	deliver(t, db, mailboxID, "one", "body", "c1@example.com", nil)
	deliver(t, db, mailboxID, "two", "body", "c2@example.com", []string{`\Seen`})
	deliver(t, db, mailboxID, "three", "body", "c3@example.com", []string{`\Flagged`})

	folders, counts, err := repo.ListWithCounts(context.Background(), db, "bob@example.com")
	if err != nil {
		t.Fatalf("ListWithCounts: %v", err)
	}

	want := map[string]imapd.FolderCounts{
		"INBOX": {Total: 3, Unread: 2, Flagged: 1},
		"Sent":  {},
	}
	for i, mb := range folders {
		if expected, ok := want[mb.Name]; ok && counts[i] != expected {
			t.Errorf("%s counts = %+v, want %+v", mb.Name, counts[i], expected)
		}
	}
}

// reply2ID returns the id of the message after rootID in a mailbox.
func reply2ID(t *testing.T, db *sqlx.DB, mailboxID, rootID int64) int64 {
	t.Helper()
	var id int64
	if err := db.GetContext(context.Background(), &id,
		`SELECT id FROM messages WHERE mailbox_id = $1 AND id <> $2 ORDER BY id DESC LIMIT 1`,
		mailboxID, rootID); err != nil {
		t.Fatalf("read reply id: %v", err)
	}
	return id
}

func names(folders []imapd.Mailbox) []string {
	out := make([]string, len(folders))
	for i, f := range folders {
		out[i] = f.Name
	}
	return out
}

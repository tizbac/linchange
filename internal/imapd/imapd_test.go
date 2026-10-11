package imapd_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/mailauth"
	pwd "github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dbURL, _ := testdb.URL()
	c := config.New()
	c.Database.URL = dbURL
	c.Listener.IMAP.Addr = ":0"
	c.Listener.IMAPS.Addr = ""
	c.Server.DataDir = t.TempDir()
	return c
}

func setupServer(t *testing.T) (*imapd.Server, *sqlx.DB, string) {
	return setupServerWithDirectory(t, &stubDirectory{})
}

func setupServerWithDirectory(t *testing.T, dir mailauth.Directory) (*imapd.Server, *sqlx.DB, string) {
	t.Helper()

	db := testdb.Migrated(t)

	blobs, err := blobstore.Open(blobstore.Options{
		Root: t.TempDir(),
	})

	if err != nil {
		t.Fatalf("open blobstore: %v", err)
	}

	hash, err := pwd.Hash("testpassword123", config.New().Auth.Local.Argon2)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	ctx := context.Background()
	aliases := store.NewAliases()
	sharedMailboxes := store.NewSharedMailboxes()
	sharedGrants := store.NewSharedMailboxGrants()

	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address:     "alice@example.com",
		Source:      "local",
		DisplayName: "Alice Test",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := store.NewCredentials().Set(ctx, db, "alice@example.com", hash); err != nil {
		t.Fatalf("set credential: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO mailboxes (owner, name, uidvalidity, uidnext)
		 VALUES ('alice@example.com', 'INBOX', 1, 1)`,
	); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}

	cfg := testConfig(t)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	mailAuth := mailauth.New(db, logger, dir)

	srv, err := imapd.New(db, blobs, logger, aliases, mailAuth, sharedMailboxes, sharedGrants, cfg, nil)
	if err != nil {
		t.Fatalf("create imap server: %v", err)
	}

	ln, err := net.Listen("tcp", srv.PlainAddr())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() { _ = srv.PlainServer().Serve(ln) }()

	// Give the server a moment to start accepting connections.
	time.Sleep(200 * time.Millisecond)

	t.Cleanup(func() {
		srv.PlainServer().Close()
		ln.Close()
	})

	addr := ln.Addr().String()
	return srv, db, addr
}

func TestLogin(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "wrongpassword")
	err = cmd.Wait()
	if err == nil {
		t.Fatal("expected login to fail with wrong password")
	}
}

func TestLoginUnknownUser(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("nobody@example.com", "testpassword123")
	err = cmd.Wait()
	if err == nil {
		t.Fatal("expected login to fail for unknown user")
	}
}

func TestListMailboxes(t *testing.T) {
	_, db, addr := setupServer(t)

	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO mailboxes (owner, name, uidvalidity, uidnext)
		 VALUES ('alice@example.com', 'Work', 2, 1)`,
	); err != nil {
		t.Fatalf("create mailbox: %v", err)
	}

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	listCmd := client.List("", "%%", nil)
	list, err := listCmd.Collect()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	t.Logf("list result: %d mailboxes", len(list))
	for i, d := range list {
		t.Logf("  [%d] attrs=%v delim=%c mailbox=%q", i, d.Attrs, d.Delim, d.Mailbox)
	}

	names := make([]string, 0, len(list))
	for _, d := range list {
		names = append(names, d.Mailbox)
	}

	foundINBOX := false
	foundWork := false
	for _, n := range names {
		if n == "INBOX" {
			foundINBOX = true
		}
		if n == "Work" {
			foundWork = true
		}
	}
	if !foundINBOX {
		t.Error("INBOX not found in mailbox list")
	}
	if !foundWork {
		t.Error("Work folder not found in mailbox list")
	}
}

func TestSelectMailbox(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	data, err := selectCmd.Wait()
	if err != nil {
		t.Fatalf("select INBOX: %v", err)
	}

	if data.NumMessages != 0 {
		t.Errorf("expected 0 messages in new mailbox, got %d", data.NumMessages)
	}

	if data.UIDNext == 0 {
		t.Error("UIDNext should be non-zero")
	}

	if data.UIDValidity == 0 {
		t.Error("UIDValidity should be non-zero")
	}
}

func TestAppendAndFetch(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	if _, err := selectCmd.Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	msg := []byte("From: alice@example.com\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: Hello World\r\n" +
		"Date: Mon, 1 Jan 2024 12:00:00 +0000\r\n" +
		"\r\n" +
		"This is the message body.\r\n")

	appendCmd := client.Append("INBOX", int64(len(msg)), nil)
	if _, err := appendCmd.Write(msg); err != nil {
		t.Fatalf("append write: %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}

	appendData, err := appendCmd.Wait()
	if err != nil {
		t.Fatalf("append wait: %v", err)
	}

	if appendData.UID == 0 {
		t.Error("APPEND should return a non-zero UID")
	}

	fetchCmd := client.Fetch(imap.SeqSetNum(1), nil)
	messages, err := fetchCmd.Collect()
	if err != nil {
		t.Fatalf("fetch collect: %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("expected 1 fetched message, got %d", len(messages))
	}

	if messages[0].Envelope == nil {
		t.Fatal("expected envelope data in fetch response")
	}

	if messages[0].Envelope.Subject != "Hello World" {
		t.Errorf("expected subject 'Hello World', got %q", messages[0].Envelope.Subject)
	}

	if messages[0].Envelope.From == nil || len(messages[0].Envelope.From) == 0 {
		t.Error("expected sender in envelope")
	}
}

func TestFetchBodySection(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	if _, err := selectCmd.Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	msg := []byte("From: alice@example.com\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: Test Body\r\n" +
		"\r\n" +
		"Body content here.\r\n")

	appendCmd := client.Append("INBOX", int64(len(msg)), nil)
	if _, err := appendCmd.Write(msg); err != nil {
		t.Fatalf("append write: %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("append wait: %v", err)
	}

	fetchCmd := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}},
	})
	messages, err := fetchCmd.Collect()
	if err != nil {
		t.Fatalf("fetch collect: %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}

	if messages[0].BodySection == nil {
		t.Fatal("expected body section in fetch response")
	}

	body := messages[0].BodySection[0].Bytes
	if len(body) == 0 {
		t.Error("body section should not be empty")
	}
}

func TestStoreFlagsAddSeen(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	if _, err := selectCmd.Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	msg := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Flag Test\r\n\r\nBody.\r\n")

	appendCmd := client.Append("INBOX", int64(len(msg)), nil)
	if _, err := appendCmd.Write(msg); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("append wait: %v", err)
	}

	storeCmd := client.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{"\\Seen"}}, nil)
	results, err := storeCmd.Collect()
	if err != nil {
		t.Fatalf("store collect: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 store result, got %d", len(results))
	}

	foundSeen := false
	for _, f := range results[0].Flags {
		if string(f) == "\\Seen" {
			foundSeen = true
		}
	}
	if !foundSeen {
		t.Error("\\Seen flag should have been added")
	}
}

func TestCreateMailbox(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	createCmd := client.Create("TestFolder", nil)
	if err := createCmd.Wait(); err != nil {
		t.Fatalf("create mailbox: %v", err)
	}

	listCmd := client.List("", "TestFolder*", nil)
	list, err := listCmd.Collect()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	t.Logf("list result: %d mailboxes", len(list))
	for i, d := range list {
		t.Logf("  [%d] attrs=%v delim=%c mailbox=%q", i, d.Attrs, d.Delim, d.Mailbox)
	}

	if len(list) != 1 {
		t.Fatalf("expected 1 mailbox matching TestFolder*, got %d", len(list))
	}

	if list[0].Mailbox != "TestFolder" {
		t.Errorf("expected mailbox name 'TestFolder', got %q", list[0].Mailbox)
	}
}

func TestStatusMailbox(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	statusCmd := client.Status("INBOX", &imap.StatusOptions{NumMessages: true})
	data, err := statusCmd.Wait()
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	if data.NumMessages == nil || *data.NumMessages != 0 {
		t.Errorf("expected 0 messages, got %v", data.NumMessages)
	}
}

func TestSearch(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	if _, err := selectCmd.Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	for i := 0; i < 3; i++ {
		msg := []byte(fmt.Sprintf("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Message %d\r\n\r\nBody %d.\r\n", i, i))
		appendCmd := client.Append("INBOX", int64(len(msg)), nil)
		if _, err := appendCmd.Write(msg); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if err := appendCmd.Close(); err != nil {
			t.Fatalf("append close %d: %v", i, err)
		}
		if _, err := appendCmd.Wait(); err != nil {
			t.Fatalf("append wait %d: %v", i, err)
		}
	}

	searchCmd := client.Search(&imap.SearchCriteria{}, nil)
	searchData, err := searchCmd.Wait()
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	uids := searchData.AllSeqNums()
	if len(uids) != 3 {
		t.Fatalf("expected 3 messages in search results, got %d", len(uids))
	}
}

func TestCopyMessage(t *testing.T) {
	_, db, addr := setupServer(t)

	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO mailboxes (owner, name, uidvalidity, uidnext)
		 VALUES ('alice@example.com', 'Archive', 2, 1)`,
	); err != nil {
		t.Fatalf("create Archive mailbox: %v", err)
	}

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	if _, err := selectCmd.Wait(); err != nil {
		t.Fatalf("select INBOX: %v", err)
	}

	msg := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: To Archive\r\n\r\nMove this.\r\n")
	appendCmd := client.Append("INBOX", int64(len(msg)), nil)
	if _, err := appendCmd.Write(msg); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("append wait: %v", err)
	}

	copyCmd := client.Copy(imap.SeqSetNum(1), "Archive")
	data, err := copyCmd.Wait()
	if err != nil {
		t.Fatalf("copy: %v", err)
	}

	if data.DestUIDs == nil || len(data.DestUIDs) == 0 {
		t.Error("copy should return destination UIDs")
	}
}

func TestExpungeMessage(t *testing.T) {
	_, _, addr := setupServer(t)

	var debugBuf bytes.Buffer
	options := &imapclient.Options{
		DebugWriter: &debugBuf,
	}

	client, err := imapclient.DialInsecure(addr, options)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	if _, err := selectCmd.Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	msg := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Delete Me\r\n\r\nBye.\r\n")
	appendCmd := client.Append("INBOX", int64(len(msg)), nil)
	if _, err := appendCmd.Write(msg); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("append wait: %v", err)
	}

	fetchCmd := client.Fetch(imap.SeqSetNum(1), nil)
	messages, err := fetchCmd.Collect()
	if err != nil {
		t.Fatalf("fetch before expunge: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message before expunge, got %d", len(messages))
	}

	uid := messages[0].UID
	if uid == 0 {
		t.Fatal("UID should be non-zero")
	}

	// Use STORE \\Deleted, then EXPUNGE to remove the message.
	storeDel := client.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{"\\Deleted"}}, nil)
	if _, err := storeDel.Collect(); err != nil {
		t.Fatalf("store \\Deleted: %v", err)
	}

	expungeCmd := client.Expunge()
	t.Log("calling Collect...")

	expunged, err := expungeCmd.Collect()

	t.Logf("Debug output after Collect:\n%s", debugBuf.String())
	t.Logf("Collect returned: %d expunged, err=%v", len(expunged), err)
	if err != nil {
		t.Fatalf("expunge collect: %v", err)
	}
	expungeCmd.Close()

	if len(expunged) != 1 {
		t.Fatalf("expected 1 expunged message, got %d", len(expunged))
	}

	fetchCmd2 := client.Fetch(imap.SeqSetNum(1), nil)
	messages2, err := fetchCmd2.Collect()
	if err != nil {
		t.Fatalf("fetch after expunge: %v", err)
	}
	if len(messages2) != 0 {
		t.Errorf("expected 0 messages after expunge, got %d", len(messages2))
	}
}

func TestDeleteMailbox(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	createCmd := client.Create("ToDelete", nil)
	if err := createCmd.Wait(); err != nil {
		t.Fatalf("create: %v", err)
	}

	deleteCmd := client.Delete("ToDelete")
	if err := deleteCmd.Wait(); err != nil {
		t.Fatalf("delete: %v", err)
	}

	listCmd := client.List("", "ToDelete*", nil)
	list, err := listCmd.Collect()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected 0 mailboxes after delete, got %d", len(list))
	}
}

func TestRenameMailbox(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	createCmd := client.Create("OldName", nil)
	if err := createCmd.Wait(); err != nil {
		t.Fatalf("create: %v", err)
	}

	renameCmd := client.Rename("OldName", "NewName", nil)
	if err := renameCmd.Wait(); err != nil {
		t.Fatalf("rename: %v", err)
	}

	listCmd := client.List("", "NewName*", nil)
	list, err := listCmd.Collect()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 mailbox, got %d", len(list))
	}
	if list[0].Mailbox != "NewName" {
		t.Errorf("expected 'NewName', got %q", list[0].Mailbox)
	}

	listOld := client.List("", "OldName*", nil)
	oldList, err := listOld.Collect()
	if err != nil {
		t.Fatalf("list old: %v", err)
	}
	if len(oldList) != 0 {
		t.Errorf("expected 0 old mailboxes, got %d", len(oldList))
	}
}

func TestUIDFetch(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	if _, err := selectCmd.Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	msg := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: UID Test\r\n\r\nBody.\r\n")
	appendCmd := client.Append("INBOX", int64(len(msg)), nil)
	if _, err := appendCmd.Write(msg); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}

	appendData, err := appendCmd.Wait()
	if err != nil {
		t.Fatalf("append wait: %v", err)
	}

	uid := appendData.UID
	fetchCmd := client.Fetch(imap.UIDSetNum(uid), nil)
	messages, err := fetchCmd.Collect()
	if err != nil {
		t.Fatalf("uid fetch: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].UID == 0 {
		t.Error("UID should be set in fetch response")
	}
}

func TestCapability(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	caps := client.Capability()
	result, err := caps.Wait()
	if err != nil {
		t.Fatalf("capability: %v", err)
	}

	if !result.Has(imap.CapIMAP4rev1) {
		t.Error("capability should include IMAP4rev1")
	}
	if !result.Has(imap.CapIMAP4rev2) {
		t.Error("capability should include IMAP4rev2")
	}
	if !result.Has(imap.CapIdle) {
		t.Error("capability should include IDLE")
	}
}

func TestLogout(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	logoutCmd := client.Logout()
	if err := logoutCmd.Wait(); err != nil {
		t.Fatalf("logout: %v", err)
	}

	client.Close()
}

func TestNoop(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	noopCmd := client.Noop()
	if err := noopCmd.Wait(); err != nil {
		t.Fatalf("noop: %v", err)
	}
}

func TestAppendMultipleMessages(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	if _, err := selectCmd.Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	for i := 0; i < 5; i++ {
		msg := []byte(fmt.Sprintf("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Message %d\r\n\r\nBody %d.\r\n", i, i))
		appendCmd := client.Append("INBOX", int64(len(msg)), nil)
		if _, err := appendCmd.Write(msg); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if err := appendCmd.Close(); err != nil {
			t.Fatalf("append close %d: %v", i, err)
		}
		if _, err := appendCmd.Wait(); err != nil {
			t.Fatalf("append wait %d: %v", i, err)
		}
	}

	fetchCmd := client.Fetch(imap.SeqSetNum(1, 2, 3, 4, 5), &imap.FetchOptions{
		UID: true,
	})
	messages, err := fetchCmd.Collect()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(messages) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(messages))
	}
}

func TestUIDPlusCapability(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	caps := client.Capability()
	result, err := caps.Wait()
	if err != nil {
		t.Fatalf("capability: %v", err)
	}
	if !result.Has(imap.CapUIDPlus) {
		t.Error("UIDPLUS capability should be advertised")
	}
}

func TestStoreFlagsDeleteSeen(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	if _, err := selectCmd.Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	msg := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Unmark\r\n\r\nBody.\r\n")
	appendCmd := client.Append("INBOX", int64(len(msg)), nil)
	if _, err := appendCmd.Write(msg); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("append wait: %v", err)
	}

	storeAdd := client.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{"\\Seen"}}, nil)
	if _, err := storeAdd.Collect(); err != nil {
		t.Fatalf("store add: %v", err)
	}

	storeDel := client.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsDel, Flags: []imap.Flag{"\\Seen"}}, nil)
	results, err := storeDel.Collect()
	if err != nil {
		t.Fatalf("store delete: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	for _, f := range results[0].Flags {
		if string(f) == "\\Seen" {
			t.Error("\\Seen flag should have been removed")
		}
	}
}

func TestFetchNonExistent(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	if _, err := selectCmd.Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	fetchCmd := client.Fetch(imap.SeqSetNum(9999), nil)
	messages, err := fetchCmd.Collect()
	if err != nil {
		t.Fatalf("fetch non-existent: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("expected 0 messages, got %d", len(messages))
	}
}

func TestStoreOnUnselectedMailbox(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	storeCmd := client.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{"\\Seen"}}, nil)
	_, err = storeCmd.Collect()
	if err == nil {
		t.Fatal("expected error when storing on unselected mailbox")
	}
}

func TestFetchOnUnselectedMailbox(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	fetchCmd := client.Fetch(imap.SeqSetNum(1), nil)
	_, err = fetchCmd.Collect()
	if err == nil {
		t.Fatal("expected error when fetching on unselected mailbox")
	}
}

func TestSelectNonExistentMailbox(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("NonExistentFolder", nil)
	_, err = selectCmd.Wait()
	if err == nil {
		t.Fatal("expected error when selecting non-existent mailbox")
	}
}

func TestCreateDuplicateMailbox(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	createCmd := client.Create("DupFolder", nil)
	if err := createCmd.Wait(); err != nil {
		t.Fatalf("create first: %v", err)
	}

	createCmd2 := client.Create("DupFolder", nil)
	if err := createCmd2.Wait(); err == nil {
		t.Fatal("expected error when creating duplicate mailbox")
	}
}

func TestDeleteINBOX(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	deleteCmd := client.Delete("INBOX")
	if err := deleteCmd.Wait(); err == nil {
		t.Fatal("expected error when deleting INBOX")
	}
}

func TestListSubscribed(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	createCmd := client.Create("SubFolder", nil)
	if err := createCmd.Wait(); err != nil {
		t.Fatalf("create: %v", err)
	}

	subscribeCmd := client.Subscribe("SubFolder")
	if err := subscribeCmd.Wait(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	listCmd := client.List("", "SubFolder*", &imap.ListOptions{
		ReturnSubscribed: true,
	})
	list, err := listCmd.Collect()
	if err != nil {
		t.Fatalf("list subscribed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 subscribed mailbox, got %d", len(list))
	}
	if list[0].Mailbox != "SubFolder" {
		t.Errorf("expected 'SubFolder', got %q", list[0].Mailbox)
	}

	unsubscribeCmd := client.Unsubscribe("SubFolder")
	if err := unsubscribeCmd.Wait(); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}

	listCmd2 := client.List("", "SubFolder*", &imap.ListOptions{
		ReturnSubscribed: true,
	})
	list2, err := listCmd2.Collect()
	if err != nil {
		t.Fatalf("list unsubscribed: %v", err)
	}
	if len(list2) != 0 {
		t.Errorf("expected 0 subscribed mailboxes after unsubscribe, got %d", len(list2))
	}
}

func TestIDCommand(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	idCmd := client.ID(&imap.IDData{
		Name:    "test-client",
		Version: "1.0",
	})
	_, err = idCmd.Wait()
	if err != nil {
		t.Logf("ID command result (non-fatal): %v", err)
	}
}

func TestAppendLargeMessage(t *testing.T) {
	_, _, addr := setupServer(t)

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmd := client.Login("alice@example.com", "testpassword123")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	selectCmd := client.Select("INBOX", nil)
	if _, err := selectCmd.Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	header := "From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Large Message\r\n\r\n"
	body := strings.Repeat("x", 50000)
	msg := []byte(header + body)

	appendCmd := client.Append("INBOX", int64(len(msg)), nil)
	if _, err := appendCmd.Write(msg); err != nil {
		t.Fatalf("append large: %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("append large close: %v", err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("append large wait: %v", err)
	}

	fetchCmd := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}},
	})
	messages, err := fetchCmd.Collect()
	if err != nil {
		t.Fatalf("fetch large: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].BodySection == nil {
		t.Fatal("expected body section")
	}
	if len(messages[0].BodySection[0].Bytes) != 50072 {
		t.Errorf("expected body length 50072, got %d", len(messages[0].BodySection[0].Bytes))
	}
}

// stubDirectory stands in for a real directory, so that the rule being tested is
// the listener's rather than the bind's. The bind itself is exercised in
// internal/auth/ldap against the protocol fixture.
type stubDirectory struct {
	ok   bool
	err  error
	seen []string
}

func (s *stubDirectory) Authenticate(_ context.Context, address, _ string) (bool, error) {
	s.seen = append(s.seen, address)
	return s.ok, s.err
}

// addDirectoryUser provisions the shape the reconcile leaves: a directory-backed
// principal with no local credential row.
func addDirectoryUser(t *testing.T, db *sqlx.DB, address string) {
	t.Helper()
	ctx := context.Background()
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: address, Source: "ldap", DisplayName: "Directory User",
	}); err != nil {
		t.Fatalf("create directory user: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO mailboxes (owner, name, uidvalidity, uidnext) VALUES ($1, 'INBOX', 1, 1)`,
		address); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
}

// TestLoginWithDirectoryPassword is the reason the listener takes a verifier:
// a directory-backed account signs in on the directory's answer, not on a
// stored hash.
func TestLoginWithDirectoryPassword(t *testing.T) {
	dir := &stubDirectory{ok: true}
	_, db, addr := setupServerWithDirectory(t, dir)
	addDirectoryUser(t, db, "dana@corp.example.com")

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if err := client.Login("dana@corp.example.com", "from-the-directory").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if len(dir.seen) != 1 || dir.seen[0] != "dana@corp.example.com" {
		t.Errorf("the directory saw %v, want one attempt for dana", dir.seen)
	}
}

// TestLoginDirectoryRefused checks the negative half: the directory's refusal is
// a failed login.
func TestLoginDirectoryRefused(t *testing.T) {
	dir := &stubDirectory{ok: false}
	_, db, addr := setupServerWithDirectory(t, dir)
	addDirectoryUser(t, db, "dana@corp.example.com")

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if err := client.Login("dana@corp.example.com", "wrong").Wait(); err == nil {
		t.Fatal("a password the directory refused must not log in")
	}
}

// TestLoginDirectoryUnavailable proves the listener fails closed rather than
// falling through to something else when the directory cannot be reached.
func TestLoginDirectoryUnavailable(t *testing.T) {
	dir := &stubDirectory{err: errors.New("the directory is unreachable")}
	_, db, addr := setupServerWithDirectory(t, dir)
	addDirectoryUser(t, db, "dana@corp.example.com")

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if err := client.Login("dana@corp.example.com", "whatever").Wait(); err == nil {
		t.Fatal("an unreachable directory must not admit anyone")
	}
}

// TestLoginWithoutCredentialIsRefused is the bypass. This account has no stored
// password, which the listener used to read as "any password will do".
func TestLoginWithoutCredentialIsRefused(t *testing.T) {
	// No directory is wired up, so nothing can answer for a principal whose
	// password nobody holds.
	_, db, addr := setupServer(t)
	addDirectoryUser(t, db, "dana@corp.example.com")

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if err := client.Login("dana@corp.example.com", "anything at all").Wait(); err == nil {
		t.Fatal("an account with no stored password was admitted: this is the bypass")
	}
}

// TestLoginDomainAdminRefused shows a principal that may sign in to the web still
// cannot read mail.
func TestLoginDomainAdminRefused(t *testing.T) {
	dir := &stubDirectory{ok: true}
	_, db, addr := setupServerWithDirectory(t, dir)

	ctx := context.Background()
	hash, err := pwd.Hash("admin-pw", config.New().Auth.Local.Argon2)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
		Address: "admin@example.com", Source: "domain_admin", DisplayName: "Admin",
	}); err != nil {
		t.Fatalf("create domain admin: %v", err)
	}
	if err := store.NewCredentials().Set(ctx, db, "admin@example.com", hash); err != nil {
		t.Fatalf("set credential: %v", err)
	}

	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if err := client.Login("admin@example.com", "admin-pw").Wait(); err == nil {
		t.Fatal("a domain admin must not be able to read mail")
	}
}

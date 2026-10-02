package pop3d_test

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	pwd "github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/pop3d"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dbURL, _ := testdb.URL()
	c := config.New()
	c.Database.URL = dbURL
	c.Listener.POP3.Addr = ":0"
	c.Listener.POP3S.Addr = ""
	c.Server.DataDir = t.TempDir()
	return c
}

func setupServer(t *testing.T) (*pop3d.Server, *sqlx.DB, string) {
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
	users := store.NewUsers()
	aliases := store.NewAliases()
	creds := store.NewCredentials()
	sharedMailboxes := store.NewSharedMailboxes()
	sharedGrants := store.NewSharedMailboxGrants()

	if err := users.Create(ctx, db, store.CreateUserRequest{
		Address:     "alice@example.com",
		Source:      "local",
		DisplayName: "Alice Test",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := creds.Set(ctx, db, "alice@example.com", hash); err != nil {
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

	srv, err := pop3d.New(db, blobs, logger, users, aliases, creds, sharedMailboxes, sharedGrants, cfg)
	if err != nil {
		t.Fatalf("create pop3 server: %v", err)
	}

	ln, err := net.Listen("tcp", srv.PlainAddr())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() { _ = srv.PlainServer().Serve(ln) }()

	// Give the server a moment to start accepting connections.
	time.Sleep(200 * time.Millisecond)

	addr := ln.Addr().String()
	t.Cleanup(func() {
		srv.PlainServer().Close()
		ln.Close()
	})

	return srv, db, addr
}

func connectPOP3(t *testing.T, addr string) (net.Conn, *bufio.Scanner) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no greeting: %v", scanner.Err())
	}
	return conn, scanner
}

func sendCommand(t *testing.T, conn net.Conn, scanner *bufio.Scanner, cmd string) string {
	t.Helper()
	if conn != nil {
		_, err := fmt.Fprintf(conn, "%s\r\n", cmd)
		if err != nil {
			t.Fatalf("send %q: %v", cmd, err)
		}
	}

	var firstLine string

	for scanner.Scan() {
		line := scanner.Text()
		t.Logf("POP3 response: %s", line)
		if firstLine == "" {
			firstLine = line
			// Commands with multi-line responses: LIST, RETR, UIDL, TOP
			if line != "" && (line[0] == '+' || line[0] == '-') {
				if strings.HasPrefix(cmd, "LIST") ||
					strings.HasPrefix(cmd, "RETR") ||
					strings.HasPrefix(cmd, "UIDL") ||
					strings.HasPrefix(cmd, "TOP") {
					// Error responses are single-line even for multi-line commands
					if strings.HasPrefix(line, "-ERR") {
						break
					}
				} else {
					break
				}
			}
		}

		if line == "." {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error for %q: %v", cmd, err)
	}

	return firstLine
}

func TestPOP3Login(t *testing.T) {
	_, _, addr := setupServer(t)
	conn, scanner := connectPOP3(t, addr)

	resp := sendCommand(t, conn, scanner, "USER alice@example.com")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("USER: expected +OK, got: %s", resp)
	}

	resp = sendCommand(t, conn, scanner, "PASS testpassword123")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("PASS: expected +OK, got: %s", resp)
	}

	resp = sendCommand(t, conn, scanner, "QUIT")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("QUIT: expected +OK, got: %s", resp)
	}
}

func TestPOP3LoginFailed(t *testing.T) {
	_, _, addr := setupServer(t)
	conn, scanner := connectPOP3(t, addr)

	resp := sendCommand(t, conn, scanner, "USER alice@example.com")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("USER: expected +OK, got: %s", resp)
	}

	resp = sendCommand(t, conn, scanner, "PASS wrongpassword")
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("PASS: expected -ERR, got: %s", resp)
	}

	resp = sendCommand(t, conn, scanner, "QUIT")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("QUIT: expected +OK, got: %s", resp)
	}
}

func TestPOP3NoAuthList(t *testing.T) {
	_, _, addr := setupServer(t)
	conn, scanner := connectPOP3(t, addr)

	resp := sendCommand(t, conn, scanner, "LIST")
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("LIST without auth: expected -ERR, got: %s", resp)
	}

	resp = sendCommand(t, conn, scanner, "QUIT")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("QUIT: expected +OK, got: %s", resp)
	}
}

func TestPOP3EmptyMailbox(t *testing.T) {
	_, _, addr := setupServer(t)
	conn, scanner := connectPOP3(t, addr)

	sendCommand(t, conn, scanner, "USER alice@example.com")
	sendCommand(t, conn, scanner, "PASS testpassword123")

	resp := sendCommand(t, conn, scanner, "STAT")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("STAT: expected +OK, got: %s", resp)
	}

	resp = sendCommand(t, conn, scanner, "LIST")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("LIST: expected +OK, got: %s", resp)
	}

	sendCommand(t, conn, scanner, "QUIT")
}

func TestPOP3AppendAndRetr(t *testing.T) {
	_, db, addr := setupServer(t)

	// Insert a test message.
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `
		INSERT INTO messages (mailbox_id, uid, blob_sha256, size, internal_date,
		                      flags, subject, from_address, from_name, snippet, thread_key, message_id, has_attachments)
		SELECT id, 1, decode('aa', 'hex'), 100, now(), '{}', 'Test Subject',
		       'sender@example.com', 'Sender', '', gen_random_uuid()::text, gen_random_uuid()::text, false
		FROM mailboxes WHERE name = 'INBOX'
	`)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	conn, scanner := connectPOP3(t, addr)
	sendCommand(t, conn, scanner, "USER alice@example.com")
	sendCommand(t, conn, scanner, "PASS testpassword123")

	resp := sendCommand(t, conn, scanner, "STAT")
	if !strings.Contains(resp, "1") {
		t.Fatalf("STAT: expected 1 message, got: %s", resp)
	}

	resp = sendCommand(t, conn, scanner, "LIST")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("LIST: expected +OK, got: %s", resp)
	}

	sendCommand(t, conn, scanner, "QUIT")
}

func TestPOP3DELE(t *testing.T) {
	_, db, addr := setupServer(t)

	// Insert a test message.
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `
		INSERT INTO messages (mailbox_id, uid, blob_sha256, size, internal_date,
		                      flags, subject, from_address, from_name, snippet, thread_key, message_id, has_attachments)
		SELECT id, 1, decode('aa', 'hex'), 100, now(), '{}', 'Test Subject',
		       'sender@example.com', 'Sender', '', gen_random_uuid()::text, gen_random_uuid()::text, false
		FROM mailboxes WHERE name = 'INBOX'
	`)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	conn, scanner := connectPOP3(t, addr)
	sendCommand(t, conn, scanner, "USER alice@example.com")
	sendCommand(t, conn, scanner, "PASS testpassword123")

	resp := sendCommand(t, conn, scanner, "LIST")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("LIST: expected +OK, got: %s", resp)
	}

	resp = sendCommand(t, conn, scanner, "DELE 1")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("DELE: expected +OK, got: %s", resp)
	}

	resp = sendCommand(t, conn, scanner, "RSET")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("RSET: expected +OK, got: %s", resp)
	}

	resp = sendCommand(t, conn, scanner, "DELE 1")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("DELE: expected +OK, got: %s", resp)
	}

	resp = sendCommand(t, conn, scanner, "QUIT")
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("QUIT: expected +OK, got: %s", resp)
	}

	// Check message is deleted.
	var count int
	err = db.GetContext(ctx, &count, `
		SELECT COUNT(*) FROM messages
		WHERE mailbox_id IN (SELECT id FROM mailboxes WHERE name = 'INBOX')
		AND uid = 1
	`)
	if err != nil {
		t.Fatalf("check deleted: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 messages, got %d", count)
	}
}

func TestPOP3ListMultiple(t *testing.T) {
	_, db, addr := setupServer(t)

	// Insert multiple test messages.
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		_, err := db.ExecContext(ctx, `
			INSERT INTO messages (mailbox_id, uid, blob_sha256, size, internal_date,
			                      flags, subject, from_address, from_name, snippet, thread_key, message_id, has_attachments)
			SELECT id, $1, decode('bb', 'hex'), 100, now(), '{}', 'Test Subject',
			       'sender@example.com', 'Sender', '', gen_random_uuid()::text, gen_random_uuid()::text, false
			FROM mailboxes WHERE name = 'INBOX'
		`, i)
		if err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}

	conn, scanner := connectPOP3(t, addr)
	sendCommand(t, conn, scanner, "USER alice@example.com")
	sendCommand(t, conn, scanner, "PASS testpassword123")

	resp := sendCommand(t, conn, scanner, "STAT")
	if !strings.Contains(resp, "3") {
		t.Fatalf("STAT: expected 3 messages, got: %s", resp)
	}

	sendCommand(t, conn, scanner, "QUIT")
}

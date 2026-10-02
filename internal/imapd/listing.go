package imapd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/store"
)

// ListOptions selects a page of a folder.
//
// The listing is keyset-paged on (internal_date, id) rather than offset-paged,
// because offset paging over a folder that is being delivered into skips or
// repeats rows: rows inserted ahead of the offset shift everything under the
// reader. With a cursor, a message arriving mid-scroll only appears on the next
// page, where the client can handle it as new mail.
type ListOptions struct {
	MailboxID int64
	Limit     int
	// Cursor is the (internal_date, id) of the last message of the previous
	// page. Both are required together, because the date alone is not unique.
	CursorDate time.Time
	CursorID   int64
	UnreadOnly bool
	Flagged    bool
	// Ascending lists oldest first. It exists for the tests and for the IMAP
	// sequences, which are ascending; a mail client lists newest first.
	Ascending bool
}

// DefaultListLimit and MaxListLimit bound a page.
//
// A client that asks for everything should not be able to make the server
// allocate a result set proportional to a mailbox with a million messages in
// it, and a page smaller than a screenful is not useful.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// ListPage is one page of a folder plus what a client needs to keep paging.
type ListPage struct {
	Messages []Message
	// Total is the number of messages matching the filter, not the number on
	// this page. A client uses it to show "1-50 of 812".
	Total int
	// Cursor is the value to pass as the next page's cursor, empty when the
	// page is the last one.
	CursorDate time.Time
	CursorID   int64
	HasMore    bool
}

// List returns a page of a folder, newest first by default.
func (r *MessageRepo) List(ctx context.Context, db *sqlx.DB, opts ListOptions) (ListPage, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	where := []string{"mailbox_id = $1"}
	args := []any{opts.MailboxID}

	if opts.UnreadOnly {
		where = append(where, `NOT ('\Seen' = ANY (flags))`)
	}
	if opts.Flagged {
		where = append(where, `'\Flagged' = ANY (flags)`)
	}
	hasCursor := !opts.CursorDate.IsZero()
	if hasCursor {
		// Row-value comparison is the whole point of the cursor: it is one
		// index scan and it cannot straddle a tie, where a pair of
		// "internal_date < d OR (internal_date = d AND id < i)" clauses can.
		args = append(args, opts.CursorDate, opts.CursorID)
		where = append(where, fmt.Sprintf("(internal_date, id) < ($%d, $%d)", len(args)-1, len(args)))
	}

	condition := strings.Join(where, " AND ")
	order := "DESC"
	if opts.Ascending {
		order = "ASC"
	}

	// One extra row is fetched and then dropped: that is how HasMore is known
	// without a second COUNT query that could disagree with this one.
	query := `
		SELECT id, mailbox_id, uid, blob_sha256, size, internal_date, flags,
		       subject, from_address, from_name, snippet, thread_key,
		       COALESCE(message_id, '') AS message_id, has_attachments
		FROM messages
		WHERE ` + condition + `
		ORDER BY internal_date ` + order + `, id ` + order + `
		LIMIT $` + fmt.Sprint(len(args)+1)

	args = append(args, limit+1)

	var rows []Message
	if err := db.SelectContext(ctx, &rows, query, args...); err != nil {
		return ListPage{}, fmt.Errorf("store: list messages: %w", err)
	}

	page := ListPage{Messages: rows}
	if len(rows) > limit {
		page.HasMore = true
		page.Messages = rows[:limit]
	}
	if n := len(page.Messages); n > 0 {
		last := page.Messages[n-1]
		page.CursorDate, page.CursorID = last.InternalDate, last.ID
	}

	var total int
	if err := db.GetContext(ctx, &total,
		`SELECT count(*) FROM messages WHERE `+condition, args[:len(args)-1]...); err != nil {
		return ListPage{}, fmt.Errorf("store: count messages: %w", err)
	}
	page.Total = total

	return page, nil
}

// ListAll returns every message of a folder in UID order.
//
// It is for delivery and search paths that must see the whole folder. A folder
// list in the UI uses List.
func (r *MessageRepo) ListAll(ctx context.Context, db *sqlx.DB, mailboxID int64) ([]Message, error) {
	return r.GetAllMessages(ctx, db, mailboxID)
}

// ThreadRow is one conversation in a folder listing.
//
// A thread is presented as its most recent message, because that is what the
// reader needs to decide what the conversation is about, plus the numbers a
// client shows beside it.
type ThreadRow struct {
	ThreadKey string
	Subject   string
	// MessageCount and UnreadCount span the whole conversation, including
	// messages in other folders, so that opening a thread cannot reveal more
	// messages than the badge promised.
	MessageCount int
	UnreadCount  int
	Latest       Message
}

// ListThreads returns the conversations in a folder, newest first.
func (r *MessageRepo) ListThreads(ctx context.Context, db *sqlx.DB, mailboxID int64, limit int) ([]ThreadRow, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	// The window function picks the newest message of each conversation, and
	// the join then counts the conversation as a whole. Doing it in one pass
	// keeps the counts and the preview row from being taken at different
	// moments, which on an active mailbox shows a preview of one message and a
	// count that includes another.
	const query = `
		WITH newest AS (
			SELECT DISTINCT ON (thread_key)
			       thread_key, id, mailbox_id, uid, blob_sha256, size, internal_date,
			       flags, subject, from_address, from_name, snippet,
			       COALESCE(message_id, '') AS message_id, has_attachments
			FROM messages
			WHERE mailbox_id = $1
			ORDER BY thread_key, internal_date DESC, id DESC
		)
		SELECT n.thread_key, n.subject, n.id, n.mailbox_id, n.uid, n.blob_sha256,
		       n.size, n.internal_date, n.flags, n.from_address, n.from_name,
		       n.snippet, n.message_id, n.has_attachments,
		       (SELECT count(*) FROM messages t WHERE t.thread_key = n.thread_key)::int AS message_count,
		       (SELECT count(*) FROM messages t WHERE t.thread_key = n.thread_key
		          AND NOT ('\Seen' = ANY (t.flags)))::int AS unread_count
		FROM newest n
		ORDER BY n.internal_date DESC, n.id DESC
		LIMIT $2`

	rows, err := db.QueryxContext(ctx, query, mailboxID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list threads: %w", err)
	}
	defer rows.Close()

	var out []ThreadRow
	for rows.Next() {
		var row ThreadRow
		if err := rows.Scan(&row.ThreadKey, &row.Subject,
			&row.Latest.ID, &row.Latest.MailboxID, &row.Latest.UID, &row.Latest.BlobSHA256,
			&row.Latest.Size, &row.Latest.InternalDate, &row.Latest.Flags,
			&row.Latest.FromAddress, &row.Latest.FromName, &row.Latest.Snippet,
			&row.Latest.MessageID, &row.Latest.HasAttachments,
			&row.MessageCount, &row.UnreadCount); err != nil {
			return nil, fmt.Errorf("store: scan thread: %w", err)
		}
		row.Latest.ThreadKey = row.ThreadKey
		row.Latest.ThreadSubject = row.Subject
		out = append(out, row)
	}
	return out, rows.Err()
}

// ThreadMessages returns every message of a conversation, oldest first.
//
// It is scoped to the owner as well as the thread key, because a thread key is
// only unique within a mailbox set: two senders can and do pick the same
// Message-ID.
func (r *MessageRepo) ThreadMessages(ctx context.Context, db *sqlx.DB, owner, threadKey string) ([]Message, error) {
	const query = `
		SELECT m.id, m.mailbox_id, m.uid, m.blob_sha256, m.size, m.internal_date,
		       m.flags, m.subject, m.from_address, m.from_name, m.snippet,
		       m.thread_key, COALESCE(m.message_id, '') AS message_id, m.has_attachments
		FROM messages m
		JOIN mailboxes b ON b.id = m.mailbox_id
		WHERE b.owner = $1 AND m.thread_key = $2
		ORDER BY m.internal_date, m.id`

	var messages []Message
	if err := db.SelectContext(ctx, &messages, query, owner, threadKey); err != nil {
		return nil, fmt.Errorf("store: list thread messages: %w", err)
	}
	return messages, nil
}

// SearchOptions selects a full-text search across an owner's mail.
type SearchOptions struct {
	Owner string
	// Folder restricts the search to one folder. Zero means every folder.
	FolderID int64
	// Text is the query. It goes through websearch_to_tsquery, so it is treated
	// as words rather than as a regular expression: a user typing "invoice (2)"
	// means those words, not a group.
	Text  string
	Limit int
}

// SearchMessages runs a full-text search over an owner's mail.
//
// The GIN index on search_vector is what makes this fast; the plainto_tsquery
// form is what makes it safe, since a query string reaches this from a request
// body and an arbitrary regex there is a denial of service.
func (r *MessageRepo) SearchMessages(ctx context.Context, db *sqlx.DB, opts SearchOptions) ([]Message, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	where := []string{"b.owner = $1"}
	args := []any{opts.Owner}

	if opts.FolderID != 0 {
		args = append(args, opts.FolderID)
		where = append(where, fmt.Sprintf("m.mailbox_id = $%d", len(args)))
	}
	if query := strings.TrimSpace(opts.Text); query != "" {
		args = append(args, query)
		where = append(where, fmt.Sprintf("m.search_vector @@ plainto_tsquery('english', $%d)", len(args)))
	}

	args = append(args, limit)

	selectStmt := `
		SELECT m.id, m.mailbox_id, m.uid, m.blob_sha256, m.size, m.internal_date,
		       m.flags, m.subject, m.from_address, m.from_name, m.snippet,
		       m.thread_key, COALESCE(m.message_id, '') AS message_id, m.has_attachments
		FROM messages m
		JOIN mailboxes b ON b.id = m.mailbox_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY m.internal_date DESC, m.id DESC
		LIMIT $` + fmt.Sprint(len(args))

	var messages []Message
	if err := db.SelectContext(ctx, &messages, selectStmt, args...); err != nil {
		return nil, fmt.Errorf("store: search messages: %w", err)
	}
	return messages, nil
}

// AddFlag sets a flag on a message without disturbing the others.
//
// IMAP stores flags as a set, and a client that stars one message must not
// clear the \Seen flag it set by opening it. Reassigning the array, as the
// IMAP path does when the client sends a whole flag list, would lose it.
func (r *MessageRepo) AddFlag(ctx context.Context, db *sqlx.DB, messageID int64, flag string) error {
	return r.arrayAdd(ctx, db, messageID, flag)
}

// RemoveFlag clears one flag from a message.
func (r *MessageRepo) RemoveFlag(ctx context.Context, db *sqlx.DB, messageID int64, flag string) error {
	return r.arrayRemove(ctx, db, messageID, flag)
}

func (r *MessageRepo) arrayAdd(ctx context.Context, db *sqlx.DB, messageID int64, flag string) error {
	// The WHERE keeps the array free of duplicates. That means a repeated add
	// changes no rows, which is indistinguishable from a missing message by row
	// count alone, so the row is checked separately below rather than reporting
	// a message that exists as not found.
	const query = `
		UPDATE messages
		SET flags = array_append(flags, $2)
		WHERE id = $1 AND NOT ($2 = ANY (flags))`

	affected, err := execFlagUpdate(ctx, db, messageID, flag, query)
	if err != nil {
		return err
	}
	if affected == 0 && !r.exists(ctx, db, messageID) {
		return fmt.Errorf("store: message %d: %w", messageID, store.ErrNotFound)
	}
	return nil
}

func (r *MessageRepo) arrayRemove(ctx context.Context, db *sqlx.DB, messageID int64, flag string) error {
	// array_remove rewrites the array even when the flag was not there, so
	// every existing row counts as affected and only a missing one is an error.
	const query = `UPDATE messages SET flags = array_remove(flags, $2) WHERE id = $1`

	if _, err := execFlagUpdate(ctx, db, messageID, flag, query); err != nil {
		return err
	}
	return nil
}

// execFlagUpdate runs a flag statement and returns how many rows it changed.
func execFlagUpdate(ctx context.Context, db *sqlx.DB, messageID int64, flag, query string) (int64, error) {
	res, err := db.ExecContext(ctx, query, messageID, flag)
	if err != nil {
		return 0, fmt.Errorf("store: update flag: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: update flag: %w", err)
	}
	return affected, nil
}

func (r *MessageRepo) exists(ctx context.Context, db *sqlx.DB, messageID int64) bool {
	var exists bool
	if err := db.GetContext(ctx, &exists,
		`SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1)`, messageID); err != nil {
		return false
	}
	return exists
}

func (r *MessageRepo) GetByID(ctx context.Context, db *sqlx.DB, owner string, messageID int64) (Message, error) {
	const query = `
		SELECT m.id, m.mailbox_id, m.uid, m.blob_sha256, m.size, m.internal_date,
		       m.flags, m.subject, m.from_address, m.from_name, m.snippet,
		       m.thread_key, COALESCE(m.message_id, '') AS message_id, m.has_attachments
		FROM messages m
		JOIN mailboxes b ON b.id = m.mailbox_id
		WHERE m.id = $1 AND b.owner = $2`

	var msg Message
	if err := db.GetContext(ctx, &msg, query, messageID, owner); err != nil {
		if store.IsNotFound(err) {
			return Message{}, fmt.Errorf("store: message %d for %s: %w", messageID, owner, store.ErrNotFound)
		}
		return Message{}, fmt.Errorf("store: get message: %w", err)
	}
	return msg, nil
}

// GetFolder resolves a folder for a request, returning ErrNotFound when the
// caller does not own it.
func (r *MailboxRepo) GetFolder(ctx context.Context, db *sqlx.DB, owner string, folderID int64) (Mailbox, error) {
	const query = `
		SELECT id, owner, name, parent_id, uidvalidity, uidnext,
		       special_use, subscribed, created_by
		FROM mailboxes
		WHERE id = $1 AND owner = $2`

	var mb Mailbox
	if err := db.GetContext(ctx, &mb, query, folderID, owner); err != nil {
		if store.IsNotFound(err) {
			return Mailbox{}, fmt.Errorf("store: folder %d for %s: %w", folderID, owner, store.ErrNotFound)
		}
		return Mailbox{}, fmt.Errorf("store: get folder: %w", err)
	}
	return mb, nil
}

// GetFolderByName resolves a folder by name for a request.
//
// The name is matched exactly rather than case-insensitively: IMAP folder names
// are case sensitive apart from INBOX, and folding here would make "work" and
// "Work" the same folder to the API while remaining distinct to the protocol.
func (r *MailboxRepo) GetFolderByName(ctx context.Context, db *sqlx.DB, owner, name string) (Mailbox, error) {
	if name == "" {
		return Mailbox{}, fmt.Errorf("store: folder name is empty: %w", store.ErrNotFound)
	}
	if strings.EqualFold(name, "INBOX") {
		name = "INBOX"
	} else {
		// The stored name is what the caller must send back, so that the API
		// echoes the name the mailbox actually has rather than a guess.
		mb, err := r.GetByName(ctx, db, owner, name)
		if err != nil {
			return Mailbox{}, err
		}
		return mb, nil
	}
	return r.GetByName(ctx, db, owner, name)
}

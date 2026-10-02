package imapd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	imap "github.com/emersion/go-imap/v2"
)

// Message holds message metadata for IMAP.
type Message struct {
	ID           int64     `db:"id"`
	MailboxID    int64     `db:"mailbox_id"`
	UID          uint32    `db:"uid"`
	BlobSHA256   []byte    `db:"blob_sha256"`
	Size         int64     `db:"size"`
	InternalDate time.Time `db:"internal_date"`
	Flags        []string  `db:"flags"`
	Subject      string    `db:"subject"`
	FromAddress  string    `db:"from_address"`
	FromName     string    `db:"from_name"`
	Snippet      string    `db:"snippet"`
	ThreadKey    string    `db:"thread_key"`
	MessageID    string    `db:"message_id"`
}

// MessageResult holds the result of an insert.
type MessageResult struct {
	ID  int64
	UID uint32
}

// MessageRepo handles message CRUD operations.
type MessageRepo struct{}

// NewMessageRepo returns a message repository.
func NewMessageRepo() *MessageRepo {
	return &MessageRepo{}
}

// Insert adds a message to a mailbox.
func (r *MessageRepo) Insert(ctx context.Context, db *sqlx.DB, mailboxID int64,
	blobSHA256 []byte, size int64, internalDate time.Time,
	flags []string, fromAddr, fromName, subject, snippet string,
) (MessageResult, error) {

	const insertQuery = `
		INSERT INTO messages (mailbox_id, uid, blob_sha256, size, internal_date,
		                      flags, subject, from_address, from_name, snippet,
		                      thread_key, message_id)
		VALUES ($1, (SELECT uidnext FROM mailboxes WHERE id = $2),
		        $3, $4, $5, $6, $7, $8, $9, $10,
		        gen_random_uuid()::text, gen_random_uuid()::text)
		RETURNING id, uid`

	var result MessageResult
	if err := db.GetContext(ctx, &result, insertQuery,
		mailboxID, mailboxID, blobSHA256, size, internalDate, flags,
		subject, fromAddr, fromName, snippet); err != nil {
		return MessageResult{}, fmt.Errorf("store: insert message: %w", err)
	}

	// Update uidnext for the mailbox.
	if _, err := db.ExecContext(ctx,
		`UPDATE mailboxes SET uidnext = $2, updated_at = now() WHERE id = $1`,
		mailboxID, result.UID+1); err != nil {
		return MessageResult{}, fmt.Errorf("store: update uidnext: %w", err)
	}

	return result, nil
}

// GetByUIDSet returns messages matching a UID set.
func (r *MessageRepo) GetByUIDSet(ctx context.Context, db *sqlx.DB,
	mailboxID int64, numSet imap.NumSet,
) ([]Message, error) {

	var uids []uint32

	switch ns := numSet.(type) {
	case imap.SeqSet:
		for _, r := range ns {
			for i := r.Start; i <= r.Stop; i++ {
				uids = append(uids, i)
			}
		}
	case imap.UIDSet:
		for _, r := range ns {
			for i := r.Start; i <= r.Stop; i++ {
				uids = append(uids, uint32(i))
			}
		}
	default:
		return nil, fmt.Errorf("unsupported numSet type")
	}

	if len(uids) == 0 {
		return nil, nil
	}

	// Build query with mailbox_id = $1 and uid IN ($2, $3, ...)
	allArgs := make([]any, 0, len(uids)+1)
	allArgs = append(allArgs, mailboxID)
	for _, uid := range uids {
		allArgs = append(allArgs, uid)
	}
	inPlaceholders := make([]string, len(uids))
	for i := range uids {
		inPlaceholders[i] = fmt.Sprintf("$%d", i+2)
	}

	query := fmt.Sprintf(`
		SELECT id, mailbox_id, uid, blob_sha256, size, internal_date,
		       flags, subject, from_address, from_name, thread_key, message_id
		FROM messages
		WHERE mailbox_id = $1 AND uid IN (%s)
		ORDER BY uid`, strings.Join(inPlaceholders, ", "))

	var messages []Message
	if err := db.SelectContext(ctx, &messages, query, allArgs...); err != nil {
		return nil, fmt.Errorf("store: get messages: %w", err)
	}

	return messages, nil
}

// CountByMailbox returns the number of messages in a mailbox.
func (r *MessageRepo) CountByMailbox(ctx context.Context, db *sqlx.DB, mailboxID int64) (int, error) {
	var count int
	const query = `SELECT count(*) FROM messages WHERE mailbox_id = $1`
	if err := db.GetContext(ctx, &count, query, mailboxID); err != nil {
		return 0, fmt.Errorf("store: count messages: %w", err)
	}
	return count, nil
}

// GetDeletedMessages returns all messages with the \Deleted flag in a mailbox.
func (r *MessageRepo) GetDeletedMessages(ctx context.Context, db *sqlx.DB, mailboxID int64) ([]Message, error) {
	const query = `
		SELECT id, mailbox_id, uid, blob_sha256, size, internal_date,
		       flags, subject, from_address, from_name, thread_key, message_id
		FROM messages
		WHERE mailbox_id = $1 AND flags::text LIKE '%Deleted%'
		ORDER BY uid`

	var messages []Message
	if err := db.SelectContext(ctx, &messages, query, mailboxID); err != nil {
		return nil, fmt.Errorf("store: get deleted messages: %w", err)
	}

	return messages, nil
}

// GetAllMessages returns all messages in a mailbox (for debugging).
func (r *MessageRepo) GetAllMessages(ctx context.Context, db *sqlx.DB, mailboxID int64) ([]Message, error) {
	const query = `
		SELECT id, mailbox_id, uid, blob_sha256, size, internal_date,
		       flags, subject, from_address, from_name, thread_key, message_id
		FROM messages
		WHERE mailbox_id = $1
		ORDER BY uid`

	var messages []Message
	if err := db.SelectContext(ctx, &messages, query, mailboxID); err != nil {
		return nil, fmt.Errorf("store: get all messages: %w", err)
	}

	return messages, nil
}

// SearchSeq searches by sequence numbers.
func (r *MessageRepo) SearchSeq(ctx context.Context, db *sqlx.DB,
	mailboxID int64, criteria *imap.SearchCriteria,
) ([]uint32, error) {
	return r.searchUID(ctx, db, mailboxID, criteria)
}

// SearchUID searches by UIDs.
func (r *MessageRepo) SearchUID(ctx context.Context, db *sqlx.DB,
	mailboxID int64, criteria *imap.SearchCriteria,
) ([]uint32, error) {
	return r.searchUID(ctx, db, mailboxID, criteria)
}

func (r *MessageRepo) searchUID(ctx context.Context, db *sqlx.DB,
	mailboxID int64, criteria *imap.SearchCriteria,
) ([]uint32, error) {
	query, args := buildSearchQuery(mailboxID, criteria)

	rows, err := db.QueryxContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search messages: %w", err)
	}
	defer rows.Close()

	var uids []uint32
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("store: scan uid: %w", err)
		}
		uids = append(uids, uid)
	}

	return uids, nil
}

func buildSearchQuery(mailboxID int64, criteria *imap.SearchCriteria) (string, []any) {
	var conditions []string
	var args []any
	argIndex := 1

	// Base condition
	conditions = append(conditions, fmt.Sprintf("mailbox_id = $%d", argIndex))
	args = append(args, mailboxID)
	argIndex++

	// UID search
	if len(criteria.UID) > 0 {
		for _, uidSet := range criteria.UID {
			placeholders := make([]string, 0, len(uidSet))
			for _, uidRange := range uidSet {
				for i := uidRange.Start; i <= uidRange.Stop; i++ {
					placeholders = append(placeholders, fmt.Sprintf("$%d", argIndex))
					args = append(args, uint32(i))
					argIndex++
				}
			}
			conditions = append(conditions, fmt.Sprintf("uid IN (%s)", strings.Join(placeholders, ",")))
		}
	}

	// Sequence number search (treat as UID for simplicity)
	if len(criteria.SeqNum) > 0 {
		for _, seqSet := range criteria.SeqNum {
			placeholders := make([]string, 0, len(seqSet))
			for _, seqRange := range seqSet {
				for i := seqRange.Start; i <= seqRange.Stop; i++ {
					placeholders = append(placeholders, fmt.Sprintf("$%d", argIndex))
					args = append(args, i)
					argIndex++
				}
			}
			conditions = append(conditions, fmt.Sprintf("uid IN (%s)", strings.Join(placeholders, ",")))
		}
	}

	// Flag search
	if len(criteria.Flag) > 0 {
		for _, flag := range criteria.Flag {
			conditions = append(conditions, fmt.Sprintf("flags @> ARRAY[$%d::text]", argIndex))
			args = append(args, string(flag))
			argIndex++
		}
	}

	// Not flag search
	if len(criteria.NotFlag) > 0 {
		for _, flag := range criteria.NotFlag {
			conditions = append(conditions, fmt.Sprintf("NOT (flags @> ARRAY[$%d::text])", argIndex))
			args = append(args, string(flag))
			argIndex++
		}
	}

	// Date range: Since (messages since date)
	if !criteria.Since.IsZero() {
		conditions = append(conditions, fmt.Sprintf("internal_date >= $%d", argIndex))
		args = append(args, criteria.Since)
		argIndex++
	}

	// Date range: Before (messages before date)
	if !criteria.Before.IsZero() {
		conditions = append(conditions, fmt.Sprintf("internal_date < $%d", argIndex))
		args = append(args, criteria.Before)
		argIndex++
	}

	// Date range: SentSince (messages sent since date)
	if !criteria.SentSince.IsZero() {
		conditions = append(conditions, fmt.Sprintf("internal_date >= $%d", argIndex))
		args = append(args, criteria.SentSince)
		argIndex++
	}

	// Date range: SentBefore (messages sent before date)
	if !criteria.SentBefore.IsZero() {
		conditions = append(conditions, fmt.Sprintf("internal_date < $%d", argIndex))
		args = append(args, criteria.SentBefore)
		argIndex++
	}

	// Size constraints
	if criteria.Larger > 0 {
		conditions = append(conditions, fmt.Sprintf("size > $%d", argIndex))
		args = append(args, criteria.Larger)
		argIndex++
	}

	if criteria.Smaller > 0 {
		conditions = append(conditions, fmt.Sprintf("size < $%d", argIndex))
		args = append(args, criteria.Smaller)
		argIndex++
	}

	// Header search (FROM, SUBJECT, etc.)
	for _, header := range criteria.Header {
		key := strings.ToLower(header.Key)
		value := header.Value

		switch key {
		case "from":
			conditions = append(conditions, fmt.Sprintf("(from_address ILIKE $%d OR from_name ILIKE $%d)", argIndex, argIndex+1))
			args = append(args, "%"+value+"%", "%"+value+"%")
			argIndex += 2
		case "to":
			// To is not stored separately, search in snippet/body
			conditions = append(conditions, fmt.Sprintf("search_vector @@ websearch_to_tsquery('english', $%d)", argIndex))
			args = append(args, value)
			argIndex++
		case "subject":
			conditions = append(conditions, fmt.Sprintf("search_vector @@ websearch_to_tsquery('english', $%d)", argIndex))
			args = append(args, value)
			argIndex++
		default:
			// For other headers, search in snippet
			conditions = append(conditions, fmt.Sprintf("search_vector @@ websearch_to_tsquery('english', $%d)", argIndex))
			args = append(args, value)
			argIndex++
		}
	}

	// Body search
	for _, body := range criteria.Body {
		conditions = append(conditions, fmt.Sprintf("search_vector @@ websearch_to_tsquery('english', $%d)", argIndex))
		args = append(args, body)
		argIndex++
	}

	// Text search (subject, from, body combined)
	for _, text := range criteria.Text {
		conditions = append(conditions, fmt.Sprintf("search_vector @@ websearch_to_tsquery('english', $%d)", argIndex))
		args = append(args, text)
		argIndex++
	}

	// Build WHERE clause
	whereClause := strings.Join(conditions, " AND ")
	query := fmt.Sprintf("SELECT uid FROM messages WHERE %s ORDER BY uid", whereClause)

	return query, args
}

// UpdateFlags updates message flags.
func (r *MessageRepo) UpdateFlags(ctx context.Context, db *sqlx.DB, messageID int64, flags []string) error {
	const query = `UPDATE messages SET flags = $2 WHERE id = $1`
	_, err := db.ExecContext(ctx, query, messageID, flags)
	return err
}

// DeleteByUID removes a message by UID.
func (r *MessageRepo) DeleteByUID(ctx context.Context, db *sqlx.DB, mailboxID int64, uid uint32) error {
	const query = `DELETE FROM messages WHERE mailbox_id = $1 AND uid = $2`
	result, err := db.ExecContext(ctx, query, mailboxID, uid)
	if err != nil {
		return fmt.Errorf("store: delete message: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("store: message not found")
	}

	// Adjust quota.
	_, err = db.ExecContext(ctx,
		`UPDATE quota_usage SET used_bytes = GREATEST(used_bytes - (SELECT size FROM messages WHERE mailbox_id = $1 AND uid = $2), 0),
		 message_count = GREATEST(message_count - 1, 0), updated_at = now()
		 WHERE owner = (SELECT owner FROM mailboxes WHERE id = $1)`,
		mailboxID, uid)

	return err
}

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
	flags []string, fromAddr, fromName, subject string,
) (MessageResult, error) {

	const insertQuery = `
		INSERT INTO messages (mailbox_id, uid, blob_sha256, size, internal_date,
		                      flags, subject, from_address, from_name, thread_key, message_id)
		VALUES ($1, (SELECT uidnext FROM mailboxes WHERE id = $2),
		        $3, $4, $5, $6, $7, $8, $9, gen_random_uuid()::text, gen_random_uuid()::text)
		RETURNING id, uid`

	var result MessageResult
	if err := db.GetContext(ctx, &result, insertQuery,
		mailboxID, mailboxID, blobSHA256, size, internalDate, flags,
		subject, fromAddr, fromName); err != nil {
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

	placeholders := make([]string, len(uids))
	args := make([]any, len(uids))
	for i, uid := range uids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = uid
	}

	query := fmt.Sprintf(`
		SELECT id, mailbox_id, uid, blob_sha256, size, internal_date,
		       flags, subject, from_address, from_name, thread_key, message_id
		FROM messages
		WHERE mailbox_id = $%d AND uid IN (%s)
		ORDER BY uid`, len(uids)+1, strings.Join(placeholders, ", "))

	args = append([]any{mailboxID}, args...)

	var messages []Message
	if err := db.SelectContext(ctx, &messages, query, args...); err != nil {
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
	// Simple search: match all messages in the mailbox.
	// Extended search would parse criteria for FROM, TO, SUBJECT, DATE, etc.
	const query = `SELECT uid FROM messages WHERE mailbox_id = $1 ORDER BY uid`

	rows, err := db.QueryxContext(ctx, query, mailboxID)
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

// UpdateFlags updates message flags.
func (r *MessageRepo) UpdateFlags(ctx context.Context, db *sqlx.DB, messageID int64, flags []string) error {
	const query = `UPDATE messages SET flags = $2, updated_at = now() WHERE id = $1`
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

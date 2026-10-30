package imapd

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	imap "github.com/emersion/go-imap/v2"

	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
)

// Message holds message metadata for IMAP.
type Message struct {
	ID             int64     `db:"id"`
	MailboxID      int64     `db:"mailbox_id"`
	UID            uint32    `db:"uid"`
	BlobSHA256     []byte    `db:"blob_sha256"`
	Size           int64     `db:"size"`
	InternalDate   time.Time `db:"internal_date"`
	Flags          []string  `db:"flags"`
	Subject        string    `db:"subject"`
	FromAddress    string    `db:"from_address"`
	FromName       string    `db:"from_name"`
	Snippet        string    `db:"snippet"`
	ThreadKey      string    `db:"thread_key"`
	ThreadSubject  string    `db:"thread_subject"`
	MessageID      string    `db:"message_id"`
	HasAttachments bool      `db:"has_attachments"`
	// ItemClass is the MS-OXPROPS class. See mail.ItemClass.
	ItemClass string `db:"item_class"`
}

// itemClassOf returns a class that is never empty.
//
// The column has a default but a row can still be written without one, and a
// client filtering on an empty class matches nothing -- which is how a folder
// full of mail ends up showing an empty search result.
func itemClassOf(meta mail.Metadata) string {
	if meta.ItemClass == "" {
		return mail.ItemClassNote
	}
	return meta.ItemClass
}

// metadata rebuilds the storeable summary from a stored row, for the paths
// that re-insert an existing message: IMAP COPY.
func (m Message) metadata() mail.Metadata {
	return mail.Metadata{
		Subject:        m.Subject,
		FromAddress:    m.FromAddress,
		FromName:       m.FromName,
		Snippet:        m.Snippet,
		MessageID:      m.MessageID,
		ThreadKey:      m.ThreadKey,
		ThreadSubject:  m.ThreadSubject,
		HasAttachments: m.HasAttachments,
		Size:           m.Size,
		Date:           m.InternalDate,
		ItemClass:      m.ItemClass,
	}
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

// Insert adds a message to a mailbox and records what is known about it.
//
// The whole thing is one transaction. The parts have to agree: a message row
// whose UID was reserved but never written would leave a hole in the UID
// sequence, and a row written without its references would group into threads
// only until the next message arrived.
//
// The UID is reserved by an UPDATE ... RETURNING rather than by reading
// uidnext and writing it back. Two deliveries to the same mailbox can run at
// once, and a read-then-write would hand both the same UID.
func (r *MessageRepo) Insert(ctx context.Context, db *sqlx.DB, mailboxID int64,
	blobSHA256 []byte, size int64, internalDate time.Time,
	flags []string, meta mail.Metadata,
) (MessageResult, error) {

	var result MessageResult
	err := store.InTx(ctx, db, func(tx *sqlx.Tx) error {
		var err error
		result, err = r.insertTx(ctx, tx, mailboxID, blobSHA256, size, internalDate, flags, meta)
		return err
	})
	if err != nil {
		return MessageResult{}, err
	}
	return result, nil
}

// InsertTx adds a message inside a transaction the caller already owns. Import
// jobs use it so that a whole batch commits or rolls back as one unit, rather
// than leaving a half-imported batch behind when the connection drops.
func (r *MessageRepo) InsertTx(ctx context.Context, tx *sqlx.Tx, mailboxID int64,
	blobSHA256 []byte, size int64, internalDate time.Time,
	flags []string, meta mail.Metadata,
) (MessageResult, error) {
	return r.insertTx(ctx, tx, mailboxID, blobSHA256, size, internalDate, flags, meta)
}

func (r *MessageRepo) insertTx(ctx context.Context, tx *sqlx.Tx, mailboxID int64,
	blobSHA256 []byte, size int64, internalDate time.Time,
	flags []string, meta mail.Metadata,
) (MessageResult, error) {

	// A nil slice would be sent as SQL NULL, and the column is NOT NULL. A
	// message with no flags is an empty set, not an absent value.
	if flags == nil {
		flags = []string{}
	}

	var result MessageResult

	threadKey, err := adoptThread(ctx, tx, mailboxID, meta, internalDate)
	if err != nil {
		return MessageResult{}, err
	}

	const reserveUID = `
		UPDATE mailboxes SET uidnext = uidnext + 1, updated_at = now()
		WHERE id = $1
		RETURNING uidnext - 1`
	if err := tx.GetContext(ctx, &result.UID, reserveUID, mailboxID); err != nil {
		return MessageResult{}, fmt.Errorf("store: reserve uid: %w", err)
	}

	const insertMessage = `
		INSERT INTO messages (mailbox_id, uid, blob_sha256, size, internal_date,
		                      flags, subject, from_address, from_name, snippet,
		                      message_id, thread_key, thread_subject, has_attachments,
		                      item_class)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''),
		        $12, $13, $14, $15)
		RETURNING id`
	var id int64
	err = tx.GetContext(ctx, &id, insertMessage,
		mailboxID, result.UID, blobSHA256, size, internalDate, flags,
		meta.Subject, meta.FromAddress, meta.FromName, meta.Snippet,
		meta.MessageID, threadKey, meta.ThreadSubject, meta.HasAttachments,
		itemClassOf(meta))
	if err != nil {
		return MessageResult{}, fmt.Errorf("store: insert message: %w", err)
	}
	result.ID = id

	if err := insertAddresses(ctx, tx, id, meta); err != nil {
		return MessageResult{}, err
	}
	if err := insertReferences(ctx, tx, id, meta.References); err != nil {
		return MessageResult{}, err
	}
	return result, nil
}

// insertAddresses records the header addresses that IMAP SEARCH, recipient
// autocomplete and conversation grouping read instead of parsing bodies.
//
// The addresses are written even though the same values live in the blob,
// because a search over every message must not read every blob.
func insertAddresses(ctx context.Context, tx *sqlx.Tx, messageID int64, meta mail.Metadata) error {
	const query = `
		INSERT INTO message_addresses (message_id, header_name, address, display_name, position)
		VALUES ($1, $2, $3, $4, $5)`

	groups := []struct {
		header string
		list   []mail.Address
	}{
		{"From", []mail.Address{{Name: meta.FromName, Address: meta.FromAddress}}},
		{"To", meta.To},
		{"Cc", meta.Cc},
		{"Bcc", meta.Bcc},
		{"Reply-To", meta.ReplyTo},
	}

	for _, group := range groups {
		for i, addr := range group.list {
			if addr.Address == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, query, messageID, group.header, addr.Address, addr.Name, i); err != nil {
				return fmt.Errorf("store: insert %s address: %w", group.header, err)
			}
		}
	}
	return nil
}

// insertReferences records the message IDs this message answers, which is what
// conversation grouping follows when the thread key alone is not enough.
func insertReferences(ctx context.Context, tx *sqlx.Tx, messageID int64, references []string) error {
	const query = `
		INSERT INTO message_references (message_id, reference, position)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`

	for i, ref := range references {
		if _, err := tx.ExecContext(ctx, query, messageID, ref, i); err != nil {
			return fmt.Errorf("store: insert reference: %w", err)
		}
	}
	return nil
}

// GetByUIDSet returns messages matching a sequence or UID set.
//
// This server reports a message's UID as its sequence number, so a sequence
// set is matched against the uid column just like a UID set; that keeps a
// client's follow-up FETCH consistent with the sequence numbers it was given.
//
// A sequence number of "*" is encoded by go-imap as 0, so a range is never
// expanded by looping Start..Stop: "1:*" would loop 1..0 and match nothing,
// which is exactly the first fetch almost every client sends. Instead the
// wildcard is resolved against the mailbox's highest UID and each range
// becomes a BETWEEN predicate, so "1:*" is one bounded scan rather than one
// query argument per message.
func (r *MessageRepo) GetByUIDSet(ctx context.Context, db *sqlx.DB,
	mailboxID int64, numSet imap.NumSet,
) ([]Message, error) {

	var conds []string
	var args []any

	// Placeholders start at $2: $1 is the mailbox_id that is prepended to the
	// argument list when the query is assembled.
	appendRange := func(start, stop uint32) {
		switch {
		case stop == 0 && start == 0:
			// "*" alone: everything.
		case stop == 0:
			conds = append(conds, fmt.Sprintf("uid >= $%d", len(args)+2))
			args = append(args, start)
		case start == 0:
			// "*:n" — "*" is the ceiling, so this is bounded above only.
			conds = append(conds, fmt.Sprintf("uid <= $%d", len(args)+2))
			args = append(args, stop)
		default:
			conds = append(conds, fmt.Sprintf("uid BETWEEN $%d AND $%d",
				len(args)+2, len(args)+3))
			args = append(args, start, stop)
		}
	}

	switch ns := numSet.(type) {
	case imap.SeqSet:
		// Resolve "*" to the highest UID so the range is bounded.
		ceil, err := r.maxUID(ctx, db, mailboxID)
		if err != nil {
			return nil, err
		}
		for _, rng := range ns {
			start, stop := rng.Start, rng.Stop
			if start == 0 {
				start = ceil
			}
			if stop == 0 {
				stop = ceil
			}
			appendRange(start, stop)
		}
	case imap.UIDSet:
		ceil, err := r.maxUID(ctx, db, mailboxID)
		if err != nil {
			return nil, err
		}
		for _, rng := range ns {
			start, stop := uint32(rng.Start), uint32(rng.Stop)
			if start == 0 {
				start = ceil
			}
			if stop == 0 {
				stop = ceil
			}
			appendRange(start, stop)
		}
	default:
		return nil, fmt.Errorf("unsupported numSet type")
	}

	if len(conds) == 0 {
		return nil, nil
	}

	query := `
		SELECT id, mailbox_id, uid, blob_sha256, size, internal_date,
		       flags, subject, from_address, from_name, snippet, thread_key,
		       COALESCE(message_id, '') AS message_id, has_attachments, item_class
		FROM messages
		WHERE mailbox_id = $1 AND (` + strings.Join(conds, " OR ") + `
		)
		ORDER BY uid`

	allArgs := append([]any{mailboxID}, args...)

	var messages []Message
	if err := db.SelectContext(ctx, &messages, query, allArgs...); err != nil {
		return nil, fmt.Errorf("store: get messages: %w", err)
	}

	return messages, nil
}

// maxUID returns the highest UID in a mailbox, or 0 when it is empty.
//
// It backs the "*" wildcard in FETCH and SEARCH number sets: "*" means the
// last message, and because this server reports a UID as its sequence number,
// the last message is the one with the highest UID whether the set is a
// sequence set or a UID set.
func (r *MessageRepo) maxUID(ctx context.Context, db *sqlx.DB, mailboxID int64) (uint32, error) {
	var max sql.NullInt64
	const query = `SELECT max(uid) FROM messages WHERE mailbox_id = $1`
	if err := db.GetContext(ctx, &max, query, mailboxID); err != nil {
		return 0, fmt.Errorf("store: max uid: %w", err)
	}
	if !max.Valid {
		return 0, nil
	}
	return uint32(max.Int64), nil
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
		       flags, subject, from_address, from_name, snippet, thread_key,
		       COALESCE(message_id, '') AS message_id, has_attachments, item_class
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
		       flags, subject, from_address, from_name, snippet, thread_key,
		       COALESCE(message_id, '') AS message_id, has_attachments, item_class
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
	// "*" is a legal bound in a SEARCH number set and encodes as zero, so the
	// query builder needs the mailbox's highest UID to resolve it.
	ceil, err := r.maxUID(ctx, db, mailboxID)
	if err != nil {
		return nil, err
	}
	query, args := buildSearchQuery(mailboxID, criteria, ceil)

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

// rangeCond renders one IMAP number-set range as a SQL predicate on the uid
// column, appending its bound arguments. Both bounds are already resolved:
// a wildcard "*" arrives here as the mailbox ceiling, never as zero.
func rangeCond(argIndex *int, args *[]any, start, stop uint32) string {
	if start == stop {
		s := fmt.Sprintf("uid = $%d", *argIndex)
		*args = append(*args, start)
		*argIndex++
		return s
	}
	s := fmt.Sprintf("uid BETWEEN $%d AND $%d", *argIndex, *argIndex+1)
	*args = append(*args, start, stop)
	*argIndex += 2
	return s
}

// buildSearchQuery turns IMAP search criteria into SQL.
//
// ceil is the mailbox's highest UID: a number set may name "*" (encoded as
// zero) as either bound, and without the ceiling there is nothing to expand
// it to. Ranges become BETWEEN predicates so a wildcard search stays one
// bounded scan instead of one argument per message.
func buildSearchQuery(mailboxID int64, criteria *imap.SearchCriteria, ceil uint32) (string, []any) {
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
			parts := make([]string, 0, len(uidSet))
			for _, uidRange := range uidSet {
				start, stop := uint32(uidRange.Start), uint32(uidRange.Stop)
				if start == 0 {
					start = ceil
				}
				if stop == 0 {
					stop = ceil
				}
				parts = append(parts, rangeCond(&argIndex, &args, start, stop))
			}
			conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
		}
	}

	// Sequence number search. This server reports a UID as its sequence number,
	// so the same column carries both.
	if len(criteria.SeqNum) > 0 {
		for _, seqSet := range criteria.SeqNum {
			parts := make([]string, 0, len(seqSet))
			for _, seqRange := range seqSet {
				start, stop := seqRange.Start, seqRange.Stop
				if start == 0 {
					start = ceil
				}
				if stop == 0 {
					stop = ceil
				}
				parts = append(parts, rangeCond(&argIndex, &args, start, stop))
			}
			conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
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

// DeleteByUID removes a message by UID and gives the owner's quota back.
//
// The size and owner are read before the row disappears. Selecting them from
// the same DELETE would leave a NULL for the quota adjustment, because the
// row is already gone by then.
func (r *MessageRepo) DeleteByUID(ctx context.Context, db *sqlx.DB, mailboxID int64, uid uint32) error {
	return store.InTx(ctx, db, func(tx *sqlx.Tx) error {
		var doomed struct {
			Size  int64  `db:"size"`
			Owner string `db:"owner"`
		}
		const lookup = `
			SELECT m.size, b.owner
			FROM messages m
			JOIN mailboxes b ON b.id = m.mailbox_id
			WHERE m.mailbox_id = $1 AND m.uid = $2`
		if err := tx.GetContext(ctx, &doomed, lookup, mailboxID, uid); err != nil {
			if store.IsNotFound(err) {
				return fmt.Errorf("store: message not found: %w", err)
			}
			return fmt.Errorf("store: look up message for delete: %w", err)
		}

		const del = `DELETE FROM messages WHERE mailbox_id = $1 AND uid = $2`
		if _, err := tx.ExecContext(ctx, del, mailboxID, uid); err != nil {
			return fmt.Errorf("store: delete message: %w", err)
		}

		const adjust = `
			UPDATE quota_usage
			SET used_bytes = GREATEST(used_bytes - $2, 0),
			    message_count = GREATEST(message_count - 1, 0),
			    updated_at = now()
			WHERE owner = $1`
		if _, err := tx.ExecContext(ctx, adjust, doomed.Owner, doomed.Size); err != nil {
			return fmt.Errorf("store: adjust quota after delete: %w", err)
		}
		return nil
	})
}

// MoveTo relocates messages into another mailbox.
//
// A move is a relocation, not a copy followed by a delete: the rows keep their
// message_id, thread key, reference rows and address rows, so a message that
// is moved mid-conversation still threads with its replies. It also cannot
// change the owner's usage, because the same bytes are now in a different
// folder rather than in two of them.
//
// UIDs are new in the destination, as RFC 3501 requires: a UID is only unique
// within its own mailbox.
func (r *MessageRepo) MoveTo(ctx context.Context, db *sqlx.DB,
	fromMailboxID int64, uids []uint32, toMailboxID int64) ([]uint32, error) {

	if len(uids) == 0 {
		return nil, nil
	}

	var destUIDs []uint32
	err := store.InTx(ctx, db, func(tx *sqlx.Tx) error {
		destUIDs = make([]uint32, 0, len(uids))
		for _, uid := range uids {
			var newUID uint32
			if err := tx.GetContext(ctx, &newUID, `
				UPDATE mailboxes SET uidnext = uidnext + 1, updated_at = now()
				WHERE id = $1 RETURNING uidnext - 1`, toMailboxID); err != nil {
				return fmt.Errorf("store: reserve uid for move: %w", err)
			}

			res, err := tx.ExecContext(ctx, `
				UPDATE messages SET mailbox_id = $1, uid = $2
				WHERE mailbox_id = $3 AND uid = $4`, toMailboxID, newUID, fromMailboxID, uid)
			if err != nil {
				return fmt.Errorf("store: move message: %w", err)
			}
			moved, _ := res.RowsAffected()
			if moved == 0 {
				return fmt.Errorf("store: message %d not found in mailbox for move", uid)
			}

			// \Recent belongs to the session that saw the message arrive, and a
			// message the client has just moved is no longer new to it.
			if _, err := tx.ExecContext(ctx,
				`UPDATE messages SET flags = array_remove(flags, '\Recent')
				 WHERE mailbox_id = $1 AND uid = $2`, toMailboxID, newUID); err != nil {
				return fmt.Errorf("store: clear \\Recent on move: %w", err)
			}
			destUIDs = append(destUIDs, newUID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return destUIDs, nil
}

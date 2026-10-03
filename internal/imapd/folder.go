package imapd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/store"
)

// SystemFolders are the mailboxes every owner gets, created together when the
// first one is needed.
//
// INBOX is deliberately absent: it is not a nicety, it is where delivery puts
// mail, and a mailbox that lacked it would bounce every message. The rest are
// where a client files what the user does rather than receives, so provisioning
// them at the same moment means a folder list is never half-built.
var SystemFolders = []struct {
	Name       string
	SpecialUse string
}{
	{Name: "Sent", SpecialUse: `\sent`},
	{Name: "Drafts", SpecialUse: `\drafts`},
	{Name: "Junk", SpecialUse: `\junk`},
	{Name: "Trash", SpecialUse: `\trash`},
	{Name: "Archive", SpecialUse: `\archive`},
}

// FolderCounts is what a folder list shows next to each name.
type FolderCounts struct {
	Total   int
	Unread  int
	Flagged int
}

// FolderStats returns the counts for every folder of an owner in one pass.
//
// A folder list that ran a count query per folder would scale with the number
// of folders and hold a connection per row; one grouped scan does the same work
// on every platform, and the numbers are consistent with each other because
// they come from the same snapshot.
func (r *MailboxRepo) FolderStats(ctx context.Context, db *sqlx.DB, owner string) (map[int64]FolderCounts, error) {
	const query = `
		SELECT m.id AS mailbox_id,
		       count(msg.id) AS total,
		       count(msg.id) FILTER (WHERE NOT ('\Seen' = ANY (msg.flags))) AS unread,
		       count(msg.id) FILTER (WHERE '\Flagged' = ANY (msg.flags)) AS flagged
		FROM mailboxes m
		LEFT JOIN messages msg ON msg.mailbox_id = m.id
		WHERE m.owner = $1
		GROUP BY m.id`

	rows, err := db.QueryxContext(ctx, query, owner)
	if err != nil {
		return nil, fmt.Errorf("store: folder stats: %w", err)
	}
	defer rows.Close()

	out := map[int64]FolderCounts{}
	for rows.Next() {
		var id int64
		var counts FolderCounts
		if err := rows.Scan(&id, &counts.Total, &counts.Unread, &counts.Flagged); err != nil {
			return nil, fmt.Errorf("store: scan folder stats: %w", err)
		}
		out[id] = counts
	}
	return out, rows.Err()
}

// ListWithCounts returns an owner's folders with their counts attached.
func (r *MailboxRepo) ListWithCounts(ctx context.Context, db *sqlx.DB, owner string) ([]Mailbox, []FolderCounts, error) {
	mailboxes, err := r.ListForUser(ctx, db, owner)
	if err != nil {
		return nil, nil, err
	}
	stats, err := r.FolderStats(ctx, db, owner)
	if err != nil {
		return nil, nil, err
	}
	counts := make([]FolderCounts, len(mailboxes))
	for i, mb := range mailboxes {
		counts[i] = stats[mb.ID]
	}
	return mailboxes, counts, nil
}

// EnsureFolders creates INBOX and the standard folders for an owner if they are
// missing.
//
// It is idempotent and safe to call on every delivery: the names are unique per
// owner, so a second call inserts nothing and a concurrent one loses the race
// to the unique constraint rather than creating a duplicate.
func (r *MailboxRepo) EnsureFolders(ctx context.Context, q store.Querier, owner string) error {
	const insertInbox = `
		INSERT INTO mailboxes (owner, name, uidvalidity, uidnext)
		VALUES ($1, 'INBOX', 1, 1)
		ON CONFLICT (owner, name) DO NOTHING`
	if _, err := q.ExecContext(ctx, insertInbox, owner); err != nil {
		return fmt.Errorf("store: create INBOX for %s: %w", owner, err)
	}

	// The folder set lives in system_folders so that adding one is a migration
	// rather than a code change, and so the ordering the UI shows is declared
	// with the rows.
	const insertSystem = `
		INSERT INTO mailboxes (owner, name, uidvalidity, uidnext, special_use, subscribed)
		SELECT $1, sf.name, 1, 1, sf.special_use, true
		FROM system_folders sf
		ON CONFLICT (owner, name) DO NOTHING`
	if _, err := q.ExecContext(ctx, insertSystem, owner); err != nil {
		return fmt.Errorf("store: create system folders for %s: %w", owner, err)
	}
	return nil
}

// Rename moves a mailbox to a new name.
//
// Renaming INBOX is refused: INBOX is not a label, it is the address every
// client and every piece of stored state refers to, and renaming it would break
// the delivery path as well as every client that has it selected.
func (r *MailboxRepo) RenameChecked(ctx context.Context, q store.Querier, id int64, newName string) error {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return fmt.Errorf("store: folder name is empty")
	}
	if strings.ContainsAny(newName, "/\r\n\x00") {
		return fmt.Errorf("store: %q is not a usable folder name", newName)
	}

	var current string
	if err := q.GetContext(ctx, &current, `SELECT name FROM mailboxes WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: read mailbox before rename: %w", err)
	}
	if current == "INBOX" {
		return fmt.Errorf("store: INBOX cannot be renamed")
	}

	const update = `UPDATE mailboxes SET name = $2, updated_at = now() WHERE id = $1`
	if _, err := q.ExecContext(ctx, update, id, newName); err != nil {
		return fmt.Errorf("store: rename mailbox: %w", err)
	}
	return nil
}

// SetSpecialUse marks or clears the RFC 6154 role of a folder, which is how a
// client knows which folder is Sent rather than one the user happens to have
// named Sent.
func (r *MailboxRepo) SetSpecialUse(ctx context.Context, q store.Querier, id int64, specialUse *string) error {
	const query = `UPDATE mailboxes SET special_use = $2, updated_at = now() WHERE id = $1`
	if _, err := q.ExecContext(ctx, query, id, specialUse); err != nil {
		return fmt.Errorf("store: set special use: %w", err)
	}
	return nil
}

// Trim moves messages to the Trash folder rather than deleting them, and
// returns the number moved.
//
// Trashing is the default destructive action in a mail client because deleting
// is the one a user should have to ask for twice. The folder is created on
// demand, so a caller does not have to check for it first.
func (r *MailboxRepo) Trim(ctx context.Context, db *sqlx.DB, owner string, mailboxID int64, uids []uint32) (int, error) {
	return r.MoveToNamed(ctx, db, owner, mailboxID, uids, "Trash")
}

// MoveToNamed moves messages into a named folder, creating it if it does not
// exist, and returns how many moved.
//
// It is the one move every caller shares, because the reason a caller does not
// create the folder first is the obvious one: two requests doing that at the
// same time should not be a conflict.
func (r *MailboxRepo) MoveToNamed(ctx context.Context, db *sqlx.DB, owner string, mailboxID int64, uids []uint32, name string) (int, error) {
	if len(uids) == 0 {
		return 0, nil
	}

	target, err := r.ensureNamed(ctx, db, owner, name)
	if err != nil {
		return 0, err
	}
	moved, err := new(MessageRepo).MoveTo(ctx, db, mailboxID, uids, target)
	if err != nil {
		return 0, err
	}
	return len(moved), nil
}

// ensureNamed returns the id of a folder, creating it with its declared
// special use when it does not exist yet.
func (r *MailboxRepo) ensureNamed(ctx context.Context, db *sqlx.DB, owner, name string) (int64, error) {
	mb, err := r.GetByName(ctx, db, owner, name)
	if err == nil {
		return mb.ID, nil
	}
	if !store.IsNotFound(err) {
		return 0, err
	}

	var specialUse *string
	if decl, ok := systemFolderByName(name); ok {
		specialUse = &decl
	}

	var id int64
	const insert = `
		INSERT INTO mailboxes (owner, name, uidvalidity, uidnext, special_use, subscribed)
		VALUES ($1, $2, 1, 1, $3, true)
		ON CONFLICT (owner, name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`
	if err := db.GetContext(ctx, &id, insert, owner, name, specialUse); err != nil {
		return 0, fmt.Errorf("store: create folder %q: %w", name, err)
	}
	return id, nil
}

// systemFolderByName looks a folder up in the declared set, so that a folder
// created on demand gets the same special use as one from provisioning.
func systemFolderByName(name string) (string, bool) {
	for _, f := range SystemFolders {
		if f.Name == name {
			return f.SpecialUse, true
		}
	}
	return "", false
}

// folderRank is where a folder sits in the display order.
//
// The rank is a pair rather than a name so that INBOX and the standard folders
// sort by declaration instead of by alphabet, while user folders fall back to
// their name. The name is folded to lower case so "receipts" and "Receipts" sit
// next to each other instead of in whatever order the collation produces.
type folderRank struct {
	rank int
	name string
}

func rankOf(m Mailbox) folderRank {
	if m.Name == "INBOX" {
		return folderRank{rank: 0}
	}
	for i, f := range SystemFolders {
		if f.Name == m.Name {
			return folderRank{rank: i + 1}
		}
	}
	return folderRank{rank: len(SystemFolders) + 1, name: strings.ToLower(m.Name)}
}

// SortFolders orders a folder list for display.
//
// The sort is stable so that two folders whose names differ only by case, or
// two that we cannot rank at all, keep the order the database returned them in
// rather than shuffling between requests and moving under the cursor.
func SortFolders(folders []Mailbox) {
	sort.SliceStable(folders, func(i, j int) bool {
		a, b := rankOf(folders[i]), rankOf(folders[j])
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.name < b.name
	})
}

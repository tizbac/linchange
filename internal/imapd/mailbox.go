package imapd

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// Mailbox holds mailbox metadata for IMAP.
type Mailbox struct {
	ID           int64     `db:"id"`
	Owner        string    `db:"owner"`
	Name         string    `db:"name"`
	ParentID     *int64    `db:"parent_id"`
	UIDValidity  uint32    `db:"uidvalidity"`
	UIDNext      uint32    `db:"uidnext"`
	SpecialUse   *string   `db:"special_use"`
	Subscribed   bool      `db:"subscribed"`
	CreatedBy    *string   `db:"created_by"`
}

// MailboxRepo handles mailbox CRUD operations.
type MailboxRepo struct{}

// NewMailboxRepo returns a mailbox repository.
func NewMailboxRepo() *MailboxRepo {
	return &MailboxRepo{}
}

// ListForUser returns all mailboxes for a user.
func (r *MailboxRepo) ListForUser(ctx context.Context, db *sqlx.DB, owner string) ([]Mailbox, error) {
	const query = `
		SELECT id, owner, name, parent_id, uidvalidity, uidnext,
		       special_use, subscribed, created_by
		FROM mailboxes
		WHERE owner = $1
		ORDER BY name`

	var mailboxes []Mailbox
	if err := db.SelectContext(ctx, &mailboxes, query, owner); err != nil {
		return nil, fmt.Errorf("store: list mailboxes: %w", err)
	}

	return mailboxes, nil
}

// GetByName returns a mailbox by owner and name.
func (r *MailboxRepo) GetByName(ctx context.Context, db *sqlx.DB, owner, name string) (Mailbox, error) {
	const query = `
		SELECT id, owner, name, parent_id, uidvalidity, uidnext,
		       special_use, subscribed, created_by
		FROM mailboxes
		WHERE owner = $1 AND name = $2`

	var mb Mailbox
	if err := db.GetContext(ctx, &mb, query, owner, name); err != nil {
		return Mailbox{}, fmt.Errorf("store: get mailbox: %w", err)
	}

	return mb, nil
}

// Create creates a new mailbox.
func (r *MailboxRepo) Create(ctx context.Context, db *sqlx.DB, owner, name string, parentID *int64) error {
	const query = `
		INSERT INTO mailboxes (owner, name, parent_id, uidvalidity, uidnext, special_use, subscribed)
		VALUES ($1, $2, $3, 
		        EXTRACT(EPOCH FROM NOW())::bigint, 1, NULL, TRUE)
		RETURNING id`

	var id int64
	return db.GetContext(ctx, &id, query, owner, name, parentID)
}

// Delete removes a mailbox.
func (r *MailboxRepo) Delete(ctx context.Context, db *sqlx.DB, id int64) error {
	const query = `DELETE FROM mailboxes WHERE id = $1`
	result, err := db.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("store: delete mailbox: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("store: mailbox not found")
	}

	return nil
}

// Rename changes a mailbox name.
func (r *MailboxRepo) Rename(ctx context.Context, db *sqlx.DB, id int64, newName string) error {
	const query = `UPDATE mailboxes SET name = $2, updated_at = now() WHERE id = $1`
	result, err := db.ExecContext(ctx, query, id, newName)
	if err != nil {
		return fmt.Errorf("store: rename mailbox: %w", err)
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("store: mailbox not found")
	}

	return nil
}

// SetSubscribed updates the subscribed flag.
func (r *MailboxRepo) SetSubscribed(ctx context.Context, db *sqlx.DB, id int64, subscribed bool) error {
	const query = `UPDATE mailboxes SET subscribed = $2, updated_at = now() WHERE id = $1`
	_, err := db.ExecContext(ctx, query, id, subscribed)
	return err
}

// SetUIDNext updates the uidnext value.
func (r *MailboxRepo) SetUIDNext(ctx context.Context, db *sqlx.DB, id int64, uidNext uint32) error {
	const query = `UPDATE mailboxes SET uidnext = $2, updated_at = now() WHERE id = $1`
	_, err := db.ExecContext(ctx, query, id, uidNext)
	return err
}

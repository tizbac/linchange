package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// Address book scopes.
const (
	AddressBookPersonal     = "personal"
	AddressBookShared       = "shared"
	AddressBookOrganization = "organization"
)

// Address book share levels. Write implies read.
const (
	AddressBookShareRead  = "read"
	AddressBookShareWrite = "write"
)

// Share statuses, mirroring calendar shares: a person's offer is pending until
// the subject accepts it; an organization book needs no offer at all.
const (
	AddressBookSharePending = "pending"
	AddressBookShareActive  = "active"
)

// AddressBook is one collection of contacts.
type AddressBook struct {
	ID          int64     `db:"id" json:"id"`
	Owner       string    `db:"owner" json:"owner"`
	WorkgroupID *int64    `db:"workgroup_id" json:"workgroupId,omitempty"`
	Scope       string    `db:"scope" json:"scope"`
	Name        string    `db:"name" json:"name"`
	Slug        string    `db:"slug" json:"slug"`
	Description string    `db:"description" json:"description"`
	CreatedAt   time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt   time.Time `db:"updated_at" json:"updatedAt"`
}

// AddressBookAccess is a book together with what the caller may do with it.
type AddressBookAccess struct {
	AddressBook
	Level   string `db:"level" json:"level"`
	IsOwner bool   `db:"is_owner" json:"isOwner"`
}

// AddressContact is one card, stored as its vCard plus a searchable projection.
type AddressContact struct {
	ID           int64     `db:"id" json:"id"`
	BookID       int64     `db:"book_id" json:"bookId"`
	UID          string    `db:"uid" json:"uid"`
	VCard        string    `db:"vcard" json:"vcard"`
	DisplayName  string    `db:"display_name" json:"displayName"`
	Email        string    `db:"email" json:"email"`
	SearchText   string    `db:"search_text" json:"-"`
	Organization string    `db:"organization" json:"organization"`
	Title        string    `db:"title" json:"title"`
	Notes        string    `db:"notes" json:"notes"`
	CreatedAt    time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt    time.Time `db:"updated_at" json:"updatedAt"`
}

// AddressBookShare grants one subject a level of access to one book.
type AddressBookShare struct {
	ID             int64     `db:"id" json:"id"`
	BookID         int64     `db:"book_id" json:"bookId"`
	SubjectKind    string    `db:"subject_kind" json:"subjectKind"`
	SubjectAddress *string   `db:"subject_address" json:"subjectAddress,omitempty"`
	SubjectGroupID *int64    `db:"subject_group_id" json:"subjectGroupId,omitempty"`
	Level          string    `db:"level" json:"level"`
	Status         string    `db:"status" json:"status"`
	CreatedAt      time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt      time.Time `db:"updated_at" json:"updatedAt"`
}

// GroupID returns the group a group share names, or zero for a person.
func (s AddressBookShare) GroupID() int64 {
	if s.SubjectGroupID == nil {
		return 0
	}
	return *s.SubjectGroupID
}

// AddressBookShareOffer is a pending share as the person receiving it sees it:
// enough of the book to decide about it.
type AddressBookShareOffer struct {
	AddressBookShare
	BookName  string `db:"book_name" json:"bookName"`
	BookOwner string `db:"book_owner" json:"bookOwner"`
}

// AddressBooks reads and writes address books, contacts and their shares.
type AddressBooks struct{}

// NewAddressBooks returns the repository.
func NewAddressBooks() *AddressBooks { return &AddressBooks{} }

const addressBookColumns = `id, owner, workgroup_id, scope, name, slug, description,
	created_at, updated_at`

const contactColumns = `id, book_id, uid, vcard, display_name, email, search_text,
	organization, title, notes, created_at, updated_at`

const addressBookShareColumns = `id, book_id, subject_kind, subject_address,
	subject_group_id, level, status, created_at, updated_at`

// EnsurePersonal returns the user's personal book, creating it on first use.
//
// Every path that needs a place to put a contact calls this rather than
// checking whether one exists: the default book is a promise to the user, and
// the place it is created is the place it is used.
func (b *AddressBooks) EnsurePersonal(ctx context.Context, q Querier, owner string) (AddressBook, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	var out AddressBook
	err := q.QueryRowxContext(ctx, `
		INSERT INTO address_books (owner, scope, name, slug, description)
		VALUES ($1, 'personal', 'My contacts', 'personal', '')
		ON CONFLICT (owner) WHERE scope = 'personal' DO NOTHING
		RETURNING `+addressBookColumns, owner).Scan(
		&out.ID, &out.Owner, &out.WorkgroupID, &out.Scope, &out.Name, &out.Slug,
		&out.Description, &out.CreatedAt, &out.UpdatedAt)
	if err == nil {
		return out, nil
	}
	if err != sql.ErrNoRows {
		return AddressBook{}, fmt.Errorf("store: ensuring a personal address book: %w", err)
	}
	// The row does not come back from DO NOTHING, so read the one that was
	// already there.
	if err := q.GetContext(ctx, &out, `
		SELECT `+addressBookColumns+`
		FROM address_books WHERE owner = $1 AND scope = 'personal'`, owner); err != nil {
		return AddressBook{}, fmt.Errorf("store: reading the personal address book: %w", err)
	}
	return out, nil
}

// EnsureOrganization returns the organization's directory, creating it on
// first use. There is one per workgroup; the owner is the administrator who
// created it, and every member of the organization may read it.
func (b *AddressBooks) EnsureOrganization(ctx context.Context, q Querier, owner string, workgroupID int64, name, slug string) (AddressBook, error) {
	var out AddressBook
	err := q.QueryRowxContext(ctx, `
		INSERT INTO address_books (owner, workgroup_id, scope, name, slug, description)
		VALUES ($1, $2, 'organization', $3, $4, '')
		ON CONFLICT (workgroup_id) WHERE scope = 'organization' DO NOTHING
		RETURNING `+addressBookColumns,
		strings.ToLower(strings.TrimSpace(owner)), workgroupID, strings.TrimSpace(name), strings.TrimSpace(slug),
	).Scan(&out.ID, &out.Owner, &out.WorkgroupID, &out.Scope, &out.Name, &out.Slug,
		&out.Description, &out.CreatedAt, &out.UpdatedAt)
	if err == nil {
		return out, nil
	}
	if err != sql.ErrNoRows {
		return AddressBook{}, fmt.Errorf("store: ensuring the organization address book: %w", err)
	}
	if err := q.GetContext(ctx, &out, `
		SELECT `+addressBookColumns+`
		FROM address_books WHERE workgroup_id = $1 AND scope = 'organization'`,
		workgroupID); err != nil {
		return AddressBook{}, fmt.Errorf("store: reading the organization address book: %w", err)
	}
	return out, nil
}

// Get returns one book by id.
func (b *AddressBooks) Get(ctx context.Context, q Querier, id int64) (AddressBook, error) {
	var out AddressBook
	if err := q.GetContext(ctx, &out, `
		SELECT `+addressBookColumns+` FROM address_books WHERE id = $1`, id); err != nil {
		if IsNotFound(err) {
			return AddressBook{}, ErrNotFound
		}
		return AddressBook{}, fmt.Errorf("store: reading an address book: %w", err)
	}
	return out, nil
}

// Create adds a shared or organization book. A personal book is created by
// EnsurePersonal, so this refuses to make a second one.
func (b *AddressBooks) Create(ctx context.Context, q Querier, book AddressBook) (AddressBook, error) {
	var out AddressBook
	err := q.QueryRowxContext(ctx, `
		INSERT INTO address_books (owner, workgroup_id, scope, name, slug, description)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+addressBookColumns,
		strings.ToLower(strings.TrimSpace(book.Owner)), book.WorkgroupID, book.Scope,
		strings.TrimSpace(book.Name), strings.TrimSpace(book.Slug),
		strings.TrimSpace(book.Description),
	).Scan(&out.ID, &out.Owner, &out.WorkgroupID, &out.Scope, &out.Name, &out.Slug,
		&out.Description, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		if IsConflict(err) {
			return AddressBook{}, ErrConflict
		}
		return AddressBook{}, fmt.Errorf("store: creating an address book: %w", err)
	}
	return out, nil
}

// Update changes the name and description of a book.
func (b *AddressBooks) Update(ctx context.Context, q Querier, id int64, name, description string) (AddressBook, error) {
	var out AddressBook
	err := q.QueryRowxContext(ctx, `
		UPDATE address_books
		SET name = $2, description = $3, updated_at = now()
		WHERE id = $1
		RETURNING `+addressBookColumns,
		id, strings.TrimSpace(name), strings.TrimSpace(description),
	).Scan(&out.ID, &out.Owner, &out.WorkgroupID, &out.Scope, &out.Name, &out.Slug,
		&out.Description, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		if IsNotFound(err) {
			return AddressBook{}, ErrNotFound
		}
		return AddressBook{}, fmt.Errorf("store: updating an address book: %w", err)
	}
	return out, nil
}

// Delete removes a book. The personal book is refused: a user always has
// somewhere to keep a contact.
func (b *AddressBooks) Delete(ctx context.Context, q Querier, id int64) error {
	res, err := q.ExecContext(ctx,
		`DELETE FROM address_books WHERE id = $1 AND scope <> 'personal'`, id)
	if err != nil {
		return fmt.Errorf("store: deleting an address book: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListOwned returns the books a person owns, personal first.
func (b *AddressBooks) ListOwned(ctx context.Context, q Querier, owner string) ([]AddressBook, error) {
	var out []AddressBook
	err := q.SelectContext(ctx, &out, `
		SELECT `+addressBookColumns+`
		FROM address_books
		WHERE owner = $1
		ORDER BY CASE scope WHEN 'personal' THEN 0 WHEN 'organization' THEN 1 ELSE 2 END, name`,
		strings.ToLower(strings.TrimSpace(owner)))
	if err != nil {
		return nil, fmt.Errorf("store: listing owned address books: %w", err)
	}
	return out, nil
}

// Accessible returns every book the address may use, with the level it holds.
//
// Sharing is read from the offer and its decision, never from the status alone:
// a pending offer that the subject declined grants nothing, and a group offer is
// answered person by person.
func (b *AddressBooks) Accessible(ctx context.Context, q Querier, address string) ([]AddressBookAccess, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	books := make(map[int64]AddressBookAccess)

	owned, err := b.ListOwned(ctx, q, address)
	if err != nil {
		return nil, err
	}
	for _, book := range owned {
		books[book.ID] = AddressBookAccess{AddressBook: book, Level: AddressBookShareWrite, IsOwner: true}
	}

	shared, err := b.sharedBooks(ctx, q, address)
	if err != nil {
		return nil, err
	}
	for _, row := range shared {
		if current, ok := books[row.AddressBook.ID]; !ok || rankBookLevel(row.Level) > rankBookLevel(current.Level) {
			books[row.AddressBook.ID] = row
		}
	}

	org, err := b.organizationBooks(ctx, q, address)
	if err != nil {
		return nil, err
	}
	for _, row := range org {
		if _, ok := books[row.AddressBook.ID]; !ok {
			books[row.AddressBook.ID] = row
		}
	}

	out := make([]AddressBookAccess, 0, len(books))
	for _, book := range books {
		out = append(out, book)
	}
	// Stable order for the client: personal, organization, then shared, by name.
	sortBookAccess(out)
	return out, nil
}

// sharedBooks returns books shared with the address, with the strongest level
// any granted share gives.
func (b *AddressBooks) sharedBooks(ctx context.Context, q Querier, address string) ([]AddressBookAccess, error) {
	rows := []struct {
		AddressBook
		Level string `db:"level"`
	}{}
	err := q.SelectContext(ctx, &rows, `
		SELECT b.id, b.owner, b.workgroup_id, b.scope, b.name, b.slug, b.description,
		       b.created_at, b.updated_at,
		       CASE WHEN max(CASE s.level WHEN 'write' THEN 2 ELSE 1 END) = 2
		            THEN 'write' ELSE 'read' END AS level
		FROM address_books b
		JOIN address_book_shares s ON s.book_id = b.id
		WHERE b.scope = 'shared'
		  AND (s.status = 'active' OR EXISTS (
		      SELECT 1 FROM address_book_share_decisions d
		      WHERE d.share_id = s.id AND d.address = $1::citext AND d.decision = 'accepted'))
		  AND (
		      (s.subject_kind = 'user' AND s.subject_address = $1::citext)
		      OR (s.subject_kind = 'group' AND EXISTS (
		          SELECT 1 FROM workgroup_group_members m
		          WHERE m.group_id = s.subject_group_id AND m.address = $1::citext))
		  )
		GROUP BY b.id`, address)
	if err != nil {
		return nil, fmt.Errorf("store: listing shared address books: %w", err)
	}
	out := make([]AddressBookAccess, 0, len(rows))
	for _, row := range rows {
		out = append(out, AddressBookAccess{AddressBook: row.AddressBook, Level: row.Level})
	}
	return out, nil
}

// organizationBooks returns the organization books the address can read.
func (b *AddressBooks) organizationBooks(ctx context.Context, q Querier, address string) ([]AddressBookAccess, error) {
	rows := []AddressBook{}
	err := q.SelectContext(ctx, &rows, `
		SELECT `+addressBookColumns+`
		FROM address_books
		WHERE scope = 'organization' AND workgroup_id IN (
		    SELECT w.id FROM workgroups w
		    WHERE lower(w.domain) = lower(split_part($1::text, '@', 2))
		       OR EXISTS (
		           SELECT 1 FROM domains d
		           WHERE d.workgroup_id = w.id
		             AND lower(d.name) = lower(split_part($1::text, '@', 2)))
		       OR EXISTS (
		           SELECT 1 FROM workgroup_members m
		           WHERE m.workgroup_id = w.id AND m.address = $1::citext)
		)
		ORDER BY name`, address)
	if err != nil {
		return nil, fmt.Errorf("store: listing organization address books: %w", err)
	}
	out := make([]AddressBookAccess, 0, len(rows))
	for _, book := range rows {
		out = append(out, AddressBookAccess{AddressBook: book, Level: AddressBookShareRead})
	}
	return out, nil
}

// Access returns the caller's level on one book. The bool is false when the
// book exists but is not the caller's to use; a missing book is ErrNotFound.
func (b *AddressBooks) Access(ctx context.Context, q Querier, bookID int64, address string) (AddressBookAccess, bool, error) {
	book, err := b.Get(ctx, q, bookID)
	if err != nil {
		return AddressBookAccess{}, false, err
	}
	address = strings.ToLower(strings.TrimSpace(address))

	if strings.EqualFold(book.Owner, address) {
		return AddressBookAccess{AddressBook: book, Level: AddressBookShareWrite, IsOwner: true}, true, nil
	}
	switch book.Scope {
	case AddressBookPersonal:
		return AddressBookAccess{AddressBook: book}, false, nil
	case AddressBookOrganization:
		member, err := b.isOrganizationMember(ctx, q, book, address)
		if err != nil {
			return AddressBookAccess{}, false, err
		}
		if member {
			admin, err := b.isDomainAdmin(ctx, q, address)
			if err != nil {
				return AddressBookAccess{}, false, err
			}
			if admin {
				return AddressBookAccess{AddressBook: book, Level: AddressBookShareWrite}, true, nil
			}
			return AddressBookAccess{AddressBook: book, Level: AddressBookShareRead}, true, nil
		}
		return AddressBookAccess{AddressBook: book}, false, nil
	case AddressBookShared:
		level, err := b.ShareLevel(ctx, q, bookID, address)
		if err != nil {
			return AddressBookAccess{}, false, err
		}
		if level == "" {
			return AddressBookAccess{AddressBook: book}, false, nil
		}
		return AddressBookAccess{AddressBook: book, Level: level}, true, nil
	default:
		return AddressBookAccess{AddressBook: book}, false, nil
	}
}

// isOrganizationMember reports whether the address is in the organization's
// domains, which is the same rule the rest of the organization uses.
func (b *AddressBooks) isOrganizationMember(ctx context.Context, q Querier, book AddressBook, address string) (bool, error) {
	if book.WorkgroupID == nil {
		return false, nil
	}
	var member bool
	err := q.GetContext(ctx, &member, `
		SELECT EXISTS (
		    SELECT 1 FROM workgroups w
		    WHERE w.id = $1
		      AND (lower(w.domain) = lower(split_part($2::text, '@', 2))
		           OR EXISTS (
		               SELECT 1 FROM domains d
		               WHERE d.workgroup_id = w.id
		                 AND lower(d.name) = lower(split_part($2::text, '@', 2))))
		)`, *book.WorkgroupID, address)
	if err != nil {
		return false, fmt.Errorf("store: checking organization membership: %w", err)
	}
	return member, nil
}

// isDomainAdmin reports whether the address administers its domain. An
// organization's directory is maintained by its administrators, so they may
// write it; everyone else in the organization reads it.
func (b *AddressBooks) isDomainAdmin(ctx context.Context, q Querier, address string) (bool, error) {
	var admin bool
	err := q.GetContext(ctx, &admin,
		`SELECT COALESCE(is_domain_admin, false) FROM users WHERE address = $1`,
		strings.ToLower(strings.TrimSpace(address)))
	if IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: checking the domain administrator: %w", err)
	}
	return admin, nil
}

// ShareLevel returns the level a person holds on a book through its shares,
// or "" when no granted share names them.
func (b *AddressBooks) ShareLevel(ctx context.Context, q Querier, bookID int64, address string) (string, error) {
	var level sql.NullString
	err := q.GetContext(ctx, &level, `
		SELECT CASE WHEN max(CASE s.level WHEN 'write' THEN 2 ELSE 1 END) = 2
		            THEN 'write' ELSE 'read' END
		FROM address_book_shares s
		WHERE s.book_id = $1
		  AND (s.status = 'active' OR EXISTS (
		      SELECT 1 FROM address_book_share_decisions d
		      WHERE d.share_id = s.id AND d.address = $2::citext AND d.decision = 'accepted'))
		  AND (
		      (s.subject_kind = 'user' AND s.subject_address = $2::citext)
		      OR (s.subject_kind = 'group' AND EXISTS (
		          SELECT 1 FROM workgroup_group_members m
		          WHERE m.group_id = s.subject_group_id AND m.address = $2::citext))
		  )`, bookID, strings.ToLower(strings.TrimSpace(address)))
	if err != nil {
		return "", fmt.Errorf("store: reading address book access: %w", err)
	}
	return level.String, nil
}

// ListShares returns the shares on one book.
func (b *AddressBooks) ListShares(ctx context.Context, q Querier, bookID int64) ([]AddressBookShare, error) {
	var out []AddressBookShare
	err := q.SelectContext(ctx, &out, `
		SELECT `+addressBookShareColumns+`
		FROM address_book_shares WHERE book_id = $1
		ORDER BY subject_kind, COALESCE(subject_address::text, subject_group_id::text)`, bookID)
	if err != nil {
		return nil, fmt.Errorf("store: listing address book shares: %w", err)
	}
	return out, nil
}

// PutShare adds a share or changes its level. Re-sharing replaces the old
// decision, because what somebody accepted was the offer made then.
func (b *AddressBooks) PutShare(ctx context.Context, q Querier, share AddressBookShare) (AddressBookShare, error) {
	level, err := validAddressBookLevel(share.Level)
	if err != nil {
		return AddressBookShare{}, err
	}
	status := share.Status
	if status == "" {
		status = AddressBookSharePending
	}
	if status != AddressBookSharePending && status != AddressBookShareActive {
		return AddressBookShare{}, fmt.Errorf("store: unknown address book share status %q", status)
	}

	const upsert = `
		WITH upserted AS (
			INSERT INTO address_book_shares
			    (book_id, subject_kind, subject_address, subject_group_id, level, status)
			VALUES (%s)
			ON CONFLICT %s
			DO UPDATE SET level = EXCLUDED.level, status = EXCLUDED.status, updated_at = now()
			RETURNING *, (xmax = 0) AS is_new
		), reset AS (
			DELETE FROM address_book_share_decisions d
			USING upserted u
			WHERE d.share_id = u.id AND NOT u.is_new
		)
		SELECT ` + addressBookShareColumns + ` FROM upserted`

	var (
		out   AddressBookShare
		query string
		args  []any
	)
	switch share.SubjectKind {
	case ShareSubjectUser:
		if share.SubjectAddress == nil || strings.TrimSpace(*share.SubjectAddress) == "" {
			return AddressBookShare{}, fmt.Errorf("store: a user share needs an address")
		}
		query = fmt.Sprintf(upsert, "$1, $2, $3, NULL, $4, $5",
			"(book_id, subject_address) WHERE subject_kind = 'user'")
		args = []any{share.BookID, ShareSubjectUser, strings.ToLower(strings.TrimSpace(*share.SubjectAddress)), level, status}
	case ShareSubjectGroup:
		if share.SubjectGroupID == nil || *share.SubjectGroupID == 0 {
			return AddressBookShare{}, fmt.Errorf("store: a group share needs a group id")
		}
		query = fmt.Sprintf(upsert, "$1, $2, NULL, $3, $4, $5",
			"(book_id, subject_group_id) WHERE subject_kind = 'group'")
		args = []any{share.BookID, ShareSubjectGroup, *share.SubjectGroupID, level, status}
	default:
		return AddressBookShare{}, fmt.Errorf("store: unknown share subject kind %q", share.SubjectKind)
	}

	if err := q.GetContext(ctx, &out, query, args...); err != nil {
		if IsConflict(err) {
			return AddressBookShare{}, ErrConflict
		}
		return AddressBookShare{}, fmt.Errorf("store: sharing an address book: %w", err)
	}
	return out, nil
}

// DeleteShare removes a share by id.
func (b *AddressBooks) DeleteShare(ctx context.Context, q Querier, id int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM address_book_shares WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: deleting an address book share: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Offers returns the pending shares waiting for the address to answer.
func (b *AddressBooks) Offers(ctx context.Context, q Querier, address string) ([]AddressBookShareOffer, error) {
	var out []AddressBookShareOffer
	err := q.SelectContext(ctx, &out, `
		SELECT s.id, s.book_id, s.subject_kind, s.subject_address, s.subject_group_id,
		       s.level, s.status, s.created_at, s.updated_at,
		       b.name AS book_name, b.owner AS book_owner
		FROM address_book_shares s
		JOIN address_books b ON b.id = s.book_id
		WHERE b.scope = 'shared' AND s.status = 'pending'
		  AND (
		      (s.subject_kind = 'user' AND s.subject_address = $1::citext)
		      OR (s.subject_kind = 'group' AND EXISTS (
		          SELECT 1 FROM workgroup_group_members m
		          WHERE m.group_id = s.subject_group_id AND m.address = $1::citext))
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM address_book_share_decisions d
		      WHERE d.share_id = s.id AND d.address = $1::citext)
		ORDER BY s.created_at DESC`, strings.ToLower(strings.TrimSpace(address)))
	if err != nil {
		return nil, fmt.Errorf("store: listing address book offers: %w", err)
	}
	return out, nil
}

// Decide records one person's answer to an offer. A group offer is answered by
// each member separately; this is that answer.
func (b *AddressBooks) Decide(ctx context.Context, q Querier, shareID int64, address, decision string) (AddressBookShare, error) {
	if decision != "accepted" && decision != "declined" {
		return AddressBookShare{}, fmt.Errorf("store: a share decision is accepted or declined")
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO address_book_share_decisions (share_id, address, decision)
		VALUES ($1, $2, $3)
		ON CONFLICT (share_id, address)
		DO UPDATE SET decision = EXCLUDED.decision, decided_at = now()`,
		shareID, strings.ToLower(strings.TrimSpace(address)), decision); err != nil {
		return AddressBookShare{}, fmt.Errorf("store: recording a share decision: %w", err)
	}
	var out AddressBookShare
	if err := q.GetContext(ctx, &out, `
		SELECT `+addressBookShareColumns+` FROM address_book_shares WHERE id = $1`, shareID); err != nil {
		if IsNotFound(err) {
			return AddressBookShare{}, ErrNotFound
		}
		return AddressBookShare{}, fmt.Errorf("store: reading the answered share: %w", err)
	}
	return out, nil
}

// ListContacts returns a book's contacts, by display name.
func (b *AddressBooks) ListContacts(ctx context.Context, q Querier, bookID int64) ([]AddressContact, error) {
	var out []AddressContact
	err := q.SelectContext(ctx, &out, `
		SELECT `+contactColumns+` FROM contacts
		WHERE book_id = $1
		ORDER BY COALESCE(NULLIF(display_name, ''), email), id`, bookID)
	if err != nil {
		return nil, fmt.Errorf("store: listing contacts: %w", err)
	}
	return out, nil
}

// CountContacts returns how many cards a book holds, without loading them.
func (b *AddressBooks) CountContacts(ctx context.Context, q Querier, bookID int64) (int, error) {
	var n int
	if err := q.GetContext(ctx, &n, `SELECT count(*) FROM contacts WHERE book_id = $1`, bookID); err != nil {
		return 0, fmt.Errorf("store: counting contacts: %w", err)
	}
	return n, nil
}

// GetContact returns one contact in a book.
func (b *AddressBooks) GetContact(ctx context.Context, q Querier, bookID, id int64) (AddressContact, error) {
	var out AddressContact
	if err := q.GetContext(ctx, &out, `
		SELECT `+contactColumns+` FROM contacts WHERE book_id = $1 AND id = $2`,
		bookID, id); err != nil {
		if IsNotFound(err) {
			return AddressContact{}, ErrNotFound
		}
		return AddressContact{}, fmt.Errorf("store: reading a contact: %w", err)
	}
	return out, nil
}

// GetContactByUID returns one contact by its vCard UID.
func (b *AddressBooks) GetContactByUID(ctx context.Context, q Querier, bookID int64, uid string) (AddressContact, error) {
	var out AddressContact
	if err := q.GetContext(ctx, &out, `
		SELECT `+contactColumns+` FROM contacts WHERE book_id = $1 AND uid = $2`,
		bookID, uid); err != nil {
		if IsNotFound(err) {
			return AddressContact{}, ErrNotFound
		}
		return AddressContact{}, fmt.Errorf("store: reading a contact by uid: %w", err)
	}
	return out, nil
}

// FindContactByEmail returns the first contact in a book with that address.
func (b *AddressBooks) FindContactByEmail(ctx context.Context, q Querier, bookID int64, email string) (AddressContact, error) {
	var out AddressContact
	if err := q.GetContext(ctx, &out, `
		SELECT `+contactColumns+` FROM contacts
		WHERE book_id = $1 AND lower(email) = lower($2)
		ORDER BY id LIMIT 1`, bookID, strings.TrimSpace(email)); err != nil {
		if IsNotFound(err) {
			return AddressContact{}, ErrNotFound
		}
		return AddressContact{}, fmt.Errorf("store: finding a contact by address: %w", err)
	}
	return out, nil
}

// CreateContact stores a card.
func (b *AddressBooks) CreateContact(ctx context.Context, q Querier, c AddressContact) (AddressContact, error) {
	var out AddressContact
	err := q.QueryRowxContext(ctx, `
		INSERT INTO contacts (book_id, uid, vcard, display_name, email, search_text,
		                      organization, title, notes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+contactColumns,
		c.BookID, c.UID, c.VCard, c.DisplayName, strings.ToLower(strings.TrimSpace(c.Email)),
		c.SearchText, c.Organization, c.Title, c.Notes,
	).Scan(&out.ID, &out.BookID, &out.UID, &out.VCard, &out.DisplayName, &out.Email,
		&out.SearchText, &out.Organization, &out.Title, &out.Notes,
		&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		if IsConflict(err) {
			return AddressContact{}, ErrConflict
		}
		return AddressContact{}, fmt.Errorf("store: creating a contact: %w", err)
	}
	return out, nil
}

// UpdateContact replaces a card's contents.
func (b *AddressBooks) UpdateContact(ctx context.Context, q Querier, c AddressContact) (AddressContact, error) {
	var out AddressContact
	err := q.QueryRowxContext(ctx, `
		UPDATE contacts
		SET uid = $2, vcard = $3, display_name = $4, email = $5, search_text = $6,
		    organization = $7, title = $8, notes = $9, updated_at = now()
		WHERE book_id = $1 AND id = $10
		RETURNING `+contactColumns,
		c.BookID, c.UID, c.VCard, c.DisplayName, strings.ToLower(strings.TrimSpace(c.Email)),
		c.SearchText, c.Organization, c.Title, c.Notes, c.ID,
	).Scan(&out.ID, &out.BookID, &out.UID, &out.VCard, &out.DisplayName, &out.Email,
		&out.SearchText, &out.Organization, &out.Title, &out.Notes,
		&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		if IsNotFound(err) {
			return AddressContact{}, ErrNotFound
		}
		return AddressContact{}, fmt.Errorf("store: updating a contact: %w", err)
	}
	return out, nil
}

// DeleteContact removes a card.
func (b *AddressBooks) DeleteContact(ctx context.Context, q Querier, bookID, id int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM contacts WHERE book_id = $1 AND id = $2`, bookID, id)
	if err != nil {
		return fmt.Errorf("store: deleting a contact: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Search finds contacts in the given books whose name, addresses, phones or
// organization contain the query. An empty query returns the most recent.
func (b *AddressBooks) Search(ctx context.Context, q Querier, bookIDs []int64, query string, limit int) ([]AddressContact, error) {
	if len(bookIDs) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	query = strings.ToLower(strings.TrimSpace(query))
	sqlText, args, err := sqlx.In(`
		SELECT `+contactColumns+`
		FROM contacts
		WHERE book_id IN (?)`, bookIDs)
	if err != nil {
		return nil, fmt.Errorf("store: building the contact search: %w", err)
	}
	if query != "" {
		sqlText += ` AND (lower(search_text) LIKE ? ESCAPE '\' OR lower(email) LIKE ? ESCAPE '\')`
		like := "%" + escapeLike(query) + "%"
		args = append(args, like, like)
	}
	sqlText += ` ORDER BY COALESCE(NULLIF(display_name, ''), email), id LIMIT ?`
	args = append(args, limit)

	var out []AddressContact
	if err := q.SelectContext(ctx, &out, sqlx.Rebind(sqlx.DOLLAR, sqlText), args...); err != nil {
		return nil, fmt.Errorf("store: searching contacts: %w", err)
	}
	return out, nil
}

// escapeLike makes a user's query literal inside a LIKE pattern.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func validAddressBookLevel(level string) (string, error) {
	switch level {
	case AddressBookShareRead, AddressBookShareWrite:
		return level, nil
	case "":
		return AddressBookShareRead, nil
	default:
		return "", fmt.Errorf("store: unknown address book share level %q", level)
	}
}

func rankBookLevel(level string) int {
	if level == AddressBookShareWrite {
		return 2
	}
	if level == AddressBookShareRead {
		return 1
	}
	return 0
}

func sortBookAccess(books []AddressBookAccess) {
	scopeRank := func(scope string) int {
		switch scope {
		case AddressBookPersonal:
			return 0
		case AddressBookOrganization:
			return 1
		default:
			return 2
		}
	}
	// Insertion sort: the list is a handful of books, and this avoids pulling
	// in sort.Slice with a comparator that is longer than the list.
	for i := 1; i < len(books); i++ {
		for j := i; j > 0; j-- {
			left, right := books[j-1], books[j]
			if scopeRank(left.Scope) < scopeRank(right.Scope) ||
				(scopeRank(left.Scope) == scopeRank(right.Scope) &&
					strings.ToLower(left.Name) <= strings.ToLower(right.Name)) {
				break
			}
			books[j-1], books[j] = books[j], books[j-1]
		}
	}
}

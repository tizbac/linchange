// Package contacts owns address books and the cards in them.
//
// The card is stored as a vCard, because that is what a CardDAV client reads
// and writes; the columns beside it are a searchable projection, rebuilt from
// the card whenever it changes. One representation cannot drift from the other
// when there is only one representation, and the projection is derived.
//
// The package also answers the two questions the mail path asks: who is this
// address, for autocomplete, and what is their name, for a contact that was
// added by writing to them before anybody knew it.
package contacts

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-vcard"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/store"
)

// ErrInvalid is a request the caller can fix: a book with no name, a card with
// no identity.
var ErrInvalid = errors.New("contacts: invalid request")

// ErrDenied is a card or book the caller may read but not change.
var ErrDenied = errors.New("contacts: denied")

// ErrNotFound is a missing book, card or offer.
var ErrNotFound = errors.New("contacts: not found")

const (
	maxBookName = 120
	maxCardSize = 64 * 1024
)

// Email is one address on a card.
type Email struct {
	Type  string `json:"type,omitempty"`
	Value string `json:"value"`
}

// Phone is one telephone number on a card.
type Phone struct {
	Type  string `json:"type,omitempty"`
	Value string `json:"value"`
}

// Postal is one postal address on a card.
type Postal struct {
	Type       string `json:"type,omitempty"`
	Street     string `json:"street,omitempty"`
	City       string `json:"city,omitempty"`
	Region     string `json:"region,omitempty"`
	PostalCode string `json:"postalCode,omitempty"`
	Country    string `json:"country,omitempty"`
}

// Input is a card as the web form describes it.
type Input struct {
	DisplayName  string   `json:"displayName"`
	FirstName    string   `json:"firstName,omitempty"`
	LastName     string   `json:"lastName,omitempty"`
	Emails       []Email  `json:"emails,omitempty"`
	Phones       []Phone  `json:"phones,omitempty"`
	Organization string   `json:"organization,omitempty"`
	Department   string   `json:"department,omitempty"`
	Title        string   `json:"title,omitempty"`
	Addresses    []Postal `json:"addresses,omitempty"`
	Website      string   `json:"website,omitempty"`
	Notes        string   `json:"notes,omitempty"`
	Birthday     string   `json:"birthday,omitempty"`
}

// Entry is a card as the web client reads it.
type Entry struct {
	ID           int64     `json:"id"`
	BookID       int64     `json:"bookId"`
	UID          string    `json:"uid"`
	DisplayName  string    `json:"displayName"`
	FirstName    string    `json:"firstName,omitempty"`
	LastName     string    `json:"lastName,omitempty"`
	Emails       []Email   `json:"emails,omitempty"`
	Phones       []Phone   `json:"phones,omitempty"`
	Organization string    `json:"organization,omitempty"`
	Department   string    `json:"department,omitempty"`
	Title        string    `json:"title,omitempty"`
	Addresses    []Postal  `json:"addresses,omitempty"`
	Website      string    `json:"website,omitempty"`
	Notes        string    `json:"notes,omitempty"`
	Birthday     string    `json:"birthday,omitempty"`
	VCard        string    `json:"vcard,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Suggestion is one autocomplete candidate, carrying the book it came from so
// the person choosing can tell their own contact from the company directory.
type Suggestion struct {
	Address     string `json:"address"`
	DisplayName string `json:"displayName,omitempty"`
	Source      string `json:"source"`
	Scope       string `json:"scope"`
	BookID      int64  `json:"bookId"`
}

// Recipient is one address a sent message was addressed to.
type Recipient struct {
	Address string
	Name    string
}

// Service owns address books and cards.
type Service struct {
	db    *sqlx.DB
	books *store.AddressBooks
	log   *slog.Logger
}

// New builds the service.
func New(db *sqlx.DB, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, books: store.NewAddressBooks(), log: log}
}

// EnsurePersonal returns the caller's personal book, creating it if needed.
func (s *Service) EnsurePersonal(ctx context.Context, owner string) (store.AddressBook, error) {
	return s.books.EnsurePersonal(ctx, s.db, owner)
}

const (
	organizationBookName = "Organization directory"
	organizationBookSlug = "directory"
)

// EnsureOrganization returns the workgroup's directory, creating it on first
// use. The first administrator to need one becomes its owner; every member of
// the organization reads it, and administrators may write it.
func (s *Service) EnsureOrganization(ctx context.Context, owner string, workgroupID int64) (store.AddressBook, error) {
	return s.books.EnsureOrganization(ctx, s.db, owner, workgroupID, organizationBookName, organizationBookSlug)
}

// UpsertOrganizationPerson puts a user in the organization directory, or
// refreshes the card that is already there.
//
// It is the write behind user creation: the directory is the answer to "who is
// in the organization", and an account that exists without a card is a person
// the directory cannot name. A phone number given at creation is kept on the
// card, because that is where the organization's address book keeps it.
func (s *Service) UpsertOrganizationPerson(ctx context.Context, actor string, workgroupID int64, address, displayName, phone string) error {
	address = strings.ToLower(strings.TrimSpace(address))
	displayName = strings.TrimSpace(displayName)
	phone = strings.TrimSpace(phone)
	book, err := s.EnsureOrganization(ctx, actor, workgroupID)
	if err != nil {
		return err
	}
	company := ""
	if wg, err := store.NewWorkgroups().Get(ctx, s.db, workgroupID); err == nil {
		company = strings.TrimSpace(wg.DisplayName)
		if company == "" {
			company = strings.TrimSpace(wg.Name)
		}
	}

	existing, findErr := s.books.FindContactByEmail(ctx, s.db, book.ID, address)
	if findErr == nil {
		// Keep the other fields somebody may have put on the card; only the
		// name, the address and the phone are the user record's to update.
		card, err := decodeCard(existing.VCard)
		if err != nil {
			return err
		}
		if displayName != "" {
			card.SetValue(vcard.FieldFormattedName, displayName)
		}
		if phone != "" {
			card.Set(vcard.FieldTelephone, &vcard.Field{
				Value:  phone,
				Params: vcard.Params{vcard.ParamType: []string{vcard.TypeWork}},
			})
		}
		if card.PreferredValue(vcard.FieldEmail) == "" {
			card.SetValue(vcard.FieldEmail, address)
		}
		if card.Value(vcard.FieldOrganization) == "" && company != "" {
			card.SetValue(vcard.FieldOrganization, company)
		}
		row, err := rowFromCard(book.ID, card)
		if err != nil {
			return err
		}
		row.ID = existing.ID
		_, err = s.books.UpdateContact(ctx, s.db, row)
		return err
	}
	if !errors.Is(findErr, store.ErrNotFound) {
		return findErr
	}

	input := Input{DisplayName: displayName, Emails: []Email{{Type: "work", Value: address}}}
	if phone != "" {
		input.Phones = []Phone{{Type: "work", Value: phone}}
	}
	if company != "" {
		input.Organization = company
	}
	card, err := buildCard(input, newUID())
	if err != nil {
		return err
	}
	row, err := rowFromCard(book.ID, card)
	if err != nil {
		return err
	}
	_, err = s.books.CreateContact(ctx, s.db, row)
	return err
}

// Books returns every book the caller can open, with the level they hold.
func (s *Service) Books(ctx context.Context, owner string) ([]store.AddressBookAccess, error) {
	if _, err := s.EnsurePersonal(ctx, owner); err != nil {
		return nil, err
	}
	return s.books.Accessible(ctx, s.db, owner)
}

// CreateBook creates a named book owned by the caller.
func (s *Service) CreateBook(ctx context.Context, owner, name, description string) (store.AddressBook, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.AddressBook{}, fmt.Errorf("%w: a book needs a name", ErrInvalid)
	}
	if len([]rune(name)) > maxBookName {
		return store.AddressBook{}, fmt.Errorf("%w: the name is too long", ErrInvalid)
	}
	base := slugify(name)
	for attempt := 1; attempt <= 50; attempt++ {
		slug := base
		if attempt > 1 {
			slug = fmt.Sprintf("%s-%d", base, attempt)
		}
		book, err := s.books.Create(ctx, s.db, store.AddressBook{
			Owner:       strings.ToLower(strings.TrimSpace(owner)),
			Scope:       store.AddressBookShared,
			Name:        name,
			Slug:        slug,
			Description: strings.TrimSpace(description),
		})
		if err == nil {
			return book, nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return store.AddressBook{}, err
		}
	}
	return store.AddressBook{}, fmt.Errorf("%w: too many books with that name", ErrInvalid)
}

// Book returns one book the caller may read, with their level.
func (s *Service) Book(ctx context.Context, owner string, id int64) (store.AddressBookAccess, error) {
	access, ok, err := s.require(ctx, owner, id, store.AddressBookShareRead)
	if err != nil {
		return store.AddressBookAccess{}, err
	}
	if !ok {
		return store.AddressBookAccess{}, ErrDenied
	}
	return access, nil
}

// CreateBookAt creates a named book with an explicit path segment, which is
// what a CardDAV MKCOL names. The web form uses CreateBook, which derives one.
func (s *Service) CreateBookAt(ctx context.Context, owner, name, slug, description string) (store.AddressBook, error) {
	name = strings.TrimSpace(name)
	slug = slugify(slug)
	if name == "" {
		name = slug
	}
	if slug == "" {
		return store.AddressBook{}, fmt.Errorf("%w: a book needs a path", ErrInvalid)
	}
	return s.books.Create(ctx, s.db, store.AddressBook{
		Owner:       strings.ToLower(strings.TrimSpace(owner)),
		Scope:       store.AddressBookShared,
		Name:        name,
		Slug:        slug,
		Description: strings.TrimSpace(description),
	})
}

// CardRows returns the stored cards of a book, for the CardDAV backend, which
// needs the vCard rather than the form fields.
func (s *Service) CardRows(ctx context.Context, owner string, bookID int64) ([]store.AddressContact, error) {
	if _, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareRead); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrDenied
	}
	return s.books.ListContacts(ctx, s.db, bookID)
}

// UpdateBook renames a book the caller owns. The personal book may be renamed
// but not removed; a named book is the owner's to change.
func (s *Service) UpdateBook(ctx context.Context, owner string, id int64, name, description string) (store.AddressBook, error) {
	access, ok, err := s.require(ctx, owner, id, store.AddressBookShareWrite)
	if err != nil {
		return store.AddressBook{}, err
	}
	if !ok || !access.IsOwner {
		return store.AddressBook{}, ErrDenied
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return store.AddressBook{}, fmt.Errorf("%w: a book needs a name", ErrInvalid)
	}
	return s.books.Update(ctx, s.db, id, name, description)
}

// DeleteBook removes a named book. The personal book cannot be deleted.
func (s *Service) DeleteBook(ctx context.Context, owner string, id int64) error {
	access, ok, err := s.require(ctx, owner, id, store.AddressBookShareWrite)
	if err != nil {
		return err
	}
	if !ok || !access.IsOwner || access.Scope == store.AddressBookPersonal {
		return ErrDenied
	}
	return s.books.Delete(ctx, s.db, id)
}

// Cards returns every card in a book.
func (s *Service) Cards(ctx context.Context, owner string, bookID int64) ([]Entry, error) {
	if _, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareRead); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrDenied
	}
	rows, err := s.books.ListContacts(ctx, s.db, bookID)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(rows))
	for _, row := range rows {
		out = append(out, toEntry(row))
	}
	return out, nil
}

// GetCard returns one card.
func (s *Service) GetCard(ctx context.Context, owner string, bookID, id int64) (Entry, error) {
	if _, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareRead); err != nil {
		return Entry{}, err
	} else if !ok {
		return Entry{}, ErrDenied
	}
	row, err := s.books.GetContact(ctx, s.db, bookID, id)
	if err != nil {
		return Entry{}, translate(err)
	}
	return toEntry(row), nil
}

// CreateCard adds a card to a book the caller may write.
func (s *Service) CreateCard(ctx context.Context, owner string, bookID int64, in Input) (Entry, error) {
	if _, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareWrite); err != nil {
		return Entry{}, err
	} else if !ok {
		return Entry{}, ErrDenied
	}
	card, err := buildCard(in, newUID())
	if err != nil {
		return Entry{}, err
	}
	row, err := rowFromCard(bookID, card)
	if err != nil {
		return Entry{}, err
	}
	created, err := s.books.CreateContact(ctx, s.db, row)
	if err != nil {
		return Entry{}, translate(err)
	}
	return toEntry(created), nil
}

// UpdateCard replaces a card's contents, keeping its UID.
func (s *Service) UpdateCard(ctx context.Context, owner string, bookID, id int64, in Input) (Entry, error) {
	if _, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareWrite); err != nil {
		return Entry{}, err
	} else if !ok {
		return Entry{}, ErrDenied
	}
	existing, err := s.books.GetContact(ctx, s.db, bookID, id)
	if err != nil {
		return Entry{}, translate(err)
	}
	card, err := buildCard(in, existing.UID)
	if err != nil {
		return Entry{}, err
	}
	row, err := rowFromCard(bookID, card)
	if err != nil {
		return Entry{}, err
	}
	row.ID = id
	updated, err := s.books.UpdateContact(ctx, s.db, row)
	if err != nil {
		return Entry{}, translate(err)
	}
	return toEntry(updated), nil
}

// DeleteCard removes a card.
func (s *Service) DeleteCard(ctx context.Context, owner string, bookID, id int64) error {
	if _, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareWrite); err != nil {
		return err
	} else if !ok {
		return ErrDenied
	}
	return translate(s.books.DeleteContact(ctx, s.db, bookID, id))
}

// PutCard stores a card that arrived over CardDAV. The UID in the card is the
// identity; an empty one is generated, which is what a client that only sent a
// path expects.
func (s *Service) PutCard(ctx context.Context, owner string, bookID int64, uid string, card vcard.Card) (Entry, error) {
	if _, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareWrite); err != nil {
		return Entry{}, err
	} else if !ok {
		return Entry{}, ErrDenied
	}
	if uid == "" {
		uid = card.Value(vcard.FieldUID)
	}
	if uid == "" {
		uid = newUID()
	}
	card.SetValue(vcard.FieldUID, uid)
	row, err := rowFromCard(bookID, card)
	if err != nil {
		return Entry{}, err
	}
	existing, err := s.books.GetContactByUID(ctx, s.db, bookID, uid)
	if err == nil {
		row.ID = existing.ID
		updated, err := s.books.UpdateContact(ctx, s.db, row)
		if err != nil {
			return Entry{}, translate(err)
		}
		return toEntry(updated), nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return Entry{}, err
	}
	created, err := s.books.CreateContact(ctx, s.db, row)
	if err != nil {
		return Entry{}, translate(err)
	}
	return toEntry(created), nil
}

// GetCardByUID returns a card by its vCard UID, for CardDAV.
func (s *Service) GetCardByUID(ctx context.Context, owner string, bookID int64, uid string) (store.AddressContact, error) {
	if _, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareRead); err != nil {
		return store.AddressContact{}, err
	} else if !ok {
		return store.AddressContact{}, ErrDenied
	}
	row, err := s.books.GetContactByUID(ctx, s.db, bookID, uid)
	if err != nil {
		return store.AddressContact{}, translate(err)
	}
	return row, nil
}

// DeleteCardByUID removes a card by its vCard UID, for CardDAV.
func (s *Service) DeleteCardByUID(ctx context.Context, owner string, bookID int64, uid string) error {
	if _, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareWrite); err != nil {
		return err
	} else if !ok {
		return ErrDenied
	}
	row, err := s.books.GetContactByUID(ctx, s.db, bookID, uid)
	if err != nil {
		return translate(err)
	}
	return translate(s.books.DeleteContact(ctx, s.db, bookID, row.ID))
}

// DirectoryEntry is one person as the GAL shows them: the fields Outlook's
// address book displays beside a name.
type DirectoryEntry struct {
	Address       string
	Name          string
	Title         string
	Company       string
	Department    string
	BusinessPhone string
	MobilePhone   string
	PrimaryPhone  string
}

// DirectoryEntries returns the organization directory as the MAPI address book
// needs it: everyone in an organization book, with the card fields that fit the
// GAL columns.
//
// It reads the cards rather than the user rows, because the directory is where
// an administrator puts a person's title and phone; a user row has no such
// fields to read.
func (s *Service) DirectoryEntries(ctx context.Context) ([]DirectoryEntry, error) {
	var rows []store.AddressContact
	err := s.db.SelectContext(ctx, &rows, `
		SELECT c.*
		FROM contacts c
		JOIN address_books b ON b.id = c.book_id
		WHERE b.scope = 'organization'
		ORDER BY c.display_name, c.email`)
	if err != nil {
		return nil, fmt.Errorf("contacts: listing the organization directory: %w", err)
	}

	out := make([]DirectoryEntry, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		address := strings.ToLower(strings.TrimSpace(row.Email))
		if address == "" || seen[address] {
			continue
		}
		seen[address] = true
		entry := toEntry(row)
		name := strings.TrimSpace(entry.DisplayName)
		if name == "" {
			name = address
		}
		item := DirectoryEntry{
			Address:    address,
			Name:       name,
			Title:      entry.Title,
			Company:    entry.Organization,
			Department: entry.Department,
		}
		for _, phone := range entry.Phones {
			if item.PrimaryPhone == "" {
				item.PrimaryPhone = phone.Value
			}
			switch strings.ToLower(phone.Type) {
			case "cell", "mobile":
				if item.MobilePhone == "" {
					item.MobilePhone = phone.Value
				}
			case "work", "voice":
				if item.BusinessPhone == "" {
					item.BusinessPhone = phone.Value
				}
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// Search returns autocomplete candidates from every book the caller can read.
//
// The same address can appear in several books; the personal one wins, because
// the user wrote that card and it is the name they expect to see. The source is
// carried through so a meeting or a message can show which book a suggestion
// came from.
func (s *Service) Search(ctx context.Context, owner, query string, limit int) ([]Suggestion, error) {
	books, err := s.Books(ctx, owner)
	if err != nil {
		return nil, err
	}
	if len(books) == 0 {
		return nil, nil
	}
	index := make(map[int64]store.AddressBookAccess, len(books))
	ids := make([]int64, 0, len(books))
	for _, book := range books {
		index[book.ID] = book
		ids = append(ids, book.ID)
	}
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := s.books.Search(ctx, s.db, ids, query, limit*4)
	if err != nil {
		return nil, err
	}

	best := make(map[string]Suggestion)
	for _, row := range rows {
		book := index[row.BookID]
		entry := toEntry(row)
		for _, email := range entry.Emails {
			address := strings.ToLower(strings.TrimSpace(email.Value))
			if address == "" {
				continue
			}
			candidate := Suggestion{
				Address:     address,
				DisplayName: entry.DisplayName,
				Source:      book.Name,
				Scope:       book.Scope,
				BookID:      book.ID,
			}
			current, ok := best[address]
			if !ok || scopePriority(candidate.Scope) < scopePriority(current.Scope) {
				best[address] = candidate
			}
		}
	}

	out := make([]Suggestion, 0, len(best))
	for _, candidate := range best {
		out = append(out, candidate)
	}
	sort.Slice(out, func(i, j int) bool {
		left := strings.ToLower(out[i].DisplayName + "\x00" + out[i].Address)
		right := strings.ToLower(out[j].DisplayName + "\x00" + out[j].Address)
		return left < right
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// AddRecipients adds the addresses a message was sent to to the sender's
// personal book. It never replaces an existing card: the user's own card, with
// whatever they wrote on it, is not improved by a name that arrived in a To
// header.
func (s *Service) AddRecipients(ctx context.Context, owner string, recipients []Recipient) {
	personal, err := s.EnsurePersonal(ctx, owner)
	if err != nil {
		s.log.Warn("contacts: could not open the personal book", "owner", owner, "error", err)
		return
	}
	owner = strings.ToLower(strings.TrimSpace(owner))
	for _, recipient := range recipients {
		address := strings.ToLower(strings.TrimSpace(recipient.Address))
		if address == "" || address == owner {
			continue
		}
		if _, err := s.books.FindContactByEmail(ctx, s.db, personal.ID, address); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("contacts: could not look up a recipient", "address", address, "error", err)
			continue
		}
		_, err := s.CreateCard(ctx, owner, personal.ID, Input{
			DisplayName: strings.TrimSpace(recipient.Name),
			Emails:      []Email{{Type: "work", Value: address}},
		})
		if err != nil {
			s.log.Warn("contacts: could not add a recipient", "address", address, "error", err)
			continue
		}
		// The card's FN has to be something a CardDAV client can display, so
		// it is the address for now. The projection stays empty: the address
		// is not a name, and the first non-junk message from this person is
		// what fills it in.
		if strings.TrimSpace(recipient.Name) == "" {
			row, err := s.books.FindContactByEmail(ctx, s.db, personal.ID, address)
			if err == nil && row.DisplayName != "" {
				row.DisplayName = ""
				if _, err := s.books.UpdateContact(ctx, s.db, row); err != nil {
					s.log.Warn("contacts: could not clear an unnamed card's name", "address", address, "error", err)
				}
			}
		}
	}
}

// UpdateNameFromInbound fills in a contact's name when the sender wrote to us
// with a display name and the card has none yet.
//
// It updates only a card that already exists: receiving mail from a stranger is
// not an instruction to put them in the address book, but it is the best answer
// available to "who is this?" for somebody already there.
func (s *Service) UpdateNameFromInbound(ctx context.Context, owner, fromAddress, fromName string) {
	fromName = strings.TrimSpace(fromName)
	if fromName == "" {
		return
	}
	personal, err := s.EnsurePersonal(ctx, owner)
	if err != nil {
		return
	}
	row, err := s.books.FindContactByEmail(ctx, s.db, personal.ID, fromAddress)
	if err != nil {
		// No card, or the lookup failed: either way there is nothing to update.
		return
	}
	if strings.TrimSpace(row.DisplayName) != "" {
		return
	}
	card, err := decodeCard(row.VCard)
	if err != nil {
		return
	}
	card.SetValue(vcard.FieldFormattedName, fromName)
	updated, err := rowFromCard(personal.ID, card)
	if err != nil {
		return
	}
	updated.ID = row.ID
	if _, err := s.books.UpdateContact(ctx, s.db, updated); err != nil {
		s.log.Warn("contacts: could not update a contact name", "address", fromAddress, "error", err)
	}
}

// Shares returns the shares on a book the caller owns.
func (s *Service) Shares(ctx context.Context, owner string, bookID int64) ([]store.AddressBookShare, error) {
	access, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareWrite)
	if err != nil {
		return nil, err
	}
	if !ok || !access.IsOwner {
		return nil, ErrDenied
	}
	return s.books.ListShares(ctx, s.db, bookID)
}

// Share offers a book to a person or a group. The share starts pending; the
// caller sends the notification, because only the HTTP layer knows the sender's
// display name and the mail service.
func (s *Service) Share(ctx context.Context, owner string, bookID int64, kind, address string, groupID int64, level string) (store.AddressBookShare, error) {
	access, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareWrite)
	if err != nil {
		return store.AddressBookShare{}, err
	}
	if !ok || !access.IsOwner || access.Scope != store.AddressBookShared {
		return store.AddressBookShare{}, ErrDenied
	}
	share := store.AddressBookShare{
		BookID:      bookID,
		SubjectKind: kind,
		Level:       level,
		Status:      store.AddressBookSharePending,
	}
	switch kind {
	case store.ShareSubjectUser:
		address = strings.ToLower(strings.TrimSpace(address))
		if address == "" {
			return store.AddressBookShare{}, fmt.Errorf("%w: a share needs an address", ErrInvalid)
		}
		if address == strings.ToLower(strings.TrimSpace(owner)) {
			return store.AddressBookShare{}, fmt.Errorf("%w: the owner already has the book", ErrInvalid)
		}
		share.SubjectAddress = &address
	case store.ShareSubjectGroup:
		if groupID == 0 {
			return store.AddressBookShare{}, fmt.Errorf("%w: a share needs a group", ErrInvalid)
		}
		share.SubjectGroupID = &groupID
	default:
		return store.AddressBookShare{}, fmt.Errorf("%w: subject kind must be user or group", ErrInvalid)
	}
	return s.books.PutShare(ctx, s.db, share)
}

// Unshare removes a share by id.
func (s *Service) Unshare(ctx context.Context, owner string, bookID, shareID int64) error {
	access, ok, err := s.require(ctx, owner, bookID, store.AddressBookShareWrite)
	if err != nil {
		return err
	}
	if !ok || !access.IsOwner {
		return ErrDenied
	}
	return translate(s.books.DeleteShare(ctx, s.db, shareID))
}

// Offers returns books waiting for the caller to answer.
func (s *Service) Offers(ctx context.Context, owner string) ([]store.AddressBookShareOffer, error) {
	return s.books.Offers(ctx, s.db, owner)
}

// Decide answers an offer.
func (s *Service) Decide(ctx context.Context, owner string, shareID int64, decision string) (store.AddressBookShare, error) {
	return s.books.Decide(ctx, s.db, shareID, owner, decision)
}

// require resolves the caller's access and whether it meets the wanted level.
func (s *Service) require(ctx context.Context, owner string, bookID int64, want string) (store.AddressBookAccess, bool, error) {
	access, ok, err := s.books.Access(ctx, s.db, bookID, owner)
	if err != nil {
		return access, false, translate(err)
	}
	if !ok {
		return access, false, nil
	}
	if want == store.AddressBookShareWrite && access.Level != store.AddressBookShareWrite {
		return access, false, nil
	}
	return access, true, nil
}

func scopePriority(scope string) int {
	switch scope {
	case store.AddressBookPersonal:
		return 0
	case store.AddressBookShared:
		return 1
	default:
		return 2
	}
}

func translate(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrConflict):
		return ErrInvalid
	default:
		return err
	}
}

// --- vCard ---------------------------------------------------------------

// buildCard turns the web form's fields into a vCard 3.0, the version every
// CardDAV client in use understands.
func buildCard(in Input, uid string) (vcard.Card, error) {
	card := vcard.Card{}
	card.SetValue(vcard.FieldVersion, "3.0")
	card.SetValue(vcard.FieldUID, uid)

	name := strings.TrimSpace(in.DisplayName)
	if name == "" {
		name = strings.TrimSpace(in.FirstName + " " + in.LastName)
	}
	if name == "" {
		for _, email := range in.Emails {
			if strings.TrimSpace(email.Value) != "" {
				name = strings.TrimSpace(email.Value)
				break
			}
		}
	}
	if name == "" {
		return nil, fmt.Errorf("%w: a card needs a name or an address", ErrInvalid)
	}
	card.SetValue(vcard.FieldFormattedName, name)

	if in.FirstName != "" || in.LastName != "" {
		card.Set(vcard.FieldName, &vcard.Field{Value: strings.Join([]string{
			strings.TrimSpace(in.LastName), strings.TrimSpace(in.FirstName), "", "", "",
		}, ";")})
	}
	for _, email := range in.Emails {
		value := strings.TrimSpace(email.Value)
		if value == "" {
			continue
		}
		card.Add(vcard.FieldEmail, &vcard.Field{
			Value:  value,
			Params: vcard.Params{vcard.ParamType: []string{typeOr(email.Type, vcard.TypeWork)}},
		})
	}
	for _, phone := range in.Phones {
		value := strings.TrimSpace(phone.Value)
		if value == "" {
			continue
		}
		card.Add(vcard.FieldTelephone, &vcard.Field{
			Value:  value,
			Params: vcard.Params{vcard.ParamType: []string{typeOr(phone.Type, vcard.TypeVoice)}},
		})
	}
	for _, postal := range in.Addresses {
		if strings.TrimSpace(postal.Street+postal.City+postal.Region+postal.PostalCode+postal.Country) == "" {
			continue
		}
		card.Add(vcard.FieldAddress, &vcard.Field{
			Value: strings.Join([]string{
				"", "", postal.Street, postal.City, postal.Region, postal.PostalCode, postal.Country,
			}, ";"),
			Params: vcard.Params{vcard.ParamType: []string{typeOr(postal.Type, vcard.TypeWork)}},
		})
	}
	if in.Organization != "" || in.Department != "" {
		card.SetValue(vcard.FieldOrganization, strings.Join([]string{in.Organization, in.Department}, ";"))
	}
	if in.Title != "" {
		card.SetValue(vcard.FieldTitle, in.Title)
	}
	if in.Website != "" {
		card.SetValue(vcard.FieldURL, in.Website)
	}
	if in.Notes != "" {
		card.SetValue(vcard.FieldNote, in.Notes)
	}
	if in.Birthday != "" {
		card.SetValue(vcard.FieldBirthday, in.Birthday)
	}
	if len(encodeCard(card)) > maxCardSize {
		return nil, fmt.Errorf("%w: the card is too large", ErrInvalid)
	}
	return card, nil
}

func decodeCard(raw string) (vcard.Card, error) {
	card, err := vcard.NewDecoder(strings.NewReader(raw)).Decode()
	if err != nil {
		return nil, fmt.Errorf("%w: the card is not a vCard", ErrInvalid)
	}
	if card == nil {
		return nil, fmt.Errorf("%w: the card is empty", ErrInvalid)
	}
	return card, nil
}

func encodeCard(card vcard.Card) string {
	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(card); err != nil {
		return ""
	}
	return buf.String()
}

// rowFromCard produces the stored row and its searchable projection.
func rowFromCard(bookID int64, card vcard.Card) (store.AddressContact, error) {
	if card.Value(vcard.FieldVersion) == "" {
		card.SetValue(vcard.FieldVersion, "3.0")
	}
	raw := encodeCard(card)
	if raw == "" {
		return store.AddressContact{}, fmt.Errorf("%w: the card could not be encoded", ErrInvalid)
	}
	display := strings.TrimSpace(card.PreferredValue(vcard.FieldFormattedName))
	if display == "" {
		display = strings.TrimSpace(card.PreferredValue(vcard.FieldEmail))
	}
	emails := card.Values(vcard.FieldEmail)
	phones := card.Values(vcard.FieldTelephone)
	organization := strings.Join(strings.Split(card.Value(vcard.FieldOrganization), ";"), " ")
	title := card.Value(vcard.FieldTitle)
	notes := card.Value(vcard.FieldNote)
	primary := ""
	if len(emails) > 0 {
		primary = strings.TrimSpace(emails[0])
	}
	search := strings.ToLower(strings.Join(append(append([]string{display, organization, title}, emails...), phones...), "\n"))

	return store.AddressContact{
		BookID:       bookID,
		UID:          card.Value(vcard.FieldUID),
		VCard:        raw,
		DisplayName:  display,
		Email:        primary,
		SearchText:   search,
		Organization: strings.TrimSpace(organization),
		Title:        strings.TrimSpace(title),
		Notes:        strings.TrimSpace(notes),
	}, nil
}

// toEntry turns a stored row back into the form the web client edits.
func toEntry(row store.AddressContact) Entry {
	out := Entry{
		ID:           row.ID,
		BookID:       row.BookID,
		UID:          row.UID,
		DisplayName:  row.DisplayName,
		Organization: row.Organization,
		Title:        row.Title,
		Notes:        row.Notes,
		VCard:        row.VCard,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
	card, err := decodeCard(row.VCard)
	if err != nil {
		return out
	}
	if name := card.Get(vcard.FieldName); name != nil {
		parts := strings.Split(name.Value, ";")
		if len(parts) > 0 {
			out.LastName = parts[0]
		}
		if len(parts) > 1 {
			out.FirstName = parts[1]
		}
	}
	for _, field := range card[vcard.FieldEmail] {
		out.Emails = append(out.Emails, Email{Type: firstType(field), Value: field.Value})
	}
	for _, field := range card[vcard.FieldTelephone] {
		out.Phones = append(out.Phones, Phone{Type: firstType(field), Value: field.Value})
	}
	if organization := card.Value(vcard.FieldOrganization); organization != "" {
		parts := strings.Split(organization, ";")
		out.Organization = parts[0]
		if len(parts) > 1 {
			out.Department = parts[1]
		}
	}
	for _, field := range card[vcard.FieldAddress] {
		parts := strings.Split(field.Value, ";")
		postal := Postal{Type: firstType(field)}
		if len(parts) > 2 {
			postal.Street = parts[2]
		}
		if len(parts) > 3 {
			postal.City = parts[3]
		}
		if len(parts) > 4 {
			postal.Region = parts[4]
		}
		if len(parts) > 5 {
			postal.PostalCode = parts[5]
		}
		if len(parts) > 6 {
			postal.Country = parts[6]
		}
		out.Addresses = append(out.Addresses, postal)
	}
	out.Website = card.Value(vcard.FieldURL)
	out.Birthday = card.Value(vcard.FieldBirthday)
	return out
}

func firstType(field *vcard.Field) string {
	if field == nil {
		return ""
	}
	if types := field.Params.Types(); len(types) > 0 {
		return types[0]
	}
	return ""
}

func typeOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func newUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("contacts: cannot read randomness: " + err.Error())
	}
	return fmt.Sprintf("%x", buf)
}

// slugify turns a book name into the path segment a CardDAV client uses.
func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "book"
	}
	if len(out) > 60 {
		out = strings.Trim(out[:60], "-")
	}
	return out
}

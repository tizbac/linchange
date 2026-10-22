package contacts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/contacts"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

func seedUser(t *testing.T, db *sqlx.DB, address string, admin bool) {
	t.Helper()
	if err := store.NewUsers().Create(context.Background(), db, store.CreateUserRequest{
		Address: address, Source: policy.SourceLocal, DisplayName: address, IsDomainAdmin: admin,
	}); err != nil {
		t.Fatalf("seeding %s: %v", address, err)
	}
}

func seedOrganization(t *testing.T, db *sqlx.DB) store.Workgroup {
	t.Helper()
	ctx := context.Background()
	if err := store.NewDomains().Create(ctx, db, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("seeding domain: %v", err)
	}
	wg, err := store.NewWorkgroups().Create(ctx, db, store.CreateWorkgroupRequest{
		Name: "Acme", Domain: "example.com", DisplayName: "Acme",
	})
	if err != nil {
		t.Fatalf("seeding workgroup: %v", err)
	}
	return wg
}

func TestPersonalBookIsCreatedOnFirstUse(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	seedUser(t, db, "alice@example.com", false)
	svc := contacts.New(db, nil)

	books, err := svc.Books(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("Books: %v", err)
	}
	if len(books) != 1 || books[0].Scope != store.AddressBookPersonal {
		t.Fatalf("books = %+v, want one personal book", books)
	}
	// Asking twice must not make a second one.
	again, err := svc.Books(ctx, "alice@example.com")
	if err != nil || len(again) != 1 || again[0].ID != books[0].ID {
		t.Fatalf("second Books = %+v, %v", again, err)
	}
}

func TestSharedBookNeedsTheRecipientsAnswer(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	seedUser(t, db, "alice@example.com", false)
	seedUser(t, db, "bob@example.com", false)
	svc := contacts.New(db, nil)

	book, err := svc.CreateBook(ctx, "alice@example.com", "Team contacts", "the team")
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	share, err := svc.Share(ctx, "alice@example.com", book.ID,
		store.ShareSubjectUser, "bob@example.com", 0, store.AddressBookShareRead)
	if err != nil {
		t.Fatalf("Share: %v", err)
	}

	// Pending grants nothing: bob's books do not include it yet.
	books, err := svc.Books(ctx, "bob@example.com")
	if err != nil {
		t.Fatalf("Books before accepting: %v", err)
	}
	for _, b := range books {
		if b.ID == book.ID {
			t.Fatalf("a pending share already granted access: %+v", b)
		}
	}
	offers, err := svc.Offers(ctx, "bob@example.com")
	if err != nil || len(offers) != 1 || offers[0].ID != share.ID {
		t.Fatalf("offers = %+v, %v", offers, err)
	}

	if _, err := svc.Decide(ctx, "bob@example.com", share.ID, "accepted"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	books, err = svc.Books(ctx, "bob@example.com")
	if err != nil {
		t.Fatalf("Books after accepting: %v", err)
	}
	found := false
	for _, b := range books {
		if b.ID == book.ID {
			found = true
			if b.Level != store.AddressBookShareRead {
				t.Errorf("level = %q, want read", b.Level)
			}
		}
	}
	if !found {
		t.Fatalf("an accepted share is not in bob's books: %+v", books)
	}

	// Read means read: bob may list it but not write a card into it.
	if _, err := svc.CreateCard(ctx, "bob@example.com", book.ID,
		contacts.Input{DisplayName: "X", Emails: []contacts.Email{{Value: "x@example.com"}}}); !errors.Is(err, contacts.ErrDenied) {
		t.Errorf("reader writing = %v, want ErrDenied", err)
	}
	if _, err := svc.CreateCard(ctx, "alice@example.com", book.ID,
		contacts.Input{DisplayName: "Carol", Emails: []contacts.Email{{Value: "carol@example.com"}}}); err != nil {
		t.Errorf("owner writing: %v", err)
	}
}

func TestOrganizationDirectory(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	wg := seedOrganization(t, db)
	seedUser(t, db, "alice@example.com", true)  // the administrator
	seedUser(t, db, "bob@example.com", false)   // an ordinary member
	seedUser(t, db, "dave@example.com", true)   // another administrator
	seedUser(t, db, "eve@other.example", false) // outside the organization

	svc := contacts.New(db, nil)
	if err := svc.UpsertOrganizationPerson(ctx, "alice@example.com", wg.ID,
		"bob@example.com", "Bob Member", "+39 02 1234"); err != nil {
		t.Fatalf("UpsertOrganizationPerson: %v", err)
	}

	org, err := svc.EnsureOrganization(ctx, "alice@example.com", wg.ID)
	if err != nil {
		t.Fatalf("EnsureOrganization: %v", err)
	}

	// A member reads the directory; an outsider does not see it.
	books, err := svc.Books(ctx, "bob@example.com")
	if err != nil {
		t.Fatalf("Books: %v", err)
	}
	if !containsBook(books, org.ID) {
		t.Fatalf("a member cannot see the organization directory: %+v", books)
	}
	eveBooks, err := svc.Books(ctx, "eve@other.example")
	if err != nil {
		t.Fatalf("Books for an outsider: %v", err)
	}
	if containsBook(eveBooks, org.ID) {
		t.Fatalf("an outsider sees the organization directory: %+v", eveBooks)
	}

	// A member reads but does not write; an administrator writes.
	if _, err := svc.CreateCard(ctx, "bob@example.com", org.ID,
		contacts.Input{DisplayName: "X", Emails: []contacts.Email{{Value: "x@example.com"}}}); !errors.Is(err, contacts.ErrDenied) {
		t.Errorf("member writing to the directory = %v, want ErrDenied", err)
	}
	if _, err := svc.CreateCard(ctx, "dave@example.com", org.ID,
		contacts.Input{DisplayName: "Eve", Emails: []contacts.Email{{Value: "eve@other.example"}}}); err != nil {
		t.Errorf("administrator writing to the directory: %v", err)
	}

	// The card created at user creation carries the phone that was given.
	search, err := svc.Search(ctx, "alice@example.com", "bob", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(search) != 1 || search[0].Source != "Organization directory" {
		t.Fatalf("search = %+v, want the organization directory", search)
	}
	entries, err := svc.Cards(ctx, "alice@example.com", org.ID)
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	var bobCard *contacts.Entry
	for i := range entries {
		if entries[i].DisplayName == "Bob Member" {
			bobCard = &entries[i]
		}
	}
	if bobCard == nil {
		t.Fatalf("the directory has no card for bob: %+v", entries)
	}
	if len(bobCard.Phones) != 1 || bobCard.Phones[0].Value != "+39 02 1234" {
		t.Errorf("phones = %+v, want the number given at creation", bobCard.Phones)
	}
}

func TestSearchPrefersThePersonalBookAndNamesItsSource(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	wg := seedOrganization(t, db)
	seedUser(t, db, "alice@example.com", true)
	seedUser(t, db, "bob@example.com", false)

	svc := contacts.New(db, nil)
	personal, err := svc.EnsurePersonal(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("personal book: %v", err)
	}
	if _, err := svc.CreateCard(ctx, "alice@example.com", personal.ID, contacts.Input{
		DisplayName: "Bobby (mine)", Emails: []contacts.Email{{Value: "bob@example.com"}},
	}); err != nil {
		t.Fatalf("personal card: %v", err)
	}
	if err := svc.UpsertOrganizationPerson(ctx, "alice@example.com", wg.ID,
		"bob@example.com", "Bob Directory", ""); err != nil {
		t.Fatalf("directory card: %v", err)
	}

	got, err := svc.Search(ctx, "alice@example.com", "bob", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("suggestions = %+v, want one address", got)
	}
	if got[0].Source != "My contacts" || got[0].Scope != store.AddressBookPersonal {
		t.Errorf("source = %q/%q, want the personal book", got[0].Source, got[0].Scope)
	}
	if got[0].DisplayName != "Bobby (mine)" {
		t.Errorf("name = %q, want the personal one", got[0].DisplayName)
	}
}

func TestSendAddsAndReceiveNames(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	seedUser(t, db, "alice@example.com", false)
	seedUser(t, db, "bob@example.com", false)
	svc := contacts.New(db, nil)

	svc.AddRecipients(ctx, "alice@example.com", []contacts.Recipient{
		{Address: "bob@example.com"},
		{Address: "alice@example.com"}, // never add yourself
	})

	personal, err := svc.EnsurePersonal(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("personal book: %v", err)
	}
	entries, err := svc.Cards(ctx, "alice@example.com", personal.ID)
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	if len(entries) != 1 || entries[0].DisplayName != "" {
		t.Fatalf("after sending = %+v, want bob with no name yet", entries)
	}

	// A non-junk message fills the name in.
	svc.UpdateNameFromInbound(ctx, "alice@example.com", "bob@example.com", "Bob Smith")
	entries, err = svc.Cards(ctx, "alice@example.com", personal.ID)
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	if len(entries) != 1 || entries[0].DisplayName != "Bob Smith" {
		t.Fatalf("after receiving = %+v, want the name filled in", entries)
	}

	// A message from somebody who is not in the book does not add them.
	svc.UpdateNameFromInbound(ctx, "alice@example.com", "stranger@example.com", "A Stranger")
	entries, _ = svc.Cards(ctx, "alice@example.com", personal.ID)
	if len(entries) != 1 {
		t.Errorf("receiving from a stranger added a card: %+v", entries)
	}

	// And a second send does not duplicate bob.
	svc.AddRecipients(ctx, "alice@example.com", []contacts.Recipient{{Address: "bob@example.com"}})
	entries, _ = svc.Cards(ctx, "alice@example.com", personal.ID)
	if len(entries) != 1 {
		t.Errorf("sending twice duplicated the card: %+v", entries)
	}
}

func containsBook(books []store.AddressBookAccess, id int64) bool {
	for _, book := range books {
		if book.ID == id {
			return true
		}
	}
	return false
}

package rop

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/contacts"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

func seedRopUser(t *testing.T, db *sqlx.DB, address string) {
	t.Helper()
	if err := store.NewUsers().Create(context.Background(), db, store.CreateUserRequest{
		Address: address, Source: policy.SourceLocal, DisplayName: address,
	}); err != nil {
		t.Fatalf("seeding %s: %v", address, err)
	}
}

func testEntry() contacts.Entry {
	return contacts.Entry{
		ID:           7,
		BookID:       3,
		UID:          "card-7",
		DisplayName:  "Alice Example",
		FirstName:    "Alice",
		LastName:     "Example",
		Organization: "Acme",
		Department:   "Engineering",
		Title:        "Engineer",
		Emails: []contacts.Email{
			{Type: "work", Value: "alice@example.com"},
			{Type: "home", Value: "alice@home.example"},
		},
		Phones: []contacts.Phone{
			{Type: "work", Value: "+1 555 0100"},
			{Type: "cell", Value: "+1 555 0199"},
		},
		Addresses: []contacts.Postal{
			{Type: "work", Street: "1 Main St", City: "Springfield", Region: "IL", PostalCode: "62701", Country: "US"},
		},
		Website:   "https://example.com",
		Notes:     "met at a conference",
		Birthday:  "1990-04-05",
		CreatedAt: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt: time.Date(2024, 6, 7, 8, 9, 10, 0, time.UTC),
	}
}

func TestContactPropsCarryTheCardFields(t *testing.T) {
	r := &folderResolver{back: &Backend{}, owner: "alice@example.com"}
	st := &sessionState{}
	email1 := st.assignNamed(propName{Kind: 0, GUID: psetidAddress, LID: 0x8083})

	props := r.contactProps(st, testEntry(), PRIVATE_FID_CONTACTS)

	checks := map[uint32]string{
		prDisplayName:             "Alice Example",
		prGivenName:               "Alice",
		prSurname:                 "Example",
		prCompanyName:             "Acme",
		prDepartmentName:          "Engineering",
		prTitle:                   "Engineer",
		prEmailAddress:            "alice@example.com",
		prSmtpAddress:             "alice@example.com",
		prBusinessTelephoneNumber: "+1 555 0100",
		prMobileTelephoneNumber:   "+1 555 0199",
		prBusinessAddressCity:     "Springfield",
		prBody:                    "met at a conference",
		prBusinessHomePage:        "https://example.com",
	}
	for tag, want := range checks {
		if got, _ := props[tag].(string); got != want {
			t.Errorf("prop 0x%08x = %q, want %q", tag, got, want)
		}
	}
	if got := props[propTag(email1, ptUnicode)]; got != "alice@example.com" {
		t.Errorf("Email1 = %v, want alice@example.com", got)
	}
	if props[prMessageClass] != "IPM.Contact" {
		t.Errorf("message class = %v", props[prMessageClass])
	}
	if _, ok := props[prEntryID].([]byte); !ok {
		t.Errorf("entry id is %T, want []byte", props[prEntryID])
	}
	if born, ok := props[prBirthday].(time.Time); !ok || born.Year() != 1990 || born.Month() != 4 {
		t.Errorf("birthday = %v, want 1990-04-05", props[prBirthday])
	}
}

func TestNamedPropertyIDsAreStableAndReversible(t *testing.T) {
	st := &sessionState{}
	pn := propName{Kind: 0, GUID: psetidAddress, LID: 0x8083}
	first := st.assignNamed(pn)
	again := st.assignNamed(pn)
	if first != again {
		t.Fatalf("same name got ids 0x%x and 0x%x", first, again)
	}
	if first < 0x9000 {
		t.Fatalf("named id 0x%x collides with the fixed tag range", first)
	}
	other := st.assignNamed(propName{Kind: 1, GUID: psetidAddress, Name: "X-Test"})
	if other == first {
		t.Fatal("two names got the same id")
	}
	got, ok := st.namedNames[first]
	if !ok || got.LID != 0x8083 {
		t.Fatalf("reverse mapping = %+v, %v", got, ok)
	}
}

func setPropsBody(values ...propVal) []byte {
	body := newWriter()
	size := 2
	for _, pv := range values {
		w := newWriter()
		if err := encodePropVal(w, pv.tag, pv.val); err != nil {
			panic(err)
		}
		size += 4 + len(w.buf)
	}
	body.u16(uint16(size))
	body.u16(uint16(len(values)))
	for _, pv := range values {
		body.u32(pv.tag)
		if err := encodePropVal(body, pv.tag, pv.val); err != nil {
			panic(err)
		}
	}
	return body.buf
}

func TestSetPropertiesDecodeContactValues(t *testing.T) {
	st := &sessionState{objects: map[uint32]*mapiObject{}}
	m := &messageObj{newContact: true}
	st.put(1, &mapiObject{kind: objMessage, message: m})
	handles := []uint32{0, 1}

	body := setPropsBody(
		propVal{tag: prDisplayName, val: "Alice Example"},
		propVal{tag: prGivenName, val: "Alice"},
		propVal{tag: prEmailAddress, val: "alice@example.com"},
		propVal{tag: prBirthday, val: time.Date(1990, 4, 5, 0, 0, 0, 0, time.UTC)},
	)
	var out bytes.Buffer
	if err := hSetProps(context.Background(), st, nil, nil, 1, newReader(body), handles, &out); err != nil {
		t.Fatalf("hSetProps: %v", err)
	}
	if m.pending[prDisplayName] != "Alice Example" {
		t.Errorf("pending display name = %v", m.pending[prDisplayName])
	}
	if m.pending[prEmailAddress] != "alice@example.com" {
		t.Errorf("pending email = %v", m.pending[prEmailAddress])
	}
	if _, ok := m.pending[prBirthday].(time.Time); !ok {
		t.Errorf("pending birthday = %T, want time.Time", m.pending[prBirthday])
	}
}

func TestSaveContactCreatesAndEditsACard(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	seedRopUser(t, db, "alice@example.com")
	svc := contacts.New(db, nil)
	book, err := svc.EnsurePersonal(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("EnsurePersonal: %v", err)
	}
	r := &folderResolver{
		back:  &Backend{DB: db, Mailboxes: imapd.NewMailboxRepo(), Contacts: svc},
		owner: "alice@example.com",
		ctx:   ctx,
	}
	st := &sessionState{owner: "alice@example.com", folders: r}
	email1 := st.assignNamed(propName{Kind: 0, GUID: psetidAddress, LID: 0x8083})

	m := &messageObj{
		newContact:  true,
		bookID:      book.ID,
		folderValue: PRIVATE_FID_CONTACTS,
		pending: map[uint32]any{
			prDisplayName:              "Alice Example",
			prGivenName:                "Alice",
			prSurname:                  "Example",
			prCompanyName:              "Acme",
			prTitle:                    "Engineer",
			prBusinessTelephoneNumber:  "+1 555 0100",
			prEmailAddress:             "alice@example.com",
			propTag(email1, ptUnicode): "alice@example.com",
		},
	}
	entry, err := r.saveContact(ctx, st, "alice@example.com", m)
	if err != nil {
		t.Fatalf("saveContact: %v", err)
	}
	if entry.ID == 0 || entry.DisplayName != "Alice Example" {
		t.Fatalf("created entry = %+v", entry)
	}
	if len(entry.Emails) != 1 || entry.Emails[0].Value != "alice@example.com" {
		t.Errorf("created emails = %+v", entry.Emails)
	}
	if len(entry.Phones) != 1 || entry.Phones[0].Value != "+1 555 0100" {
		t.Errorf("created phones = %+v", entry.Phones)
	}

	m.contact = &entry
	m.newContact = false
	m.pending = map[uint32]any{prTitle: "Principal Engineer"}
	updated, err := r.saveContact(ctx, st, "alice@example.com", m)
	if err != nil {
		t.Fatalf("second saveContact: %v", err)
	}
	if updated.ID != entry.ID || updated.Title != "Principal Engineer" {
		t.Fatalf("updated entry = %+v", updated)
	}
	if updated.UID != entry.UID {
		t.Errorf("UID changed from %q to %q", entry.UID, updated.UID)
	}
}

// ropRequest wraps a ROP list in the RPC_HEADER_EXT and handle table shape
// Execute consumes. Four empty handle slots are enough for the small lists
// these tests build.
func ropRequest(rops ...[]byte) []byte {
	var list bytes.Buffer
	for _, rop := range rops {
		list.Write(rop)
	}
	payload := newWriter()
	payload.u16(uint16(2 + list.Len()))
	payload.bytes(list.Bytes())
	for i := 0; i < 4; i++ {
		payload.u32(0)
	}
	out := newWriter()
	out.u16(1)
	out.u16(rheFlagLast)
	out.u16(uint16(len(payload.buf)))
	out.u16(uint16(len(payload.buf)))
	out.bytes(payload.buf)
	return out.buf
}

func TestROPOutputSeparatesResponsesFromTheHandleTable(t *testing.T) {
	st := &sessionState{
		owner:      "alice@example.com",
		nextHandle: 1,
		objects:    map[uint32]*mapiObject{0: &mapiObject{kind: objLogon}},
	}
	state := newWriter()
	state.u8(ropGetStoreState)
	state.u8(0)
	state.u8(0)
	out, err := Execute(context.Background(), st, &Backend{}, nil, ropRequest(state.buf))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out) < 10 {
		t.Fatalf("short ROP output: %x", out)
	}
	ropSize := int(binary.LittleEndian.Uint16(out[8:10]))
	// Four handle slots follow the ROP responses; RopSize covers only the
	// size field and the responses, not the handle table.
	if want := len(out) - 8 - 4*4; ropSize != want {
		t.Fatalf("RopSize = %d, want %d (output %d bytes)", ropSize, want, len(out))
	}
}

func TestContactCreateSetSaveOverTheROPLayer(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	seedRopUser(t, db, "alice@example.com")
	svc := contacts.New(db, nil)
	if _, err := svc.EnsurePersonal(ctx, "alice@example.com"); err != nil {
		t.Fatalf("EnsurePersonal: %v", err)
	}
	back := &Backend{DB: db, Mailboxes: imapd.NewMailboxRepo(), Contacts: svc}
	r, err := newFolderResolver(ctx, back, "alice@example.com")
	if err != nil {
		t.Fatalf("newFolderResolver: %v", err)
	}
	st := &sessionState{
		owner:      "alice@example.com",
		folders:    r,
		nextHandle: 1,
		objects:    map[uint32]*mapiObject{0: &mapiObject{kind: objLogon}},
	}

	openFolder := newWriter()
	openFolder.u8(ropOpenFolder)
	openFolder.u8(0)
	openFolder.u8(0) // input handle: logon
	openFolder.u8(1) // output handle
	openFolder.u64(fidFromValue(PRIVATE_FID_CONTACTS))
	openFolder.u8(0)

	create := newWriter()
	create.u8(ropCreateMessage)
	create.u8(0)
	create.u8(1) // input handle: contact folder
	create.u8(2) // output handle
	create.u16(0x04E4)
	create.u64(fidFromValue(PRIVATE_FID_CONTACTS))
	create.u8(0)

	values := setPropsBody(
		propVal{tag: prDisplayName, val: "Alice Example"},
		propVal{tag: prGivenName, val: "Alice"},
		propVal{tag: prSurname, val: "Example"},
		propVal{tag: prCompanyName, val: "Acme"},
		propVal{tag: prEmailAddress, val: "alice@example.com"},
		propVal{tag: prBusinessTelephoneNumber, val: "+1 555 0100"},
	)
	set := newWriter()
	set.u8(ropSetProperties)
	set.u8(0)
	set.u8(2) // input handle: new contact
	set.bytes(values)

	save := newWriter()
	save.u8(ropSaveChangesMessage)
	save.u8(0)
	save.u8(2) // response handle index
	save.u8(2) // input handle index
	save.u8(0) // save flags

	req := ropRequest(openFolder.buf, create.buf, set.buf, save.buf)
	out, err := Execute(ctx, st, back, nil, req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("empty ROP response")
	}
	books, err := svc.Books(ctx, "alice@example.com")
	if err != nil || len(books) == 0 {
		t.Fatalf("Books: %v, %+v", err, books)
	}
	cards, err := svc.Cards(ctx, "alice@example.com", books[0].ID)
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one contact", cards)
	}
	card := cards[0]
	if card.DisplayName != "Alice Example" || card.Organization != "Acme" {
		t.Errorf("card = %+v", card)
	}
	if len(card.Emails) != 1 || card.Emails[0].Value != "alice@example.com" {
		t.Errorf("emails = %+v", card.Emails)
	}
	if len(card.Phones) != 1 || card.Phones[0].Value != "+1 555 0100" {
		t.Errorf("phones = %+v", card.Phones)
	}
}

func TestContactContentsAndPropertiesOverTheROPLayer(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	seedRopUser(t, db, "alice@example.com")
	svc := contacts.New(db, nil)
	book, err := svc.EnsurePersonal(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("EnsurePersonal: %v", err)
	}
	card, err := svc.CreateCard(ctx, "alice@example.com", book.ID, contacts.Input{
		DisplayName: "Alice Example",
		FirstName:   "Alice",
		LastName:    "Example",
		Emails:      []contacts.Email{{Type: "work", Value: "alice@example.com"}},
		Phones:      []contacts.Phone{{Type: "work", Value: "+1 555 0100"}},
	})
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	back := &Backend{DB: db, Mailboxes: imapd.NewMailboxRepo(), Contacts: svc}
	r, err := newFolderResolver(ctx, back, "alice@example.com")
	if err != nil {
		t.Fatalf("newFolderResolver: %v", err)
	}
	newState := func() *sessionState {
		return &sessionState{
			owner:      "alice@example.com",
			folders:    r,
			nextHandle: 1,
			objects:    map[uint32]*mapiObject{0: &mapiObject{kind: objLogon}},
		}
	}

	openFolder := newWriter()
	openFolder.u8(ropOpenFolder)
	openFolder.u8(0)
	openFolder.u8(0)
	openFolder.u8(1)
	openFolder.u64(fidFromValue(PRIVATE_FID_CONTACTS))
	openFolder.u8(0)

	contents := newWriter()
	contents.u8(ropGetContentsTable)
	contents.u8(0)
	contents.u8(1)
	contents.u8(2)
	contents.u8(0)

	columns := []uint32{prDisplayName, prEmailAddress, prGivenName, prSurname, prEntryID}
	setColumns := newWriter()
	setColumns.u8(ropSetColumns)
	setColumns.u8(0)
	setColumns.u8(2)
	setColumns.u8(0)
	if err := setColumns.proptags(columns); err != nil {
		t.Fatalf("proptags: %v", err)
	}

	query := newWriter()
	query.u8(ropQueryRows)
	query.u8(0)
	query.u8(2)
	query.u8(0) // flags
	query.u8(1) // forward
	query.u16(10)

	out, err := Execute(ctx, newState(), back, nil,
		ropRequest(openFolder.buf, contents.buf, setColumns.buf, query.buf))
	if err != nil {
		t.Fatalf("contents Execute: %v", err)
	}
	if !bytes.Contains(out, utf16LE("Alice Example")) {
		t.Errorf("contents rows do not contain the display name: %x", out)
	}
	if !bytes.Contains(out, utf16LE("alice@example.com")) {
		t.Errorf("contents rows do not contain the address: %x", out)
	}

	openMessage := newWriter()
	openMessage.u8(ropOpenMessage)
	openMessage.u8(0)
	openMessage.u8(1)
	openMessage.u8(2)
	openMessage.u16(0x04E4)
	openMessage.u64(fidFromValue(PRIVATE_FID_CONTACTS))
	openMessage.u8(0)
	openMessage.u64(fidFromValue(uint64(card.ID)))

	getProps := newWriter()
	getProps.u8(ropGetPropertiesSpecific)
	getProps.u8(0)
	getProps.u8(2)
	getProps.u16(0xFFFF)
	getProps.u16(1)
	tags := []uint32{prDisplayName, prEmailAddress, prBusinessTelephoneNumber, prMessageClass}
	if err := getProps.proptags(tags); err != nil {
		t.Fatalf("proptags: %v", err)
	}
	out, err = Execute(ctx, newState(), back, nil,
		ropRequest(openFolder.buf, openMessage.buf, getProps.buf))
	if err != nil {
		t.Fatalf("props Execute: %v", err)
	}
	for _, want := range []string{"Alice Example", "alice@example.com", "+1 555 0100", "IPM.Contact"} {
		if !bytes.Contains(out, utf16LE(want)) {
			t.Errorf("properties do not contain %q: %x", want, out)
		}
	}
}

func TestContactNotesCanBeWrittenThroughAStream(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	seedRopUser(t, db, "alice@example.com")
	svc := contacts.New(db, nil)
	book, err := svc.EnsurePersonal(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("EnsurePersonal: %v", err)
	}
	card, err := svc.CreateCard(ctx, "alice@example.com", book.ID, contacts.Input{
		DisplayName: "Alice Example",
		Emails:      []contacts.Email{{Type: "work", Value: "alice@example.com"}},
	})
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	back := &Backend{DB: db, Mailboxes: imapd.NewMailboxRepo(), Contacts: svc}
	r, err := newFolderResolver(ctx, back, "alice@example.com")
	if err != nil {
		t.Fatalf("newFolderResolver: %v", err)
	}
	st := &sessionState{
		owner:      "alice@example.com",
		folders:    r,
		nextHandle: 1,
		objects:    map[uint32]*mapiObject{0: &mapiObject{kind: objLogon}},
	}

	openFolder := newWriter()
	openFolder.u8(ropOpenFolder)
	openFolder.u8(0)
	openFolder.u8(0)
	openFolder.u8(1)
	openFolder.u64(fidFromValue(PRIVATE_FID_CONTACTS))
	openFolder.u8(0)

	openMessage := newWriter()
	openMessage.u8(ropOpenMessage)
	openMessage.u8(0)
	openMessage.u8(1)
	openMessage.u8(2)
	openMessage.u16(0x04E4)
	openMessage.u64(fidFromValue(PRIVATE_FID_CONTACTS))
	openMessage.u8(0)
	openMessage.u64(fidFromValue(uint64(card.ID)))

	openStream := newWriter()
	openStream.u8(ropOpenStream)
	openStream.u8(0)
	openStream.u8(2)
	openStream.u8(3)
	openStream.u32(prBody)
	openStream.u8(0)

	notes := "Written through a MAPI stream."
	writeStream := newWriter()
	writeStream.u8(ropWriteStream)
	writeStream.u8(0)
	writeStream.u8(3)
	encoded := utf16LE(notes)
	writeStream.u16(uint16(len(encoded)))
	writeStream.bytes(encoded)

	commit := newWriter()
	commit.u8(ropCommitStream)
	commit.u8(0)
	commit.u8(3)

	save := newWriter()
	save.u8(ropSaveChangesMessage)
	save.u8(0)
	save.u8(2)
	save.u8(2)
	save.u8(0)

	if _, err := Execute(ctx, st, back, nil, ropRequest(
		openFolder.buf, openMessage.buf, openStream.buf,
		writeStream.buf, commit.buf, save.buf,
	)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, err := svc.GetCard(ctx, "alice@example.com", book.ID, card.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if got.Notes != notes {
		t.Fatalf("notes = %q, want %q", got.Notes, notes)
	}
}

func TestContactFolderIsInTheHierarchyAndOpens(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	seedRopUser(t, db, "alice@example.com")
	svc := contacts.New(db, nil)
	book, err := svc.EnsurePersonal(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("EnsurePersonal: %v", err)
	}
	if _, err := svc.CreateCard(ctx, "alice@example.com", book.ID, contacts.Input{
		DisplayName: "Alice Example",
		Emails:      []contacts.Email{{Type: "work", Value: "alice@example.com"}},
	}); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	r, err := newFolderResolver(ctx, &Backend{
		DB:        db,
		Mailboxes: imapd.NewMailboxRepo(),
		Contacts:  svc,
	}, "alice@example.com")
	if err != nil {
		t.Fatalf("newFolderResolver: %v", err)
	}
	rows, err := r.children(&folderObj{value: PRIVATE_FID_IPMSUBTREE})
	if err != nil {
		t.Fatalf("children: %v", err)
	}
	var found *folderObj
	for _, row := range rows {
		if row.props[prDisplayName] == book.Name {
			// The hierarchy row's entry id is the folder's eid.
			fid := uint64(0)
			if eid, ok := row.props[prEntryID].([]byte); ok && len(eid) == 46 {
				for i := 0; i < 6; i++ {
					fid |= uint64(eid[38+i]) << (8 * uint(i))
				}
			}
			fo, err := r.open(fidFromValue(fid))
			if err != nil {
				t.Fatalf("open(%#x): %v", fid, err)
			}
			found = fo
		}
	}
	if found == nil || found.book == nil {
		t.Fatalf("contact folder %q not found in hierarchy rows %+v", book.Name, rows)
	}
	folderProps := r.folderPropsOf(found)
	if folderProps[prContainerClass] != "IPF.Contact" {
		t.Errorf("container class = %v, want IPF.Contact", folderProps[prContainerClass])
	}
	if folderProps[prFolderType] != int32(folderGeneric) {
		t.Errorf("folder type = %v, want generic", folderProps[prFolderType])
	}
	recs, err := r.tableRecords(&sessionState{}, found, false)
	if err != nil {
		t.Fatalf("tableRecords: %v", err)
	}
	if len(recs) != 1 || recs[0].props[prDisplayName] != "Alice Example" {
		t.Fatalf("contact rows = %+v", recs)
	}

	direct, err := r.open(fidFromValue(PRIVATE_FID_CONTACTS))
	if err != nil || direct.book == nil {
		t.Fatalf("open personal contact folder = %+v, %v", direct, err)
	}
}

package rop

// MAPI contact items. Outlook's Contacts folder is not a mail folder: its
// items are vCards in the address-book store, with the display columns
// MS-OXPROPS defines for IPM.Contact and with the e-mail addresses beyond the
// first carried in the PSETID_Address named properties.
//
// The mapping here is deliberately the same one CardDAV and the web client
// use: a contact item is a contacts.Entry, so a change made in Outlook is a
// change the web client and a phone's CardDAV account see immediately.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/contacts"
)

// The PSETID_Address LIDs for the three e-mail slots (MS-OXPROPS 2.2.1.1).
// Each slot has a display name, an address type, an address and an original
// display name.
var addressEmailSlots = [3]struct {
	display  uint32
	addrType uint32
	email    uint32
	original uint32
}{
	{0x8080, 0x8082, 0x8083, 0x8084},
	{0x8090, 0x8092, 0x8093, 0x8094},
	{0x80A0, 0x80A2, 0x80A3, 0x80A4},
}

func namedKey(pn propName) string {
	return fmt.Sprintf("%d|%s|%08x|%s", pn.Kind, strings.ToLower(pn.GUID), pn.LID, pn.Name)
}

// assignNamed returns the stable id for a named property, allocating one the
// first time the client asks. The range starts at 0x9000: 0x8000-0x8FFF is
// shared with fixed tags such as PR_FILE_UNDER (0x8005), and a named id that
// collided with one of those would be read back as the wrong property.
func (st *sessionState) assignNamed(pn propName) uint16 {
	if st.namedIDs == nil {
		st.namedIDs = map[string]uint16{}
		st.namedNames = map[uint16]propName{}
		st.nextNamed = 0x9000
		// Contact e-mail addresses are the named properties Outlook always
		// needs for a contact folder. Register their ids up front so a
		// contact row can carry them even when the client has not asked for
		// the ids yet, and so RopGetPropertiesAll can report them.
		for _, slot := range addressEmailSlots {
			for _, lid := range []uint32{slot.display, slot.addrType, slot.email, slot.original} {
				st.assignNamed(propName{Kind: 0, GUID: psetidAddress, LID: lid})
			}
		}
	}
	if st.nextNamed < 0x9000 {
		st.nextNamed = 0x9000
	}
	key := namedKey(pn)
	if id, ok := st.namedIDs[key]; ok {
		return id
	}
	id := st.nextNamed
	for id < 0xFFFE {
		if _, used := st.namedNames[id]; !used {
			break
		}
		id++
	}
	st.namedIDs[key] = id
	st.namedNames[id] = pn
	st.nextNamed = id + 1
	return id
}

// namedIDFor finds the id the client already learned for a named property,
// without allocating one. A property the client never asked about is not
// included in a row, which is exactly what it expects.
func (st *sessionState) namedIDFor(guid string, lid uint32) (uint16, bool) {
	if st.namedIDs == nil {
		return 0, false
	}
	id, ok := st.namedIDs[namedKey(propName{Kind: 0, GUID: guid, LID: lid})]
	return id, ok
}

// contactLID maps a property tag back to a PSETID_Address LID when the tag is
// one of the named ids this session handed out.
func (st *sessionState) contactLID(tag uint32) (uint32, bool) {
	if st.namedNames == nil {
		return 0, false
	}
	pn, ok := st.namedNames[uint16(tag>>16)]
	if !ok || pn.Kind != 0 || !strings.EqualFold(pn.GUID, psetidAddress) {
		return 0, false
	}
	return pn.LID, true
}

// contactProps builds the property set of one contact item.
func (r *folderResolver) contactProps(st *sessionState, e contacts.Entry, folderValue uint64) map[uint32]any {
	props := map[uint32]any{
		pidTagFolderID:      uint64(fidFromValue(folderValue)),
		prObjectType:        int32(5), // MAPI_MESSAGE
		prMessageClass:      "IPM.Contact",
		prMessageClassA:     "IPM.Contact",
		prDisplayName:       e.DisplayName,
		prSubject:           e.DisplayName,
		prNormalizedSubject: e.DisplayName,
		prFileUnder:         e.DisplayName,
		prMessageFlags:      int32(msgFlagRead),
		prRead:              true,
		prMessageSize:       int32(len(e.VCard)),
		prHasAttachments:    false,
		prImportance:        int32(1),
		prSensitivity:       int32(0),
		prAddrType:          "SMTP",
	}
	if !e.CreatedAt.IsZero() {
		props[prCreationTime] = e.CreatedAt
		props[prClientSubmitTime] = e.CreatedAt
	}
	if !e.UpdatedAt.IsZero() {
		props[prLastModificationTime] = e.UpdatedAt
	}
	if e.ID != 0 {
		eid := r.messageEntryID(folderValue, uint64(e.ID))
		props[pidTagMID] = uint64(fidFromValue(uint64(e.ID)))
		props[prEntryID] = eid
		props[prRecordKey] = eid
		props[prSearchKey] = eid
	}

	if e.FirstName != "" {
		props[prGivenName] = e.FirstName
	}
	if e.LastName != "" {
		props[prSurname] = e.LastName
	}
	if e.Organization != "" {
		props[prCompanyName] = e.Organization
	}
	if e.Department != "" {
		props[prDepartmentName] = e.Department
	}
	if e.Title != "" {
		props[prTitle] = e.Title
	}
	if e.Website != "" {
		props[prBusinessHomePage] = e.Website
	}
	if e.Notes != "" {
		props[prBody] = e.Notes
	}
	if e.DisplayName != "" {
		props[prDisplayName] = e.DisplayName
	}

	if born, ok := parseContactDate(e.Birthday); ok {
		props[prBirthday] = born
	}

	for i, email := range e.Emails {
		address := strings.TrimSpace(email.Value)
		if address == "" {
			continue
		}
		if i == 0 {
			props[prEmailAddress] = address
			props[prSmtpAddress] = address
		}
		if i >= len(addressEmailSlots) {
			continue
		}
		slot := addressEmailSlots[i]
		display := e.DisplayName
		if i > 0 || display == "" {
			display = address
		}
		setNamed := func(lid uint32, value string) {
			if id, ok := st.namedIDFor(psetidAddress, lid); ok {
				props[propTag(id, ptUnicode)] = value
			}
		}
		setNamed(slot.display, display)
		setNamed(slot.addrType, "SMTP")
		setNamed(slot.email, address)
		setNamed(slot.original, address)
	}

	for _, phone := range e.Phones {
		applyPhoneProp(props, phone.Type, phone.Value)
	}
	if primary, ok := props[prBusinessTelephoneNumber]; ok {
		props[prPrimaryTelephoneNumber] = primary
	}

	for _, postal := range e.Addresses {
		applyPostalProps(props, postal)
	}
	return props
}

func applyPhoneProp(props map[uint32]any, typ, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "", "voice", "work":
		if _, used := props[prBusinessTelephoneNumber]; !used {
			props[prBusinessTelephoneNumber] = value
		} else {
			props[prBusinessTelephone2] = value
		}
	case "work2":
		props[prBusinessTelephone2] = value
	case "home":
		props[prHomeTelephoneNumber] = value
	case "home2":
		props[prHomeTelephone2] = value
	case "cell", "mobile":
		props[prMobileTelephoneNumber] = value
	case "workfax", "fax":
		props[prBusinessFaxNumber] = value
	case "homefax":
		props[prHomeFaxNumber] = value
	case "pager":
		props[prPagerTelephoneNumber] = value
	case "car":
		props[prCarTelephoneNumber] = value
	}
}

func applyPostalProps(props map[uint32]any, postal contacts.Postal) {
	home := strings.EqualFold(postal.Type, "home")
	if home {
		setIfNotEmpty(props, prHomeAddressStreet, postal.Street)
		setIfNotEmpty(props, prHomeAddressCity, postal.City)
		setIfNotEmpty(props, prHomeAddressState, postal.Region)
		setIfNotEmpty(props, prHomeAddressPostalCode, postal.PostalCode)
		setIfNotEmpty(props, prHomeAddressCountry, postal.Country)
		return
	}
	setIfNotEmpty(props, prBusinessAddressStreet, postal.Street)
	setIfNotEmpty(props, prBusinessAddressCity, postal.City)
	setIfNotEmpty(props, prBusinessAddressState, postal.Region)
	setIfNotEmpty(props, prBusinessAddressPostalCode, postal.PostalCode)
	setIfNotEmpty(props, prBusinessAddressCountry, postal.Country)
}

func setIfNotEmpty(props map[uint32]any, tag uint32, value string) {
	if value = strings.TrimSpace(value); value != "" {
		props[tag] = value
	}
}

func parseContactDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	// vCard 3.0 uses YYYY-MM-DD, and a birthday may omit the year.
	if strings.HasPrefix(value, "--") && len(value) == 7 {
		value = "1900" + value[1:]
	}
	for _, layout := range []string{"2006-01-02", "20060102", "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// entryToInput turns a stored card into the form the contacts service edits.
func entryToInput(e contacts.Entry) contacts.Input {
	return contacts.Input{
		DisplayName:  e.DisplayName,
		FirstName:    e.FirstName,
		LastName:     e.LastName,
		Emails:       append([]contacts.Email(nil), e.Emails...),
		Phones:       append([]contacts.Phone(nil), e.Phones...),
		Organization: e.Organization,
		Department:   e.Department,
		Title:        e.Title,
		Addresses:    append([]contacts.Postal(nil), e.Addresses...),
		Website:      e.Website,
		Notes:        e.Notes,
		Birthday:     e.Birthday,
	}
}

// saveContact writes the pending property changes of a contact item. A card
// that has not been created yet is created; an existing one is updated in
// place, keeping its UID so CardDAV clients see the same resource.
func (r *folderResolver) saveContact(ctx context.Context, st *sessionState, owner string, m *messageObj) (contacts.Entry, error) {
	if m.contact != nil && !m.newContact && len(m.pending) == 0 && len(m.deleted) == 0 {
		// A client that saves an untouched item must not bump its
		// modification time: that would make the folder look changed on
		// every synchronization pass.
		return *m.contact, nil
	}
	in := contacts.Input{}
	if m.contact != nil {
		in = entryToInput(*m.contact)
	}
	for tag, value := range m.pending {
		switch tag {
		case prDisplayName, prSubject, prNormalizedSubject, prEmailAddress, prSmtpAddress:
			// Folded below in a deterministic order: these properties are
			// aliases for the same field, so map order must not decide.
			continue
		}
		applyContactProp(st, &in, tag, value)
	}
	for _, tag := range []uint32{prDisplayName, prSubject, prNormalizedSubject} {
		if value, ok := m.pending[tag]; ok {
			in.DisplayName = strings.TrimSpace(stringValue(value))
			break
		}
	}
	for tag := range m.deleted {
		switch tag {
		case prDisplayName, prSubject, prNormalizedSubject, prEmailAddress, prSmtpAddress:
			continue
		}
		applyContactProp(st, &in, tag, nil)
	}
	// The primary address is written as both PR_EMAIL_ADDRESS and
	// PR_SMTP_ADDRESS. Folding the two together here keeps the result
	// independent of map iteration order; either one may be the one the
	// client set.
	emailSet, emailCleared := false, false
	if v, ok := m.pending[prSmtpAddress]; ok {
		if address := stringValue(v); address != "" {
			emailSet = true
			setEmailAt(&in, 0, address)
		} else {
			emailCleared = true
		}
	}
	if v, ok := m.pending[prEmailAddress]; ok {
		if address := stringValue(v); address != "" {
			emailSet, emailCleared = true, false
			setEmailAt(&in, 0, address)
		} else {
			emailCleared = true
		}
	}
	if !emailSet && (emailCleared || m.deleted[prEmailAddress] || m.deleted[prSmtpAddress]) {
		setEmailAt(&in, 0, "")
	}
	if strings.TrimSpace(in.DisplayName) == "" {
		if name := strings.TrimSpace(strings.TrimSpace(in.FirstName) + " " + strings.TrimSpace(in.LastName)); name != "" {
			in.DisplayName = name
		}
	}
	if strings.TrimSpace(in.DisplayName) == "" && !hasAddress(&in) {
		in.DisplayName = "Unnamed contact"
	}
	if m.newContact || m.contact == nil {
		return r.back.Contacts.CreateCard(ctx, owner, m.bookID, in)
	}
	// An Outlook edit must not throw away a photo, categories or an X-
	// property a phone's CardDAV client put on the same card.
	return r.back.Contacts.UpdateCardPreserving(ctx, owner, m.bookID, m.contact.ID, in)
}

func hasAddress(in *contacts.Input) bool {
	for _, email := range in.Emails {
		if strings.TrimSpace(email.Value) != "" {
			return true
		}
	}
	return false
}

// applyContactProp folds one MAPI property into the editable card. A nil
// value is a deletion, which is how RopDeleteProperties and a PT_NULL value
// arrive here.
func applyContactProp(st *sessionState, in *contacts.Input, tag uint32, value any) {
	if lid, ok := st.contactLID(tag); ok {
		address := stringValue(value)
		for i, slot := range addressEmailSlots {
			if lid == slot.email {
				setEmailAt(in, i, address)
				return
			}
		}
		return
	}
	text := stringValue(value)
	switch propID(tag) {
	case propID(prDisplayName), propID(prSubject), propID(prNormalizedSubject):
		in.DisplayName = strings.TrimSpace(text)
	case propID(prGivenName):
		in.FirstName = strings.TrimSpace(text)
	case propID(prSurname):
		in.LastName = strings.TrimSpace(text)
	case propID(prCompanyName):
		in.Organization = strings.TrimSpace(text)
	case propID(prDepartmentName):
		in.Department = strings.TrimSpace(text)
	case propID(prTitle):
		in.Title = strings.TrimSpace(text)
	case propID(prBody):
		in.Notes = text
	case propID(prBusinessHomePage):
		in.Website = strings.TrimSpace(text)
	case propID(prBirthday):
		switch v := value.(type) {
		case time.Time:
			if v.IsZero() {
				in.Birthday = ""
			} else {
				in.Birthday = v.Format("2006-01-02")
			}
		default:
			in.Birthday = strings.TrimSpace(text)
		}
	case propID(prBusinessTelephoneNumber):
		setPhone(in, "work", text)
	case propID(prBusinessTelephone2):
		setPhone(in, "work2", text)
	case propID(prHomeTelephoneNumber):
		setPhone(in, "home", text)
	case propID(prHomeTelephone2):
		setPhone(in, "home2", text)
	case propID(prMobileTelephoneNumber):
		setPhone(in, "cell", text)
	case propID(prBusinessFaxNumber):
		setPhone(in, "workfax", text)
	case propID(prHomeFaxNumber):
		setPhone(in, "homefax", text)
	case propID(prPagerTelephoneNumber):
		setPhone(in, "pager", text)
	case propID(prCarTelephoneNumber):
		setPhone(in, "car", text)
	case propID(prPrimaryTelephoneNumber):
		if strings.TrimSpace(text) != "" {
			setPhone(in, "work", text)
		}
	case propID(prEmailAddress), propID(prSmtpAddress):
		// Handled as a pair by saveContact so one cannot erase the other
		// depending on map iteration order.
	case propID(prBusinessAddressStreet):
		setPostal(in, "work", "street", text)
	case propID(prBusinessAddressCity):
		setPostal(in, "work", "city", text)
	case propID(prBusinessAddressState):
		setPostal(in, "work", "region", text)
	case propID(prBusinessAddressPostalCode):
		setPostal(in, "work", "postalCode", text)
	case propID(prBusinessAddressCountry):
		setPostal(in, "work", "country", text)
	case propID(prHomeAddressStreet):
		setPostal(in, "home", "street", text)
	case propID(prHomeAddressCity):
		setPostal(in, "home", "city", text)
	case propID(prHomeAddressState):
		setPostal(in, "home", "region", text)
	case propID(prHomeAddressPostalCode):
		setPostal(in, "home", "postalCode", text)
	case propID(prHomeAddressCountry):
		setPostal(in, "home", "country", text)
	}
}

func setEmailAt(in *contacts.Input, index int, value string) {
	value = strings.TrimSpace(value)
	for len(in.Emails) <= index {
		in.Emails = append(in.Emails, contacts.Email{})
	}
	in.Emails[index].Value = value
	if value != "" && in.Emails[index].Type == "" {
		in.Emails[index].Type = "work"
	}
}

func setPhone(in *contacts.Input, typ, value string) {
	value = strings.TrimSpace(value)
	out := in.Phones[:0]
	for _, phone := range in.Phones {
		if strings.EqualFold(phone.Type, typ) {
			continue
		}
		out = append(out, phone)
	}
	in.Phones = out
	if value != "" {
		in.Phones = append(in.Phones, contacts.Phone{Type: typ, Value: value})
	}
}

func setPostal(in *contacts.Input, typ, field, value string) {
	value = strings.TrimSpace(value)
	for i := range in.Addresses {
		if !strings.EqualFold(in.Addresses[i].Type, typ) {
			continue
		}
		switch field {
		case "street":
			in.Addresses[i].Street = value
		case "city":
			in.Addresses[i].City = value
		case "region":
			in.Addresses[i].Region = value
		case "postalCode":
			in.Addresses[i].PostalCode = value
		case "country":
			in.Addresses[i].Country = value
		}
		return
	}
	if value == "" {
		return
	}
	postal := contacts.Postal{Type: typ}
	switch field {
	case "street":
		postal.Street = value
	case "city":
		postal.City = value
	case "region":
		postal.Region = value
	case "postalCode":
		postal.PostalCode = value
	case "country":
		postal.Country = value
	}
	in.Addresses = append(in.Addresses, postal)
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

// decodePropVal reads one property value in its ROP (non-ABK) wire form.
// This is the inverse of encodePropVal for the types a client can set.
func decodePropVal(r *reader, tag uint32) (any, error) {
	switch propType(tag) {
	case ptNull, ptUnspecified:
		return nil, nil
	case ptI2:
		v, err := r.u16()
		return int32(v), err
	case ptLong, ptError:
		v, err := r.u32()
		return int32(v), err
	case ptR4:
		raw, err := r.bytes(4)
		if err != nil {
			return nil, err
		}
		return float64(math.Float32frombits(uint32(raw[0]) |
			uint32(raw[1])<<8 | uint32(raw[2])<<16 | uint32(raw[3])<<24)), nil
	case ptDouble, ptAppTime:
		raw, err := r.bytes(8)
		if err != nil {
			return nil, err
		}
		var bits uint64
		for i := 0; i < 8; i++ {
			bits |= uint64(raw[i]) << (8 * i)
		}
		return math.Float64frombits(bits), nil
	case ptBoolean:
		v, err := r.u8()
		return v != 0, err
	case ptI8, ptCurrency:
		v, err := r.u64()
		return int64(v), err
	case ptSystime:
		v, err := r.u64()
		if err != nil {
			return nil, err
		}
		return filetimeToTime(v), nil
	case ptString8:
		return r.zeroTerminated8()
	case ptUnicode:
		return r.zeroTerminated16()
	case ptClsid:
		return r.guid()
	case ptBinary:
		return r.bin()
	case ptMVString8:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, n)
		for i := uint32(0); i < n; i++ {
			s, err := r.zeroTerminated8()
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	case ptMVUnicode:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, n)
		for i := uint32(0); i < n; i++ {
			s, err := r.zeroTerminated16()
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	case ptMVBinary:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		out := make([][]byte, 0, n)
		for i := uint32(0); i < n; i++ {
			b, err := r.bin()
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		}
		return out, nil
	case ptMVLongI4:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		out := make([]uint32, 0, n)
		for i := uint32(0); i < n; i++ {
			v, err := r.u32()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case ptMVI2:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		out := make([]uint16, 0, n)
		for i := uint32(0); i < n; i++ {
			v, err := r.u16()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case ptMVI8, ptMVSystime, ptMVCurrency:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		out := make([]uint64, 0, n)
		for i := uint32(0); i < n; i++ {
			v, err := r.u64()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, formatError
}

func filetimeToTime(ticks uint64) time.Time {
	if ticks == 0 || ticks < filetimeEpochOffset {
		return time.Time{}
	}
	return time.Unix(0, int64(ticks-filetimeEpochOffset)*100).UTC()
}

// translateContactError keeps the ROP result codes meaningful to a client:
// a read-only shared book is an access denial, a vanished card is not found.
func translateContactError(err error) uint32 {
	switch {
	case err == nil:
		return ecSuccess
	case errors.Is(err, contacts.ErrDenied):
		return ecAccessDenied
	case errors.Is(err, contacts.ErrNotFound):
		return ecNotFound
	case errors.Is(err, contacts.ErrInvalid):
		return ecInvalidParam
	default:
		return ecError
	}
}

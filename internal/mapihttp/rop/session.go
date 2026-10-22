package rop

// The session holds everything a MAPI logon needs between Execute calls: the
// handle table (which the client re-sends every request), the objects the
// handles point at, and the resolved folder tree for the logged-on user. The
// folder and message data come from the same repositories EWS, EAS, and the
// web client use.

import (
	"context"
	"crypto/sha1"
	"fmt"
	"sort"
	"strings"

	"github.com/linchange/linchange/internal/contacts"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// objKind enumerates what a handle may point at.
type objKind uint8

const (
	objLogon objKind = iota
	objFolder
	objHierarchyTable
	objContentsTable
	objMessage
	objStream
)

// sessionState is the per-mailbox session, created on the first RopLogon.
type sessionState struct {
	owner      string
	loggedOn   bool
	storeGUID  string // mailbox_guid (PR_STORE_RECORD_KEY), stable per user
	nextHandle uint32
	objects    map[uint32]*mapiObject
	folders    *folderResolver // per-login, resolves fid -> mailbox or address book

	// Named-property registry. Outlook learns a named property's numeric id
	// through RopGetPropertyIdsFromNames and then refers to it by that id,
	// so the mapping has to stay stable for the life of the session (and be
	// reversible for RopGetNamesFromPropertyIds).
	namedIDs   map[string]uint16
	namedNames map[uint16]propName
	nextNamed  uint16
}

type mapiObject struct {
	kind    objKind
	folder  *folderObj
	table   *tableObj
	message *messageObj
	stream  *streamObj
}

// folderObj is one folder the client can hold a handle to. A pseudo folder
// (root, IPM subtree, outbox) has no mailbox row behind it.
type folderObj struct {
	value   uint64 // the value here of the eid
	fid     uint64 // full eid: replid(1)<<48 | value
	parent  uint64
	name    string
	pseudo  bool // root / IPM subtree / outbox
	mailbox *imapd.Mailbox
	book    *store.AddressBookAccess // contact folder, when this is an address book
}

// tableObj is an open hierarchy or contents table.
type tableObj struct {
	hierarchy bool
	folder    *folderObj
	columns   []uint32
	records   []*tableRecord
	position  int
	bookmarks map[uint32]int
	nextBM    uint32
}

type tableRecord struct {
	props map[uint32]any
}

// messageObj is an open message. The heavy detail (bodies, recipients) is
// loaded lazily the first time a property or stream asks for it.
type messageObj struct {
	id          int64
	folderValue uint64
	row         *imapd.Message
	detail      *webmail.MessageDetail // lazily filled
	err         error                  // lazy-load error

	// Contact items are not mail rows. They carry the address-book card
	// they stand for, and (while a new card is being composed) the
	// properties the client has set but not yet saved.
	contact    *contacts.Entry
	bookID     int64
	newContact bool
	pending    map[uint32]any
	deleted    map[uint32]bool
}

type streamObj struct {
	name  string
	data  []byte
	pos   int64
	msg   *messageObj // the message a writable body stream belongs to
	prop  uint32      // the property tag the stream stands for
	dirty bool
}

// folderResolver maps eid values onto the owner's mailboxes.
type folderResolver struct {
	back   *Backend
	owner  string
	ctx    context.Context
	byVal  map[uint64]*imapd.Mailbox // custom zone fid value -> mailbox
	byMail map[int64]*imapd.Mailbox

	// Address books are folders too. The personal book takes the standard
	// PRIVATE_FID_CONTACTS value so Outlook finds it without enumerating,
	// and every other book gets a value in the contact zone.
	byBook     map[uint64]store.AddressBookAccess
	bookValues map[int64]uint64
}

// StoreGUID returns the stable store GUID for an owner. It is derived from the
// address so the same mailbox answers with the same entryids at every logon.
func (b *Backend) StoreGUID(owner string) string {
	sum := sha1.Sum([]byte(owner))
	raw := sum[:16]
	raw[6] = raw[6]&0x0F | 0x40 // version 4
	raw[8] = raw[8]&0x3F | 0x80 // variant
	return guidToString(raw)
}

func (st *sessionState) newHandle() uint32 {
	st.nextHandle++
	return st.nextHandle
}

func (st *sessionState) put(h uint32, o *mapiObject) {
	if st.objects == nil {
		st.objects = map[uint32]*mapiObject{}
	}
	st.objects[h] = o
}

func (st *sessionState) get(h uint32) *mapiObject {
	if st.objects == nil {
		return nil
	}
	return st.objects[h]
}

func (st *sessionState) drop(h uint32) {
	delete(st.objects, h)
}

// logonFolderEIDs are the standard 13 private-store folder eids Outlook
// expects, in the exact order of the LOGON_PMB_RESPONSE (MS-OXCSTOR).
var logonFolderEIDs = []uint64{
	fidFromValue(PRIVATE_FID_ROOT),
	fidFromValue(PRIVATE_FID_DEFERRED_ACTION),
	fidFromValue(PRIVATE_FID_SPOOLER_QUEUE),
	fidFromValue(PRIVATE_FID_IPMSUBTREE),
	fidFromValue(PRIVATE_FID_INBOX),
	fidFromValue(PRIVATE_FID_OUTBOX),
	fidFromValue(PRIVATE_FID_SENT_ITEMS),
	fidFromValue(PRIVATE_FID_DELETED_ITEMS),
	fidFromValue(PRIVATE_FID_COMMON_VIEWS),
	fidFromValue(PRIVATE_FID_SCHEDULE),
	fidFromValue(PRIVATE_FID_FINDER),
	fidFromValue(PRIVATE_FID_VIEWS),
	fidFromValue(PRIVATE_FID_SHORTCUTS),
}

// PRIVATE_FID well-known values (MS-OXFOLD / gromox mapi_types.hpp).
const (
	PRIVATE_FID_ROOT            = 0x01
	PRIVATE_FID_DEFERRED_ACTION = 0x02
	PRIVATE_FID_SPOOLER_QUEUE   = 0x03
	PRIVATE_FID_SHORTCUTS       = 0x04
	PRIVATE_FID_FINDER          = 0x05
	PRIVATE_FID_VIEWS           = 0x06
	PRIVATE_FID_COMMON_VIEWS    = 0x07
	PRIVATE_FID_SCHEDULE        = 0x08
	PRIVATE_FID_IPMSUBTREE      = 0x09
	PRIVATE_FID_SENT_ITEMS      = 0x0a
	PRIVATE_FID_DELETED_ITEMS   = 0x0b
	PRIVATE_FID_OUTBOX          = 0x0c
	PRIVATE_FID_INBOX           = 0x0d
	PRIVATE_FID_DRAFT           = 0x0e
	PRIVATE_FID_CALENDAR        = 0x0f
	PRIVATE_FID_CONTACTS        = 0x10
	PRIVATE_FID_JUNK            = 0x17

	// customZoneBase is where real mailbox rows are assigned their eid value
	// (CUSTOM_EID_BEGIN in gromox): fid value = 0x100 + mailbox ID.
	customZoneBase = 0x100
	// contactZoneBase is where address books other than the personal one get
	// their eid value. It is well above the mailbox zone so the two id
	// spaces cannot collide at realistic mailbox counts.
	contactZoneBase = 0x100000
)

// PR_FOLDER_TYPE values (MS-OXCFOLD 2.2.2.2.1).
const (
	folderRoot    = 0x0
	folderGeneric = 0x1
	folderSearch  = 0x2
)

func fidFromValue(v uint64) uint64 { return uint64(1)<<48 | v }

// newFolderResolver loads the owner's mailboxes once.
func newFolderResolver(ctx context.Context, back *Backend, owner string) (*folderResolver, error) {
	boxes, err := back.Mailboxes.ListForUser(ctx, back.DB, owner)
	if err != nil {
		return nil, err
	}
	r := &folderResolver{
		back:       back,
		owner:      owner,
		ctx:        ctx,
		byVal:      map[uint64]*imapd.Mailbox{},
		byMail:     map[int64]*imapd.Mailbox{},
		byBook:     map[uint64]store.AddressBookAccess{},
		bookValues: map[int64]uint64{},
	}
	for i := range boxes {
		mb := boxes[i]
		r.byMail[mb.ID] = &mb
		r.byVal[customZoneBase+uint64(mb.ID)] = &mb
	}
	if back.Contacts != nil {
		books, err := back.Contacts.Books(ctx, owner)
		if err != nil {
			return nil, err
		}
		for i := range books {
			book := books[i]
			// Books() reports organizational books as read for everyone;
			// Book() asks the same access rule a write will use, so an
			// administrator gets the write level the store actually grants.
			if access, err := back.Contacts.Book(ctx, owner, book.ID); err == nil {
				book = access
			}
			value := contactZoneBase + uint64(book.ID)
			if book.Scope == store.AddressBookPersonal && strings.EqualFold(book.Owner, owner) {
				value = PRIVATE_FID_CONTACTS
			}
			r.byBook[value] = book
			r.bookValues[book.ID] = value
		}
	}
	return r, nil
}

// wellKnown resolves a private (replid 1) folder value to its mailbox, or nil
// for a pseudo folder. ok is false when the value is unknown entirely.
func (r *folderResolver) wellKnown(value uint64) (*imapd.Mailbox, bool) {
	switch value {
	case PRIVATE_FID_ROOT, PRIVATE_FID_IPMSUBTREE, PRIVATE_FID_OUTBOX,
		PRIVATE_FID_DEFERRED_ACTION, PRIVATE_FID_SPOOLER_QUEUE,
		PRIVATE_FID_SHORTCUTS, PRIVATE_FID_FINDER, PRIVATE_FID_VIEWS,
		PRIVATE_FID_COMMON_VIEWS, PRIVATE_FID_SCHEDULE:
		return nil, true // pseudo folder
	case PRIVATE_FID_INBOX:
		for _, mb := range r.byMail {
			if mb.Name == "INBOX" {
				return mb, true
			}
		}
		return nil, false
	case PRIVATE_FID_SENT_ITEMS:
		return r.bySpecial(`\sent`), true
	case PRIVATE_FID_DELETED_ITEMS:
		return r.bySpecial(`\trash`), true
	case PRIVATE_FID_DRAFT:
		return r.bySpecial(`\drafts`), true
	case PRIVATE_FID_JUNK:
		return r.bySpecial(`\junk`), true
	case PRIVATE_FID_CALENDAR:
		return r.bySpecial(`\calendar`), true
	}
	return nil, false
}

func (r *folderResolver) bySpecial(s string) *imapd.Mailbox {
	for _, mb := range r.byMail {
		if mb.SpecialUse != nil && *mb.SpecialUse == s {
			return mb
		}
	}
	return nil
}

// open resolves an eid (replid<<48|value) to a folder object.
func (r *folderResolver) open(fid uint64) (*folderObj, error) {
	value := fid & 0x0000FFFFFFFFFFFF
	if book, ok := r.byBook[value]; ok {
		b := book
		return &folderObj{value: value, fid: fid, name: b.Name, book: &b}, nil
	}
	if value >= customZoneBase {
		mb, ok := r.byVal[value]
		if !ok {
			return nil, errNotFound
		}
		return &folderObj{value: value, fid: fid, name: mb.Name, mailbox: mb}, nil
	}
	mb, ok := r.wellKnown(value)
	if !ok {
		return nil, errNotFound
	}
	fo := &folderObj{value: value, fid: fid, pseudo: mb == nil}
	if mb != nil {
		fo.name = mb.Name
		fo.mailbox = mb
	} else {
		fo.name = pseudoName(value)
	}
	return fo, nil
}

// pseudoName gives the hidden special folders their conventional names.
func pseudoName(value uint64) string {
	switch value {
	case PRIVATE_FID_ROOT:
		return "Root - "
	case PRIVATE_FID_IPMSUBTREE:
		return "IPM_SUBTREE"
	case PRIVATE_FID_OUTBOX:
		return "Outbox"
	case PRIVATE_FID_DEFERRED_ACTION:
		return "Deferred Action"
	case PRIVATE_FID_FINDER:
		return "Finder"
	case PRIVATE_FID_VIEWS:
		return "Views"
	case PRIVATE_FID_COMMON_VIEWS:
		return "Common Views"
	case PRIVATE_FID_SCHEDULE:
		return "Schedule"
	case PRIVATE_FID_SHORTCUTS:
		return "Shortcuts"
	case PRIVATE_FID_SPOOLER_QUEUE:
		return "Spooler Queue"
	}
	return ""
}

// children returns the hierarchy rows of a folder, in display order.
func (r *folderResolver) children(fo *folderObj) ([]tableRecord, error) {
	stats, _ := r.back.Mailboxes.FolderStats(r.ctx, r.back.DB, r.owner)
	switch fo.value {
	case PRIVATE_FID_ROOT:
		sub := &folderObj{value: PRIVATE_FID_IPMSUBTREE, fid: fidFromValue(PRIVATE_FID_IPMSUBTREE), name: "IPM_SUBTREE", pseudo: true}
		return []tableRecord{{props: r.folderProps(sub, fo, stats)}}, nil
	case PRIVATE_FID_IPMSUBTREE:
		return r.allMailboxes(stats)
	}
	if fo.mailbox == nil {
		return nil, nil
	}
	all, err := r.back.Mailboxes.ListForUser(r.ctx, r.back.DB, r.owner)
	if err != nil {
		return nil, err
	}
	out := []tableRecord{}
	for i := range all {
		mb := all[i]
		if mb.ParentID != nil && *mb.ParentID == fo.mailbox.ID {
			fv := customZoneBase + uint64(mb.ID)
			fc := &folderObj{value: fv, fid: fidFromValue(fv), name: mb.Name, mailbox: &mb}
			out = append(out, tableRecord{props: r.folderProps(fc, fo, stats)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return boxName(out[i]) < boxName(out[j]) })
	return out, nil
}

func boxName(t tableRecord) string {
	if s, ok := t.props[prDisplayName].(string); ok {
		return s
	}
	return ""
}

func (r *folderResolver) allMailboxes(stats map[int64]imapd.FolderCounts) ([]tableRecord, error) {
	boxes, err := r.back.Mailboxes.ListForUser(r.ctx, r.back.DB, r.owner)
	if err != nil {
		return nil, err
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].Name < boxes[j].Name })
	parent := &folderObj{value: PRIVATE_FID_IPMSUBTREE, fid: fidFromValue(PRIVATE_FID_IPMSUBTREE), name: "IPM_SUBTREE", pseudo: true}
	out := make([]tableRecord, 0, len(boxes)+len(r.byBook))
	for i := range boxes {
		mb := boxes[i]
		fv := customZoneBase + uint64(mb.ID)
		fc := &folderObj{value: fv, fid: fidFromValue(fv), name: mb.Name, mailbox: &mb}
		out = append(out, tableRecord{props: r.folderProps(fc, parent, stats)})
	}
	books := make([]store.AddressBookAccess, 0, len(r.byBook))
	for _, book := range r.byBook {
		books = append(books, book)
	}
	sort.Slice(books, func(i, j int) bool { return books[i].Name < books[j].Name })
	for i := range books {
		book := books[i]
		fv := r.bookValues[book.ID]
		fc := &folderObj{value: fv, fid: fidFromValue(fv), name: book.Name, book: &book}
		out = append(out, tableRecord{props: r.folderProps(fc, parent, stats)})
	}
	return out, nil
}

// folderProps builds the property set of one hierarchy row.
func (r *folderResolver) folderProps(fo *folderObj, parent *folderObj, stats map[int64]imapd.FolderCounts) map[uint32]any {
	props := map[uint32]any{
		pidTagFolderID: uint64(fo.fid),
		prObjectType:   int32(3), // MAPI_FOLDER
		prDisplayName:  fo.name,
		prFolderType:   int32(folderGeneric),
		prSubfolders:   false,
	}
	if fo.value == PRIVATE_FID_ROOT || fo.value == PRIVATE_FID_IPMSUBTREE {
		props[prFolderType] = int32(folderRoot)
	}
	if fo.value == PRIVATE_FID_ROOT {
		// Some clients read the special-folder entry ids from the root
		// folder rather than from the store object.
		props[prIPMContactEntryID] = r.folderEntryID(PRIVATE_FID_CONTACTS)
	}
	if fo.book != nil {
		props[prContainerClass] = "IPF.Contact"
		props[prContentCount] = int32(0)
		props[prContentUnread] = int32(0)
		props[prSubfolders] = false
		// Read-only books still appear in Outlook, but are marked so the
		// client does not offer to create or edit items there.
		access := int32(0x00000002) // MAPI_ACCESS_READ
		if fo.book.IsOwner || fo.book.Level == store.AddressBookShareWrite {
			access = 0x0000001F // modify, read, delete, create hierarchy, create contents
		}
		props[prAccess] = access
		props[prAccessLevel] = access
		if r.back.Contacts != nil {
			if n, err := r.back.Contacts.CountCards(r.ctx, r.owner, fo.book.ID); err == nil {
				props[prContentCount] = int32(n)
			}
		}
	}
	if fo.mailbox != nil {
		if c, ok := stats[fo.mailbox.ID]; ok {
			props[prContentCount] = int32(c.Total)
			props[prContentUnread] = int32(c.Unread)
		}
		props[prSubfolders] = fo.mailbox.ParentID != nil
	}
	if parent != nil {
		props[pidTagParentFolderID] = uint64(parent.fid)
		props[prParentEntryID] = r.folderEntryID(parent.value)
	}
	eid := r.folderEntryID(fo.value)
	props[prEntryID] = eid
	props[prRecordKey] = eid
	return props
}

// folderEntryID builds the 46-byte FOLDER_ENTRYID (MS-OXCDATA 2.2.4.1).
func (r *folderResolver) folderEntryID(value uint64) []byte {
	out := make([]byte, 46)
	leU32(out[0:], 0) // flags
	copy(out[4:20], guidToBytes(muidStoreWrap))
	leU16(out[20:], 0x0001) // EITLT_PRIVATE_FOLDER
	// folder_dbguid = the store guid; Outlook maps it to the logon replica.
	copy(out[22:38], guidToBytes(r.back.StoreGUID(r.owner)))
	put48(out[38:44], value)
	return out // pad 2 bytes zero
}

// messageEntryID builds the 70-byte MESSAGE_ENTRYID (MS-OXCDATA 2.2.4.2).
func (r *folderResolver) messageEntryID(folderValue, messageID uint64) []byte {
	out := make([]byte, 70)
	leU32(out[0:], 0)
	copy(out[4:20], guidToBytes(muidStoreWrap))
	leU16(out[20:], 0x0007) // EITLT_PRIVATE_MESSAGE
	copy(out[22:38], guidToBytes(r.back.StoreGUID(r.owner)))
	put48(out[38:44], folderValue)
	copy(out[46:62], guidToBytes(r.back.StoreGUID(r.owner)))
	put48(out[62:68], messageID)
	return out
}

// messageProps builds the property map of one row of a contents table.
func (r *folderResolver) messageProps(m *imapd.Message, folderValue uint64) map[uint32]any {
	flags := uint32(msgFlagOriginInternet)
	seen := false
	for _, f := range m.Flags {
		if f == `\Seen` {
			seen = true
		}
	}
	if seen {
		flags |= msgFlagRead
	}
	if m.HasAttachments {
		flags |= msgFlagHasAttach
	}
	props := map[uint32]any{
		pidTagMID:               uint64(fidFromValue(uint64(m.ID))),
		pidTagFolderID:          uint64(fidFromValue(folderValue)),
		prObjectType:            int32(5), // MAPI_MESSAGE
		prSubject:               m.Subject,
		prMessageClassA:         m.ItemClass,
		prMessageFlags:          int32(flags),
		prRead:                  seen,
		prMessageSize:           int32(m.Size),
		prMessageDeliveryTime:   m.InternalDate,
		prSenderName:            m.FromName,
		prSenderEmailAddress:    m.FromAddress,
		prSentRepresentingName:  m.FromName,
		prSentRepresentingEMail: m.FromAddress,
		prImportance:            int32(1),
		prHasAttachments:        m.HasAttachments,
		prMessageClass:          m.ItemClass,
		prInternetMessageID:     m.MessageID,
		prEntryID:               r.messageEntryID(folderValue, uint64(m.ID)),
		prRecordKey:             r.messageEntryID(folderValue, uint64(m.ID)),
		prLastModificationTime:  m.InternalDate,
		prClientSubmitTime:      m.InternalDate,
	}
	return props
}

// loadDetail fills in the heavy message properties once.
func (m *messageObj) loadDetail(ctx context.Context, back *Backend, owner string) {
	if m.detail != nil || m.err != nil {
		return
	}
	d, err := back.Mail.Message(ctx, owner, m.id)
	if err != nil {
		m.err = err
		return
	}
	m.detail = &d
}

// messageDetailProps adds body/recipient properties on top of the summary.
func (r *folderResolver) messageDetailProps(m *messageObj) map[uint32]any {
	base := r.messageProps(m.row, m.folderValue)
	if m.detail == nil {
		return base
	}
	d := m.detail
	base[prBody] = d.TextBody
	if d.HTMLBody != "" {
		base[prBodyHTML] = []byte(d.HTMLBody)
	}
	if len(d.To) > 0 {
		base[prDisplayTo] = displayList(d.To)
	}
	if len(d.Cc) > 0 {
		base[prDisplayCC] = displayList(d.Cc)
	}
	if len(d.Bcc) > 0 {
		base[prDisplayBCC] = displayList(d.Bcc)
	}
	return base
}

func displayList(addrs []webmail.Address) string {
	out := ""
	for i, a := range addrs {
		if i > 0 {
			out += "; "
		}
		out += a.String()
	}
	return out
}

// messageRow resolves a message eid to its store row.
func (r *folderResolver) messageRow(mid uint64) (*imapd.Message, error) {
	if mid == 0 {
		return nil, errNotFound
	}
	m, err := r.back.Messages.GetByID(r.ctx, r.back.DB, r.owner, int64(mid))
	if err != nil {
		return nil, errNotFound
	}
	return &m, nil
}

// folderPropsOf returns the property set of an open folder handle.
func (r *folderResolver) folderPropsOf(fo *folderObj) map[uint32]any {
	stats, _ := r.back.Mailboxes.FolderStats(r.ctx, r.back.DB, r.owner)
	return r.folderProps(fo, nil, stats)
}

// storeProps builds the properties of the store itself, used by
// GETPROPERTIES on the logon handle.
func (r *folderResolver) storeProps(owner string) map[uint32]any {
	guid := r.back.StoreGUID(owner)
	return map[uint32]any{
		prObjectType:       int32(1), // MAPI_STORE
		prDisplayName:      owner,
		prDefaultStore:     true,
		prStoreSupportMask: int32(0x0007FFFF),
		prStoreEntryID:     wrappedStoreEntryID(guid),
		prMdbProvider:      guidToBytes(muidStoreWrap),
		prRecordKey:        guidToBytes(guid),
		prEntryID:          wrappedStoreEntryID(guid),
		prSearchKey:        guidToBytes(guid),
		prSmtpAddress:      owner,
		// Tell Outlook where the Contacts folder is, even before it walks
		// the IPM subtree. The personal book is mapped to the standard
		// PRIVATE_FID_CONTACTS value, so this is a stable entry id.
		prIPMContactEntryID: r.folderEntryID(PRIVATE_FID_CONTACTS),
	}
}

func leU16(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}

func leU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func put48(b []byte, v uint64) {
	for i := 0; i < 6; i++ {
		b[i] = byte(v >> (8 * i))
	}
}

// wrappedStoreEntryID builds a STORE_ENTRYID with empty server and mailbox DN,
// which is what a private store with no alternate server looks like.
func wrappedStoreEntryID(guid string) []byte {
	out := make([]byte, 46)
	leU32(out[0:], 0)                           // flags
	leU16(out[4:], 0)                           // version = 0
	copy(out[6:22], guidToBytes(muidStoreWrap)) // provider_uid
	leU32(out[22:], 0)                          // wrapped_flags
	copy(out[26:42], guidToBytes(guid))         // wrapped_provider_uid
	// wrapped_type (4 bytes zero) + a single terminating NUL for the empty
	// server name / mailbox DN.
	return out
}

var errNotFound = fmt.Errorf("mapi/rop: not found")

// tableRecords materializes a table once, at creation time.
func (r *folderResolver) tableRecords(st *sessionState, fo *folderObj, hierarchy bool) ([]*tableRecord, error) {
	if hierarchy {
		recs, err := r.children(fo)
		if err != nil {
			return nil, err
		}
		out := make([]*tableRecord, 0, len(recs))
		for i := range recs {
			rec := recs[i]
			out = append(out, &rec)
		}
		return out, nil
	}
	if fo.book != nil {
		entries, err := r.back.Contacts.Cards(r.ctx, r.owner, fo.book.ID)
		if err != nil {
			return nil, err
		}
		out := make([]*tableRecord, 0, len(entries))
		for i := range entries {
			out = append(out, &tableRecord{props: r.contactProps(st, entries[i], fo.value)})
		}
		return out, nil
	}
	if fo.mailbox == nil {
		return nil, nil
	}
	messages, err := r.back.Messages.GetAllMessages(r.ctx, r.back.DB, fo.mailbox.ID)
	if err != nil {
		return nil, err
	}
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].InternalDate.Equal(messages[j].InternalDate) {
			return messages[i].ID > messages[j].ID
		}
		return messages[i].InternalDate.After(messages[j].InternalDate)
	})
	out := make([]*tableRecord, 0, len(messages))
	for i := range messages {
		m := messages[i]
		out = append(out, &tableRecord{props: r.messageProps(&m, fo.value)})
	}
	return out, nil
}

// contentsDefaultColumns is the default column set for contents tables when the
// client queries rows before SetColumns.
var contentsDefaultColumns = []uint32{
	pidTagMID, prObjectType, prSubject, prMessageClassA, prMessageFlags,
	prRead, prMessageSize, prMessageDeliveryTime, prSenderName,
	prSenderEmailAddress, prHasAttachments, prInternetMessageID,
}

// contactDefaultColumns is what a contact folder returns before the client
// chooses columns. Outlook almost always sets its own list, but a client that
// does not still gets the fields a contact list is made of.
var contactDefaultColumns = []uint32{
	pidTagMID, prObjectType, prEntryID, prDisplayName, prSubject,
	prMessageClass, prEmailAddress, prAddrType, prSmtpAddress,
	prCompanyName, prTitle, prDepartmentName,
	prBusinessTelephoneNumber, prHomeTelephoneNumber, prMobileTelephoneNumber,
	prLastModificationTime, prMessageFlags, prRead,
}

var hierarchyDefaultColumns = []uint32{
	pidTagFolderID, pidTagParentFolderID, prObjectType, prDisplayName,
	prFolderType, prSubfolders, prContentCount, prContentUnread, prEntryID,
}

// defaultColumns picks the right default set for a table.
func (t *tableObj) defaultColumns() []uint32 {
	if t.hierarchy {
		return hierarchyDefaultColumns
	}
	if t.folder != nil && t.folder.book != nil {
		return contactDefaultColumns
	}
	return contentsDefaultColumns
}

// bodyStream returns the byte content for a message body property in its wire
// encoding, and errNotFound when the message has no such body.
func (m *messageObj) bodyContent(ctx context.Context, back *Backend, owner, prop string) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.detail == nil {
		return nil, errNotFound
	}
	switch prop {
	case "body":
		if m.detail.TextBody == "" {
			return nil, errNotFound
		}
		return utf16LE(m.detail.TextBody), nil
	case "body_html":
		if m.detail.HTMLBody == "" {
			return nil, errNotFound
		}
		return []byte(m.detail.HTMLBody), nil
	}
	return nil, errNotFound
}

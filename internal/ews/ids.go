package ews

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// Identifier encoding.
//
// A client never interprets an identifier: it stores it, sends it back, and
// compares it to decide whether something has changed. What it does insist on is
// that the encoding is opaque and fixed-length, because Exchange clients embed
// these strings in XML attributes, in URIs, and in their local caches, and an
// identifier that is a different length depending on what it names is one that
// breaks a cache keyed on it.
//
// The layout is fixed and documented here rather than derived:
//
//	byte 0        the kind of object named
//	bytes 1..4    the identifier of the container, little-endian
//	bytes 5..8    the identifier of the object itself, present only for items
//	the rest      zero
//
// The kind byte is what makes a folder identifier unambiguous. A mail folder and a
// calendar folder both have a row id, and both are named by the same length of
// base64; without a kind byte a calendar identifier would be a well-formed mail
// folder identifier pointing at whichever mailbox happened to hold that id. Every
// check downstream would then be checking the caller's ownership of the wrong
// object, which is not a thing that fails safely.
const (
	// folderIDBytes is the length of a t:FolderId.
	folderIDBytes = 64
	// itemIDBytes is the length of a t:ItemId.
	itemIDBytes = 24
)

// What an identifier names.
const (
	// kindMailbox is a folder of mail.
	kindMailbox byte = 1
	// kindCalendar is a calendar.
	kindCalendar byte = 2
	// kindSharedRoot is the root of a shared mailbox: the container that stands
	// for a mailbox the caller does not own but has been granted access to. Its
	// children are that mailbox's folders, and it holds no items itself, the way
	// Exchange's root folder does.
	kindSharedRoot byte = 3
)

// Distinguished folder identifiers (MS-OXECWS §2.2.1.3).
//
// A client addresses a standard folder by name rather than by identifier, and the
// point of the vocabulary is that the name survives the server being replaced: the
// identifier of the Sent Items folder is not the same in two installations, and a
// client that cached one would have nothing to sync against.
const (
	distRoot          = "rootfolder"
	distInbox         = "inbox"
	distOutbox        = "outbox"
	distDrafts        = "drafts"
	distSentItems     = "sentitems"
	distDeletedItems  = "deleteditems"
	distJunkEmail     = "junkemail"
	distArchive       = "archive"
	distMsgFolderRoot = "msgfolderroot"
	distPublicFolders = "publicfoldersroot"
	distSearchFolders = "searchfolders"
	distCalendar      = "calendar"
	distContacts      = "contacts"
	distTasks         = "tasks"
)

// mailFolderByDistinguished maps the EWS name of a standard mail folder onto the
// folder name this server stores it under.
//
// The two vocabularies disagree deliberately rather than by accident. Exchange's
// are stable public identifiers that cannot be renamed; ours are the IMAP names a
// user sees and can rename. Mapping them in one place is what means a rename here
// cannot break a client that speaks the protocol's names, which is the only kind
// of rename a mail client is entitled to perform.
var mailFolderByDistinguished = map[string]string{
	distRoot:          "INBOX",
	distInbox:         "INBOX",
	distOutbox:        "Sent",
	distDrafts:        "Drafts",
	distSentItems:     "Sent",
	distDeletedItems:  "Trash",
	distJunkEmail:     "Junk",
	distArchive:       "Archive",
	distMsgFolderRoot: "INBOX",
}

// mailFolderClasses names the item type a standard folder holds, which is how a
// client decides whether to offer "New message" or "New appointment" in it.
//
// The class is omitted for a folder whose class is not known rather than guessed
// at. A wrong class puts the wrong compose button under the folder's name, and
// the user finds out after they have written the thing.
var mailFolderClasses = map[string]string{
	"inbox":         "IPM.Note",
	"sent":          "IPM.Note",
	"drafts":        "IPM.Note",
	"trash":         "IPM.Note",
	"junk":          "IPM.Note",
	"archive":       "IPM.Note",
	"outbox":        "IPM.Note",
	"deleted items": "IPM.Note",
}

// encodeFolderID encodes a container identifier.
func encodeFolderID(kind byte, id int64) string {
	var raw [folderIDBytes]byte
	raw[0] = kind
	binary.LittleEndian.PutUint32(raw[1:5], uint32(id))
	return base64.StdEncoding.EncodeToString(raw[:])
}

// folderID encodes a mail folder.
func folderID(mailboxID int64) string { return encodeFolderID(kindMailbox, mailboxID) }

// calendarID encodes a calendar.
func calendarID(calendar int64) string { return encodeFolderID(kindCalendar, calendar) }

// encodeItemID encodes an item identifier.
func encodeItemID(kind byte, container, item int64) string {
	var raw [itemIDBytes]byte
	raw[0] = kind
	binary.LittleEndian.PutUint32(raw[1:5], uint32(container))
	binary.LittleEndian.PutUint32(raw[5:9], uint32(item))
	return base64.StdEncoding.EncodeToString(raw[:])
}

// messageIDString encodes a message in a mail folder.
func messageIDString(mailbox, message int64) string {
	return encodeItemID(kindMailbox, mailbox, message)
}

// eventIDString encodes an event on a calendar.
func eventIDString(calendar, event int64) string {
	return encodeItemID(kindCalendar, calendar, event)
}

// errMalformedID is the base of every "that is not an identifier" error.
var errMalformedID = fmt.Errorf("ews: malformed identifier")

// decodeBase64 reads an identifier's bytes.
//
// Padded and unpadded base64 are both accepted because a client that puts an
// identifier into a URI path strips the padding, and the one that puts it into
// an XML attribute keeps it. Refusing the unpadded form would break the client
// that works correctly rather than the one that does not.
func decodeBase64(encoded string, want int) ([]byte, error) {
	s := strings.TrimSpace(encoded)
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("%w: not base64", errMalformedID)
		}
	}
	if len(raw) != want {
		return nil, fmt.Errorf("%w: %d bytes, not %d", errMalformedID, len(raw), want)
	}
	return raw, nil
}

// decodeFolderID reads the kind and id out of a t:FolderId.
//
// A malformed identifier is refused rather than read as zero. Zero is an id that
// could exist, and answering about a container nobody named is how one client ends
// up shown another's.
func decodeFolderID(encoded string) (byte, int64, error) {
	raw, err := decodeBase64(encoded, folderIDBytes)
	if err != nil {
		return 0, 0, err
	}
	kind := raw[0]
	if kind != kindMailbox && kind != kindCalendar && kind != kindSharedRoot {
		return 0, 0, fmt.Errorf("%w: unknown folder kind %d", errMalformedID, kind)
	}
	id := int64(binary.LittleEndian.Uint32(raw[1:5]))
	if id <= 0 {
		return 0, 0, fmt.Errorf("%w: the identifier names no folder", errMalformedID)
	}
	return kind, id, nil
}

// decodeItemID reads the kind and the two ids out of a t:ItemId.
func decodeItemID(encoded string) (kind byte, container, item int64, err error) {
	raw, err := decodeBase64(encoded, itemIDBytes)
	if err != nil {
		return 0, 0, 0, err
	}
	kind = raw[0]
	if kind != kindMailbox && kind != kindCalendar {
		return 0, 0, 0, fmt.Errorf("%w: unknown item kind %d", errMalformedID, kind)
	}
	container = int64(binary.LittleEndian.Uint32(raw[1:5]))
	item = int64(binary.LittleEndian.Uint32(raw[5:9]))
	if container <= 0 || item <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: the identifier names no item", errMalformedID)
	}
	return kind, container, item, nil
}

// changeKey is the opaque token a client sends back with an update to prove which
// revision it is editing.
//
// It is derived from the state it protects rather than from a counter, because a
// counter would have to be stored per item and this server stores messages in a
// table whose columns are chosen for IMAP. It changes whenever anything a client
// can see changes: the message being moved, flagged, or replaced by another copy
// of itself.
func changeKey(parts ...any) string {
	// FNV-1a, written out rather than pulled in: it is six lines, it is not
	// security, and the only property needed here is that the same inputs give
	// the same string and any change gives a different one.
	var h uint64 = 1469598103934665603
	mix := func(b byte) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	for _, p := range parts {
		var buf [8]byte
		switch v := p.(type) {
		case int64:
			binary.LittleEndian.PutUint64(buf[:], uint64(v))
		case int:
			binary.LittleEndian.PutUint64(buf[:], uint64(v))
		case int32:
			binary.LittleEndian.PutUint64(buf[:], uint64(v))
		case uint32:
			binary.LittleEndian.PutUint64(buf[:], uint64(v))
		case bool:
			if v {
				binary.LittleEndian.PutUint64(buf[:], 1)
			}
		case []byte:
			for _, b := range v {
				mix(b)
			}
			continue
		case string:
			for i := 0; i < len(v); i++ {
				mix(v[i])
			}
			continue
		default:
			continue
		}
		for _, b := range buf {
			mix(b)
		}
	}
	// Hex, upper case, fixed width. A client that caches the value compares it
	// as a string, so it must not depend on how it happened to be formatted.
	return strings.ToUpper(strconv.FormatUint(h, 16))
}

// WBXML (Wireless Binary XML) 1.3 codec for Exchange ActiveSync, per
// MS-ASWBXML.
//
// ActiveSync speaks a fixed, small vocabulary of elements. Rather than carrying
// an XML document and then serialising it, a client encodes the document as a
// stream of single-byte tag tokens, one per element, in a code page that maps
// those tokens to element names. The result is several times smaller than the
// same document as text, which is the whole reason the protocol is binary: a
// phone synchronising a folder of mail should not be shipping kilobytes of
// angle brackets.
//
// This file is the codec both directions share. The writer turns an element
// tree the handlers build into a token stream; the decoder turns a client's
// token stream back into a tree the handlers read. The two agree on the tag
// tables below, and those tables are the contract with a real client, so the
// tokens follow the published code-page assignments rather than being invented.

package eas

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// --- Token values (MS-ASWBXML §2.2) -------------------------------------

const (
	wbxmlVersion   = 0x03 // WBXML 1.3
	wbxmlPublicID  = 0x00 // no public ID
	wbxmlCharset   = 0x6A // 106 = UTF-8
	wbxmlStrTable  = 0x00 // no string table
	wbxmlProcInstr = 0x00 // no processing instructions

	tokenSwitchPage = 0x00 // followed by a code page number
	tokenEnd        = 0x01 // end of element
	tokenEntity     = 0x02 // entity reference (unused by ActiveSync)
	tokenStrI       = 0x03 // inline string, followed by UTF-8 and a 0x00
	tokenLiteral    = 0x04 // inline literal (unused by ActiveSync)
	tokenExtI0      = 0x40 // extended tag, one extra byte
	tokenExtI1      = 0x41
	tokenExtI2      = 0x42
	tokenOpaque     = 0xC3 // 1.3 opaque data, followed by an MBUInt32 length
)

// --- Code pages ----------------------------------------------------------
//
// A code page is a namespace: a set of element names bound to token values. A
// client names an element by first switching to the code page the element lives
// in and then writing the element's token. The numbers are the published
// ActiveSync assignments; a client that sends a different number for the same
// element would be speaking a different protocol.

const (
	cpAirSync         = 0
	cpContacts        = 1
	cpEmail           = 2
	cpCalendar        = 4
	cpMove            = 5
	cpGetItemEstimate = 6
	cpFolderHierarchy = 7
	cpMeetingResponse = 8
	cpResolveRecip    = 10
	cpPing            = 13
	cpProvision       = 14
	cpSearch          = 15
	cpAirSyncBase     = 17
	cpSettings        = 18
	cpItemOperations  = 20
)

// nsByCP names each code page, so the writer can decide when to switch.
var nsByCP = map[byte]string{
	cpAirSync:         "AirSync",
	cpContacts:        "Contacts",
	cpEmail:           "Email",
	cpCalendar:        "Calendar",
	cpMove:            "Move",
	cpGetItemEstimate: "GetItemEstimate",
	cpFolderHierarchy: "FolderHierarchy",
	cpMeetingResponse: "MeetingResponse",
	cpResolveRecip:    "ResolveRecipients",
	cpPing:            "Ping",
	cpProvision:       "Provision",
	cpSearch:          "Search",
	cpAirSyncBase:     "AirSyncBase",
	cpSettings:        "Settings",
	cpItemOperations:  "ItemOperations",
}

var cpByNS = func() map[string]byte {
	m := make(map[string]byte, len(nsByCP))
	for cp, ns := range nsByCP {
		m[ns] = cp
	}
	return m
}()

// --- Tag tables ----------------------------------------------------------
//
// Each table is one code page: element name to token. The names are the local
// element names a client uses; the tokens are the published values. A name that
// appears in several code pages (Status, ServerId, Folders, ...) is listed in
// each one it is used in, because the code page a client is in decides which
// token means which name.

// AirSync (code page 0): the command roots and the elements every command
// shares -- Status, SyncKey, ServerId, the collection list, the counts.
var airSyncTags = tagTable{
	"Sync":              0x05,
	"Responses":         0x06,
	"Add":               0x07,
	"Change":            0x08,
	"Delete":            0x09,
	"Fetch":             0x0A,
	"SyncKey":           0x0B,
	"ClientId":          0x0C,
	"ServerId":          0x0D,
	"Status":            0x0E,
	"Collection":        0x0F,
	"Class":             0x10,
	"CollectionId":      0x12,
	"GetChanges":        0x13,
	"MoreAvailable":     0x14,
	"WindowSize":        0x15,
	"Commands":          0x16,
	"Options":           0x17,
	"FilterType":        0x18,
	"Truncation":        0x19,
	"Conflict":          0x1A,
	"Collections":       0x1B,
	"ApplicationData":   0x1C,
	"DeletesAsMoves":    0x1D,
	"SoftDelete":        0x1E,
	"MIMESupport":       0x1F,
	"MIMETruncation":    0x20,
	"Wait":              0x21,
	"HeartbeatInterval": 0x22,
	"Folders":           0x23,
	"Folder":            0x24,
	"Supported":         0x25,
	"Version":           0x26,
	"ProtocolVersion":   0x27,
	"PolicyKey":         0x28,
	"PolicyType":        0x29,
	"Data":              0x2A,
	"ItemEstimate":      0x2B,
	"Adds":              0x2C,
	"Deletes":           0x2D,
	"Replaces":          0x2E,
	"Replace":           0x2F,
	"Fetches":           0x30,
}

// Email (code page 2): the SendMail command and the properties of a mail item,
// which are what a Sync response carries for each message.
var emailTags = tagTable{
	"SendMail":      0x05,
	"To":            0x06,
	"Cc":            0x07,
	"Bcc":           0x08,
	"From":          0x09,
	"Subject":       0x0A,
	"DateReceived":  0x0B,
	"DateSent":      0x0C,
	"DisplayTo":     0x0D,
	"DisplayCc":     0x0E,
	"DisplayBcc":    0x0F,
	"Importance":    0x10,
	"MessageClass":  0x11,
	"Read":          0x12,
	"Attachments":   0x13,
	"Attachment":    0x14,
	"AttName":       0x15,
	"AttSize":       0x16,
	"Sender":        0x17,
	"SenderName":    0x18,
	"InternetCPID":  0x19,
	"Flag":          0x1A,
	"FlagType":      0x1B,
	"Body":          0x1C,
	"BodySize":      0x1D,
	"BodyTruncated": 0x1E,
	"Data":          0x1F,
	"MessageId":     0x20,
	"SentReplica":   0x21,
	"BodyType":      0x22,
	"Status":        0x23,
	"Conversation":  0x24,
	"Email":         0x25,
	"Address":       0x26,
	"Name":          0x27,
}

// Calendar (code page 4): the properties of an appointment, which a Sync
// response carries for each event and a MeetingResponse names.
var calendarTags = tagTable{
	"Calendar":       0x0B,
	"Timezone":       0x05,
	"AllDayEvent":    0x06,
	"Attendees":      0x07,
	"Attendee":       0x08,
	"Email":          0x09,
	"Name":           0x0A,
	"BusyStatus":     0x0D,
	"Location":       0x17,
	"MeetingStatus":  0x18,
	"OrganizerEmail": 0x19,
	"OrganizerName":  0x1A,
	"Recurrence":     0x1B,
	"Type":           0x1C,
	"Subject":        0x26,
	"StartTime":      0x27,
	"EndTime":        0x28,
	"UID":            0x29,
	"Meeting":        0x2A,
	"Data":           0x2B,
	"Status":         0x2C,
}

// Move (code page 5): the MoveItems command.
var moveTags = tagTable{
	"MoveItems":   0x05,
	"Items":       0x06,
	"Id":          0x07,
	"Destination": 0x08,
	"Status":      0x09,
	"MoveErrors":  0x0A,
	"MoveError":   0x0B,
}

// GetItemEstimate (code page 6): the GetItemEstimate command and its count.
var getItemEstimateTags = tagTable{
	"GetItemEstimate": 0x05,
	"Collections":     0x06,
	"Collection":      0x07,
	"CollectionId":    0x08,
	"Class":           0x09,
	"Estimate":        0x0A,
	"Status":          0x0B,
}

// FolderHierarchy (code page 7): the FolderSync, FolderCreate, FolderDelete and
// FolderUpdate commands, and the tree a folder sync returns.
var folderHierarchyTags = tagTable{
	"FolderSync":         0x05,
	"FolderCreate":       0x06,
	"FolderDelete":       0x07,
	"FolderUpdate":       0x08,
	"Folders":            0x09,
	"Folder":             0x0A,
	"DisplayName":        0x0B,
	"ServerId":           0x0C,
	"ParentId":           0x0D,
	"Type":               0x0E,
	"Status":             0x10,
	"Changes":            0x12,
	"Add":                0x13,
	"Delete":             0x14,
	"Update":             0x15,
	"SyncKey":            0x16,
	"Count":              0x17,
	"SubFolders":         0x18,
	"MoreAvailable":      0x19,
	"GetChanges":         0x1A,
	"MaxChangesReturned": 0x1B,
	"CollectionId":       0x1C,
}

// MeetingResponse (code page 8): the command root and the meeting it names.
var meetingResponseTags = tagTable{
	"MeetingResponse": 0x05,
	"Meeting":         0x06,
	"Attendees":       0x07,
	"Attendee":        0x08,
	"Email":           0x09,
	"Status":          0x0A,
	"Data":            0x0B,
	"Type":            0x0C,
}

// Ping (code page 13): the Ping command and the folders it reports.
var pingTags = tagTable{
	"Ping":              0x05,
	"AutdState":         0x06,
	"Status":            0x07,
	"HeartbeatInterval": 0x08,
	"Folders":           0x09,
	"Folder":            0x0A,
	"Id":                0x0B,
	"Class":             0x0C,
}

// Provision (code page 14): the Provision command and the policy it returns.
var provisionTags = tagTable{
	"Provision":  0x05,
	"Policies":   0x06,
	"Policy":     0x07,
	"PolicyType": 0x08,
	"PolicyKey":  0x09,
	"Data":       0x0A,
	"Status":     0x0E,
	"RemoteWipe": 0x0F,
	"Settings":   0x10,
}

// Search (code page 15): the Search command and the stores it lists.
var searchTags = tagTable{
	"Search":       0x05,
	"Store":        0x06,
	"Stores":       0x07,
	"DisplayName":  0x08,
	"StoreId":      0x09,
	"SearchString": 0x0A,
	"Scope":        0x0B,
	"Status":       0x0C,
}

// AirSyncBase (code page 17): the modern spelling of a body and its
// attachments, which a client negotiating 12.0 or newer uses in place of the
// older Email body elements.
var airSyncBaseTags = tagTable{
	"Body":              0x05,
	"Data":              0x06,
	"Type":              0x07,
	"Truncated":         0x08,
	"EstimatedDataSize": 0x09,
	"Attachments":       0x0A,
	"Attachment":        0x0B,
	"DisplayName":       0x0C,
	"FileReference":     0x0D,
	"Method":            0x0E,
	"ContentId":         0x0F,
	"ContentLocation":   0x10,
	"IsInline":          0x11,
	"NativeBodyType":    0x12,
	"ContentType":       0x13,
	"Preview":           0x14,
	"BodyPart":          0x15,
	"BodyPartData":      0x16,
}

// Settings (code page 18): the Settings command and the push interval it sets.
var settingsTags = tagTable{
	"Settings": 0x05,
	"Account":  0x06,
	"Get":      0x07,
	"Set":      0x08,
	"Push":     0x09,
	"Interval": 0x0A,
	"Status":   0x0B,
}

// ItemOperations (code page 20): the ItemOperations command and the per-item
// flag changes it applies.
var itemOperationsTags = tagTable{
	"ItemOperations":    0x05,
	"Operations":        0x06,
	"Operation":         0x07,
	"ServerId":          0x08,
	"ItemClass":         0x09,
	"MarkAsRead":        0x0A,
	"MarkAsUnread":      0x0B,
	"MarkAsCompleted":   0x0C,
	"MarkAsUncompleted": 0x0D,
	"Status":            0x0E,
}

// tagTable is one code page: element name to token.
type tagTable map[string]byte

// tagsByCP is the full set of code pages.
var tagsByCP = map[byte]tagTable{
	cpAirSync:         airSyncTags,
	cpEmail:           emailTags,
	cpCalendar:        calendarTags,
	cpMove:            moveTags,
	cpGetItemEstimate: getItemEstimateTags,
	cpFolderHierarchy: folderHierarchyTags,
	cpMeetingResponse: meetingResponseTags,
	cpPing:            pingTags,
	cpProvision:       provisionTags,
	cpSearch:          searchTags,
	cpAirSyncBase:     airSyncBaseTags,
	cpSettings:        settingsTags,
	cpItemOperations:  itemOperationsTags,
}

// tokenToName is the reverse of tagsByCP: code page and token to name.
var tokenToName = func() map[byte]map[byte]string {
	out := make(map[byte]map[byte]string, len(tagsByCP))
	for cp, table := range tagsByCP {
		reverse := make(map[byte]string, len(table))
		for name, token := range table {
			reverse[token] = name
		}
		out[cp] = reverse
	}
	return out
}()

// resolveTag finds the token for an element in a code page.
func resolveTag(ns, name string) (byte, byte, bool) {
	cp, ok := cpByNS[ns]
	if !ok {
		return 0, 0, false
	}
	token, ok := tagsByCP[cp][name]
	if !ok {
		return 0, 0, false
	}
	return cp, token, true
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// WBXMLWriter encodes an element tree into a WBXML 1.3 token stream.
//
// It tracks the code page it is in and switches, in the stream, whenever an
// element comes from a different code page. The first element of a document is
// written after the header, and because code page 0 (AirSync) is the default a
// client starts in, an opening AirSync element does not need a switch.
type WBXMLWriter struct {
	buf      *bytes.Buffer
	codePage byte
}

// NewWBXMLWriter starts a stream with the WBXML header already written.
func NewWBXMLWriter() *WBXMLWriter {
	w := &WBXMLWriter{buf: &bytes.Buffer{}, codePage: cpAirSync}
	w.buf.WriteByte(wbxmlVersion)
	w.buf.WriteByte(wbxmlPublicID)
	w.buf.WriteByte(wbxmlCharset)
	w.buf.WriteByte(wbxmlStrTable)
	w.buf.WriteByte(wbxmlProcInstr)
	return w
}

// Start opens an element. It switches code page in the stream when the element
// lives in a different code page than the one currently open.
func (w *WBXMLWriter) Start(ns, name string) error {
	cp, token, ok := resolveTag(ns, name)
	if !ok {
		return fmt.Errorf("eas: no tag for %s:%s", ns, name)
	}
	if cp != w.codePage {
		w.buf.WriteByte(tokenSwitchPage)
		w.buf.WriteByte(cp)
		w.codePage = cp
	}
	w.buf.WriteByte(token)
	return nil
}

// End closes the current element.
func (w *WBXMLWriter) End() {
	w.buf.WriteByte(tokenEnd)
}

// Text writes character data. An empty string writes nothing, which is the same
// as an element with no content rather than one whose content is the empty
// string -- the distinction a client does not draw and does not need.
func (w *WBXMLWriter) Text(s string) {
	if s == "" {
		return
	}
	w.buf.WriteByte(tokenStrI)
	w.buf.WriteString(s)
	w.buf.WriteByte(0x00)
}

// Opaque writes binary data, length-prefixed, for a body or attachment payload.
func (w *WBXMLWriter) Opaque(data []byte) {
	if len(data) == 0 {
		return
	}
	w.buf.WriteByte(tokenOpaque)
	w.writeMBUInt32(uint32(len(data)))
	w.buf.Write(data)
}

// Leaf writes an element with only text content, opening and closing it.
func (w *WBXMLWriter) Leaf(ns, name, value string) error {
	if err := w.Start(ns, name); err != nil {
		return err
	}
	w.Text(value)
	w.End()
	return nil
}

// Bytes is the encoded stream.
func (w *WBXMLWriter) Bytes() []byte {
	return w.buf.Bytes()
}

func (w *WBXMLWriter) writeMBUInt32(v uint32) {
	for v >= 0x80 {
		w.buf.WriteByte(byte(v&0x7F) | 0x80)
		v >>= 7
	}
	w.buf.WriteByte(byte(v))
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// WBXMLNode is one decoded element: its name, code page, text content, any
// opaque payload, and its children in document order.
type WBXMLNode struct {
	Name     string
	NS       string
	Text     string
	Opaque   []byte
	Children []*WBXMLNode
}

// FindChild returns the first child with the given name, or nil.
func (n *WBXMLNode) FindChild(name string) *WBXMLNode {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// FindChildren returns every child with the given name, in order.
func (n *WBXMLNode) FindChildren(name string) []*WBXMLNode {
	var out []*WBXMLNode
	for _, c := range n.Children {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// TextOf returns the text of the first child with the given name.
func (n *WBXMLNode) TextOf(name string) string {
	if c := n.FindChild(name); c != nil {
		return c.Text
	}
	return ""
}

// FindDescendant returns the first descendant, at any depth, with the given
// name, walking depth-first. A command's arguments nest several elements deep
// (a SyncKey under FolderHierarchy under Folders), so a reader that only looked
// at the direct children of the command would miss them.
func (n *WBXMLNode) FindDescendant(name string) *WBXMLNode {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
		if d := c.FindDescendant(name); d != nil {
			return d
		}
	}
	return nil
}

// TextDescendant returns the text of the first descendant with the given name.
func (n *WBXMLNode) TextDescendant(name string) string {
	if d := n.FindDescendant(name); d != nil {
		return d.Text
	}
	return ""
}

// WBXMLDecoder decodes a client's token stream into a node tree.
type WBXMLDecoder struct {
	data     []byte
	pos      int
	codePage byte
}

// NewWBXMLDecoder prepares to read a stream.
func NewWBXMLDecoder(data []byte) *WBXMLDecoder {
	return &WBXMLDecoder{data: data, codePage: cpAirSync}
}

// readByte takes one byte, or io.EOF at the end of the stream.
func (d *WBXMLDecoder) readByte() (byte, error) {
	if d.pos >= len(d.data) {
		return 0, io.EOF
	}
	b := d.data[d.pos]
	d.pos++
	return b, nil
}

// readMBUInt32 reads a multi-byte unsigned integer.
func (d *WBXMLDecoder) readMBUInt32() (uint32, error) {
	var value uint32
	var shift uint
	for {
		b, err := d.readByte()
		if err != nil {
			return 0, err
		}
		value |= uint32(b&0x7F) << shift
		if b&0x80 == 0 {
			return value, nil
		}
		shift += 7
		if shift > 28 {
			return 0, fmt.Errorf("eas: an oversized length in the stream")
		}
	}
}

// readStrI reads a 0x00-terminated inline string.
func (d *WBXMLDecoder) readStrI() (string, error) {
	start := d.pos
	for d.pos < len(d.data) {
		if d.data[d.pos] == 0x00 {
			s := string(d.data[start:d.pos])
			d.pos++
			return s, nil
		}
		d.pos++
	}
	return "", io.ErrUnexpectedEOF
}

// DecodeHeader reads and validates the five-byte WBXML header.
func (d *WBXMLDecoder) DecodeHeader() error {
	if len(d.data) < 5 {
		return fmt.Errorf("eas: the stream is too short to be WBXML")
	}
	version := d.data[0]
	publicID := d.data[1]
	if version != wbxmlVersion {
		return fmt.Errorf("eas: unsupported WBXML version %d", version)
	}
	if publicID != wbxmlPublicID {
		return fmt.Errorf("eas: an unexpected public ID in the stream")
	}
	d.pos = 5
	return nil
}

// Decode reads the whole stream into a tree and returns the document's single
// root element, which is the command.
func (d *WBXMLDecoder) Decode() (*WBXMLNode, error) {
	root := &WBXMLNode{Children: []*WBXMLNode{}}
	stack := []*WBXMLNode{root}

	for {
		b, err := d.readByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		switch b {
		case tokenEnd:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case tokenStrI:
			text, err := d.readStrI()
			if err != nil {
				return nil, err
			}
			top := stack[len(stack)-1]
			// A child's text belongs to the child, not the parent, so the text
			// is appended to the current element's content.
			top.Text += text
		case tokenOpaque:
			length, err := d.readMBUInt32()
			if err != nil {
				return nil, err
			}
			if d.pos+int(length) > len(d.data) {
				return nil, fmt.Errorf("eas: the opaque payload is truncated")
			}
			opaque := make([]byte, length)
			copy(opaque, d.data[d.pos:d.pos+int(length)])
			d.pos += int(length)
			stack[len(stack)-1].Opaque = opaque
		case tokenSwitchPage:
			cp, err := d.readByte()
			if err != nil {
				return nil, err
			}
			d.codePage = cp
		case tokenEntity, tokenLiteral, tokenExtI0, tokenExtI1, tokenExtI2:
			// ActiveSync does not use entities, literals or extended tags; a
			// byte in this range that is not a known tag is skipped rather than
			// treated as an element, so a stray one cannot open a phantom node.
		default:
			name, ok := tokenToName[d.codePage][b]
			if !ok {
				// A token the current code page does not name is not an element
				// this protocol uses; it is skipped, and the stream is left in
				// a state the next switch can recover from.
				continue
			}
			node := &WBXMLNode{
				Name:     name,
				NS:       nsByCP[d.codePage],
				Children: []*WBXMLNode{},
			}
			stack[len(stack)-1].Children = append(stack[len(stack)-1].Children, node)
			stack = append(stack, node)
		}
	}

	// The document is one command element under the synthetic root.
	if len(root.Children) == 1 {
		return root.Children[0], nil
	}
	if len(root.Children) == 0 {
		return nil, fmt.Errorf("eas: the stream names no command")
	}
	return root, nil
}

// CommandName returns the decoded command: the root element's name.
func (n *WBXMLNode) CommandName() string {
	if n == nil {
		return ""
	}
	return n.Name
}

// asInt parses a node's text as an integer, returning def when it is not one.
func asInt(n *WBXMLNode, def int) int {
	if n == nil {
		return def
	}
	v := 0
	_, err := fmt.Sscanf(strings.TrimSpace(n.Text), "%d", &v)
	if err != nil {
		return def
	}
	return v
}

// asBool reads a 0/1 flag.
func asBool(n *WBXMLNode, def bool) bool {
	if n == nil {
		return def
	}
	switch strings.TrimSpace(n.Text) {
	case "1", "true":
		return true
	case "0", "false":
		return false
	}
	return def
}

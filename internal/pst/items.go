package pst

import (
	"encoding/binary"
	"strings"
	"time"
)

// MAPI property identifiers this reader cares about. The values come from
// [MS-PST] and the MAPI property tables.
const (
	propEntryID            = 0x0fff
	propMessageClass       = 0x001a
	propSubject            = 0x0037
	propBody               = 0x1000
	propRtfCompressed      = 0x1009
	propBodyHTML           = 0x1013
	propTransportHeader    = 0x007d
	propInternetMessageID  = 0x1035
	propInternetReferences = 0x1036
	propSenderName         = 0x0c1a
	propSenderAddress      = 0x0c1f
	propSentRepName        = 0x0c1b
	propDisplayTo          = 0x0e04
	propDisplayCc          = 0x0e03
	propDisplayBcc         = 0x0e02
	propMessageFlags       = 0x0e07
	propSubmitTime         = 0x0039
	propDeliveryTime       = 0x0019
	propStartDate          = 0x0060
	propEndDate            = 0x0061
	propLocation           = 0x3a0d
	propDisplayName        = 0x3001
	propEmailAddress       = 0x3003
	propAttachFilename     = 0x3704
	propAttachLongFilename = 0x3707
	propAttachData         = 0x3701
	propAttachMethod       = 0x3705
	propContactEmails      = 0x3a56
	propGivenName          = 0x3a06
	propSurname            = 0x3a11
	propMiddleName         = 0x3a44
	propOrganization       = 0x3a16
	propJobTitle           = 0x3a17
	propBusinessPhone      = 0x3a08
	propHomePhone          = 0x3a09
	propMobilePhone        = 0x3a1c
	propNotes              = 0x3004
	propStreetAddress      = 0x3a29
	propLocality           = 0x3a27
	propRegion             = 0x3a28
	propPostalCode         = 0x3a2a
	propCountry            = 0x3a26
	propHomeStreet         = 0x3a5d
	propHomeCity           = 0x3a59
	propHomeRegion         = 0x3a5c
	propHomeCountry        = 0x3a5a
	propHomePostalCode     = 0x3a5b
)

// Property is one record entry, resolved to something usable.
type Property struct {
	Identifier uint32
	ValueType  uint16
	Text       string
	Data       []byte
	Int        int64
	When       time.Time
	HasValue   bool
}

// Item is one node of the item tree: a folder, a message, a contact, an
// appointment, and so on.
type Item struct {
	ID          uint32
	ParentID    uint32
	NodeType    uint8
	Kind        string
	Name        string
	Properties  map[uint32]Property
	Attachments []Attachment
}

// Attachment is an attachment record of a message.
type Attachment struct {
	Name string
	Data []byte
}

// DescriptorCount reports how many item nodes the file has.
func (p *File) DescriptorCount() int { return len(p.descriptors) }

// OffsetEntries exposes the offsets index, for diagnostics.
func (p *File) OffsetEntries() map[uint64]offsetEntry { return p.offsets }

// Items returns every item node in the file, folders first.
func (p *File) Items() []Item {
	items := make([]Item, 0, len(p.descriptors))
	for id, entry := range p.descriptors {
		nodeType := uint8(id & 0x1f)
		switch nodeType {
		case nodeTypeFolder, nodeTypeSearchFolder, nodeTypeMessage,
			nodeTypeAssociatedContent, nodeTypeAttachment:
		default:
			continue
		}

		props, err := p.properties(id)
		if err != nil {
			continue
		}
		item := Item{
			ID:         uint32(id),
			ParentID:   entry.parentIdentifier,
			NodeType:   nodeType,
			Properties: map[uint32]Property{},
		}
		for _, prop := range props {
			value := p.propertyValue(id, prop)
			if value.HasValue {
				item.Properties[prop.identifier] = value
			}
		}
		item.Kind = itemKind(item.NodeType, item.Properties[propMessageClass].Text)
		item.Name = itemName(item)
		if item.Kind == "message" {
			item.Attachments = p.attachments(id)
		}
		items = append(items, item)
	}
	return items
}

// Folders returns the folder items, each with its path from the root.
func (p *File) Folders() []Item {
	items := p.Items()
	folders := make([]Item, 0, 32)
	for _, item := range items {
		if item.Kind == "folder" || item.Kind == "search folder" {
			folders = append(folders, item)
		}
	}
	return folders
}

func itemKind(nodeType uint8, messageClass string) string {
	switch nodeType {
	case nodeTypeFolder:
		return "folder"
	case nodeTypeSearchFolder:
		return "search folder"
	case nodeTypeAttachment:
		return "attachment"
	case nodeTypeAssociatedContent:
		return "associated content"
	}
	switch {
	case strings.HasPrefix(messageClass, "IPM.Contact"):
		return "contact"
	case strings.HasPrefix(messageClass, "IPM.Appointment"),
		strings.HasPrefix(messageClass, "IPM.OnlineMeeting"):
		return "appointment"
	case strings.HasPrefix(messageClass, "IPM.Task"):
		return "task"
	case strings.HasPrefix(messageClass, "IPM.Journal"):
		return "journal"
	case strings.HasPrefix(messageClass, "IPM.Note"):
		return "message"
	case strings.HasPrefix(messageClass, "IPM.Configuration"),
		strings.HasPrefix(messageClass, "IPM.Microsoft"),
		strings.HasPrefix(messageClass, "IPM.OLE"),
		strings.HasPrefix(messageClass, "IPM.Outlook"),
		strings.HasPrefix(messageClass, "IPM.Search"),
		strings.HasPrefix(messageClass, "IPM.Activity"),
		strings.HasPrefix(messageClass, "IPM.Abch"):
		// Outlook's own bookkeeping records. They are messages to the store
		// but not mail, and importing them would fill a folder with invisible
		// calendar configuration blobs.
		return "item"
	case messageClass == "":
		return "message"
	default:
		return "item"
	}
}

func itemName(item Item) string {
	if name := item.Properties[propDisplayName].Text; name != "" {
		return name
	}
	if name := item.Properties[propSubject].Text; name != "" {
		return name
	}
	return item.Properties[propAttachFilename].Text
}

// propertyValue resolves a record entry into text, bytes, an integer or a date.
func (p *File) propertyValue(descriptor uint64, entry recordEntry) Property {
	prop := Property{Identifier: entry.identifier, ValueType: entry.valueType}

	// Small values live in the record entry itself.
	switch entry.valueType {
	case 0x0002, 0x0003, 0x0004, 0x000a:
		prop.Int = int64(int32(entry.raw))
		prop.HasValue = true
		return prop
	case 0x000b:
		prop.Int = int64(entry.raw & 0xff)
		prop.HasValue = true
		return prop
	}

	if entry.raw == 0 {
		prop.HasValue = false
		return prop
	}

	var data []byte
	if entry.raw&0x1f != 0 {
		raw, err := p.localDescriptorData(descriptor, entry.raw)
		if err != nil {
			return prop
		}
		data = raw
	} else {
		data, _ = tableValue(p.mustBlocks(descriptor), entry.raw)
	}

	if len(data) == 0 {
		return prop
	}

	switch entry.valueType {
	case 0x001e:
		prop.Text = trimASCII(data)
	case 0x001f:
		prop.Text = trimUTF16(data)
	case 0x0040, 0x0007:
		if len(data) >= 8 {
			prop.When = filetimeToTime(binary.LittleEndian.Uint64(data))
		}
	case 0x0048:
		if len(data) >= 16 {
			prop.Text = formatGUID(data)
		}
		prop.Data = data
	default:
		prop.Data = data
	}
	prop.HasValue = true
	return prop
}

func (p *File) mustBlocks(descriptor uint64) []block {
	blocks, err := p.tableFor(descriptor)
	if err != nil {
		return nil
	}
	return blocks
}

// attachments walks a message's attachment table. The table's record entries
// key each attachment to a local descriptor holding that attachment's own
// property table.
func (p *File) attachments(message uint64) []Attachment {
	blocks, err := p.localDescriptorBlocks(message, attachmentsTableIdentifier)
	if err != nil || len(blocks) == 0 {
		return nil
	}

	entry, ok := p.descriptors[message]
	if !ok {
		return nil
	}
	_ = entry

	header := blocks[0].data
	if len(header) < 4 || header[2] != 0xec || header[3] != 0x8c {
		return nil
	}
	b5Ref := binary.LittleEndian.Uint32(header[4:8])
	b5, ok := tableValue(blocks, b5Ref)
	if !ok || len(b5) < 8 || b5[0] != 0xb5 {
		return nil
	}
	entries, err := recordEntries(blocks, binary.LittleEndian.Uint32(b5[4:8]), int(b5[3]), int(b5[1]), int(b5[2]), 0)
	if err != nil {
		return nil
	}

	out := make([]Attachment, 0, len(entries))
	for _, e := range entries {
		sub := e.raw
		if sub == 0 {
			continue
		}
		data, err := p.localDescriptorData(message, sub)
		if err != nil || len(data) < 4 || data[2] != 0xec {
			continue
		}
		// The attachment's own properties are a bc table in that descriptor.
		attachmentBlocks := []block{{data: data}}
		attachmentBlocks[0].index = parseBlockIndex(data)
		attachmentEntries, err := propertiesFromBlocks(attachmentBlocks)
		if err != nil {
			continue
		}
		name := ""
		body := []byte{}
		for _, prop := range attachmentEntries {
			value := p.propertyValue(message, prop)
			if !value.HasValue {
				continue
			}
			switch prop.identifier {
			case propAttachFilename:
				name = value.Text
			case propAttachData:
				body = value.Data
			}
		}
		if len(body) > 0 {
			out = append(out, Attachment{Name: name, Data: body})
		}
	}
	return out
}

// propertiesFromBlocks parses record entries out of an already decoded table.
func propertiesFromBlocks(blocks []block) ([]recordEntry, error) {
	if len(blocks) == 0 || len(blocks[0].data) < 4 || blocks[0].data[2] != 0xec {
		return nil, nil
	}
	tableType := blocks[0].data[3]
	valueRef := binary.LittleEndian.Uint32(blocks[0].data[4:8])

	b5Ref := valueRef
	if tableType != 0x8c && tableType != 0xa5 && tableType != 0xbc {
		header, ok := tableValue(blocks, valueRef)
		if !ok || len(header) < 12 {
			return nil, nil
		}
		switch tableType {
		case 0x6c, 0x9c:
			b5Ref = binary.LittleEndian.Uint32(header[0:4])
		case 0x7c, 0xac:
			b5Ref = binary.LittleEndian.Uint32(header[10:14])
		default:
			return nil, nil
		}
	}
	b5, ok := tableValue(blocks, b5Ref)
	if !ok || len(b5) < 8 || b5[0] != 0xb5 {
		return nil, nil
	}
	return recordEntries(blocks, binary.LittleEndian.Uint32(b5[4:8]), int(b5[3]), int(b5[1]), int(b5[2]), 0)
}

// localDescriptorBlocks returns the table array of one named local descriptor.
func (p *File) localDescriptorBlocks(descriptor uint64, identifier uint32) ([]block, error) {
	entry, ok := p.descriptors[descriptor]
	if !ok || entry.localDescriptors == 0 {
		return nil, nil
	}
	blocks, err := p.dataBlocks(entry.localDescriptors)
	if err != nil {
		return nil, err
	}
	for _, b := range blocks {
		var found uint64
		if err := walkLocalDescriptorTree(p, b.data, identifier, &found); err != nil {
			return nil, err
		}
		if found != 0 {
			return p.dataBlocks(found)
		}
	}
	return nil, nil
}

func walkLocalDescriptorTree(p *File, data []byte, want uint32, found *uint64) error {
	if len(data) < 4 || data[0] != 0x02 {
		return nil
	}
	level := data[1]
	count := int(binary.LittleEndian.Uint16(data[2:4]))

	nodeHeader := 4
	entrySize := 8
	if p.is64 {
		nodeHeader = 8
		entrySize = 16
	}
	if level == 0 {
		entrySize = 12
		if p.is64 {
			entrySize = 24
		}
	}

	readID := func(at int) uint32 {
		if p.is64 {
			return uint32(binary.LittleEndian.Uint64(data[at : at+8]))
		}
		return binary.LittleEndian.Uint32(data[at : at+4])
	}
	readSub := func(at int) uint64 {
		if p.is64 {
			return binary.LittleEndian.Uint64(data[at+nodeHeader : at+nodeHeader+8])
		}
		return uint64(binary.LittleEndian.Uint32(data[at+nodeHeader : at+nodeHeader+4]))
	}

	for i := 0; i < count; i++ {
		at := nodeHeader + i*entrySize
		if at+entrySize > len(data) {
			break
		}
		id := readID(at)
		if level == 0 {
			if id == want {
				*found = readSub(at)
				return nil
			}
			if id > want {
				return nil
			}
			continue
		}

		// A branch entry covers every identifier from its own up to the next
		// entry's. Descend into the one that can contain the wanted value.
		next := uint32(0xffffffff)
		nextAt := nodeHeader + (i+1)*entrySize
		if nextAt+entrySize <= len(data) {
			next = readID(nextAt)
		}
		if want < id || want >= next {
			continue
		}
		blocks, err := p.dataBlocks(readSub(at))
		if err != nil {
			return err
		}
		for _, b := range blocks {
			if err := walkLocalDescriptorTree(p, b.data, want, found); err != nil {
				return err
			}
			if *found != 0 {
				return nil
			}
		}
		return nil
	}
	return nil
}

func formatGUID(data []byte) string {
	return formatGUIDBytes(data)
}

// trimASCII drops the terminating NULs a MAPI string value can carry.
func trimASCII(data []byte) string {
	s := string(data)
	for len(s) > 0 && s[len(s)-1] == 0 {
		s = s[:len(s)-1]
	}
	return s
}

// trimUTF16 decodes a little endian UTF-16 string up to its first NUL.
func trimUTF16(data []byte) string {
	out := make([]rune, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		r := rune(binary.LittleEndian.Uint16(data[i : i+2]))
		if r == 0 {
			break
		}
		out = append(out, r)
	}
	return string(out)
}

// filetimeToTime converts a Windows FILETIME: 100 ns ticks since 1601-01-01.
func filetimeToTime(ticks uint64) time.Time {
	if ticks == 0 || ticks >= 1<<63 {
		return time.Time{}
	}
	const ticksPerSecond = 10000000
	const secondsBetweenEpochs = 11644473600
	return time.Unix(int64(ticks/ticksPerSecond)-secondsBetweenEpochs, 0).UTC()
}

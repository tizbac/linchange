package pst

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
)

// Named properties. Outlook stores newer properties (appointment start and end,
// the contact e-mail slots, the "PSETID" family) under file-local identifiers
// above 0x8000. The name-to-id map at descriptor 0x61 is what turns those back
// into a stable (GUID, name or LID) pair, and without it an appointment's start
// time is just "property 0x8004".
type NamedProperty struct {
	GUID [16]byte
	LID  uint32
	Name string
}

// The property set GUIDs this reader interprets, as hex strings.
const (
	guidAppointment = "00062002-0000-0000-c000-000000000046"
	guidAddress     = "00062004-0000-0000-c000-000000000046"
	guidCommon      = "00062008-0000-0000-c000-000000000046"
	guidMeeting     = "6ed8da90-450b-101b-98da-00aa003f1305"
)

// Well known local identifiers (LIDs) of named properties.
const (
	lidAppointmentStartWhole = 0x820d
	lidAppointmentEndWhole   = 0x820e
	lidAppointmentSubType    = 0x8215
	lidLocation              = 0x8208
	lidRecurrenceType        = 0x8231
	lidRecurring             = 0x8223
	lidBusyStatus            = 0x8205
	lidGlobalObjectID        = 0x0003
	lidEmail1Address         = 0x8083
	lidEmail1DisplayName     = 0x8080
	lidEmail2Address         = 0x8093
	lidEmail2DisplayName     = 0x8090
	lidEmail3Address         = 0x80a3
	lidEmail3DisplayName     = 0x80a0
	lidWorkAddressStreet     = 0x8045
	lidWorkAddressCity       = 0x8046
	lidWorkAddressState      = 0x8047
	lidWorkAddressPostalCode = 0x8048
	lidWorkAddressCountry    = 0x8049
)

// readNameToIDMap parses the name-to-id map. A missing map, or one that does
// not follow the expected shape, yields an empty map: named properties are
// optional and the fixed MAPI properties often carry the same information.
func (p *File) readNameToIDMap() map[uint32]NamedProperty {
	out := map[uint32]NamedProperty{}
	entries, err := p.properties(0x61)
	if err != nil || len(entries) == 0 {
		return out
	}

	var classes []byte
	var table []byte
	var stringsData []byte
	for _, entry := range entries {
		switch entry.identifier {
		case 0x0002:
			classes = p.propertyValue(0x61, entry).Data
		case 0x0003:
			table = p.propertyValue(0x61, entry).Data
		case 0x0004:
			stringsData = p.propertyValue(0x61, entry).Data
		}
	}
	if len(table) < 8 {
		return out
	}

	guids := make([][16]byte, 0, len(classes)/16)
	for i := 0; i+16 <= len(classes); i += 16 {
		var guid [16]byte
		copy(guid[:], classes[i:i+16])
		guids = append(guids, guid)
	}

	for i := 0; i+8 <= len(table); i += 8 {
		value := binary.LittleEndian.Uint32(table[i : i+4])
		entryType := binary.LittleEndian.Uint16(table[i+4 : i+6])
		number := binary.LittleEndian.Uint16(table[i+6 : i+8])

		named := NamedProperty{}
		if entryType >= 6 {
			index := int(entryType/2) - 3
			if index >= 0 && index < len(guids) {
				named.GUID = guids[index]
			}
		}
		if entryType&1 == 1 {
			named.Name = readMAPIString(stringsData, int(value))
		} else {
			named.LID = value
		}
		out[0x8000+uint32(number)] = named
	}
	return out
}

// readMAPIString reads one of the counted strings in the name-to-id map's
// string table. The table mixes UTF-16 and ASCII; a run with NUL bytes in the
// high half is treated as UTF-16.
func readMAPIString(data []byte, offset int) string {
	if offset < 0 || offset+4 > len(data) {
		return ""
	}
	length := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
	offset += 4
	if length <= 0 || offset+length > len(data) {
		return ""
	}
	raw := data[offset : offset+length]
	looksUTF16 := false
	for i := 1; i < len(raw); i += 2 {
		if raw[i] == 0 {
			looksUTF16 = true
			break
		}
	}
	if looksUTF16 {
		units := make([]uint16, 0, len(raw)/2)
		for i := 0; i+1 < len(raw); i += 2 {
			units = append(units, binary.LittleEndian.Uint16(raw[i:i+2]))
		}
		return strings.TrimRight(string(utf16.Decode(units)), "\x00")
	}
	return strings.TrimRight(string(raw), "\x00")
}

// namedProperty looks a property identifier up in the file's name-to-id map.
func (p *File) namedProperty(identifier uint32) (NamedProperty, bool) {
	if identifier < 0x8000 {
		return NamedProperty{}, false
	}
	if p.names == nil {
		p.names = p.readNameToIDMap()
	}
	named, ok := p.names[identifier]
	return named, ok
}

// guidString formats a GUID in the usual order, which is the form the LID
// constants above use.
func guidString(guid [16]byte) string {
	return formatGUIDBytes(guid[:])
}

func formatGUIDBytes(guid []byte) string {
	if len(guid) < 16 {
		return ""
	}
	return fmt.Sprintf("%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		binary.LittleEndian.Uint32(guid[0:4]),
		binary.LittleEndian.Uint16(guid[4:6]),
		binary.LittleEndian.Uint16(guid[6:8]),
		guid[8], guid[9],
		guid[10], guid[11], guid[12], guid[13], guid[14], guid[15])
}

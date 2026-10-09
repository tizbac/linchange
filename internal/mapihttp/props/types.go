package props

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// PropertyType represents a MAPI property type (MS-OXPROPS).
type PropertyType uint16

const (
	PT_UNSPECIFIED PropertyType = 0x0000
	PT_NULL        PropertyType = 0x0001
	PT_I2          PropertyType = 0x0002
	PT_I4          PropertyType = 0x0003
	PT_R4          PropertyType = 0x0004
	PT_R8          PropertyType = 0x0005
	PT_CURRENCY    PropertyType = 0x0006
	PT_APPTIME     PropertyType = 0x0007
	PT_ERROR       PropertyType = 0x000A
	PT_BOOLEAN     PropertyType = 0x000B
	PT_OBJECT      PropertyType = 0x000D
	PT_I8          PropertyType = 0x0014
	PT_STRING8     PropertyType = 0x001E
	PT_UNICODE     PropertyType = 0x001F
	PT_SYSTIME     PropertyType = 0x0040
	PT_CLSID       PropertyType = 0x0048
	PT_BINARY      PropertyType = 0x0102
	// Multi-value types
	PT_MV_I2       PropertyType = 0x1002
	PT_MV_I4       PropertyType = 0x1003
	PT_MV_R4       PropertyType = 0x1004
	PT_MV_R8       PropertyType = 0x1005
	PT_MV_CURRENCY PropertyType = 0x1006
	PT_MV_APPTIME  PropertyType = 0x1007
	PT_MV_I8       PropertyType = 0x1014
	PT_MV_STRING8  PropertyType = 0x101E
	PT_MV_UNICODE  PropertyType = 0x101F
	PT_MV_SYSTIME  PropertyType = 0x1040
	PT_MV_CLSID    PropertyType = 0x1048
	PT_MV_BINARY   PropertyType = 0x1102
)

// PropertyTag is a 32-bit value: high 16 bits = property ID, low 16 bits = type.
type PropertyTag uint32

func (t PropertyTag) ID() uint16         { return uint16(t >> 16) }
func (t PropertyTag) Type() PropertyType { return PropertyType(t & 0xFFFF) }
func (t PropertyTag) String() string {
	return fmt.Sprintf("0x%08X (id=0x%04X, type=%s)", uint32(t), t.ID(), t.Type())
}

// PropertyTagFromIDAndType creates a tag from ID and type.
func PropertyTagFromIDAndType(id uint16, typ PropertyType) PropertyTag {
	return PropertyTag(uint32(id)<<16 | uint32(typ))
}

// PropertyValue represents a typed MAPI property value.
type PropertyValue struct {
	Tag   PropertyTag
	Type  PropertyType
	Value interface{}
}

// IsMultiValue returns true if the property type is multi-value.
func (pv PropertyValue) IsMultiValue() bool {
	return pv.Type >= PT_MV_I2
}

// SingleValueType returns the corresponding single-value type for a multi-value type.
func SingleValueType(mv PropertyType) PropertyType {
	switch mv {
	case PT_MV_I2:
		return PT_I2
	case PT_MV_I4:
		return PT_I4
	case PT_MV_R4:
		return PT_R4
	case PT_MV_R8:
		return PT_R8
	case PT_MV_CURRENCY:
		return PT_CURRENCY
	case PT_MV_APPTIME:
		return PT_APPTIME
	case PT_MV_I8:
		return PT_I8
	case PT_MV_STRING8:
		return PT_STRING8
	case PT_MV_UNICODE:
		return PT_UNICODE
	case PT_MV_SYSTIME:
		return PT_SYSTIME
	case PT_MV_CLSID:
		return PT_CLSID
	case PT_MV_BINARY:
		return PT_BINARY
	default:
		return mv
	}
}

// MultiValueType returns the corresponding multi-value type for a single-value type.
func MultiValueType(sv PropertyType) PropertyType {
	switch sv {
	case PT_I2:
		return PT_MV_I2
	case PT_I4:
		return PT_MV_I4
	case PT_R4:
		return PT_MV_R4
	case PT_R8:
		return PT_MV_R8
	case PT_CURRENCY:
		return PT_MV_CURRENCY
	case PT_APPTIME:
		return PT_MV_APPTIME
	case PT_I8:
		return PT_MV_I8
	case PT_STRING8:
		return PT_MV_STRING8
	case PT_UNICODE:
		return PT_MV_UNICODE
	case PT_SYSTIME:
		return PT_MV_SYSTIME
	case PT_CLSID:
		return PT_MV_CLSID
	case PT_BINARY:
		return PT_MV_BINARY
	default:
		return sv
	}
}

// ---------------------------------------------------------------------------
// Common Property Tags (MS-OXPROPS)
// ---------------------------------------------------------------------------

// Message properties
const (
	PR_SUBJECT                         PropertyTag = 0x0037001F // PT_UNICODE
	PR_SUBJECT_A                       PropertyTag = 0x0037001E // PT_STRING8
	PR_MESSAGE_CLASS                   PropertyTag = 0x001A001E // PT_STRING8
	PR_MESSAGE_DELIVERY_TIME           PropertyTag = 0x0E060040 // PT_SYSTIME
	PR_CLIENT_SUBMIT_TIME              PropertyTag = 0x00390040 // PT_SYSTIME
	PR_SENDER_NAME                     PropertyTag = 0x0C1A001F // PT_UNICODE
	PR_SENDER_EMAIL_ADDRESS            PropertyTag = 0x0C1F001F // PT_UNICODE
	PR_SENT_REPRESENTING_NAME          PropertyTag = 0x0042001F // PT_UNICODE
	PR_SENT_REPRESENTING_EMAIL_ADDRESS PropertyTag = 0x0065001F // PT_UNICODE
	PR_DISPLAY_TO                      PropertyTag = 0x0E04001F // PT_UNICODE
	PR_DISPLAY_CC                      PropertyTag = 0x0E03001F // PT_UNICODE
	PR_BODY                            PropertyTag = 0x1000001F // PT_UNICODE
	PR_BODY_A                          PropertyTag = 0x1000001E // PT_STRING8
	PR_RTF_COMPRESSED                  PropertyTag = 0x10090102 // PT_BINARY
	PR_RTF_IN_SYNC                     PropertyTag = 0x1006000B // PT_BOOLEAN
	PR_HASATTACH                       PropertyTag = 0x0E1B000B // PT_BOOLEAN
	PR_ATTACHMENT_TABLE                PropertyTag = 0x37010000 // PT_OBJECT
	PR_RECIPIENT_TABLE                 PropertyTag = 0x37020000 // PT_OBJECT
)

// Recipient properties
const (
	PR_DISPLAY_NAME      PropertyTag = 0x3001001F // PT_UNICODE
	PR_EMAIL_ADDRESS     PropertyTag = 0x3003001F // PT_UNICODE
	PR_ADDRTYPE          PropertyTag = 0x3002001E // PT_STRING8
	PR_RECIPIENT_TYPE    PropertyTag = 0x0C150003 // PT_I4
	PR_RECIPIENT_FLAGS   PropertyTag = 0x5FFD0003 // PT_I4
	PR_RESPONSIBILITY    PropertyTag = 0x5FFE0003 // PT_I4
	PR_RECIPIENT_ENTRYID PropertyTag = 0x0FFF0102 // PT_BINARY
)

// Folder properties
const (
	PR_DISPLAY_NAME_W  PropertyTag = 0x3001001F // PT_UNICODE
	PR_CONTAINER_CLASS PropertyTag = 0x3613001E // PT_STRING8
	PR_FOLDER_TYPE     PropertyTag = 0x36010003 // PT_I4
	PR_CONTENT_COUNT   PropertyTag = 0x36020003 // PT_I4
	PR_CONTENT_UNREAD  PropertyTag = 0x36030003 // PT_I4
	PR_FOLDER_ID       PropertyTag = 0x67480014 // PT_I8
	PR_PARENT_ENTRYID  PropertyTag = 0x0E090102 // PT_BINARY
)

// Store properties
const (
	PR_IPM_SUBTREE_ENTRYID     PropertyTag = 0x35E00102 // PT_BINARY
	PR_IPM_OUTBOX_ENTRYID      PropertyTag = 0x35E20102 // PT_BINARY
	PR_IPM_WASTEBASKET_ENTRYID PropertyTag = 0x35E30102 // PT_BINARY
	PR_IPM_SENTMAIL_ENTRYID    PropertyTag = 0x35E40102 // PT_BINARY
	PR_IPM_DRAFTS_ENTRYID      PropertyTag = 0x35E60102 // PT_BINARY
	PR_IPM_APPOINTMENT_ENTRYID PropertyTag = 0x35E50102 // PT_BINARY
	PR_IPM_CONTACT_ENTRYID     PropertyTag = 0x35E70102 // PT_BINARY
	PR_IPM_JOURNAL_ENTRYID     PropertyTag = 0x35E80102 // PT_BINARY
	PR_IPM_NOTE_ENTRYID        PropertyTag = 0x35E90102 // PT_BINARY
	PR_IPM_TASK_ENTRYID        PropertyTag = 0x35EA0102 // PT_BINARY
	PR_MDB_PROVIDER            PropertyTag = 0x34140102 // PT_BINARY
)

// Attachment properties
const (
	PR_ATTACH_NUM              PropertyTag = 0x37040003 // PT_I4
	PR_ATTACH_METHOD           PropertyTag = 0x37050003 // PT_I4
	PR_ATTACH_FILENAME         PropertyTag = 0x3707001F // PT_UNICODE
	PR_ATTACH_LONG_FILENAME    PropertyTag = 0x3707001E // PT_STRING8
	PR_ATTACH_PATHNAME         PropertyTag = 0x3708001F // PT_UNICODE
	PR_ATTACH_DATA_BIN         PropertyTag = 0x37010102 // PT_BINARY
	PR_ATTACH_SIZE             PropertyTag = 0x37060003 // PT_I4
	PR_ATTACH_MIME_TAG         PropertyTag = 0x370E001E // PT_STRING8
	PR_ATTACH_CONTENT_ID       PropertyTag = 0x3712001E // PT_STRING8
	PR_ATTACH_CONTENT_LOCATION PropertyTag = 0x3713001E // PT_STRING8
)

// Calendar properties
const (
	PR_APPT_START_TIME       PropertyTag = 0x80020040 // PT_SYSTIME
	PR_APPT_END_TIME         PropertyTag = 0x80030040 // PT_SYSTIME
	PR_APPT_SUBJECT          PropertyTag = 0x0037001F // PT_UNICODE
	PR_APPT_LOCATION         PropertyTag = 0x8004001F // PT_UNICODE
	PR_APPT_BODY             PropertyTag = 0x1000001F // PT_UNICODE
	PR_APPT_RECUR_PATTERN    PropertyTag = 0x80050003 // PT_I4
	PR_APPT_RECUR_START      PropertyTag = 0x80060040 // PT_SYSTIME
	PR_APPT_RECUR_END        PropertyTag = 0x80070040 // PT_SYSTIME
	PR_APPT_ALL_DAY          PropertyTag = 0x8008000B // PT_BOOLEAN
	PR_APPT_STATE_FLAGS      PropertyTag = 0x80090003 // PT_I4
	PR_APPT_RESP_TIME        PropertyTag = 0x800A0040 // PT_SYSTIME
	PR_APPT_RESP_REQUIRED    PropertyTag = 0x800B000B // PT_BOOLEAN
	PR_APPT_COUNTER_PROPOSAL PropertyTag = 0x800C000B // PT_BOOLEAN
	PR_APPT_TZ_DEFINITION    PropertyTag = 0x800D0102 // PT_BINARY
)

// Recipient types (PR_RECIPIENT_TYPE)
const (
	MAPI_TO   = 1
	MAPI_CC   = 2
	MAPI_BCC  = 3
	MAPI_ORIG = 0
	MAPI_P1   = 100
)

// Folder types (PR_FOLDER_TYPE)
const (
	FOLDER_GENERIC         = 0
	FOLDER_SEARCH          = 1
	FOLDER_PRIMARY         = 2
	FOLDER_DEFAULT_VIEW    = 3
	FOLDER_REMINDER        = 4
	FOLDER_OUTLOOK         = 5
	FOLDER_ROOT            = 6
	FOLDER_IPM_SUBTREE     = 7
	FOLDER_IPM_OUTBOX      = 8
	FOLDER_IPM_WASTEBASKET = 9
	FOLDER_IPM_SENTMAIL    = 10
	FOLDER_IPM_DRAFTS      = 11
	FOLDER_IPM_APPOINTMENT = 12
	FOLDER_IPM_CONTACT     = 13
	FOLDER_IPM_JOURNAL     = 14
	FOLDER_IPM_NOTE        = 15
	FOLDER_IPM_TASK        = 16
)

// Appointment state flags (PR_APPT_STATE_FLAGS)
const (
	APPT_STATE_MEETING   = 0x0001
	APPT_STATE_RECEIVED  = 0x0002
	APPT_STATE_CANCELLED = 0x0004
	APPT_STATE_RESPONDED = 0x0008
)

// ---------------------------------------------------------------------------
// Property Marshaling
// ---------------------------------------------------------------------------

var (
	ErrInvalidPropertyType = errors.New("invalid property type")
	ErrBufferTooSmall      = errors.New("buffer too small")
	ErrInvalidTag          = errors.New("invalid property tag")
)

// UnmarshalProperty reads a property value from a reader.
func UnmarshalProperty(r io.Reader) (PropertyValue, error) {
	var pv PropertyValue

	// Read property tag (4 bytes, little-endian)
	var tagBuf [4]byte
	if _, err := io.ReadFull(r, tagBuf[:]); err != nil {
		return pv, err
	}
	pv.Tag = PropertyTag(binary.LittleEndian.Uint32(tagBuf[:]))
	pv.Type = pv.Tag.Type()

	// Read value based on type
	var err error
	switch pv.Type {
	case PT_I2:
		var v int16
		err = binary.Read(r, binary.LittleEndian, &v)
		pv.Value = v
	case PT_I4:
		var v int32
		err = binary.Read(r, binary.LittleEndian, &v)
		pv.Value = v
	case PT_R4:
		var v float32
		err = binary.Read(r, binary.LittleEndian, &v)
		pv.Value = v
	case PT_R8:
		var v float64
		err = binary.Read(r, binary.LittleEndian, &v)
		pv.Value = v
	case PT_CURRENCY:
		var v int64
		err = binary.Read(r, binary.LittleEndian, &v)
		pv.Value = v
	case PT_APPTIME:
		var v float64
		err = binary.Read(r, binary.LittleEndian, &v)
		pv.Value = v
	case PT_I8:
		var v int64
		err = binary.Read(r, binary.LittleEndian, &v)
		pv.Value = v
	case PT_BOOLEAN:
		var v uint16
		err = binary.Read(r, binary.LittleEndian, &v)
		pv.Value = v != 0
	case PT_STRING8:
		pv.Value, err = readNullTerminatedString8(r)
	case PT_UNICODE:
		pv.Value, err = readNullTerminatedUnicode(r)
	case PT_SYSTIME:
		var ft uint64
		err = binary.Read(r, binary.LittleEndian, &ft)
		if err == nil {
			pv.Value = fileTimeToTime(ft)
		}
	case PT_CLSID:
		var guid [16]byte
		_, err = io.ReadFull(r, guid[:])
		pv.Value = guid
	case PT_BINARY:
		pv.Value, err = readBinary(r)
	case PT_ERROR:
		var v uint32
		err = binary.Read(r, binary.LittleEndian, &v)
		pv.Value = v

	// Multi-value types
	case PT_MV_I2:
		pv.Value, err = readMultiValueI2(r)
	case PT_MV_I4:
		pv.Value, err = readMultiValueI4(r)
	case PT_MV_R4:
		pv.Value, err = readMultiValueR4(r)
	case PT_MV_R8:
		pv.Value, err = readMultiValueR8(r)
	case PT_MV_CURRENCY:
		pv.Value, err = readMultiValueCurrency(r)
	case PT_MV_APPTIME:
		pv.Value, err = readMultiValueApptime(r)
	case PT_MV_I8:
		pv.Value, err = readMultiValueI8(r)
	case PT_MV_STRING8:
		pv.Value, err = readMultiValueString8(r)
	case PT_MV_UNICODE:
		pv.Value, err = readMultiValueUnicode(r)
	case PT_MV_SYSTIME:
		pv.Value, err = readMultiValueSysTime(r)
	case PT_MV_CLSID:
		pv.Value, err = readMultiValueCLSID(r)
	case PT_MV_BINARY:
		pv.Value, err = readMultiValueBinary(r)

	default:
		return pv, fmt.Errorf("%w: 0x%04X", ErrInvalidPropertyType, pv.Type)
	}

	return pv, err
}

// MarshalProperty writes a property value to a writer.
func MarshalProperty(w io.Writer, pv PropertyValue) error {
	// Write property tag
	if err := binary.Write(w, binary.LittleEndian, uint32(pv.Tag)); err != nil {
		return err
	}

	// Write value based on type
	switch pv.Type {
	case PT_I2:
		return binary.Write(w, binary.LittleEndian, pv.Value.(int16))
	case PT_I4:
		return binary.Write(w, binary.LittleEndian, pv.Value.(int32))
	case PT_R4:
		return binary.Write(w, binary.LittleEndian, pv.Value.(float32))
	case PT_R8:
		return binary.Write(w, binary.LittleEndian, pv.Value.(float64))
	case PT_CURRENCY:
		return binary.Write(w, binary.LittleEndian, pv.Value.(int64))
	case PT_APPTIME:
		return binary.Write(w, binary.LittleEndian, pv.Value.(float64))
	case PT_I8:
		return binary.Write(w, binary.LittleEndian, pv.Value.(int64))
	case PT_BOOLEAN:
		var v uint16
		if pv.Value.(bool) {
			v = 1
		}
		return binary.Write(w, binary.LittleEndian, v)
	case PT_STRING8:
		return writeNullTerminatedString8(w, pv.Value.(string))
	case PT_UNICODE:
		return writeNullTerminatedUnicode(w, pv.Value.(string))
	case PT_SYSTIME:
		t := pv.Value.(time.Time)
		return binary.Write(w, binary.LittleEndian, timeToFileTime(t))
	case PT_CLSID:
		guid := pv.Value.([16]byte)
		_, err := w.Write(guid[:])
		return err
	case PT_BINARY:
		return writeBinary(w, pv.Value.([]byte))

	// Multi-value types
	case PT_MV_I2:
		return writeMultiValueI2(w, pv.Value.([]int16))
	case PT_MV_I4:
		return writeMultiValueI4(w, pv.Value.([]int32))
	case PT_MV_R4:
		return writeMultiValueR4(w, pv.Value.([]float32))
	case PT_MV_R8:
		return writeMultiValueR8(w, pv.Value.([]float64))
	case PT_MV_CURRENCY:
		return writeMultiValueCurrency(w, pv.Value.([]int64))
	case PT_MV_APPTIME:
		return writeMultiValueApptime(w, pv.Value.([]float64))
	case PT_MV_I8:
		return writeMultiValueI8(w, pv.Value.([]int64))
	case PT_MV_STRING8:
		return writeMultiValueString8(w, pv.Value.([]string))
	case PT_MV_UNICODE:
		return writeMultiValueUnicode(w, pv.Value.([]string))
	case PT_MV_SYSTIME:
		return writeMultiValueSysTime(w, pv.Value.([]time.Time))
	case PT_MV_CLSID:
		return writeMultiValueCLSID(w, pv.Value.([][16]byte))
	case PT_MV_BINARY:
		return writeMultiValueBinary(w, pv.Value.([][]byte))

	default:
		return fmt.Errorf("%w: 0x%04X", ErrInvalidPropertyType, pv.Type)
	}
}

// PropertyRow represents a row of properties (used in tables).
type PropertyRow []PropertyValue

// PropertyRowSet represents a set of property rows.
type PropertyRowSet struct {
	Columns []PropertyTag
	Rows    []PropertyRow
}

// ---------------------------------------------------------------------------
// Helpers for reading/writing values
// ---------------------------------------------------------------------------

func readNullTerminatedString8(r io.Reader) (string, error) {
	var buf strings.Builder
	for {
		var b byte
		_, err := r.Read([]byte{b})
		if err != nil {
			return "", err
		}
		if b == 0 {
			break
		}
		buf.WriteByte(b)
	}
	return buf.String(), nil
}

func readNullTerminatedUnicode(r io.Reader) (string, error) {
	var runes []rune
	for {
		var c uint16
		if err := binary.Read(r, binary.LittleEndian, &c); err != nil {
			return "", err
		}
		if c == 0 {
			break
		}
		runes = append(runes, rune(c))
	}
	return string(runes), nil
}

func writeNullTerminatedString8(w io.Writer, s string) error {
	if _, err := w.Write([]byte(s)); err != nil {
		return err
	}
	_, err := w.Write([]byte{0})
	return err
}

func writeNullTerminatedUnicode(w io.Writer, s string) error {
	for _, r := range s {
		if err := binary.Write(w, binary.LittleEndian, uint16(r)); err != nil {
			return err
		}
	}
	return binary.Write(w, binary.LittleEndian, uint16(0))
}

func readBinary(r io.Reader) ([]byte, error) {
	var size uint32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func writeBinary(w io.Writer, data []byte) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func readMultiValueI2(r io.Reader) ([]int16, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([]int16, count)
	for i := uint32(0); i < count; i++ {
		var v int16
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

func readMultiValueI4(r io.Reader) ([]int32, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([]int32, count)
	for i := uint32(0); i < count; i++ {
		var v int32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

func readMultiValueR4(r io.Reader) ([]float32, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([]float32, count)
	for i := uint32(0); i < count; i++ {
		var v float32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

func readMultiValueR8(r io.Reader) ([]float64, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([]float64, count)
	for i := uint32(0); i < count; i++ {
		var v float64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

func readMultiValueCurrency(r io.Reader) ([]int64, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([]int64, count)
	for i := uint32(0); i < count; i++ {
		var v int64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

func readMultiValueApptime(r io.Reader) ([]float64, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([]float64, count)
	for i := uint32(0); i < count; i++ {
		var v float64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

func readMultiValueI8(r io.Reader) ([]int64, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([]int64, count)
	for i := uint32(0); i < count; i++ {
		var v int64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

func readMultiValueString8(r io.Reader) ([]string, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([]string, count)
	for i := uint32(0); i < count; i++ {
		s, err := readNullTerminatedString8(r)
		if err != nil {
			return nil, err
		}
		vals[i] = s
	}
	return vals, nil
}

func readMultiValueUnicode(r io.Reader) ([]string, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([]string, count)
	for i := uint32(0); i < count; i++ {
		s, err := readNullTerminatedUnicode(r)
		if err != nil {
			return nil, err
		}
		vals[i] = s
	}
	return vals, nil
}

func readMultiValueSysTime(r io.Reader) ([]time.Time, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([]time.Time, count)
	for i := uint32(0); i < count; i++ {
		var ft uint64
		if err := binary.Read(r, binary.LittleEndian, &ft); err != nil {
			return nil, err
		}
		vals[i] = fileTimeToTime(ft)
	}
	return vals, nil
}

func readMultiValueCLSID(r io.Reader) ([][16]byte, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([][16]byte, count)
	for i := uint32(0); i < count; i++ {
		var guid [16]byte
		if _, err := io.ReadFull(r, guid[:]); err != nil {
			return nil, err
		}
		vals[i] = guid
	}
	return vals, nil
}

func readMultiValueBinary(r io.Reader) ([][]byte, error) {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	vals := make([][]byte, count)
	for i := uint32(0); i < count; i++ {
		data, err := readBinary(r)
		if err != nil {
			return nil, err
		}
		vals[i] = data
	}
	return vals, nil
}

func writeMultiValueI2(w io.Writer, vals []int16) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, v := range vals {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return nil
}

func writeMultiValueI4(w io.Writer, vals []int32) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, v := range vals {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return nil
}

func writeMultiValueR4(w io.Writer, vals []float32) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, v := range vals {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return nil
}

func writeMultiValueR8(w io.Writer, vals []float64) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, v := range vals {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return nil
}

func writeMultiValueCurrency(w io.Writer, vals []int64) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, v := range vals {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return nil
}

func writeMultiValueApptime(w io.Writer, vals []float64) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, v := range vals {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return nil
}

func writeMultiValueI8(w io.Writer, vals []int64) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, v := range vals {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return nil
}

func writeMultiValueString8(w io.Writer, vals []string) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, s := range vals {
		if err := writeNullTerminatedString8(w, s); err != nil {
			return err
		}
	}
	return nil
}

func writeMultiValueUnicode(w io.Writer, vals []string) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, s := range vals {
		if err := writeNullTerminatedUnicode(w, s); err != nil {
			return err
		}
	}
	return nil
}

func writeMultiValueSysTime(w io.Writer, vals []time.Time) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, t := range vals {
		if err := binary.Write(w, binary.LittleEndian, timeToFileTime(t)); err != nil {
			return err
		}
	}
	return nil
}

func writeMultiValueCLSID(w io.Writer, vals [][16]byte) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, guid := range vals {
		if _, err := w.Write(guid[:]); err != nil {
			return err
		}
	}
	return nil
}

func writeMultiValueBinary(w io.Writer, vals [][]byte) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vals))); err != nil {
		return err
	}
	for _, data := range vals {
		if err := writeBinary(w, data); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// FILETIME conversion (100ns intervals since Jan 1, 1601 UTC)
// ---------------------------------------------------------------------------

const fileTimeEpochOffset = 116444736000000000 // 100ns intervals from 1601 to 1970

func fileTimeToTime(ft uint64) time.Time {
	if ft == 0 {
		return time.Time{}
	}
	ns := (ft - fileTimeEpochOffset) * 100
	return time.Unix(0, int64(ns)).UTC()
}

func timeToFileTime(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	ns := uint64(t.UnixNano())
	return ns/100 + fileTimeEpochOffset
}

// ---------------------------------------------------------------------------
// Tag helpers
// ---------------------------------------------------------------------------

// PT_UNSPECIFIED tags used for column requests
func UnspecifiedTag(id uint16) PropertyTag {
	return PropertyTagFromIDAndType(id, PT_UNSPECIFIED)
}

// IsUnspecified returns true if the tag has PT_UNSPECIFIED type.
func (pv PropertyValue) IsUnspecified() bool {
	return pv.Type == PT_UNSPECIFIED
}

// ---------------------------------------------------------------------------
// String representation
// ---------------------------------------------------------------------------

var typeNames = map[PropertyType]string{
	PT_UNSPECIFIED: "PT_UNSPECIFIED",
	PT_NULL:        "PT_NULL",
	PT_I2:          "PT_I2",
	PT_I4:          "PT_I4",
	PT_R4:          "PT_R4",
	PT_R8:          "PT_R8",
	PT_CURRENCY:    "PT_CURRENCY",
	PT_APPTIME:     "PT_APPTIME",
	PT_ERROR:       "PT_ERROR",
	PT_BOOLEAN:     "PT_BOOLEAN",
	PT_OBJECT:      "PT_OBJECT",
	PT_I8:          "PT_I8",
	PT_STRING8:     "PT_STRING8",
	PT_UNICODE:     "PT_UNICODE",
	PT_SYSTIME:     "PT_SYSTIME",
	PT_CLSID:       "PT_CLSID",
	PT_BINARY:      "PT_BINARY",
	PT_MV_I2:       "PT_MV_I2",
	PT_MV_I4:       "PT_MV_I4",
	PT_MV_R4:       "PT_MV_R4",
	PT_MV_R8:       "PT_MV_R8",
	PT_MV_CURRENCY: "PT_MV_CURRENCY",
	PT_MV_APPTIME:  "PT_MV_APPTIME",
	PT_MV_I8:       "PT_MV_I8",
	PT_MV_STRING8:  "PT_MV_STRING8",
	PT_MV_UNICODE:  "PT_MV_UNICODE",
	PT_MV_SYSTIME:  "PT_MV_SYSTIME",
	PT_MV_CLSID:    "PT_MV_CLSID",
	PT_MV_BINARY:   "PT_MV_BINARY",
}

func (pt PropertyType) String() string {
	if name, ok := typeNames[pt]; ok {
		return name
	}
	return fmt.Sprintf("PT_UNKNOWN(0x%04X)", uint16(pt))
}

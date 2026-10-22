package rop

// Property value encoding for ROP streams (MS-OXCDATA section 2.11). The ROP
// stream forms differ from the NSPI forms: binary values carry a u16 length,
// strings are NUL-terminated without a WCOUNT prefix, and there are no ABK
// presence bytes. gromox configures the ROP encoder with EXT_FLAG_UTF16, so
// PT_UNICODE is UTF-16LE throughout.

import (
	"math"
	"time"
	"unicode/utf16"
)

// Property type constants (MS-OXCDATA 2.11.1.2 / mapidefs.h).
const (
	ptUnspecified = 0x0000
	ptNull        = 0x0001
	ptI2          = 0x0002
	ptLong        = 0x0003
	ptR4          = 0x0004
	ptDouble      = 0x0005
	ptCurrency    = 0x0006
	ptAppTime     = 0x0007
	ptError       = 0x000A
	ptBoolean     = 0x000B
	ptObject      = 0x000D
	ptI8          = 0x0014
	ptString8     = 0x001E
	ptUnicode     = 0x001F
	ptClsid       = 0x0048
	ptSystime     = 0x0040
	ptSReSTRICK   = 0x00FD
	ptActions     = 0x00FE
	ptBinary      = 0x0102
	mvFlag        = 0x1000
	ptMVString8   = 0x101E
	ptMVUnicode   = 0x101F
	ptMVBinary    = 0x1102
)

func propType(tag uint32) uint16               { return uint16(tag & 0xFFFF) }
func propID(tag uint32) uint16                 { return uint16(tag >> 16) }
func propTag(id, typ uint16) uint32            { return uint32(id)<<16 | uint32(typ) }
func changeType(tag uint32, typ uint16) uint32 { return tag&0xFFFF0000 | uint32(typ) }

// Property tags used by the store side (MS-OXPROPS / gromox mapitags.hpp).
// The values are exact; the ROP handlers and the NSPI GAL share them.
const (
	pidTagFolderID          = 0x67480014 // PT_I8
	pidTagParentFolderID    = 0x67490014 // PT_I8
	pidTagMID               = 0x674A0014 // PT_I8
	prObjectType            = 0x0FFE0003
	prEntryID               = 0x0FFF0102
	prParentEntryID         = 0x0E090102
	prRecordKey             = 0x0FFA0102
	prSearchKey             = 0x300B0102
	prMessageClassA         = 0x001A001E
	prMessageClass          = 0x001A001F
	prSubject               = 0x0037001F
	prSubjectPrefix         = 0x003D001F
	prNormalizedSubject     = 0x0E1D001F
	prImportance            = 0x00170003
	prSensitivity           = 0x00360003
	prClientSubmitTime      = 0x00390040
	prSentRepresentingName  = 0x0042001F
	prSentRepresentingEMail = 0x0065001F
	prDisplayTo             = 0x0E04001F
	prDisplayCC             = 0x0E03001F
	prDisplayBCC            = 0x0E02001F
	prMessageFlags          = 0x0E070003
	prRead                  = 0x0E69000B
	prMessageSize           = 0x0E080003
	prMessageDeliveryTime   = 0x0E060040
	prHasAttachments        = 0x0E1B000B
	prInternetMessageID     = 0x1035001F
	prBody                  = 0x1000001F
	prBodyHTML              = 0x10130102
	prSenderName            = 0x0C1A001F
	prSenderEmailAddress    = 0x0C1F001F
	prFolderType            = 0x36010003
	prContentCount          = 0x36020003
	prContentUnread         = 0x36030003
	prSubfolders            = 0x360A000B
	prContainerClass        = 0x3613001F
	prDisplayName           = 0x3001001F
	prLocaleID              = 0x3FF10003
	prDefaultStore          = 0x3400000B
	prStoreSupportMask      = 0x340D0003
	prStoreEntryID          = 0x0FFB0102
	prMdbProvider           = 0x34140102
	prSmtpAddress           = 0x39FE001F
	prDisplayType           = 0x39000003
	prEmailAddress          = 0x3003001F
	prAddrType              = 0x3002001F
	prMapiIdentity          = 0x3FFD0102
	prLastModificationTime  = 0x30080040
	prCreationTime          = 0x30070040

	// Contact item properties (MS-OXPROPS / mapitags.h). Outlook reads and
	// writes these fixed tags on an IPM.Contact item; the e-mail addresses
	// beyond the first live in named properties instead.
	prFileUnder                 = 0x8005001F
	prGivenName                 = 0x3A06001F
	prSurname                   = 0x3A11001F
	prMiddleName                = 0x3A44001F
	prDisplayNamePrefix         = 0x3A45001F
	prNickname                  = 0x3A4F001F
	prCompanyName               = 0x3A16001F
	prTitle                     = 0x3A17001F
	prDepartmentName            = 0x3A18001F
	prOfficeLocation            = 0x3A19001F
	prBusinessTelephoneNumber   = 0x3A08001F
	prBusinessTelephone2        = 0x3A1B001F
	prHomeTelephoneNumber       = 0x3A09001F
	prHomeTelephone2            = 0x3A2F001F
	prMobileTelephoneNumber     = 0x3A1C001F
	prBusinessFaxNumber         = 0x3A24001F
	prHomeFaxNumber             = 0x3A25001F
	prPagerTelephoneNumber      = 0x3A21001F
	prCarTelephoneNumber        = 0x3A1E001F
	prPrimaryTelephoneNumber    = 0x3A1A001F
	prBusinessAddressStreet     = 0x3A1D001F
	prBusinessAddressCity       = 0x3A27001F
	prBusinessAddressState      = 0x3A28001F
	prBusinessAddressPostalCode = 0x3A2A001F
	prBusinessAddressCountry    = 0x3A26001F
	prHomeAddressStreet         = 0x3A5D001F
	prHomeAddressCity           = 0x3A5E001F
	prHomeAddressState          = 0x3A5F001F
	prHomeAddressPostalCode     = 0x3A60001F
	prHomeAddressCountry        = 0x3A5C001F
	prBirthday                  = 0x3A420040
	prWeddingAnniversary        = 0x3A410040
	prBusinessHomePage          = 0x3A51001F
	prIPMContactEntryID         = 0x36D00102
	prAccess                    = 0x0FF40003
	prAccessLevel               = 0x0FF70003
)

// psetidAddress is the property-set GUID for the contact e-mail properties
// (PidLidEmail1EmailAddress and friends, MS-OXPROPS 2.2.1.1).
const psetidAddress = "00062004-0000-0000-c000-000000000046"

// Well-known GUIDs in canonical string form.
const (
	muidStoreWrap      = "10bba138-e505-1a10-a1bb-08002b2a56c2" // STORE provider UID on the wire
	guidOUserDirectory = "c840a7dc-42c0-1a10-b4b9-08002b2fe182" // OmsNspiExchange (NSPI server GUID)
)

// propVal is one property value for a row or a GetProperties reply.
//
//	val == nil  the property is absent (a FLAGGED cell reports ecNotFound)
//	otherwise   val is one of: string, []byte, bool, int, int32, int64,
//	            uint16, uint32, uint64, float64, time.Time, []uint16,
//	            []uint32, []uint64, []string, [][]byte
type propVal struct {
	tag uint32
	val any
}

// encodePropVal writes the value body for type(tag) exactly as p_propval does
// in a non-ABK (ROP) stream. It must not be called with val == nil.
func encodePropVal(w *writer, tag uint32, val any) error {
	switch propType(tag) {
	case ptI2:
		w.u16(uint16(asInt64(val)))
	case ptLong, ptError:
		w.u32(uint32(asInt64(val)))
	case ptR4:
		f := val.(float64)
		w.buf = append(w.buf, float32toLE(float32(f), make([]byte, 4))...)
	case ptDouble, ptAppTime:
		w.buf = append(w.buf, float64toLE(val.(float64), make([]byte, 8))...)
	case ptBoolean:
		if asBool(val) {
			w.u8(1)
		} else {
			w.u8(0)
		}
	case ptI8, ptCurrency:
		w.u64(uint64(asInt64(val)))
	case ptSystime:
		w.u64(filetimeTicks(val))
	case ptString8:
		w.str(asString(val))
	case ptUnicode:
		w.wstr(asString(val))
	case ptClsid:
		if g, ok := val.(string); ok {
			w.guid(g)
		} else {
			w.guid("")
		}
	case ptBinary:
		if err := w.bin(asBytes(val)); err != nil {
			return err
		}
	case ptMVString8:
		w.u32(uint32(len(asStringSlice(val))))
		for _, s := range asStringSlice(val) {
			w.str(s)
		}
	case ptMVUnicode:
		w.u32(uint32(len(asStringSlice(val))))
		for _, s := range asStringSlice(val) {
			w.wstr(s)
		}
	case ptMVBinary:
		items := val.([][]byte)
		w.u32(uint32(len(items)))
		for _, b := range items {
			if err := w.bin(b); err != nil {
				return err
			}
		}
	case ptMVLongI4:
		items := toUint32Slice(val)
		w.u32(uint32(len(items)))
		for _, v := range items {
			w.u32(v)
		}
	case ptMVI2:
		items := toUint16Slice(val)
		w.u32(uint32(len(items)))
		for _, v := range items {
			w.u16(v)
		}
	case ptMVI8, ptMVSystime, ptMVCurrency:
		items := toUint64Slice(val)
		w.u32(uint32(len(items)))
		for _, v := range items {
			w.u64(v)
		}
	default:
		return formatError
	}
	return nil
}

const (
	ptMVLong     = 0x1003 // PT_MV_LONG
	ptMVSystime  = 0x1040 // PT_MV_SYSTIME
	ptMVLongI4   = ptMVLong
	ptMVI2       = 0x1002
	ptMVI8       = 0x1014
	ptMVCurrency = 0x1006
	ptMVDouble   = 0x1005
	ptMVAppTime  = 0x1007
)

// writePropRow encodes one PROPERTY_ROW given the column list. Exactly like
// gromox cu_propvals_to_row: the row is FLAGGED when any column is missing,
// NONE when every column has a value. Every cell in a FLAGGED row is
// AVAILABLE or ERROR(ecNotFound); UNAVAILABLE is never produced.
func writePropRow(w *writer, columns []uint32, props map[uint32]any) error {
	all := true
	for _, c := range columns {
		if _, ok := props[c]; !ok {
			all = false
			break
		}
	}
	if all {
		w.u8(propRowFlagNone)
		for _, c := range columns {
			if err := encodePropVal(w, c, props[c]); err != nil {
				return err
			}
		}
		return nil
	}
	w.u8(propRowFlagFlagged)
	for _, c := range columns {
		v, ok := props[c]
		if !ok {
			// FLAGGED cell, error code ecNotFound follows the flag byte.
			w.u8(flaggedFlagError)
			w.u32(ecNotFound)
			continue
		}
		w.u8(flaggedFlagAvailable)
		if err := encodePropVal(w, c, v); err != nil {
			return err
		}
	}
	return nil
}

// writeGetPropsRow encodes the GETPROPERTIESSPECIFIC_RESPONSE PROPERTY_ROW:
// a flagged row of only the requested properties.
func writeGetPropsRow(w *writer, columns []uint32, props map[uint32]any) error {
	w.u8(propRowFlagFlagged)
	for _, c := range columns {
		v, ok := props[c]
		if !ok {
			w.u8(flaggedFlagError)
			w.u32(ecNotFound)
			continue
		}
		w.u8(flaggedFlagAvailable)
		if err := encodePropVal(w, c, v); err != nil {
			return err
		}
	}
	return nil
}

// tpropval writes a TAGGED_PROPVAL_ARRAY (u16 count + tag + value per item),
// used by GETPROPERTIESALL_RESPONSE.
func writeTPropval(w *writer, props map[uint32]any) error {
	tags := make([]uint32, 0, len(props))
	for t := range props {
		tags = append(tags, t)
	}
	if len(tags) > math.MaxUint16 {
		return formatError
	}
	w.u16(uint16(len(tags)))
	for _, t := range tags {
		w.u32(t)
		if err := encodePropVal(w, t, props[t]); err != nil {
			return err
		}
	}
	return nil
}

// filetimeTicks converts val (time.Time or int64 raw ticks) to FILETIME ticks,
// the 100 ns intervals since 1601-01-01 that MAPI uses for PT_SYSTIME.
func filetimeTicks(val any) uint64 {
	switch v := val.(type) {
	case time.Time:
		return uint64(v.UnixNano()/100 + filetimeEpochOffset)
	case int64:
		return uint64(v)
	case uint64:
		return v
	}
	return 0
}

const filetimeEpochOffset = 116444736000000000 // 1601-01-01..1970-01-01 in 100ns

// ---- type coercion helpers -------------------------------------------------

func asInt64(v any) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int8:
		return int64(t)
	case int16:
		return int64(t)
	case int32:
		return int64(t)
	case int64:
		return t
	case uint8:
		return int64(t)
	case uint16:
		return int64(t)
	case uint32:
		return int64(t)
	case uint64:
		return int64(t)
	}
	return 0
}

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int:
		return t != 0
	case uint8:
		return t != 0
	}
	return false
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asBytes(v any) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	return nil
}

func asStringSlice(v any) []string {
	if s, ok := v.([]string); ok {
		return s
	}
	return nil
}

func toUint32Slice(v any) []uint32 {
	switch t := v.(type) {
	case []uint32:
		return t
	case []int32:
		out := make([]uint32, len(t))
		for i, x := range t {
			out[i] = uint32(x)
		}
		return out
	case []int:
		out := make([]uint32, len(t))
		for i, x := range t {
			out[i] = uint32(x)
		}
		return out
	case []uint16:
		out := make([]uint32, len(t))
		for i, x := range t {
			out[i] = uint32(x)
		}
		return out
	}
	return nil
}

func toUint16Slice(v any) []uint16 {
	if s, ok := v.([]uint16); ok {
		return s
	}
	return nil
}

func toUint64Slice(v any) []uint64 {
	switch t := v.(type) {
	case []uint64:
		return t
	case []int64:
		out := make([]uint64, len(t))
		for i, x := range t {
			out[i] = uint64(x)
		}
		return out
	case []uint32:
		out := make([]uint64, len(t))
		for i, x := range t {
			out[i] = uint64(x)
		}
		return out
	}
	return nil
}

// utf16LE returns the UTF-16LE bytes of s, without a terminator.
func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func float32toLE(v float32, b []byte) []byte {
	u := math.Float32bits(v)
	b[0] = byte(u)
	b[1] = byte(u >> 8)
	b[2] = byte(u >> 16)
	b[3] = byte(u >> 24)
	return b
}

func float64toLE(v float64, b []byte) []byte {
	u := math.Float64bits(v)
	for i := 0; i < 8; i++ {
		b[i] = byte(u >> (8 * i))
	}
	return b
}

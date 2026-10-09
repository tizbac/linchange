package nspi

// Low-level pack/unpack machinery for the NSPI stream (MS-OXNSPI over the
// MAPI/HTTP transport). All integers are little-endian.
//
// The NSPI stream differs from the ROP stream (internal/mapihttp/rop) in
// three ways, all confirmed against the gromox reference implementation:
//
//   - binary values carry a u32 length (EXT_FLAG_WCOUNT), not u16;
//   - property values that are strings, binaries or multi-value arrays carry
//     a 0xFF "presence" prefix byte in the address-book ("ABK") serialization;
//     inside MV string/binary arrays every element has its own 0xFF presence
//     byte as well;
//   - every property tag array uses a u32 count, where the ROP stream uses u16.
//
// Requests arrive with the client's chosen serialization (the ABK mode is
// only switched on by requests that carry property values: getmatches,
// modprops, seekentries, and the string/binary props inside restrictions);
// responses are always written in the ABK form gromox uses for NSPI rows.

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf16"
)

// formatError is the "the request body is invalid" condition that the
// transport maps to resp_code invalid_rq_body.
var formatError = errors.New("mapi/nspi: malformed NSPI data")

// Property type constants (MS-OXCDATA 2.11.1.2).
const (
	ptUnspecified = 0x0000
	ptNull        = 0x0001
	ptShort       = 0x0002
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
	ptSystime     = 0x0040
	ptClsid       = 0x0048
	ptBinary      = 0x0102
	mvFlag        = 0x1000
	ptMVShort     = 0x1002
	ptMVLong      = 0x1003
	ptMVR4        = 0x1004
	ptMVDouble    = 0x1005
	ptMVCurrency  = 0x1006
	ptMVAppTime   = 0x1007
	ptMVI8        = 0x1014
	ptMVString8   = 0x101E
	ptMVUnicode   = 0x101F
	ptMVSystime   = 0x1040
	ptMVClsid     = 0x1048
	ptMVBinary    = 0x1102
)

func propType(tag uint32) uint16 { return uint16(tag & 0xFFFF) }
func propID(tag uint32) uint16   { return uint16(tag >> 16) }
func changeType(tag uint32, typ uint16) uint32 {
	return (tag & 0xFFFF0000) | uint32(typ)
}

// NSPI property identifiers used by the GAL here; the values are the MAPI
// property tags (MS-OXPROPS) the reference implementation serves for address
// book objects.
const (
	prANR                      = 0x360C001F // PR_ANR
	prANRA                     = 0x360C001E // PR_ANR_A
	prContainerFlags           = 0x36000003
	prDepth                    = 0x30050003
	prComment                  = 0x3004001F
	prDisplayName              = 0x3001001F
	prDisplayNameA             = 0x3001001E
	prAddrType                 = 0x3002001F
	prEmailAddress             = 0x3003001F
	prSearchKey                = 0x300B0102
	prObjectType               = 0x0FFE0003
	prEntryID                  = 0x0FFF0102
	prRecordKey                = 0x0FFA0102
	prInstanceKey              = 0x0FF60102
	prMappingSignature         = 0x0FF80102
	prSmtpAddress              = 0x39FE001F
	prEmABDisplayNamePrintable = 0x39FF001F
	prDisplayType              = 0x39000003
	prDisplayTypeEx            = 0x39050003
	prTemplateID               = 0x39020102 // PR_TEMPLATEID
	prAccount                  = 0x3A00001F
	prOriginalEntryID          = 0x3A0A0102
	prBusinessTelephoneNumber  = 0x3A08001F // PR_BUSINESS_TELEPHONE_NUMBER
	prNickname                 = 0x3A4F001F // PR_NICKNAME
	prTitle                    = 0x3A17001F
	prCompanyName              = 0x3A16001F
	prDepartmentName           = 0x3A18001F
	prOfficeLocation           = 0x3A19001F
	prPrimaryTelephoneNumber   = 0x3A1A001F
	prMobileTelephoneNumber    = 0x3A1C001F
	prHomeAddressStreet        = 0x3A5D001F // PR_HOME_ADDRESS_STREET
	prSendRichInfo             = 0x3A20000B
	prTransmitableDisplayName  = 0x3A20001F
	prCreationTime             = 0x30070040 // PR_CREATION_TIME

	// Address book (PR_EMS_AB_*) tags this GAL serves.
	prEmABHomeMDB             = 0x8006001F
	prEmABNetworkAddress      = 0x8170101F // PR_EMS_AB_NETWORK_ADDRESS (MV)
	prEmABProxyAddresses      = 0x800F101F // PR_EMS_AB_PROXY_ADDRESSES (MV)
	prEmABObjectGUID          = 0x8C6D0102
	prEmABPhoneticDisplayName = 0x8C92001F
	prEmABThumbnailPhoto      = 0x8C9E0102
	prEmABIsMaster            = 0xFFFB000B
	prEmABContainerID         = 0xFFFD0003
	prEmABMember              = 0x8009000D
	prEmABPublicDelegates     = 0x8015000D

	// The 8-bit twins of the string tags above. A client that asks in a
	// non-Unicode code page asks for these, and the reference answers the
	// pair from one directory value.
	prAddrTypeA          = 0x3002001E
	prEmailAddressA      = 0x3003001E
	prSmtpAddressA       = 0x39FE001E
	prPrintableA         = 0x39FF001E
	prAccountA           = 0x3A00001E
	prTransmitableNameA  = 0x3A20001E
	prHomeMdbA           = 0x8006001E
	prNetworkAddressA    = 0x8170101E
	prProxyAddressesA    = 0x800F101E
	prBusinessTelA       = 0x3A08001E
	prNicknameA          = 0x3A4F001E
	prTitleA             = 0x3A17001E
	prCompanyNameA       = 0x3A16001E
	prDepartmentNameA    = 0x3A18001E
	prOfficeLocationA    = 0x3A19001E
	prPrimaryTelephonA   = 0x3A1A001E
	prMobileTelephonA    = 0x3A1C001E
	prHomeAddressStreetA = 0x3A5D001E
)

// Display types (MS-OXNSPI 2.2.1.6) and object types.
const (
	dtMailuser  = 0
	dtContainer = 0x100
	objMailuser = 6
)

// NSPI flags the implementation honours (common_util.hpp of the reference).
const (
	fEphID                  = 0x2
	nspiUnicodeStrings      = 0x4
	nspiAddressCreationTpls = 0x2
	fAnonymousLogin         = 0x20
	nspiUnicodeProptypes    = 0x80000000
)

// GetTemplateInfo flags (nsp_interface.cpp of the reference).
const (
	tiTemplate = 0x1
	tiScript   = 0x4
)

// STAT sort types the address book accepts; anything else makes GetMatches,
// QueryRows, ResortRestriction, SeekEntries and UpdateStat fail with
// ecNotSupported because the GAL has one fixed order.
const (
	sortTypeDisplayName   = 0x000
	sortTypePhoneticName  = 0x003
	sortTypeDisplayNameRO = 0x3E8
	sortTypeDisplayNameW  = 0x3E9
)

// Address book container flags (MS-OXNSPI 2.2.1.1).
const (
	abRecipients    = 0x1
	abSubcontainers = 0x2
	abUnmodifiable  = 0x8
)

// Code pages. CP_WINUNICODE is the one a client must not send: every
// operation that would have to pick a string flavour refuses it, which is
// how MS-OXNSPI tells a client to ask for Unicode property types instead.
const (
	cpACP       = 0
	codePageUTF = 1200
)

// STAT minid special values (MS-OXNSPI 2.2.1.5). The same numbers double as
// the MinidStatus a resolver reports: unresolved and ambiguous are distinct
// meanings of the same reserved values.
const (
	beginningOfTable = 0
	currentOfTable   = 1
	endOfTable       = 2

	unresolvedMid = 0 // MinidStatus: no recipient matched
	ambiguousMid  = 1 // MinidStatus: more than one recipient matched
	resolvedMid   = 2 // MinidStatus: exactly one recipient matched
)

const (
	// mids range starts after the small reserved values.
	midBase = 0x10
	// The GAL root container id, matching the special root node of the
	// reference address book tree.
	galContainerID = 0x0F
)

// muidEMSAB is the address book provider GUID (GUID_NSPI).
const muidEMSAB = "c840a7dc-42c0-1a10-b4b9-08002b2fe182"

// MAPI error/status codes used in responses (mapierr.hpp).
const (
	ecSuccess         = 0x00000000
	ecUnbindSuccess   = 0x00000001 // MAPI_E_UNBINDSUCCESS, the status Unbind reports
	ecServerOOM       = 0x000003F0
	ecBufferTooSmall  = 0x0000047D
	ecWarnWithErrors  = 0x00040380
	ecError           = 0x80004005
	ecNotSupported    = 0x80040102
	ecInvalidObject   = 0x80040108
	ecNotFound        = 0x8004010F
	ecLoginFailure    = 0x80040111 // MAPI_E_LOGON_FAILED
	ecFailOneProvider = 0x8004011D // MAPI_E_FAILONEPROVIDER
	ecUnknownCPID     = 0x8004011E // MAPI_E_UNKNOWN_CPID
	ecUnknownLCID     = 0x8004011F // MAPI_E_UNKNOWN_LCID
	ecTableTooBig     = 0x80040403
	ecInvalidBookmark = 0x80040405
	ecAccessDenied    = 0x80070005
	ecInvalidParam    = 0x80070057
)

type reader struct {
	buf []byte
	off int
}

func newReader(b []byte) *reader { return &reader{buf: b} }

func (r *reader) remaining() int { return len(r.buf) - r.off }

func (r *reader) u8() (uint8, error) {
	if r.off+1 > len(r.buf) {
		return 0, formatError
	}
	v := r.buf[r.off]
	r.off++
	return v, nil
}

func (r *reader) u16() (uint16, error) {
	if r.off+2 > len(r.buf) {
		return 0, formatError
	}
	v := binary.LittleEndian.Uint16(r.buf[r.off:])
	r.off += 2
	return v, nil
}

func (r *reader) u32() (uint32, error) {
	if r.off+4 > len(r.buf) {
		return 0, formatError
	}
	v := binary.LittleEndian.Uint32(r.buf[r.off:])
	r.off += 4
	return v, nil
}

func (r *reader) i32() (int32, error) {
	v, err := r.u32()
	return int32(v), err
}

func (r *reader) u64() (uint64, error) {
	if r.off+8 > len(r.buf) {
		return 0, formatError
	}
	v := binary.LittleEndian.Uint64(r.buf[r.off:])
	r.off += 8
	return v, nil
}

func (r *reader) bytes(n int) ([]byte, error) {
	if n < 0 || r.off+n > len(r.buf) {
		return nil, formatError
	}
	v := r.buf[r.off : r.off+n]
	r.off += n
	return v, nil
}

func (r *reader) skip(n int) error {
	_, err := r.bytes(n)
	return err
}

// bin reads a u32-length-prefixed binary blob (the NSPI/WCOUNT form).
func (r *reader) bin() ([]byte, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	return r.bytes(int(n))
}

// guid reads a 16-byte GUID in MAPI wire order and returns the canonical
// dash-separated string.
func (r *reader) guid() (string, error) {
	raw, err := r.bytes(16)
	if err != nil {
		return "", err
	}
	return guidToString(raw), nil
}

// zstr8 reads a NUL-terminated 8-bit string.
func (r *reader) zstr8() (string, error) {
	for i := r.off; i < len(r.buf); i++ {
		if r.buf[i] == 0 {
			s := string(r.buf[r.off:i])
			r.off = i + 1
			return s, nil
		}
	}
	return "", formatError
}

// zwstr reads a NUL-terminated UTF-16LE string.
func (r *reader) zwstr() (string, error) {
	if (len(r.buf)-r.off)%2 != 0 {
		return "", formatError
	}
	for i := r.off; i+1 < len(r.buf); i += 2 {
		if r.buf[i] == 0 && r.buf[i+1] == 0 {
			units := make([]uint16, 0, (i-r.off)/2)
			for j := r.off; j < i; j += 2 {
				units = append(units, binary.LittleEndian.Uint16(r.buf[j:]))
			}
			s := string(utf16.Decode(units))
			r.off = i + 2
			return s, nil
		}
	}
	return "", formatError
}

// proptags reads a count-prefixed array of property tags. size selects the
// count width: 2 reads a u16 count, any other value a u32 count.
func (r *reader) proptags(size int) ([]uint32, error) {
	var n uint64
	switch size {
	case 2:
		v, err := r.u16()
		if err != nil {
			return nil, err
		}
		n = uint64(v)
	default:
		v, err := r.u32()
		if err != nil {
			return nil, err
		}
		n = uint64(v)
	}
	if n > uint64(r.remaining()/4) {
		return nil, formatError
	}
	out := make([]uint32, 0, n)
	for i := uint64(0); i < n; i++ {
		t, err := r.u32()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// str8Array reads a u32-count array of NUL-terminated 8-bit strings. When
// abk is set, each element carries a 0xFF presence byte (absent elements
// decode to "").
func (r *reader) str8Array(abk bool) ([]string, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	// Every element occupies at least one byte (its terminator), so the
	// remaining buffer bounds the count and a hostile one cannot turn into
	// a huge allocation.
	if n > uint32(r.remaining()) {
		return nil, formatError
	}
	out := make([]string, 0, n)
	for i := uint32(0); i < n; i++ {
		if abk {
			p, err := r.u8()
			if err != nil {
				return nil, err
			}
			if p == 0 {
				out = append(out, "")
				continue
			}
			if p != 0xFF {
				return nil, formatError
			}
		}
		s, err := r.zstr8()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// wstrArray reads a u32-count array of NUL-terminated UTF-16LE strings. When
// abk is set, each element carries a 0xFF presence byte.
func (r *reader) wstrArray(abk bool) ([]string, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	if n > uint32(r.remaining()) {
		return nil, formatError
	}
	out := make([]string, 0, n)
	for i := uint32(0); i < n; i++ {
		if abk {
			p, err := r.u8()
			if err != nil {
				return nil, err
			}
			if p == 0 {
				out = append(out, "")
				continue
			}
			if p != 0xFF {
				return nil, formatError
			}
		}
		s, err := r.zwstr()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// binArray reads a u32-count array of u32-length-prefixed binaries.
func (r *reader) binArray(abk bool) ([][]byte, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	if n > uint32(r.remaining()) {
		return nil, formatError
	}
	out := make([][]byte, 0, n)
	for i := uint32(0); i < n; i++ {
		if abk {
			p, err := r.u8()
			if err != nil {
				return nil, err
			}
			if p == 0 {
				out = append(out, nil)
				continue
			}
			if p != 0xFF {
				return nil, formatError
			}
		}
		b, err := r.bin()
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// decPropVal reads one property value of the given type. In ABK mode
// (used for NSPI requests that carry values), string, binary and MV values
// are preceded by a presence byte: 0x00 means absent and yields a nil value,
// 0xFF means present. MV string/binary elements carry their own presence
// byte, handled inside the array readers.
func (r *reader) decPropVal(typ uint16, abk bool) (any, error) {
	if abk && (typ == ptString8 || typ == ptUnicode || typ == ptBinary || typ&mvFlag != 0) {
		p, err := r.u8()
		if err != nil {
			return nil, err
		}
		if p == 0 {
			return nil, nil
		}
		if p != 0xFF {
			return nil, formatError
		}
	}
	switch typ {
	case ptShort:
		return r.u16()
	case ptLong, ptError:
		return r.u32()
	case ptR4:
		b, err := r.bytes(4)
		if err != nil {
			return nil, err
		}
		return math.Float32frombits(binary.LittleEndian.Uint32(b)), nil
	case ptDouble, ptAppTime:
		b, err := r.bytes(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
	case ptCurrency, ptSystime, ptI8:
		return r.u64()
	case ptBoolean:
		return r.u8()
	case ptString8:
		return r.zstr8()
	case ptUnicode:
		return r.zwstr()
	case ptClsid:
		return r.guid()
	case ptBinary:
		return r.bin()
	case ptUnspecified:
		sub, err := r.u16()
		if err != nil {
			return nil, err
		}
		return r.decPropVal(sub, abk)
	case ptMVShort:
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
	case ptMVLong:
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
	case ptMVR4:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		out := make([]float32, 0, n)
		for i := uint32(0); i < n; i++ {
			b, err := r.bytes(4)
			if err != nil {
				return nil, err
			}
			out = append(out, math.Float32frombits(binary.LittleEndian.Uint32(b)))
		}
		return out, nil
	case ptMVDouble, ptMVAppTime:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		out := make([]float64, 0, n)
		for i := uint32(0); i < n; i++ {
			b, err := r.bytes(8)
			if err != nil {
				return nil, err
			}
			out = append(out, math.Float64frombits(binary.LittleEndian.Uint64(b)))
		}
		return out, nil
	case ptMVCurrency, ptMVSystime, ptMVI8:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		out := make([]int64, 0, n)
		for i := uint32(0); i < n; i++ {
			v, err := r.u64()
			if err != nil {
				return nil, err
			}
			out = append(out, int64(v))
		}
		return out, nil
	case ptMVString8:
		return r.str8Array(abk)
	case ptMVUnicode:
		return r.wstrArray(abk)
	case ptMVBinary:
		return r.binArray(abk)
	case ptMVClsid:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, n)
		for i := uint32(0); i < n; i++ {
			g, err := r.guid()
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, nil
	}
	return nil, formatError
}

type writer struct {
	buf []byte
}

func newWriter() *writer { return &writer{} }

func (w *writer) u8(v uint8)     { w.buf = append(w.buf, v) }
func (w *writer) u16(v uint16)   { w.buf = binary.LittleEndian.AppendUint16(w.buf, v) }
func (w *writer) u32(v uint32)   { w.buf = binary.LittleEndian.AppendUint32(w.buf, v) }
func (w *writer) i32(v int32)    { w.u32(uint32(v)) }
func (w *writer) u64(v uint64)   { w.buf = binary.LittleEndian.AppendUint64(w.buf, v) }
func (w *writer) bytes(b []byte) { w.buf = append(w.buf, b...) }

// bin writes a u32-length-prefixed blob (the NSPI/WCOUNT form).
func (w *writer) bin(b []byte) {
	w.u32(uint32(len(b)))
	w.bytes(b)
}

// str writes a NUL-terminated 8-bit string.
func (w *writer) str(s string) {
	w.buf = append(w.buf, s...)
	w.u8(0)
}

// wstr writes a NUL-terminated UTF-16LE string.
func (w *writer) wstr(s string) {
	for _, u := range utf16.Encode([]rune(s)) {
		w.u16(u)
	}
	w.u16(0)
}

// guid writes a 16-byte GUID in MAPI wire order.
func (w *writer) guid(s string) {
	w.bytes(guidToBytes(s))
}

// tagArray writes a u32-count array of property tags.
func (w *writer) tagArray(tags []uint32) {
	w.u32(uint32(len(tags)))
	for _, t := range tags {
		w.u32(t)
	}
}

// propval writes one property value in the ABK stream form used for all NSPI
// responses. A nil value writes an absent presence byte for the types that
// carry one; scalar types reject nil because nothing meaningful can be put on
// the wire for them.
func (w *writer) propval(tag uint32, v any) error {
	typ := propType(tag)
	if v == nil {
		switch typ {
		case ptString8, ptUnicode, ptBinary:
			w.u8(0) // absent
			return nil
		}
		if typ&mvFlag != 0 {
			w.u8(0) // absent
			return nil
		}
		return fmt.Errorf("mapi/nspi: cannot encode a nil %04x value", typ)
	}
	if typ == ptString8 || typ == ptUnicode || typ == ptBinary || typ&mvFlag != 0 {
		w.u8(0xFF) // present
	}
	switch typ {
	case ptShort:
		w.u16(uint16(asInt64(v)))
	case ptLong, ptError:
		w.u32(uint32(asInt64(v)))
	case ptR4:
		w.buf = binary.LittleEndian.AppendUint32(w.buf, math.Float32bits(float32(asFloat64(v))))
	case ptDouble, ptAppTime:
		w.buf = binary.LittleEndian.AppendUint64(w.buf, math.Float64bits(asFloat64(v)))
	case ptCurrency, ptSystime, ptI8:
		w.u64(uint64(asInt64(v)))
	case ptBoolean:
		if asBool(v) {
			w.u8(1)
		} else {
			w.u8(0)
		}
	case ptString8:
		w.str(asString(v))
	case ptUnicode:
		w.wstr(asString(v))
	case ptClsid:
		w.guid(asString(v))
	case ptBinary:
		w.bin(asBytes(v))
	case ptMVShort:
		elems := asManyInt64(v)
		w.u32(uint32(len(elems)))
		for _, e := range elems {
			w.u16(uint16(e))
		}
	case ptMVLong:
		elems := asManyInt64(v)
		w.u32(uint32(len(elems)))
		for _, e := range elems {
			w.u32(uint32(e))
		}
	case ptMVR4:
		elems := asManyFloat64(v)
		w.u32(uint32(len(elems)))
		for _, e := range elems {
			w.buf = binary.LittleEndian.AppendUint32(w.buf, math.Float32bits(float32(e)))
		}
	case ptMVDouble, ptMVAppTime:
		elems := asManyFloat64(v)
		w.u32(uint32(len(elems)))
		for _, e := range elems {
			w.buf = binary.LittleEndian.AppendUint64(w.buf, math.Float64bits(e))
		}
	case ptMVCurrency, ptMVSystime, ptMVI8:
		elems := asManyInt64(v)
		w.u32(uint32(len(elems)))
		for _, e := range elems {
			w.u64(uint64(e))
		}
	case ptMVString8:
		elems := asManyString(v)
		w.u32(uint32(len(elems)))
		for _, e := range elems {
			w.u8(0xFF)
			w.str(e)
		}
	case ptMVUnicode:
		elems := asManyString(v)
		w.u32(uint32(len(elems)))
		for _, e := range elems {
			w.u8(0xFF)
			w.wstr(e)
		}
	case ptMVBinary:
		elems := asManyBytes(v)
		w.u32(uint32(len(elems)))
		for _, e := range elems {
			w.u8(0xFF)
			w.bin(e)
		}
	case ptMVClsid:
		elems := asManyString(v)
		w.u32(uint32(len(elems)))
		for _, e := range elems {
			w.u8(0xFF)
			w.guid(e)
		}
	default:
		return fmt.Errorf("mapi/nspi: cannot encode property type %04x", typ)
	}
	return nil
}

// STAT is the NSPI table position state (MS-OXNSPI 2.2.1.8): nine u32
// fields in a fixed order.
type stat struct {
	sortType       uint32
	containerID    uint32
	curRec         uint32
	delta          int32
	numPos         uint32
	totalRec       uint32
	codepage       uint32
	templateLocale uint32
	sortLocale     uint32
}

func (r *reader) readStat() (stat, error) {
	var s stat
	var err error
	if s.sortType, err = r.u32(); err != nil {
		return s, err
	}
	if s.containerID, err = r.u32(); err != nil {
		return s, err
	}
	if s.curRec, err = r.u32(); err != nil {
		return s, err
	}
	if s.delta, err = r.i32(); err != nil {
		return s, err
	}
	if s.numPos, err = r.u32(); err != nil {
		return s, err
	}
	if s.totalRec, err = r.u32(); err != nil {
		return s, err
	}
	if s.codepage, err = r.u32(); err != nil {
		return s, err
	}
	if s.templateLocale, err = r.u32(); err != nil {
		return s, err
	}
	if s.sortLocale, err = r.u32(); err != nil {
		return s, err
	}
	return s, nil
}

func (w *writer) writeStat(s stat) {
	w.u32(s.sortType)
	w.u32(s.containerID)
	w.u32(s.curRec)
	w.i32(s.delta)
	w.u32(s.numPos)
	w.u32(s.totalRec)
	w.u32(s.codepage)
	w.u32(s.templateLocale)
	w.u32(s.sortLocale)
}

// tpropval writes a property tag followed by its value (MS-OXNSPI tagged
// property value). For NSPI responses the ABK presence byte is emitted by
// propval itself.
func (w *writer) tpropval(tag uint32, v any) error {
	w.u32(tag)
	return w.propval(tag, v)
}

// tpropvalLA writes a u32-count array of tagged property values.
func (w *writer) tpropvalLA(props []prop) error {
	w.u32(uint32(len(props)))
	for _, p := range props {
		if err := w.tpropval(p.tag, p.value); err != nil {
			return err
		}
	}
	return nil
}

// tpropvalRow writes the row GetProps answers with: a u32 count of tagged
// property values, one per requested column, in the order requested.
//
// An error cell differs from how a column row set carries one. There the row
// borrows its tags from the response's column list and writes only the
// status; here a row is self-describing, so the tag is written with its type
// replaced by PT_ERROR — which also drops the multi-value flag, so an absent
// multi-value property is tagged PT_ERROR and not PT_MV_ERROR — followed by
// the status. That is the reference's own shape: its fetch rewrites the type
// of the tag it failed on before recording the failure, and its writer then
// serialises the pair through the ordinary tagged-value path, where PT_ERROR
// is simply a u32.
func (w *writer) tpropvalRow(cells []prop) error {
	w.u32(uint32(len(cells)))
	for _, p := range cells {
		if code, isErr := errCode(p.value); isErr {
			w.u32(changeType(p.tag, ptError))
			w.u32(code)
			continue
		}
		w.u32(p.tag)
		if err := w.propval(p.tag, p.value); err != nil {
			return err
		}
	}
	return nil
}

// errCell marks a row cell as a flagged error carrying an explicit MAPI
// status code. It distinguishes the two error rows the reference produces:
// an object that could not be fetched at all (status 0) and an individual
// property the object simply lacks (ecNotFound, which proprow writes for a
// nil cell).
type errCell struct{ code uint32 }

// proprow writes one PROPERTY_ROW (MS-OXNSPI 2.2.1.3): a row flag byte
// followed by per-column cells. cols and vals must be aligned. A val of nil
// is written as a FLAGGED error cell with ecNotFound, mirroring what the
// reference implementation does for requested properties that objects lack.
//
// flagged selects the row flag byte. It is passed in rather than derived
// from vals because the reference derives it from the row it fetched: with
// an empty column list there are no cells to inspect, yet the fetched row's
// errors still set the flag.
func (w *writer) proprow(cols []uint32, vals []any, flagged bool) error {
	if !flagged {
		// PROPERTY_ROW_FLAG_NONE
		w.u8(0)
		for i, tag := range cols {
			if err := w.propval(tag, vals[i]); err != nil {
				return err
			}
		}
		return nil
	}
	// PROPERTY_ROW_FLAG_FLAGGED
	w.u8(1)
	for i, tag := range cols {
		if code, isErr := errCode(vals[i]); isErr {
			w.u8(0x0A) // FLAGGED_PROPVAL_FLAG_ERROR
			w.u32(code)
			continue
		}
		w.u8(0x00) // FLAGGED_PROPVAL_FLAG_AVAILABLE
		if err := w.propval(tag, vals[i]); err != nil {
			return err
		}
	}
	return nil
}

// errCode reports whether v denotes a flagged error cell and which status
// code it carries: nil means a property an existing object lacks
// (ecNotFound), errCell an explicit code.
func errCode(v any) (uint32, bool) {
	switch t := v.(type) {
	case nil:
		return ecNotFound, true
	case errCell:
		return t.code, true
	}
	return 0, false
}

// row is one row of an NSPI row set: its cells, aligned with the response
// column list, and the row's FLAGGED flag.
//
// The flag is carried rather than derived from the cells because the
// reference derives it from the row it fetched — the row keeps its flag even
// when the response column list is empty and therefore holds no cells at all
// (which happens when the client sends QueryRows or ResolveNames without
// columns: the server fetches the default tags, maps nothing onto the empty
// column list, and still reports the fetched row's property errors).
type row struct {
	cells   []prop
	flagged bool
}

// rowFor renders a record's property map as one response row. fetchCols is
// the column list the server fetched — the client's columns when it sent
// any, otherwise the default tags — and respCols is the column list the
// response carries (nil when the client sent none, which means no cells).
func rowFor(props map[uint32]any, fetchCols, respCols []uint32) row {
	fetch := numberedRow(fetchCols, props)
	fl := false
	for _, p := range fetch {
		if _, isErr := errCode(p.value); isErr {
			fl = true
			break
		}
	}
	if respCols == nil {
		return row{flagged: fl}
	}
	return row{cells: fetch, flagged: fl}
}

// errorRowFor is rowFor for an object that could not be fetched at all: the
// reference fills such a row with PT_ERROR cells carrying status 0
// (make_ptyperror_row).
//
// The flag comes from fetchCols rather than being set unconditionally,
// because the reference builds the error cells from the tags it was asked to
// fetch and a row with no cells in it has no error to flag: a client that
// sent no columns gets a flagged row and no cells (the fetch used the
// default tags and their errors are still reported), and a client that
// explicitly asked for none gets an unflagged row with none either.
func errorRowFor(fetchCols, respCols []uint32) row {
	fl := len(fetchCols) > 0
	if len(respCols) == 0 {
		return row{flagged: fl}
	}
	cells := make([]prop, len(respCols))
	for i, tag := range respCols {
		cells[i] = prop{tag: tag, value: errCell{0}}
	}
	return row{cells: cells, flagged: fl}
}

// colrow writes the NSPI "column row set": a proptag_la column list, a u32
// row count and one PROPERTY_ROW per row.
func (w *writer) colrow(cols []uint32, rows []row) error {
	w.tagArray(cols)
	w.u32(uint32(len(rows)))
	for _, r := range rows {
		if len(r.cells) != len(cols) {
			return fmt.Errorf("mapi/nspi: row/column count mismatch")
		}
		vals := make([]any, len(cols))
		for i, p := range r.cells {
			vals[i] = p.value
		}
		if err := w.proprow(cols, vals, r.flagged); err != nil {
			return err
		}
	}
	return nil
}

// numberedRow converts a property map into a row aligned with cols: present
// properties keep their value, absent ones carry nil (which proprow renders
// as an error cell).
func numberedRow(cols []uint32, props map[uint32]any) []prop {
	out := make([]prop, len(cols))
	for i, tag := range cols {
		v, ok := props[tag]
		out[i] = prop{tag: tag}
		if ok {
			out[i].value = v
		}
	}
	return out
}

// prop pairs a property tag with its wire value; value may be nil to mark
// the property absent.
type prop struct {
	tag   uint32
	value any
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(v)
	}
}

func asBytes(v any) []byte {
	switch t := v.(type) {
	case []byte:
		return t
	case string:
		return []byte(t)
	default:
		panic("mapi/nspi: value is not byte-shaped")
	}
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case int:
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
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return int64(t)
	default:
		panic("mapi/nspi: value is not integer-shaped")
	}
}

func asFloat64(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint32:
		return float64(t)
	default:
		panic("mapi/nspi: value is not float-shaped")
	}
}

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case uint8:
		return t != 0
	case uint32:
		return t != 0
	default:
		panic("mapi/nspi: value is not boolean-shaped")
	}
}

func asManyString(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []byte:
		return []string{string(t)}
	default:
		panic("mapi/nspi: value is not a string array")
	}
}

func asManyBytes(v any) [][]byte {
	switch t := v.(type) {
	case [][]byte:
		return t
	case []byte:
		return [][]byte{t}
	default:
		panic("mapi/nspi: value is not a binary array")
	}
}

func asManyInt64(v any) []int64 {
	switch t := v.(type) {
	case []int:
		out := make([]int64, len(t))
		for i, e := range t {
			out[i] = int64(e)
		}
		return out
	case []int64:
		return t
	case []uint8:
		out := make([]int64, len(t))
		for i, e := range t {
			out[i] = int64(e)
		}
		return out
	case []uint16:
		out := make([]int64, len(t))
		for i, e := range t {
			out[i] = int64(e)
		}
		return out
	case []uint32:
		out := make([]int64, len(t))
		for i, e := range t {
			out[i] = int64(e)
		}
		return out
	case []uint64:
		out := make([]int64, len(t))
		for i, e := range t {
			out[i] = int64(e)
		}
		return out
	default:
		panic("mapi/nspi: value is not an integer array")
	}
}

func asManyFloat64(v any) []float64 {
	switch t := v.(type) {
	case []float64:
		return t
	case []float32:
		out := make([]float64, len(t))
		for i, e := range t {
			out[i] = float64(e)
		}
		return out
	case []int:
		out := make([]float64, len(t))
		for i, e := range t {
			out[i] = float64(e)
		}
		return out
	case []int64:
		out := make([]float64, len(t))
		for i, e := range t {
			out[i] = float64(e)
		}
		return out
	case []uint32:
		out := make([]float64, len(t))
		for i, e := range t {
			out[i] = float64(e)
		}
		return out
	default:
		panic("mapi/nspi: value is not a float array")
	}
}

// newGUID generates a random RFC 4122 version 4 GUID string.
func newGUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("mapi/nspi: cannot read random GUID: %v", err))
	}
	raw[6] = raw[6]&0x0F | 0x40
	raw[8] = raw[8]&0x3F | 0x80
	return guidToString(raw[:])
}

// guidToString renders a 16-byte wire GUID in the canonical dash form
// (the first three fields are little-endian, RFC 4122 mixed-endian).
func guidToString(raw []byte) string {
	if len(raw) != 16 {
		return ""
	}
	d1 := binary.LittleEndian.Uint32(raw[0:4])
	d2 := binary.LittleEndian.Uint16(raw[4:6])
	d3 := binary.LittleEndian.Uint16(raw[6:8])
	return fmt.Sprintf("%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		d1, d2, d3,
		raw[8], raw[9], raw[10], raw[11], raw[12], raw[13], raw[14], raw[15])
}

// guidToBytes reverses guidToString.
func guidToBytes(s string) []byte {
	raw := make([]byte, 16)
	var b [32]byte
	hexChars := 0
	for i := 0; i < len(s) && hexChars < 32; i++ {
		if c := s[i]; c != '-' {
			b[hexChars] = c
			hexChars++
		}
	}
	var hd [16]byte
	for i := 0; i < hexChars; i += 2 {
		hd[i/2] = hexVal(b[i])<<4 | hexVal(b[i+1])
	}
	copy(raw, hd[0:4])
	for i, j := 0, 3; i < j; i, j = i+1, j-1 {
		raw[i], raw[j] = raw[j], raw[i]
	}
	copy(raw[4:6], hd[4:6])
	raw[4], raw[5] = raw[5], raw[4]
	copy(raw[6:8], hd[6:8])
	raw[6], raw[7] = raw[7], raw[6]
	copy(raw[8:], hd[8:])
	return raw
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}

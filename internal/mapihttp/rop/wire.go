package rop

// This file is the low-level pack/unpack machinery shared by the ROP layer
// (MS-OXCROPS, MS-OXCDATA stream versions). All integers are little-endian,
// which is the only byte order MAPI uses on the wire.
//
// The ROP streams differ from the NSPI streams in three deliberate ways that
// this package normalises:
//
//   - binary lengths are u16 (p_bin_s), not u32;
//   - strings are NUL-terminated UTF-16LE / ANSI without a WCOUNT prefix;
//   - multi-value property counts are u32 but the elements carry no "ABK"
//     presence bytes (the NSPI /mapi/nspi endpoint gets those elsewhere).
//
// Every reader here is bounds-checked: a truncated or malformed request is
// refused with a format error rather than allowed to panic or over-read.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf16"
)

// formatError is the "the request body is invalid" condition that the MAPI/HTTP
// transport maps to resp_code invalid_rq_body. The ROP layer keeps its own sentinel
// so the transport does not need to recognise every wire shape it can produce.
var formatError = errors.New("mapi/rop: malformed ROP data")

type reader struct {
	buf []byte
	off int
}

func newReader(buf []byte) *reader { return &reader{buf: buf} }

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

// bin reads a u16-length-prefixed binary blob (p_bin_s).
func (r *reader) bin() ([]byte, error) {
	n, err := r.u16()
	if err != nil {
		return nil, err
	}
	return r.bytes(int(n))
}

// zeroTerminated8 reads an 8-bit NUL-terminated string (p_str) minus the NUL.
func (r *reader) zeroTerminated8() (string, error) {
	for i := r.off; i < len(r.buf); i++ {
		if r.buf[i] == 0 {
			s := string(r.buf[r.off:i])
			r.off = i + 1
			return s, nil
		}
	}
	return "", formatError
}

// zeroTerminated16 reads a UTF-16LE NUL-terminated string (p_wstr) minus the NUL.
func (r *reader) zeroTerminated16() (string, error) {
	if (len(r.buf)-r.off)%2 != 0 {
		return "", formatError
	}
	for i := r.off; i+1 < len(r.buf); i += 2 {
		if r.buf[i] == 0 && r.buf[i+1] == 0 {
			s, err := utf16BytesToString(r.buf[r.off:i])
			if err != nil {
				return "", err
			}
			r.off = i + 2
			return s, nil
		}
	}
	return "", formatError
}

// eid returns the next u16 replid + u48 value as a single u64 eid.
func (r *reader) eid() (uint64, error) {
	return r.u64()
}

// guid reads 16 bytes in MAPI wire order (that is, the RFC 4122 mixed-endian
// byte order) and returns the canonical dashed form.
func (r *reader) guid() (string, error) {
	raw, err := r.bytes(16)
	if err != nil {
		return "", err
	}
	return guidToString(raw), nil
}

type writer struct {
	buf []byte
}

func newWriter() *writer { return &writer{} }

func (w *writer) u8(v uint8) { w.buf = append(w.buf, v) }

func (w *writer) u16(v uint16) {
	w.buf = binary.LittleEndian.AppendUint16(w.buf, v)
}

func (w *writer) u32(v uint32) {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, v)
}

func (w *writer) i32(v int32) { w.u32(uint32(v)) }

func (w *writer) u64(v uint64) {
	w.buf = binary.LittleEndian.AppendUint64(w.buf, v)
}

func (w *writer) bytes(v []byte) { w.buf = append(w.buf, v...) }

// bin writes a u16-length-prefixed binary blob (p_bin_s).
func (w *writer) bin(v []byte) error {
	if len(v) > math.MaxUint16 {
		return formatError
	}
	w.u16(uint16(len(v)))
	w.bytes(v)
	return nil
}

// str writes an 8-bit NUL-terminated string (p_str).
func (w *writer) str(s string) {
	w.bytes(append([]byte(s), 0))
}

// wstr writes a UTF-16LE NUL-terminated string (p_wstr).
func (w *writer) wstr(s string) {
	units := utf16.Encode([]rune(s))
	for _, u := range units {
		w.u16(u)
	}
	w.u16(0)
}

// guid writes 16 bytes in MAPI wire order (RFC 4122 mixed-endian).
func (w *writer) guid(s string) {
	raw := guidToBytes(s)
	w.bytes(raw)
}

// typedStr writes a TYPED_STRING (MS-OXCDATA 2.11.7): a u8 string type
// followed by the payload. gromox emits 0x01 (empty) or 0x04 (Unicode).
func (w *writer) typedStr(s string) {
	if s == "" {
		w.u8(0x01)
		return
	}
	w.u8(0x04)
	w.wstr(s)
}

// proptags writes a property tag array (p_proptag_a): u16 count + u32 tags.
func (w *writer) proptags(tags []uint32) error {
	if len(tags) > math.MaxUint16 {
		return formatError
	}
	w.u16(uint16(len(tags)))
	for _, t := range tags {
		w.u32(t)
	}
	return nil
}

// propids writes a property ID array (p_propid_a): u16 count + u16 ids.
func (w *writer) propids(ids []uint16) error {
	if len(ids) > math.MaxUint16 {
		return formatError
	}
	w.u16(uint16(len(ids)))
	for _, id := range ids {
		w.u16(id)
	}
	return nil
}

// eids writes an eid array (p_eid_a): u16 count + u64 eids.
func (w *writer) eids(ids []uint64) error {
	if len(ids) > math.MaxUint16 {
		return formatError
	}
	w.u16(uint16(len(ids)))
	for _, id := range ids {
		w.u64(id)
	}
	return nil
}

// typedeids writes an eid array with u32 length (the DNTABLE/EXPLICIT form).
func (w *writer) typedeids(ids []uint64) {
	w.u32(uint32(len(ids)))
	for _, id := range ids {
		w.u64(id)
	}
}

func (r *reader) proptags() ([]uint32, error) {
	n, err := r.u16()
	if err != nil {
		return nil, err
	}
	if n > uint16(r.remaining()/4) {
		return nil, formatError
	}
	tags := make([]uint32, n)
	for i := range tags {
		t, err := r.u32()
		if err != nil {
			return nil, err
		}
		tags[i] = t
	}
	return tags, nil
}

func (r *reader) propids() ([]uint16, error) {
	n, err := r.u16()
	if err != nil {
		return nil, err
	}
	ids := make([]uint16, n)
	for i := range ids {
		id, err := r.u16()
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return ids, nil
}

func (r *reader) eids() ([]uint64, error) {
	n, err := r.u16()
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, n)
	for i := range ids {
		id, err := r.u64()
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return ids, nil
}

// propName is a PROPERTY_NAME (MS-OXCDATA 2.9.1): a kind byte (0 = MNID_ID,
// 1 = MNID_STRING), a GUID, then either a 32-bit LID or a length-prefixed
// UTF-16 string.
type propName struct {
	Kind uint8
	GUID string
	LID  uint32
	Name string // MNID_STRING
}

// propname reads a PROPERTY_NAME (MNID_ID or MNID_STRING) from the stream.
func (r *reader) propname() (pn propName, err error) {
	kind, err := r.u8()
	if err != nil {
		return pn, err
	}
	pn.Kind = kind
	pn.GUID, err = r.guid()
	if err != nil {
		return pn, err
	}
	if kind == 0 { // MNID_ID
		pn.LID, err = r.u32()
		if err != nil {
			return pn, err
		}
		return pn, nil
	}
	if kind == 1 { // MNID_STRING
		size, err := r.u8()
		if err != nil {
			return pn, err
		}
		// size is the byte length of the UTF-16 string that follows.
		if int(size)%2 != 0 || size > uint8(r.remaining()) {
			return pn, formatError
		}
		name, err := r.bytes(int(size))
		if err != nil {
			return pn, err
		}
		pn.Name, err = utf16BytesToString(name)
		return pn, err
	}
	return pn, formatError
}

func (w *writer) propname(pn propName) error {
	w.u8(pn.Kind)
	w.guid(pn.GUID)
	if pn.Kind == 0 {
		w.u32(pn.LID)
		return nil
	}
	if pn.Kind == 1 {
		units := utf16.Encode([]rune(pn.Name))
		if len(units) > 255 {
			return formatError
		}
		w.u8(uint8(len(units) * 2))
		for _, u := range units {
			w.u16(u)
		}
		return nil
	}
	return formatError
}

func (r *reader) propNameArray() ([]propName, error) {
	n, err := r.u16()
	if err != nil {
		return nil, err
	}
	out := make([]propName, n)
	for i := range out {
		pn, err := r.propname()
		if err != nil {
			return nil, err
		}
		out[i] = pn
	}
	return out, nil
}

func (w *writer) propNameArray(pns []propName) error {
	if len(pns) > math.MaxUint16 {
		return formatError
	}
	w.u16(uint16(len(pns)))
	for _, pn := range pns {
		if err := w.propname(pn); err != nil {
			return err
		}
	}
	return nil
}

// utf16BytesToString decodes a UTF-16LE byte slice.
func utf16BytesToString(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", formatError
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(units)), nil
}

// GUID wire helpers (RFC 4122 mixed-endian: the first three fields little-endian,
// the last two big-endian, matching the exchange GUID string that Outlook shows).

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

func guidToBytes(s string) []byte {
	raw := make([]byte, 16)
	// RFC 4122 string form "00112233-4455-6677-8899-aabbccddeeff" is stored on
	// the wire as 33 22 11 00 | 55 44 | 77 66 | 88 99 aa bb cc dd ee ff, so the
	// first three fields are byte-reversed and the last two are not.
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
	reverse4(raw[0:4])
	copy(raw[4:6], hd[4:6])
	reverse2(raw[4:6])
	copy(raw[6:8], hd[6:8])
	reverse2(raw[6:8])
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

func reverse2(b []byte) { b[0], b[1] = b[1], b[0] }

func reverse4(b []byte) { b[0], b[3] = b[3], b[0]; b[1], b[2] = b[2], b[1] }

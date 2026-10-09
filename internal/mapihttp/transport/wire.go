package transport

import (
	"encoding/binary"
	"errors"
	"unicode/utf16"
)

// The MAPI/HTTP request and response bodies are little-endian field streams:
// fixed-width integers, NUL-terminated strings, and byte arrays prefixed with
// their u32 length (MS-OXCMAPIHTTP 2.2.4 for the mailbox endpoint and 2.2.5
// for the address book endpoint). This file is those two directions and
// nothing else; the operations live in the rop and nspi packages.
//
// A read stops at the first field that does not fit and reports the body as
// malformed, which is what the reference's ext_pull does — its pack_result
// failure is what becomes X-ResponseCode 12. Bytes left over after the last
// field the specification names are ignored rather than rejected, because the
// reference ignores them too: an auxiliary buffer the server does not
// understand is still a well-formed request as far as the client is
// concerned.
var errTruncated = errors.New("mapi/http: request body ended early")

type reader struct {
	buf []byte
	off int
}

func newReader(b []byte) *reader { return &reader{buf: b} }

func (r *reader) remaining() int { return len(r.buf) - r.off }

// u32 reads a little-endian unsigned 32-bit field.
func (r *reader) u32() (uint32, error) {
	if r.remaining() < 4 {
		return 0, errTruncated
	}
	v := binary.LittleEndian.Uint32(r.buf[r.off:])
	r.off += 4
	return v, nil
}

// cstr reads a NUL-terminated 8-bit string.
func (r *reader) cstr() (string, error) {
	for i := r.off; i < len(r.buf); i++ {
		if r.buf[i] == 0 {
			s := string(r.buf[r.off:i])
			r.off = i + 1
			return s, nil
		}
	}
	return "", errTruncated
}

// blob reads a byte array of the length the preceding u32 field names. The
// length is not re-read here: every AuxiliaryBuffer in the protocol is
// preceded by its own size field, so the caller reads that and passes it in.
func (r *reader) blob(n uint32) ([]byte, error) {
	if uint64(n) > uint64(r.remaining()) {
		return nil, errTruncated
	}
	b := r.buf[r.off : r.off+int(n)]
	r.off += int(n)
	return b, nil
}

// aux reads an AuxiliaryBuffer: its u32 size and then that many bytes. The
// bytes are parsed by nobody — the auxiliary payload is an extension point
// this server has nothing to say on — but they are read all the same, so a
// client that sends one is not mistaken for a client that sent a short body.
func (r *reader) aux() error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	_, err = r.blob(n)
	return err
}

// writer is the response direction of the same framing.
type writer struct {
	buf []byte
}

func (w *writer) u16(v uint16) { w.buf = binary.LittleEndian.AppendUint16(w.buf, v) }
func (w *writer) u32(v uint32) { w.buf = binary.LittleEndian.AppendUint32(w.buf, v) }

func (w *writer) str(s string) {
	w.buf = append(w.buf, s...)
	w.buf = append(w.buf, 0)
}

// wstr writes a NUL-terminated UTF-16LE string, which is the form the
// Connect response's DisplayName takes (MS-OXCMAPIHTTP 2.2.4.1.2).
func (w *writer) wstr(s string) {
	for _, u := range utf16.Encode([]rune(s)) {
		w.u16(u)
	}
	w.u16(0)
}

// blob writes a byte array with its u32 length in front.
func (w *writer) blob(b []byte) {
	w.u32(uint32(len(b)))
	w.buf = append(w.buf, b...)
}

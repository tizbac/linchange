package rop

// This file is the ROP (Remote OPeration) layer core: the ROP buffer framing
// (MS-OXCROPS 2.2.1) around the RPC_HEADER_EXT (MS-OXCRPC 2.2.1.2) and the
// dispatch of the ROPs Outlook sends against a mailbox store. The individual
// handlers live in handlers.go.
//
// A ROP "request" received in rgbRopIn is one RPC_HEADER_EXT followed by the
// ROP list and the client's handle table:
//
//	RPC_HEADER_EXT (8 bytes): version u16, flags u16, size u16, size_actual u16
//	  size is the on-the-wire length of the following data;
//	  size_actual is its logical length (the decompressed size).
//	sub-buffer:
//	  u16 size (= 2 + length of the ROP list)
//	  ROP list: one or more [rop_id u8, logon_id u8, hindex u8, body...]
//	  handle table: (size_actual - size)/4 little-endian u32 handles
//
// The response echoes the same shape: RPC_HEADER_EXT, then u16 size, then the
// concatenated ROP responses and finally the (possibly changed) handle table.
// The server only ever answers uncompressed and un-XORed, like a server that
// never negotiated compression or obfuscation at all (see rop_ext_make_rpc_ext
// in gromox).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/contacts"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// ROP IDs (MS-OXCROPS 2.2.1.1, gromox rop_ids.hpp).
const (
	ropRelease                 = 0x01
	ropOpenFolder              = 0x02
	ropOpenMessage             = 0x03
	ropGetHierarchyTable       = 0x04
	ropGetContentsTable        = 0x05
	ropCreateMessage           = 0x06
	ropGetPropertiesSpecific   = 0x07
	ropGetPropertiesAll        = 0x08
	ropGetPropertiesList       = 0x09
	ropSetProperties           = 0x0A
	ropDeleteProperties        = 0x0B
	ropSaveChangesMessage      = 0x0C
	ropSetColumns              = 0x12
	ropSortTable               = 0x13
	ropRestrict                = 0x14
	ropQueryRows               = 0x15
	ropGetStatus               = 0x16
	ropQueryPosition           = 0x17
	ropSeekRow                 = 0x18
	ropSeekRowBookmark         = 0x19
	ropSeekRowFractional       = 0x1A
	ropCreateBookmark          = 0x1B
	ropDeleteMessages          = 0x1E
	ropGetMessageStatus        = 0x1F
	ropSetMessageStatus        = 0x20
	ropGetReceiveFolder        = 0x27
	ropRegisterNotification    = 0x29
	ropFallbackNotify          = 0x2A
	ropOpenStream              = 0x2B
	ropReadStream              = 0x2C
	ropWriteStream             = 0x2D
	ropSeekStream              = 0x2E
	ropSetStreamSize           = 0x2F
	ropGetNamesFromPropertyIds = 0x55
	ropGetPropertyIdsFromNames = 0x56
	ropCommitStream            = 0x5D
	ropGetStreamSize           = 0x5E
	ropGetPerUserLongTermIds   = 0x60
	ropSetReadFlags            = 0x66
	ropGetReceiveFolderTable   = 0x68
	ropQueryColumnsAll         = 0x37
	ropGetStoreState           = 0x7B
	ropFreeBookmark            = 0x89
	ropTellVersion             = 0x86
	ropLogon                   = 0xFE
	ropBufferTooSmall          = 0xFF
)

// RPC_HEADER_EXT flags (MS-OXCRPC 2.2.1.2.2).
const (
	rheFlagCompressed = 0x0001
	rheFlagXORMagic   = 0x0002
	rheFlagLast       = 0x0004
)

// Logon flags (MS-OXCSTOR logon_flags).
const (
	logonFlagPrivate = 0x1
)

// Logon open_flags (MS-OXCSTOR).
const (
	logonOpenFlagUsePerMDBReplidMapping = 0x01000000
	logonOpenFlagAlternateServer        = 0x00000100
)

// RopLogon response_flags (MS-OXCSTOR).
const (
	responseFlagReserved    = 0x1
	responseFlagOwnerRight  = 0x2
	responseFlagSendAsRight = 0x4
	responseFlagOOF         = 0x10
)

// Property row and flagged propval control bytes (MS-OXCDATA 2.11.1).
const (
	propRowFlagNone      = 0x00
	propRowFlagFlagged   = 0x01
	flaggedFlagAvailable = 0x00
	flaggedFlagUnavail   = 0x01
	flaggedFlagError     = 0x0A
)

// QueryRows flags (MS-OXCTABL QueryRowsFlags).
const (
	queryRowsAdvance   = 0x00
	queryRowsNoAdvance = 0x01
	queryRowsPacked    = 0x02
)

// Table status values (MS-OXCTABL TableStatus).
const (
	tblStatComplete    = 0
	tblStatSorting     = 9
	tblStatSettingCols = 11
)

// Bookmar origin / seek position values (MS-OXCTABL).
const (
	bookmarkBeginning = 0x00
	bookmarkCurrent   = 0x01
	bookmarkEnd       = 0x02
)

// Message status (RopGetMessageStatus), MS-OXOMSG PR_MESSAGE_FLAGS subset.
const (
	msgFlagRead           = 0x0001
	msgFlagUnsent         = 0x0008
	msgFlagHasAttach      = 0x0010
	msgFlagFromMe         = 0x0020
	msgFlagRnpending      = 0x0080
	msgFlagNRNPending     = 0x0100
	msgFlagEVERREAD       = 0x0400
	msgFlagOriginInternet = 0x2000
)

// GLOBCNT helper: the "value" half of an eid (replid<<48 | value).
func fidValue(eid uint64) uint64 { return eid & 0x0000FFFFFFFFFFFF }

// Error codes (MS-OXCROPS / mapierr).
const (
	ecSuccess           = 0x00000000
	ecUnknownUser       = 0x000003EB
	ecServerOOM         = 0x000003F0
	ecLoginPerm         = 0x000003F2
	ecParameterOverflow = 0x00000450
	ecNotSearchFolder   = 0x00000461
	ecNoReceiveFolder   = 0x00000463
	ecWrongServer       = 0x00000478
	ecBufferTooSmall    = 0x0000047D
	ecRpcFormat         = 0x000004B6
	ecNullObject        = 0x000004B9
	ecWarnWithErrors    = 0x00040380
	ecError             = 0x80004005
	ecNotSupported      = 0x80040102
	ecInvalidObject     = 0x80040108
	ecNotFound          = 0x8004010F
	ecLoginFailure      = 0x80040111
	ecRpcFailed         = 0x80040115
	ecTooComplex        = 0x80040117
	ecInvalidBookmark   = 0x80040405
	ecDuplicateName     = 0x80040604
	ecInvalidParam      = 0x80070057
	ecInsufficientResrc = 0x8004010E
	ecAccessDenied      = 0x80070005
)

// Backend is everything the ROP layer needs past its own session state: the
// mail/calendar stores that EWS and EAS already use, so an Outlook client sees
// the same rows every other client does.
type Backend struct {
	DB        *sqlx.DB
	Mail      *webmail.Service
	Messages  *imapd.MessageRepo
	Mailboxes *imapd.MailboxRepo
	Users     *store.Users
	// Contacts is the address-book store. Its personal and shared books are
	// exposed as MAPI contact folders, so Outlook's Contacts folder is the
	// same set of cards CardDAV and the web client see.
	Contacts *contacts.Service
	// Hostname is the public name this server is reachable at; it appears in
	// the URLs the NSPI GetMailboxUrl/GetAddressBookUrl operations return.
	Hostname string
}

// rpcExtError aborts the whole RPC with an error code that lands in the
// Execute response's result field, with no ROP responses at all. This is what
// gromox does when dispatch fails before a response is produced (bad handle
// index, unparseable ROP list, unsupported ROP).
type rpcExtError struct{ code uint32 }

func (e *rpcExtError) Error() string { return fmt.Sprintf("mapi/rop: rpc error 0x%08x", e.code) }

func rpcAbort(code uint32) error { return &rpcExtError{code: code} }

// ErrorCode reports the status an Execute failure should be answered with,
// in the Execute response's ErrorCode field.
//
// An error raised by the ROP layer carries a code — a handle that names no
// object, an operation this build does not implement — and that code is what
// the client is told, because it is the diagnosis the layer actually made.
// An error that carries none (a buffer that could not be parsed, a store
// that would not answer) is reported as ecRpcFailed, which is what the
// reference answers when the call as a whole cannot be carried out.
func ErrorCode(err error) (uint32, bool) {
	var e *rpcExtError
	if errors.As(err, &e) {
		return e.code, true
	}
	return 0, false
}

type execResult struct {
	handles   []uint32
	responses []byte
	flags     uint16
	version   uint16
}

// Execute parses one rgbRopIn and returns the rgbRopOut buffer, both in the
// MS-OXCROPS framing described above. The session holds the handle table and
// object registry across Executes.
func Execute(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger, in []byte) ([]byte, error) {
	res, err := dispatch(ctx, st, back, log, in)
	if err != nil {
		return nil, err
	}
	return buildOutput(res), nil
}

// dispatch reads, decodes (XOR), and runs the ROP list.
func dispatch(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger, in []byte) (*execResult, error) {
	if len(in) < 8 {
		return nil, formatError
	}
	rd := newReader(in)
	version, _ := rd.u16()
	flags, _ := rd.u16()
	size, _ := rd.u16()
	sizeActual, _ := rd.u16()
	if flags&rheFlagLast == 0 {
		return nil, formatError
	}
	if flags&rheFlagCompressed != 0 {
		// We do not ship an lzxpress codec; a client that asks for compression
		// on input gets a clean refusal rather than a misinterpreted body.
		return nil, errors.New("mapi/rop: compressed ROP input is not supported")
	}
	if int(size) > rd.remaining() {
		return nil, formatError
	}
	if int(sizeActual) > len(in)-8 {
		return nil, formatError
	}
	payload := in[8 : 8+int(sizeActual)]
	if flags&rheFlagXORMagic != 0 {
		buf := append([]byte(nil), payload...)
		if len(buf) > int(size) {
			buf = buf[:size]
		}
		for i := range buf {
			buf[i] ^= 0xA5
		}
		copy(payload, buf)
	}
	sub := newReader(payload)
	ropSize, err := sub.u16()
	if err != nil {
		return nil, err
	}
	if int(ropSize) > len(payload) {
		return nil, formatError
	}
	hnum := (len(payload) - int(ropSize)) / 4
	if (len(payload)-int(ropSize))%4 != 0 {
		return nil, formatError
	}
	handles := make([]uint32, 0, hnum)
	for i := 0; i < hnum; i++ {
		h, err := sub.u32()
		if err != nil {
			return nil, err
		}
		handles = append(handles, h)
	}
	rops := newReader(payload[2:int(ropSize)])
	out := &execResult{handles: handles, version: version, flags: rheFlagLast}
	var resp bytes.Buffer
	for rops.remaining() > 0 {
		id, err := rops.u8()
		if err != nil {
			break
		}
		logonID, err := rops.u8()
		if err != nil {
			break
		}
		hindex, err := rops.u8()
		if err != nil {
			break
		}
		_ = logonID // single-store deployment, always logon 0
		// runROP consumes the ROP body from the shared reader, so the next
		// iteration continues at the next ROP in the list.
		if err := runROP(ctx, st, back, log, id, hindex, rops, out.handles, &resp); err != nil {
			var re *rpcExtError
			if errors.As(err, &re) {
				return nil, re
			}
			// Body parse failure: the whole request fails with a format error,
			// matching gromox parsing every ROP before executing any.
			return nil, rpcAbort(ecRpcFormat)
		}
	}
	out.responses = resp.Bytes()
	return out, nil
}

func putU32LE(v uint32, b []byte) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

// buildOutput assembles the rgbRopOut buffer from the responses.
func buildOutput(res *execResult) []byte {
	// sub-buffer: u16 rop_size + responses + handles. rop_size covers only
	// itself and the ROP responses; the handle table follows it and is not
	// part of that count (MS-OXCROPS 2.2.1). The outer size fields cover the
	// whole sub-buffer, handle table included.
	var sub bytes.Buffer
	responseLen := len(res.responses)
	sub.WriteByte(byte(responseLen + 2))
	sub.WriteByte(byte((responseLen + 2) >> 8))
	sub.Write(res.responses)
	for _, h := range res.handles {
		var b [4]byte
		putU32LE(h, b[:])
		sub.Write(b[:])
	}
	out := make([]byte, 0, 8+sub.Len())
	var hdr [8]byte
	hdr[0] = byte(res.version)
	hdr[1] = byte(res.version >> 8)
	hdr[2] = byte(res.flags)
	hdr[3] = byte(res.flags >> 8)
	hdr[4] = byte(sub.Len())
	hdr[5] = byte(sub.Len() >> 8)
	hdr[6] = hdr[4]
	hdr[7] = hdr[5]
	out = append(out, hdr[:]...)
	out = append(out, sub.Bytes()...)
	return out
}

// Session is one EMSMDB connection's ROP state: the handle table and object
// registry that persists across Execute calls. The MAPI/HTTP transport keeps
// one Session per client connection and hands it to Execute.
type Session struct {
	state *sessionState
}

// NewSession creates an empty session. The owning mailbox is not known until
// the first successful logon (RopLogon carries the store entry id); SetOwner
// is used at connect time where the authenticated principal is available.
func NewSession() *Session {
	return &Session{state: &sessionState{objects: make(map[uint32]*mapiObject)}}
}

// Owner returns the owning account once one has been recorded.
func (s *Session) Owner() string {
	return s.state.owner
}

// SetOwner records the authenticated principal that owns the session. It is
// safe to call before any Execute (it initialises the logon) and again later
// for the rare re-logon from a different (but still authorised) identity.
func (s *Session) SetOwner(owner string) {
	s.state.owner = owner
}

// LoggedOn reports whether a RopLogon has completed.
func (s *Session) LoggedOn() bool {
	return s.state.loggedOn
}

// Execute parses the rgbRopIn buffer (RPC_HEADER_EXT + ROP list + handle table)
// and returns the rgbRopOut response.
func (s *Session) Execute(ctx context.Context, back *Backend, log *slog.Logger, in []byte) ([]byte, error) {
	return Execute(ctx, s.state, back, log, in)
}

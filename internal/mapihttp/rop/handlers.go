package rop

// The ROP handlers. Every layout here matches gromox rop_ext.cpp and the MS
// specs (MS-OXCSTOR, MS-OXCFOLD, MS-OXCTABL, MS-OXCPRPT, MS-OXCFXICS,
// MS-OXOMSG). The response framing -- u8 rop_id, u8 hindex, u32 result, body
// only on ecSuccess -- matches the rop_response base push in gromox,
// including the RopGetMessageStatus quirk (the response is stamped 0x20).

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"time"
)

// runROP dispatches one ROP. It consumes the request body from req (the shared
// ROP-list reader), and appends the response to out. Creating ROPs write the
// new handle value in place at the ohindex position of the client's handle
// table, exactly like gromox.
//
// A nil return means the list continues; an error aborts the whole RPC.
func runROP(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	id, hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	if hindex >= uint8(len(handles)) {
		return rpcAbort(ecInvalidObject)
	}
	switch id {
	case ropRelease:
		hRelease(st, handles[hindex])
		return nil
	case ropLogon:
		return hLogon(ctx, st, back, log, hindex, req, handles, out)
	case ropOpenFolder:
		return hOpenFolder(ctx, st, back, log, hindex, req, handles, out)
	case ropOpenMessage:
		return hOpenMessage(ctx, st, back, log, hindex, req, handles, out)
	case ropGetHierarchyTable:
		return hGetTable(ctx, st, back, log, hindex, req, handles, out, true)
	case ropGetContentsTable:
		return hGetTable(ctx, st, back, log, hindex, req, handles, out, false)
	case ropGetPropertiesSpecific:
		return hGetPropsSpecific(ctx, st, back, log, hindex, req, handles, out)
	case ropGetPropertiesAll:
		return hGetPropsAll(ctx, st, back, log, hindex, req, handles, out)
	case ropGetPropertiesList:
		return hGetPropsList(ctx, st, back, log, hindex, req, handles, out)
	case ropSetProperties:
		return hSetProps(ctx, st, back, log, hindex, req, out)
	case ropDeleteProperties:
		return hDeleteProps(ctx, st, back, log, hindex, req, out)
	case ropSetColumns:
		return hSetColumns(ctx, st, back, log, hindex, req, handles, out)
	case ropSortTable:
		return hSortTable(ctx, st, back, log, hindex, req, handles, out)
	case ropRestrict:
		return hRestrict(ctx, st, back, log, hindex, req, handles, out)
	case ropQueryRows:
		return hQueryRows(ctx, st, back, log, hindex, req, handles, out)
	case ropGetStatus:
		return hGetStatus(ctx, st, back, log, hindex, handles, out)
	case ropQueryPosition:
		return hQueryPosition(ctx, st, back, log, hindex, handles, out)
	case ropSeekRow:
		return hSeekRow(ctx, st, back, log, hindex, req, handles, out)
	case ropSeekRowBookmark:
		return hSeekRowBookmark(ctx, st, back, log, hindex, req, handles, out)
	case ropCreateBookmark:
		return hCreateBookmark(ctx, st, back, log, hindex, handles, out)
	case ropFreeBookmark:
		return hFreeBookmark(ctx, st, back, log, hindex, req, handles, out)
	case ropDeleteMessages:
		return hDeleteMessages(ctx, st, back, log, hindex, req, handles, out)
	case ropGetMessageStatus:
		return hGetMessageStatus(ctx, st, back, log, hindex, req, handles, out)
	case ropGetReceiveFolder:
		return hGetReceiveFolder(ctx, st, back, log, hindex, req, handles, out)
	case ropRegisterNotification:
		return hRegisterNotification(ctx, st, back, log, hindex, req, handles, out)
	case ropOpenStream:
		return hOpenStream(ctx, st, back, log, hindex, req, handles, out)
	case ropReadStream:
		return hReadStream(ctx, st, back, log, hindex, req, handles, out)
	case ropQueryColumnsAll:
		return hQueryColumnsAll(ctx, st, back, log, hindex, handles, out)
	case ropGetNamesFromPropertyIds:
		return hGetNamesFromPropIDs(ctx, st, back, log, hindex, req, handles, out)
	case ropGetPropertyIdsFromNames:
		return hGetPropIDsFromNames(ctx, st, back, log, hindex, req, handles, out)
	case ropGetStreamSize:
		return hGetStreamSize(ctx, st, back, log, hindex, handles, out)
	case ropGetPerUserLongTermIds:
		return hGetPerUserLongTermIDs(ctx, st, back, log, hindex, req, handles, out)
	case ropSetReadFlags:
		return hSetReadFlags(ctx, st, back, log, hindex, req, handles, out)
	case ropGetReceiveFolderTable:
		return hGetReceiveFolderTable(ctx, st, back, log, hindex, handles, out)
	case ropGetStoreState:
		return hGetStoreState(ctx, st, back, log, hindex, handles, out)
	case ropTellVersion:
		return hTellVersion(ctx, st, back, log, hindex, req, handles, out)
	}
	// An unknown ROP aborts the list, matching gromox's dispatch default.
	return rpcAbort(ecNotSupported)
}

// writeROPResponse writes the standard framing and, when the result is
// ecSuccess and a body is given, the body bytes.
func writeROPResponse(out *bytes.Buffer, id, hindex uint8, result uint32, body func(w *writer) error) {
	out.WriteByte(id)
	out.WriteByte(hindex)
	var b [4]byte
	putU32LE(result, b[:])
	out.Write(b[:])
	if result != ecSuccess || body == nil {
		return
	}
	w := newWriter()
	if err := body(w); err != nil {
		out.Truncate(out.Len() - 6)
		out.WriteByte(id)
		out.WriteByte(hindex)
		putU32LE(ecError, b[:])
		out.Write(b[:])
		return
	}
	out.Write(w.buf)
}

// objAt resolves the object a handle stands for.
func objAt(st *sessionState, handles []uint32, hindex uint8) *mapiObject {
	if hindex >= uint8(len(handles)) {
		return nil
	}
	return st.get(handles[hindex])
}

// hRelease frees the object behind a handle. RopRelease writes no response.
func hRelease(st *sessionState, handle uint32) {
	st.drop(handle)
}

func loggedOut(st *sessionState) bool {
	return st.folders == nil
}

// allocHandle hands a fresh handle value to a creating ROP. It returns false
// when ohindex is outside the client's handle table (the abort case).
func allocHandle(st *sessionState, ohindex uint8, handles []uint32, o *mapiObject) bool {
	if ohindex >= uint8(len(handles)) {
		return false
	}
	h := st.newHandle()
	st.put(h, o)
	handles[ohindex] = h
	return true
}

// ---------------------------------------------------------------- RopLogon --

// hLogon handles RopLogon (MS-OXCSTOR 2.2.2.1):
//
//	u8  logon_flags (LOGON_FLAG_PRIVATE = 0x1)
//	u32 open_flags  (LOGON_OPEN_FLAG_USE_PER_MDB_REPLID_MAPPING = 0x01000000)
//	u32 store_state
//	u16 essdn_size + essdn (the mailbox distinguished name, NUL-terminated)
//
// The response is the LOGON_PMB_RESPONSE.
func hLogon(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	logonFlags, err := req.u8()
	if err != nil {
		return err
	}
	openFlags, err := req.u32()
	if err != nil {
		return err
	}
	if _, err = req.u32(); err != nil { // store_state
		return err
	}
	essdnSize, err := req.u16()
	if err != nil {
		return err
	}
	essdn, err := req.bytes(int(essdnSize))
	if err != nil {
		return err
	}
	_ = essdn

	if openFlags&logonOpenFlagUsePerMDBReplidMapping == 0 {
		writeROPResponse(out, ropLogon, hindex, ecInvalidParam, nil)
		return nil
	}
	if logonFlags&logonFlagPrivate == 0 {
		// No public-store support: reply with a redirect, like gromox for a
		// public logon.
		writeROPResponse(out, ropLogon, hindex, ecWrongServer, func(w *writer) error {
			w.u8(logonFlags)
			w.str(strings.TrimRight(string(essdn), "\x00"))
			return nil
		})
		return nil
	}
	if st.owner == "" {
		writeROPResponse(out, ropLogon, hindex, ecLoginFailure, nil)
		return nil
	}

	st.loggedOn = true
	if st.storeGUID == "" {
		st.storeGUID = back.StoreGUID(st.owner)
	}
	if st.objects == nil {
		st.objects = map[uint32]*mapiObject{}
		st.nextHandle = 1
		st.objects[0] = &mapiObject{kind: objLogon}
	}
	if st.folders == nil {
		fr, err := newFolderResolver(ctx, back, st.owner)
		if err != nil {
			writeROPResponse(out, ropLogon, hindex, ecError, nil)
			return nil
		}
		st.folders = fr
	}
	// The logon object always occupies handle value 0; the handle table now
	// points at it at hindex.
	handles[hindex] = 0

	writeROPResponse(out, ropLogon, hindex, ecSuccess, func(w *writer) error {
		w.u8(logonFlags)
		for _, fid := range logonFolderEIDs {
			w.u64(fid)
		}
		w.u8(responseFlagReserved | responseFlagOwnerRight | responseFlagSendAsRight)
		w.guid(st.storeGUID)
		w.u16(1) // replid
		w.guid(st.storeGUID)
		writeLogonTime(w, time.Now())
		w.u64(0) // gwart_time
		w.u32(0) // store_stat
		return nil
	})
	return nil
}

// writeLogonTime writes the LOGON_TIME structure
// (u8 sec, u8 min, u8 hour, u8 dow, u8 day, u8 month, u16 year).
func writeLogonTime(w *writer, t time.Time) {
	w.u8(uint8(t.Second()))
	w.u8(uint8(t.Minute()))
	w.u8(uint8(t.Hour()))
	w.u8(uint8(t.Weekday())) // 0 = Sunday, matching day_of_week
	w.u8(uint8(t.Day()))
	w.u8(uint8(t.Month()))
	w.u16(uint16(t.Year()))
}

// ------------------------------------------------------------- creating ops --

// hOpenFolder handles RopOpenFolder (MS-OXCFOLD 2.2.2.1):
//
//	u8  ohindex
//	u64 folder_id
//	u8  open_flags
func hOpenFolder(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	ohindex, err := req.u8()
	if err != nil {
		return err
	}
	fid, err := req.u64()
	if err != nil {
		return err
	}
	if _, err = req.u8(); err != nil { // open_flags
		return err
	}
	if loggedOut(st) {
		writeROPResponse(out, ropOpenFolder, ohindex, ecLoginFailure, nil)
		return nil
	}
	fo, err := st.folders.open(fid)
	if err != nil {
		writeROPResponse(out, ropOpenFolder, ohindex, ecNotFound, nil)
		return nil
	}
	if !allocHandle(st, ohindex, handles, &mapiObject{kind: objFolder, folder: fo}) {
		return rpcAbort(ecInvalidObject)
	}
	writeROPResponse(out, ropOpenFolder, ohindex, ecSuccess, func(w *writer) error {
		w.u8(0) // has_rules
		w.u8(0) // no ghost folder
		return nil
	})
	return nil
}

// hOpenMessage handles RopOpenMessage (MS-OXCFOLD 2.2.5.1):
//
//	u8  ohindex
//	u16 cpid
//	u64 folder_id
//	u8  open_mode_flags
//	u64 message_id
func hOpenMessage(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	ohindex, err := req.u8()
	if err != nil {
		return err
	}
	if _, err = req.u16(); err != nil { // cpid
		return err
	}
	folderID, err := req.u64()
	if err != nil {
		return err
	}
	if _, err = req.u8(); err != nil { // open_mode_flags
		return err
	}
	messageID, err := req.u64()
	if err != nil {
		return err
	}
	if loggedOut(st) {
		writeROPResponse(out, ropOpenMessage, ohindex, ecLoginFailure, nil)
		return nil
	}
	fo, err := st.folders.open(folderID)
	if err != nil {
		writeROPResponse(out, ropOpenMessage, ohindex, ecNotFound, nil)
		return nil
	}
	row, err := st.folders.messageRow(fidValue(messageID))
	if err != nil {
		writeROPResponse(out, ropOpenMessage, ohindex, ecNotFound, nil)
		return nil
	}
	mo := &messageObj{id: row.ID, folderValue: fo.value, row: row}
	mo.loadDetail(ctx, back, st.owner)
	if !allocHandle(st, ohindex, handles, &mapiObject{kind: objMessage, message: mo}) {
		return rpcAbort(ecInvalidObject)
	}
	writeROPResponse(out, ropOpenMessage, ohindex, ecSuccess, func(w *writer) error {
		w.u8(0) // has_named_properties
		w.typedStr("")
		w.typedStr(row.Subject)
		w.u16(0) // recipient_count
		if err := w.proptags(nil); err != nil {
			return err
		}
		w.u8(0) // recipient row count
		return nil
	})
	return nil
}

// hGetTable handles RopGetHierarchyTable (0x04) and RopGetContentsTable (0x05):
//
//	u8 ohindex
//	u8 table_flags
func hGetTable(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer, hierarchy bool) error {

	ohindex, err := req.u8()
	if err != nil {
		return err
	}
	if _, err = req.u8(); err != nil { // table_flags
		return err
	}
	tag := uint8(ropGetContentsTable)
	if hierarchy {
		tag = ropGetHierarchyTable
	}
	if loggedOut(st) {
		writeROPResponse(out, tag, ohindex, ecLoginFailure, nil)
		return nil
	}
	parent := objAt(st, handles, hindex)
	if parent == nil || parent.folder == nil {
		writeROPResponse(out, tag, ohindex, ecNullObject, nil)
		return nil
	}
	records, err := st.folders.tableRecords(parent.folder, hierarchy)
	if err != nil {
		writeROPResponse(out, tag, ohindex, ecError, nil)
		return nil
	}
	tbl := &tableObj{
		hierarchy: hierarchy,
		folder:    parent.folder,
		records:   records,
		bookmarks: map[uint32]int{},
	}
	kind := objContentsTable
	if hierarchy {
		kind = objHierarchyTable
	}
	if !allocHandle(st, ohindex, handles, &mapiObject{kind: kind, table: tbl}) {
		return rpcAbort(ecInvalidObject)
	}
	writeROPResponse(out, tag, ohindex, ecSuccess, func(w *writer) error {
		w.u32(uint32(len(records)))
		return nil
	})
	return nil
}

// ------------------------------------------------------------------ tables --

// tableAt resolves the table handle; unit is the ROP id for the error
// response. It mirrors gromox: an unresolvable handle is ecNullObject, a
// handle that is not a table is ecNotSupported.
func tableAt(st *sessionState, handles []uint32, hindex uint8) (*tableObj, uint32) {
	obj := objAt(st, handles, hindex)
	if obj == nil {
		return nil, ecNullObject
	}
	if obj.table == nil {
		return nil, ecNotSupported
	}
	return obj.table, ecSuccess
}

// hSetColumns handles RopSetColumns (MS-OXCTABL 2.2.1.1):
//
//	u8 table_flags
//	proptag_a
func hSetColumns(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	if _, err := req.u8(); err != nil { // table_flags
		return err
	}
	tags, err := req.proptags()
	if err != nil {
		return err
	}
	tbl, code := tableAt(st, handles, hindex)
	if code != ecSuccess {
		writeROPResponse(out, ropSetColumns, hindex, code, nil)
		return nil
	}
	tbl.columns = append([]uint32(nil), tags...)
	writeROPResponse(out, ropSetColumns, hindex, ecSuccess, func(w *writer) error {
		w.u8(tblStatComplete)
		return nil
	})
	return nil
}

// hSortTable handles RopSortTable. The SortOrderSet is consumed and ignored
// (rows keep insertion order):
//
//	u8 table_flags
//	u16 count, then per criterion: u16 type + u32 proptag + u8 order
func hSortTable(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	if _, err := req.u8(); err != nil {
		return err
	}
	count, err := req.u16()
	if err != nil {
		return err
	}
	for i := 0; i < int(count); i++ {
		if _, err = req.u16(); err != nil {
			return err
		}
		if _, err = req.u32(); err != nil {
			return err
		}
		if _, err = req.u8(); err != nil {
			return err
		}
	}
	if _, code := tableAt(st, handles, hindex); code != ecSuccess {
		writeROPResponse(out, ropSortTable, hindex, code, nil)
		return nil
	}
	writeROPResponse(out, ropSortTable, hindex, ecSuccess, func(w *writer) error {
		w.u8(tblStatComplete)
		return nil
	})
	return nil
}

// hRestrict handles RopRestrict. The restriction is consumed and ignored:
//
//	u8 res_flags
//	u16 res_size, then res_size bytes of the restriction structure
func hRestrict(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	if _, err := req.u8(); err != nil { // res_flags
		return err
	}
	resSize, err := req.u16()
	if err != nil {
		return err
	}
	if _, err = req.bytes(int(resSize)); err != nil {
		return err
	}
	if _, code := tableAt(st, handles, hindex); code != ecSuccess {
		writeROPResponse(out, ropRestrict, hindex, code, nil)
		return nil
	}
	writeROPResponse(out, ropRestrict, hindex, ecSuccess, func(w *writer) error {
		w.u8(tblStatComplete)
		return nil
	})
	return nil
}

// hQueryRows handles RopQueryRows (MS-OXCTABL 2.2.4.1):
//
//	u8  flags (QUERY_ROWS_*)
//	u8  forward_read (0 = backward)
//	u16 row_count
func hQueryRows(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	flags, err := req.u8()
	if err != nil {
		return err
	}
	forwardRead, err := req.u8()
	if err != nil {
		return err
	}
	rowCount, err := req.u16()
	if err != nil {
		return err
	}
	tbl, code := tableAt(st, handles, hindex)
	if code != ecSuccess {
		writeROPResponse(out, ropQueryRows, hindex, code, nil)
		return nil
	}
	columns := tbl.columns
	if len(columns) == 0 {
		if tbl.hierarchy {
			columns = hierarchyDefaultColumns
		} else {
			columns = contentsDefaultColumns
		}
	}

	forward := forwardRead != 0
	total := len(tbl.records)
	start := tbl.position
	var rows []*tableRecord
	if forward {
		end := start + int(rowCount)
		if end > total {
			end = total
		}
		rows = tbl.records[start:end]
	} else {
		begin := start - int(rowCount)
		if begin < 0 {
			begin = 0
		}
		rows = tbl.records[begin:start]
	}
	count := len(rows)

	wr := newWriter()
	for _, rec := range rows {
		if err := writePropRow(wr, columns, rec.props); err != nil {
			break
		}
	}
	if flags&queryRowsNoAdvance == 0 {
		if forward {
			tbl.position = start + count
		} else {
			tbl.position = start - count
		}
	}
	seekPos := uint8(bookmarkCurrent)
	if forward && tbl.position >= total {
		seekPos = bookmarkEnd
	} else if !forward && tbl.position == 0 {
		seekPos = bookmarkBeginning
	}

	writeROPResponse(out, ropQueryRows, hindex, ecSuccess, func(w *writer) error {
		w.u8(seekPos)
		w.u16(uint16(count))
		return w.bin(wr.buf)
	})
	return nil
}

func hGetStatus(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, handles []uint32, out *bytes.Buffer) error {
	if _, code := tableAt(st, handles, hindex); code != ecSuccess {
		writeROPResponse(out, ropGetStatus, hindex, code, nil)
		return nil
	}
	writeROPResponse(out, ropGetStatus, hindex, ecSuccess, func(w *writer) error {
		w.u8(tblStatComplete)
		return nil
	})
	return nil
}

func hQueryPosition(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, handles []uint32, out *bytes.Buffer) error {
	tbl, code := tableAt(st, handles, hindex)
	if code != ecSuccess {
		writeROPResponse(out, ropQueryPosition, hindex, code, nil)
		return nil
	}
	writeROPResponse(out, ropQueryPosition, hindex, ecSuccess, func(w *writer) error {
		w.u32(uint32(tbl.position))
		w.u32(uint32(len(tbl.records)))
		return nil
	})
	return nil
}

// seekOrigin resolves one of the bookmark beginning/current/end constants to
// an absolute row index in a table.
func seekOrigin(tbl *tableObj, origin uint8) int {
	switch origin {
	case bookmarkBeginning:
		return 0
	case bookmarkEnd:
		return len(tbl.records)
	default:
		return tbl.position
	}
}

// hSeekRow handles RopSeekRow (MS-OXCTABL 2.2.6.1):
//
//	u8  seek_pos (origin)
//	i32 offset
//	u8  want_moved_count
func hSeekRow(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	origin, err := req.u8()
	if err != nil {
		return err
	}
	offset, err := req.i32()
	if err != nil {
		return err
	}
	if _, err = req.u8(); err != nil { // want_moved_count
		return err
	}
	tbl, code := tableAt(st, handles, hindex)
	if code != ecSuccess {
		writeROPResponse(out, ropSeekRow, hindex, code, nil)
		return nil
	}
	base := seekOrigin(tbl, origin)
	hasLess, target := seekClamp(base, offset, len(tbl.records))
	tbl.position = target
	moved := target - base
	writeROPResponse(out, ropSeekRow, hindex, ecSuccess, func(w *writer) error {
		w.u8(0)
		if hasLess {
			w.u8(1)
		}
		w.i32(int32(moved))
		return nil
	})
	return nil
}

// seekClamp clamps base+offset into [0,total]; hasLess reports the clamp.
func seekClamp(base int, offset int32, total int) (bool, int) {
	target := base + int(offset)
	if target < 0 {
		return true, 0
	}
	if target > total {
		return true, total
	}
	return false, target
}

// hSeekRowBookmark handles RopSeekRowBookmark (MS-OXCTABL 2.2.7.1):
//
//	bin bookmark
//	i32 offset
//	u8  want_moved_count
func hSeekRowBookmark(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	bookmark, err := req.bin()
	if err != nil {
		return err
	}
	offset, err := req.i32()
	if err != nil {
		return err
	}
	if _, err = req.u8(); err != nil { // want_moved_count
		return err
	}
	tbl, code := tableAt(st, handles, hindex)
	if code != ecSuccess {
		writeROPResponse(out, ropSeekRowBookmark, hindex, code, nil)
		return nil
	}
	bm := decodeBookmark(bookmark)
	pos, ok := tbl.bookmarks[bm]
	if !ok {
		writeROPResponse(out, ropSeekRowBookmark, hindex, ecInvalidBookmark, nil)
		return nil
	}
	// This server never deletes table rows while a bookmark is held, so the
	// bookmark always resolves to a visible row: row_invisible stays 0.
	hasLess, target := seekClamp(pos, offset, len(tbl.records))
	tbl.position = target
	moved := target - pos
	writeROPResponse(out, ropSeekRowBookmark, hindex, ecSuccess, func(w *writer) error {
		w.u8(0) // row_invisible
		w.u8(0)
		if hasLess {
			w.u8(1)
		}
		w.u32(uint32(loSought(hasLess, moved)))
		return nil
	})
	return nil
}

// loSought is the unsigned offset_sought for the SEEKROWBOOKMARK response.
func loSought(hasLess bool, v int) uint32 {
	if hasLess && v < 0 {
		return 0
	}
	if v < 0 {
		return uint32(-v)
	}
	return uint32(v)
}

// decodeBookmark recovers the value CreateBookmark stored in the 4-byte blob.
func decodeBookmark(b []byte) uint32 {
	if len(b) != 4 {
		return 0
	}
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func hCreateBookmark(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, handles []uint32, out *bytes.Buffer) error {
	tbl, code := tableAt(st, handles, hindex)
	if code != ecSuccess {
		writeROPResponse(out, ropCreateBookmark, hindex, code, nil)
		return nil
	}
	tbl.nextBM++
	bm := tbl.nextBM
	tbl.bookmarks[bm] = tbl.position
	writeROPResponse(out, ropCreateBookmark, hindex, ecSuccess, func(w *writer) error {
		w.u16(4)
		w.u32(bm)
		return nil
	})
	return nil
}

func hFreeBookmark(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {
	bookmark, err := req.bin()
	if err != nil {
		return err
	}
	tbl, code := tableAt(st, handles, hindex)
	if code != ecSuccess {
		writeROPResponse(out, ropFreeBookmark, hindex, code, nil)
		return nil
	}
	delete(tbl.bookmarks, decodeBookmark(bookmark))
	writeROPResponse(out, ropFreeBookmark, hindex, ecSuccess, nil)
	return nil
}

func hDeleteMessages(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {
	if _, err := req.u8(); err != nil { // want_asynchronous
		return err
	}
	if _, err := req.u8(); err != nil { // notify_non_read
		return err
	}
	if _, err := req.eids(); err != nil { // message ids
		return err
	}
	writeROPResponse(out, ropDeleteMessages, hindex, ecSuccess, func(w *writer) error {
		w.u16(0) // no problems
		return nil
	})
	return nil
}

func hGetMessageStatus(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	messageID, err := req.u64()
	if err != nil {
		return err
	}
	if loggedOut(st) {
		writeROPResponse(out, ropSetMessageStatus, hindex, ecLoginFailure, nil)
		return nil
	}
	row, err := st.folders.messageRow(fidValue(messageID))
	if err != nil {
		writeROPResponse(out, ropSetMessageStatus, hindex, ecNotFound, nil)
		return nil
	}
	status := uint32(msgFlagOriginInternet)
	for _, f := range row.Flags {
		if f == `\Seen` {
			status |= msgFlagRead
		}
	}
	// The response is stamped 0x20 (RopSetMessageStatus), per gromox.
	writeROPResponse(out, ropSetMessageStatus, hindex, ecSuccess, func(w *writer) error {
		w.u32(status)
		return nil
	})
	return nil
}

// hGetReceiveFolder handles RopGetReceiveFolder (MS-OXCFOLD 2.2.13.1):
//
//	8-bit NUL-terminated message class
func hGetReceiveFolder(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	cls, err := req.zeroTerminated8()
	if err != nil {
		return err
	}
	if loggedOut(st) {
		writeROPResponse(out, ropGetReceiveFolder, hindex, ecLoginFailure, nil)
		return nil
	}
	writeROPResponse(out, ropGetReceiveFolder, hindex, ecSuccess, func(w *writer) error {
		w.u64(receiveFolderFor(cls))
		w.str(cls)
		return nil
	})
	return nil
}

// receiveFolderFor maps a message class to its receive-folder eid.
func receiveFolderFor(cls string) uint64 {
	switch {
	case strings.HasPrefix(cls, "IPM.Appointment"),
		strings.HasPrefix(cls, "IPM.Schedule"):
		return fidFromValue(PRIVATE_FID_CALENDAR)
	default:
		return fidFromValue(PRIVATE_FID_INBOX)
	}
}

// hGetReceiveFolderTable handles RopGetReceiveFolderTable (MS-OXCFOLD 2.2.16.1).
// gromox's columns: PidTagFolderId, PR_MESSAGE_CLASS_A, PR_LAST_MODIFICATION_TIME.
func hGetReceiveFolderTable(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, handles []uint32, out *bytes.Buffer) error {
	if loggedOut(st) {
		writeROPResponse(out, ropGetReceiveFolderTable, hindex, ecLoginFailure, nil)
		return nil
	}
	cols := []uint32{pidTagFolderID, prMessageClassA, prLastModificationTime}
	row := map[uint32]any{
		pidTagFolderID:         uint64(fidFromValue(PRIVATE_FID_INBOX)),
		prMessageClassA:        "IPM.Note",
		prLastModificationTime: time.Now(),
	}
	writeROPResponse(out, ropGetReceiveFolderTable, hindex, ecSuccess, func(w *writer) error {
		w.u32(1)
		return writePropRow(w, cols, row)
	})
	return nil
}

func hRegisterNotification(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	ohindex, err := req.u8()
	if err != nil {
		return err
	}
	if _, err = req.u8(); err != nil { // notification_types
		return err
	}
	if _, err = req.u8(); err != nil { // reserved
		return err
	}
	wantWholeStore, err := req.u8()
	if err != nil {
		return err
	}
	if wantWholeStore == 0 {
		if _, err = req.u64(); err != nil { // folder_id
			return err
		}
		if _, err = req.u64(); err != nil { // message_id
			return err
		}
	}
	if !allocHandle(st, ohindex, handles, &mapiObject{kind: objLogon}) {
		return rpcAbort(ecInvalidObject)
	}
	writeROPResponse(out, ropRegisterNotification, ohindex, ecSuccess, nil)
	return nil
}

// ------------------------------------------------------------- message props --

// propsOf returns the property set of an object handle. Body loading happens
// here so GetProperties on an open message triggers the heavy read once.
func propsOf(ctx context.Context, st *sessionState, back *Backend, obj *mapiObject) map[uint32]any {
	switch obj.kind {
	case objLogon:
		return st.folders.storeProps(st.owner)
	case objFolder:
		return st.folders.folderPropsOf(obj.folder)
	case objMessage:
		obj.message.loadDetail(ctx, back, st.owner)
		return st.folders.messageDetailProps(obj.message)
	case objStream:
		return map[uint32]any{}
	}
	return nil
}

// hGetPropsSpecific handles RopGetPropertiesSpecific (MS-OXCPRPT 2.2.4.1):
//
//	u16 size_limit
//	u16 want_unicode
//	proptag_a
func hGetPropsSpecific(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	if _, err := req.u16(); err != nil { // size_limit
		return err
	}
	if _, err := req.u16(); err != nil { // want_unicode
		return err
	}
	tags, err := req.proptags()
	if err != nil {
		return err
	}
	obj := objAt(st, handles, hindex)
	if obj == nil {
		writeROPResponse(out, ropGetPropertiesSpecific, hindex, ecNullObject, nil)
		return nil
	}
	props := propsOf(ctx, st, back, obj)
	if props == nil {
		writeROPResponse(out, ropGetPropertiesSpecific, hindex, ecNullObject, nil)
		return nil
	}
	writeROPResponse(out, ropGetPropertiesSpecific, hindex, ecSuccess, func(w *writer) error {
		return writeGetPropsRow(w, tags, props)
	})
	return nil
}

// hGetPropsAll handles RopGetPropertiesAll (MS-OXCPRPT 2.2.3.1):
//
//	u16 size_limit
//	u16 want_unicode
func hGetPropsAll(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	if _, err := req.u16(); err != nil { // size_limit
		return err
	}
	if _, err := req.u16(); err != nil { // want_unicode
		return err
	}
	obj := objAt(st, handles, hindex)
	if obj == nil {
		writeROPResponse(out, ropGetPropertiesAll, hindex, ecNullObject, nil)
		return nil
	}
	props := propsOf(ctx, st, back, obj)
	if props == nil {
		writeROPResponse(out, ropGetPropertiesAll, hindex, ecNullObject, nil)
		return nil
	}
	writeROPResponse(out, ropGetPropertiesAll, hindex, ecSuccess, func(w *writer) error {
		return writeTPropval(w, props)
	})
	return nil
}

// hGetPropsList handles RopGetPropertiesList (MS-OXCPRPT 2.2.8.1): no body.
func hGetPropsList(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {
	obj := objAt(st, handles, hindex)
	if obj == nil {
		writeROPResponse(out, ropGetPropertiesList, hindex, ecNullObject, nil)
		return nil
	}
	props := propsOf(ctx, st, back, obj)
	if props == nil {
		writeROPResponse(out, ropGetPropertiesList, hindex, ecNullObject, nil)
		return nil
	}
	writeROPResponse(out, ropGetPropertiesList, hindex, ecSuccess, func(w *writer) error {
		return w.proptags(sortedTags(props))
	})
	return nil
}

func hSetProps(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, out *bytes.Buffer) error {
	size, err := req.u16()
	if err != nil {
		return err
	}
	if _, err = req.bytes(int(size)); err != nil {
		return err
	}
	writeROPResponse(out, ropSetProperties, hindex, ecSuccess, func(w *writer) error {
		w.u16(0) // no problems
		return nil
	})
	return nil
}

func hDeleteProps(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, out *bytes.Buffer) error {
	if _, err := req.proptags(); err != nil {
		return err
	}
	writeROPResponse(out, ropDeleteProperties, hindex, ecSuccess, func(w *writer) error {
		w.u16(0)
		return nil
	})
	return nil
}

// ------------------------------------------------------------- stream holds --

func hOpenStream(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	ohindex, err := req.u8()
	if err != nil {
		return err
	}
	proptag, err := req.u32()
	if err != nil {
		return err
	}
	if _, err = req.u8(); err != nil { // flags
		return err
	}
	parent := objAt(st, handles, hindex)
	if parent == nil || parent.message == nil {
		writeROPResponse(out, ropOpenStream, ohindex, ecNullObject, nil)
		return nil
	}
	m := parent.message
	m.loadDetail(ctx, back, st.owner)
	if m.detail == nil {
		writeROPResponse(out, ropOpenStream, ohindex, ecNotFound, nil)
		return nil
	}
	var content []byte
	switch propID(proptag) {
	case propID(prBody):
		content = utf16LE(m.detail.TextBody)
	case propID(prBodyHTML):
		content = []byte(m.detail.HTMLBody)
	default:
		writeROPResponse(out, ropOpenStream, ohindex, ecNotFound, nil)
		return nil
	}
	if !allocHandle(st, ohindex, handles, &mapiObject{kind: objStream, stream: &streamObj{data: content}}) {
		return rpcAbort(ecInvalidObject)
	}
	writeROPResponse(out, ropOpenStream, ohindex, ecSuccess, func(w *writer) error {
		w.u32(uint32(len(content)))
		return nil
	})
	return nil
}

func hReadStream(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	byteCount, err := req.u16()
	if err != nil {
		return err
	}
	limit := int(byteCount)
	if byteCount == 0xBABE { // RopReadStream max-bytes form
		maxBytes, err := req.u32()
		if err != nil {
			return err
		}
		limit = int(maxBytes)
	}
	obj := objAt(st, handles, hindex)
	if obj == nil || obj.stream == nil {
		writeROPResponse(out, ropReadStream, hindex, ecNullObject, nil)
		return nil
	}
	s := obj.stream
	end := s.pos + int64(limit)
	if end > int64(len(s.data)) {
		end = int64(len(s.data))
	}
	if end < s.pos {
		end = s.pos
	}
	data := s.data[s.pos:end]
	s.pos = end
	writeROPResponse(out, ropReadStream, hindex, ecSuccess, func(w *writer) error {
		return w.bin(data)
	})
	return nil
}

func hGetStreamSize(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, handles []uint32, out *bytes.Buffer) error {
	obj := objAt(st, handles, hindex)
	if obj == nil || obj.stream == nil {
		writeROPResponse(out, ropGetStreamSize, hindex, ecNullObject, nil)
		return nil
	}
	writeROPResponse(out, ropGetStreamSize, hindex, ecSuccess, func(w *writer) error {
		w.u32(uint32(len(obj.stream.data)))
		return nil
	})
	return nil
}

// ------------------------------------------------------ named props lookups --

func hGetPropIDsFromNames(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	if _, err := req.u8(); err != nil { // flags
		return err
	}
	names, err := req.propNameArray()
	if err != nil {
		return err
	}
	ids := make([]uint16, len(names))
	for i := range names {
		// Named properties are assigned in the 0x8000-0x8FFF range and get
		// the same id on every call, so the ids are stable within a session.
		ids[i] = 0x8000 + uint16(i)
	}
	writeROPResponse(out, ropGetPropertyIdsFromNames, hindex, ecSuccess, func(w *writer) error {
		return w.propids(ids)
	})
	return nil
}

func hGetNamesFromPropIDs(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {

	ids, err := req.propids()
	if err != nil {
		return err
	}
	pns := make([]propName, len(ids))
	for i, id := range ids {
		// The store does not retain name mappings yet, so a named id is
		// echoed back as an MNID_ID with the same LID.
		pns[i] = propName{Kind: 0, GUID: "", LID: uint32(id)}
	}
	writeROPResponse(out, ropGetNamesFromPropertyIds, hindex, ecSuccess, func(w *writer) error {
		return w.propNameArray(pns)
	})
	return nil
}

func hQueryColumnsAll(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, handles []uint32, out *bytes.Buffer) error {
	tbl, code := tableAt(st, handles, hindex)
	if code != ecSuccess {
		writeROPResponse(out, ropQueryColumnsAll, hindex, code, nil)
		return nil
	}
	tags := tbl.columns
	if len(tags) == 0 {
		if tbl.hierarchy {
			tags = hierarchyDefaultColumns
		} else {
			tags = contentsDefaultColumns
		}
	}
	writeROPResponse(out, ropQueryColumnsAll, hindex, ecSuccess, func(w *writer) error {
		return w.proptags(tags)
	})
	return nil
}

func hGetPerUserLongTermIDs(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {
	if _, err := req.guid(); err != nil {
		return err
	}
	writeROPResponse(out, ropGetPerUserLongTermIds, hindex, ecSuccess, func(w *writer) error {
		w.u16(0) // empty LONG_TERM_ID_ARRAY
		return nil
	})
	return nil
}

func hSetReadFlags(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {
	if _, err := req.u8(); err != nil { // want_asynchronous
		return err
	}
	if _, err := req.u8(); err != nil { // read_flags
		return err
	}
	if _, err := req.eids(); err != nil { // message ids
		return err
	}
	// No-op read state; an empty problem array keeps the exchange well-formed.
	writeROPResponse(out, ropSetReadFlags, hindex, ecSuccess, func(w *writer) error {
		w.u16(0)
		return nil
	})
	return nil
}

func hGetStoreState(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, handles []uint32, out *bytes.Buffer) error {
	writeROPResponse(out, ropGetStoreState, hindex, ecSuccess, func(w *writer) error {
		w.u32(0) // store_stat: ready
		return nil
	})
	return nil
}

func hTellVersion(ctx context.Context, st *sessionState, back *Backend, log *slog.Logger,
	hindex uint8, req *reader, handles []uint32, out *bytes.Buffer) error {
	for i := 0; i < 3; i++ {
		if _, err := req.u16(); err != nil {
			return err
		}
	}
	writeROPResponse(out, ropTellVersion, hindex, ecSuccess, nil)
	return nil
}

// sortedTags returns the property tags of a prop map in ascending order.
func sortedTags(props map[uint32]any) []uint32 {
	tags := make([]uint32, 0, len(props))
	for t := range props {
		tags = append(tags, t)
	}
	for i := 1; i < len(tags); i++ {
		for j := i; j > 0 && tags[j] < tags[j-1]; j-- {
			tags[j], tags[j-1] = tags[j-1], tags[j]
		}
	}
	return tags
}

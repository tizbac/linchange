package nspi

import (
	"sort"
	"strings"
)

// The 19 NSPI operations (MS-OXNSPI 3.1.4.1). Each one follows the
// reference implementation in the order the reference itself is written:
// the request body is parsed in full first (a body that does not parse is
// the transport's invalid_rq_body, not an operation result), then the
// operation's rules are applied, then the response is written.
//
// Two things hold for all of them:
//
//   - Status, the first response dword, is always zero. The reference never
//     assigns it; the operation's outcome goes into Result.
//   - Once the body parses, every outcome — every failure included — is
//     written as a complete response. The fields after Result are fixed in
//     number and kind by the operation: an error shows up as Result plus the
//     operation's own "no value" markers (an absent row, an absent row set),
//     never as a truncated body.
//
// Request shapes follow the reference's g_nsp_request overloads exactly,
// including where it is lenient: an optional field's presence byte means
// "absent" only when it is zero, and a trailing auxiliary buffer is read and
// discarded rather than validated.

// resp writes the common response shape: Status (zero), Result, whatever the
// operation appends, and the trailing zero dword every NSPI response ends
// with. payload may be nil for operations that write nothing in between.
func resp(result uint32, payload func(*writer) error) ([]byte, error) {
	w := newWriter()
	w.u32(0)
	w.u32(result)
	if payload != nil {
		if err := payload(w); err != nil {
			return nil, err
		}
	}
	w.u32(0)
	return w.buf, nil
}

// respStat writes the shape shared by GetMatches, QueryRows, ResolveNames,
// ResortRestriction, SeekEntries and UpdateStat: Status, Result, an
// unconditionally present STAT, then the operation's tail.
func respStat(result uint32, st stat, tail func(*writer) error) ([]byte, error) {
	return resp(result, func(w *writer) error {
		w.u8(0xFF)
		w.writeStat(st)
		if tail != nil {
			return tail(w)
		}
		return nil
	})
}

// errTail is the "the operation produced no rows" marker the row-bearing
// operations write when Result is a failure: an absent mids array followed
// by an absent column-row set.
func errTail(w *writer) {
	w.u8(0)
	w.u8(0)
}

// readOptStat reads the optional STAT most requests open with. An absent
// field is the all-zero STAT the reference leaves in the request struct.
func readOptStat(r *reader) (stat, error) {
	pres, err := r.u8()
	if err != nil {
		return stat{}, err
	}
	if pres == 0 {
		return stat{}, nil
	}
	return r.readStat()
}

// readOptProptags reads an optional tag array (the presence byte, then a
// u32-count array of tags). The nil/non-nil distinction is the one the
// reference's std::optional makes and it is load-bearing: nil means the
// client sent no column list at all, which sends the server to its default
// tag set, while a present-but-empty list asks it to fetch nothing.
func readOptProptags(r *reader) ([]uint32, error) {
	pres, err := r.u8()
	if err != nil {
		return nil, err
	}
	if pres == 0 {
		return nil, nil
	}
	return r.proptags(4)
}

// readOptStrings8 reads an optional array of 8-bit strings without the ABK
// element presence bytes (the reference reads DnToMid's names with its
// address-book mode switched off).
func readOptStrings8(r *reader) ([]string, error) {
	pres, err := r.u8()
	if err != nil {
		return nil, err
	}
	if pres == 0 {
		return nil, nil
	}
	return r.str8Array(false)
}

// readOptStringsW reads an optional array of UTF-16 strings without element
// presence bytes: ResolveNames parses its names with address-book mode
// switched off.
func readOptStringsW(r *reader) ([]string, error) {
	pres, err := r.u8()
	if err != nil {
		return nil, err
	}
	if pres == 0 {
		return nil, nil
	}
	return r.wstrArray(false)
}

// readOptBinaries reads an optional array of binaries (ModLinkAtt's entry
// ids), again without element presence bytes.
func readOptBinaries(r *reader) ([][]byte, error) {
	pres, err := r.u8()
	if err != nil {
		return nil, err
	}
	if pres == 0 {
		return nil, nil
	}
	return r.binArray(false)
}

// readOptU32 reads an optional single u32 (GetSpecialTable's version).
func readOptU32(r *reader) (*uint32, error) {
	pres, err := r.u8()
	if err != nil {
		return nil, err
	}
	if pres == 0 {
		return nil, nil
	}
	v, err := r.u32()
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// readOptProp reads one optional tagged property value in ABK form (the
// SeekEntries target): absence yields a zero prop, whose tag of 0 fails the
// sort-type tag check below.
func readOptProp(r *reader) (prop, error) {
	pres, err := r.u8()
	if err != nil {
		return prop{}, err
	}
	if pres == 0 {
		return prop{}, nil
	}
	tag, err := r.u32()
	if err != nil {
		return prop{}, err
	}
	v, err := r.decPropVal(propType(tag), true)
	if err != nil {
		return prop{}, err
	}
	return prop{tag: tag, value: v}, nil
}

// readOptValues reads ModProps' optional values array: a u32 count of tagged
// property values.
func readOptValues(r *reader) ([]prop, error) {
	pres, err := r.u8()
	if err != nil {
		return nil, err
	}
	if pres == 0 {
		return nil, nil
	}
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	// A tagged value is at least five bytes (a u32 tag plus one byte of
	// value), so the count cannot outrun the buffer and be turned into an
	// allocation.
	if n > uint32(r.remaining()/5) {
		return nil, formatError
	}
	out := make([]prop, 0, n)
	for i := uint32(0); i < n; i++ {
		p, err := readOptProp(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// readOptRestriction reads an optional GetMatches filter.
func readOptRestriction(r *reader) (*restNode, error) {
	pres, err := r.u8()
	if err != nil {
		return nil, err
	}
	if pres == 0 {
		return nil, nil
	}
	return parseRestriction(r)
}

// readOptPropName reads the optional property name GetMatches carries. The
// reference parses it and then refuses the request (reserved or a name in a
// GetMatches means ecNotSupported), so the value is read only to advance the
// stream; the returned flag is all the handler needs.
func readOptPropName(r *reader) (bool, error) {
	pres, err := r.u8()
	if err != nil {
		return false, err
	}
	if pres == 0 {
		return false, nil
	}
	if _, err := r.guid(); err != nil {
		return false, err
	}
	if _, err := r.u32(); err != nil {
		return false, err
	}
	return true, nil
}

// skipAux reads and discards the auxiliary buffer that ends every NSPI
// request. Its contents are an opaque, server-defined blob (MS-OXCMAPIHTTP
// 2.2.1) that nothing here interprets, but its length field has to be
// consumed or the body would not parse.
func skipAux(r *reader) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	if n > uint32(r.remaining()) {
		return formatError
	}
	return r.skip(int(n))
}

// flatContainer reports whether a STAT container id names the flat
// recipient list rather than a node inside it. The reference's address book
// is a tree with a special GAL root (minid 0x0F) whose children are every
// recipient, and a client's STAT starts at container 0; both name the same
// rows, and only those two ids ever do.
func flatContainer(id uint32) bool { return id == 0 || id == galContainerID }

// containerRows resolves a STAT container onto the rows it holds. ok is
// false only when the id names nothing at all, which callers report as
// ecInvalidBookmark.
//
// An id that names a recipient is an existing container holding no rows: the
// reference's tree gives user nodes no children, and its GetMatches answers
// such a container with a success and no rows — which is also what the
// reference writes into STAT after a GetMatches, so a client echoing its own
// STAT back lands here rather than on an error.
func (s *Session) containerRows(recs []record, id uint32) ([]record, bool) {
	if flatContainer(id) {
		return recs, true
	}
	if _, ok := s.lookup(recs, id); ok {
		return nil, true
	}
	return nil, false
}

// statPosition resolves STAT's cursor onto a container's rows, returning the
// index the next row is read from and the number of rows the container
// holds. Flat containers use the list rules (the cursor may name a record
// anywhere in the GAL), nested ones the table rules (the cursor names a
// child, and a container here never has any).
func (s *Session) statPosition(st *stat, rows []record, id uint32) (uint32, uint32) {
	if flatContainer(id) {
		return s.positionInList(st, rows)
	}
	return positionInTable(st, uint32(len(rows)))
}

// positionInList mirrors nsp_interface_position_in_list. BEGINNING and END
// are the absolute anchors of MS-OXNSPI 3.1.4.5.1; CURRENT is the fractional
// position of 3.1.4.5.2, clamped onto the last row rather than past it; any
// other value is a minid whose index the cursor takes. An unknown minid
// parks the cursor on the first row, because the position of a record that
// is not there is undefined and the reference picks row zero for it.
func (s *Session) positionInList(st *stat, rows []record) (uint32, uint32) {
	total := uint32(len(rows))
	switch st.curRec {
	case currentOfTable:
		row := fractional(total, st.numPos, st.totalRec)
		if row > 0 && row >= total {
			row = total - 1
		}
		return row, total
	case beginningOfTable:
		return 0, total
	case endOfTable:
		return total, total
	}
	if i := s.position(rows, st.curRec); i < 0 {
		return 0, total
	} else {
		return uint32(i), total
	}
}

// positionInTable mirrors nsp_interface_position_in_table for a container
// whose children the address book holds: CURRENT is fractional, BEGINNING
// and END absolute, and an absolute cursor naming no child lands on the
// first row. Linchange's containers are the two roots, which are handled by
// positionInList, and recipient-named containers, which hold nothing — so
// the absolute branch is only ever reached with an empty container, where
// every answer is 0 anyway.
func positionInTable(st *stat, children uint32) (uint32, uint32) {
	switch st.curRec {
	case currentOfTable:
		row := fractional(children, st.numPos, st.totalRec)
		if row > children {
			row = children
		}
		return row, children
	case beginningOfTable:
		return 0, children
	case endOfTable:
		return children, children
	}
	return 0, children
}

// fractional computes the MS-OXNSPI 3.1.4.5.2 fractional position: how far
// into a table of total rows the pair (numPos, totalRec) points. A zero
// totalRec names no position at all and reads as the first row.
func fractional(total, numPos, totalRec uint32) uint32 {
	if totalRec == 0 {
		return 0
	}
	return uint32(float64(total) * float64(numPos) / float64(totalRec))
}

// fetchColumns returns the tags the server fetches for a row: the client's
// own column list when it sent one, the default set when it sent none. An
// empty list fetches nothing, which is the reference's behaviour for a
// client that names its columns and then names none.
func fetchColumns(respCols []uint32) []uint32 {
	if respCols != nil {
		return respCols
	}
	return defaultTags()
}

// hasError reports whether any cell of a fetched row carries an error, which
// is what turns a GetProps result from success into warn-with-errors.
func hasError(cells []prop) bool {
	for _, p := range cells {
		if _, isErr := errCode(p.value); isErr {
			return true
		}
	}
	return false
}

// errorCells builds the row the reference writes for an object that could
// not be fetched: every requested tag, each carrying status 0.
func errorCells(tags []uint32) []prop {
	out := make([]prop, len(tags))
	for i, t := range tags {
		out[i] = prop{tag: t, value: errCell{0}}
	}
	return out
}

// keepPresent drops the cells a record has no value for. GetProps does this
// only when the client asked for no tags in particular: the canonical list
// it then works from should carry no error for a property the directory
// simply does not store, which is what MS-OXNSPI 3.1.4.1.7 has the
// server answer with.
func keepPresent(cells []prop) []prop {
	out := cells[:0]
	for _, p := range cells {
		if code, isErr := errCode(p.value); isErr && code == ecNotFound {
			continue
		}
		out = append(out, p)
	}
	return out
}

// lowerName is the comparison key the address book orders and searches by:
// the display name, case-insensitively. Ties are not broken anywhere the
// reference does not break them either.
func lowerName(rec *record) string { return strings.ToLower(rec.name) }

// resolveNode is the reference's test of one resolution keyword against one
// record: the display name contains it, or the X500 DN equals it, or the
// address contains it — each case-insensitively, in that order. The
// reference goes on to search a record's aliases, nickname, title, comment
// and telephone fields as well; linchange's directory has none of those, so
// the three values a record does carry are all there are to search.
//
// The reference converts the keyword to its punycode form first, an identity
// transformation for the local part of an address; linchange matches the
// keyword as it arrived.
func resolveNode(rec *record, needle string) bool {
	n := strings.ToLower(needle)
	if strings.Contains(lowerName(rec), n) {
		return true
	}
	if strings.EqualFold(userDN(rec), needle) {
		return true
	}
	return strings.Contains(strings.ToLower(rec.address), n)
}

// resolveName resolves one DnToMid keyword: a distinguished name or an
// address, compared case-insensitively after any "prefix:" that the client
// may have wrapped it in is dropped. The reference resolves only the
// hex-minid form of its own distinguished names; linchange's names carry
// the display name, so the name itself is what is matched, with the address
// accepted as well because that is what a client that does not speak X500
// hands over.
func resolveName(recs []record, keyword string) (*record, bool) {
	if i := strings.IndexByte(keyword, ':'); i >= 0 {
		keyword = keyword[i+1:]
	}
	if keyword == "" {
		return nil, false
	}
	for i := range recs {
		if strings.EqualFold(recs[i].address, keyword) || strings.EqualFold(userDN(&recs[i]), keyword) {
			return &recs[i], true
		}
	}
	return nil, false
}

// opBind validates the connection and hands back the address book server
// GUID (MS-OXNSPI 3.1.4.1.1). The checks are the reference's, in its order:
// an anonymous login is refused before the code page is looked at, the code
// page before the login name. Every failure still writes a full response —
// with a zeroed GUID, which is how a client sees that no session was
// created.
func (s *Session) opBind(_ *Backend, principal string, _ []record, r *reader) ([]byte, error) {
	flags, err := r.u32()
	if err != nil {
		return nil, err
	}
	st, err := readOptStat(r)
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	switch {
	case flags&fAnonymousLogin != 0:
		return bindResp(ecFailOneProvider, "")
	case st.codepage == codePageUTF:
		return bindResp(ecNotSupported, "")
	case !acceptableCPID(st.codepage):
		return bindResp(ecUnknownCPID, "")
	case !strings.Contains(principal, "@"):
		// The reference splits the login at '@' and looks the domain
		// part up in its directories; linchange has exactly one, so
		// only the shape of the address is checked, and a caller whose
		// authentication produced no address at all has no domain.
		return bindResp(ecLoginFailure, "")
	}
	return bindResp(ecSuccess, ServerGUID())
}

func bindResp(result uint32, guid string) ([]byte, error) {
	w := newWriter()
	w.u32(0)
	w.u32(result)
	if guid == "" {
		w.bytes(make([]byte, 16))
	} else {
		w.guid(guid)
	}
	w.u32(0)
	return w.buf, nil
}

// opUnbind drops the session (MS-OXNSPI 3.1.4.1.2). Its request carries no
// STAT — only a reserved dword and the auxiliary buffer — and its result is
// the one MAPI status that is not a success code: MAPI_E_UNBINDSUCCESS
// tells the client the session was dropped deliberately.
func (s *Session) opUnbind(_ *Backend, _ string, _ []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}
	return resp(ecUnbindSuccess, nil)
}

// opCompareMids orders two records (MS-OXNSPI 3.1.4.1.12). The container of
// the STAT decides which list they are looked up in: the flat GAL for a root
// container, the children of a nested one, which must exist and hold
// something. A minid that names no record in that list is an error rather
// than an "unresolved" answer — there is nothing to order.
//
// cmp is the sign of the distance from Mid1 to Mid2 in that order: it is
// positive when Mid1 comes first, which is how the reference reads the
// question (and the opposite of a qsort comparator).
func (s *Session) opCompareMids(_ *Backend, _ string, recs []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil {
		return nil, err // reserved
	}
	st, err := readOptStat(r)
	if err != nil {
		return nil, err
	}
	mid1, err := r.u32()
	if err != nil {
		return nil, err
	}
	mid2, err := r.u32()
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	if st.codepage == codePageUTF {
		return compareResp(ecNotSupported, 0)
	}
	i1, i2 := s.indexOf(recs, st.containerID, mid1), s.indexOf(recs, st.containerID, mid2)
	if i1 < 0 || i2 < 0 {
		return compareResp(ecError, 0)
	}
	cmp := int32(0)
	switch {
	case i1 > i2:
		cmp = -1
	case i1 < i2:
		cmp = 1
	}
	return compareResp(ecSuccess, cmp)
}

// indexOf locates a minid inside one container's rows, resolving the
// container first: a container that names nothing reports -1 without
// looking at the minid at all.
func (s *Session) indexOf(recs []record, containerID, mid uint32) int {
	rows, ok := s.containerRows(recs, containerID)
	if !ok {
		return -1
	}
	for i := range rows {
		if rows[i].mid == mid {
			return i
		}
	}
	return -1
}

// compareResp writes NSPICompareMids' response, whose result and comparison
// are in the opposite order to every other operation: Status, cmp, Result,
// trailing zero.
func compareResp(result uint32, cmp int32) ([]byte, error) {
	w := newWriter()
	w.u32(0)
	w.i32(cmp)
	w.u32(result)
	w.u32(0)
	return w.buf, nil
}

// opDnToMid maps distinguished names onto minids (MS-OXNSPI 3.1.4.1.13). It
// cannot fail for a well-formed request: every name that names no record
// yields the UNRESOLVED minid, so the response always carries its array.
func (s *Session) opDnToMid(_ *Backend, _ string, recs []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil {
		return nil, err // reserved
	}
	names, err := readOptStrings8(r)
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	mids := make([]uint32, 0, len(names))
	for _, name := range names {
		mid := uint32(unresolvedMid)
		if rec, ok := resolveName(recs, name); ok {
			mid = rec.mid
		}
		mids = append(mids, mid)
	}
	return resp(ecSuccess, func(w *writer) error {
		w.u8(0xFF)
		w.tagArray(mids)
		return nil
	})
}

// opGetMatches walks the container STAT names and picks out the records a
// filter matches (MS-OXNSPI 3.1.4.1.10).
//
// Two of STAT's container values are not containers at all: they ask for the
// members or the delegates of the record the cursor is on. Linchange's
// directory holds neither a membership list nor a delegate list, so the first
// answers not-found and the second an empty set — but only after the record
// has been found, so a stale cursor is an invalid bookmark whichever of the
// two questions was being asked.
//
// With no filter the request means "does this record carry the property the
// container names?": the record offers itself as the only match when it
// does, and nothing when it does not. A client that has just resolved a name
// uses this to check that a record is still the one for that property.
//
// Whichever branch runs, a success writes the record the request was
// thinking of into STAT's container (point 16 of 3.1.4.1.10): the STAT
// returned here is where the client's next request starts, and it has to
// name the record rather than the container it was asked to look in.
//
// A client that named no columns gets no rows at all — not rows of no cells —
// because the row set exists only when the request asked for one; the mids
// array is what carries the answer either way.
func (s *Session) opGetMatches(_ *Backend, _ string, recs []record, r *reader) ([]byte, error) {
	reserved1, err := r.u32()
	if err != nil {
		return nil, err
	}
	st, err := readOptStat(r)
	if err != nil {
		return nil, err
	}
	if _, err := readOptProptags(r); err != nil {
		return nil, err // an input mid list: read, then ignored
	}
	if _, err := r.u32(); err != nil {
		return nil, err // reserved2
	}
	filter, err := readOptRestriction(r)
	if err != nil {
		return nil, err
	}
	named, err := readOptPropName(r)
	if err != nil {
		return nil, err
	}
	rowCount, err := r.u32()
	if err != nil {
		return nil, err
	}
	respCols, err := readOptProptags(r)
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	fail := func(result uint32) ([]byte, error) {
		return respStat(result, st, func(w *writer) error {
			errTail(w)
			return nil
		})
	}
	success := func(mids []uint32, rows []row) ([]byte, error) {
		out := st
		out.containerID = st.curRec
		return respStat(ecSuccess, out, func(w *writer) error {
			w.u8(0xFF)
			w.tagArray(mids)
			w.u8(0xFF)
			return w.colrow(respCols, rows)
		})
	}

	switch {
	case st.codepage == codePageUTF:
		return fail(ecNotSupported)
	case !sortTypeOK(st.sortType):
		return fail(ecNotSupported)
	case reserved1 != 0 || named:
		// A property name is a second, finer filter that nothing here
		// evaluates, and the reserved field must stay zero; either in
		// a request means the server cannot honour it.
		return fail(ecNotSupported)
	}
	if respCols != nil && len(respCols) > 100 {
		return fail(ecTableTooBig)
	}

	mids := []uint32{}
	rows := []row{}
	fetch := fetchColumns(respCols)

	switch {
	case st.containerID == prEmABMember:
		if _, ok := s.lookup(recs, st.curRec); !ok {
			return fail(ecInvalidBookmark)
		}
		// No distribution lists exist, so no record has members to
		// report — and asking one for its members that is not a list
		// at all is a not-found rather than an error.
		return fail(ecNotFound)
	case st.containerID == prEmABPublicDelegates:
		if _, ok := s.lookup(recs, st.curRec); !ok {
			return fail(ecInvalidBookmark)
		}
		// No delegates are stored, so the answer is an empty set and
		// a success — ModLinkAtt reports itself unsupported for the
		// same reason, and this is where a client sees the result.
		return success(mids, rows)
	case filter == nil:
		if rec, ok := s.lookup(recs, st.curRec); ok && rowCount > 0 {
			if _, has := s.recordProp(rec, st.containerID); has {
				mids = append(mids, st.curRec)
			}
		}
	default:
		holds, ok := s.containerRows(recs, st.containerID)
		if !ok {
			return fail(ecInvalidBookmark)
		}
		start, _ := s.statPosition(&st, holds, st.containerID)
		for i := start; i < uint32(len(holds)) && uint32(len(mids)) < rowCount; i++ {
			if !s.evalRestriction(filter, &holds[i]) {
				continue
			}
			mids = append(mids, holds[i].mid)
		}
	}

	if respCols != nil {
		// Rows are fetched for ephemeral entry ids, the form a client
		// browsing the address book asks for.
		for _, mid := range mids {
			rec, ok := s.lookup(recs, mid)
			if !ok {
				rows = append(rows, errorRowFor(fetch, respCols))
				continue
			}
			rows = append(rows, rowFor(s.props(rec, true), fetch, respCols))
		}
	}
	return success(mids, rows)
}

// opGetPropList lists the properties a record has values for
// (MS-OXNSPI 3.1.4.1.6). The list is the canonical set of the record,
// flavoured for the request's code page and already trimmed to the tags the
// record actually carries. Unlike the other operations this one does not
// reject code page 1200: it is what tells a client whether to ask for
// Unicode or 8-bit twins next.
func (s *Session) opGetPropList(_ *Backend, _ string, recs []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil {
		return nil, err // flags
	}
	mid, err := r.u32()
	if err != nil {
		return nil, err
	}
	codepage, err := r.u32()
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	if mid == 0 {
		return resp(ecInvalidObject, nil)
	}
	rec, ok := s.lookup(recs, mid)
	if !ok {
		return resp(ecInvalidObject, nil)
	}
	tags := canonicalTags(s.props(rec, false), codepage == codePageUTF)
	return resp(ecSuccess, func(w *writer) error {
		w.u8(0xFF)
		w.tagArray(tags)
		return nil
	})
}

// opGetProps fetches properties of the record the STAT points at
// (MS-OXNSPI 3.1.4.1.7).
//
// The record is the STAT's current record if it names one, or — when the
// current record is an anchor rather than a record — whatever the cursor
// lands on in the STAT's container. A container that names nothing is an
// invalid bookmark; a current record that names nothing yields a row of
// error cells and warn-with-errors instead, because a stale minid is a
// condition the client can recover from by refreshing.
//
// A request with no tags of its own is answered from the record's canonical
// list and has the properties the record lacks filtered out, so the list
// stays a list of what the record has rather than of what it is missing.
// An explicit request keeps those cells: their ecNotFound is the answer.
func (s *Session) opGetProps(_ *Backend, _ string, recs []record, r *reader) ([]byte, error) {
	flags, err := r.u32()
	if err != nil {
		return nil, err
	}
	st, err := readOptStat(r)
	if err != nil {
		return nil, err
	}
	tags, err := readOptProptags(r)
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	unicode := st.codepage == codePageUTF
	if unicode {
		// A Unicode code page may not be answered with 8-bit string
		// properties; the client asks for the Unicode twins instead.
		for _, t := range tags {
			if propType(t) == ptString8 {
				return resp(ecNotSupported, nil)
			}
		}
	}

	var rec *record
	if st.curRec <= midBase {
		rows, ok := s.containerRows(recs, st.containerID)
		if !ok {
			return resp(ecInvalidBookmark, nil)
		}
		if start, _ := s.statPosition(&st, rows, st.containerID); start < uint32(len(rows)) {
			rec = &rows[start]
		}
	} else {
		r2, ok := s.lookup(recs, st.curRec)
		if ok && !flatContainer(st.containerID) {
			// The reference only consults the container when the record
			// itself resolved: a record in a container that has since
			// disappeared is an invalid bookmark, not a missing record.
			if _, cok := s.containerRows(recs, st.containerID); !cok {
				return resp(ecInvalidBookmark, nil)
			}
		}
		if ok {
			rec = r2
		}
	}

	explicit := tags != nil
	cols := tags
	if !explicit && rec != nil {
		cols = canonicalTags(s.props(rec, flags&fEphID != 0), unicode)
	}
	if len(cols) > 100 {
		return resp(ecTableTooBig, nil)
	}

	// The response's code page is the request's on success and on
	// warn-with-errors, and zero on every failure.
	codepage := st.codepage
	if rec == nil {
		return resp(ecWarnWithErrors, func(w *writer) error {
			w.u32(codepage)
			w.u8(0xFF)
			return w.tpropvalRow(errorCells(cols))
		})
	}

	cells := numberedRow(cols, s.props(rec, flags&fEphID != 0))
	result := uint32(ecSuccess)
	if explicit {
		if hasError(cells) {
			result = ecWarnWithErrors
		}
	} else {
		cells = keepPresent(cells)
	}
	return resp(result, func(w *writer) error {
		w.u32(codepage)
		w.u8(0xFF)
		return w.tpropvalRow(cells)
	})
}

// opGetSpecialTable describes the address book's containers
// (MS-OXNSPI 3.1.4.1.3). Linchange's directory has one: the GAL itself,
// returned as a single container row whose flags say it holds recipients
// and cannot be modified and whose display name is deliberately absent —
// a client names it from its own position in the tree.
//
// The creation-templates flag returns success with no rows at all and
// without advancing the version counter; the reference does the same, and
// with it returns a zero code page because it never reaches the point of
// assigning one.
func (s *Session) opGetSpecialTable(_ *Backend, _ string, _ []record, r *reader) ([]byte, error) {
	flags, err := r.u32()
	if err != nil {
		return nil, err
	}
	st, err := readOptStat(r)
	if err != nil {
		return nil, err
	}
	version, err := readOptU32(r)
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	if flags&nspiAddressCreationTpls != 0 {
		return resp(ecSuccess, func(w *writer) error {
			w.u32(0)
			if version == nil {
				w.u8(0)
			} else {
				w.u8(0xFF)
				w.u32(*version)
			}
			w.u8(0) // no rows
			return nil
		})
	}

	unicode := flags&nspiUnicodeStrings != 0
	if !unicode && st.codepage == codePageUTF {
		// A failure writes an absent version and no rows, whatever the
		// client asked for.
		return resp(ecNotSupported, func(w *writer) error {
			w.u32(0)
			w.u8(0)
			w.u8(0)
			return nil
		})
	}

	// The version counter advances on every real call, exactly as the
	// reference's does (it increments the value the client echoed back on
	// each of its own calls too), but it is only written when the client
	// asked for it — otherwise the response carries an absent version.
	versionOut := s.bumpVersion()
	row := []prop{
		{tag: prEntryID, value: permanentEID(dtContainer, "/")},
		{tag: prContainerFlags, value: int64(abRecipients | abUnmodifiable)},
		{tag: prDepth, value: int64(0)},
		{tag: prEmABContainerID, value: int64(0)},
		{tag: flavorTag(prDisplayName, unicode), value: nil},
		{tag: prEmABIsMaster, value: false},
	}
	return resp(ecSuccess, func(w *writer) error {
		w.u32(st.codepage)
		if version == nil {
			w.u8(0)
		} else {
			w.u8(0xFF)
			w.u32(versionOut)
		}
		w.u8(0xFF)
		w.u32(1)
		return w.tpropvalLA(row)
	})
}

// opGetTemplateInfo fetches an address book form template
// (MS-OXNSPI 3.1.4.1.18). The form templates live in a server-side archive
// of .abkt files that linchange does not ship, so the request passes the
// reference's own gates — the flags must select a template and nothing
// else, the code page must be a usable one, no explicit DN may be given —
// and then reports the template as unknown, exactly as the reference does
// when its archive is absent.
//
// The response's code page is zero on every failure, including this one.
func (s *Session) opGetTemplateInfo(_ *Backend, _ string, _ []record, r *reader) ([]byte, error) {
	flags, err := r.u32()
	if err != nil {
		return nil, err
	}
	if _, err := r.u32(); err != nil { // type
		return nil, err
	}
	pres, err := r.u8()
	if err != nil {
		return nil, err
	}
	dn := ""
	if pres != 0 {
		if dn, err = r.zstr8(); err != nil {
			return nil, err
		}
	}
	codepage, err := r.u32()
	if err != nil {
		return nil, err
	}
	if _, err := r.u32(); err != nil { // locale id
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	result := uint32(ecUnknownLCID)
	switch {
	case flags&(tiTemplate|tiScript) != tiTemplate:
		result = ecNotSupported
	case !acceptableCPID(codepage):
		result = ecUnknownCPID
	case dn != "":
		result = ecUnknownLCID
	}
	return resp(result, func(w *writer) error {
		w.u32(0)
		w.u8(0)
		return nil
	})
}

// opModLinkAtt adds or removes a value of a linked addressing property
// (MS-OXNSPI 3.1.4.1.15). The reference accepts exactly one property this
// way — PR_EMS_AB_PUBLIC_DELEGATES — and hands it to the store's delegate
// list. linchange's directory is read-only and keeps no delegate list, so
// after the reference's checks (a real record, and the caller's own record:
// nobody edits someone else's delegates) the operation reports itself
// unsupported rather than silently doing nothing.
func (s *Session) opModLinkAtt(_ *Backend, principal string, recs []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil { // flags
		return nil, err
	}
	proptag, err := r.u32()
	if err != nil {
		return nil, err
	}
	mid, err := r.u32()
	if err != nil {
		return nil, err
	}
	if _, err := readOptBinaries(r); err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	switch {
	case mid == 0:
		return resp(ecInvalidObject, nil)
	case proptag != prEmABPublicDelegates:
		return resp(ecNotSupported, nil)
	}
	rec, ok := s.lookup(recs, mid)
	if !ok {
		return resp(ecInvalidObject, nil)
	}
	if !strings.EqualFold(rec.address, principal) {
		return resp(ecAccessDenied, nil)
	}
	return resp(ecNotSupported, nil)
}

// opModProps replaces properties of the record the STAT points at
// (MS-OXNSPI 3.1.4.1.14). The whole body — STAT, the tags and their values —
// is parsed, so a client's request is checked end to end, and the operation
// then reports itself unsupported: the address book is a projection of the
// user directory and holds nothing a client could write back to it.
func (s *Session) opModProps(_ *Backend, _ string, _ []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil { // reserved
		return nil, err
	}
	if _, err := readOptStat(r); err != nil {
		return nil, err
	}
	if _, err := readOptProptags(r); err != nil {
		return nil, err
	}
	if _, err := readOptValues(r); err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}
	return resp(ecNotSupported, nil)
}

// opQueryColumns lists the columns a client may ask for
// (MS-OXNSPI 3.1.4.1.5). The text members follow the request's Unicode flag
// and the typed members never do, which is the reference's split.
func (s *Session) opQueryColumns(_ *Backend, _ string, _ []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil { // reserved
		return nil, err
	}
	flags, err := r.u32()
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}
	unicode := flags&nspiUnicodeProptypes != 0
	return resp(ecSuccess, func(w *writer) error {
		w.u8(0xFF)
		w.tagArray(queryColumnTags(unicode))
		return nil
	})
}

// opQueryRows walks the container the STAT names and returns its rows
// (MS-OXNSPI 3.1.4.1.8).
//
// An explicit table — a client handing back minids of its own — is walked
// in the order given, with an error row for any minid that no longer names a
// record, and the STAT is passed through untouched: it describes a table
// position and this request did not come from one. Otherwise the rows come
// from the STAT's container, starting at the cursor and moving by STAT's
// delta, and the STAT is advanced to just past the last row returned.
//
// The response's column list is the client's, so a client that named no
// columns gets rows with no cells in them — but still with the flag the
// fetch set, because the fetch used the default tags and their errors are
// the client's to see.
func (s *Session) opQueryRows(_ *Backend, _ string, recs []record, r *reader) ([]byte, error) {
	flags, err := r.u32()
	if err != nil {
		return nil, err
	}
	st, err := readOptStat(r)
	if err != nil {
		return nil, err
	}
	explicitTable, err := r.proptags(4)
	if err != nil {
		return nil, err
	}
	count, err := r.u32()
	if err != nil {
		return nil, err
	}
	respCols, err := readOptProptags(r)
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	fail := func(result uint32) ([]byte, error) {
		return respStat(result, st, func(w *writer) error {
			w.u8(0)
			return nil
		})
	}
	if st.codepage == codePageUTF {
		return fail(ecNotSupported)
	}
	if count == 0 {
		return fail(ecInvalidParam)
	}
	fetch := fetchColumns(respCols)
	if len(fetch) > 100 {
		return fail(ecTableTooBig)
	}
	eph := flags&fEphID != 0

	rows := []row{}
	out := st
	if len(explicitTable) > 0 {
		for _, mid := range explicitTable {
			rec, ok := s.lookup(recs, mid)
			if !ok {
				rows = append(rows, errorRowFor(fetch, respCols))
				continue
			}
			rows = append(rows, rowFor(s.props(rec, eph), fetch, respCols))
		}
		return respStat(ecSuccess, out, func(w *writer) error {
			w.u8(0xFF)
			return w.colrow(respCols, rows)
		})
	}

	holds, ok := s.containerRows(recs, st.containerID)
	if !ok {
		return fail(ecInvalidBookmark)
	}
	if st.containerID != 0 && len(holds) == 0 {
		// A container with no rows answers with no rows and an untouched
		// STAT: there is no position to advance to.
		return respStat(ecSuccess, out, func(w *writer) error {
			w.u8(0xFF)
			return w.colrow(respCols, rows)
		})
	}
	start, total := s.statPosition(&st, holds, st.containerID)
	if start > total {
		start = total
	}
	// STAT.delta moves the cursor before the rows are read; a delta that
	// would run past either end of the table stops at that end rather than
	// wrapping, which is the one place where the reference's unsigned
	// arithmetic and this implementation's have to differ — its wrap reads
	// as a huge offset and returns nothing anyway.
	switch {
	case st.delta >= 0:
		if uint64(start)+uint64(st.delta) > uint64(total) {
			start = total
		} else {
			start += uint32(st.delta)
		}
	default:
		d := uint32(-int64(st.delta))
		if d > st.numPos || d > start {
			start = 0
		} else {
			start -= d
		}
	}
	if start >= total {
		return respStat(ecSuccess, out, func(w *writer) error {
			w.u8(0xFF)
			return w.colrow(respCols, rows)
		})
	}
	end := total - start
	if count < end {
		end = count
	}
	for i := start; i < start+end; i++ {
		rows = append(rows, rowFor(s.props(&holds[i], eph), fetch, respCols))
	}

	out.delta = 0
	out.numPos = start + end
	out.totalRec = total
	if start+end >= total {
		out.curRec = endOfTable
	} else {
		out.curRec = holds[start+end].mid
		if out.curRec == 0 {
			out.curRec = endOfTable
			out.numPos = total
		}
	}
	return respStat(ecSuccess, out, func(w *writer) error {
		w.u8(0xFF)
		return w.colrow(respCols, rows)
	})
}

// opResolveNames resolves names against the container the STAT names
// (MS-OXNSPI 3.1.4.1.17). Each name becomes one entry in the mids array:
// RESOLVED, AMBIGUOUS or UNRESOLVED — and only a resolved name contributes a
// row, so the row count is the number of names that resolved.
//
// The mids are the three reserved values rather than real minids: a
// resolution result is a status, and the caller reads the record from the
// row that came with it.
//
// Divergence from the reference: a name that matches nothing is reported
// UNRESOLVED here, as MS-OXNSPI 3.1.4.1.17 requires. The reference
// reports AMBIGUOUS for zero matches as well as for several, because it
// folds "not found" into its ambiguity flag; a client acting on the spec
// would retry a resolution it cannot succeed at, so the spec is followed.
func (s *Session) opResolveNames(_ *Backend, _ string, recs []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil { // reserved
		return nil, err
	}
	st, err := readOptStat(r)
	if err != nil {
		return nil, err
	}
	respCols, err := readOptProptags(r)
	if err != nil {
		return nil, err
	}
	names, err := readOptStringsW(r)
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	fail := func(result uint32) ([]byte, error) {
		// The code page this operation answers with is fixed at UTF-16
		// whatever happens: it is a property of the response struct, not
		// of the outcome.
		return resp(result, func(w *writer) error {
			w.u32(codePageUTF)
			errTail(w)
			return nil
		})
	}
	if st.codepage == codePageUTF {
		return fail(ecNotSupported)
	}
	fetch := fetchColumns(respCols)
	if len(fetch) > 100 {
		return fail(ecTableTooBig)
	}
	holds, ok := s.containerRows(recs, st.containerID)
	if !ok {
		return fail(ecInvalidBookmark)
	}

	mids := make([]uint32, 0, len(names))
	rows := make([]row, 0, len(names))
	for _, name := range names {
		if name == "" {
			mids = append(mids, unresolvedMid)
			continue
		}
		needle := name
		if i := strings.IndexByte(needle, ':'); i >= 0 {
			// "=SMTP:user@host": the client echoes the address book's
			// own prefix back, the keyword is what follows it.
			needle = needle[i+1:]
		}
		var found *record
		matches := 0
		for i := range holds {
			if !resolveNode(&holds[i], needle) {
				continue
			}
			matches++
			if matches == 1 {
				found = &holds[i]
			}
		}
		switch matches {
		case 0:
			mids = append(mids, unresolvedMid)
		case 1:
			mids = append(mids, resolvedMid)
			// The resolver asks for permanent entry ids.
			rows = append(rows, rowFor(s.props(found, false), fetch, respCols))
		default:
			mids = append(mids, ambiguousMid)
		}
	}
	return resp(ecSuccess, func(w *writer) error {
		w.u32(codePageUTF)
		w.u8(0xFF)
		w.tagArray(mids)
		w.u8(0xFF)
		return w.colrow(respCols, rows)
	})
}

// opResortRestriction re-orders a set of minids by display name
// (MS-OXNSPI 3.1.4.1.11). Minids that no longer name a record are dropped,
// and the STAT's current record is only kept if it survived the input: a
// cursor onto a record that is gone is reset to the start of the table.
func (s *Session) opResortRestriction(_ *Backend, _ string, recs []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil { // reserved
		return nil, err
	}
	st, err := readOptStat(r)
	if err != nil {
		return nil, err
	}
	inMids, err := readOptProptags(r)
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	fail := func(result uint32) ([]byte, error) {
		return respStat(result, st, func(w *writer) error {
			w.u8(0)
			return nil
		})
	}
	if st.codepage == codePageUTF {
		return fail(ecNotSupported)
	}

	type entry struct {
		mid  uint32
		name string
	}
	kept := make([]entry, 0, len(inMids))
	found := false
	for _, mid := range inMids {
		rec, ok := s.lookup(recs, mid)
		if !ok {
			continue
		}
		if st.curRec == mid {
			found = true
		}
		kept = append(kept, entry{mid: mid, name: lowerName(rec)})
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].name < kept[j].name })

	out := st
	out.totalRec = uint32(len(kept))
	if !found {
		out.curRec = beginningOfTable
		out.numPos = 0
	}
	mids := make([]uint32, 0, len(kept))
	for _, e := range kept {
		mids = append(mids, e.mid)
	}
	return respStat(ecSuccess, out, func(w *writer) error {
		w.u8(0xFF)
		w.tagArray(mids)
		return nil
	})
}

// opSeekEntries finds where a name sits in the order the STAT's sort type
// names (MS-OXNSPI 3.1.4.1.9).
//
// The target's property tag has to agree with the sort type: a display-name
// sort seeks on PR_DISPLAY_NAME, a phonetic sort on
// PR_EMS_AB_PHONETIC_DISPLAY_NAME, and anything else is an error — the
// address book is held in one order only. A phonetic sort therefore always
// fails here, since no phonetic names are stored to sort by or to seek on.
//
// An explicit table is walked as given and everything from the first name
// that is not before the target on to the end becomes the result, so the
// STAT describes the matched run. Without one the container's rows are
// searched from the start for the first name that is not before the target,
// and one row — the position itself — is returned.
func (s *Session) opSeekEntries(_ *Backend, _ string, recs []record, r *reader) ([]byte, error) {
	reserved, err := r.u32()
	if err != nil {
		return nil, err
	}
	st, err := readOptStat(r)
	if err != nil {
		return nil, err
	}
	target, err := readOptProp(r)
	if err != nil {
		return nil, err
	}
	explicitTable, err := readOptProptags(r)
	if err != nil {
		return nil, err
	}
	respCols, err := readOptProptags(r)
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	fail := func(result uint32) ([]byte, error) {
		return respStat(result, st, func(w *writer) error {
			w.u8(0)
			return nil
		})
	}
	if reserved != 0 || st.codepage == codePageUTF {
		return fail(ecNotSupported)
	}
	switch st.sortType {
	case sortTypeDisplayName:
		if target.tag != prDisplayName && target.tag != prDisplayNameA {
			return fail(ecError)
		}
	case sortTypePhoneticName:
		if target.tag != prEmABPhoneticDisplayName &&
			target.tag != changeType(prEmABPhoneticDisplayName, ptString8) {
			return fail(ecError)
		}
	default:
		return fail(ecError)
	}

	fetch := fetchColumns(respCols)
	if len(fetch) > 100 {
		return fail(ecTableTooBig)
	}
	// SeekEntries always asks for ephemeral entry ids.
	const ephemeral = true
	want := lowerNameOf(target.value)

	if len(explicitTable) > 0 {
		rows := []row{}
		firstMid := uint32(0)
		firstPos := uint32(0)
		for i, mid := range explicitTable {
			rec, ok := s.lookup(recs, mid)
			if !ok {
				continue
			}
			if strings.ToLower(rec.name) < want {
				continue
			}
			if firstMid == 0 {
				firstMid = mid
				firstPos = uint32(i)
			}
			rows = append(rows, rowFor(s.props(rec, ephemeral), fetch, respCols))
		}
		if firstMid == 0 {
			return fail(ecNotFound)
		}
		out := st
		out.curRec = firstMid
		out.numPos = firstPos
		out.totalRec = uint32(len(rows))
		return respStat(ecSuccess, out, func(w *writer) error {
			w.u8(0xFF)
			return w.colrow(respCols, rows)
		})
	}

	holds, ok := s.containerRows(recs, st.containerID)
	if !ok {
		return fail(ecInvalidBookmark)
	}
	_, total := s.statPosition(&st, holds, st.containerID)
	if total == 0 {
		return fail(ecNotFound)
	}
	// The search always starts at the container's first row: an absolute
	// cursor is where the result is reported, not where the search begins.
	idx := sort.Search(int(total), func(i int) bool {
		return strings.ToLower(holds[i].name) >= want
	})
	if idx >= int(total) {
		return fail(ecNotFound)
	}
	out := st
	out.curRec = holds[idx].mid
	out.numPos = uint32(idx)
	out.totalRec = total
	return respStat(ecSuccess, out, func(w *writer) error {
		w.u8(0xFF)
		return w.colrow(respCols, []row{rowFor(s.props(&holds[idx], ephemeral), fetch, respCols)})
	})
}

// lowerNameOf is lowerName for a value that may not be a string at all (an
// absent or oddly typed seek target): anything but a string seeks as an
// empty name, which matches the first row, rather than panicking.
func lowerNameOf(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.ToLower(s)
}

// opUpdateStat advances STAT's cursor by a delta
// (MS-OXNSPI 3.1.4.1.4). The delta section of the response appears exactly
// when the client asked for one, failure included, with a zero delta — so
// the shape of the response matches what the request asked for either way.
func (s *Session) opUpdateStat(_ *Backend, _ string, recs []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil { // reserved
		return nil, err
	}
	st, err := readOptStat(r)
	if err != nil {
		return nil, err
	}
	deltaRequested, err := r.u8()
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	fail := func(result uint32) ([]byte, error) {
		return respStat(result, st, func(w *writer) error {
			if deltaRequested == 0 {
				w.u8(0)
				return nil
			}
			w.u8(0xFF)
			w.i32(0)
			return nil
		})
	}
	if st.codepage == codePageUTF {
		return fail(ecNotSupported)
	}
	holds, ok := s.containerRows(recs, st.containerID)
	if !ok {
		return fail(ecInvalidBookmark)
	}
	start, total := s.statPosition(&st, holds, st.containerID)
	if start > total {
		start = total
	}

	row := start
	switch {
	case st.delta < 0 && uint32(-int64(st.delta)) >= row:
		row = 0
	case st.delta < 0:
		row -= uint32(-int64(st.delta))
	case uint64(row)+uint64(st.delta) > uint64(total):
		row = total
	default:
		row += uint32(st.delta)
	}

	out := st
	if row >= total {
		row = total
		out.curRec = endOfTable
	} else if holds[row].mid == 0 {
		row = total
		out.curRec = endOfTable
	} else {
		out.curRec = holds[row].mid
	}
	// The delta the response reports is the distance actually travelled,
	// from where the cursor was to where it now is.
	moved := int32(row) - int32(start)
	out.delta = 0
	out.numPos = row
	out.totalRec = total
	return respStat(ecSuccess, out, func(w *writer) error {
		if deltaRequested == 0 {
			w.u8(0)
			return nil
		}
		w.u8(0xFF)
		w.i32(moved)
		return nil
	})
}

// opGetAddressBookURL reports where this address book is served
// (MS-OXCMAPIHTTP 2.2.5.19). The URL names the caller's own address book, so
// the authenticated principal goes into it. The reference packs a numeric
// user id into the MailboxId as well; linchange's ids are not numbers and
// the transport ignores MailboxId entirely — the caller is established by
// authentication — so the principal alone identifies the mailbox.
//
// The URL is always written, empty if the operation failed.
func (s *Session) opGetAddressBookURL(_ *Backend, principal string, _ []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil { // flags
		return nil, err
	}
	if _, err := r.zwstr(); err != nil { // user DN
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}
	return resp(ecSuccess, func(w *writer) error {
		w.wstr(s.addressBookURL(principal))
		return nil
	})
}

// opGetMailboxURL reports where this server's mailbox endpoint is
// (MS-OXCMAPIHTTP 2.2.5.18).
//
// The DN the request carries is walked back over its last two "cn="
// components: the trailing one names the recipient, the one before it the
// recipient container, and only a DN with both is a mailbox DN — anything
// else falls back to the address book's own URL, which is where a caller
// that is not naming a mailbox should go.
//
// The reference takes the container's CN for its MailboxId; the mailbox's
// own CN is taken here instead, since that is what the value names. It makes
// no difference to this server — MailboxId is ignored, authentication
// governs the session — but it does make the URL say what it means.
func (s *Session) opGetMailboxURL(_ *Backend, principal string, _ []record, r *reader) ([]byte, error) {
	if _, err := r.u32(); err != nil { // flags
		return nil, err
	}
	dn, err := r.zwstr()
	if err != nil {
		return nil, err
	}
	if err := skipAux(r); err != nil {
		return nil, err
	}

	url := s.addressBookURL(principal)
	if cn, rest, ok := splitLastCN(dn); ok {
		if _, _, ok := splitLastCN(rest); ok {
			url = mailboxURL(s.hostname(), cn)
		}
	}
	return resp(ecSuccess, func(w *writer) error {
		w.wstr(url)
		return nil
	})
}

// splitLastCN splits a distinguished name at its final "/cn=", returning what
// follows it and the name with that component removed.
func splitLastCN(dn string) (cn string, rest string, ok bool) {
	i := strings.LastIndex(dn, "/cn=")
	if i < 0 {
		return "", dn, false
	}
	return dn[i+len("/cn="):], dn[:i], true
}

func mailboxURL(host, cn string) string {
	return "https://" + host + "/mapi/emsmdb/?MailboxId=" + cn
}

func (s *Session) addressBookURL(principal string) string {
	return "https://" + s.hostname() + "/mapi/nspi/?MailboxId=" + principal
}

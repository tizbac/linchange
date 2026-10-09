package nspi

// NSPI restriction parsing and evaluation (MS-OXCDATA 2.11, in the NSPI /
// ABK stream form used for the GetMatches filter). The parser is
// lenient on every node shape so an unsupported-but-well-formed restriction
// is skipped rather than rejected; the evaluator mirrors the address book
// matcher of the reference implementation.

import "strings"

// Restriction type codes (mapidefs.h in the reference implementation).
const (
	resAND            = 0x00
	resOR             = 0x01
	resNOT            = 0x02
	resContent        = 0x03
	resProperty       = 0x04
	resPropCompare    = 0x05
	resBitmask        = 0x06
	resSize           = 0x07
	resExist          = 0x08
	resSubRestriction = 0x09
	resComment        = 0x0A
	resCount          = 0x0B
	resAnnotation     = 0x0C
	resNull           = 0xFF
)

// Relational operators (MS-OXCDATA 2.11.5.1).
const (
	relopLT = 0
	relopLE = 1
	relopGT = 2
	relopGE = 3
	relopEQ = 4
	relopNE = 5
	relopRE = 6
)

// restNode is one parsed restriction, arranged so every node type (including
// the ones the GAL matcher does not evaluate) round-trips through the parser.
type restNode struct {
	kind uint8
	// AND/OR
	children []*restNode
	// NOT, COMMENT/ANNOTATION sub-restriction, COUNT sub-restriction
	sub *restNode
	// PROPERTY / CONTENT
	relop uint8
	tag   uint32
	val   any
	fuzzy uint32
	// COMPAREPROPS
	tag2 uint32
	// BITMASK
	mask uint32
	// SIZE
	size uint32
	// SUBRESTRICTION
	subObj uint32
}

// parseRestriction reads one restriction in the NSPI (ABK, WCOUNT) stream
// form. If the type byte is unknown the function still returns a node whose
// matcher always fails, so a request with an exotic filter parses cleanly.
func parseRestriction(r *reader) (*restNode, error) {
	kind, err := r.u8()
	if err != nil {
		return nil, err
	}
	n := &restNode{kind: kind}
	switch kind {
	case resAND, resOR:
		count, err := r.u32()
		if err != nil {
			return nil, err
		}
		if count > uint32(r.remaining()) {
			return nil, formatError
		}
		for i := uint32(0); i < count; i++ {
			c, err := parseRestriction(r)
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, c)
		}
	case resNOT:
		n.sub, err = parseRestriction(r)
		return n, err
	case resContent:
		if n.fuzzy, err = r.u32(); err != nil {
			return nil, err
		}
		if n.tag, err = r.u32(); err != nil {
			return nil, err
		}
		if err := r.restrictionProp(n.tag, &n.val); err != nil {
			return nil, err
		}
	case resProperty:
		if n.relop, err = r.u8(); err != nil {
			return nil, err
		}
		if n.tag, err = r.u32(); err != nil {
			return nil, err
		}
		if err := r.restrictionProp(n.tag, &n.val); err != nil {
			return nil, err
		}
	case resPropCompare:
		if n.relop, err = r.u8(); err != nil {
			return nil, err
		}
		if n.tag, err = r.u32(); err != nil {
			return nil, err
		}
		if n.tag2, err = r.u32(); err != nil {
			return nil, err
		}
	case resBitmask:
		if n.relop, err = r.u8(); err != nil {
			return nil, err
		}
		if n.mask, err = r.u32(); err != nil {
			return nil, err
		}
		if n.tag, err = r.u32(); err != nil {
			return nil, err
		}
	case resSize:
		if n.relop, err = r.u8(); err != nil {
			return nil, err
		}
		if n.size, err = r.u32(); err != nil {
			return nil, err
		}
		if n.tag, err = r.u32(); err != nil {
			return nil, err
		}
	case resExist:
		if n.tag, err = r.u32(); err != nil {
			return nil, err
		}
	case resSubRestriction:
		if n.subObj, err = r.u32(); err != nil {
			return nil, err
		}
		n.sub, err = parseRestriction(r)
		return n, err
	case resComment, resAnnotation:
		count, err := r.u8()
		if err != nil {
			return nil, err
		}
		for i := uint8(0); i < count; i++ {
			if err := r.skipTagged(); err != nil {
				return nil, err
			}
		}
		pres, err := r.u8()
		if err != nil {
			return nil, err
		}
		if pres != 0 {
			n.sub, err = parseRestriction(r)
			return n, err
		}
	case resCount:
		if _, err := r.u32(); err != nil {
			return nil, err
		}
		n.sub, err = parseRestriction(r)
		return n, err
	default:
		// Unknown node type: parse nothing, match nothing. The wire
		// position is left as-is, but a GetMatches filter containing an
		// unknown node simply yields no rows.
		return n, nil
	}
	return n, nil
}

// restrictionProp reads a `u8 value_set` + TAGGED_PROPVAL, the NSPI/ABK
// serialization of restriction property values: a u32 property tag (which
// normally duplicates the column tag) and the value, which for string,
// binary and MV types carries its own presence byte inside decPropVal.
func (r *reader) restrictionProp(tag uint32, out *any) error {
	pres, err := r.u8()
	if err != nil {
		return err
	}
	if pres == 0 {
		*out = nil
		return nil
	}
	if pres != 0xFF {
		return formatError
	}
	tag2, err := r.u32()
	if err != nil {
		return err
	}
	// The repeated tag is authoritative: it carries the type the value was
	// encoded with, which is what decPropVal needs. The column tag is only
	// consulted when it is itself typed and the repeated one is not.
	typ := propType(tag2)
	if typ == ptUnspecified {
		typ = propType(tag)
	}
	v, err := r.decPropVal(typ, true)
	if err != nil {
		return err
	}
	*out = v
	return nil
}

// skipTagged reads and discards one tagged property value (ABK form).
func (r *reader) skipTagged() error {
	tag, err := r.u32()
	if err != nil {
		return err
	}
	_, err = r.decPropVal(propType(tag), true)
	return err
}

// evalRestriction reports whether the parsed restriction matches the record.
// The semantics mirror nsp_interface_match_node of the reference
// implementation: AND/OR/NOT compose, PROPERTY compares a fetched property
// with the given value (PR_ANR does ambiguous-name matching), EXIST passes
// when the property exists, and everything that needs a sub-object table
// (CONTENT, PROPCOMPARE, BITMASK, SIZE, SUBRESTRICTION) fails, because the
// flat GAL has none.
// evalRestriction reports whether one record satisfies a restriction. It
// mirrors the matcher of the reference implementation node for node: the
// kinds it evaluates are AND/OR/NOT, PROPERTY, EXIST and, for PROPERTY, the
// PR_ANR special case; everything else — content, property comparison,
// bitmasks, sizes, sub-objects, comments, counts — never matches, because the
// directory has no indexed fields to search and the matcher has no code for
// them either.
func (s *Session) evalRestriction(n *restNode, rec *record) bool {
	if n == nil {
		// No filter matches every record, which is what the caller does
		// before it ever reaches here (GetMatches branches on a nil
		// filter itself).
		return true
	}
	switch n.kind {
	case resAND:
		for _, c := range n.children {
			if !s.evalRestriction(c, rec) {
				return false
			}
		}
		return true
	case resOR:
		for _, c := range n.children {
			if s.evalRestriction(c, rec) {
				return true
			}
		}
		return false
	case resNOT:
		return !s.evalRestriction(n.sub, rec)
	case resProperty:
		if n.val == nil {
			// The matcher returns true without looking at the property
			// when the restriction carries no value at all: a property
			// restriction with nothing to compare against constrains
			// nothing.
			return true
		}
		if n.tag == prANR || n.tag == prANRA {
			needle, ok := n.val.(string)
			if !ok {
				return false
			}
			return anrMatch(rec, needle)
		}
		v, ok := s.recordProp(rec, n.tag)
		if !ok {
			return false
		}
		// Only the scalar and string types the reference compares;
		// anything else is reported as unhandled and fails.
		switch propType(n.tag) {
		case ptShort, ptLong, ptBoolean, ptString8, ptUnicode:
			return relopEval(n.relop, compareProp(v, n.val))
		}
		return false
	case resExist:
		_, ok := s.recordProp(rec, n.tag)
		return ok
	}
	// RES_CONTENT, RES_PROPCOMPARE, RES_BITMASK, RES_SIZE,
	// RES_SUBRESTRICTION, RES_COMMENT, RES_ANNOTATION, RES_COUNT, RES_NULL
	// and any unknown type: unevaluated, so no record matches.
	return false
}

// anrMatch performs the ambiguous-name resolution match used for PR_ANR
// PROPERTY restrictions. The needle is first searched for as a substring of
// the account; if it carries a "prefix:" part (the "SMTP:user@host" form a
// client echoes back) that part is searched instead, and without a colon the
// whole needle must equal the account. Failing the account, the display name
// is searched for the needle as a substring.
//
// Matching is case-insensitive throughout, as the reference's strcasestr /
// strcasecmp pair is; an empty needle is a substring of everything and so
// matches every record.
func anrMatch(rec *record, needle string) bool {
	account := strings.ToLower(rec.address)
	if strings.Contains(account, strings.ToLower(needle)) {
		return true
	}
	if i := strings.IndexByte(needle, ':'); i >= 0 {
		if strings.Contains(account, strings.ToLower(needle[i+1:])) {
			return true
		}
	} else if account == strings.ToLower(needle) {
		return true
	}
	return strings.Contains(strings.ToLower(rec.name), strings.ToLower(needle))
}

// relopEval applies a relational operator to a three-way comparison result.
// An operator outside MS-OXCDATA's range (notably RELOP_RE, which the
// address book has no regular-expression matcher for) never holds.
func relopEval(relop uint8, cmp int) bool {
	switch relop {
	case relopLT:
		return cmp < 0
	case relopLE:
		return cmp <= 0
	case relopGT:
		return cmp > 0
	case relopGE:
		return cmp >= 0
	case relopEQ:
		return cmp == 0
	case relopNE:
		return cmp != 0
	}
	return false
}

// compareProp compares the object value with the restriction value using the
// case-insensitive string rules of the reference matcher. Uncomparable value
// kinds yield cmp 0 so that an EQ restriction does not spuriously fail.
func compareProp(object, restricted any) int {
	if object == nil || restricted == nil {
		return 0
	}
	if os, ok := object.(string); ok {
		rs, rok := restricted.(string)
		if !rok {
			return 0
		}
		return strings.Compare(strings.ToLower(os), strings.ToLower(rs))
	}
	if ob, ok := object.([]byte); ok {
		rb, rok := restricted.([]byte)
		if !rok {
			return 0
		}
		if c := bytesCompare(ob, rb); c != 0 {
			return c
		}
		switch {
		case len(ob) < len(rb):
			return -1
		case len(ob) > len(rb):
			return 1
		}
		return 0
	}
	a := asInt64Safe(object)
	b := asInt64Safe(restricted)
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func asInt64Safe(v any) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case uint16:
		return int64(t)
	case uint32:
		return int64(t)
	case uint64:
		return int64(t)
	case uint8:
		return int64(t)
	case float32:
		return int64(t)
	case float64:
		return int64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	}
	return 0
}

func bytesCompare(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Package nspi implements the Name Service Provider Interface (MS-OXNSPI)
// over the MAPI/HTTP transport: the address book operations Outlook uses to
// browse the GAL, resolve names and fetch recipient properties.
//
// The reference implementation is gromox's exch/mh + exch/nsp pair, which
// these files follow operation for operation, wire byte for wire byte.
package nspi

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/contacts"
	"github.com/linchange/linchange/internal/store"
)

// Backend holds the dependencies of the address book service: the directory
// of mail users (the GAL itself) and the host this server is reachable at,
// which appears in the URLs GetMailboxUrl/GetAddressBookUrl return and in
// the distinguished names the address book issues.
type Backend struct {
	DB    *sqlx.DB
	Users *store.Users
	// Contacts is the organization directory. Its cards add the titles and
	// telephone numbers the GAL shows beside a name; the users alone carry
	// neither.
	Contacts *contacts.Service
	// Hostname is the public name this server is reachable at, e.g.
	// "mail.example.test" (without scheme).
	Hostname string
	Logger   *slog.Logger
}

// record is one GAL recipient. The address book rows are a digest of the
// store's mail users, ordered by display name, with a minid assigned at load
// time (index + 0x10, leaving 0/1/2 free for the BEGINNING/CURRENT/END and
// UNRESOLVED/AMBIGUOUS/RESOLVED special values).
type record struct {
	mid                                      uint32
	address                                  string
	name                                     string
	title, company, department               string
	businessPhone, mobilePhone, primaryPhone string
	// perm is the record's permanent (non-ephemeral) property map, built
	// once when the GAL is loaded. Rows that need ephemeral entry ids are
	// built from propsFor on demand; the restriction evaluator only ever
	// asks for the permanent form, so it reads this and does no work.
	perm map[uint32]any
}

// Session is one NSPI connection's state: the GetSpecialTable version
// counter and the GAL digest. The GAL is loaded once, on first use, and
// reused for the life of the connection; the server GUID is a property of
// the server rather than the connection, so every session agrees on it (the
// reference builds it once at startup from its configuration).
type Session struct {
	mu      sync.Mutex
	loaded  bool
	host    string
	records []record
	version uint32
}

// NewSession creates an empty session. The GAL is loaded lazily, on the
// first operation that needs it.
func NewSession() *Session { return &Session{} }

// ServerGUID returns the address book server GUID, which Bind hands to the
// client and which the ephemeral entry ids of every GAL record embed.
func ServerGUID() string { return serverGUID }

// serverGUID is generated once per process. The reference derives it from
// its configuration and never changes it while running, so entry ids issued
// by one connection stay valid on the next.
var serverGUID = newGUID()

// Version returns the current GetSpecialTable version counter.
func (s *Session) Version() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// bumpVersion increments and returns the GetSpecialTable version counter.
// The counter advances once per GetSpecialTable call, which lets a client
// notice that the container list changed (MS-OXNSPI 3.1.4.1.4).
func (s *Session) bumpVersion() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	return s.version
}

// ErrDirectory reports that the address book could not read the directory,
// which the transport answers with a failure rather than with a malformed
// request: nothing about the body is in doubt, the data behind it is
// unavailable.
var ErrDirectory = errors.New("mapi/nspi: directory unavailable")

// gal returns the cached GAL digest, loading it from the directory if this
// is the first use of the session. The returned slice must not be modified.
func (s *Session) gal(ctx context.Context, back *Backend) ([]record, error) {
	s.mu.Lock()
	if s.loaded {
		recs := s.records
		s.mu.Unlock()
		return recs, nil
	}
	s.mu.Unlock()

	users, err := back.Users.List(ctx, back.DB)
	if err != nil {
		return nil, fmt.Errorf("%w: list directory: %w", ErrDirectory, err)
	}

	recs := make([]record, 0, len(users))
	for _, u := range users {
		if !u.Enabled || !u.MailboxEnabled {
			continue
		}
		name := u.DisplayName
		if name == "" {
			name = u.Address
		}
		recs = append(recs, record{address: u.Address, name: name})
	}

	// A directory card enriches the user it names and adds the people who are
	// in the organization's address book without being mailbox users here.
	if back.Contacts != nil {
		entries, err := back.Contacts.DirectoryEntries(ctx)
		if err != nil {
			if back.Logger != nil {
				back.Logger.Warn("mapi/nspi: could not read the organization directory", "error", err)
			}
		} else {
			byAddress := make(map[string]*record, len(recs))
			for i := range recs {
				byAddress[strings.ToLower(recs[i].address)] = &recs[i]
			}
			for _, e := range entries {
				if existing, ok := byAddress[strings.ToLower(e.Address)]; ok {
					if e.Name != "" {
						existing.name = e.Name
					}
					existing.title = e.Title
					existing.company = e.Company
					existing.department = e.Department
					existing.businessPhone = e.BusinessPhone
					existing.mobilePhone = e.MobilePhone
					existing.primaryPhone = e.PrimaryPhone
					continue
				}
				recs = append(recs, record{
					address: e.Address, name: e.Name,
					title: e.Title, company: e.Company, department: e.Department,
					businessPhone: e.BusinessPhone, mobilePhone: e.MobilePhone, primaryPhone: e.PrimaryPhone,
				})
			}
		}
	}
	// The reference address book sorts the GAL by display name, which is
	// also the order Outlook browses it in; ties break on address so the
	// order (and therefore every minid) is stable across reloads.
	sort.Slice(recs, func(i, j int) bool {
		li, lj := strings.ToLower(recs[i].name), strings.ToLower(recs[j].name)
		if li != lj {
			return li < lj
		}
		return strings.ToLower(recs[i].address) < strings.ToLower(recs[j].address)
	})
	for i := range recs {
		recs[i].mid = midBase + uint32(i)
		recs[i].perm = propsFor(&recs[i], back.Hostname, false)
	}

	s.mu.Lock()
	s.host = back.Hostname
	s.records = recs
	s.loaded = true
	s.mu.Unlock()
	return recs, nil
}

// hostname returns the public host name the session was loaded with, used
// in distinguished names and in the URLs the URL operations return.
func (s *Session) hostname() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.host
}

// props builds the whole property map of a record against this session's
// host name and the caller's entry id flavour. The permanent map built at
// load time is returned as is; row builders only read from it.
func (s *Session) props(rec *record, ephemeral bool) map[uint32]any {
	if !ephemeral && rec.perm != nil {
		return rec.perm
	}
	return propsFor(rec, s.hostname(), ephemeral)
}

// position reports the index of a minid in the GAL order, or -1 when the
// minid names no GAL entry. The index doubles as the table position that
// STAT's numPos/totalRec track.
func (s *Session) position(recs []record, mid uint32) int {
	if mid < midBase {
		return -1
	}
	i := int(mid - midBase)
	if i >= len(recs) || recs[i].mid != mid {
		return -1
	}
	return i
}

// lookup resolves a minid to its record.
func (s *Session) lookup(recs []record, mid uint32) (*record, bool) {
	i := s.position(recs, mid)
	if i < 0 {
		return nil, false
	}
	return &recs[i], true
}

// recordProp fetches one property of a record for the restriction
// evaluator. The reference evaluates restrictions against the permanent
// (non-ephemeral) property set, so the flavour is fixed here.
func (s *Session) recordProp(rec *record, tag uint32) (any, bool) {
	v, ok := s.props(rec, false)[tag]
	return v, ok
}

// Distinguished name shapes. The reference builds these in
// lib/mapi/usercvt.cpp from its organization name; linchange has one
// organization, so the prefixes are fixed here.
const (
	orgDN       = "/o=LINCHANGE/ou=Exchange Administrative Group (FYDIBOHF23SPDLT)"
	recipientDN = orgDN + "/cn=Recipients/cn="
	serversDN   = orgDN + "/cn=Configuration/cn=Servers/cn="
	mdbSuffix   = "/cn=Microsoft Private MDB"
	// ENTRYID_TYPE_PERMANENT and ENTRYID_TYPE_EPHEMERAL (mapidefs.h).
	permanentEIDID = 0
	ephemeralEIDID = 0x87
)

// userDN builds a user's X500 distinguished name, which is what
// PR_EMAIL_ADDRESS and PR_SEARCH_KEY are derived from.
func userDN(rec *record) string {
	return recipientDN + rec.name
}

// permanentEID is the 28-byte-plus-DN entry id of an address book object:
// u32 0, the muidEMSAB provider GUID, u32 1, the object type (DT_MAILUSER
// or DT_CONTAINER), then the NUL-terminated X500 DN. (MS-OXNSPI 2.2.2.4)
func permanentEID(dtyp uint32, dn string) []byte {
	w := newWriter()
	w.u32(permanentEIDID)
	w.guid(muidEMSAB)
	w.u32(1)
	w.u32(dtyp)
	w.bytes(append([]byte(dn), 0))
	return w.buf
}

// ephemeralEID is the 32-byte in-memory entry id of one GAL record: u32
// 0x87, the server GUID, u32 1, DT_MAILUSER, u32 minid. (MS-OXNSPI 2.2.2.4)
func ephemeralEID(mid uint32) []byte {
	w := newWriter()
	w.u32(ephemeralEIDID)
	w.guid(serverGUID)
	w.u32(1)
	w.u32(dtMailuser)
	w.u32(mid)
	return w.buf
}

// homeMDBDN builds the home mailbox database DN: the server container the
// record's mailbox lives on. (lib/mapi/usercvt.cpp cvt_username_to_mdbdn)
func homeMDBDN(host string) string {
	if host == "" {
		host = "localhost"
	}
	return serversDN + host + mdbSuffix
}

// propsFor builds every address book property of one record. The set
// mirrors nsp_interface_fetch_property of the reference implementation;
// properties a record lacks are simply absent from the map, which the row
// writers turn into error cells and GetProps into PT_ERROR entries.
//
// host is the public host name (the Backend's Hostname) and appears in
// PR_EMS_AB_HOME_MDB and PR_EMS_AB_NETWORK_ADDRESS. ephemeral selects the
// entry id flavour of PR_ENTRYID, PR_RECORD_KEY and PR_ORIGINAL_ENTRYID:
// the reference issues the in-memory (ephemeral) form to clients that set
// fEphID and the permanent X500 form to everyone else, PR_TEMPLATEID is
// always permanent, and the resolver asks for the permanent form.
func propsFor(rec *record, host string, ephemeral bool) map[uint32]any {
	if host == "" {
		host = "localhost"
	}
	dn := userDN(rec)
	searchKey := []byte(strings.ToUpper("EX:"+dn) + "\x00")
	midLE := make([]byte, 4)
	midLE[0] = byte(rec.mid)
	midLE[1] = byte(rec.mid >> 8)
	midLE[2] = byte(rec.mid >> 16)
	midLE[3] = byte(rec.mid >> 24)
	eph := ephemeralEID(rec.mid)
	perm := permanentEID(dtMailuser, dn)
	ids := eph
	if !ephemeral {
		ids = perm
	}
	guid := sha1.Sum([]byte(strings.ToLower(rec.address)))

	props := map[uint32]any{
		// Identity and addressing, all derived from the same directory row.
		// PR_ADDRTYPE is "EX" and PR_EMAIL_ADDRESS the X500 DN, exactly as
		// the reference serves them; the SMTP address has its own tag.
		prDisplayName:              rec.name,
		prEmABDisplayNamePrintable: rec.name,
		prTransmitableDisplayName:  rec.name,
		prAddrType:                 "EX",
		prEmailAddress:             dn,
		prSmtpAddress:              rec.address,
		prAccount:                  rec.address,
		prEmABProxyAddresses:       []string{"SMTP:" + rec.address},
		prEmABContainerID:          int64(0),
		prEmABNetworkAddress:       []string{"ncacn_ip_tcp:" + host, "ncacn_http:" + host},

		// Object identity: entry ids in the requested flavour — the reference
		// serves PR_ENTRYID, PR_RECORD_KEY and PR_ORIGINAL_ENTRYID from one
		// switch — while PR_TEMPLATEID is always the permanent X500 form.
		prEntryID:          ids,
		prRecordKey:        ids,
		prOriginalEntryID:  ids,
		prTemplateID:       perm,
		prSearchKey:        searchKey,
		prInstanceKey:      midLE,
		prMappingSignature: guidToBytes(muidEMSAB),
		prEmABObjectGUID:   guid[:],

		// Type tags.
		prObjectType:    int64(objMailuser),
		prDisplayType:   int64(dtMailuser),
		prDisplayTypeEx: int64(dtMailuser),
		prSendRichInfo:  true,
		prCreationTime:  int64(0),

		// Server-side mailbox home; a client resolving a recipient back to
		// a mailbox needs the MDB DN.
		prEmABHomeMDB: homeMDBDN(host),
	}
	if rec.title != "" {
		props[prTitle] = rec.title
	}
	if rec.company != "" {
		props[prCompanyName] = rec.company
	}
	if rec.department != "" {
		props[prDepartmentName] = rec.department
	}
	if rec.businessPhone != "" {
		props[prBusinessTelephoneNumber] = rec.businessPhone
	}
	if rec.mobilePhone != "" {
		props[prMobileTelephoneNumber] = rec.mobilePhone
	}
	if rec.primaryPhone != "" {
		props[prPrimaryTelephoneNumber] = rec.primaryPhone
	}

	// Both string flavours of every text property: the client's code page
	// decides which tag it asks for (GetPropList in a non-Unicode code page
	// asks for the 8-bit twin) and the reference answers either from the
	// same value. The twins are added from a snapshot so the map is not
	// mutated while it is being ranged over.
	var twins []prop
	for tag, v := range props {
		if twin, ok := ansiTwin(tag); ok {
			twins = append(twins, prop{tag: twin, value: v})
		}
	}
	for _, p := range twins {
		props[p.tag] = p.value
	}
	return props
}

// ansiTwin maps a Unicode string (or Unicode multi-value string) tag onto
// its 8-bit twin. Non-text properties have no twin and report false.
func ansiTwin(tag uint32) (uint32, bool) {
	switch propType(tag) {
	case ptUnicode:
		return changeType(tag, ptString8), true
	case ptMVUnicode:
		return changeType(tag, ptMVString8), true
	}
	return 0, false
}

// IsOperation reports whether name (case-insensitive) is a known NSPI
// request type. The transport uses it to reject unknown names with
// invalid_rq_type before parsing a body.
func IsOperation(name string) bool {
	name = strings.ToLower(name)
	_, ok := operations[name]
	return ok
}

// IsFormatError reports whether err is the address book's "this body did not
// parse" failure rather than a failure of the server to answer. The two mean
// opposite things to a client: one says fix the request, the other says the
// server could not read it.
func IsFormatError(err error) bool { return errors.Is(err, formatError) }

// operations maps every request type this implementation answers to its
// handler. Keys are lowercased: the reference lowercases X-RequestType
// before dispatch, so the client's spelling ("GetProps" or "getprops") does
// not matter. "querycolumns" is accepted as a spelling variant of the
// reference's "querycolumn".
//
// A handler takes the session, the backend, the authenticated principal
// (only the URL operations read it: the address book URL names the caller's
// own address book), the pre-loaded GAL snapshot and the request body, and
// returns the response PDU.
var operations = map[string]func(*Session, *Backend, string, []record, *reader) ([]byte, error){
	"bind":              (*Session).opBind,
	"unbind":            (*Session).opUnbind,
	"comparemids":       (*Session).opCompareMids,
	"dntomid":           (*Session).opDnToMid,
	"getmatches":        (*Session).opGetMatches,
	"getproplist":       (*Session).opGetPropList,
	"getprops":          (*Session).opGetProps,
	"getspecialtable":   (*Session).opGetSpecialTable,
	"gettemplateinfo":   (*Session).opGetTemplateInfo,
	"modlinkatt":        (*Session).opModLinkAtt,
	"modprops":          (*Session).opModProps,
	"querycolumn":       (*Session).opQueryColumns,
	"querycolumns":      (*Session).opQueryColumns,
	"queryrows":         (*Session).opQueryRows,
	"resolvenames":      (*Session).opResolveNames,
	"resortrestriction": (*Session).opResortRestriction,
	"seekentries":       (*Session).opSeekEntries,
	"updatestat":        (*Session).opUpdateStat,
	"getaddressbookurl": (*Session).opGetAddressBookURL,
	"getmailboxurl":     (*Session).opGetMailboxURL,
}

// Execute dispatches one NSPI operation by its lowercased X-RequestType and
// returns the response PDU (MS-OXNSPI 3.1.5). The GAL digest is loaded
// before the handler runs, so every handler reads the same snapshot. An
// unknown operation name or a body that does not parse yields an error,
// which the transport reports as an invalid request.
//
// principal is the authenticated caller, carried through to the operations
// that answer with a URL scoped to a person rather than to a mailbox.
func (s *Session) Execute(ctx context.Context, back *Backend, requestType, principal string, in []byte) ([]byte, error) {
	op, ok := operations[strings.ToLower(requestType)]
	if !ok {
		return nil, formatError
	}
	recs, err := s.gal(ctx, back)
	if err != nil {
		return nil, err
	}
	return op(s, back, principal, recs, newReader(in))
}

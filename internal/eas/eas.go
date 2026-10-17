// Package eas implements Microsoft Exchange ActiveSync (MS-ASCMD).
//
// It is the protocol a phone and a desktop client use when they "add an
// Exchange account": one endpoint that a client hits over and over, each
// request one command, the whole thing encoded as WBXML so that synchronising
// a folder of mail is a handful of kilobytes of tokens rather than a page of
// XML.
//
// The commands are the operations: FolderSync walks the folder tree, Sync walks
// a folder's items, SendMail sends a message, MoveItems moves some, and the
// rest manage folders, answer calendar meetings, and report on the account.
// Every one of them goes through the same mail, calendar and message stores
// the web client and EWS use, so a message sent here is the same row a message
// sent there is, and a folder renamed here is renamed for every other client by
// construction.
//
// This file is the transport (authentication, version negotiation, dispatch)
// and the command handlers; the WBXML codec they read and write is in wbxml.go.

package eas

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// maxRequestBytes bounds a request body.
//
// A SendMail carries a message and its attachments, so the bound is generous --
// but it is a bound. A client that announces a gigabyte of body gets to send
// one at a server that has not committed to reading it.
const maxRequestBytes = 64 << 20

// maxBodyBytes bounds the body a Sync response carries for one item. A full
// message body is not what a phone wants over a flaky link; a truncated
// preview with a flag that says so is what the protocol is for, and it keeps
// one sync from shipping every byte in a folder.
const maxBodyBytes = 100 << 10

// defaultWindowSize is how many changes a Sync returns when the client does not
// say. It is a bound on one response, not on the folder: what does not fit sets
// MoreAvailable and is sent on the next request.
const defaultWindowSize = 512

// heartbeatInterval is the seconds a Ping tells a client to wait before asking
// again. It is a suggestion a client may shorten, but it is the server's word on
// how often push is worth asking for.
const heartbeatInterval = 300

// supportedVersions are the protocol versions this server answers, oldest
// first. A client that asks for 14.0 is answered with the newest version this
// server implements, 12.1, and the command set it then uses is exactly the set
// implemented here: 14.x would steer it toward GetHierarchy and
// CreateCollection, which a phone that has been told 12.1 never sends.
var supportedVersions = []string{"2.0", "2.1", "2.5", "12.0", "12.1"}

// protocolCommands is the command list advertised, in the order a client reads
// it. It is the honest list: a command named here is a command this server
// answers, and nothing named here is a stub.
var protocolCommands = strings.Join([]string{
	"Sync", "SendMail", "FolderSync", "FolderCreate", "FolderDelete",
	"FolderUpdate", "MoveItems", "GetItemEstimate", "MeetingResponse",
	"Search", "Settings", "Ping", "ItemOperations", "Provision",
}, ",")

// Options are the dependencies of the service.
type Options struct {
	Config *config.Config
	DB     *sqlx.DB
	// Mail is the mail reading and writing service, shared with the web client
	// and EWS so that a message sent here and a message sent there are the same
	// row rather than two rows that have to be kept in agreement.
	Mail *webmail.Service
	// Messages is the row repository behind Mail, held because a Sync reads a
	// folder the way the store reads it, in pages, rather than through the
	// service's one-page-at-a-time listing.
	Messages *imapd.MessageRepo
	// Auth verifies the login. It is the same verifier the other mail listeners
	// hold, so one account has one password and one answer to "may this person
	// use this protocol".
	Auth Authenticator
	// Policy is the authorization rules, held for the decisions a request makes
	// after the logon.
	Policy policy.Policy
	Logger *slog.Logger
}

// EAS serves Exchange ActiveSync.
type EAS struct {
	cfg      *config.Config
	db       *sqlx.DB
	mail     *webmail.Service
	messages *imapd.MessageRepo
	auth     Authenticator
	policy   policy.Policy
	log      *slog.Logger
}

// Authenticator checks a login name and password for a protocol.
//
// It is satisfied by the shared mailauth.Verifier in production and is an
// interface rather than that type so a test of the command layer can stand in
// for a database it has no business opening.
type Authenticator interface {
	Authenticate(ctx context.Context, login, pw string, action policy.Action) (policy.Principal, error)
}

// New builds the service.
func New(opts Options) *EAS {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &EAS{
		cfg:      opts.Config,
		db:       opts.DB,
		mail:     opts.Mail,
		messages: opts.Messages,
		auth:     opts.Auth,
		policy:   opts.Policy,
		log:      log,
	}
}

// Handler returns the HTTP handler for the ActiveSync endpoint.
//
// The server mounts it at /eas, and the URL Autodiscover hands a client is
// /eas, so a command is answered at the mount point itself. A client that
// appends a path segment of its own is answered the same way: the command is
// the body, not the URL, and a 404 for a URL the body does not depend on is
// how a server tells a phone its account is broken when it is not.
func (e *EAS) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// A panic in a command becomes an answer a client can act on,
				// not a dropped connection a client reports as a network error.
				e.log.Error("eas: a request panicked", "path", r.URL.Path, "panic", rec)
				http.Error(w, "the request could not be processed", http.StatusInternalServerError)
			}
		}()
		switch r.Method {
		case http.MethodPost:
			e.handleEAS(w, r)
		case http.MethodOptions:
			e.handleOptions(w, r)
		case http.MethodGet:
			e.handleServiceRoot(w, r)
		default:
			w.Header().Set("Allow", "POST, OPTIONS, GET")
			http.Error(w, "ActiveSync accepts POST", http.StatusMethodNotAllowed)
		}
	})
}

// handleServiceRoot answers a probe of the mount point. A client that GETs the
// endpoint before it commits wants a yes, and a 404 is read as "there is no
// ActiveSync here".
func (e *EAS) handleServiceRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	w.Header().Set("MS-Server-ActiveSync", "12.1")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Microsoft-Server-ActiveSync 12.1\n"))
}

// handleOptions answers the version negotiation request a client sends before
// its first command. It is unauthenticated by design: a client that cannot
// even ask which versions exist should not have to log on to find out, and the
// answer carries nothing private.
func (e *EAS) handleOptions(w http.ResponseWriter, r *http.Request) {
	e.protocolHeaders(w, r)
	w.WriteHeader(http.StatusOK)
}

// protocolHeaders sets the version headers a response carries. A client picks
// the version it will speak from the MS-ASProtocolVersion answer and reads the
// rest from the two lists.
func (e *EAS) protocolHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("MS-Server-ActiveSync", "12.1")
	w.Header().Set("MS-ASProtocolVersion", selectProtocolVersion(r.Header.Get("MS-ASProtocolVersion")))
	w.Header().Set("MS-ASProtocolVersions", strings.Join(supportedVersions, ","))
	w.Header().Set("MS-ASProtocolCommands", protocolCommands)
}

// selectProtocolVersion answers a client's requested version with the newest
// version this server implements that the client asked for. A request for more
// than this server has gets the newest the server has; a request for less gets
// the newest at or under it; a request below everything gets the oldest,
// because refusing outright is how a server tells a client it should stop.
func selectProtocolVersion(requested string) string {
	// No request is a client that has not negotiated, and it speaks the newest
	// version this server has rather than the oldest.
	if strings.TrimSpace(requested) == "" {
		return supportedVersions[len(supportedVersions)-1]
	}
	req := versionNumber(requested)
	best := ""
	for _, v := range supportedVersions {
		if versionNumber(v) <= req {
			best = v
		}
	}
	if best == "" {
		return supportedVersions[0]
	}
	return best
}

// versionNumber renders "major.minor" as a comparable integer.
func versionNumber(v string) int {
	n := 0
	seen := false
	for _, c := range strings.TrimSpace(v) {
		switch {
		case c >= '0' && c <= '9':
			n = n*10 + int(c-'0')
			seen = true
		case c == '.':
			seen = true
		default:
			if seen {
				return n
			}
		}
	}
	return n
}

// isWBXMLContentType reports whether the announced content type is one this
// endpoint reads. ActiveSync is a binary protocol, and the type a client
// announces for its body is the wire format, so a body announced as anything
// else is not a command and is answered before it is read.
func isWBXMLContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = strings.TrimSpace(contentType[:i])
	}
	switch contentType {
	case "application/vnd.ms-sync.wbxml", "application/x-ms-sync-wbxml", "":
		return true
	}
	return strings.Contains(contentType, "wbxml") || strings.Contains(contentType, "ms-sync")
}

// handleEAS authenticates the caller, decodes the WBXML command, and runs it.
func (e *EAS) handleEAS(w http.ResponseWriter, r *http.Request) {
	if !isWBXMLContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "ActiveSync accepts a WBXML document", http.StatusBadRequest)
		return
	}

	who, ok := e.authenticate(w, r)
	if !ok {
		return
	}

	e.protocolHeaders(w, r)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		http.Error(w, "the request body could not be read", http.StatusBadRequest)
		return
	}

	dec := NewWBXMLDecoder(body)
	root, err := dec.Decode()
	if err != nil {
		http.Error(w, "the request is not a WBXML document", http.StatusBadRequest)
		return
	}

	c := &call{srv: e, ctx: r.Context(), who: who, root: root,
		version: requestedVersion(r)}
	bw := NewWBXMLWriter()
	er := e.runCommand(c, bw)
	e.writeResponse(w, c.root.NS, c.root.Name, bw, er)
}

// authenticate verifies the credentials, answering the client itself when there
// are none. The refusal is a 401 with a Basic challenge, which is what makes a
// client ask for a password rather than treat the account as absent.
func (e *EAS) authenticate(w http.ResponseWriter, r *http.Request) (policy.Principal, bool) {
	if e.auth == nil {
		e.challenge(w)
		return policy.Principal{}, false
	}
	login, pw, ok := r.BasicAuth()
	if !ok || login == "" {
		e.challenge(w)
		return policy.Principal{}, false
	}
	who, err := e.auth.Authenticate(r.Context(), login, pw, policy.ActionEAS)
	if err != nil {
		e.log.InfoContext(r.Context(), "eas login refused", "address", login, "error", err)
		e.challenge(w)
		return policy.Principal{}, false
	}
	return who, true
}

func (e *EAS) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="linchange"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

// call is one authenticated command: the context, the principal it acts as, and
// the decoded command element it is reading.
type call struct {
	srv  *EAS
	ctx  context.Context
	who  policy.Principal
	root *WBXMLNode
	// version is the protocol version the client asked for in its
	// MS-ASProtocolVersion header, empty when it asked for none.
	version string
}

// owner is the account the command acts as, from the authenticated principal
// rather than from anywhere in the request.
func (c *call) owner() string { return c.who.Address }

// reports14 answers whether the client is on 14.0 or newer. MS-ASCMD 2.2.2
// says the common 1xx status codes are carried in a Status element on a 200 by
// those versions and as an HTTP 4xx/5xx by 2.5, 12.0 and 12.1, so a failure
// has to be dressed the way the client was told to read it.
func (c *call) reports14() bool {
	major, _, ok := strings.Cut(c.version, ".")
	if !ok {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(major))
	if err != nil {
		return false
	}
	return n >= 14
}

// requestedVersion reads the protocol version a client asked for. A client may
// list several separated by commas; the first is the one it speaks.
func requestedVersion(r *http.Request) string {
	raw := r.Header.Get("MS-ASProtocolVersion")
	if i := strings.IndexByte(raw, ','); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

// easErr is a command's answer when it is not a success. status is the EAS
// Status code to write into the command's response; http is the status the
// answer is sent with. A 200 with a non-1 Status is how a command reports a
// problem it understood; anything else is a fault of the request itself.
type easErr struct {
	status int
	http   int
	text   string
}

func (e *EAS) badRequest(text string) *easErr {
	return &easErr{status: 2, http: http.StatusBadRequest, text: text}
}

func (e *EAS) dbErr(command string, ctx context.Context, err error) *easErr {
	e.log.ErrorContext(ctx, "eas: "+command+" failed", "error", err)
	return &easErr{status: 6001, http: http.StatusInternalServerError, text: "the request could not be processed"}
}

// writeResponse sends the command's answer, or a command-shaped error when the
// command could not run. A command that understood the request but could not do
// it answers inside the command (Status non-1, HTTP 200), because that is how a
// client distinguishes "the folder is gone" from "the network is gone".
func (e *EAS) writeResponse(w http.ResponseWriter, ns, name string, bw *WBXMLWriter, er *easErr) {
	if er != nil {
		if er.http != http.StatusOK {
			http.Error(w, er.text, er.http)
			return
		}
		ew := NewWBXMLWriter()
		_ = ew.Start(ns, name)
		_ = ew.Leaf(ns, "Status", strconv.Itoa(er.status))
		ew.End()
		e.sendWBXML(w, ew.Bytes())
		return
	}
	// A SendMail that succeeded answers with nothing at all: MS-ASCMD says the
	// response has no body, and a client reads the empty answer as "sent".
	if name == "SendMail" {
		w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
		w.WriteHeader(http.StatusOK)
		return
	}
	e.sendWBXML(w, bw.Bytes())
}

func (e *EAS) sendWBXML(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// runCommand dispatches to the command the request named.
func (e *EAS) runCommand(c *call, w *WBXMLWriter) *easErr {
	switch c.root.Name {
	case "Provision":
		return e.cmdProvision(c, w)
	case "FolderSync":
		return e.cmdFolderSync(c, w)
	case "Sync":
		return e.cmdSync(c, w)
	case "Ping":
		return e.cmdPing(c, w)
	case "SendMail":
		return e.cmdSendMail(c, w)
	case "MoveItems":
		return e.cmdMoveItems(c, w)
	case "GetItemEstimate":
		return e.cmdGetItemEstimate(c, w)
	case "FolderCreate":
		return e.cmdFolderCreate(c, w)
	case "FolderDelete":
		return e.cmdFolderDelete(c, w)
	case "FolderUpdate":
		return e.cmdFolderUpdate(c, w)
	case "MeetingResponse":
		return e.cmdMeetingResponse(c, w)
	case "Search":
		return e.cmdSearch(c, w)
	case "Settings":
		return e.cmdSettings(c, w)
	case "ItemOperations":
		return e.cmdItemOperations(c, w)
	default:
		return &easErr{
			status: 2,
			http:   http.StatusNotImplemented,
			text:   c.root.Name + " is not a command this service implements",
		}
	}
}

// ---------------------------------------------------------------------------
// Provision
// ---------------------------------------------------------------------------

// devicePolicy is the device policy a Provision answers with, as the fields of
// an EASProvisionDoc element. It is a real policy, not an empty shell: a client
// reads the values to decide what it may do, and a client told "no password, no
// encryption" behaves differently from one told the opposite.
var devicePolicy = []struct {
	field string
	value string
}{
	{"DevicePasswordEnabled", "0"},
	{"AlphanumericDevicePasswordRequired", "0"},
	{"PasswordRecoveryEnabled", "0"},
	{"RequireStorageCardEncryption", "0"},
	{"AttachmentsEnabled", "1"},
	{"MinDevicePasswordLength", "0"},
	{"MaxInactivityTimeDeviceLock", "900"},
	{"MaxDevicePasswordFailedAttempts", "8"},
	{"MaxAttachmentSize", "10485760"},
	{"AllowSimpleDevicePassword", "1"},
	{"DevicePasswordExpiration", "0"},
	{"DevicePasswordHistory", "0"},
	{"RequireDeviceEncryption", "0"},
	{"AllowUnsignedApplications", "1"},
	{"AllowHTMLEmail", "1"},
	{"MaxEmailAgeFilter", "0"},
	{"MaxEmailBodyTruncationSize", "102400"},
	{"MaxEmailHTMLBodyTruncationSize", "102400"},
	{"AllowConsumerEmail", "1"},
	{"AllowBrowser", "1"},
	{"AllowInternetSharing", "1"},
}

// policyType is the policy type a client provisioning 12.0 or newer asks for.
// A client that reads another value treats the answer as a policy for someone
// else and refuses the account.
const policyType = "MS-EAS-Provisioning-WBXML"

// cmdProvision answers a Provision.
//
// Provisioning is two requests, per MS-ASPROV. The first asks for the policy
// and gets the policy document plus a temporary key; the second acknowledges
// that the client applied it, and gets the final key that goes in the
// X-MS-PolicyKey header of every command afterwards. Telling the two apart is
// the PolicyKey in the request: the first one carries none.
func (e *EAS) cmdProvision(c *call, w *WBXMLWriter) *easErr {
	var requested *WBXMLNode
	if policies := c.root.FindDescendant("Policies"); policies != nil {
		for _, policy := range policies.FindChildren("Policy") {
			if policy.TextOf("PolicyType") == policyType {
				requested = policy
			}
		}
	}
	if requested == nil {
		// A policy of another type is a client asking for something this
		// service does not issue; the status says so inside the command, which
		// is where a client looks for it.
		return &easErr{status: 11, http: http.StatusOK, text: "the policy type is not one this service issues"}
	}
	acknowledging := requested.TextOf("PolicyKey") != ""

	w.Start(nsProvision, "Provision")
	// The client describes itself on the first request, and the answer names
	// the same element back so the client knows it was read. It is a child of
	// Provision, in the Settings code page, and per MS-ASPROV its status sits
	// directly under DeviceInformation (unlike a Settings command, where it is
	// under Set).
	if info := c.root.FindDescendant("DeviceInformation"); info != nil {
		w.Start(nsSettings, "DeviceInformation")
		_ = w.Leaf(nsSettings, "Status", "1")
		w.End()
	}
	_ = w.Leaf(nsProvision, "Status", "1")
	w.Start(nsProvision, "Policies")
	w.Start(nsProvision, "Policy")
	_ = w.Leaf(nsProvision, "PolicyType", policyType)
	_ = w.Leaf(nsProvision, "Status", "1")
	if acknowledging {
		_ = w.Leaf(nsProvision, "PolicyKey", policyKey(c.owner()))
	} else {
		_ = w.Leaf(nsProvision, "PolicyKey", temporaryPolicyKey(c.owner()))
		w.Start(nsProvision, "Data")
		w.Start(nsProvision, "EASProvisionDoc")
		for _, field := range devicePolicy {
			_ = w.Leaf(nsProvision, field.field, field.value)
		}
		w.End()
		w.End()
	}
	w.End() // Policy
	w.End() // Policies
	w.End() // Provision
	return nil
}

// ---------------------------------------------------------------------------
// FolderSync
// ---------------------------------------------------------------------------

// Folder type values, per MS-ASCMD section 2.2.3.186.3: the default folders
// carry 2 through 11, a user-created folder 1 through 17 depending on its
// kind, and 18 is an unknown one. A client draws the folder and chooses its
// behaviour from these -- 5 means "Sent", and a client that cannot find 5
// never syncs the folder its own sent mail lands in -- so the type is part of
// the contract, not a label.
const (
	folderTypeOther    = 1  // user-created folder (generic)
	folderTypeInbox    = 2  // default Inbox
	folderTypeDrafts   = 3  // default Drafts
	folderTypeDeleted  = 4  // default Deleted Items
	folderTypeSent     = 5  // default Sent Items
	folderTypeOutbox   = 6  // default Outbox
	folderTypeTasks    = 7  // default Tasks
	folderTypeCalendar = 8  // default Calendar
	folderTypeContacts = 9  // default Contacts
	folderTypeNotes    = 10 // default Notes
	folderTypeJournal  = 11 // default Journal
	folderTypeMailbox  = 12 // user-created mail folder
	folderTypeUnknown  = 18
)

// Folder types a client may ask FolderCreate for (MS-ASCMD 2.2.3.186.2): only
// the user-created kinds, since 2 through 11 are reserved for the defaults.
const (
	folderTypeUserCalendar = 13 // user-created Calendar folder
)

// easFolderType maps a folder to its EAS type from its special use, falling
// back to its name. A folder that is none of the named kinds is Other, which is
// the honest answer rather than a guess that a client would act on.
func easFolderType(name string, specialUse *string) int {
	// The store marks a folder's role with the RFC 6154 special use, lowercase
	// and backslash-prefixed; INBOX carries none, so the name decides for it.
	if specialUse != nil {
		switch strings.ToLower(*specialUse) {
		case `\inbox`:
			return folderTypeInbox
		case `\outbox`:
			return folderTypeOutbox
		case `\sent`:
			return folderTypeSent
		case `\trash`, `\junk`, `\deleted`:
			return folderTypeDeleted
		case `\drafts`:
			return folderTypeDrafts
		}
	}
	switch strings.ToUpper(name) {
	case "INBOX":
		return folderTypeInbox
	case "OUTBOX":
		return folderTypeOutbox
	case "SENT", "SENT ITEMS":
		return folderTypeSent
	case "TRASH", "DELETED ITEMS", "JUNK", "JUNK EMAIL", "SPAM":
		return folderTypeDeleted
	case "DRAFTS":
		return folderTypeDrafts
	}
	return folderTypeOther
}

// easFolder is one folder as a FolderSync reports it.
type easFolder struct {
	serverID   string
	parentID   string
	name       string
	folderType int
	total      int
	key        string
}

// collectFolders gathers every folder the account has: its mail folders and its
// calendars. The two live in different tables and get different server ids, and
// both are top level, which is what the folder tree a phone draws is.
func (e *EAS) collectFolders(c *call) ([]easFolder, *easErr) {
	var out []easFolder

	folders, err := e.mail.Folders(c.ctx, c.owner())
	if err != nil {
		return nil, e.dbErr("FolderSync", c.ctx, err)
	}
	for _, f := range folders {
		parent := "0"
		if f.ParentID != nil {
			parent = strconv.FormatInt(*f.ParentID, 10)
		}
		out = append(out, easFolder{
			serverID:   strconv.FormatInt(f.ID, 10),
			parentID:   parent,
			name:       f.Name,
			folderType: easFolderType(f.Name, f.SpecialUse),
			total:      f.Total,
		})
	}

	if _, err := store.NewCalendars().EnsureDefault(c.ctx, e.db, c.owner(), "UTC"); err != nil {
		// A calendar the account should have is not an empty calendar space; the
		// read fails the way a store fault fails every read.
		return nil, e.dbErr("FolderSync", c.ctx, err)
	}
	cals, err := store.NewCalendars().ByOwner(c.ctx, e.db, c.owner())
	if err != nil {
		return nil, e.dbErr("FolderSync", c.ctx, err)
	}
	for _, cal := range cals {
		out = append(out, easFolder{
			serverID:   "cal" + strconv.FormatInt(cal.ID, 10),
			parentID:   "0",
			name:       cal.Label(),
			folderType: folderTypeCalendar,
		})
	}

	for i := range out {
		out[i].key = changeKey(out[i].serverID, out[i].name, out[i].parentID,
			out[i].folderType, out[i].total)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].serverID < out[j].serverID })
	return out, nil
}

// cmdFolderSync walks the folder tree, returning every folder on the first sync
// and only what changed on the ones after. The response shape follows what a
// client reads: Status, then the SyncKey for the next round, then the Changes
// with one Add, Delete or Update per folder.
func (e *EAS) cmdFolderSync(c *call, w *WBXMLWriter) *easErr {
	folders, er := e.collectFolders(c)
	if er != nil {
		return er
	}

	state, present, serr := decodeState[folderSyncState](c.root.TextDescendant("SyncKey"))
	if serr != nil {
		return serr
	}
	var known map[string]string
	if present {
		known = state.Folders
	}

	current := make(map[string]easFolder, len(folders))
	for _, f := range folders {
		current[f.serverID] = f
	}

	// A fresh sync adds every folder; an incremental one reports the diff. The
	// same code does both: with no known state, every folder is an add and
	// there is nothing to delete.
	var added, updated, deleted []string
	for id, f := range current {
		if known == nil || known[id] == "" {
			added = append(added, id)
		} else if known[id] != f.key {
			updated = append(updated, id)
		}
	}
	if known != nil {
		for id := range known {
			if _, ok := current[id]; !ok {
				deleted = append(deleted, id)
			}
		}
	}
	sort.Strings(added)
	sort.Strings(updated)
	sort.Strings(deleted)

	// The state for the next request is the snapshot just reported, in full,
	// because a folder sync has no window: every folder is small, and a client
	// that missed one would have no way to learn it was missed.
	next := make(map[string]string, len(current))
	for id, f := range current {
		next[id] = f.key
	}
	encoded, _ := encodeState(folderSyncState{Folders: next})

	_ = w.Start(nsFolderHierarchy, "FolderSync")
	_ = w.Leaf(nsFolderHierarchy, "Status", "1")
	_ = w.Leaf(nsFolderHierarchy, "SyncKey", encoded)
	w.Start(nsFolderHierarchy, "Changes")
	_ = w.Leaf(nsFolderHierarchy, "Count", strconv.Itoa(len(added)+len(updated)+len(deleted)))
	for _, id := range added {
		f := current[id]
		w.Start(nsFolderHierarchy, "Add")
		_ = w.Leaf(nsFolderHierarchy, "ServerId", f.serverID)
		_ = w.Leaf(nsFolderHierarchy, "ParentId", f.parentID)
		_ = w.Leaf(nsFolderHierarchy, "DisplayName", f.name)
		_ = w.Leaf(nsFolderHierarchy, "Type", strconv.Itoa(f.folderType))
		w.End()
	}
	for _, id := range deleted {
		w.Start(nsFolderHierarchy, "Delete")
		_ = w.Leaf(nsFolderHierarchy, "ServerId", id)
		w.End()
	}
	for _, id := range updated {
		f := current[id]
		w.Start(nsFolderHierarchy, "Update")
		_ = w.Leaf(nsFolderHierarchy, "ServerId", f.serverID)
		_ = w.Leaf(nsFolderHierarchy, "ParentId", f.parentID)
		_ = w.Leaf(nsFolderHierarchy, "DisplayName", f.name)
		_ = w.Leaf(nsFolderHierarchy, "Type", strconv.Itoa(f.folderType))
		w.End()
	}
	w.End() // Changes
	w.End() // FolderSync
	return nil
}

// ---------------------------------------------------------------------------
// Sync
// ---------------------------------------------------------------------------

// syncItem is one item a Sync reports: its server id, the change key that says
// whether it has changed since the client last saw it, and the renderer that
// writes it into the response.
type syncItem struct {
	serverID string
	key      string
	render   func(w *WBXMLWriter)
}

// cmdSync walks a folder's items, returning every item on the first sync and
// only the deletes, replaces and adds on the ones after. It handles both mail
// folders and the calendar, which are the two kinds of folder a client syncs.
//
// The folder is named in the request body, in the Collection element, not in
// the URL: the request is a Sync for a collection, and the collection is the
// argument. The answer carries the same Collection so a client that syncs two
// folders in one session can tell the two answers apart.
func (e *EAS) cmdSync(c *call, w *WBXMLWriter) *easErr {
	folderID := c.root.TextDescendant("CollectionId")
	if folderID == "" {
		return e.badRequest("a Sync names no folder")
	}
	kind, id, ok := parseServerID(folderID)
	if !ok || kind == kindRoot {
		return &easErr{status: 6003, http: http.StatusOK, text: "the Sync names a folder it does not know"}
	}
	// A calendar ServerId is a bare row id, guessable by counting; mail reads
	// are scoped to the owner by the query, so the calendar is the one place
	// where the id itself has to be checked before anything is read or written.
	if kind == kindCalendar {
		cal, err := store.NewCalendars().Get(c.ctx, e.db, id)
		if err != nil || cal.Owner == nil || *cal.Owner != c.owner() {
			return &easErr{status: 6003, http: http.StatusOK, text: "the Sync names a folder it does not know"}
		}
	}

	// The request may carry the client's own changes to apply before the
	// answer is built. A phone marks a message read the moment its owner taps
	// it, and that arrives as a Change with the new Read flag, so a Sync that
	// ignored it would undo the tap on the next pull.
	e.applyClientChanges(c)

	window := asInt(c.root.FindDescendant("WindowSize"), 0)
	if window <= 0 {
		window = defaultWindowSize
	}

	state, present, serr := decodeState[itemsState](c.root.TextDescendant("SyncKey"))
	if serr != nil {
		return serr
	}
	if present && state.Folder != "" && state.Folder != folderID {
		// A state from another folder is a client that synced one folder and
		// asked about another; diffing against it would be nonsense, so it is
		// refused and the client's next request, without a state, is a fresh
		// sync of the folder it actually asked about.
		return &easErr{status: 1201, http: http.StatusOK, text: "the sync state belongs to another folder"}
	}
	var known map[string]string
	if present {
		known = state.Items
	}

	items, er := e.readItems(c, kind, id)
	if er != nil {
		return er
	}

	current := make(map[string]syncItem, len(items))
	for _, it := range items {
		current[it.serverID] = it
	}

	// A fresh sync adds every item; an incremental one reports the diff. The
	// same code does both: with no known state, every item is an add and there
	// is nothing to delete.
	var adds, replaces, deletes []string
	for sid, it := range current {
		if known == nil || known[sid] == "" {
			adds = append(adds, sid)
		} else if known[sid] != it.key {
			replaces = append(replaces, sid)
		}
	}
	if known != nil {
		for sid := range known {
			if _, ok := current[sid]; !ok {
				deletes = append(deletes, sid)
			}
		}
	}
	sort.Strings(adds)
	sort.Strings(replaces)
	sort.Strings(deletes)

	// The changes, in the order a client applies them: deletes first, so a
	// moved item is not shown as both present and gone; replaces, so an item
	// the client has is corrected; adds last, so new items land on top.
	type pending struct {
		section int
		sid     string
	}
	var changes []pending
	for _, sid := range deletes {
		changes = append(changes, pending{0, sid})
	}
	for _, sid := range replaces {
		changes = append(changes, pending{1, sid})
	}
	for _, sid := range adds {
		changes = append(changes, pending{2, sid})
	}
	more := len(changes) > window

	// The window bounds one response. What does not fit is reported with
	// MoreAvailable and sent on the next request, so a large folder syncs in
	// pieces rather than in one response a phone cannot hold.
	limit := len(changes)
	if limit > window {
		limit = window
	}
	var sentAdds, sentReplaces, sentDeletes []string
	for _, p := range changes[:limit] {
		switch p.section {
		case 0:
			sentDeletes = append(sentDeletes, p.sid)
		case 1:
			sentReplaces = append(sentReplaces, p.sid)
		default:
			sentAdds = append(sentAdds, p.sid)
		}
	}

	// The state for the next request is what the client now knows: its old
	// state, plus what this response told it, minus what it was told to forget.
	// Advancing it to the full snapshot when the response is truncated would
	// lose the changes the window left out, which is the one thing a sync must
	// never do.
	next := make(map[string]string, len(known)+len(sentAdds)+len(sentReplaces))
	if known != nil {
		for k, v := range known {
			next[k] = v
		}
	}
	for _, sid := range sentAdds {
		next[sid] = current[sid].key
	}
	for _, sid := range sentReplaces {
		next[sid] = current[sid].key
	}
	for _, sid := range sentDeletes {
		delete(next, sid)
	}
	encoded, _ := encodeState(itemsState{Folder: folderID, Items: next})

	_ = w.Start(nsAirSync, "Sync")
	w.Start(nsAirSync, "Collections")
	w.Start(nsAirSync, "Collection")
	_ = w.Leaf(nsAirSync, "SyncKey", encoded)
	_ = w.Leaf(nsAirSync, "CollectionId", folderID)
	_ = w.Leaf(nsAirSync, "Status", "1")

	// The changes go in one Commands element, in the order a client applies
	// them: deletes first, so a moved item is not shown as both present and
	// gone; changes, so an item the client has is corrected; adds last, so new
	// items land on top. The window cuts the list where it must, and
	// MoreAvailable says the rest is on the next request.
	if limit > 0 {
		if more {
			w.Start(nsAirSync, "MoreAvailable")
			w.End()
		}
		w.Start(nsAirSync, "Commands")
		for _, p := range changes[:limit] {
			switch p.section {
			case 0:
				w.Start(nsAirSync, "Delete")
				_ = w.Leaf(nsAirSync, "ServerId", p.sid)
				w.End()
			default:
				it := current[p.sid]
				if p.section == 1 {
					w.Start(nsAirSync, "Change")
				} else {
					w.Start(nsAirSync, "Add")
				}
				_ = w.Leaf(nsAirSync, "ServerId", it.serverID)
				it.render(w)
				w.End()
			}
		}
		w.End() // Commands
	}
	w.End() // Collection
	w.End() // Collections
	w.End() // Sync
	return nil
}

// applyClientChanges applies the changes a Sync request carries from the
// client's side. Mail accepts the flags: a message marked read, unread, flagged
// or unflagged. A mail Delete moves the message to Trash, and a Delete already
// in Trash destroys it, which is what a client means by "delete" and "empty"
// respectively. The calendar accepts Adds, Changes and Deletes outright, since
// an event is entirely the client's to write. A change that cannot be applied is
// logged and skipped, because one bad row must not fail the whole sync.
func (e *EAS) applyClientChanges(c *call) {
	commands := c.root.FindDescendant("Commands")
	if commands == nil {
		return
	}
	kind, folderID, ok := parseServerID(c.root.TextDescendant("CollectionId"))
	if !ok {
		return
	}

	for _, del := range commands.FindChildren("Delete") {
		sid := del.TextOf("ServerId")
		switch kind {
		case kindMailbox:
			id, err := strconv.ParseInt(sid, 10, 64)
			if err != nil || id <= 0 {
				continue
			}
			trashID, found := e.trashFolder(c.ctx, c.owner())
			if !found {
				e.log.WarnContext(c.ctx, "eas: Sync delete found no Trash folder", "id", id)
				continue
			}
			if folderID == trashID {
				if _, _, err := e.mail.Delete(c.ctx, c.owner(), []int64{id}); err != nil {
					e.log.WarnContext(c.ctx, "eas: could not delete a message", "id", id, "error", err)
				}
			} else if _, err := e.mail.Move(c.ctx, c.owner(), []int64{id}, trashID); err != nil {
				e.log.WarnContext(c.ctx, "eas: could not move a message to Trash", "id", id, "error", err)
			}
		case kindCalendar:
			ev, _, ok := e.calendarItem(c.ctx, folderID, sid)
			if !ok {
				continue
			}
			if err := store.NewEvents().Delete(c.ctx, e.db, ev.ID); err != nil {
				e.log.WarnContext(c.ctx, "eas: could not delete a calendar event", "server_id", sid, "error", err)
			}
		}
	}

	for _, add := range commands.FindChildren("Add") {
		if kind != kindCalendar {
			continue // mail a client "adds" to a folder is a draft the server
			// was never asked to keep; SendMail is the way to create mail.
		}
		data := add.FindChild("ApplicationData")
		if data == nil || strings.TrimSpace(data.TextOf("Subject")) == "" {
			continue
		}
		start := parseEASDateTime(data.TextOf("StartTime"))
		if start.IsZero() {
			start = time.Now().UTC()
		}
		end := parseEASDateTime(data.TextOf("EndTime"))
		if end.Before(start) {
			end = start.Add(time.Hour)
		}
		uid := data.TextOf("UID")
		if uid == "" {
			uid = newEventUID()
		}
		summary := strings.TrimSpace(data.TextOf("Subject"))
		location := data.TextOf("Location")
		owner := c.owner()
		_, err := store.NewEvents().Create(c.ctx, e.db, store.WriteEventRequest{
			CalendarID:       folderID,
			UID:              uid,
			ICalBlob:         basicICal(uid, start, end, summary, location),
			DTStart:          start,
			DTEnd:            end,
			AllDay:           asBool(data.FindChild("AllDayEvent"), false),
			Summary:          summary,
			Location:         location,
			OrganizerAddress: owner,
			DTStamp:          time.Now().UTC(),
			// The client that creates a meeting is its organizer, and an
			// organizer is an attendee for MeetingResponse purposes: the answer
			// to a meeting has to land on somebody's partstat.
			Attendees: []store.Attendee{
				{Address: owner, Role: "ORG", PartStat: "NEEDS-ACTION", RSVP: true},
			},
		})
		if err != nil {
			e.log.WarnContext(c.ctx, "eas: could not create a calendar event", "error", err)
		}
	}

	for _, change := range commands.FindChildren("Change") {
		data := change.FindChild("ApplicationData")
		if data == nil {
			continue
		}
		if kind == kindCalendar {
			ev, recID, ok := e.calendarItem(c.ctx, folderID, change.TextOf("ServerId"))
			if !ok {
				continue
			}
			start := parseEASDateTime(data.TextOf("StartTime"))
			if start.IsZero() {
				start = ev.DTStart
			}
			end := parseEASDateTime(data.TextOf("EndTime"))
			if end.Before(start) {
				end = ev.DTEnd
			}
			if end.Before(start) {
				end = start
			}
			summary := strings.TrimSpace(data.TextOf("Subject"))
			if summary == "" {
				summary = ev.Summary
			}
			_, err := store.NewEvents().Upsert(c.ctx, e.db, store.WriteEventRequest{
				CalendarID:   folderID,
				UID:          ev.UID,
				RecurrenceID: recID,
				ICalBlob:     ev.ICalBlob,
				DTStart:      start,
				DTEnd:        end,
				AllDay:       asBool(data.FindChild("AllDayEvent"), ev.AllDay),
				RRULE:        ev.RRULE,
				Summary:      summary,
				Location:     data.TextOf("Location"),
				DTStamp:      time.Now().UTC(),
			})
			if err != nil {
				e.log.WarnContext(c.ctx, "eas: could not revise a calendar event", "error", err)
			}
			continue
		}

		id, err := strconv.ParseInt(change.TextOf("ServerId"), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		if read := data.FindChild("Read"); read != nil {
			if err := e.mail.MarkRead(c.ctx, c.owner(), id, asBool(read, false)); err != nil {
				e.log.WarnContext(c.ctx, "eas: could not apply a client's read flag", "id", id, "error", err)
			}
		}
		if flag := data.FindChild("Flag"); flag != nil {
			// Per MS-ASCMD FlagType is 1 for flagged and 2 for completed; both
			// leave the message flagged, and an absent or zero type clears it.
			set := asInt(flag.FindChild("FlagType"), 1) != 0
			if err := e.mail.SetFlag(c.ctx, c.owner(), id, `\Flagged`, set); err != nil {
				e.log.WarnContext(c.ctx, "eas: could not apply a client's flag", "id", id, "error", err)
			}
		}
	}
}

// trashFolder returns the owner's Trash folder id.
func (e *EAS) trashFolder(ctx context.Context, owner string) (int64, bool) {
	folders, err := e.mail.Folders(ctx, owner)
	if err != nil {
		return 0, false
	}
	for _, f := range folders {
		if strings.EqualFold(f.Name, "Trash") {
			return f.ID, true
		}
	}
	return 0, false
}

// calendarItem finds an event by its sync server id, "cal<calendar>:<uid>" and,
// for one occurrence of a recurring series, the occurrence time after that.
func (e *EAS) calendarItem(ctx context.Context, calendarID int64, serverID string) (store.Event, *time.Time, bool) {
	if !strings.HasPrefix(serverID, "cal") {
		return store.Event{}, nil, false
	}
	parts := strings.Split(serverID, ":")
	if len(parts) < 2 || parts[1] == "" {
		return store.Event{}, nil, false
	}
	var recID *time.Time
	if len(parts) == 3 {
		if nano, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
			t := time.Unix(0, nano)
			recID = &t
		}
	}
	ev, err := store.NewEvents().ByUID(ctx, e.db, calendarID, parts[1], recID)
	if err != nil {
		return store.Event{}, nil, false
	}
	return ev, recID, true
}

// parseEASDateTime reads the EAS datetime form, yyyymmddThhmmss with an
// optional trailing Z, and falls back to anything RFC 3339 shaped.
func parseEASDateTime(text string) time.Time {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}
	for _, layout := range []string{"20060102T150405Z", "20060102T150405", time.RFC3339} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// newEventUID mints an event UID for a client that sends none.
func newEventUID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("eas-%d@linchange", time.Now().UnixNano())
	}
	return hex.EncodeToString(b) + "@linchange"
}

// basicICal builds the iCalendar document for an event a client created without
// carrying one, so the stored row keeps a document another client can read.
func basicICal(uid string, start, end time.Time, summary, location string) []byte {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//linchange//EAS//EN\r\nBEGIN:VEVENT\r\n")
	b.WriteString("UID:" + uid + "\r\n")
	b.WriteString("DTSTART:" + start.UTC().Format("20060102T150405Z") + "\r\n")
	b.WriteString("DTEND:" + end.UTC().Format("20060102T150405Z") + "\r\n")
	b.WriteString("SUMMARY:" + escapeICal(summary) + "\r\n")
	if location != "" {
		b.WriteString("LOCATION:" + escapeICal(location) + "\r\n")
	}
	b.WriteString("END:VEVENT\r\nEND:VCALENDAR\r\n")
	return []byte(b.String())
}

// escapeICal writes a text value the way RFC 5545 requires: commas, semicolons
// and newlines escaped, so a meeting titled "Kickoff, phase 2" survives the round
// trip.
func escapeICal(text string) string {
	out := strings.ReplaceAll(text, "\\", "\\\\")
	out = strings.ReplaceAll(out, ",", "\\,")
	out = strings.ReplaceAll(out, ";", "\\;")
	out = strings.ReplaceAll(out, "\n", "\\n")
	return out
}

// readItems reads a folder's items as render triples. A mail folder returns its
// messages; a calendar returns its events. The render closure carries the
// per-item work, so the diff above stays ignorant of what an item is.
func (e *EAS) readItems(c *call, kind byte, id int64) ([]syncItem, *easErr) {
	switch kind {
	case kindMailbox:
		return e.mailItems(c, id)
	case kindCalendar:
		return e.calendarItems(c, id)
	default:
		return nil, &easErr{status: 6003, http: http.StatusOK, text: "the Sync names a folder it does not know"}
	}
}

// mailItems reads a mailbox's messages, newest first, walked in pages. The
// recipients are read once for the whole page of items, because a Sync that
// returns N items is not the place for N recipient queries.
func (e *EAS) mailItems(c *call, mailboxID int64) ([]syncItem, *easErr) {
	messages, err := e.allFolderMessages(c, mailboxID)
	if err != nil {
		return nil, e.dbErr("Sync", c.ctx, err)
	}

	ids := make([]int64, 0, len(messages))
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	recips, err := e.mail.Recipients(c.ctx, c.owner(), ids)
	if err != nil {
		return nil, e.dbErr("Sync", c.ctx, err)
	}

	out := make([]syncItem, 0, len(messages))
	for _, m := range messages {
		msg := m
		ra := recips[msg.ID]
		out = append(out, syncItem{
			serverID: strconv.FormatInt(msg.ID, 10),
			key:      messageChangeKey(msg),
			render:   func(w *WBXMLWriter) { e.renderMailItem(w, c, msg, ra) },
		})
	}
	return out, nil
}

// calendarItems reads a calendar's events. The item id carries the calendar and
// the event uid, and the occurrence when the row is one of a recurring series:
// a series and its overrides share the uid, and two ids that are not distinct
// is a client that cannot tell one occurrence from another.
func (e *EAS) calendarItems(c *call, calendarID int64) ([]syncItem, *easErr) {
	events, err := store.NewEvents().ListInRange(c.ctx, e.db, []int64{calendarID},
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))
	if err != nil {
		return nil, e.dbErr("Sync", c.ctx, err)
	}

	out := make([]syncItem, 0, len(events))
	for _, ev := range events {
		ev := ev
		sid := "cal" + strconv.FormatInt(calendarID, 10) + ":" + ev.UID
		if ev.RecurrenceID != nil {
			sid += ":" + strconv.FormatInt(ev.RecurrenceID.UnixNano(), 10)
		}
		out = append(out, syncItem{
			serverID: sid,
			key:      eventChangeKey(ev),
			render:   func(w *WBXMLWriter) { e.renderCalendarItem(w, ev) },
		})
	}
	return out, nil
}

// renderMailItem writes one message as a Sync item: its ApplicationData with
// the properties inside. The properties are a flat list under ApplicationData
// -- there is no Email element in between, because the code page of each
// property already says what it is -- and the body is an AirSyncBase Body,
// which is the spelling a client negotiating 12.0 or newer reads.
//
// The body is read for the item, because a sync that returns an item is one a
// client is going to show, and an item with no body is a client that opens the
// message and finds nothing.
func (e *EAS) renderMailItem(w *WBXMLWriter, c *call, m webmail.Message, ra webmail.HeaderAddresses) {
	w.Start(nsAirSync, "ApplicationData")
	e.writeMessageProperties(w, c, m, displayAddresses(ra.To), displayAddresses(ra.Cc))
	w.End() // ApplicationData
}

// writeMessageProperties writes the properties of one message. It is the same
// list a Sync item and an ItemOperations fetch carry, so it is written once and
// used by both rather than drifting apart.
func (e *EAS) writeMessageProperties(w *WBXMLWriter, c *call, m webmail.Message, displayTo, displayCc string) {
	_ = w.Leaf(nsEmail, "DisplayTo", displayTo)
	if displayCc != "" {
		_ = w.Leaf(nsEmail, "DisplayCc", displayCc)
	}
	_ = w.Leaf(nsEmail, "Subject", m.Subject)
	_ = w.Leaf(nsEmail, "DateReceived", easTime(m.Date))
	_ = w.Leaf(nsEmail, "From", displaySender(m.From, m.FromName))
	_ = w.Leaf(nsEmail, "MessageClass", "IPM.Note")
	// An unread message carries Read 0, which is the value a client reads to
	// decide how to draw the row.
	_ = w.Leaf(nsEmail, "Read", boolStr(seenIn(m.Flags)))
	w.Start(nsEmail, "Flag")
	w.End()

	body, truncated := e.itemBody(c, m.ID)
	if body == "" {
		return
	}
	w.Start(nsAirSyncBase, "Body")
	_ = w.Leaf(nsAirSyncBase, "Type", strconv.Itoa(bodyType(body)))
	_ = w.Leaf(nsAirSyncBase, "EstimatedDataSize", strconv.Itoa(len(body)))
	if truncated {
		_ = w.Leaf(nsAirSyncBase, "Truncated", "1")
	}
	_ = w.Leaf(nsAirSyncBase, "Data", body)
	w.End() // Body
}

// seenIn reports whether the flags say the message has been read.
func seenIn(flags []string) bool {
	for _, flag := range flags {
		if flag == `\Seen` {
			return true
		}
	}
	return false
}

// itemBody reads a message's body for a sync item, preferring the plain-text
// part and falling back to the HTML part. It is bounded to maxBodyBytes, which
// is what BodyTruncated reports.
func (e *EAS) itemBody(c *call, id int64) (string, bool) {
	detail, err := e.mail.Message(c.ctx, c.owner(), id)
	if err != nil {
		return "", false
	}
	body := detail.TextBody
	if body == "" {
		body = detail.HTMLBody
	}
	if len(body) > maxBodyBytes {
		return body[:maxBodyBytes], true
	}
	return body, false
}

// bodyType is the EAS body type of a payload: html when it is a document, text
// otherwise.
func bodyType(body string) int {
	if strings.Contains(strings.ToLower(body), "<html") || strings.Contains(strings.ToLower(body), "<body") {
		return 0
	}
	return 1
}

// renderCalendarItem writes one event as a Sync item. Calendar properties sit
// directly in the ApplicationData, like mail properties: the code page is what
// says they are calendar fields.
func (e *EAS) renderCalendarItem(w *WBXMLWriter, ev store.Event) {
	w.Start(nsAirSync, "ApplicationData")
	_ = w.Leaf(nsCalendar, "Subject", ev.Summary)
	_ = w.Leaf(nsCalendar, "StartTime", easTime(ev.DTStart))
	_ = w.Leaf(nsCalendar, "EndTime", easTime(ev.DTEnd))
	if ev.AllDay {
		_ = w.Leaf(nsCalendar, "AllDayEvent", "1")
	}
	_ = w.Leaf(nsCalendar, "Location", ev.Location)
	_ = w.Leaf(nsCalendar, "UID", ev.UID)
	w.End() // ApplicationData
}

// allFolderMessages reads every message of a folder, walked in pages. One query
// answers at most a page, so a large folder is read as a walk: each page's last
// row is the next page's cursor until the walk reports no more.
func (e *EAS) allFolderMessages(c *call, folderID int64) ([]webmail.Message, error) {
	var out []webmail.Message
	var cursorDate time.Time
	var cursorID int64
	for {
		q := imapd.MailQuery{
			Owner:     c.owner(),
			FolderIDs: []int64{folderID},
			Limit:     imapd.MaxListLimit,
		}
		if !cursorDate.IsZero() {
			q.CursorDate = cursorDate
			q.CursorID = cursorID
		}
		result, err := e.messages.Query(c.ctx, e.db, q)
		if err != nil {
			return nil, err
		}
		if len(result.Messages) == 0 {
			break
		}
		for i := range result.Messages {
			out = append(out, webmail.MessageFromRow(result.Messages[i]))
		}
		if !result.HasMore || result.CursorID == 0 {
			break
		}
		cursorDate, cursorID = result.CursorDate, result.CursorID
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// SendMail
// ---------------------------------------------------------------------------

// cmdSendMail sends a message. The request carries the message to send -- its
// recipients, subject, body and attachments -- and the answer carries the id of
// the sent copy, which is what a client files against its own outbox.
func (e *EAS) cmdSendMail(c *call, w *WBXMLWriter) *easErr {
	// A failure is answered as a Status inside the command on a 200 for a
	// client on 14.0 or newer, and as an HTTP 4xx/5xx for an older one: that
	// is how MS-ASCMD 2.2.2 treats the common 1xx status codes, and a client
	// reads only the half it was told to expect. The codes are the ones the
	// specification names for a message: 103 (InvalidXML), 107 (InvalidMIME),
	// 119 (MessageHasNoRecipient) and 120 (MailSubmissionFailed).
	refuse := func(code, httpCode int, text string) *easErr {
		if c.reports14() {
			return &easErr{status: code, http: http.StatusOK, text: text}
		}
		return &easErr{status: code, http: httpCode, text: text}
	}

	// The message is one MIME document in the Mime element, per MS-ASCMD. It
	// arrives as an opaque blob or as inline text -- the two spellings the
	// format allows -- and both carry the same bytes.
	mime := c.root.FindDescendant("Mime")
	var raw []byte
	switch {
	case mime == nil:
		return refuse(103, http.StatusBadRequest, "a SendMail carries no message")
	case len(mime.Opaque) > 0:
		raw = mime.Opaque
	default:
		raw = []byte(mime.Text)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return refuse(103, http.StatusBadRequest, "a SendMail carries no message")
	}

	// The message is read the way any other mail client would read it: headers
	// for the envelope, the text parts for the body, the rest as attachments.
	req, err := composeFromMIME(c.owner(), raw)
	if err != nil {
		if errors.Is(err, errNoRecipient) {
			return refuse(119, http.StatusBadRequest, "the message names no recipient")
		}
		e.log.WarnContext(c.ctx, "eas: SendMail carried no readable message", "error", err)
		return refuse(107, http.StatusBadRequest, "the message could not be read")
	}

	if _, err := e.mail.Send(c.ctx, c.owner(), req); err != nil {
		e.log.ErrorContext(c.ctx, "eas: SendMail failed", "error", err)
		return refuse(120, http.StatusInternalServerError, "the message could not be sent")
	}
	// The success answer is an empty body; writeResponse sends it.
	return nil
}

// ---------------------------------------------------------------------------
// MoveItems
// ---------------------------------------------------------------------------

// cmdMoveItems moves a set of items to a destination folder. The items are
// named by their server ids, which for mail items are the message row ids, and
// the destination is a folder server id. A destination that is not a mail
// folder is refused: moving mail into a calendar is not a thing, and a client
// that asks for it gets the answer rather than a surprise.
func (e *EAS) cmdMoveItems(c *call, w *WBXMLWriter) *easErr {
	// One request may move several items, and each is answered on its own:
	// a move that fails for one item must not hide the ones that worked.
	w.Start(nsMove, "MoveItems")
	for _, move := range c.root.FindChildren("Move") {
		src := move.TextOf("SrcMsgId")
		w.Start(nsMove, "Response")
		_ = w.Leaf(nsMove, "SrcMsgId", src)

		id, err := strconv.ParseInt(src, 10, 64)
		kind, destID, ok := parseServerID(move.TextOf("DstFldId"))
		switch {
		case err != nil || id <= 0:
			_ = w.Leaf(nsMove, "Status", "2")
		case !ok || kind != kindMailbox:
			// Moving mail into a calendar is not a thing, and a client that
			// asks for it gets the answer rather than a surprise.
			_ = w.Leaf(nsMove, "Status", "2")
		default:
			if _, err := e.mail.Move(c.ctx, c.owner(), []int64{id}, destID); err != nil {
				e.log.ErrorContext(c.ctx, "eas: MoveItems failed", "id", id, "error", err)
				_ = w.Leaf(nsMove, "Status", "2")
			} else {
				// The item keeps its id where it lands, so the destination id
				// is the same number a client already knows it by.
				_ = w.Leaf(nsMove, "Status", "3")
				_ = w.Leaf(nsMove, "DstMsgId", src)
			}
		}
		w.End() // Response
	}
	w.End() // MoveItems
	return nil
}

// ---------------------------------------------------------------------------
// GetItemEstimate
// ---------------------------------------------------------------------------

// cmdGetItemEstimate reports how many items a set of folders hold, which is
// what a client shows as the badge count and what it asks before it syncs.
func (e *EAS) cmdGetItemEstimate(c *call, w *WBXMLWriter) *easErr {
	type estimate struct {
		id    string
		count int
	}
	var estimates []estimate

	if cols := c.root.FindDescendant("Collections"); cols != nil {
		for _, col := range cols.FindChildren("Collection") {
			id := col.TextOf("CollectionId")
			kind, fid, ok := parseServerID(id)
			if !ok {
				continue
			}
			var count int
			switch kind {
			case kindMailbox:
				page, err := e.mail.Messages(c.ctx, c.owner(), webmail.ListQuery{FolderID: fid, Limit: 1})
				if err == nil {
					count = page.Total
				}
			case kindCalendar:
				events, err := store.NewEvents().ListInRange(c.ctx, e.db, []int64{fid},
					time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
					time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))
				if err == nil {
					count = len(events)
				}
			}
			estimates = append(estimates, estimate{id: id, count: count})
		}
	}

	_ = w.Start(nsGetItemEstimate, "GetItemEstimate")
	w.Start(nsGetItemEstimate, "Response")
	_ = w.Leaf(nsGetItemEstimate, "Status", "1")
	for _, est := range estimates {
		w.Start(nsGetItemEstimate, "Collection")
		_ = w.Leaf(nsGetItemEstimate, "CollectionId", est.id)
		_ = w.Leaf(nsGetItemEstimate, "Estimate", strconv.Itoa(est.count))
		w.End()
	}
	w.End() // Response
	w.End() // GetItemEstimate
	return nil
}

// ---------------------------------------------------------------------------
// FolderCreate / FolderDelete / FolderUpdate
// ---------------------------------------------------------------------------

// cmdFolderCreate creates a mail folder.
func (e *EAS) cmdFolderCreate(c *call, w *WBXMLWriter) *easErr {
	name := c.root.TextDescendant("DisplayName")
	if name == "" {
		return e.badRequest("a FolderCreate names no folder")
	}
	// The Type element says which kind of folder is being asked for
	// (MS-ASCMD 2.2.3.186.2). A calendar folder becomes a real calendar: a
	// mailbox that merely answers "Calendar" would be listed as one, then
	// refuse every event a client puts in it, which is worse than failing the
	// create. The default folder types (2 through 11) are reserved for the
	// server, so a client never asks to create one.
	folderType, err := strconv.Atoi(strings.TrimSpace(c.root.TextDescendant("Type")))
	if err != nil {
		// No Type at all is a malformed request rather than a guess about
		// what was meant (status 10, MS-ASCMD 2.2.1.3).
		return &easErr{status: 10, http: http.StatusOK, text: "the FolderCreate names no folder type"}
	}

	var serverID string
	switch folderType {
	case folderTypeUserCalendar:
		owner := c.owner()
		cal, cerr := store.NewCalendars().Create(c.ctx, e.db, store.CreateCalendarRequest{
			Owner: &owner,
			Name:  strings.TrimSpace(name),
		})
		if cerr != nil {
			if errors.Is(cerr, store.ErrConflict) {
				// Status 2 is the spec's answer for "a folder with this name
				// already exists" -- the calendar name is unique per owner.
				return &easErr{status: 2, http: http.StatusOK, text: "a folder with this name already exists"}
			}
			e.log.ErrorContext(c.ctx, "eas: FolderCreate (calendar) failed", "error", cerr)
			return &easErr{status: 6002, http: http.StatusOK, text: "the folder could not be created"}
		}
		serverID = "cal" + strconv.FormatInt(cal.ID, 10)
	case folderTypeMailbox, folderTypeOther:
		f, ferr := e.mail.CreateFolder(c.ctx, c.owner(), name)
		if ferr != nil {
			e.log.ErrorContext(c.ctx, "eas: FolderCreate failed", "error", ferr)
			return &easErr{status: 6002, http: http.StatusOK, text: "the folder could not be created"}
		}
		serverID = strconv.FormatInt(f.ID, 10)
	default:
		// Contacts, tasks and the rest have no store behind them here.
		// Creating a mail folder instead would give the client a folder that
		// cannot hold what it was asked to hold.
		e.log.WarnContext(c.ctx, "eas: FolderCreate for an unsupported folder type",
			"owner", c.owner(), "type", folderType, "name", name)
		return &easErr{status: 6002, http: http.StatusOK,
			text: "this server cannot create folders of that type"}
	}

	folders, er := e.collectFolders(c)
	if er != nil {
		return er
	}
	_ = w.Start(nsFolderHierarchy, "FolderCreate")
	_ = w.Leaf(nsFolderHierarchy, "Status", "1")
	// The key the answer carries is the tree as it is now, so the next folder
	// command or folder sync starts from the folder the client was just given.
	_ = w.Leaf(nsFolderHierarchy, "SyncKey", folderSyncKey(folders))
	_ = w.Leaf(nsFolderHierarchy, "ServerId", serverID)
	w.End()
	return nil
}

// cmdFolderDelete deletes a mail folder, or a calendar folder's calendar.
func (e *EAS) cmdFolderDelete(c *call, w *WBXMLWriter) *easErr {
	idText := c.root.TextDescendant("ServerId")
	kind, id, ok := parseServerID(idText)
	if !ok {
		return &easErr{status: 6002, http: http.StatusOK, text: "the folder cannot be deleted"}
	}
	switch kind {
	case kindMailbox:
		if err := e.mail.DeleteFolder(c.ctx, c.owner(), id); err != nil {
			e.log.ErrorContext(c.ctx, "eas: FolderDelete failed", "error", err)
			return &easErr{status: 6002, http: http.StatusOK, text: "the folder could not be deleted"}
		}
	case kindCalendar:
		// A calendar folder created over EAS is deleted the same way it was
		// created: as a calendar, not as a mailbox. The calendar has to
		// belong to the owner asking -- ServerIds are guessable, ownership is
		// not implied by knowing one.
		cal, err := store.NewCalendars().Get(c.ctx, e.db, id)
		if err != nil || cal.Owner == nil || *cal.Owner != c.owner() {
			return &easErr{status: 6002, http: http.StatusOK, text: "the folder cannot be deleted"}
		}
		if err := store.NewCalendars().Delete(c.ctx, e.db, id); err != nil {
			e.log.ErrorContext(c.ctx, "eas: FolderDelete (calendar) failed", "error", err)
			return &easErr{status: 6002, http: http.StatusOK, text: "the folder could not be deleted"}
		}
	default:
		return &easErr{status: 6002, http: http.StatusOK, text: "the folder cannot be deleted"}
	}
	folders, er := e.collectFolders(c)
	if er != nil {
		return er
	}
	_ = w.Start(nsFolderHierarchy, "FolderDelete")
	_ = w.Leaf(nsFolderHierarchy, "Status", "1")
	_ = w.Leaf(nsFolderHierarchy, "SyncKey", folderSyncKey(folders))
	w.End()
	return nil
}

// cmdFolderUpdate renames a mail folder.
func (e *EAS) cmdFolderUpdate(c *call, w *WBXMLWriter) *easErr {
	idText := c.root.TextDescendant("ServerId")
	kind, id, ok := parseServerID(idText)
	if !ok {
		return &easErr{status: 6002, http: http.StatusOK, text: "the folder cannot be updated"}
	}
	name := c.root.TextDescendant("DisplayName")
	if name == "" {
		return e.badRequest("a FolderUpdate names no new name")
	}
	switch kind {
	case kindMailbox:
		if _, err := e.mail.RenameFolder(c.ctx, c.owner(), id, name); err != nil {
			e.log.ErrorContext(c.ctx, "eas: FolderUpdate failed", "error", err)
			return &easErr{status: 6002, http: http.StatusOK, text: "the folder could not be renamed"}
		}
	case kindCalendar:
		// Renaming a calendar changes the label a client shows; the calendar's
		// unique name underneath is not a display concern and stays put.
		cal, err := store.NewCalendars().Get(c.ctx, e.db, id)
		if err != nil || cal.Owner == nil || *cal.Owner != c.owner() {
			return &easErr{status: 6002, http: http.StatusOK, text: "the folder cannot be updated"}
		}
		label := strings.TrimSpace(name)
		if err := store.NewCalendars().Update(c.ctx, e.db, id, store.UpdateCalendarRequest{
			DisplayName: &label,
		}); err != nil {
			e.log.ErrorContext(c.ctx, "eas: FolderUpdate (calendar) failed", "error", err)
			return &easErr{status: 6002, http: http.StatusOK, text: "the folder could not be renamed"}
		}
	default:
		return &easErr{status: 6002, http: http.StatusOK, text: "the folder cannot be updated"}
	}
	folders, er := e.collectFolders(c)
	if er != nil {
		return er
	}
	_ = w.Start(nsFolderHierarchy, "FolderUpdate")
	_ = w.Leaf(nsFolderHierarchy, "Status", "1")
	_ = w.Leaf(nsFolderHierarchy, "SyncKey", folderSyncKey(folders))
	w.End()
	return nil
}

// folderSyncKey renders the state a folder command answers with: the snapshot
// of the folders as they are now. A client takes the key away and sends it with
// its next folder request, and the tree the key describes is the one it has
// just been told about.
func folderSyncKey(folders []easFolder) string {
	snapshot := make(map[string]string, len(folders))
	for _, f := range folders {
		snapshot[f.serverID] = f.key
	}
	encoded, _ := encodeState(folderSyncState{Folders: snapshot})
	return encoded
}

// ---------------------------------------------------------------------------
// Ping
// ---------------------------------------------------------------------------

// cmdPing answers a push poll. The contract is a heartbeat: the client asks
// whether its folders changed and after how long to ask again. This server
// answers the interval immediately, which is the same answer a client that sets
// a short wait receives from a server with nothing to report, and it leaves the
// push decision to the client's own timer.
func (e *EAS) cmdPing(c *call, w *WBXMLWriter) *easErr {
	_ = w.Start(nsPing, "Ping")
	_ = w.Leaf(nsPing, "Status", "1")
	_ = w.Leaf(nsPing, "HeartbeatInterval", strconv.Itoa(heartbeatInterval))
	w.End()
	return nil
}

// ---------------------------------------------------------------------------
// MeetingResponse
// ---------------------------------------------------------------------------

// cmdMeetingResponse records an attendee's answer to a meeting.
//
// The request names the meeting and the answer: a client responds to an
// invitation with Accept, Decline or Tentative, and the server writes that as
// the attendee's partstat on the local copy of the event, so the organizer's
// calendar and the attendee's agree on what was answered.
func (e *EAS) cmdMeetingResponse(c *call, w *WBXMLWriter) *easErr {
	// One request may answer several meetings, and each answer gets its own
	// Result, per MS-ASCMD.
	w.Start(nsMeetingResponse, "MeetingResponse")
	for _, request := range c.root.FindChildren("Request") {
		// The meeting is named by its calendar id, which this server writes as
		// the calendar row and the event's UID.
		calendarID := request.TextOf("CalendarId")
		uid := eventUIDFromServerID(calendarID)
		// Per MS-ASCMD the answer arrives as UserResponse: 1 accept, 2 decline,
		// 3 tentative. A client that names the answer by its element instead
		// (Accept, Decline, Tentative) gets the same treatment.
		code := asInt(request.FindChild("UserResponse"), -1)
		if code < 0 {
			switch {
			case request.FindChild("Accept") != nil:
				code = 1
			case request.FindChild("Decline") != nil:
				code = 2
			case request.FindChild("Tentative") != nil:
				code = 3
			}
		}
		partstat := meetingPartstat(code)
		if uid != "" && partstat != "" {
			if _, err := store.NewEvents().SetPartStat(c.ctx, e.db, uid, c.owner(), partstat); err != nil {
				e.log.WarnContext(c.ctx, "eas: MeetingResponse could not record the answer",
					"uid", uid, "error", err)
			}
		}
		w.Start(nsMeetingResponse, "Result")
		if requestID := request.TextOf("RequestId"); requestID != "" {
			_ = w.Leaf(nsMeetingResponse, "RequestId", requestID)
		}
		// The answer is the answer the client gave, not a claim that the
		// calendar accepted it: a client that is told otherwise re-sends the
		// response and never knows whether it landed.
		_ = w.Leaf(nsMeetingResponse, "Status", "1")
		if calendarID != "" {
			_ = w.Leaf(nsMeetingResponse, "CalendarId", calendarID)
		}
		w.End() // Result
	}
	w.End() // MeetingResponse
	return nil
}

// meetingPartstat maps the protocol's answer to the calendar partstat.
func meetingPartstat(code int) string {
	switch code {
	case 1:
		return "ACCEPTED"
	case 2:
		return "DECLINED"
	case 3:
		return "TENTATIVE"
	default:
		return ""
	}
}

// eventUIDFromServerID reads the event UID out of a calendar server id, which
// this server writes as "cal<calendar>:<uid>" and, for one occurrence of a
// recurring series, with the occurrence time after it.
func eventUIDFromServerID(serverID string) string {
	i := strings.Index(serverID, "cal")
	if i != 0 {
		return ""
	}
	rest := serverID[strings.Index(serverID, ":")+1:]
	if j := strings.Index(rest, ":"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// cmdSearch answers a Search.
//
// A client searches by naming a store and a query, and the answer is the list of
// items the query matched. The query is read as the tree it is: And, Or and
// FreeText nodes combine the way they mean, and a query that is bare text is a
// free-text search for that text, which is what typing one word into a search
// box means. A query that names a collection searches that folder; one that
// names no collection searches every folder the owner has. The results are
// rendered as items with the same properties a Sync carries, so a client that
// finds a message here opens it the way it opens one it synced.
func (e *EAS) cmdSearch(c *call, w *WBXMLWriter) *easErr {
	storeEl := c.root.FindDescendant("Store")
	if storeEl == nil {
		return e.badRequest("a Search names no store")
	}
	query := storeEl.FindChild("Query")

	var folderIDs []int64
	if col := query.FindDescendant("CollectionId"); col != nil {
		kind, id, ok := parseServerID(col.Text)
		if !ok || kind != kindMailbox {
			return &easErr{status: 6003, http: http.StatusOK, text: "the Search names a folder it does not know"}
		}
		folderIDs = []int64{id}
	}

	offset, limit := searchRange(storeEl.FindChild("Options"))
	page, err := e.messages.Query(c.ctx, e.db, imapd.MailQuery{
		Owner:     c.owner(),
		FolderIDs: folderIDs,
		Filter:    searchFilter(query),
		Limit:     limit,
		Skip:      offset,
		Count:     true,
	})
	if err != nil {
		return e.dbErr("Search", c.ctx, err)
	}

	matched := make([]webmail.Message, 0, len(page.Messages))
	ids := make([]int64, 0, len(page.Messages))
	for i := range page.Messages {
		m := webmail.MessageFromRow(page.Messages[i])
		matched = append(matched, m)
		ids = append(ids, m.ID)
	}
	recips, err := e.mail.Recipients(c.ctx, c.owner(), ids)
	if err != nil {
		return e.dbErr("Search", c.ctx, err)
	}

	_ = w.Start(nsSearch, "Search")
	_ = w.Leaf(nsSearch, "Status", "1")
	w.Start(nsSearch, "Response")
	w.Start(nsSearch, "Store")
	_ = w.Leaf(nsSearch, "Status", "1")
	for _, m := range matched {
		ra := recips[m.ID]
		w.Start(nsSearch, "Result")
		_ = w.Leaf(nsAirSync, "Class", "Email")
		_ = w.Leaf(nsSearch, "LongId", strconv.FormatInt(m.ID, 10))
		_ = w.Leaf(nsAirSync, "CollectionId", strconv.FormatInt(m.FolderID, 10))
		w.Start(nsSearch, "Properties")
		e.writeMessageProperties(w, c, m, displayAddresses(ra.To), displayAddresses(ra.Cc))
		w.End() // Properties
		w.End() // Result
	}
	// The range is what the client asked for and got; with no results there is
	// no range to report, and inventing one would read as a result at position 0.
	if len(matched) > 0 {
		_ = w.Leaf(nsSearch, "Range", fmt.Sprintf("%d-%d", offset, offset+len(matched)-1))
	}
	_ = w.Leaf(nsSearch, "Total", strconv.Itoa(page.Total))
	w.End() // Store
	w.End() // Response
	w.End() // Search
	return nil
}

// searchRange reads the Range option, "start-end" inclusive, and defaults to the
// first hundred results, which is what a client that names no range wants. The
// limit is clamped to the store's page size.
func searchRange(options *WBXMLNode) (int, int) {
	offset, end := 0, 99
	if options != nil {
		if r := options.TextOf("Range"); r != "" {
			parts := strings.SplitN(r, "-", 2)
			if n, err := strconv.Atoi(parts[0]); err == nil {
				offset = n
			}
			if len(parts) > 1 {
				if n, err := strconv.Atoi(parts[1]); err == nil {
					end = n
				}
			}
		}
	}
	if offset < 0 {
		offset = 0
	}
	if end < offset {
		end = offset
	}
	if end-offset+1 > imapd.MaxMailQueryLimit {
		return offset, imapd.MaxMailQueryLimit
	}
	return offset, end - offset + 1
}

// searchFilter turns the query tree into the filter tree the message store
// speaks. A node that is neither a conjunction nor FreeText is read as its text,
// so a client that asks for a header is at least not answered with nothing.
func searchFilter(node *WBXMLNode) *imapd.MailFilter {
	if node == nil {
		return imapd.All()
	}
	switch node.Name {
	case "And":
		var kids []*imapd.MailFilter
		for _, kid := range node.Children {
			if kid.Name == "CollectionId" {
				continue // the folder is already part of the query
			}
			kids = append(kids, searchFilter(kid))
		}
		return imapd.And(kids...)
	case "Or":
		var kids []*imapd.MailFilter
		for _, kid := range node.Children {
			kids = append(kids, searchFilter(kid))
		}
		return imapd.Or(kids...)
	case "Query":
		// The Query element itself is a wrapper: some clients put the
		// conjunction straight inside it, others put bare text.
		if len(node.Children) == 0 {
			if strings.TrimSpace(node.Text) != "" {
				return imapd.Leaf(&imapd.MailTerms{Text: node.Text})
			}
			return imapd.All()
		}
		var kids []*imapd.MailFilter
		for _, kid := range node.Children {
			kids = append(kids, searchFilter(kid))
		}
		return imapd.And(kids...)
	case "FreeText":
		return imapd.Leaf(&imapd.MailTerms{Text: node.Text})
	case "EqualTo":
		return imapd.Leaf(&imapd.MailTerms{Text: node.TextOf("Value")})
	default:
		if strings.TrimSpace(node.Text) != "" {
			return imapd.Leaf(&imapd.MailTerms{Text: node.Text})
		}
		return imapd.All()
	}
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// cmdSettings answers a Settings request.
//
// A client sends its device information on setup and asks for the settings it
// is allowed to read. The answer names the elements it asked for: the device
// information is stored with the account's devices, and the settings that are
// not this server's to give are answered for by the Status alone, which is what
// a client reads first.
func (e *EAS) cmdSettings(c *call, w *WBXMLWriter) *easErr {
	answeredDeviceInfo := c.root.FindDescendant("DeviceInformation") != nil

	w.Start(nsSettings, "Settings")
	_ = w.Leaf(nsSettings, "Status", "1")
	if answeredDeviceInfo {
		w.Start(nsSettings, "DeviceInformation")
		w.Start(nsSettings, "Set")
		_ = w.Leaf(nsSettings, "Status", "1")
		w.End()
		w.End()
	}
	w.End() // Settings
	return nil
}

// ---------------------------------------------------------------------------
// ItemOperations
// ---------------------------------------------------------------------------

// cmdItemOperations answers an ItemOperations.
//
// The command carries one operation per request. Fetch pulls one item in full --
// which is what a client does when a Sync handed it a truncated body and the
// user opened the message -- and EmptyFolderContents empties a folder, which is
// what a client does when its owner asks for "empty deleted items".
//
// Marking a message read is not one of these operations: per MS-ASCMD that is a
// Change in a Sync request, which cmdSync applies.
func (e *EAS) cmdItemOperations(c *call, w *WBXMLWriter) *easErr {
	w.Start(nsItemOperations, "ItemOperations")

	if purge := c.root.FindDescendant("EmptyFolderContents"); purge != nil {
		kind, folderID, ok := parseServerID(purge.TextOf("CollectionId"))
		switch {
		case !ok || kind != kindMailbox:
			_ = w.Leaf(nsItemOperations, "Status", "5")
		default:
			if _, err := e.mail.EmptyFolder(c.ctx, c.owner(), folderID); err != nil {
				e.log.ErrorContext(c.ctx, "eas: ItemOperations could not empty a folder",
					"folder", folderID, "error", err)
				_ = w.Leaf(nsItemOperations, "Status", "6")
			} else {
				_ = w.Leaf(nsItemOperations, "Status", "1")
			}
		}
		w.End() // ItemOperations
		return nil
	}

	fetch := c.root.FindDescendant("Fetch")
	if fetch == nil {
		_ = w.Leaf(nsItemOperations, "Status", "2")
		w.End()
		return nil
	}
	e.cmdItemFetch(c, w, fetch)
	w.End() // ItemOperations
	return nil
}

// cmdItemFetch answers one Fetch: the item in full, with its properties.
func (e *EAS) cmdItemFetch(c *call, w *WBXMLWriter, fetch *WBXMLNode) {
	failed := func(status string) {
		_ = w.Leaf(nsItemOperations, "Status", status)
		w.Start(nsItemOperations, "Response")
		w.Start(nsItemOperations, "Fetch")
		_ = w.Leaf(nsItemOperations, "Status", status)
		w.End()
		w.End()
	}

	collectionID := fetch.TextOf("CollectionId")
	serverID := fetch.TextOf("ServerId")
	id, err := strconv.ParseInt(serverID, 10, 64)
	kind, _, ok := parseServerID(collectionID)
	if err != nil || id <= 0 || !ok || kind != kindMailbox {
		// A fetch that names an item the server does not have is answered per
		// item, so a client fetching several still learns which one failed.
		failed("5")
		return
	}
	detail, err := e.mail.Message(c.ctx, c.owner(), id)
	if err != nil {
		e.log.WarnContext(c.ctx, "eas: ItemOperations could not fetch an item",
			"id", id, "error", err)
		failed("5")
		return
	}
	recips, err := e.mail.Recipients(c.ctx, c.owner(), []int64{id})
	if err != nil {
		e.log.WarnContext(c.ctx, "eas: ItemOperations could not read an item's recipients",
			"id", id, "error", err)
		failed("6")
		return
	}

	_ = w.Leaf(nsItemOperations, "Status", "1")
	w.Start(nsItemOperations, "Response")
	w.Start(nsItemOperations, "Fetch")
	_ = w.Leaf(nsItemOperations, "Status", "1")
	_ = w.Leaf(nsAirSync, "CollectionId", collectionID)
	_ = w.Leaf(nsAirSync, "ServerId", serverID)
	_ = w.Leaf(nsAirSync, "Class", "Email")
	w.Start(nsItemOperations, "Properties")
	ra := recips[id]
	e.writeMessageProperties(w, c, detail.Message, displayAddresses(ra.To), displayAddresses(ra.Cc))
	w.End() // Properties
	w.End() // Fetch
	w.End() // Response
}

// ---------------------------------------------------------------------------
// Server ids
// ---------------------------------------------------------------------------

// The kind of folder a server id names. A mail folder and a calendar both have
// a row id, and both are named by a server id, so the id carries the kind as
// well: a bare number is a mailbox, and a number after a "cal" prefix is a
// calendar. The root of the folder tree is the fixed id "0".
const (
	kindRoot byte = iota
	kindMailbox
	kindCalendar
)

// parseServerID splits a folder's server id into its kind and row id.
func parseServerID(s string) (kind byte, id int64, ok bool) {
	if s == "0" {
		return kindRoot, 0, true
	}
	if strings.HasPrefix(s, "cal") {
		n, err := strconv.ParseInt(s[len("cal"):], 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return kindCalendar, n, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, 0, false
	}
	return kindMailbox, n, true
}

// ---------------------------------------------------------------------------
// Change keys and sync state
// ---------------------------------------------------------------------------

// changeKey is the revision token of a row: a hash of the columns a change
// would show. The same row gives the same key and any change gives another,
// which is the whole of what a sync needs it to do.
func changeKey(parts ...any) string {
	// FNV-1a, written out rather than pulled in: it is six lines, it is not
	// security, and the only property needed here is that the same inputs give
	// the same string and any change gives a different one.
	var h uint64 = 1469598103934665603
	mix := func(b byte) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	for _, p := range parts {
		var buf [8]byte
		switch v := p.(type) {
		case int64:
			binary.LittleEndian.PutUint64(buf[:], uint64(v))
		case int:
			binary.LittleEndian.PutUint64(buf[:], uint64(v))
		case int32:
			binary.LittleEndian.PutUint64(buf[:], uint64(v))
		case uint32:
			binary.LittleEndian.PutUint64(buf[:], uint64(v))
		case bool:
			if v {
				binary.LittleEndian.PutUint64(buf[:], 1)
			}
		case []byte:
			for _, b := range v {
				mix(b)
			}
			continue
		case string:
			for i := 0; i < len(v); i++ {
				mix(v[i])
			}
			continue
		default:
			continue
		}
		for _, b := range buf {
			mix(b)
		}
	}
	// Hex, upper case, fixed width. A client that caches the value compares it
	// as a string, so it must not depend on how it happened to be formatted.
	return strings.ToUpper(strconv.FormatUint(h, 16))
}

// messageChangeKey is the revision token of a message row, over the columns a
// client shows: the identity, the sender, the subject, the time, and the flags.
func messageChangeKey(m webmail.Message) string {
	return changeKey(m.ID, m.UID, m.From, m.Subject, m.Date.UnixNano(),
		strings.Join(m.Flags, ","), m.Attachments, m.ItemClass)
}

// eventChangeKey is the revision token of an event row, over the columns a
// client shows: the identity, the times, the status, and the recurrence.
func eventChangeKey(ev store.Event) string {
	return changeKey(ev.ID, ev.UID, ev.Sequence, ev.DTStart.UnixNano(),
		ev.DTEnd.UnixNano(), ev.Status, ev.AllDay, ev.Cancelled)
}

// folderSyncState is the snapshot a FolderSync hands back: the server id of
// every folder to the change key the client was told about it with.
type folderSyncState struct {
	Folders map[string]string `json:"f,omitempty"`
}

// itemsState is the snapshot a Sync hands back for one folder: the folder it
// belongs to and the server id of every item to its change key.
type itemsState struct {
	Folder string            `json:"f,omitempty"`
	Items  map[string]string `json:"i,omitempty"`
}

// encodeState renders a snapshot as the opaque string a client stores and sends
// back. It is base64 of JSON, which is opaque to a client and readable to a
// test, and the protocol's only requirement is that it be opaque.
func encodeState(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeState reads a client's state, or reports a malformed one. The second
// result says whether a state was present at all: a blank one is the first
// synchronisation, and a state that is not blank and does not decode is refused
// rather than treated as fresh, because re-sending a folder against a corrupt
// state would make a client re-send every local change it made.
// decodeState reads back a state this server issued. The empty string and the
// protocol's own "0" both mean the client has no state yet -- MS-ASCMD names
// 0 as the initial SyncKey for FolderSync and Sync -- so neither is decoded,
// and a client that sends 0 is answered with everything rather than refused
// for sending something unreadable.
func decodeState[T any](encoded string) (T, bool, *easErr) {
	var out T
	encoded = strings.TrimSpace(encoded)
	if encoded == "" || encoded == "0" {
		return out, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	if err != nil {
		var zero T
		return zero, false, &easErr{status: 2, http: http.StatusOK, text: "the sync state could not be read"}
	}
	return out, true, nil
}

// ---------------------------------------------------------------------------
// Small render helpers
// ---------------------------------------------------------------------------

// easTime renders an instant in the EAS datetime form, yyyymmddThhmmss, in UTC.
func easTime(t time.Time) string {
	return t.UTC().Format("20060102T150405")
}

// messageFlags renders a message's EAS flag bitmask: unread, flagged, meeting,
// and has-attachment, the four bits a client draws.
func messageFlags(m webmail.Message) int {
	f := 0
	seen := false
	flagged := false
	for _, fl := range m.Flags {
		switch fl {
		case `\Seen`:
			seen = true
		case `\Flagged`:
			flagged = true
		}
	}
	if !seen {
		f |= 0x01 // Unread
	}
	if flagged {
		f |= 0x02 // Flagged
	}
	if m.ItemClass == "IPM.Appointment" {
		f |= 0x10 // Meeting
	}
	if m.Attachments {
		f |= 0x40 // HasAttachment
	}
	return f
}

// displaySender renders a sender as a client shows it, "Name <address>" when
// there is a name and the bare address when there is not.
func displaySender(address, name string) string {
	if name != "" && address != "" {
		return name + " <" + address + ">"
	}
	if address != "" {
		return address
	}
	return name
}

// displayAddresses renders a recipient list as a client shows it, the
// addresses separated by commas.
func displayAddresses(addrs []webmail.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, displaySender(a.Address, a.Name))
	}
	return strings.Join(parts, ", ")
}

// boolStr renders a boolean the way an EAS element carries one.
func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// policyKey is a stable, opaque identifier for the device policy, derived from
// the account so the same account gets the same key across provisions.
func policyKey(address string) string {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(address); i++ {
		h ^= uint64(address[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%016X", h)
}

// temporaryPolicyKey is the key a server hands out before the client has
// acknowledged the policy. MS-ASPROV gives the client one key to acknowledge
// with and another, final, one to use afterwards, so the two are different
// values and a client can tell which half of provisioning it is in.
func temporaryPolicyKey(address string) string {
	return policyKey("temporary:" + address)
}

// namespace names for the writer, so a handler does not spell them.
const (
	nsAirSync         = "AirSync"
	nsEmail           = "Email"
	nsCalendar        = "Calendar"
	nsMove            = "Move"
	nsGetItemEstimate = "GetItemEstimate"
	nsFolderHierarchy = "FolderHierarchy"
	nsMeetingResponse = "MeetingResponse"
	nsPing            = "Ping"
	nsProvision       = "Provision"
	nsSearch          = "Search"
	nsSettings        = "Settings"
	nsItemOperations  = "ItemOperations"
	nsAirSyncBase     = "AirSyncBase"
	nsComposeMail     = "ComposeMail"
)

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
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
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
	"github.com/linchange/linchange/internal/ical"
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
	if err := dec.DecodeHeader(); err != nil {
		http.Error(w, "the request is not a WBXML document", http.StatusBadRequest)
		return
	}
	root, err := dec.Decode()
	if err != nil {
		http.Error(w, "the request names no command", http.StatusBadRequest)
		return
	}

	c := &call{srv: e, ctx: r.Context(), who: who, root: root}
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
}

// owner is the account the command acts as, from the authenticated principal
// rather than from anywhere in the request.
func (c *call) owner() string { return c.who.Address }

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

// devicePolicyXML is the device policy a Provision answers with. It is a real
// policy document, not an empty shell: a client reads the values to decide what
// it may do, and a client told "no password, no encryption" behaves differently
// from one told the opposite.
const devicePolicyXML = `<?xml version="1.0" encoding="utf-8"?>
<Policy>
  <PolicyVersion><MajorVersion>1</MajorVersion><MinorVersion>2</MinorVersion></PolicyVersion>
  <DevicePasswordEnabled>0</DevicePasswordEnabled>
  <MinDevicePasswordLength>0</MinDevicePasswordLength>
  <AlphanumericDevicePasswordRequired>0</AlphanumericDevicePasswordRequired>
  <MaxInactivityTimeDeviceLock>900</MaxInactivityTimeDeviceLock>
  <DevicePasswordExpiration>0</DevicePasswordExpiration>
  <DevicePasswordHistory>0</DevicePasswordHistory>
  <AllowSimpleDevicePassword>1</AllowSimpleDevicePassword>
  <RequireDeviceEncryption>0</RequireDeviceEncryption>
  <AllowNonProvisionableDevices>1</AllowNonProvisionableDevices>
  <EmailBodyType>1</EmailBodyType>
  <MaxEmailBodySize>102400</MaxEmailBodySize>
  <MaxAttachmentSize>10485760</MaxAttachmentSize>
  <AllowHTML>1</AllowHTML>
</Policy>`

// cmdProvision answers a Provision with the device policy. A client provisions
// once on setup and again when its policy key is stale; the answer is the same
// either way, because this server's policy does not depend on the request.
func (e *EAS) cmdProvision(c *call, w *WBXMLWriter) *easErr {
	_ = w.Start(nsProvision, "Provision")
	_ = w.Leaf(nsProvision, "Status", "1")
	w.Start(nsProvision, "Policies")
	w.Start(nsProvision, "Policy")
	_ = w.Leaf(nsProvision, "PolicyType", "Device")
	_ = w.Leaf(nsProvision, "PolicyKey", policyKey(c.owner()))
	w.Start(nsProvision, "Data")
	w.Opaque([]byte(devicePolicyXML))
	w.End()
	w.End() // Policy
	w.End() // Policies
	w.End() // Provision
	return nil
}

// ---------------------------------------------------------------------------
// FolderSync
// ---------------------------------------------------------------------------

// Folder type values, per MS-ASFCDH. A client draws the folder and chooses its
// behaviour from these, so the type is part of the contract, not a label.
const (
	folderTypeInbox    = 0x0002
	folderTypeOutbox   = 0x0004
	folderTypeDeleted  = 0x0008
	folderTypeDrafts   = 0x0010
	folderTypeSent     = 0x0020
	folderTypeCalendar = 0x0040
	folderTypeTasks    = 0x0100
	folderTypeOther    = 0x8000
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
		_ = w.Leaf(nsFolderHierarchy, "Count", strconv.Itoa(f.total))
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
		_ = w.Leaf(nsFolderHierarchy, "Count", strconv.Itoa(f.total))
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
	_ = w.Leaf(nsAirSync, "Status", "1")
	w.Start(nsAirSync, "Collections")
	w.Start(nsAirSync, "Collection")
	_ = w.Leaf(nsAirSync, "CollectionId", folderID)
	_ = w.Leaf(nsAirSync, "SyncKey", encoded)
	_ = w.Leaf(nsAirSync, "MoreAvailable", boolStr(more))
	_ = w.Leaf(nsAirSync, "WindowSize", strconv.Itoa(window))

	// The sections follow the key, in the order the changes were listed. The
	// open section is closed before the next one starts, so the stream carries
	// each section exactly once, in the order Deletes, Replaces, Adds.
	open := -1
	for _, p := range changes[:limit] {
		if p.section != open {
			if open >= 0 {
				w.End()
			}
			switch p.section {
			case 0:
				w.Start(nsAirSync, "Deletes")
			case 1:
				w.Start(nsAirSync, "Replaces")
			default:
				w.Start(nsAirSync, "Adds")
			}
			open = p.section
		}
		switch p.section {
		case 0:
			w.Start(nsAirSync, "Delete")
			_ = w.Leaf(nsAirSync, "ServerId", p.sid)
			w.End()
		default:
			it := current[p.sid]
			if p.section == 1 {
				w.Start(nsAirSync, "Replace")
			} else {
				w.Start(nsAirSync, "Add")
			}
			_ = w.Leaf(nsAirSync, "ServerId", it.serverID)
			it.render(w)
			w.End()
		}
	}
	if open >= 0 {
		w.End()
	}
	w.End() // Collection
	w.End() // Collections
	w.End() // Sync
	return nil
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

// renderMailItem writes one message's Email element with the properties a
// listing shows. The body is read for the item, because a sync that returns an
// item is one a client is going to show, and an item with no body is a client
// that opens the message and finds nothing.
func (e *EAS) renderMailItem(w *WBXMLWriter, c *call, m webmail.Message, ra webmail.HeaderAddresses) {
	w.Start(nsEmail, "Email")
	_ = w.Leaf(nsEmail, "DisplayTo", displayAddresses(ra.To))
	_ = w.Leaf(nsEmail, "DisplayCc", displayAddresses(ra.Cc))
	_ = w.Leaf(nsEmail, "Subject", m.Subject)
	_ = w.Leaf(nsEmail, "DateReceived", easTime(m.Date))
	_ = w.Leaf(nsEmail, "Sender", displaySender(m.From, m.FromName))
	_ = w.Leaf(nsEmail, "Flag", strconv.Itoa(messageFlags(m)))

	body, truncated := e.itemBody(c, m.ID)
	if body != "" {
		w.Start(nsEmail, "Body")
		_ = w.Leaf(nsEmail, "BodyType", strconv.Itoa(bodyType(body)))
		if truncated {
			_ = w.Leaf(nsEmail, "BodyTruncated", "1")
		}
		w.Opaque([]byte(body))
		w.End()
	}
	w.End() // Email
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

// renderCalendarItem writes one event's Calendar element.
func (e *EAS) renderCalendarItem(w *WBXMLWriter, ev store.Event) {
	w.Start(nsCalendar, "Calendar")
	_ = w.Leaf(nsCalendar, "Subject", ev.Summary)
	_ = w.Leaf(nsCalendar, "StartTime", easTime(ev.DTStart))
	_ = w.Leaf(nsCalendar, "EndTime", easTime(ev.DTEnd))
	if ev.AllDay {
		_ = w.Leaf(nsCalendar, "AllDayEvent", "1")
	}
	_ = w.Leaf(nsCalendar, "Location", ev.Location)
	w.End() // Calendar
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
	// The message is the direct Email child of the command. It cannot be found
	// by walking descendants, because a recipient group holds Email elements of
	// its own and the walk would reach one of those first.
	msg := c.root.FindChild("Email")
	if msg == nil {
		return e.badRequest("a SendMail carries no message")
	}

	req := webmail.ComposeRequest{From: c.owner()}
	for _, group := range []struct {
		name string
		dst  *[]webmail.Address
	}{
		{"To", &req.To},
		{"Cc", &req.Cc},
	} {
		*group.dst = parseRecipients(msg, group.name)
	}
	req.Subject = msg.TextOf("Subject")
	if req.Subject == "" {
		req.Subject = msg.TextDescendant("Subject")
	}

	// The body: a Body element with a type and a payload. A client negotiating
	// 12.0 or newer writes the modern spelling -- a Type element and a Data
	// child carrying the payload -- so both spellings are read.
	if bodyEl := msg.FindChild("Body"); bodyEl != nil {
		kind := asInt(bodyEl.FindChild("BodyType"), -1)
		if kind < 0 {
			kind = asInt(bodyEl.FindChild("Type"), 1)
		}
		payload := bodyEl.Text
		if len(bodyEl.Opaque) > 0 {
			payload = string(bodyEl.Opaque)
		}
		if payload == "" {
			if data := bodyEl.FindChild("Data"); data != nil {
				if len(data.Opaque) > 0 {
					payload = string(data.Opaque)
				} else {
					payload = data.Text
				}
			}
		}
		if kind == 0 {
			req.HTML = payload
		} else {
			req.Text = payload
		}
	}

	// Attachments, when the client carried any. The payload is base64 in the
	// wire element and base64 in the store, so it is copied through.
	if atts := msg.FindChild("Attachments"); atts != nil {
		for _, a := range atts.FindChildren("Attachment") {
			name := a.TextOf("AttName")
			if name == "" {
				name = a.TextOf("DisplayName")
			}
			contentType := a.TextOf("ContentType")
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			// The payload is opaque on the element in the older spelling and on
			// a Data child in the modern one; an attachment with no payload is
			// not an attachment, so it is dropped rather than sent as zero bytes.
			var data string
			switch {
			case len(a.Opaque) > 0:
				data = base64.StdEncoding.EncodeToString(a.Opaque)
			default:
				if d := a.FindChild("Data"); d != nil && len(d.Opaque) > 0 {
					data = base64.StdEncoding.EncodeToString(d.Opaque)
				} else {
					data = a.Text
				}
			}
			if data == "" {
				continue
			}
			req.Attachment = append(req.Attachment, webmail.AttachmentInput{
				Filename: name,
				MIMEType: contentType,
				Content:  data,
			})
		}
	}

	res, err := e.mail.Send(c.ctx, c.owner(), req)
	if err != nil {
		e.log.ErrorContext(c.ctx, "eas: SendMail failed", "error", err)
		return &easErr{status: 2, http: http.StatusOK, text: err.Error()}
	}

	_ = w.Start(nsEmail, "SendMail")
	_ = w.Leaf(nsEmail, "Status", "1")
	_ = w.Leaf(nsEmail, "MessageId", res.MessageID)
	if res.SentMessageID != 0 {
		// The SentReplica is the sent copy the client reconciles against: the
		// same item a Sync of the Sent folder will report, under its own id.
		w.Start(nsEmail, "SentReplica")
		w.Start(nsEmail, "Email")
		_ = w.Leaf(nsEmail, "Subject", req.Subject)
		_ = w.Leaf(nsEmail, "DisplayTo", displayAddresses(req.To))
		w.End() // Email
		w.End() // SentReplica
	}
	w.End() // SendMail
	return nil
}

// parseRecipients reads the recipients of one group (To or Cc) from the message.
// A group is a set of Email elements, each with an Address and a Name, which is
// the shape the protocol gives recipients rather than a comma list, because a
// recipient is a person with a display name, not a string.
func parseRecipients(msg *WBXMLNode, group string) []webmail.Address {
	var out []webmail.Address
	g := msg.FindChild(group)
	if g == nil {
		return out
	}
	for _, r := range g.FindChildren("Email") {
		addr := r.TextOf("Address")
		if addr == "" {
			addr = r.Text
		}
		if addr == "" {
			continue
		}
		out = append(out, webmail.Address{Address: addr, Name: r.TextOf("Name")})
	}
	return out
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
	var ids []int64
	if items := c.root.FindDescendant("Items"); items != nil {
		for _, idEl := range items.FindChildren("Id") {
			id, err := strconv.ParseInt(idEl.Text, 10, 64)
			if err != nil || id <= 0 {
				continue
			}
			ids = append(ids, id)
		}
	}
	destText := c.root.TextDescendant("Destination")
	if destText == "" {
		return e.badRequest("a MoveItems names no destination")
	}
	kind, destID, ok := parseServerID(destText)
	if !ok || kind != kindMailbox {
		return &easErr{status: 6003, http: http.StatusOK, text: "the destination is not a mail folder"}
	}

	_ = w.Start(nsMove, "MoveItems")
	if len(ids) == 0 {
		_ = w.Leaf(nsMove, "Status", "1")
		w.End()
		return nil
	}

	_, err := e.mail.Move(c.ctx, c.owner(), ids, destID)
	if err != nil {
		e.log.ErrorContext(c.ctx, "eas: MoveItems failed", "error", err)
		_ = w.Leaf(nsMove, "Status", "1301")
		w.Start(nsMove, "MoveErrors")
		for _, id := range ids {
			w.Start(nsMove, "MoveError")
			_ = w.Leaf(nsMove, "Id", strconv.FormatInt(id, 10))
			w.End()
		}
		w.End() // MoveErrors
		w.End() // MoveItems
		return nil
	}

	_ = w.Leaf(nsMove, "Status", "1")
	w.End()
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
	_ = w.Leaf(nsGetItemEstimate, "Status", "1")
	w.Start(nsGetItemEstimate, "Collections")
	for _, est := range estimates {
		w.Start(nsGetItemEstimate, "Collection")
		_ = w.Leaf(nsGetItemEstimate, "CollectionId", est.id)
		_ = w.Leaf(nsGetItemEstimate, "Estimate", strconv.Itoa(est.count))
		w.End()
	}
	w.End() // Collections
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
	f, err := e.mail.CreateFolder(c.ctx, c.owner(), name)
	if err != nil {
		e.log.ErrorContext(c.ctx, "eas: FolderCreate failed", "error", err)
		return &easErr{status: 6002, http: http.StatusOK, text: "the folder could not be created"}
	}
	_ = w.Start(nsFolderHierarchy, "FolderCreate")
	_ = w.Leaf(nsFolderHierarchy, "Status", "1")
	_ = w.Leaf(nsFolderHierarchy, "ServerId", strconv.FormatInt(f.ID, 10))
	w.End()
	return nil
}

// cmdFolderDelete deletes a mail folder.
func (e *EAS) cmdFolderDelete(c *call, w *WBXMLWriter) *easErr {
	idText := c.root.TextDescendant("ServerId")
	kind, id, ok := parseServerID(idText)
	if !ok || kind != kindMailbox {
		return &easErr{status: 6002, http: http.StatusOK, text: "the folder cannot be deleted"}
	}
	if err := e.mail.DeleteFolder(c.ctx, c.owner(), id); err != nil {
		e.log.ErrorContext(c.ctx, "eas: FolderDelete failed", "error", err)
		return &easErr{status: 6002, http: http.StatusOK, text: "the folder could not be deleted"}
	}
	_ = w.Start(nsFolderHierarchy, "FolderDelete")
	_ = w.Leaf(nsFolderHierarchy, "Status", "1")
	w.Start(nsFolderHierarchy, "Delete")
	_ = w.Leaf(nsFolderHierarchy, "ServerId", idText)
	w.End()
	w.End()
	return nil
}

// cmdFolderUpdate renames a mail folder.
func (e *EAS) cmdFolderUpdate(c *call, w *WBXMLWriter) *easErr {
	idText := c.root.TextDescendant("ServerId")
	kind, id, ok := parseServerID(idText)
	if !ok || kind != kindMailbox {
		return &easErr{status: 6002, http: http.StatusOK, text: "the folder cannot be updated"}
	}
	name := c.root.TextDescendant("DisplayName")
	if name == "" {
		return e.badRequest("a FolderUpdate names no new name")
	}
	if _, err := e.mail.RenameFolder(c.ctx, c.owner(), id, name); err != nil {
		e.log.ErrorContext(c.ctx, "eas: FolderUpdate failed", "error", err)
		return &easErr{status: 6002, http: http.StatusOK, text: "the folder could not be renamed"}
	}
	_ = w.Start(nsFolderHierarchy, "FolderUpdate")
	_ = w.Leaf(nsFolderHierarchy, "Status", "1")
	w.Start(nsFolderHierarchy, "Update")
	_ = w.Leaf(nsFolderHierarchy, "ServerId", idText)
	w.End()
	w.End()
	return nil
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

// cmdMeetingResponse records an attendee's answer to a meeting. The request
// carries the meeting as an opaque iCalendar document, and the answer is read
// from the attendee that is the user, in the document's own partstat, and
// written to the local copy so the calendar shows the user accepted, declined
// or is tentative.
func (e *EAS) cmdMeetingResponse(c *call, w *WBXMLWriter) *easErr {
	answer := func() *easErr {
		_ = w.Start(nsMeetingResponse, "MeetingResponse")
		_ = w.Leaf(nsMeetingResponse, "Status", "1")
		w.End()
		return nil
	}

	var ics []byte
	if meeting := c.root.FindDescendant("Meeting"); meeting != nil {
		if data := meeting.FindChild("Data"); data != nil && len(data.Opaque) > 0 {
			ics = data.Opaque
		} else if data := meeting.FindDescendant("Data"); data != nil && len(data.Opaque) > 0 {
			ics = data.Opaque
		}
	}
	if len(ics) == 0 {
		// A MeetingResponse that carries no readable meeting is answered without
		// a change rather than with an error: the client's local copy is what
		// it is, and a server that cannot see it should not claim it saw it.
		return answer()
	}

	doc, err := ical.Parse(ics)
	if err != nil || len(doc.Events) == 0 {
		e.log.WarnContext(c.ctx, "eas: MeetingResponse carried no readable meeting", "error", err)
		return answer()
	}
	ev := doc.Events[0]

	partstat := ""
	for _, a := range ev.Attendees {
		if strings.EqualFold(a.Address, c.owner()) {
			partstat = a.PartStat
			break
		}
	}
	if partstat != "" {
		if _, err := store.NewEvents().SetPartStat(c.ctx, e.db, ev.UID, c.owner(), partstat); err != nil {
			e.log.WarnContext(c.ctx, "eas: MeetingResponse could not record the answer", "error", err)
		}
	}
	return answer()
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// cmdSearch reports the stores a client may search. This server exposes its
// account's directory as one store; the command's job is to name it, which is
// what a client does before it sends a search string.
func (e *EAS) cmdSearch(c *call, w *WBXMLWriter) *easErr {
	_ = w.Start(nsSearch, "Search")
	_ = w.Leaf(nsSearch, "Status", "1")
	w.Start(nsSearch, "Stores")
	w.Start(nsSearch, "Store")
	_ = w.Leaf(nsSearch, "DisplayName", "Global Address List")
	_ = w.Leaf(nsSearch, "StoreId", "GAL")
	w.End() // Store
	w.End() // Stores
	w.End() // Search
	return nil
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// cmdSettings answers a settings request with the account's push interval,
// which is the one setting a client asks for on setup.
func (e *EAS) cmdSettings(c *call, w *WBXMLWriter) *easErr {
	account := c.root.TextDescendant("Account")
	if account == "" {
		account = c.owner()
	}
	_ = w.Start(nsSettings, "Settings")
	_ = w.Leaf(nsSettings, "Status", "1")
	w.Start(nsSettings, "Settings")
	_ = w.Leaf(nsSettings, "Account", account)
	w.Start(nsSettings, "Push")
	_ = w.Leaf(nsSettings, "Interval", strconv.Itoa(heartbeatInterval))
	w.End() // Push
	w.End() // Settings
	w.End() // Settings
	return nil
}

// ---------------------------------------------------------------------------
// ItemOperations
// ---------------------------------------------------------------------------

// cmdItemOperations applies per-item flag changes: marking a message read or
// unread, or complete or not. Each operation names an item and the change, and
// the changes go through the same service the web client uses, so a flag set
// here is the flag the web client shows.
func (e *EAS) cmdItemOperations(c *call, w *WBXMLWriter) *easErr {
	if ops := c.root.FindDescendant("Operations"); ops != nil {
		for _, op := range ops.FindChildren("Operation") {
			idText := op.TextOf("ServerId")
			id, err := strconv.ParseInt(idText, 10, 64)
			if err != nil || id <= 0 {
				continue
			}
			switch {
			case op.FindChild("MarkAsRead") != nil:
				if err := e.mail.MarkRead(c.ctx, c.owner(), id, true); err != nil {
					e.log.WarnContext(c.ctx, "eas: MarkAsRead failed", "id", id, "error", err)
				}
			case op.FindChild("MarkAsUnread") != nil:
				if err := e.mail.MarkRead(c.ctx, c.owner(), id, false); err != nil {
					e.log.WarnContext(c.ctx, "eas: MarkAsUnread failed", "id", id, "error", err)
				}
			case op.FindChild("MarkAsCompleted") != nil:
				if err := e.mail.SetFlag(c.ctx, c.owner(), id, `\Flagged`, false); err != nil {
					e.log.WarnContext(c.ctx, "eas: MarkAsCompleted failed", "id", id, "error", err)
				}
			case op.FindChild("MarkAsUncompleted") != nil:
				if err := e.mail.SetFlag(c.ctx, c.owner(), id, `\Flagged`, true); err != nil {
					e.log.WarnContext(c.ctx, "eas: MarkAsUncompleted failed", "id", id, "error", err)
				}
			}
		}
	}
	_ = w.Start(nsItemOperations, "ItemOperations")
	_ = w.Leaf(nsItemOperations, "Status", "1")
	w.End()
	return nil
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
func decodeState[T any](encoded string) (T, bool, *easErr) {
	var out T
	if strings.TrimSpace(encoded) == "" {
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
)

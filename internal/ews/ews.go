// Package ews implements Exchange Web Services (MS-OXECWS).
//
// It is the protocol a mail client that speaks "Exchange" uses to do everything:
// list folders, read and write mail, manage a calendar, and look people up. It is
// SOAP over HTTP, which makes it the most tractable of the three Exchange
// protocols this server speaks, and the one that matters most -- a client that
// can read mail over EWS works in Outlook, in Evolution, and in anything else
// with an Exchange option.
//
// The operations are in folders.go, items.go, calendar.go and people.go. This
// file is the transport: authentication, the SOAP envelope, and dispatch.
package ews

import (
	"context"
	"encoding/xml"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/webmail"
)

// maxRequestBytes bounds a request body.
//
// An EWS request carries a message being composed, with attachments, so the bound
// is generous -- but it is a bound. Without one, a client that announces a gigabyte
// gets to send a gigabyte at a server that has already committed to answering.
const maxRequestBytes = 64 << 20

// Options are the dependencies of the service.
type Options struct {
	Config *config.Config
	DB     *sqlx.DB
	// Mail is the mail reading and writing service. EWS does not have its own
	// SQL: a folder created here and a folder created in the web client are the
	// same row, and a message moved here and a message moved there are the same
	// move. Sharing the service is what makes that true by construction rather
	// than by agreement.
	Mail *webmail.Service
	// Calendar is the calendar service, shared with the REST API and CalDAV for
	// the same reason Mail is.
	Calendar *calendar.Service
	// Blobs is where message bytes live, and Messages is the row that points at
	// them. A copy has to write both, and the copy is the one operation in EWS
	// that creates a message without composing one, so it goes through the same
	// store every other delivery does rather than through a second writer that
	// would have to remember the columns.
	Blobs *blobstore.Store
	// Messages writes a message row.
	Messages *imapd.MessageRepo
	// Auth verifies the login. It is the same verifier the IMAP, POP3, SMTP,
	// EAS and CalDAV listeners hold, so one account has one password and one
	// answer to "may this person use this protocol".
	Auth Authenticator
	// Policy is the authorization rules. It is already inside Auth for the
	// logon; it is held here for the decisions a request makes after it.
	Policy policy.Policy
	Logger *slog.Logger
}

// EWS serves Exchange Web Services.
type EWS struct {
	cfg      *config.Config
	db       *sqlx.DB
	mail     *webmail.Service
	cal      *calendar.Service
	blobs    *blobstore.Store
	messages *imapd.MessageRepo
	auth     Authenticator
	policy   policy.Policy
	log      *slog.Logger
	// version is the ServerVersion this build reports, so that a client that
	// asks and a test that checks are reading the same number.
	version string
}

// Authenticator checks a login name and password for a protocol.
//
// It is satisfied by the shared mailauth.Verifier in production, and is an
// interface rather than that concrete type so that a test can stand in for a
// database that a test of the SOAP layer has no business opening. The interface
// is the shape every logon path already speaks: one call, four arguments, a
// principal or a reason.
type Authenticator interface {
	Authenticate(ctx context.Context, login, pw string, action policy.Action) (policy.Principal, error)
}

// New builds the service.
func New(opts Options) *EWS {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &EWS{
		cfg:      opts.Config,
		db:       opts.DB,
		mail:     opts.Mail,
		cal:      opts.Calendar,
		blobs:    opts.Blobs,
		messages: opts.Messages,
		auth:     opts.Auth,
		policy:   opts.Policy,
		log:      log,
		version:  serverVersionMajor + "." + serverVersionMinor,
	}
}

// Handler returns the HTTP handler for the EWS endpoints.
//
// The routes are relative to the mount point: the server mounts this handler at
// /ews, so "/ews/exchange.asmx" here would only answer at
// /ews/ews/exchange.asmx, which is not where Exchange clients -- or the URL
// Autodiscover hands them -- look.
func (e *EWS) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(e.recover)

	// MS-OXECWS §2.1 names the path /EWS/Exchange.asmx, and HTTP paths are
	// case-insensitive in every client that implements this protocol, so both
	// spellings are registered. A 404 here is read by a client as "there is no
	// Exchange server here", which is a different and much more expensive thing
	// to discover than a redirect.
	for _, path := range []string{"/exchange.asmx", "/Exchange.asmx", "/Exchange.asmx/"} {
		r.Post(path, e.handleSOAP)
	}

	// The path a client checks before it commits to talking to a server.
	r.Get("/", e.handleServiceRoot)

	r.NotFound(e.davNotFound)
	r.MethodNotAllowed(e.methodNotAllowed)
	return r
}

// isSOAPContentType reports whether a Content-Type names a document this
// endpoint can read.
//
// The two spellings find in the wild are text/xml and application/soap+xml,
// each with or without a charset, and both are accepted. A client that sends
// neither is answered before the body is read.
func isSOAPContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = strings.TrimSpace(contentType[:i])
	}
	switch contentType {
	case "text/xml", "application/xml", "application/soap+xml":
		return true
	}
	return false
}

// handleServiceRoot answers GET on the mount point.
//
// Some clients probe the endpoint with a GET to see whether anything is there
// before sending a SOAP envelope. A 405 with the list of methods would be the
// pedantically correct answer, and it is also the one that makes a client give
// up: it is looking for a yes.
func (e *EWS) handleServiceRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	w.Header().Set("X-RequestServerVersion", e.version)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Exchange Web Services " + e.version + "\n"))
}

func (e *EWS) davNotFound(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "no EWS endpoint at this path", http.StatusNotFound)
}

func (e *EWS) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	http.Error(w, "EWS accepts POST", http.StatusMethodNotAllowed)
}

// recover turns a panic in an operation into a SOAP fault.
//
// A client that sent something this service could not parse must be told so in a
// document it can read. If the connection is simply closed instead, the client
// reports a network error and the real fault is only in the server's log.
func (e *EWS) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				e.log.Error("ews: an operation panicked",
					"path", r.URL.Path, "panic", rec)
				e.writeFault(w, &soapError{
					code:    codeErrorInternalServerError,
					text:    "the request could not be processed",
					status:  http.StatusInternalServerError,
					element: "",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// principalKey is the context key for the authenticated principal.
type principalKey struct{}

// handleSOAP authenticates the caller and dispatches the operation.
func (e *EWS) handleSOAP(w http.ResponseWriter, r *http.Request) {
	// MS-OXECWS clients carry a SOAP envelope, and the envelope is XML. A
	// request that names another content type is not a client of this endpoint
	// and is answered before it is read: what has been announced as JSON could
	// be anything, and parsing it as XML would be how a pocket of "find me
	// anything" XML ended up feeding an operation.
	if !isSOAPContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "EWS accepts a SOAP document", http.StatusBadRequest)
		return
	}

	who, ok := e.authenticate(w, r)
	if !ok {
		return
	}
	ctx := context.WithValue(r.Context(), principalKey{}, who)

	// Outlook sends the mailbox it believes it is acting on. Echoing it back lets
	// a client show a sensible error instead of a generic one when the account
	// has been renamed, and it costs nothing to honour.
	if mb := r.Header.Get("X-AnchorMailbox"); mb != "" {
		w.Header().Set("X-AnchorMailbox", mb)
	}
	w.Header().Set("X-RequestServerVersion", e.version)
	w.Header().Set("X-Powered-By", "linchange")

	body, err := readBody(r.Body, maxRequestBytes)
	if err != nil {
		e.writeFault(w, errorf("", codeErrorInvalidRequest, http.StatusBadRequest,
			"the request body could not be read: %v", err))
		return
	}

	var env SOAPEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		e.writeFault(w, errorf("", codeErrorInvalidRequest, http.StatusBadRequest,
			"the request is not a SOAP envelope: %v", err))
		return
	}
	if env.Body == nil {
		e.writeFault(w, errMissingOperation)
		return
	}

	name := soapActionName(r.Header.Get("SOAPAction"))
	if name == "" {
		// Not every client sets SOAPAction. MS-OXECWS identifies the operation
		// by the element in the body, so fall back to that rather than guessing.
		name = env.Body.Operation()
	}
	if name == "" {
		e.writeFault(w, errMissingOperation)
		return
	}

	run, known := operations[name]
	if !known {
		// Refused with the status a client acts on. It used to be a 200 carrying
		// a response element for an operation this server does not have, which
		// a client reads as success with no items in it: an Outlook connected to
		// this endpoint showed an empty inbox that could not be filled by any
		// action the user could take.
		e.writeFault(w, errorf("", codeErrorInvalidRequest, http.StatusNotImplemented,
			"%s is not an operation this service implements", name))
		return
	}

	call := &call{
		srv:     e,
		r:       r.WithContext(ctx),
		who:     who,
		name:    name,
		element: operationElement(env.Body.Inner, name),
	}
	resp := run(e, call)

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Content-Length", "")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(envelope(resp.element, resp.body))
}

// operationElement returns the raw XML of the operation element, so that each
// operation can decode the arguments it has.
//
// The body holds exactly one operation, so the whole body is that element. It is
// taken as text rather than decoded here because the arguments are the operation's
// own: this file would otherwise have to name every one of them, and adding an
// argument would mean editing the transport.
func operationElement(body, name string) []byte {
	return []byte(strings.TrimSpace(body))
}

// authenticate verifies the credentials, answering the client itself when there
// are none.
//
// The refusal is a 401 with a Basic challenge, which is what makes a client ask
// for a password rather than treat the endpoint as absent.
func (e *EWS) authenticate(w http.ResponseWriter, r *http.Request) (policy.Principal, bool) {
	if e.auth == nil {
		// No verifier means nothing can be authenticated, and serving anyway
		// would serve one person's mail to whoever asked.
		e.challenge(w, r)
		return policy.Principal{}, false
	}
	login, pw, ok := r.BasicAuth()
	if !ok || login == "" {
		e.challenge(w, r)
		return policy.Principal{}, false
	}
	who, err := e.auth.Authenticate(r.Context(), login, pw, policy.ActionEWS)
	if err != nil {
		e.log.InfoContext(r.Context(), "ews login refused",
			"address", login, "error", err)
		e.challenge(w, r)
		return policy.Principal{}, false
	}
	return who, true
}

func (e *EWS) challenge(w http.ResponseWriter, r *http.Request) {
	// The realm is what a client shows the user, so it names the server rather
	// than the protocol.
	realm := "linchange"
	if e.cfg != nil && e.cfg.Server.Hostname != "" {
		realm = e.cfg.Server.Hostname
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("authentication required\n"))
}

// writeFault answers a failure in SOAP.
func (e *EWS) writeFault(w http.ResponseWriter, err *soapError) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(err.status)
	_, _ = w.Write(fault(err))
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

// call is one decoded operation in progress: the identity that asked, the
// operation name, and the XML of the operation element.
type call struct {
	srv  *EWS
	r    *http.Request
	who  policy.Principal
	name string
	// element is the raw XML of the <m:Operation> element.
	element []byte
}

// decode reads the operation element into a typed request.
//
// A request that does not match the shape the operation expects is refused with
// ErrorSchemaValidation, which is what makes a client report its own bug rather
// than showing the user an empty mailbox.
func (c *call) decode(v any) *soapError {
	if err := xml.Unmarshal(c.element, v); err != nil {
		return errorf(c.name+"Response", codeErrorSchemaValidation, http.StatusOK,
			"the %s request could not be read: %v", c.name, err)
	}
	return nil
}

// ctx is the request context, which carries the authenticated principal.
func (c *call) ctx() context.Context { return c.r.Context() }

// operations is the dispatch table.
//
// It is a map rather than a switch because the table is the answer to "what does
// this server support", and a client -- or a test -- can read it. A switch hid
// that behind a chain of string comparisons in which one missing case is invisible.
var operations = map[string]func(*EWS, *call) responseCode{
	// Folders.
	"FindFolder":          (*EWS).findFolder,
	"GetFolder":           (*EWS).getFolder,
	"CreateFolder":        (*EWS).createFolder,
	"UpdateFolder":        (*EWS).updateFolder,
	"DeleteFolder":        (*EWS).deleteFolder,
	"MoveFolder":          (*EWS).moveFolder,
	"SyncFolderHierarchy": (*EWS).syncFolderHierarchy,

	// Items: mail.
	"FindItem":        (*EWS).findItem,
	"GetItem":         (*EWS).getItem,
	"CreateItem":      (*EWS).createItem,
	"UpdateItem":      (*EWS).updateItem,
	"DeleteItem":      (*EWS).deleteItem,
	"MoveItem":        (*EWS).moveItem,
	"CopyItem":        (*EWS).copyItem,
	"SendItem":        (*EWS).sendItem,
	"MarkAsJunk":      (*EWS).markAsJunk,
	"SyncFolderItems": (*EWS).syncFolderItems,

	// People and time.
	"ResolveNames":        (*EWS).resolveNames,
	"ExpandDL":            (*EWS).expandDL,
	"GetUserAvailability": (*EWS).getUserAvailability,
	"GetServerTimeZones":  (*EWS).getServerTimeZones,
}

// soapActionName extracts the operation from a SOAPAction header. The header
// holds a URI - typically
// "http://schemas.microsoft.com/exchange/services/2006/messages/FindItem" - and
// sometimes the empty string, so the last path segment is the operation name.
func soapActionName(header string) string {
	header = strings.TrimSpace(header)
	header = strings.Trim(header, `"`)
	if header == "" {
		return ""
	}
	if i := strings.LastIndex(header, "/"); i >= 0 {
		header = header[i+1:]
	}
	// A client that sends the prefixed name rather than the qualified one is
	// unusual but not wrong, and the prefix is not part of the operation name.
	return strings.TrimPrefix(header, "m:")
}

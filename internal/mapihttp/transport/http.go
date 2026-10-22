// Package transport carries the MAPI/HTTP framing for both MAPI/HTTP
// endpoints (MS-OXCMAPIHTTP): the request headers, the session cookies, the
// response meta-tags, and the two dispatch loops — ROPs on
// /mapi/emsmdb, address book operations on /mapi/nspi.
//
// Everything below the framing lives in the rop and nspi packages. What is
// here is what a client sees on the wire before it ever gets to an operation,
// and the shape of it follows the reference implementation (gromox's exch/mh)
// field for field, header for header.
package transport

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/contacts"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/mapihttp/nspi"
	"github.com/linchange/linchange/internal/mapihttp/rop"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// Authenticator checks a login name and password for a protocol.
//
// It is satisfied by the shared mailauth.Verifier in production and is an
// interface rather than that concrete type so that a test of the framing can
// stand in for a database it has no business opening. The interface is the
// shape every logon path in this server already speaks.
type Authenticator interface {
	Authenticate(ctx context.Context, login, pw string, action policy.Action) (policy.Principal, error)
}

// Options are the transport's dependencies.
//
// Mail, Messages, Mailboxes and Users feed the ROP layer, so an Outlook
// client reads and writes the same rows EWS, EAS, IMAP and the web client do;
// Users and Hostname additionally feed the address book, which is a
// projection of the same user directory. Auth is the same verifier every
// other mail protocol holds, called with ActionMAPIHTTP so that "may this
// account speak MAPI/HTTP" is decided by one rule alongside the others.
type Options struct {
	DB        *sqlx.DB
	Mail      *webmail.Service
	Messages  *imapd.MessageRepo
	Mailboxes *imapd.MailboxRepo
	Users     *store.Users
	// Contacts is the organization directory the address book enriches its
	// rows from.
	Contacts *contacts.Service
	// Hostname is the public name this server is reachable at, without a
	// scheme. It appears in the URLs the address book's GetMailboxUrl and
	// GetAddressBookUrl operations return, and is the realm a client is
	// challenged with.
	Hostname string
	Auth     Authenticator
	Logger   *slog.Logger
	// ServerVersion is what the X-ServerApplication header reports. It is a
	// dotted Exchange version because that is the shape the specification
	// names and the shape a client parses.
	ServerVersion string
	// MaxBodyBytes bounds a request body. MAPI/HTTP carries a whole ROP
	// list or a whole address book request, so the bound is generous, but
	// it is a bound: an unbounded read is a memory-exhaustion vector on a
	// protocol that has to answer before it can authenticate.
	MaxBodyBytes int64
}

// Transport serves the two MAPI/HTTP endpoints.
type Transport struct {
	opts Options
	log  *slog.Logger

	// emsmdb and nspi backends are built once here rather than per
	// request, so a session opened by one request is served by the same
	// store wiring as the next.
	emsmdb *rop.Backend
	nspi   *nspi.Backend

	mu sync.Mutex
	// emsmdbSessions and nspiSessions are keyed by the sid cookie. The two
	// endpoints keep separate tables, as the reference does: the cookie is
	// scoped to the path it was issued on, and a sid that arrived at the
	// wrong endpoint is a context that does not exist there.
	emsmdbSessions map[string]*session
	nspiSessions   map[string]*session
}

// New builds a transport from its options. Missing optional fields get the
// safe value: no logger becomes a discard logger, no size bound becomes ten
// megabytes, no server version becomes the reference's default.
func New(opts Options) *Transport {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.ServerVersion == "" {
		opts.ServerVersion = defaultServerVersion
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	return &Transport{
		opts: opts,
		log:  log,
		emsmdb: &rop.Backend{
			DB:        opts.DB,
			Mail:      opts.Mail,
			Messages:  opts.Messages,
			Mailboxes: opts.Mailboxes,
			Users:     opts.Users,
			Hostname:  opts.Hostname,
		},
		nspi: &nspi.Backend{
			DB:       opts.DB,
			Users:    opts.Users,
			Contacts: opts.Contacts,
			Hostname: opts.Hostname,
			Logger:   log,
		},
		emsmdbSessions: make(map[string]*session),
		nspiSessions:   make(map[string]*session),
	}
}

// Handler returns the HTTP handler for both endpoints. The routes are
// relative to the mount point: the server mounts this handler at /mapi, so
// "/emsmdb" here answers at "/mapi/emsmdb".
//
// Each endpoint is registered with and without a trailing slash because that
// is how the two spell the request. The reference requires the request target
// to begin with "/mapi/emsmdb/?MailboxId=" — the trailing slash is part of
// the path, not an artifact of the query — and a client that drops the
// trailing slash has not sent a different request, only written it differently.
func (t *Transport) Handler() http.Handler {
	r := chi.NewRouter()
	for _, endpoint := range []string{endpointEMSMDB, endpointNSPI} {
		handler := t.serve(endpoint)
		r.Post("/"+endpoint, handler)
		r.Post("/"+endpoint+"/", handler)
	}
	return r
}

// serve answers one request on one endpoint.
//
// The order of the checks is the reference's, and it is load-bearing: the
// credentials are checked before the headers are even looked at, so a
// caller with no password learns nothing about what headers the endpoint
// wants; the headers are checked before the cookies, so a request that never
// named a request type is refused for not naming one rather than for the
// cookie it did not need to send; and the cookies are checked before the
// body is read, so a request to a session that does not exist costs the
// server no parsing at all.
func (t *Transport) serve(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tw := &trackedWriter{ResponseWriter: w}
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				t.log.ErrorContext(r.Context(), "mapi/http: a request panicked",
					"endpoint", endpoint, "panic", rec)
				if !tw.wrote {
					http.Error(tw, "internal server error", http.StatusInternalServerError)
				}
			}
		}()

		who, ok := t.authenticate(tw, r)
		if !ok {
			return
		}

		q := &request{
			t:         t,
			w:         tw,
			r:         r,
			endpoint:  endpoint,
			principal: who,
			start:     start,
		}
		if !q.parseHeaders() {
			return
		}
		if !q.loadCookies() {
			return
		}
		body, ok := q.readBody()
		if !ok {
			return
		}
		if endpoint == endpointEMSMDB {
			q.dispatchEMSMDB(body)
			return
		}
		q.dispatchNSPI(body)
	}
}

// authenticate verifies the credentials, answering the client itself when
// there are none.
//
// The refusal is a 401 with a Basic challenge, which is what makes a client
// ask for a password rather than treat the endpoint as absent. The action is
// ActionMAPIHTTP, so the decision "may this account use this protocol" is the
// same one the IMAP, POP3, SMTP, EWS and EAS listeners make about theirs.
func (t *Transport) authenticate(w http.ResponseWriter, r *http.Request) (policy.Principal, bool) {
	var none policy.Principal
	if t.opts.Auth == nil {
		// No verifier means nothing can be authenticated, and serving
		// anyway would serve one person's mail to whoever asked.
		t.challenge(w)
		return none, false
	}
	login, pw, ok := r.BasicAuth()
	if !ok || login == "" {
		t.challenge(w)
		return none, false
	}
	who, err := t.opts.Auth.Authenticate(r.Context(), login, pw, policy.ActionMAPIHTTP)
	if err != nil {
		t.log.InfoContext(r.Context(), "mapi/http: login refused", "address", login, "error", err)
		t.challenge(w)
		return none, false
	}
	return who, true
}

func (t *Transport) challenge(w http.ResponseWriter) {
	realm := "linchange"
	if t.opts.Hostname != "" {
		realm = t.opts.Hostname
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("authentication required\n"))
}

const (
	endpointEMSMDB = "emsmdb"
	endpointNSPI   = "nspi"
)

// request is one MAPI/HTTP request in flight.
type request struct {
	t         *Transport
	w         *trackedWriter
	r         *http.Request
	endpoint  string
	principal policy.Principal

	requestType string
	requestID   string
	clientInfo  string

	// sid and sequence are the cookie values the response carries back.
	// They stay empty when no session was established or when the cookie
	// is not to be repeated, which is how the reference decides not to
	// send a Set-Cookie at all.
	sid      string
	sequence string
	session  *session

	start time.Time
}

// parseHeaders reads the headers every request must carry and refuses the
// request when one is missing.
//
// X-RequestType is the operation's name; without it the request has no verb.
// X-RequestId and X-ClientInfo are required so that a failure can be traced
// back to the client instance that caused it, and both are echoed in the
// response so that the client can pair its log lines with the reply.
func (q *request) parseHeaders() bool {
	q.requestType = q.r.Header.Get("X-RequestType")
	q.requestID = q.r.Header.Get("X-RequestId")
	q.clientInfo = q.r.Header.Get("X-ClientInfo")
	if q.requestType == "" {
		q.errorResponse(respInvalidVerb)
		return false
	}
	if q.requestID == "" || q.clientInfo == "" {
		q.errorResponse(respMissingHeader)
		return false
	}
	return true
}

// loadCookies validates the request's session cookies against the endpoint
// it arrived at. It reports false when a response — an error, or a PING's
// answer — has already been written and nothing further is owed.
func (q *request) loadCookies() bool {
	if q.endpoint == endpointEMSMDB {
		return q.loadEMSMDBCookies()
	}
	return q.loadNSPICookies()
}

// readBody reads the request body, bounded.
//
// It runs after the cookies have been checked so that a request to a session
// that does not exist is refused without reading anything, which is the
// reference's order too.
func (q *request) readBody() ([]byte, bool) {
	limit := q.t.opts.MaxBodyBytes
	if q.r.ContentLength > limit {
		q.errorResponse(respTooLarge)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(q.r.Body, limit+1))
	if err != nil {
		q.t.log.InfoContext(q.r.Context(), "mapi/http: could not read the request body",
			"endpoint", q.endpoint, "error", err)
		q.errorResponse(respUnknownFailure)
		return nil, false
	}
	if int64(len(body)) > limit {
		q.errorResponse(respTooLarge)
		return nil, false
	}
	return body, true
}

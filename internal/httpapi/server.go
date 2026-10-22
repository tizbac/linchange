// Package httpapi is the chi router behind the REST API and the web interface.
//
// It is deliberately a thin seam: it knows how to speak HTTP and how to turn a
// request into a call on the login and reset orchestrators, and nothing more.
// The security decisions live in those packages, so a rule is enforced once
// regardless of which listener a request arrives on.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/admin"
	ldapauth "github.com/linchange/linchange/internal/auth/ldap"
	"github.com/linchange/linchange/internal/auth/lockout"
	"github.com/linchange/linchange/internal/auth/login"
	"github.com/linchange/linchange/internal/auth/mailauth"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/reset"
	"github.com/linchange/linchange/internal/auth/token"
	"github.com/linchange/linchange/internal/auth/totp"
	"github.com/linchange/linchange/internal/autodiscover"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/caldav"
	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/chat"
	"github.com/linchange/linchange/internal/delivery"
	"github.com/linchange/linchange/internal/eas"
	"github.com/linchange/linchange/internal/ews"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/imapimport"
	"github.com/linchange/linchange/internal/mapihttp"
	"github.com/linchange/linchange/internal/meetings"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// maxAuthBody bounds a request body to what an auth payload can plausibly be,
// so a client cannot stream gigabytes into a decode.
const maxAuthBody = 4096

// context keys for the principal and session the middleware resolves.
type contextKey int

const (
	principalKey contextKey = iota
	sessionIDKey
	mailOwnerKey
)

// Server holds the long-lived dependencies the handlers share.
type Server struct {
	db       *sqlx.DB
	log      *slog.Logger
	auth     *login.Authenticator
	reset    *reset.Resetter
	admin    *admin.Service
	tokens   *token.Manager
	sessions *store.Sessions
	// mail is the webmail service: folders, messages, composing and rules. It
	// is a separate object from the HTTP layer so that the same operations are
	// reachable by delivery and by the tests, not only by a request.
	mail *webmail.Service
	// calendar is the calendar service: occurrences, free/busy, iTIP.
	// It is separate so that an inbound scheduling message and the HTTP API
	// both apply the same logic.
	calendar *calendar.Service
	// caldav is the CalDAV server for standard calendar clients.
	caldav *caldav.Server
	// autodiscover is the Autodiscover service for Outlook.
	autodiscover *autodiscover.Autodiscover
	// ews is the Exchange Web Services endpoint.
	ews *ews.EWS
	// eas is the ActiveSync endpoint.
	eas *eas.EAS
	// mapihttp is the MAPI/HTTP endpoint for Outlook native.
	mapihttp *mapihttp.Server
	// The repositories below back the read-only listings the web client needs:
	// which aliases reach this mailbox, and which team mailboxes this member
	// may open.
	aliases *store.Aliases
	shared  *store.SharedMailboxes
	grants  *store.SharedMailboxGrants
	// workgroups answers "which organization is this address in", which the top
	// bar and the address book both need.
	workgroups *store.Workgroups
	// policies is held so a handler can answer a question about this
	// deployment's switches. It is the same object the login path and the admin
	// service decide with, so there is one answer rather than three.
	policies  policy.Policy
	staticDir string
	// imports runs remote-IMAP import jobs. It is nil when the application
	// did not build one (tests that never touch it); the routes are then
	// simply not mounted.
	imports *imapimport.Service
	// meetings creates meetings: the invitation emails with their reply
	// links, the answers, and the native chat room behind the chat panel.
	meetings *meetings.Service
	// chat is the durable chat store, and chatHub carries it live over a
	// WebSocket.
	chat    *chat.Service
	chatHub *chat.Hub
}

// Options are the inputs to New.
type Options struct {
	DB     *sqlx.DB
	Config *config.Config
	Logger *slog.Logger
	// Blobs is the content-addressed store holding message bodies. The webmail
	// API reads messages through it, so a server built without one cannot serve
	// mail and says so at construction rather than at the first read.
	Blobs *blobstore.Store
	// StaticDir, when set, is the directory holding the built web app. The
	// router serves its asset files and falls back to index.html for the
	// client-side routes so the single-binary server can host the SPA too.
	StaticDir string
	// Directory authenticates the principals whose passwords a directory holds.
	// The application passes the same instance to the mail listeners so a
	// browser and a mail client agree about who an account is; left nil, one is
	// built from the configuration, which is what a caller that has no other
	// surface to share with wants.
	Directory login.Directory
	// MailAuth authenticates the non-browser protocol clients on this server:
	// CalDAV today, and whatever else arrives over HTTP with a password rather
	// than a bearer token. The application passes the same verifier the IMAP,
	// POP3 and SMTP listeners use. Sharing it is the point: one rule decides who
	// may log in and how the password is checked, so a calendar client and a mail
	// client cannot come to disagree about the same account. Left nil, one is
	// built here from DB and Directory, which refuses directory accounts the
	// caller did not supply a directory for.
	MailAuth *mailauth.Verifier
	// Imports is the remote-IMAP import service. Left nil, the import routes
	// are not mounted.
	Imports *imapimport.Service
}

// New builds a Server from an open database and a loaded config.
//
// It wires the login and reset orchestrators the same way every other listener
// does, which is the point of keeping authentication in one place. Directory
// sign-ins go through the LDAP client built here from the configuration; with no
// provider configured that client refuses, which is what keeps a directory
// account from being answered by a stored hash.
func New(opts Options) (*Server, error) {
	if opts.DB == nil {
		return nil, errors.New("httpapi: a database handle is required")
	}
	cfg := opts.Config
	if cfg == nil {
		return nil, errors.New("httpapi: config is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	// The directory is shared with the mail listeners when the caller supplied
	// one, and built here when it did not. Either way it is never nil, because a
	// nil Directory would let login.New substitute its denial for the whole
	// application rather than for a deployment that merely has no directory.
	directory := opts.Directory
	if directory == nil {
		directory = ldapauth.New(cfg.LDAP, store.NewLDAPLinks(), opts.DB, nil)
	}

	// The mail verifier is the same object the IMAP, POP3 and SMTP listeners
	// hold, when the caller supplied it. Built here otherwise, from the directory
	// just resolved, so a CalDAV login goes through the same password check as a
	// mail login rather than through a second one that could disagree with it.
	mailVerifier := opts.MailAuth
	if mailVerifier == nil {
		mailVerifier = mailauth.New(opts.DB, log, directory)
	}

	tokens, err := token.NewManager(cfg.Auth.JWT)
	if err != nil {
		return nil, err
	}

	// A deployment that has not enabled a second factor has no verifier, and
	// the login package treats a nil verifier as "no second factor here".
	var verifier *totp.Verifier
	if cfg.Auth.TOTP.Enabled {
		verifier, err = totp.New(cfg.Auth.TOTP, cfg.Auth.EncryptionKey)
		if err != nil {
			return nil, err
		}
	}

	// The policy is built once and handed to everything that decides what a
	// principal may do. Constructing it per consumer would work until the day
	// one consumer learned a switch the others did not, and then impersonation
	// would be permitted in one place and refused in another.
	pol := policy.Policy{
		Impersonation: policy.Impersonation{
			Allowed:       cfg.Auth.Impersonation.Allowed,
			RequireReason: cfg.Auth.Impersonation.RequireReason,
			RequireGrant:  cfg.Auth.Impersonation.RequireGrant,
		},
	}

	auth, err := login.New(login.Options{
		Credentials: store.NewCredentials(),
		Enrolments:  store.NewTOTP(),
		Attempts:    store.NewAuthAttempts(),
		Challenges:  store.NewChallenges(),
		Sessions:    store.NewSessions(),
		// The two repositories below are what make impersonation more than a
		// configuration switch: one holds the per-user permission and the
		// other records that it was used. They are stateless, so sharing the
		// instances the admin service already uses costs nothing and keeps a
		// single view of the table.
		Grants: store.NewImpersonationGrants(),
		Audit:  store.NewAuditLog(),
		Tokens: tokens,
		TOTP:   verifier,
		Argon2: cfg.Auth.Local.Argon2,
		Lockout: lockout.Both{
			Account: lockout.Policy{
				MaxAttempts: cfg.Auth.Lockout.Account.MaxAttempts,
				Window:      cfg.Auth.Lockout.Account.Window,
				Duration:    cfg.Auth.Lockout.Account.Duration,
			},
			IP: lockout.Policy{
				MaxAttempts: cfg.Auth.Lockout.IP.MaxAttempts,
				Window:      cfg.Auth.Lockout.IP.Window,
				Duration:    cfg.Auth.Lockout.IP.Duration,
			},
		},
		Policy:       pol,
		ChallengeTTL: cfg.Auth.Challenge.TTL,
		// The directory backs the sign-ins of principals whose password it
		// holds. It is always present, so a deployment without a configured
		// provider gets the login package's own denial, which is what refuses
		// a directory account rather than letting it fall through to a stored
		// hash.
		Directory: directory,
	})
	if err != nil {
		return nil, err
	}

	// The reset link email is an outbox send that arrives with P3; until then a
	// nil sinker means the token is stored and treated as sent, which keeps the
	// flow exercisable end to end.
	r, err := reset.New(reset.Options{
		SecondaryEmails:   store.NewSecondaryEmails(),
		Tokens:            store.NewResetTokens(),
		Credentials:       store.NewCredentials(),
		Sessions:          store.NewSessions(),
		Sinker:            nil,
		Enabled:           cfg.Auth.Reset.Enabled,
		Argon2:            cfg.Auth.Local.Argon2,
		MinPasswordLength: cfg.Auth.Local.MinPasswordLength,
		TokenTTL:          cfg.Auth.Reset.TokenTTL,
		Logger:            log,
	})
	if err != nil {
		return nil, err
	}

	// The administration service is built here so the HTTP layer only ever
	// calls methods on it. It holds the same policy object the login path uses,
	// so an impersonation decision is the same decision wherever it is made.
	adm, err := admin.New(admin.Options{
		DB:                opts.DB,
		Argon2:            cfg.Auth.Local.Argon2,
		MinPasswordLength: cfg.Auth.Local.MinPasswordLength,
		Policy: policy.Policy{
			Impersonation: policy.Impersonation{
				Allowed:       cfg.Auth.Impersonation.Allowed,
				RequireReason: cfg.Auth.Impersonation.RequireReason,
				RequireGrant:  cfg.Auth.Impersonation.RequireGrant,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	// The webmail service is built here so the HTTP layer only calls methods on
	// it. It is the same object the inbound delivery path will use to apply
	// rules, so a rule behaves identically however a message arrived.
	mailService, err := webmail.New(webmail.Options{
		DB:       opts.DB,
		Blobs:    opts.Blobs,
		Logger:   log,
		Hostname: cfg.Server.Hostname,
		Outbound: delivery.Config{
			SMTP: delivery.SMTPConfig{
				Host:           cfg.Mail.Outbound.SMTPHost,
				Port:           cfg.Mail.Outbound.SMTPPort,
				TLS:            cfg.Mail.Outbound.SMTPStartTLS,
				ConnectTimeout: cfg.Mail.Outbound.ConnectTimeout,
				CommandTimeout: cfg.Mail.Outbound.CommandTimeout,
				Username:       cfg.Mail.Outbound.SMTPUsername,
				Password:       cfg.Mail.Outbound.SMTPPassword,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	// The calendar service is built here so the HTTP layer and the inbound
	// scheduling path both use the same logic. It holds the store repositories
	// and the expansion limits from the configuration.
	calService := calendar.NewService(
		store.NewCalendars(),
		store.NewEvents(),
		store.NewWorkgroups(),
	)

	// The CalDAV server is built here for standard calendar clients (Thunderbird,
	// iOS/macOS Calendar, Evolution, etc.). It uses the same calendar service.
	caldavServer := caldav.NewServer("/caldav", calService, opts.DB, mailVerifier)

	// Chat is native: it is built here, over the same database, and there is no
	// external chat server to configure or to be down. The hub is the live
	// socket layer; the service is the durable one.
	chatService := chat.New(opts.DB, log)
	chatHub := chat.NewHub(chatService, log)

	// The meetings service turns a calendar event into an invitation: one
	// email per guest with their own reply link, and a chat room in the local
	// chat store.
	meetingsService, err := meetings.New(meetings.Options{
		DB:        opts.DB,
		Mail:      mailService,
		Calendar:  calService,
		Chat:      chatService,
		PublicURL: cfg.MeetingsPublicURL(),
		Logger:    log,
	})
	if err != nil {
		return nil, err
	}

	// The Autodiscover service for Outlook automatic configuration.
	autodiscoverService := autodiscover.New(cfg, opts.DB)

	// The EWS (Exchange Web Services) endpoint.
	ewsService := ews.New(ews.Options{
		Config:   cfg,
		DB:       opts.DB,
		Mail:     mailService,
		Calendar: calService,
		Blobs:    opts.Blobs,
		Messages: imapd.NewMessageRepo(),
		Auth:     mailVerifier,
		Policy:   pol,
		Logger:   log,
	})

	// The EAS (Exchange ActiveSync) endpoint, for phones and the mobile Outlook
	// app. It reads and writes through the same mail service and message store
	// the web client and EWS use, so a message sent on a phone and one sent in
	// the browser are rows in the same table.
	easService := eas.New(eas.Options{
		Config:   cfg,
		DB:       opts.DB,
		Mail:     mailService,
		Messages: imapd.NewMessageRepo(),
		Auth:     mailVerifier,
		Policy:   pol,
		Logger:   log,
	})

	// The MAPI/HTTP endpoint, for Outlook's native protocol. It reads and
	// writes through the same mail service, message store and user
	// directory EWS, EAS and the web client use, so a message sent from
	// Outlook and one sent from the browser are rows in the same table and
	// the address book Outlook browses is the same directory every other
	// logon path reads.
	mapihttpService := mapihttp.New(mapihttp.Options{
		DB:        opts.DB,
		Mail:      mailService,
		Messages:  imapd.NewMessageRepo(),
		Mailboxes: imapd.NewMailboxRepo(),
		Users:     store.NewUsers(),
		Hostname:  cfg.Server.Hostname,
		Auth:      mailVerifier,
		Logger:    log,
	})

	return &Server{
		db:           opts.DB,
		log:          log,
		auth:         auth,
		reset:        r,
		admin:        adm,
		tokens:       tokens,
		sessions:     store.NewSessions(),
		mail:         mailService,
		calendar:     calService,
		caldav:       caldavServer,
		autodiscover: autodiscoverService,
		ews:          ewsService,
		eas:          easService,
		mapihttp:     mapihttpService,
		aliases:      store.NewAliases(),
		shared:       store.NewSharedMailboxes(),
		grants:       store.NewSharedMailboxGrants(),
		workgroups:   store.NewWorkgroups(),
		staticDir:    opts.StaticDir,
		imports:      opts.Imports,
		meetings:     meetingsService,
		chat:         chatService,
		chatHub:      chatHub,
	}, nil
}

// Handler builds the chi router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// No IP middleware runs by default, so the client IP is the direct peer.
	// Behind a reverse proxy, add middleware.ClientIPFromXFF(trusted CIDRs)
	// here: clientIP then prefers the value it publishes over RemoteAddr.
	r.Use(s.recover)
	r.NotFound(notFound)
	r.MethodNotAllowed(methodNotAllowed)

	r.Get("/healthz", s.health)

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/second-factor", s.handleSecondFactor)
		r.Post("/auth/refresh", s.handleRefresh)
		r.Post("/auth/reset/request", s.handleResetRequest)
		r.Post("/auth/reset/complete", s.handleResetComplete)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Post("/auth/logout", s.handleLogout)
			r.Get("/auth/me", s.handleMe)
			r.Post("/auth/stop-impersonating", s.handleStopImpersonating)
		})

		// Every administrative route requires a bearer token, and then requires
		// the caller to be a domain admin. The second check is the service's,
		// not the router's: a route group that assumed "authenticated" meant
		// "permitted" would be one refactor away from being wrong.
		s.mountAdmin(r)

		// The mail routes are for the signed-in user's own mailbox. They are
		// mounted in the same authenticated group, and resolve the owner from
		// the token rather than from the request.
		s.mountMail(r)

		// The calendar routes are for the signed-in user's calendars and any
		// workgroup calendars they may see. The same authenticated group applies.
		s.mountCalendar(r)

		// The organization routes answer "who is in my organization", which the
		// header, the address book and the invite picker all need.
		s.mountOrganization(r)

		// Meetings sit on top of the calendar and the mailbox: creating one
		// writes an event and queues an invitation. The invitee-facing routes
		// are mounted in the same group but outside requireAuth, because the
		// person following an invitation link has no session to present.
		s.mountMeetings(r)

		// Chat is native and lives on the same server: the REST routes for
		// history and conversation lists, and the WebSocket that carries
		// messages, typing, receipts and WebRTC signalling.
		s.mountChat(r)
	})

	// CalDAV is not part of the REST surface: a calendar client is handed
	// /.well-known/caldav or /caldav and follows it, so the endpoint has to sit
	// where those point. Mounting it under /api/v1 worked only for a caller who
	// already knew the path, which no calendar client does.
	r.Mount("/", s.caldav.Handler())

	// Autodiscover (outside /api/v1 for Outlook compatibility)
	r.Mount("/autodiscover", s.autodiscover.Handler())

	// EWS (Exchange Web Services)
	r.Mount("/ews", s.ews.Handler())

	// EAS (Exchange ActiveSync)
	r.Mount("/eas", s.eas.Handler())

	// MAPI/HTTP (Outlook native). The mount is the prefix both endpoints
	// share: /mapi/emsmdb carries the ROPs a mailbox client sends and
	// /mapi/nspi the address book it browses, each holding its own Session
	// Context.
	r.Mount("/mapi", s.mapihttp.Handler())

	// With a web build present, the same server hosts the SPA: asset files are
	// served directly and every other GET is answered with the app shell, which
	// the client-side router then takes over. The API routes above win for their
	// exact paths, so this never shadows the API.
	if s.staticDir != "" {
		r.Get("/*", s.serveSPA)
	}

	return r
}

// serveSPA serves a real asset if one exists at the requested path and otherwise
// hands back index.html, so /login and /second-factor work without a 404.
func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/api" || strings.HasPrefix(path, "/api/") || path == "/healthz" {
		s.writeError(w, http.StatusNotFound, "not found")
		return
	}

	root := filepath.Clean(s.staticDir)
	rel := strings.Trim(path, "/")
	full := filepath.Join(root, filepath.FromSlash(rel))

	// r.URL.Path is already cleaned, but keep the resolved file inside the web
	// root regardless before anything reads from it.
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		s.writeError(w, http.StatusNotFound, "not found")
		return
	}

	// The root URL and anything that is not a real file are all the app shell;
	// the client-side router takes over from there.
	target := full
	if rel == "" {
		target = filepath.Join(root, "index.html")
	} else if info, err := os.Stat(full); err != nil || info.IsDir() {
		target = filepath.Join(root, "index.html")
	}

	f, err := os.Open(target)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "not found")
		return
	}
	defer f.Close()
	if stat, err := f.Stat(); err == nil {
		http.ServeContent(w, r, filepath.Base(target), stat.ModTime(), f)
		return
	}
	s.writeError(w, http.StatusNotFound, "not found")
}

// requireAuth validates the bearer access token and confirms the session it
// names is still live before the handler runs.
//
// The token proves the caller minted it; the session check proves it still may
// be used. Access tokens are a cache, and this is where the cache is checked.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := token.BearerToken(r.Header.Get("Authorization"))
		if err != nil {
			s.writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		principal, sessionID, err := s.tokens.Parse(raw, token.TypeAccess)
		if err != nil {
			s.writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if _, err := s.sessions.Load(r.Context(), s.db, sessionID); err != nil {
			s.writeError(w, http.StatusUnauthorized, "this sign-in is no longer active")
			return
		}

		ctx := context.WithValue(r.Context(), principalKey, principal)
		ctx = context.WithValue(ctx, sessionIDKey, sessionID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recover turns a panic into a 500 instead of killing the connection, and keeps
// the request going if the handler recovers cleanly.
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("handler panicked", "error", rec, "path", r.URL.Path)
				s.writeError(w, http.StatusInternalServerError, "the server hit an unexpected error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Principal returns the authenticated principal the middleware resolved, or the
// zero value if the handler ran outside it.
func Principal(ctx context.Context) policy.Principal {
	p, _ := ctx.Value(principalKey).(policy.Principal)
	return p
}

// SessionID returns the session the authenticated request belongs to.
func SessionID(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey).(string)
	return id
}

// clientIP resolves the caller's address. It prefers the value a proxy
// middleware (middleware.ClientIPFromXFF) has published, and otherwise parses
// the direct peer from RemoteAddr. When nothing resolves it fails closed to the
// zero address, which relaxes per-address lockout rather than inventing an IP.
func clientIP(r *http.Request) netip.Addr {
	if ip := middleware.GetClientIPAddr(r.Context()); ip.IsValid() {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ip
}

// decodeJSON reads a small JSON body into v, rejecting unknown fields so a
// typo'd request fails loudly instead of being silently half-applied.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	defer r.Body.Close()
	return strictJSON(r).Decode(v)
}

// strictJSON decodes a body, rejecting unknown fields so a typo'd request fails
// loudly instead of being silently half-applied.
func strictJSON(r *http.Request) *json.Decoder {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec
}

// writeJSON encodes v as the response body.
func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError is the one shape every failure response takes.
func (s *Server) writeError(w http.ResponseWriter, code int, message string) {
	s.writeJSON(w, code, map[string]string{"error": message})
}

// mapErrorCode returns the HTTP status code for an error.
//
// The policy denial codes map to 403, the store errors map to 404/409,
// everything else is 500 or 400 depending on whether it's a client or server
// fault. This keeps the logic in one place rather than scattered across handlers.
func mapErrorCode(err error) int {
	var denied *policy.Denied
	if errors.As(err, &denied) {
		switch denied.Code {
		case policy.CodeNotCalendarOwner, policy.CodeWorkgroupClosed,
			policy.CodeNotWorkgroupAdmin, policy.CodeFreeBusyHidden,
			policy.CodeNotAWorkgroupCalendar:
			return http.StatusForbidden
		default:
			return http.StatusForbidden
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, store.ErrConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, calendar.ErrDenied) {
		return http.StatusForbidden
	}
	if errors.Is(err, calendar.ErrStale) {
		return http.StatusConflict
	}
	if errors.Is(err, calendar.ErrBadRequest) {
		return http.StatusBadRequest
	}
	if errors.Is(err, meetings.ErrInvalid) {
		return http.StatusBadRequest
	}
	if errors.Is(err, meetings.ErrDenied) {
		return http.StatusForbidden
	}
	if errors.Is(err, ErrInvalid) {
		return http.StatusBadRequest
	}
	// Unknown: assume server error to be safe
	return http.StatusInternalServerError
}

func notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusMethodNotAllowed)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
}

// secondsUntil is a duration-to-seconds helper for the expires_in fields.
func secondsUntil(t time.Time) int64 {
	return int64(time.Until(t).Seconds())
}

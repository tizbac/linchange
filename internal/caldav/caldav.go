package caldav

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/mailauth"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/store"
)

func init() {
	// Register WebDAV/CalDAV methods that chi doesn't support by default.
	//
	// The names are the ones on the wire, so registering them is how chi learns
	// to route them: a method it has never heard of is answered 405 by the
	// router before any handler runs, which is how a server that "supports
	// CalDAV" ends up refusing every PROPFIND.
	chi.RegisterMethod("PROPFIND")
	chi.RegisterMethod("REPORT")
	chi.RegisterMethod("MKCALENDAR")
	chi.RegisterMethod("PROPPATCH")
	chi.RegisterMethod("MOVE")
	chi.RegisterMethod("COPY")
	chi.RegisterMethod("LOCK")
	chi.RegisterMethod("UNLOCK")
}

// Server is the CalDAV service: RFC 4791 on top of WebDAV (RFC 4918), with
// synchronisation (RFC 6578) and the Apple properties that Apple's clients
// require in order to show a calendar as editable rather than read-only.
//
// The URL layout is the one RFC 6764 and RFC 4791 describe, and clients hard-code
// it:
//
//	/caldav/                                       the service root
//	/caldav/principals/                            the principal collection
//	/caldav/principals/alice@example.com/          a principal
//	/caldav/alice@example.com/                     that principal's calendar home
//	/caldav/alice@example.com/work/                a calendar collection
//	/caldav/alice@example.com/work/ev-1.ics        a calendar object resource
//
// The principal is a path segment, so the layout has three levels below the
// prefix where a two-level shape would look simpler. A client is given the
// principal URL by a discovery document and then walks down from it; it does not
// guess. Collapsing the levels would make every calendar collection look like
// every other principal's, and two users would share one.
type Server struct {
	prefix    string
	calSvc    *calendar.Service
	db        *sqlx.DB
	auth      *mailauth.Verifier
	calendars *store.Calendars
	events    *store.Events
	changes   *store.CalendarChanges
	// maxObjectBytes is the largest iCalendar document accepted in a PUT.
	// A client that sends more than this gets 413 rather than a database error,
	// and the limit is advertised in a DAV property so a client can see it
	// instead of discovering it by being refused.
	maxObjectBytes int64
}

// The default PUT limit. It is generous enough for a large recurring series
// with a long description and small enough that one request cannot hold a
// connection and a large allocation for as long as it likes.
const defaultMaxObjectBytes = 1 << 20

// NewServer creates a CalDAV server.
//
// The verifier is the same one the IMAP, POP3 and SMTP listeners use. A calendar
// client and a mail client must not come to disagree about who an account is, so
// they are not two checks that happen to look alike: they are one check.
func NewServer(prefix string, calSvc *calendar.Service, db *sqlx.DB, auth *mailauth.Verifier) *Server {
	if prefix == "" {
		prefix = "/caldav"
	}
	// A nil prefix must not collapse the tree: with it, every request would
	// answer at the site root and a CalDAV client would be sharing a namespace
	// with the web application.
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	prefix = strings.TrimSuffix(prefix, "/")
	return &Server{
		prefix:         prefix,
		calSvc:         calSvc,
		db:             db,
		auth:           auth,
		calendars:      store.NewCalendars(),
		events:         store.NewEvents(),
		changes:        store.NewCalendarChanges(),
		maxObjectBytes: defaultMaxObjectBytes,
	}
}

// Handler returns the chi router for CalDAV endpoints.
//
// The handler owns its own prefix rather than being mounted under it. That is
// deliberate for one reason: RFC 6764 puts the discovery document at the root of
// the origin, so /.well-known/caldav has to answer, and a handler mounted at
// /caldav cannot serve a path outside its own mount point.
//
// The routes are written out flat rather than nested. Nesting them with
// r.Route would make each collection respond only to the spelling with a trailing
// slash, and clients disagree about which that is: some send /caldav/user/cal/,
// some send /caldav/user/cal. Both are registered for every collection, because a
// redirect between them is a round trip that some clients will not make and a 404
// looks to the others like a calendar that has been deleted.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.recover)

	// A collection is addressed two ways and answers to both, so the two spellings
	// are registered together for every level rather than one at a time.
	both := func(target chi.Router, base string, methods map[string]http.HandlerFunc) {
		for verb, handler := range methods {
			target.MethodFunc(verb, base, handler)
			target.MethodFunc(verb, base+"/", handler)
		}
	}

	// RFC 6764 puts discovery at the root of the origin rather than under the
	// service prefix: a client given only a domain name asks /.well-known/caldav,
	// and nested under the prefix it would have answered at
	// /caldav/.well-known/caldav, which nothing looks for.
	//
	// It is answered before authentication, and it has to be. The client that
	// follows this redirect is one holding a domain name and no credentials --
	// that is the whole population RFC 6764 is written for -- and a 401 leaves it
	// with nothing: a redirect tells it where the service is, and nothing about
	// where the service is is a secret. The redirect target is a fixed path.
	both(r, "/.well-known/caldav", map[string]http.HandlerFunc{
		"GET":  s.handleWellKnown,
		"HEAD": s.handleWellKnown,
		// OPTIONS is here for the same reason: a client checks what the service
		// speaks before it has any reason to log in, and the answer describes the
		// server rather than the caller's resources.
		"OPTIONS": s.handleOptions,
	})

	// Everything else is behind the login, including the service root: a PROPFIND
	// there answers with the caller's own principal URL, which is nothing to a
	// stranger but is not nothing.
	r.Group(func(private chi.Router) {
		private.Use(s.authenticate)

		both(private, s.prefix, map[string]http.HandlerFunc{
			"OPTIONS":  s.handleOptions,
			"PROPFIND": s.handleRootPropfind,
		})

		// RFC 6764: the principal collection, and the principals in it. Only the
		// caller's own principal is reachable, which handlePrincipal enforces.
		both(private, s.prefix+"/principals", map[string]http.HandlerFunc{
			"OPTIONS":  s.handleOptions,
			"PROPFIND": s.handlePrincipalCollection,
			"REPORT":   s.handlePrincipalSearch,
		})
		both(private, s.prefix+"/principals/{principal}", map[string]http.HandlerFunc{
			"OPTIONS":  s.handleOptions,
			"PROPFIND": s.handlePrincipal,
		})

		// A principal's calendar home set.
		both(private, s.prefix+"/{principal}", map[string]http.HandlerFunc{
			"OPTIONS":  s.handleOptions,
			"PROPFIND": s.handleCalendarHome,
			"REPORT":   s.handleCalendarHomeReport,
		})

		// A workgroup's calendars.
		//
		// A team calendar has no single owner to hang it under, so it lives at the
		// workgroup's own URL rather than inside one member's home set. Two
		// consequences, both of them the point: every member is given the same URL for
		// the same calendar, so a subscribed URL keeps working when the person who
		// subscribed it leaves; and a team calendar does not belong to whichever member
		// happened to create it.
		both(private, s.prefix+"/workgroup/{group}", map[string]http.HandlerFunc{
			"OPTIONS":  s.handleOptions,
			"PROPFIND": s.handleWorkgroupHome,
		})

		// The calendar collection, under a principal's home set or a workgroup's. The
		// same handlers serve both and the difference is only in how the path resolves
		// to a row, so they are registered from one list: a method that works on a
		// personal calendar and answers 405 on a team one is a bug nobody reports,
		// because the client that hit it silently stops syncing that calendar.
		for _, parent := range []string{s.prefix + "/{principal}", s.prefix + "/workgroup/{group}"} {
			both(private, parent+"/{calendar}", s.collectionMethods())
			both(private, parent+"/{calendar}/{object}", s.objectMethods())
		}
	})

	// A path this service does not host answers in DAV rather than with chi's
	// plain-text 404. A client that has misspelled a calendar name reads a body it
	// can parse and shows the user which URL it tried; a bare text 404 becomes a
	// generic "the calendar is gone" in most clients.
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		fail(w, &davError{
			Status: http.StatusNotFound,
			Code:   "DAV:not-found",
			Detail: "there is no such resource at this URL",
		})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCALENDAR")
		fail(w, &davError{
			Status: http.StatusMethodNotAllowed,
			Detail: "this method is not one this resource accepts",
		})
	})

	return r
}

// collectionMethods are the methods a calendar collection answers.
//
// A GET is here because RFC 4918 does not define one, and several clients send it
// as a liveness check before subscribing to a calendar. Answering with the same
// body as a PROPFIND is harmless to a client that understands it and useful to one
// that does not; a 405 makes the client conclude the calendar has been deleted,
// which is a conclusion the user then reports as "my calendar disappeared".
func (s *Server) collectionMethods() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"OPTIONS":    s.handleOptions,
		"PROPFIND":   s.handleCalendarPropfind,
		"REPORT":     s.handleCalendarReport,
		"MKCALENDAR": s.handleMkCalendar,
		"PROPPATCH":  s.handlePropPatch,
		"DELETE":     s.handleDeleteCalendar,
		"GET":        s.handleCalendarGet,
		"HEAD":       s.handleCalendarGet,
	}
}

// objectMethods are the methods a calendar object resource answers.
//
// This is the only place an iCalendar document is read or written.
func (s *Server) objectMethods() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"OPTIONS": s.handleOptions,
		"GET":     s.handleGetObject,
		"HEAD":    s.handleGetObject,
		"PUT":     s.handlePutObject,
		"DELETE":  s.handleDeleteObject,
	}
}

// handleWellKnown answers the discovery document.
//
// The redirect is a 302 rather than a 301 because the target here is chosen per
// request -- the service root for the authenticated principal, whose calendar
// home is under their own address -- and a permanently cached redirect from one
// principal's root to the same URL would then be wrong for the next one.
func (s *Server) handleWellKnown(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.prefix+"/", http.StatusFound)
}

// handleOptions advertises what this server accepts.
//
// The DAV header is not decoration: a client reads it to decide whether the
// server speaks WebDAV class 1 or 2 before it tries anything, and answers
// "addressbook" or "calendar-access" decides whether it offers to configure the
// account at all. Getting it wrong produces a client that connects and then
// shows an empty calendar with no error anywhere.
func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	// 3 declares the extended-MKCOL namespace, which is how a client learns it
	// may send MKCALENDAR. RFC 5689 also requires the namespace itself, since a
	// client that does not know the token has no way to look up what it means.
	w.Header().Set("DAV", "1, 2, 3, access-control, calendar-access")
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCALENDAR")
	w.Header().Set("MS-Author-Via", "DAV")
	w.WriteHeader(http.StatusOK)
}

// principalKey is the context key the authenticated principal is stored under.
type principalKey struct{}

// principalOf returns the authenticated principal for the request.
func principalOf(r *http.Request) policy.Principal {
	p, _ := r.Context().Value(principalKey{}).(policy.Principal)
	return p
}

// authenticate resolves the caller's identity from HTTP Basic credentials.
//
// Basic is not a choice: RFC 4791 §6.3.1 requires it, and it is what Apple
// Calendar, Thunderbird, Evolution and every phone's calendar app send. There is
// no cookie and no bearer token, so a client cannot be told to use one.
//
// The failure answers carry a challenge, because a client with no challenge treats
// a bare 401 as an endpoint that does not exist rather than as a request for a
// password, and reports nothing to the user at all.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil {
			// Without a verifier nothing can be authenticated, and letting
			// requests through would serve one user's calendar to another. The
			// server says so rather than quietly answering 200 to everything.
			s.challenge(w, "this CalDAV service has no authentication configured")
			return
		}

		login, pw, ok := r.BasicAuth()
		if !ok {
			s.challenge(w, "")
			return
		}

		who, err := s.auth.Authenticate(r.Context(), login, pw, policy.ActionCalDAV)
		if err != nil {
			// ErrRejected and ErrUnavailable are answered identically. A client
			// that could tell them apart could use the difference to learn whether
			// an account exists or whether a directory is down.
			s.challenge(w, "")
			return
		}

		ctx := context.WithValue(r.Context(), principalKey{}, who)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// challenge answers 401 with the realm a client needs in order to prompt.
//
// The realm is the service root rather than a bare word: it is the string a
// client shows beside the password box, and "linchange CalDAV" tells a user
// which of several accounts on the same server they are giving a password to.
func (s *Server) challenge(w http.ResponseWriter, detail string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="linchange CalDAV", charset="UTF-8"`)
	if detail != "" {
		w.Header().Set("X-CalDAV-Error", detail)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
}

// recover turns a panic in a handler into a DAV error element rather than a
// dropped connection.
//
// A DAV client reads the status line and the body separately, and a connection
// that closes mid-response leaves it with a parse error rather than with
// anything it can show a user. A 500 with a DAV:error body is at least a
// response it understands.
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				w.Header().Set("Content-Type", "application/xml; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write(render(el(nsDAV, "error", leaf(nsDAV, "responsedescription",
					"the server hit an unexpected error"))))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Resolving a path to a resource
// ---------------------------------------------------------------------------

// resolvePrincipal works out which principal a path segment names.
//
// A client is supposed to address its own calendar home, so the segment is
// normally the caller's own address. It is compared case-insensitively and with
// surrounding whitespace ignored, because the same account arrives spelled both
// ways over the years and a client that cannot open its own calendar after a
// server upgrade is a bug report nobody can act on.
//
// A path naming somebody else's principal is not resolved to that principal.
// CalDAV has no delegation model here, so there is nothing to authorise against
// and answering with the caller's own home would report the wrong URL rather
// than an error; the caller is told the path is not theirs, which is a question
// it can act on.
func (s *Server) resolvePrincipal(r *http.Request) (policy.Principal, *davError) {
	raw := chi.URLParam(r, "principal")
	if unescaped, err := decodeSegment(raw); err == nil {
		raw = unescaped
	}
	who := principalOf(r)
	if !strings.EqualFold(strings.TrimSpace(raw), who.Address) {
		return policy.Principal{}, &davError{
			Status: http.StatusForbidden,
			Code:   "DAV:need-privileges",
			Detail: "a principal may only address its own calendar home",
		}
	}
	return who, nil
}

// decodeSegment undoes the percent-encoding a client applied to a path segment.
//
// A principal is an email address, so its segment contains an @ and a dot and,
// in every real deployment, a percent-escaped @. chi hands over the raw segment,
// so it is decoded here. An address containing a literal slash cannot be a
// segment at all, and the error is left to the caller: a name that will not
// decode is a name that cannot exist.
func decodeSegment(seg string) (string, error) {
	if !strings.Contains(seg, "%") {
		return seg, nil
	}
	var b strings.Builder
	for i := 0; i < len(seg); {
		if seg[i] != '%' {
			b.WriteByte(seg[i])
			i++
			continue
		}
		if i+2 >= len(seg) {
			return seg, errors.New("caldav: truncated percent-escape in a path segment")
		}
		hi, err1 := unhex(seg[i+1])
		lo, err2 := unhex(seg[i+2])
		if err1 != nil || err2 != nil {
			return seg, errors.New("caldav: invalid percent-escape in a path segment")
		}
		b.WriteByte(hi<<4 | lo)
		i += 3
	}
	return b.String(), nil
}

func unhex(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, errors.New("caldav: not a hex digit")
}

// calendarName is the segment a client uses for a calendar.
//
// It is compared folded, for the same reason the database index is: "Work" and
// "work" are one calendar as far as a client is concerned, and refusing one
// spelling of a calendar a client just listed is the kind of thing that shows up
// as "my calendar disappeared".
func calendarName(r *http.Request) string {
	raw := chi.URLParam(r, "calendar")
	if unescaped, err := decodeSegment(raw); err == nil {
		raw = unescaped
	}
	return raw
}

// resolveCalendar finds the calendar a request's path names.
//
// Two shapes of path reach here, and they are the two the RFCs describe: a
// calendar under a principal's own home set, and a calendar under a workgroup's.
// The workgroup one is not restricted to members of that workgroup, because a
// team calendar that only its own members can see cannot be booked by the people
// it exists to be booked by. Writing is a separate question, answered by the
// write path through the same policy the web interface uses.
func (s *Server) resolveCalendar(r *http.Request, who policy.Principal) (store.Calendar, *davError) {
	ctx := r.Context()
	name := calendarName(r)
	if name == "" {
		return store.Calendar{}, &davError{
			Status: http.StatusBadRequest,
			Code:   "DAV:valid-resourcetype",
			Detail: "the request names no calendar",
		}
	}

	if raw := chi.URLParam(r, "group"); raw != "" {
		id, ok := s.pathID(r, "group")
		if !ok {
			return store.Calendar{}, &davError{
				Status: http.StatusBadRequest,
				Detail: "the workgroup in the path is not a number",
			}
		}
		rows, err := s.calendars.ByWorkgroup(ctx, s.db, id)
		if err != nil {
			return store.Calendar{}, s.databaseError(ctx, "looking up the workgroup calendars", err)
		}
		return findByName(rows, name)
	}

	rows, err := s.calendars.ByOwner(ctx, s.db, who.Address)
	if err != nil {
		return store.Calendar{}, s.databaseError(ctx, "listing the calendars", err)
	}
	return findByName(rows, name)
}

// findByName picks a calendar out of a set by its path segment.
//
// The comparison is folded rather than exact, because a client lists a calendar
// and then addresses it, and a server that is strict about the case of a name it
// just handed out is a server whose calendars intermittently vanish.
func findByName(rows []store.Calendar, name string) (store.Calendar, *davError) {
	for _, cal := range rows {
		if strings.EqualFold(cal.Name, name) {
			return cal, nil
		}
	}
	return store.Calendar{}, &davError{
		Status: http.StatusNotFound,
		Code:   "DAV:not-found",
		Detail: "there is no calendar called " + name + " here",
	}
}

// pathID reads a numeric path parameter.
func (s *Server) pathID(r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// databaseError turns a store failure into a DAV error.
//
// A store failure is a 500 even where the row was also absent, and the two are
// kept apart on purpose: reporting a database outage as "no such calendar" tells
// a client to delete its subscription, and a client that believes it will.
func (s *Server) databaseError(ctx context.Context, what string, err error) *davError {
	if errors.Is(err, store.ErrNotFound) {
		return &davError{Status: http.StatusNotFound, Code: "DAV:not-found", Detail: what + ": not found"}
	}
	// The underlying message is deliberately not echoed to the client. It names
	// tables and columns, which is more than a caller of a calendar service
	// needs to know and less than would help anybody.
	_ = ctx
	return &davError{
		Status: http.StatusInternalServerError,
		Detail: "the calendar store could not be reached",
	}
}

// ---------------------------------------------------------------------------
// URLs
// ---------------------------------------------------------------------------

// principalURL is the URL of a principal's resource.
func (s *Server) principalURL(address string) string {
	return s.prefix + "/principals/" + escapeSegment(address) + "/"
}

// calendarHomeURL is the URL of a principal's calendar home set.
func (s *Server) calendarHomeURL(address string) string {
	return s.prefix + "/" + escapeSegment(address) + "/"
}

// calendarURL is the URL of a calendar collection.
//
// A workgroup calendar is addressed under the workgroup rather than under any one
// member, so that every member is given the same URL for the same calendar and a
// subscribed URL keeps working when the person who subscribed it leaves.
func (s *Server) calendarURL(cal store.Calendar) string {
	if cal.Owner != nil {
		return s.calendarHomeURL(*cal.Owner) + escapeSegment(cal.Name) + "/"
	}
	if cal.WorkgroupID != nil {
		return s.prefix + "/workgroup/" + strconvItoa(*cal.WorkgroupID) + "/" + escapeSegment(cal.Name) + "/"
	}
	// Neither is set, which the schema forbids. Answering the service root is
	// better than emitting a path with an empty segment in it.
	return s.prefix + "/"
}

// objectURL is the URL of one calendar object resource.
//
// No trailing slash: an object resource is not a collection, and RFC 4918 §8.3
// requires the two to be told apart by their URLs. A client is asked to append
// the name to the collection's href, and a name with a slash on the end of it
// resolves to nothing.
func (s *Server) objectURL(cal store.Calendar, ev store.Event) string {
	return s.calendarURL(cal) + escapeSegment(objectName(ev))
}

// objectName is the file name a client sees for an event.
//
// The UID is the identity, and the suffix is what a client that decides how to
// open a resource looks for. An override is distinguished from its master,
// because a master and an override share a UID and are two resources: a client
// that fetched both under one name would keep only the second, silently losing
// one occurrence of a series.
func objectName(ev store.Event) string {
	if ev.RecurrenceID != nil {
		return escapeUID(ev.UID) + "_" + ev.RecurrenceID.UTC().Format("20060102T150405Z") + ".ics"
	}
	return escapeUID(ev.UID) + ".ics"
}

// escapeUID makes a UID safe as a path segment.
//
// A UID is chosen by the client that created the event, and iCalendar allows
// almost anything in one, including a slash. A UID holding a slash cannot be a
// path segment, so it is percent-encoded here and decoded again when a request
// comes back in. The mapping is lossless: encodeSegment below reverses it.
func escapeUID(uid string) string { return escapeSegment(uid) }

// escapeSegment percent-encodes the characters that would otherwise change a
// URL's structure.
//
// Everything outside the unreserved set of RFC 3986 is encoded, not just the
// delimiters. Encoding more than strictly necessary costs a few bytes in an href
// and removes a whole class of client-side path confusion, and a client that
// decodes a path segment before matching it -- which every one does -- gets the
// original name back either way.
func escapeSegment(s string) string {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

const hexDigits = "0123456789ABCDEF"

func strconvItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// hrefOf turns an absolute-or-relative path into the href a DAV document
// carries.
//
// RFC 4918 §8.3 requires every href in a multistatus to be either an absolute
// URI or an absolute path, and requires that the same resource be reported the
// same way throughout a response. The paths here are already built from the
// configured prefix and are used verbatim, so a deployment behind a proxy that
// strips a path prefix still produces hrefs that resolve, which is the property
// that actually matters and the one a client checks by fetching what it was
// given.
//
// The path is cleaned, but the trailing slash a collection's URL carries is put
// back: path.Clean removes it, and a collection's href has to end in one. Clients
// treat "/caldav/alice@example.com/Work" and "/caldav/alice@example.com/Work/" as
// two resources -- that is the whole point of the slash -- and a client that
// matched what it was given against the URL it asked for would then conclude the
// calendar had been renamed.
func hrefOf(p string) string {
	if p == "" {
		return "/"
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	cleaned := path.Clean(p)
	if cleaned != "/" && strings.HasSuffix(p, "/") && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned
}

// icalFromDocument turns a parsed VCALENDAR into the one event a PUT carries.
//
// A PUT holds a single calendar object resource, so a body with several VEVENTs
// is a body this server cannot store as one resource. It is refused rather than
// half-stored: a client that sent a batch would be told it worked, and the
// events it could not see would be missing from the copy on the server.
func icalFromDocument(doc *ical.Document) (ical.Event, error) {
	if len(doc.Events) == 0 {
		return ical.Event{}, errors.New("caldav: the document holds no VEVENT")
	}
	if len(doc.Events) > 1 {
		return ical.Event{}, errors.New("caldav: a calendar object resource holds exactly one VEVENT")
	}
	ev := doc.Events[0]
	if strings.TrimSpace(ev.UID) == "" {
		return ical.Event{}, errors.New("caldav: the VEVENT has no UID, so there is no name to store it under")
	}
	return ev, nil
}

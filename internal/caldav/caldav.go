package caldav

import (
	"encoding/xml"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/ical"
)

func init() {
	// Register WebDAV/CalDAV methods that chi doesn't support by default
	chi.RegisterMethod("PROPFIND")
	chi.RegisterMethod("REPORT")
	chi.RegisterMethod("MKCALENDAR")
	chi.RegisterMethod("PROPPATCH")
	chi.RegisterMethod("MOVE")
	chi.RegisterMethod("COPY")
	chi.RegisterMethod("LOCK")
	chi.RegisterMethod("UNLOCK")
}

// Server is the CalDAV server implementation.
//
// It implements RFC 4791 (CalDAV) on top of WebDAV (RFC 4918).
// The main endpoints are:
//
//   - PROPFIND /caldav/ - list calendars
//   - PROPFIND /caldav/{calendar}/ - calendar properties
//   - REPORT /caldav/{calendar}/ - calendar-query for events in range
//   - PUT /caldav/{calendar}/{uid}.ics - create/update event
//   - GET /caldav/{calendar}/{uid}.ics - get event
//   - DELETE /caldav/{calendar}/{uid}.ics - delete event
//   - MKCALENDAR /caldav/{calendar}/ - create calendar
//   - PROPPATCH /caldav/{calendar}/ - update calendar properties
//
// All paths are under a configurable prefix (default "/caldav").
type Server struct {
	prefix    string
	calSvc    *calendar.Service
	db        *sqlx.DB
	principal policy.Principal
}

// NewServer creates a CalDAV server.
func NewServer(prefix string, calSvc *calendar.Service, db *sqlx.DB) *Server {
	if prefix == "" {
		prefix = "/caldav"
	}
	return &Server{
		prefix: prefix,
		calSvc: calSvc,
		db:     db,
	}
}

// Handler returns the chi router for CalDAV endpoints.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.authMiddleware)

	r.Route(s.prefix, func(r chi.Router) {
		// Principal home set
		r.Get("/", s.handleHome)

		// Calendar operations
		r.Route("/{calendar}", func(r chi.Router) {
			r.Get("/", s.handleCalendar)
			r.Head("/", s.handleCalendar)
			r.Options("/", s.handleOptions)
			r.MethodFunc("PROPFIND", "/", s.handleCalendarPropfind)
			r.MethodFunc("REPORT", "/", s.handleCalendarReport)
			r.MethodFunc("MKCALENDAR", "/", s.handleMkCalendar) // MKCALENDAR
			r.MethodFunc("PROPPATCH", "/", s.handlePropPatch)   // PROPPATCH
			r.Delete("/", s.handleDeleteCalendar)               // DELETE calendar

			// Event operations
			r.Route("/{event}", func(r chi.Router) {
				r.Get("/", s.handleEvent)
				r.Head("/", s.handleEvent)
				r.Put("/", s.handlePutEvent)
				r.Delete("/", s.handleDeleteEvent)
			})
		})
	})

	// RFC 6764 puts the discovery URI at the root of the origin, not under the
	// service prefix: a client that is given only a domain name requests
	// /.well-known/caldav. Nested inside s.prefix it would have answered at
	// /caldav/.well-known/caldav, which nothing looks for.
	r.Get("/.well-known/caldav", s.handleWellKnown)

	return r
}

// authMiddleware extracts the principal from the request.
// In production, this would validate Basic Auth or Bearer token.
// For now, it uses a test principal.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TODO: Implement proper authentication
		// For now, use a test principal
		ctx := r.Context()
		// In real implementation, extract from Authorization header
		// and validate against auth system
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ---------------------------------------------------------------------------
// XML Structures for WebDAV/CalDAV
// ---------------------------------------------------------------------------

// MultiStatus is the DAV:multistatus response.
type MultiStatus struct {
	XMLName   xml.Name   `xml:"D:multistatus"`
	XmlnsD    string     `xml:"xmlns:D,attr"`
	XmlnsC    string     `xml:"xmlns:C,attr"`
	Responses []Response `xml:"D:response"`
}

type Response struct {
	Href     string     `xml:"D:href"`
	Propstat []PropStat `xml:"D:propstat,omitempty"`
	Status   string     `xml:"D:status,omitempty"`
	Error    *Error     `xml:"D:error,omitempty"`
	RespDesc string     `xml:"D:responsedescription,omitempty"`
}

type PropStat struct {
	Prop   *Property `xml:"D:prop"`
	Status string    `xml:"D:status"`
}

type Property struct {
	// Calendar properties
	DisplayName                   string               `xml:"D:displayname,omitempty"`
	CalendarDescription           string               `xml:"C:calendar-description,omitempty"`
	CalendarTimezone              string               `xml:"C:calendar-timezone,omitempty"`
	SupportedCalendarComponentSet []SupportedComponent `xml:"C:supported-calendar-component-set,omitempty"`
	CalendarHomeSet               *Href                `xml:"D:calendar-home-set,omitempty"`
	CurrentUserPrincipal          *Href                `xml:"D:current-user-principal,omitempty"`
	ResourceType                  *ResourceType        `xml:"D:resourcetype,omitempty"`
	Owner                         *Href                `xml:"D:owner,omitempty"`
	SyncToken                     string               `xml:"D:sync-token,omitempty"`

	// Event properties
	GetContentType   string        `xml:"D:getcontenttype,omitempty"`
	GetETag          string        `xml:"D:getetag,omitempty"`
	GetContentLength int64         `xml:"D:getcontentlength,omitempty"`
	CalendarData     *CalendarData `xml:"C:calendar-data,omitempty"`
}

type Href struct {
	Href string `xml:"D:href,attr"`
}

type SupportedComponent struct {
	Name string `xml:"name,attr"`
}

type ResourceType struct {
	Collection *struct{} `xml:"D:collection,omitempty"`
	Calendar   *struct{} `xml:"C:calendar,omitempty"`
}

type CalendarData struct {
	Content string `xml:",innerxml"`
}

type Error struct {
	Code string `xml:"code,attr"`
}

// PropfindRequest is the parsed PROPFIND request body.
type PropfindRequest struct {
	XMLName  xml.Name  `xml:"D:propfind"`
	Prop     *Prop     `xml:"D:prop"`
	AllProp  *struct{} `xml:"D:allprop,omitempty"`
	PropName *struct{} `xml:"D:propname,omitempty"`
}

type Prop struct {
	DisplayName                   *struct{}           `xml:"D:displayname"`
	CalendarDescription           *struct{}           `xml:"C:calendar-description"`
	CalendarTimezone              *struct{}           `xml:"C:calendar-timezone"`
	SupportedCalendarComponentSet *struct{}           `xml:"C:supported-calendar-component-set"`
	CalendarHomeSet               *struct{}           `xml:"D:calendar-home-set"`
	CurrentUserPrincipal          *struct{}           `xml:"D:current-user-principal"`
	ResourceType                  *struct{}           `xml:"D:resourcetype"`
	Owner                         *struct{}           `xml:"D:owner"`
	SyncToken                     *struct{}           `xml:"D:sync-token"`
	GetContentType                *struct{}           `xml:"D:getcontenttype"`
	GetETag                       *struct{}           `xml:"D:getetag"`
	GetContentLength              *struct{}           `xml:"D:getcontentlength"`
	CalendarData                  *CalendarDataFilter `xml:"C:calendar-data"`
}

type CalendarDataFilter struct {
	Comp *CompFilter `xml:"C:comp-filter"`
	All  bool        `xml:"all,attr,omitempty"`
}

type CompFilter struct {
	Name       string       `xml:"name,attr"`
	TimeRange  *TimeRange   `xml:"C:time-range,omitempty"`
	PropFilter []PropFilter `xml:"C:prop-filter,omitempty"`
}

type TimeRange struct {
	Start string `xml:"start,attr"`
	End   string `xml:"end,attr"`
}

type PropFilter struct {
	Name  string `xml:"name,attr"`
	Test  string `xml:"test,attr,omitempty"`
	Value string `xml:",chardata"`
}

// ReportRequest is the parsed REPORT request body (calendar-query).
type ReportRequest struct {
	XMLName xml.Name    `xml:"C:calendar-query"`
	Filter  *CompFilter `xml:"C:filter"`
	Prop    *Prop       `xml:"D:prop"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	// Return the calendar home set for the current user
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_ = xml.NewEncoder(w).Encode(MultiStatus{
		XmlnsD: "DAV:",
		XmlnsC: "urn:ietf:params:xml:ns:caldav",
		Responses: []Response{{
			Href: s.prefix + "/",
			Propstat: []PropStat{{
				Status: "HTTP/1.1 200 OK",
				Prop: &Property{
					CalendarHomeSet:      &Href{Href: s.prefix + "/"},
					CurrentUserPrincipal: &Href{Href: "/"},
				},
			}},
		}},
	})
}

func (s *Server) handleWellKnown(w http.ResponseWriter, r *http.Request) {
	// Redirect to the CalDAV root
	http.Redirect(w, r, s.prefix+"/", http.StatusFound)
}

func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("DAV", "1, calendar-access")
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCALENDAR")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleCalendar(w http.ResponseWriter, r *http.Request) {
	// Return calendar properties
	calendarName := chi.URLParam(r, "calendar")
	if calendarName == "" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_ = xml.NewEncoder(w).Encode(MultiStatus{
		XmlnsD: "DAV:",
		XmlnsC: "urn:ietf:params:xml:ns:caldav",
		Responses: []Response{{
			Href: path.Join(s.prefix, calendarName) + "/",
			Propstat: []PropStat{{
				Status: "HTTP/1.1 200 OK",
				Prop: &Property{
					DisplayName:                   calendarName,
					CalendarDescription:           calendarName,
					CalendarTimezone:              "UTC",
					SupportedCalendarComponentSet: []SupportedComponent{{Name: "VEVENT"}},
					ResourceType: &ResourceType{
						Collection: &struct{}{},
						Calendar:   &struct{}{},
					},
				},
			}},
		}},
	})
}

type CalendarDescription struct {
	Content string `xml:",chardata"`
}

type CalendarTimezone struct {
	Content string `xml:",chardata"`
}

func (s *Server) handleCalendarPropfind(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var req PropfindRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		// If no body, treat as allprop
		if len(body) == 0 {
			req.AllProp = &struct{}{}
		} else {
			http.Error(w, "invalid XML", http.StatusBadRequest)
			return
		}
	}

	calendarName := chi.URLParam(r, "calendar")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")

	// For simplicity, return basic calendar properties
	resp := MultiStatus{
		XmlnsD: "DAV:",
		XmlnsC: "urn:ietf:params:xml:ns:caldav",
		Responses: []Response{{
			Href: path.Join(s.prefix, calendarName) + "/",
			Propstat: []PropStat{{
				Status: "HTTP/1.1 200 OK",
				Prop: &Property{
					DisplayName:                   calendarName,
					CalendarDescription:           calendarName,
					CalendarTimezone:              "UTC",
					SupportedCalendarComponentSet: []SupportedComponent{{Name: "VEVENT"}},
					ResourceType: &ResourceType{
						Collection: &struct{}{},
						Calendar:   &struct{}{},
					},
				},
			}},
		}},
	}
	_ = xml.NewEncoder(w).Encode(resp)
}

func (s *Server) handleCalendarReport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var req ReportRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid XML", http.StatusBadRequest)
		return
	}

	// Parse time range from filter
	var start, end time.Time
	if req.Filter != nil && req.Filter.TimeRange != nil {
		start, _ = time.Parse(time.RFC3339, req.Filter.TimeRange.Start)
		end, _ = time.Parse(time.RFC3339, req.Filter.TimeRange.End)
	}
	if start.IsZero() {
		start = time.Now().AddDate(0, -1, 0)
	}
	if end.IsZero() {
		end = time.Now().AddDate(0, 2, 0)
	}

	// TODO: Query actual events from the calendar
	// For now, return empty response
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_ = xml.NewEncoder(w).Encode(MultiStatus{
		XmlnsD:    "DAV:",
		XmlnsC:    "urn:ietf:params:xml:ns:caldav",
		Responses: []Response{},
	})
}

func (s *Server) handleMkCalendar(w http.ResponseWriter, r *http.Request) {
	calendarName := chi.URLParam(r, "calendar")
	if calendarName == "" {
		http.Error(w, "calendar name required", http.StatusBadRequest)
		return
	}

	// TODO: Create calendar in database
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handlePropPatch(w http.ResponseWriter, r *http.Request) {
	// TODO: Update calendar properties
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDeleteCalendar(w http.ResponseWriter, r *http.Request) {
	// TODO: Delete calendar
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	_ = chi.URLParam(r, "calendar")
	_ = chi.URLParam(r, "event")

	// TODO: Fetch event from database and return as ICS
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
}

func (s *Server) handlePutEvent(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Parse the ICS data
	doc, err := ical.Parse(body)
	if err != nil {
		http.Error(w, "invalid iCalendar: "+err.Error(), http.StatusBadRequest)
		return
	}

	if len(doc.Events) == 0 {
		http.Error(w, "no VEVENT in iCalendar", http.StatusBadRequest)
		return
	}

	// TODO: Store event in database
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	// TODO: Delete event from database
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Helper methods
// ---------------------------------------------------------------------------

func parseHref(href string) (calendarName, eventName string) {
	parts := strings.Split(strings.Trim(href, "/"), "/")
	if len(parts) >= 2 {
		calendarName = parts[0]
		if len(parts) >= 3 {
			eventName = parts[2]
		}
	}
	return
}

func buildHref(prefix, calendarName, eventName string) string {
	if eventName != "" {
		return path.Join(prefix, calendarName, eventName)
	}
	return path.Join(prefix, calendarName) + "/"
}

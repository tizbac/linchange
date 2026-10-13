// Package httpapi is the chi router behind the REST API and the web interface.
package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/recurrence"
	"github.com/linchange/linchange/internal/store"
)

// ErrInvalid is the class of errors a malformed request produces.
var ErrInvalid = errors.New("invalid request")

// mountCalendar routes the calendar API.
//
// Every route here is under the authenticated group, and every handler
// resolves the calendar owner from the token rather than from the request.
func (s *Server) mountCalendar(r chi.Router) {
	r.Route("/calendars", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/", s.handleListCalendars)
		r.Post("/", s.handleCreateCalendar)
		r.Get("/{id}", s.handleGetCalendar)
		r.Patch("/{id}", s.handleUpdateCalendar)
		r.Delete("/{id}", s.handleDeleteCalendar)
		r.Get("/{id}/events", s.handleListEvents)
		r.Post("/{id}/events", s.handleCreateEvent)
		r.Get("/{id}/events/{eventID}", s.handleGetEvent)
		r.Patch("/{id}/events/{eventID}", s.handleUpdateEvent)
		r.Delete("/{id}/events/{eventID}", s.handleDeleteEvent)
		r.Post("/{id}/events/{eventID}/cancel", s.handleCancelEvent)

		r.Get("/{id}/freebusy", s.handleFreeBusy)
		r.Post("/{id}/itip", s.handleITIP)

		// ICS download for an event
		r.Get("/{id}/events/{eventID}.ics", s.handleEventICS)
	})
}

// calendarOwner is the principal on whose behalf the request acts.
// It is the authenticated address, always.
func calendarOwner(r *http.Request) string {
	return Principal(r.Context()).Address
}

// ---------------------------------------------------------------------------
// Calendar CRUD
// ---------------------------------------------------------------------------

type listCalendarsResponse struct {
	Calendars []store.Calendar `json:"calendars"`
}

func (s *Server) handleListCalendars(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := Principal(ctx)

	calendars, err := store.NewCalendars().ByOwner(ctx, s.db, who.Address)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	// A brand-new account has no calendars until something creates one, and the
	// web client lists them before it can create an event. EnsureDefault is
	// idempotent (it returns the existing default when there is one), so calling
	// it here guarantees the list is never empty for a fresh user without adding
	// a calendar on every request.
	if len(calendars) == 0 {
		cal, err := store.NewCalendars().EnsureDefault(ctx, s.db, who.Address, "UTC")
		if err != nil {
			s.writeError(w, mapErrorCode(err), err.Error())
			return
		}
		calendars = append(calendars, cal)
	}
	s.writeJSON(w, http.StatusOK, listCalendarsResponse{Calendars: calendars})
}

type createCalendarRequest struct {
	Name         string `json:"name"`
	Color        string `json:"color,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
	Description  string `json:"description,omitempty"`
	ShowFreeBusy *bool  `json:"showFreeBusy,omitempty"`
	IsDefault    bool   `json:"isDefault,omitempty"`
	WorkgroupID  *int64 `json:"workgroupId,omitempty"`
}

func (s *Server) handleCreateCalendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := Principal(ctx)

	var req createCalendarRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	var cal store.Calendar
	var err error
	if req.WorkgroupID != nil {
		// Creating a workgroup calendar requires being an admin of the workgroup
		cal, err = store.NewCalendars().Create(ctx, s.db, store.CreateCalendarRequest{
			WorkgroupID:  req.WorkgroupID,
			Name:         req.Name,
			Color:        req.Color,
			Timezone:     req.Timezone,
			Description:  req.Description,
			ShowFreeBusy: req.ShowFreeBusy,
			IsDefault:    req.IsDefault,
		})
	} else {
		cal, err = store.NewCalendars().Create(ctx, s.db, store.CreateCalendarRequest{
			Owner:        &who.Address,
			Name:         req.Name,
			Color:        req.Color,
			Timezone:     req.Timezone,
			Description:  req.Description,
			ShowFreeBusy: req.ShowFreeBusy,
			IsDefault:    req.IsDefault,
		})
	}
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, cal)
}

func (s *Server) handleGetCalendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}

	cal, err := store.NewCalendars().Get(ctx, s.db, id)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, cal)
}

type updateCalendarRequest struct {
	Name         *string `json:"name,omitempty"`
	Color        *string `json:"color,omitempty"`
	Timezone     *string `json:"timezone,omitempty"`
	Description  *string `json:"description,omitempty"`
	ShowFreeBusy *bool   `json:"showFreeBusy,omitempty"`
	IsDefault    *bool   `json:"isDefault,omitempty"`
}

// handleUpdateCalendar changes a calendar's settings.
//
// Renaming sets the display name as well as the name. A calendar renamed from here
// should look the same in a CalDAV client as it does in the web interface, and the
// name is the only thing this interface offers to rename it by -- so the label
// follows rather than being left showing the old name for ever.
func (s *Server) handleUpdateCalendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}

	var req updateCalendarRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	err = store.NewCalendars().Update(ctx, s.db, id, store.UpdateCalendarRequest{
		Name:         req.Name,
		DisplayName:  req.Name,
		Color:        req.Color,
		Timezone:     req.Timezone,
		Description:  req.Description,
		ShowFreeBusy: req.ShowFreeBusy,
		IsDefault:    req.IsDefault,
	})
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	updated, err := store.NewCalendars().Get(ctx, s.db, id)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteCalendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}

	if err := store.NewCalendars().Delete(ctx, s.db, id); err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Event CRUD
// ---------------------------------------------------------------------------

type listEventsResponse struct {
	Events  []calendar.View          `json:"events"`
	Skipped []calendar.SkippedSeries `json:"skipped,omitempty"`
}

type listEventsRequest struct {
	Start string `json:"start"` // RFC3339
	End   string `json:"end"`   // RFC3339
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}

	who := Principal(ctx)

	// Parse time window
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")
	if startStr == "" || endStr == "" {
		s.writeError(w, http.StatusBadRequest, "start and end query parameters are required")
		return
	}
	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "start must be RFC3339")
		return
	}
	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "end must be RFC3339")
		return
	}

	result, err := s.calendar.Query(ctx, s.db, who, calendar.Selector{
		CalendarIDs: []int64{id},
	}, recurrence.Window{Start: start, End: end})
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, listEventsResponse{
		Events:  result.Occurrences,
		Skipped: result.Skipped,
	})
}

type createEventRequest struct {
	Event ical.Event `json:"event"`
}

func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}

	var req createEventRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	who := Principal(ctx)

	if req.Event.Summary == "" {
		s.writeError(w, http.StatusBadRequest, "event summary is required")
		return
	}

	created, err := s.calendar.Create(ctx, s.db, who, calendar.Draft{
		CalendarID: id,
		Event:      req.Event,
	})
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}
	eventID, err := strconv.ParseInt(chi.URLParam(r, "eventID"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid event id")
		return
	}

	// Get the stored event
	row, err := store.NewEvents().Get(ctx, s.db, eventID)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	if row.CalendarID != id {
		s.writeError(w, http.StatusBadRequest, "event not in this calendar")
		return
	}

	// Parse it for the full model
	doc, err := ical.Parse(row.ICalBlob)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	if len(doc.Events) == 0 {
		s.writeError(w, mapErrorCode(fmt.Errorf("stored event has no VEVENT")), "stored event has no VEVENT")
		return
	}
	s.writeJSON(w, http.StatusOK, doc.Events[0])
}

type updateEventRequest struct {
	Event ical.Event `json:"event"`
	// ExpectedSequence is the sequence the client read; zero skips the check
	ExpectedSequence int `json:"expectedSequence"`
}

func (s *Server) handleUpdateEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}
	eventID, err := strconv.ParseInt(chi.URLParam(r, "eventID"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid event id")
		return
	}

	var req updateEventRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	who := Principal(ctx)

	if req.Event.Summary == "" {
		s.writeError(w, http.StatusBadRequest, "event summary is required")
		return
	}

	updated, err := s.calendar.Update(ctx, s.db, who, eventID, calendar.Draft{
		CalendarID: id,
		Event:      req.Event,
	}, req.ExpectedSequence)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}
	eventID, err := strconv.ParseInt(chi.URLParam(r, "eventID"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid event id")
		return
	}

	if err := store.NewEvents().Delete(ctx, s.db, eventID); err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCancelEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}
	eventID, err := strconv.ParseInt(chi.URLParam(r, "eventID"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid event id")
		return
	}

	who := Principal(ctx)

	cancelled, err := s.calendar.Cancel(ctx, s.db, who, eventID)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, cancelled)
}

// ---------------------------------------------------------------------------
// Free/busy
// ---------------------------------------------------------------------------

type freeBusyRequest struct {
	Addresses []string `json:"addresses"`
	Start     string   `json:"start"` // RFC3339
	End       string   `json:"end"`   // RFC3339
}

type freeBusyResponse struct {
	Availabilities []calendar.Availability `json:"availabilities"`
}

func (s *Server) handleFreeBusy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Support both query params (GET) and JSON body (POST)
	var addresses []string
	if r.Method == http.MethodGet {
		addresses = strings.Split(r.URL.Query().Get("addresses"), ",")
	} else {
		var req freeBusyRequest
		if err := decodeJSON(w, r, &req); err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		addresses = req.Addresses
	}
	who := Principal(ctx)

	if len(addresses) == 0 {
		s.writeError(w, http.StatusBadRequest, "at least one address is required")
		return
	}

	var start, end time.Time
	if r.Method == http.MethodGet {
		var err error
		startStr := r.URL.Query().Get("start")
		endStr := r.URL.Query().Get("end")
		if startStr == "" || endStr == "" {
			s.writeError(w, http.StatusBadRequest, "start and end are required")
			return
		}
		start, err = time.Parse(time.RFC3339, startStr)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "start must be RFC3339")
			return
		}
		end, err = time.Parse(time.RFC3339, endStr)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "end must be RFC3339")
			return
		}
	} else {
		// Parse from the request body already done above
		var req freeBusyRequest
		_ = decodeJSON(w, r, &req) // already decoded
		start, _ = time.Parse(time.RFC3339, req.Start)
		end, _ = time.Parse(time.RFC3339, req.End)
	}

	avail, err := s.calendar.FreeBusy(ctx, s.db, who, calendar.FreeBusyRequest{
		Addresses: addresses,
		Window:    recurrence.Window{Start: start, End: end},
	})
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, freeBusyResponse{Availabilities: avail})
}

// ---------------------------------------------------------------------------
// iTIP inbound
// ---------------------------------------------------------------------------

type itipRequest struct {
	Document   []byte `json:"document"`
	AutoAccept bool   `json:"autoAccept,omitempty"`
}

type itipResponse struct {
	Method   string `json:"method"`
	Action   string `json:"action"`
	UID      string `json:"uid,omitempty"`
	Sequence int    `json:"sequence,omitempty"`
	Reply    []byte `json:"reply,omitempty"`
}

func (s *Server) handleITIP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req itipRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	who := Principal(ctx)

	if len(req.Document) == 0 {
		s.writeError(w, http.StatusBadRequest, "document is required")
		return
	}

	result, err := s.calendar.Apply(ctx, s.db, who, calendar.Incoming{
		Document:   req.Document,
		AutoAccept: req.AutoAccept,
	})
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, itipResponse{
		Method:   string(result.Method),
		Action:   string(result.Action),
		UID:      result.UID,
		Sequence: result.Sequence,
		Reply:    result.Reply,
	})
}

// ---------------------------------------------------------------------------
// ICS download
// ---------------------------------------------------------------------------

func (s *Server) handleEventICS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}
	eventID, err := strconv.ParseInt(chi.URLParam(r, "eventID"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid event id")
		return
	}

	row, err := store.NewEvents().Get(ctx, s.db, eventID)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	if row.CalendarID != id {
		s.writeError(w, http.StatusBadRequest, "event not in this calendar")
		return
	}

	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"event-%d.ics\"", eventID))
	w.Write(row.ICalBlob)
}

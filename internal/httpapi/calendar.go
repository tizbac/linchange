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

	"github.com/linchange/linchange/internal/auth/policy"
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

		// Sharing: who else may read this calendar, and who may write into it.
		// A share is administered by the calendar's own side — its owner, or
		// the administrators of the organization it belongs to — so these
		// routes are the ones that ask, and the admin page is not involved.
		r.Get("/{id}/shares", s.handleListCalendarShares)
		r.Post("/{id}/shares", s.handleShareCalendar)
		r.Delete("/{id}/shares/{shareID}", s.handleUnshareCalendar)

		// ICS download for an event
		r.Get("/{id}/events/{eventID}.ics", s.handleEventICS)
		// Generate an iTIP REQUEST (invitation) for an event
		r.Get("/{id}/events/{eventID}/invite.ics", s.handleEventInvite)
	})
}

// calendarOwner is the principal on whose behalf the request acts.
// It is the authenticated address, always.
func calendarOwner(r *http.Request) string {
	return Principal(r.Context()).Address
}

// calendarFromPath loads the calendar the path names, writing the failure
// itself. The bool says whether the handler may carry on.
func (s *Server) calendarFromPath(w http.ResponseWriter, r *http.Request) (store.Calendar, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return store.Calendar{}, false
	}
	cal, err := store.NewCalendars().Get(r.Context(), s.db, id)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return store.Calendar{}, false
	}
	return cal, true
}

// administerCalendar refuses the request unless the caller may change the
// calendar itself — rename it, delete it, decide who it is shared with.
//
// That is a narrower right than writing events into it, and the difference is
// the whole point of the distinction: a team's rota can be booked by anyone who
// needs the room, and a group shared into a colleague's diary can add to it,
// but neither may throw the calendar away. The calendar's own side decides —
// the owner for a personal one, the organization's administrators (or a domain
// admin) for a team one.
func (s *Server) administerCalendar(w http.ResponseWriter, r *http.Request, cal store.Calendar) bool {
	if cal.Owner != nil {
		if strings.EqualFold(*cal.Owner, Principal(r.Context()).Address) {
			return true
		}
		s.writeError(w, http.StatusForbidden, "only the owner of a calendar may change it")
		return false
	}
	if err := s.requireWorkgroupAdmin(r, *cal.WorkgroupID); err != nil {
		s.writeError(w, http.StatusForbidden, "only an administrator of that organization may change its calendars")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Calendar CRUD
// ---------------------------------------------------------------------------

type listCalendarsResponse struct {
	Calendars []calendarView `json:"calendars"`
}

// calendarView is a calendar as the list hands it back: the row, plus what the
// caller may do with it.
//
// The level rides along because the client has to know before it draws the page
// whether the controls it would draw should work. The alternative is offering
// somebody an edit control that fails on Save, which is the least useful moment
// to mention a permission.
type calendarView struct {
	store.Calendar
	// Level is "read" or "write": the strongest thing the caller may do with
	// this calendar's events. Empty means neither, which the list leaves out
	// rather than sending.
	Level string `json:"level,omitempty"`
}

func (s *Server) handleListCalendars(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := Principal(ctx)

	// Three sources feed the list, in the order they matter to a reader: their
	// own calendars, the organization's, and the ones somebody else shared
	// with them. An id may come from more than one — a team calendar shared
	// with a group the caller is in is already in the second set — so each
	// calendar is added once.
	out := make([]calendarView, 0, 4)
	seen := make(map[int64]bool, 4)
	add := func(cals []store.Calendar) {
		for _, cal := range cals {
			if seen[cal.ID] {
				continue
			}
			level, err := s.calendar.Level(ctx, s.db, who, cal)
			if err != nil || level == "" {
				// A calendar that fails its own access check is not shown as
				// readable-with-no-level: showing it would put a door in the
				// list that opens onto a refusal.
				s.log.Debug("leaving calendar out of the list",
					"calendar_id", cal.ID, "error", err)
				continue
			}
			seen[cal.ID] = true
			out = append(out, calendarView{Calendar: cal, Level: level})
		}
	}

	own, err := store.NewCalendars().ByOwner(ctx, s.db, who.Address)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	// A brand-new account has no calendars until something creates one, and the
	// web client lists them before it can create an event. EnsureDefault is
	// idempotent (it returns the existing default when there is one), so calling
	// it here guarantees the list is never empty for a fresh user without adding
	// a calendar on every request.
	if len(own) == 0 {
		cal, err := store.NewCalendars().EnsureDefault(ctx, s.db, who.Address, "UTC")
		if err != nil {
			s.writeError(w, mapErrorCode(err), err.Error())
			return
		}
		own = append(own, cal)
	}
	add(own)

	// The organization's calendars come along too. A team calendar nobody can
	// see cannot be booked, so it is listed for everyone in the organization;
	// whether a booking is accepted is decided when the event is written, not
	// here.
	if wg, ok := s.organizationFor(ctx, who.Address); ok {
		if group, err := store.NewCalendars().ByWorkgroup(ctx, s.db, wg.ID); err == nil {
			add(group)
		}
	}

	// And the ones shared with them by name or through a group, which is the
	// only kind of calendar here that belongs to somebody outside the
	// organization and is still meant to be opened from this list.
	shared, err := store.NewCalendarShares().SharedCalendars(ctx, s.db, who.Address)
	if err != nil {
		s.log.Warn("could not list calendars shared with the caller", "error", err)
	} else {
		add(shared)
	}

	s.writeJSON(w, http.StatusOK, listCalendarsResponse{Calendars: out})
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

// requireWorkgroupAdmin refuses a request about an organization's calendars
// unless the caller administers that organization.
//
// Two people do: an admin member of the group, and a domain admin, who
// administers everyone in the group's domains and therefore the group's
// resources. A plain member may book a group calendar - that is what a team
// calendar is for - but may not create or rename one, which is why this is
// asked separately from the booking rule.
//
// The check lives here rather than in the store because the store has no
// notion of who is asking.
func (s *Server) requireWorkgroupAdmin(r *http.Request, workgroupID int64) error {
	who := Principal(r.Context())
	// A domain admin administers the organization's people, and what belongs to
	// them follows.
	if err := who.Can(policy.ActionAdminister); err == nil {
		return nil
	}
	isAdmin, err := s.workgroups.IsAdmin(r.Context(), s.db, workgroupID, who.Address)
	if err != nil {
		return err
	}
	if isAdmin {
		return nil
	}
	return errors.New("only an administrator of that organization may change its calendars")
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
		// An organization's calendar is created on its behalf, not on the
		// caller's: the group owns it, and it must not appear in the personal
		// list of whoever happened to press the button. Membership of the group
		// is what decides, and it is decided here rather than in the store,
		// which has no notion of who is asking.
		if err := s.requireWorkgroupAdmin(r, *req.WorkgroupID); err != nil {
			s.writeError(w, http.StatusForbidden, "only an administrator of that organization may create its calendars")
			return
		}
		cal, err = store.NewCalendars().Create(ctx, s.db, store.CreateCalendarRequest{
			WorkgroupID:  req.WorkgroupID,
			Name:         req.Name,
			Color:        req.Color,
			Timezone:     req.Timezone,
			Description:  req.Description,
			ShowFreeBusy: req.ShowFreeBusy,
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
	who := Principal(ctx)
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
	// The check is made here rather than left to the events underneath: a
	// calendar is a door, and answering it by id with no question asked would
	// turn every share rule into a suggestion. The level comes back with the
	// calendar for the same reason it does in the list — the client is about to
	// draw controls based on it.
	level, err := s.calendar.Level(ctx, s.db, who, cal)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	if level == "" {
		s.writeError(w, http.StatusForbidden, "you do not have access to that calendar")
		return
	}
	s.writeJSON(w, http.StatusOK, calendarView{Calendar: cal, Level: level})
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
	cal, ok := s.calendarFromPath(w, r)
	if !ok {
		return
	}
	if !s.administerCalendar(w, r, cal) {
		return
	}

	var req updateCalendarRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	err := store.NewCalendars().Update(ctx, s.db, cal.ID, store.UpdateCalendarRequest{
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
	updated, err := store.NewCalendars().Get(ctx, s.db, cal.ID)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteCalendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cal, ok := s.calendarFromPath(w, r)
	if !ok {
		return
	}
	// Deleting a calendar deletes every event in it, so it is the same
	// administration right as renaming it, asked the same way.
	if !s.administerCalendar(w, r, cal) {
		return
	}

	if err := store.NewCalendars().Delete(ctx, s.db, cal.ID); err != nil {
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

	// The calendar is checked before the event is read: an event is only worth
	// loading once it is known the caller may look at where it lives.
	if _, err := s.calendar.Readable(ctx, s.db, Principal(ctx), id); err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
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

	// The service loads the event's own calendar and checks write access to
	// it. Deleting by id with no question asked was how any signed-in person
	// could remove anybody's event: the path's calendar id was parsed and then
	// dropped.
	if err := s.calendar.Delete(ctx, s.db, Principal(ctx), eventID); err != nil {
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
// Calendar sharing
// ---------------------------------------------------------------------------

// shareView is one share as the client draws it: who it names and what they
// may do. The group's name comes along because a list of ids is not something
// a person can read — "Sales" tells them what they shared, and an id does not.
type shareView struct {
	ID             int64   `json:"id"`
	SubjectKind    string  `json:"subject_kind"`
	SubjectAddress *string `json:"subject_address,omitempty"`
	SubjectGroupID *int64  `json:"subject_group_id,omitempty"`
	SubjectName    string  `json:"subject_name,omitempty"`
	Level          string  `json:"level"`
}

func viewCalendarShare(share store.CalendarShare, groupName string) shareView {
	return shareView{
		ID:             share.ID,
		SubjectKind:    share.SubjectKind,
		SubjectAddress: share.SubjectAddress,
		SubjectGroupID: share.SubjectGroupID,
		SubjectName:    groupName,
		Level:          share.Level,
	}
}

// handleListCalendarShares answers who else may see this calendar and at what
// level.
//
// The list is shown only to the calendar's own side, because it is the answer
// to "who can read my diary" — a question a colleague with read access has no
// business asking, and answering it would tell them exactly how far their own
// access reaches sideways.
func (s *Server) handleListCalendarShares(w http.ResponseWriter, r *http.Request) {
	cal, ok := s.calendarFromPath(w, r)
	if !ok {
		return
	}
	if !s.administerCalendar(w, r, cal) {
		return
	}

	shares, err := store.NewCalendarShares().List(r.Context(), s.db, cal.ID)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	names, err := s.groupNames(r, shares)
	if err != nil {
		s.log.Warn("could not name the groups on a calendar share", "error", err)
	}
	out := make([]shareView, 0, len(shares))
	for _, share := range shares {
		out = append(out, viewCalendarShare(share, names[share.GroupID()]))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"shares": out})
}

// handleShareCalendar grants a person or a group access to a calendar.
func (s *Server) handleShareCalendar(w http.ResponseWriter, r *http.Request) {
	cal, ok := s.calendarFromPath(w, r)
	if !ok {
		return
	}
	if !s.administerCalendar(w, r, cal) {
		return
	}

	var in struct {
		SubjectKind    string `json:"subject_kind"`
		SubjectAddress string `json:"subject_address"`
		SubjectGroupID int64  `json:"subject_group_id"`
		Level          string `json:"level"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Level == "" {
		in.Level = store.CalendarShareRead
	}
	if in.Level != store.CalendarShareRead && in.Level != store.CalendarShareWrite {
		s.writeError(w, http.StatusBadRequest, "level must be read or write")
		return
	}

	share := store.CalendarShare{
		CalendarID:  cal.ID,
		SubjectKind: in.SubjectKind,
		Level:       in.Level,
	}
	switch in.SubjectKind {
	case store.ShareSubjectUser:
		address := strings.TrimSpace(in.SubjectAddress)
		if address == "" {
			s.writeError(w, http.StatusBadRequest, "subject_address is required")
			return
		}
		if cal.Owner != nil && strings.EqualFold(*cal.Owner, address) {
			s.writeError(w, http.StatusBadRequest, "the owner already has every right on this calendar")
			return
		}
		share.SubjectAddress = &address
	case store.ShareSubjectGroup:
		if in.SubjectGroupID == 0 {
			s.writeError(w, http.StatusBadRequest, "subject_group_id is required")
			return
		}
		share.SubjectGroupID = &in.SubjectGroupID
	default:
		s.writeError(w, http.StatusBadRequest, "subject_kind must be user or group")
		return
	}

	created, err := store.NewCalendarShares().Put(r.Context(), s.db, share)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	names, err := s.groupNames(r, []store.CalendarShare{created})
	if err != nil {
		s.log.Warn("could not name the group on a calendar share", "error", err)
	}
	s.writeJSON(w, http.StatusCreated, viewCalendarShare(created, names[created.GroupID()]))
}

// handleUnshareCalendar takes one subject's access away. The share's own id is
// the thing to delete by: an address or a group id would say who, but not which
// of possibly several subjects on other calendars, and a delete that removes
// more than the caller was shown is the kind of mistake nobody notices until
// the following week.
func (s *Server) handleUnshareCalendar(w http.ResponseWriter, r *http.Request) {
	cal, ok := s.calendarFromPath(w, r)
	if !ok {
		return
	}
	if !s.administerCalendar(w, r, cal) {
		return
	}
	shareID, err := strconv.ParseInt(chi.URLParam(r, "shareID"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid share id")
		return
	}
	if err := store.NewCalendarShares().Delete(r.Context(), s.db, shareID); err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// groupNames resolves the group ids a set of shares name, for display. A share
// that names a person contributes nothing to the map, which is why the lookup
// is by id rather than by position.
func (s *Server) groupNames(r *http.Request, shares []store.CalendarShare) (map[int64]string, error) {
	ids := make([]int64, 0, len(shares))
	seen := make(map[int64]bool, len(shares))
	for _, share := range shares {
		id := share.GroupID()
		if id != 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return map[int64]string{}, nil
	}
	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		group, err := store.NewGroups().Get(r.Context(), s.db, id)
		if err != nil {
			// A share naming a group that has since gone would have been
			// deleted by the cascade; anything else is worth reporting to the
			// caller as an empty name rather than as a failed page.
			s.log.Debug("could not name a shared group", "group_id", id, "error", err)
			continue
		}
		out[id] = group.Name
	}
	return out, nil
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

// ---------------------------------------------------------------------------
// iTIP invitation generation
// ---------------------------------------------------------------------------

// handleEventInvite generates an iTIP REQUEST (invitation) for an event.
// The response is an iCalendar document with METHOD:REQUEST that can be
// attached to an email to invite attendees.
func (s *Server) handleEventInvite(w http.ResponseWriter, r *http.Request) {
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

	// Parse the stored iCalendar blob to an ical.Document
	doc, err := ical.Parse(row.ICalBlob)
	if err != nil || len(doc.Events) == 0 {
		s.writeError(w, http.StatusInternalServerError, "failed to parse event")
		return
	}
	ev := doc.Events[0]

	// The organizer must be the calendar owner for the invitation to be valid.
	// The stored event may have a different organizer if it was received via iTIP.
	who := Principal(ctx)
	ev.Organizer = ical.Organizer{Address: who.Address}

	// Generate the iTIP REQUEST
	invite := ical.BuildRequest(ev)

	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"invite-%d.ics\"", eventID))
	w.Write(invite)
}

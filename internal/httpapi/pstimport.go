package httpapi

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/pst"
	"github.com/linchange/linchange/internal/pstimport"
	"github.com/linchange/linchange/internal/store"
)

// maxPSTBatchBody bounds one uploaded batch. The browser converts a 64 MiB
// slice of the archive to JSON with base64 content, which is about 86 MiB; the
// extra headroom is for the item metadata.
const maxPSTBatchBody = 100 << 20

type pstCreateRequest struct {
	Owner         string `json:"owner,omitempty"`
	FileName      string `json:"file_name"`
	SHA256        string `json:"sha256"`
	CalendarID    int64  `json:"calendar_id,omitempty"`
	AddressBookID int64  `json:"address_book_id,omitempty"`
}

type pstBatchRequest struct {
	Items []pstBatchItem `json:"items"`
}

type pstBatchItem struct {
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Target  string `json:"target,omitempty"`
	Content string `json:"content,omitempty"`
	Date    string `json:"date,omitempty"`
	Seen    bool   `json:"seen,omitempty"`

	UID         string `json:"uid,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
	Location    string `json:"location,omitempty"`
	DTStart     string `json:"dtstart,omitempty"`
	DTEnd       string `json:"dtend,omitempty"`
	AllDay      bool   `json:"all_day,omitempty"`

	DisplayName  string   `json:"display_name,omitempty"`
	Email        string   `json:"email,omitempty"`
	Emails       []string `json:"emails,omitempty"`
	Phones       []string `json:"phones,omitempty"`
	Organization string   `json:"organization,omitempty"`
	Title        string   `json:"title,omitempty"`
	Notes        string   `json:"notes,omitempty"`
}

type pstFinishRequest struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func decodePSTBody(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	return strictJSON(r).Decode(v)
}

// pstRequireAdmin and pstRefuseShared mirror the IMAP import guards but use
// this package's sentinels, so the error mapper below answers 403 rather than
// falling through to a 500.
func pstRequireAdmin(actor policy.Principal) error {
	if err := actor.Can(policy.ActionAdminister); err != nil {
		return pstimport.ErrDenied
	}
	return nil
}

func pstRefuseShared(actor policy.Principal) error {
	if actor.Source == policy.SourceShared {
		return pstimport.ErrDenied
	}
	return nil
}

func pstImportErrors(w http.ResponseWriter, s *Server, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, pstimport.ErrInvalid):
		s.writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, pstimport.ErrDenied):
		s.writeError(w, http.StatusForbidden, "not permitted")
	case errors.Is(err, store.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "no such import")
	default:
		s.log.Error("pst import request failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "the request could not be completed")
	}
}

// mountPSTImportRoutes registers the browser batch-import routes under /mail.
func (s *Server) mountPSTImportRoutes(r chi.Router) {
	if s.pstImports == nil {
		return
	}
	r.Route("/pst-import", func(r chi.Router) {
		r.Get("/", s.handleListPSTImports)
		r.Post("/", s.handleCreatePSTImport)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", s.handleGetPSTImport)
			r.Post("/batch", s.handlePSTBatch)
			r.Post("/finish", s.handlePSTFinish)
		})
	})
}

// mountAdminPSTImportRoutes registers the admin routes under /admin.
func (s *Server) mountAdminPSTImportRoutes(r chi.Router) {
	if s.pstImports == nil {
		return
	}
	r.Route("/pst-import", func(r chi.Router) {
		r.Get("/", s.handleAdminListPSTImports)
		r.Post("/", s.handleAdminCreatePSTImport)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", s.handleAdminGetPSTImport)
			r.Post("/batch", s.handleAdminPSTBatch)
			r.Post("/finish", s.handleAdminPSTFinish)
		})
	})
}

func (s *Server) handleListPSTImports(w http.ResponseWriter, r *http.Request) {
	owner := mailOwner(r)
	limit, offset := pstPage(r)
	imports, total, err := s.pstImports.ListSources(r.Context(), &owner, limit, offset)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"imports": imports, "total": total, "limit": limit, "offset": offset,
	})
}

func (s *Server) handleAdminListPSTImports(w http.ResponseWriter, r *http.Request) {
	if err := pstRequireAdmin(Principal(r.Context())); err != nil {
		pstImportErrors(w, s, err)
		return
	}
	var owner *string
	if value := r.URL.Query().Get("owner"); value != "" {
		owner = &value
	}
	limit, offset := pstPage(r)
	imports, total, err := s.pstImports.ListSources(r.Context(), owner, limit, offset)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"imports": imports, "total": total, "limit": limit, "offset": offset,
	})
}

func (s *Server) handleCreatePSTImport(w http.ResponseWriter, r *http.Request) {
	actor := Principal(r.Context())
	if err := pstRefuseShared(actor); err != nil {
		pstImportErrors(w, s, err)
		return
	}
	var in pstCreateRequest
	if err := decodePSTBody(w, r, &in, maxImportBody); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	source, err := s.createPSTSource(r, mailOwner(r), actor.Address, in)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, source)
}

func (s *Server) handleAdminCreatePSTImport(w http.ResponseWriter, r *http.Request) {
	actor := Principal(r.Context())
	if err := pstRequireAdmin(actor); err != nil {
		pstImportErrors(w, s, err)
		return
	}
	var in pstCreateRequest
	if err := decodePSTBody(w, r, &in, maxImportBody); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	owner := strings.ToLower(strings.TrimSpace(in.Owner))
	if owner == "" {
		s.writeError(w, http.StatusBadRequest, "target address is required")
		return
	}
	source, err := s.createPSTSource(r, owner, actor.Address, in)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, source)
}

func (s *Server) createPSTSource(r *http.Request, owner, startedBy string, in pstCreateRequest) (pstimport.Source, error) {
	digest, err := hex.DecodeString(strings.TrimSpace(in.SHA256))
	if err != nil || len(digest) != 32 {
		return pstimport.Source{}, fmt.Errorf("%w: sha256 must be 32 bytes of hex", pstimport.ErrInvalid)
	}
	var sum [32]byte
	copy(sum[:], digest)

	if err := s.validatePSTTargets(r, owner, in.CalendarID, in.AddressBookID); err != nil {
		return pstimport.Source{}, err
	}
	return s.pstImports.CreateSource(r.Context(), owner, startedBy, sum, in.FileName,
		in.CalendarID, in.AddressBookID)
}

// validatePSTTargets refuses a calendar or address book that belongs to
// somebody else. The owner check is what stops a crafted request from writing
// an import into another user's calendar.
func (s *Server) validatePSTTargets(r *http.Request, owner string, calendarID, addressBookID int64) error {
	if calendarID != 0 {
		calendar, err := store.NewCalendars().Get(r.Context(), s.db, calendarID)
		if err != nil {
			return pstimport.ErrInvalid
		}
		if calendar.Owner == nil || !strings.EqualFold(*calendar.Owner, owner) {
			return pstimport.ErrInvalid
		}
	}
	if addressBookID != 0 {
		book, err := store.NewAddressBooks().Get(r.Context(), s.db, addressBookID)
		if err != nil {
			return pstimport.ErrInvalid
		}
		if !strings.EqualFold(book.Owner, owner) || book.Scope == "organization" {
			return pstimport.ErrInvalid
		}
	}
	return nil
}

func (s *Server) handleGetPSTImport(w http.ResponseWriter, r *http.Request) {
	source, err := s.pstSourceFor(r, false)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, source)
}

func (s *Server) handleAdminGetPSTImport(w http.ResponseWriter, r *http.Request) {
	if err := pstRequireAdmin(Principal(r.Context())); err != nil {
		pstImportErrors(w, s, err)
		return
	}
	source, err := s.pstSourceFor(r, true)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, source)
}

func (s *Server) handlePSTBatch(w http.ResponseWriter, r *http.Request) {
	source, err := s.pstSourceFor(r, false)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.writePSTBatch(w, r, source)
}

func (s *Server) handleAdminPSTBatch(w http.ResponseWriter, r *http.Request) {
	if err := pstRequireAdmin(Principal(r.Context())); err != nil {
		pstImportErrors(w, s, err)
		return
	}
	source, err := s.pstSourceFor(r, true)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.writePSTBatch(w, r, source)
}

func (s *Server) writePSTBatch(w http.ResponseWriter, r *http.Request, source pstimport.Source) {
	var in pstBatchRequest
	if err := decodePSTBody(w, r, &in, maxPSTBatchBody); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body (a batch may not exceed 64 MiB)")
		return
	}
	items := make([]pstimport.WorkItem, 0, len(in.Items))
	for _, raw := range in.Items {
		item, err := pstWorkItem(raw)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		items = append(items, item)
	}

	plan := pstimport.Plan{Owner: source.Owner}
	if source.CalendarID != nil {
		plan.CalendarID = *source.CalendarID
	}
	if source.AddressBookID != nil {
		plan.AddressBookID = *source.AddressBookID
	}
	stats, err := s.pstImports.ImportBatch(r.Context(), plan, source.ID, items)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"stats": stats})
}

func pstWorkItem(in pstBatchItem) (pstimport.WorkItem, error) {
	if in.Key == "" || len(in.Key) > 200 {
		return pstimport.WorkItem{}, fmt.Errorf("%w: every item needs a key", pstimport.ErrInvalid)
	}
	switch in.Kind {
	case "message":
		target := strings.Trim(strings.TrimSpace(in.Target), "/")
		if target == "" {
			return pstimport.WorkItem{}, fmt.Errorf("%w: a message needs a target folder", pstimport.ErrInvalid)
		}
		raw, err := decodeContent(in.Content, 64<<20)
		if err != nil {
			return pstimport.WorkItem{}, err
		}
		item := pstimport.WorkItem{
			Key: in.Key, Kind: "message", Target: target, MIME: raw, Seen: in.Seen,
		}
		if in.Date != "" {
			if when, err := time.Parse(time.RFC3339, in.Date); err == nil {
				item.Date = when
			}
		}
		return item, nil

	case "event":
		if strings.TrimSpace(in.UID) == "" {
			return pstimport.WorkItem{}, fmt.Errorf("%w: an event needs a uid", pstimport.ErrInvalid)
		}
		start, err := time.Parse(time.RFC3339, in.DTStart)
		if err != nil {
			return pstimport.WorkItem{}, fmt.Errorf("%w: an event needs a valid start", pstimport.ErrInvalid)
		}
		end := start.Add(time.Hour)
		if in.DTEnd != "" {
			if parsed, err := time.Parse(time.RFC3339, in.DTEnd); err == nil {
				end = parsed
			}
		}
		return pstimport.WorkItem{
			Key: in.Key, Kind: "event",
			Event: ical.Event{
				UID: in.UID, Summary: in.Summary, Description: in.Description,
				Location: in.Location, Start: start, End: end, AllDay: in.AllDay,
				Status: ical.StatusConfirmed, ShowAs: ical.ShowAsBusy,
				DTStamp: time.Now().UTC(),
			},
		}, nil

	case "contact":
		if strings.TrimSpace(in.DisplayName) == "" && strings.TrimSpace(in.Email) == "" {
			return pstimport.WorkItem{}, fmt.Errorf("%w: a contact needs a name or an address", pstimport.ErrInvalid)
		}
		vcard, err := decodeContent(in.Content, 4<<20)
		if err != nil && in.Content != "" {
			return pstimport.WorkItem{}, err
		}
		uid := strings.TrimSpace(in.UID)
		if uid == "" {
			uid = "pst-" + in.Key + "@linchange.import"
		}
		emails := append([]string(nil), in.Emails...)
		if in.Email != "" {
			emails = append([]string{in.Email}, emails...)
		}
		return pstimport.WorkItem{
			Key: in.Key, Kind: "contact",
			Card: pst.Contact{
				UID: uid, DisplayName: in.DisplayName, Organization: in.Organization,
				Title: in.Title, Notes: in.Notes, Emails: emails, Phones: in.Phones,
			},
			VCard: string(vcard),
		}, nil
	}
	return pstimport.WorkItem{}, fmt.Errorf("%w: unknown item kind %q", pstimport.ErrInvalid, in.Kind)
}

func decodeContent(encoded string, limit int64) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	if int64(len(encoded)) > limit*4/3+4 {
		return nil, fmt.Errorf("%w: an item exceeds the size limit", pstimport.ErrInvalid)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: content is not valid base64", pstimport.ErrInvalid)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%w: an item exceeds the size limit", pstimport.ErrInvalid)
	}
	return raw, nil
}

func (s *Server) handlePSTFinish(w http.ResponseWriter, r *http.Request) {
	source, err := s.pstSourceFor(r, false)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.finishPST(w, r, source)
}

func (s *Server) handleAdminPSTFinish(w http.ResponseWriter, r *http.Request) {
	if err := pstRequireAdmin(Principal(r.Context())); err != nil {
		pstImportErrors(w, s, err)
		return
	}
	source, err := s.pstSourceFor(r, true)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.finishPST(w, r, source)
}

func (s *Server) finishPST(w http.ResponseWriter, r *http.Request, source pstimport.Source) {
	var in pstFinishRequest
	if err := decodePSTBody(w, r, &in, maxImportBody); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	status := strings.TrimSpace(in.Status)
	if status != "completed" && status != "failed" {
		s.writeError(w, http.StatusBadRequest, "status must be completed or failed")
		return
	}
	var jobErr error
	if status == "failed" {
		if strings.TrimSpace(in.Error) == "" {
			in.Error = "the browser stopped the import"
		}
		jobErr = errors.New(in.Error)
	}
	if err := s.pstImports.FinishSource(r.Context(), source.ID, status, jobErr); err != nil {
		pstImportErrors(w, s, err)
		return
	}
	updated, err := s.pstImports.GetSource(r.Context(), source.ID)
	if err != nil {
		pstImportErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, updated)
}

// pstSourceFor resolves {id} and enforces ownership. Admin callers skip the
// owner check; the route already required the administration grant.
func (s *Server) pstSourceFor(r *http.Request, admin bool) (pstimport.Source, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		return pstimport.Source{}, pstimport.ErrInvalid
	}
	source, err := s.pstImports.GetSource(r.Context(), id)
	if err != nil {
		return pstimport.Source{}, err
	}
	if !admin && !strings.EqualFold(source.Owner, mailOwner(r)) {
		return pstimport.Source{}, store.ErrNotFound
	}
	return source, nil
}

func pstPage(r *http.Request) (int, int) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

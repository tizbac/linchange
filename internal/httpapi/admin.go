package httpapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/admin"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/delivery"
	"github.com/linchange/linchange/internal/store"
)

// This file is the HTTP edge of the administration API and nothing else. It
// decodes a request, calls one method on admin.Service, and maps the result to
// a status code. Every question of who may do what has already been answered
// by the time a handler here runs, so there is deliberately no authorization
// logic in this file to drift from the service's.

// maxAdminBody bounds an administrative request. Account creation and the
// mailbox settings are the largest, and nothing legitimate is anywhere near
// this.
const maxAdminBody = 64 * 1024

// adminListOptions is the server-side page every administration table uses.
// Search is one free-text field over the row the table would draw; limit and
// offset come from the pager, not from the browser slicing its own copy.
type adminListOptions struct {
	Search string
	Limit  int
	Offset int
}

func adminListOptionsFrom(r *http.Request) (adminListOptions, error) {
	q := r.URL.Query()
	opts := adminListOptions{
		Search: strings.TrimSpace(q.Get("q")),
	}
	if opts.Search == "" {
		opts.Search = strings.TrimSpace(q.Get("search"))
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return opts, errors.New("limit is not a number")
		}
		if n > 100 {
			n = 100
		}
		opts.Limit = n
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return opts, errors.New("offset is not a number")
		}
		opts.Offset = n
	}
	return opts, nil
}

// pageAdmin scans the row values as the table's search index and returns one
// page. It is server-side on purpose: an administrator's browser receives
// twenty rows, not the whole organization, and the total describes the
// filtered set rather than the page.
func pageAdmin[T any](items []T, opts adminListOptions) ([]T, int) {
	filtered := items
	if opts.Search != "" {
		query := strings.ToLower(opts.Search)
		filtered = make([]T, 0, len(items))
		for _, item := range items {
			if strings.Contains(strings.ToLower(fmt.Sprintf("%+v", item)), query) {
				filtered = append(filtered, item)
			}
		}
	}
	total := len(filtered)
	start := opts.Offset
	if start > total {
		start = total
	}
	end := total
	if opts.Limit > 0 && start+opts.Limit < end {
		end = start + opts.Limit
	}
	return filtered[start:end], total
}

// adminErrors maps a service error to a status. store's sentinels are compared
// with errors.Is because a repository may wrap one on its way out.
func adminErrors(w http.ResponseWriter, s *Server, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, admin.ErrDenied):
		s.writeError(w, http.StatusForbidden, "you are not permitted to do that")
	case errors.Is(err, admin.ErrInvalid):
		// The caller can fix this, and the message says what is wrong: an
		// administrator mistyping an address should be told, not left guessing.
		s.writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "no such thing")
	case errors.Is(err, store.ErrConflict):
		// A conflict is a rule refusing the write rather than a fault: the
		// address is taken, or the change would contradict something.
		s.writeError(w, http.StatusConflict, "that conflicts with something that already exists")
	case errors.Is(err, store.ErrNoMailbox):
		// Not a conflict: nothing exists twice, the principal simply may not
		// hold mail, which is something the administrator can change.
		s.writeError(w, http.StatusBadRequest,
			"that principal cannot hold mail, so it cannot be a member")
	default:
		s.log.Error("administrative request failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "the request could not be completed")
	}
}

// decodeAdmin reads a request body under the administrative limit.
func decodeAdmin(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBody)
	defer r.Body.Close()
	return strictJSON(r).Decode(v)
}

func (s *Server) mountAdmin(r chi.Router) {
	r.Route("/admin", func(r chi.Router) {
		r.Use(s.requireAuth)
		mountAdminRoutes(r, s)
	})
}

func mountAdminRoutes(r chi.Router, s *Server) {
	r.Route("/domains", func(r chi.Router) {
		r.Get("/", s.listDomains)
		r.Post("/", s.createDomain)
		r.Route("/{name}", func(r chi.Router) {
			r.Get("/", s.getDomain)
			r.Patch("/", s.updateDomain)
			r.Put("/status", s.setDomainStatus)
		})
	})

	r.Route("/users", func(r chi.Router) {
		r.Get("/", s.listUsers)
		r.Post("/", s.createUser)
		r.Route("/{address}", func(r chi.Router) {
			r.Get("/", s.getUser)
			r.Patch("/", s.updateUser)
			r.Delete("/", s.deleteUser)
			r.Put("/enabled", s.setUserEnabled)
			r.Put("/quota", s.setUserQuota)
			r.Put("/password", s.resetUserPassword)
		})
	})

	r.Route("/aliases", func(r chi.Router) {
		r.Get("/", s.listAliases)
		r.Post("/", s.createAlias)
		r.Put("/{address}/enabled", s.setAliasEnabled)
		r.Put("/{address}/targets", s.updateAliasTargets)
		r.Delete("/{address}", s.deleteAlias)
	})

	r.Route("/shared-mailboxes", func(r chi.Router) {
		r.Get("/", s.listSharedMailboxes)
		r.Post("/", s.declareSharedMailbox)
		r.Route("/{address}", func(r chi.Router) {
			r.Patch("/", s.updateSharedMailbox)
			r.Delete("/", s.undeclareSharedMailbox)
			r.Get("/members", s.listSharedMailboxMembers)
			r.Post("/members", s.grantSharedMailboxAccess)
			r.Delete("/members", s.revokeSharedMailboxAccess)
		})
	})

	r.Route("/impersonation", func(r chi.Router) {
		r.Get("/grants", s.listImpersonationGrants)
		r.Post("/grants", s.grantImpersonation)
		r.Delete("/grants/{actor}/{target}", s.revokeImpersonation)
		r.Get("/who/{target}", s.whoCanImpersonate)
	})

	s.mountAdminImportRoutes(r)

	r.Route("/contact-groups", func(r chi.Router) {
		r.Get("/", s.listContactGroups)
		r.Post("/", s.createContactGroup)
		r.Route("/{id}", func(r chi.Router) {
			r.Patch("/", s.updateContactGroup)
			r.Delete("/", s.deleteContactGroup)
		})
	})

	// --- workgroups (organizations) ---
	r.Route("/workgroups", func(r chi.Router) {
		r.Get("/", s.listWorkgroups)
		r.Post("/", s.createWorkgroup)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", s.getWorkgroup)
			r.Patch("/", s.updateWorkgroup)
			r.Delete("/", s.deleteWorkgroup)
			r.Post("/logo", s.uploadWorkgroupLogo)
			r.Delete("/logo", s.deleteWorkgroupLogo)
			r.Get("/members", s.listWorkgroupMembers)
			r.Post("/members", s.addWorkgroupMember)
			r.Delete("/members/{address}", s.removeWorkgroupMember)
			// Groups are the sets an administrator addresses: invite one to a
			// meeting, share a calendar with it.
			r.Get("/groups", s.listWorkgroupGroups)
			r.Post("/groups", s.createWorkgroupGroup)
			r.Route("/groups/{groupID}", func(r chi.Router) {
				r.Patch("/", s.updateWorkgroupGroup)
				r.Delete("/", s.deleteWorkgroupGroup)
				r.Get("/members", s.listWorkgroupGroupMembers)
				r.Post("/members", s.addWorkgroupGroupMember)
				r.Delete("/members/{address}", s.removeWorkgroupGroupMember)
			})
			// The organization's calendars are administered here rather than
			// through the calendar API: that path is for a person's own
			// calendars, and "create a team calendar" is an organizational act.
			r.Get("/calendars", s.listWorkgroupCalendars)
			r.Post("/calendars", s.createWorkgroupCalendar)
			r.Route("/calendars/{calendarID}", func(r chi.Router) {
				r.Patch("/", s.updateWorkgroupCalendar)
				r.Delete("/", s.deleteWorkgroupCalendar)
				// Shares on an organization calendar are administered by the
				// organization's admins (and the domain admin), so they sit
				// alongside the calendar CRUD here rather than in the personal
				// calendar API, which is for a person's own calendars.
				r.Get("/shares", s.listWorkgroupCalendarShares)
				r.Post("/shares", s.shareWorkgroupCalendar)
				r.Delete("/shares/{shareID}", s.unshareWorkgroupCalendar)
			})
		})
	})

	// --- custom themes ---
	// A theme is uploaded as a ZIP and stored in the blob store. This is the
	// only place it can enter the system, which is what keeps an arbitrary
	// stylesheet out of every user's page.
	r.Route("/themes", func(r chi.Router) {
		r.Get("/", s.listThemes)
		r.Post("/", s.uploadTheme)
		// The built-in themes are not database rows, but they are the best
		// starting point for a custom one. This returns the starter package.
		r.Get("/example.zip", s.exportExampleTheme)
		r.Delete("/{uid}", s.deleteTheme)
	})

	// --- outbound queue ---
	// Where undelivered mail can be looked at: what was submitted, who it was
	// going to, what each attempt said, and a way to try a stuck message
	// again. Without it the only answer to "why did it not arrive" is a log
	// file, read in the dark.
	r.Route("/outbox", func(r chi.Router) {
		r.Get("/", s.listOutbox)
		r.Get("/{id}", s.getOutbox)
		r.Post("/{id}/requeue", s.requeueOutbox)
	})

	r.Get("/audit", s.listAudit)
	r.Get("/audit.csv", s.exportAuditCSV)
}

// --- domains ---

func (s *Server) listDomains(w http.ResponseWriter, r *http.Request) {
	opts, err := adminListOptionsFrom(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	domains, err := s.admin.ListDomains(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	page, total := pageAdmin(domains, opts)
	out := make([]domainView, 0, len(page))
	for _, d := range page {
		out = append(out, viewDomain(d))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"domains": out, "total": total, "limit": opts.Limit, "offset": opts.Offset,
	})
}

func (s *Server) createDomain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name              string  `json:"name"`
		IsLocal           bool    `json:"is_local"`
		Description       string  `json:"description"`
		DefaultQuotaBytes *int64  `json:"default_quota_bytes"`
		DailySendLimit    *int    `json:"daily_send_limit"`
		DKIMSelector      *string `json:"dkim_selector"`
		ContactGroupID    *int64  `json:"contact_group_id"`
		WorkgroupID       *int64  `json:"workgroup_id"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	domain, err := s.admin.CreateDomain(r.Context(), Principal(r.Context()), store.CreateDomainRequest{
		Name:              in.Name,
		IsLocal:           in.IsLocal,
		Description:       in.Description,
		DefaultQuotaBytes: in.DefaultQuotaBytes,
		DailySendLimit:    in.DailySendLimit,
		DKIMSelector:      in.DKIMSelector,
		ContactGroupID:    in.ContactGroupID,
		WorkgroupID:       in.WorkgroupID,
	})
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, viewDomain(domain))
}

func (s *Server) getDomain(w http.ResponseWriter, r *http.Request) {
	domain, err := s.admin.GetDomain(r.Context(), Principal(r.Context()), pathParam(r, "name"))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewDomain(domain))
}

func (s *Server) updateDomain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Description       *string       `json:"description"`
		DefaultQuotaBytes *int64        `json:"default_quota_bytes"`
		DailySendLimit    *int          `json:"daily_send_limit"`
		DKIMSelector      *string       `json:"dkim_selector"`
		ContactGroupID    optionalInt64 `json:"contact_group_id"`
		WorkgroupID       optionalInt64 `json:"workgroup_id"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	// A PATCH names only the fields being changed, so the current values are
	// read first and anything the caller omitted is carried over. Without this
	// an omitted quota would be written as zero and every mailbox in the domain
	// would suddenly be unlimited.
	name := pathParam(r, "name")
	current, err := s.admin.GetDomain(r.Context(), Principal(r.Context()), name)
	if err != nil {
		adminErrors(w, s, err)
		return
	}

	domain, err := s.admin.UpdateDomain(r.Context(), Principal(r.Context()), name,
		store.UpdateDomainRequest{
			IsLocal:           current.IsLocal,
			Description:       stringOr(in.Description, current.Description),
			DefaultQuotaBytes: int64Or(in.DefaultQuotaBytes, current.DefaultQuotaBytes),
			DailySendLimit:    in.DailySendLimit,
			DKIMSelector:      in.DKIMSelector,
			// The organization of a domain is carried over when the request does
			// not mention it, and cleared when it says null. Both are real
			// intentions: editing a domain's quota must not silently detach it
			// from its organization, and taking a domain out of one must work.
			ContactGroupID: in.ContactGroupID.or(current.ContactGroupID),
			WorkgroupID:    in.WorkgroupID.or(current.WorkgroupID),
		})
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewDomain(domain))
}

func (s *Server) setDomainStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status store.DomainStatus `json:"status"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := s.admin.SetDomainStatus(r.Context(), Principal(r.Context()),
		pathParam(r, "name"), in.Status); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- users ---

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	opts, err := adminListOptionsFrom(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	users, err := s.admin.ListUsers(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	views := make([]userView, 0, len(users))
	for _, u := range users {
		views = append(views, viewUser(u))
	}
	out, total := pageAdmin(views, opts)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"users": out, "total": total, "limit": opts.Limit, "offset": opts.Offset,
	})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Address     string        `json:"address"`
		Source      policy.Source `json:"source"`
		DisplayName string        `json:"display_name"`
		// Phone is not stored on the user row: it belongs to the person's card
		// in the organization directory, which is where the address book and
		// CardDAV clients read it from.
		Phone              string `json:"phone"`
		Password           string `json:"password"`
		QuotaBytes         *int64 `json:"quota_bytes"`
		MustChangePassword *bool  `json:"must_change_password"`
		DailySendLimit     *int   `json:"daily_send_limit"`
		Timezone           string `json:"timezone"`
		Language           string `json:"language"`
		IsDomainAdmin      bool   `json:"is_domain_admin"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	user, err := s.admin.CreateUser(r.Context(), Principal(r.Context()), admin.CreateUserRequest{
		Address:            in.Address,
		Source:             in.Source,
		DisplayName:        in.DisplayName,
		Password:           in.Password,
		QuotaBytes:         in.QuotaBytes,
		MustChangePassword: in.MustChangePassword,
		DailySendLimit:     in.DailySendLimit,
		Timezone:           in.Timezone,
		Language:           in.Language,
		IsDomainAdmin:      in.IsDomainAdmin,
	})
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	// The new account becomes a card in the organization's directory. This is
	// best effort and never fails the creation: the user exists, and a
	// directory that could not be written is a card the administrator can add
	// again, not a reason to refuse an account that was created.
	if wg, ok := s.organizationFor(r.Context(), user.Address); ok {
		if err := s.contacts.UpsertOrganizationPerson(r.Context(),
			Principal(r.Context()).Address, wg.ID, user.Address, in.DisplayName, in.Phone); err != nil {
			s.log.Warn("could not add the new user to the organization directory",
				"address", user.Address, "error", err)
		}
	}
	// The password is in the request and is not echoed back: a response body
	// ends up in logs and browser history.
	s.writeJSON(w, http.StatusCreated, viewUser(admin.UserView{User: user}))
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	user, err := s.admin.GetUser(r.Context(), Principal(r.Context()), pathParam(r, "address"))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewUser(user))
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DisplayName    string `json:"display_name"`
		QuotaBytes     *int64 `json:"quota_bytes"`
		DailySendLimit *int   `json:"daily_send_limit"`
		Timezone       string `json:"timezone"`
		Language       string `json:"language"`
		// IsDomainAdmin is a pointer so that an update which does not mention it
		// leaves the grant alone. A plain bool would read as "false" for a
		// request that only meant to rename somebody.
		IsDomainAdmin *bool `json:"is_domain_admin"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	user, err := s.admin.UpdateUser(r.Context(), Principal(r.Context()), pathParam(r, "address"),
		admin.UpdateUserRequest{
			DisplayName:    in.DisplayName,
			QuotaBytes:     in.QuotaBytes,
			DailySendLimit: in.DailySendLimit,
			Timezone:       in.Timezone,
			Language:       in.Language,
			IsDomainAdmin:  in.IsDomainAdmin,
		})
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewUser(admin.UserView{User: user}))
}

func (s *Server) setUserEnabled(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := s.admin.SetUserEnabled(r.Context(), Principal(r.Context()),
		pathParam(r, "address"), in.Enabled); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setUserQuota(w http.ResponseWriter, r *http.Request) {
	var in struct {
		QuotaBytes *int64 `json:"quota_bytes"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := s.admin.SetUserQuota(r.Context(), Principal(r.Context()),
		pathParam(r, "address"), in.QuotaBytes); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := s.admin.ResetUserPassword(r.Context(), Principal(r.Context()),
		pathParam(r, "address"), in.Password); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if err := s.admin.DeleteUser(r.Context(), Principal(r.Context()), pathParam(r, "address")); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- aliases ---

func (s *Server) listAliases(w http.ResponseWriter, r *http.Request) {
	opts, err := adminListOptionsFrom(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	aliases, err := s.admin.ListAliases(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	views := make([]aliasView, 0, len(aliases))
	for _, a := range aliases {
		views = append(views, viewAlias(a))
	}
	out, total := pageAdmin(views, opts)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"aliases": out, "total": total, "limit": opts.Limit, "offset": opts.Offset,
	})
}

func (s *Server) createAlias(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Address     string             `json:"address"`
		Targets     []aliasTargetInput `json:"targets"`
		DisplayName string             `json:"display_name"`
		Description string             `json:"description"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if len(in.Targets) == 0 {
		s.writeError(w, http.StatusBadRequest, "at least one target is required")
		return
	}
	req := store.CreateAliasRequest{
		Address:     in.Address,
		DisplayName: in.DisplayName,
		Description: in.Description,
		UpdatedBy:   addrOrNil(Principal(r.Context()).Address),
		Targets:     make([]store.AliasTargetRequest, len(in.Targets)),
	}
	for i, t := range in.Targets {
		req.Targets[i] = store.AliasTargetRequest{
			TargetKind:    t.TargetKind,
			TargetAddress: t.TargetAddress,
			WorkgroupID:   t.WorkgroupID,
			Position:      t.Position,
		}
	}
	if err := s.admin.CreateAlias(r.Context(), Principal(r.Context()), req); err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]string{"address": in.Address})
}

type aliasTargetInput struct {
	TargetKind    store.AliasTargetKind `json:"target_kind"`
	TargetAddress *string               `json:"target_address"`
	WorkgroupID   *int64                `json:"workgroup_id"`
	Position      int                   `json:"position"`
}

func (s *Server) setAliasEnabled(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := s.admin.SetAliasEnabled(r.Context(), Principal(r.Context()),
		pathParam(r, "address"), in.Enabled); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	if err := s.admin.DeleteAlias(r.Context(), Principal(r.Context()), pathParam(r, "address")); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateAliasTargets(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Targets []aliasTargetInput `json:"targets"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if len(in.Targets) == 0 {
		s.writeError(w, http.StatusBadRequest, "at least one target is required")
		return
	}
	req := make([]store.AliasTargetRequest, len(in.Targets))
	for i, t := range in.Targets {
		req[i] = store.AliasTargetRequest{
			TargetKind:    t.TargetKind,
			TargetAddress: t.TargetAddress,
			WorkgroupID:   t.WorkgroupID,
			Position:      t.Position,
		}
	}
	if err := s.admin.UpdateAliasTargets(r.Context(), Principal(r.Context()),
		pathParam(r, "address"), req); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- shared mailboxes ---

func (s *Server) listSharedMailboxes(w http.ResponseWriter, r *http.Request) {
	opts, err := adminListOptionsFrom(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	boxes, err := s.admin.ListSharedMailboxes(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	views := make([]sharedMailboxView, 0, len(boxes))
	for _, b := range boxes {
		views = append(views, viewSharedMailbox(b))
	}
	out, total := pageAdmin(views, opts)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"shared_mailboxes": out, "total": total, "limit": opts.Limit, "offset": opts.Offset,
	})
}

func (s *Server) declareSharedMailbox(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Address     string  `json:"address"`
		DisplayName string  `json:"display_name"`
		Description string  `json:"description"`
		Colour      *string `json:"colour"`
		OpenAccess  bool    `json:"open_access"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	box, err := s.admin.DeclareSharedMailbox(r.Context(), Principal(r.Context()), store.CreateSharedMailboxRequest{
		Address:     in.Address,
		DisplayName: in.DisplayName,
		Description: in.Description,
		Colour:      in.Colour,
		OpenAccess:  in.OpenAccess,
		UpdatedBy:   addrOrNil(Principal(r.Context()).Address),
	})
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, viewSharedMailbox(admin.SharedMailboxView{SharedMailbox: box}))
}

func (s *Server) updateSharedMailbox(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DisplayName        string  `json:"display_name"`
		Description        string  `json:"description"`
		Colour             *string `json:"colour"`
		OpenAccess         bool    `json:"open_access"`
		AutoReplySubject   *string `json:"auto_reply_subject"`
		AutoReplyBody      *string `json:"auto_reply_body"`
		AutoReplyExpiresAt *string `json:"auto_reply_expires_at"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	expires, err := parseOptionalTime(in.AutoReplyExpiresAt)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "auto_reply_expires_at is not a timestamp")
		return
	}
	shared := pathParam(r, "address")
	box, err := s.admin.UpdateSharedMailbox(r.Context(), Principal(r.Context()), shared,
		store.UpdateSharedMailboxRequest{
			DisplayName:        in.DisplayName,
			Description:        in.Description,
			Colour:             in.Colour,
			OpenAccess:         in.OpenAccess,
			AutoReplySubject:   in.AutoReplySubject,
			AutoReplyBody:      in.AutoReplyBody,
			AutoReplyExpiresAt: expires,
			UpdatedBy:          addrOrNil(Principal(r.Context()).Address),
		})
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewSharedMailbox(admin.SharedMailboxView{SharedMailbox: box}))
}

func (s *Server) undeclareSharedMailbox(w http.ResponseWriter, r *http.Request) {
	shared := pathParam(r, "address")
	if err := s.admin.UndeclareSharedMailbox(r.Context(), Principal(r.Context()),
		shared); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listSharedMailboxMembers(w http.ResponseWriter, r *http.Request) {
	boxes, err := s.admin.ListSharedMailboxes(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	address := pathParam(r, "address")
	for _, box := range boxes {
		if box.Address == address {
			s.writeJSON(w, http.StatusOK, map[string]any{"members": viewSharedMailbox(box).Members})
			return
		}
	}
	s.writeError(w, http.StatusNotFound, "no such shared mailbox")
}

func (s *Server) grantSharedMailboxAccess(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Member string            `json:"member"`
		Level  policy.GrantLevel `json:"level"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if in.Member == "" {
		s.writeError(w, http.StatusBadRequest, "member is required")
		return
	}
	shared := pathParam(r, "address")
	if err := s.admin.GrantSharedMailboxAccess(r.Context(), Principal(r.Context()),
		shared, in.Member, in.Level); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeSharedMailboxAccess(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Member string `json:"member"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if in.Member == "" {
		s.writeError(w, http.StatusBadRequest, "member is required")
		return
	}
	shared := pathParam(r, "address")
	if err := s.admin.RevokeSharedMailboxAccess(r.Context(), Principal(r.Context()),
		shared, in.Member); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- impersonation and audit ---

func (s *Server) listImpersonationGrants(w http.ResponseWriter, r *http.Request) {
	// The administrator whose grants are listed defaults to the caller, so the
	// common case is a bare GET and a supervisor names the parameter.
	actorAddr := r.URL.Query().Get("actor")
	if actorAddr == "" {
		actorAddr = Principal(r.Context()).Address
	}
	opts, err := adminListOptionsFrom(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	grants, err := s.admin.ListImpersonationGrants(r.Context(), Principal(r.Context()), actorAddr)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	views := viewImpersonations(grants)
	out, total := pageAdmin(views, opts)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"grants": out, "total": total, "limit": opts.Limit, "offset": opts.Offset,
	})
}

func (s *Server) grantImpersonation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Actor  string `json:"actor"`
		Target string `json:"target"`
		Reason string `json:"reason"`
		// TTLSeconds is a number rather than a duration string so a client does
		// not have to agree on a duration syntax. Zero means no expiry.
		TTLSeconds int `json:"ttl_seconds"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	var ttl *time.Duration
	if in.TTLSeconds > 0 {
		d := time.Duration(in.TTLSeconds) * time.Second
		ttl = &d
	}
	if err := s.admin.GrantImpersonation(r.Context(), Principal(r.Context()), admin.GrantImpersonationRequest{
		Actor: in.Actor, Target: in.Target, Reason: in.Reason, TTL: ttl,
	}); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeImpersonation(w http.ResponseWriter, r *http.Request) {
	if err := s.admin.RevokeImpersonation(r.Context(), Principal(r.Context()),
		pathParam(r, "actor"), pathParam(r, "target")); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) whoCanImpersonate(w http.ResponseWriter, r *http.Request) {
	who, err := s.admin.WhoCanImpersonate(r.Context(), Principal(r.Context()), pathParam(r, "target"))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"grants": viewImpersonations(who)})
}

// --- contact groups ---

func (s *Server) listContactGroups(w http.ResponseWriter, r *http.Request) {
	if err := requireImportAdmin(Principal(r.Context())); err != nil {
		adminErrors(w, s, err)
		return
	}
	contactGroups, err := s.admin.ListContactGroups(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"contact_groups": contactGroups})
}

func (s *Server) createContactGroup(w http.ResponseWriter, r *http.Request) {
	if err := requireImportAdmin(Principal(r.Context())); err != nil {
		adminErrors(w, s, err)
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if in.Name == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	contactGroup, err := s.admin.CreateContactGroup(r.Context(), Principal(r.Context()), in.Name, in.Description)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, contactGroup)
}

func (s *Server) updateContactGroup(w http.ResponseWriter, r *http.Request) {
	if err := requireImportAdmin(Principal(r.Context())); err != nil {
		adminErrors(w, s, err)
		return
	}
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid contact group id")
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	contactGroup, err := s.admin.UpdateContactGroup(r.Context(), Principal(r.Context()), id, in.Name, in.Description)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, contactGroup)
}

func (s *Server) deleteContactGroup(w http.ResponseWriter, r *http.Request) {
	if err := requireImportAdmin(Principal(r.Context())); err != nil {
		adminErrors(w, s, err)
		return
	}
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid contact group id")
		return
	}
	if err := s.admin.DeleteContactGroup(r.Context(), Principal(r.Context()), id); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// auditPageSize is the default number of entries the audit page asks for.
// maxAuditPageSize bounds what a caller can request; the CSV export is the
// path for "everything", not a page with a very large number.
const (
	auditPageSize    = 50
	maxAuditPageSize = 200
)

// auditFilterFromQuery parses the filters the audit page and its CSV export
// share. Keeping one parser means the export can never quietly use a different
// set of filters from the table it was launched from.
func auditFilterFromQuery(q url.Values) (store.AuditFilter, error) {
	f := store.AuditFilter{
		Actor:  strings.TrimSpace(q.Get("actor")),
		Target: strings.TrimSpace(q.Get("target")),
		Action: strings.TrimSpace(q.Get("action")),
		Search: strings.TrimSpace(q.Get("search")),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return f, errors.New("limit is not a number")
		}
		f.Limit = n
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return f, errors.New("offset is not a number")
		}
		f.Offset = n
	}
	// The window is read as a number of days back, which is the only shape an
	// audit page needs and avoids agreeing on a date format here.
	if raw := q.Get("since_days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return f, errors.New("since_days is not a number")
		}
		since := time.Now().AddDate(0, 0, -n)
		f.Since = &since
	}
	return f, nil
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	f, err := auditFilterFromQuery(r.URL.Query())
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if f.Limit == 0 {
		f.Limit = auditPageSize
	}
	if f.Limit > maxAuditPageSize {
		f.Limit = maxAuditPageSize
	}
	total, err := s.admin.CountAudit(r.Context(), Principal(r.Context()), f)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	entries, err := s.admin.ListAudit(r.Context(), Principal(r.Context()), f)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	out := make([]auditView, 0, len(entries))
	for _, e := range entries {
		out = append(out, viewAudit(e))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"entries": out,
		"total":   total,
		"limit":   f.Limit,
		"offset":  f.Offset,
	})
}

// exportAuditCSV writes every entry that matches the current filter, with no
// page applied. The response streams as the rows are read: an audit log is
// exactly the table that can be too large to hold in memory.
func (s *Server) exportAuditCSV(w http.ResponseWriter, r *http.Request) {
	f, err := auditFilterFromQuery(r.URL.Query())
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The limit and offset belong to the table, not the export; clearing them
	// documents that the whole filtered set is what leaves here.
	f.Limit, f.Offset = 0, 0

	// Authorise before committing a response: an administrator check that
	// runs only when the first row is read would have already sent a 200 and
	// a header line to a caller who is not allowed to read any of it.
	if _, err := s.admin.CountAudit(r.Context(), Principal(r.Context()), f); err != nil {
		adminErrors(w, s, err)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="audit-log-`+time.Now().UTC().Format("20060102-150405")+`.csv"`)
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"id", "created_at", "actor_address", "target_address", "action",
		"target_type", "target_id", "ip", "detail",
	}); err != nil {
		return
	}
	// Commit the header before the callback starts. If the query then fails,
	// the status is already 200 and the truthful response is a truncated file,
	// not a JSON error inside a CSV.
	cw.Flush()
	if cw.Error() != nil {
		return
	}
	err = s.admin.WalkAudit(r.Context(), Principal(r.Context()), f, func(e store.AuditEntry) error {
		ip := ""
		if e.IP != nil {
			ip = e.IP.String()
		}
		return cw.Write([]string{
			strconv.FormatInt(e.ID, 10),
			e.CreatedAt.UTC().Format(time.RFC3339),
			csvSafe(derefString(e.ActorAddress)),
			csvSafe(derefString(e.TargetAddress)),
			csvSafe(e.Action),
			csvSafe(e.TargetType),
			csvSafe(e.TargetID),
			csvSafe(ip),
			csvSafe(string(e.Detail)),
		})
	})
	if err != nil {
		s.log.Error("exporting the audit log", "error", err)
		return
	}
	cw.Flush()
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// csvSafe keeps a spreadsheet from treating an attacker-controlled audit
// field as a formula. The data is not altered for any ordinary value; only a
// leading marker that Excel or Sheets would execute is quoted with an
// apostrophe, which both programs display as text.
func csvSafe(value string) string {
	if value == "" {
		return value
	}
	switch value[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + value
	}
	return value
}

// --- outbound queue ---

// outboxView is one message as the queue page draws it: the row, how far its
// recipients got, and the times that say what became of it.
//
// The timestamps are omitted when they never happened rather than sent as a
// zero value: a page rendering "0001-01-01" for "never sent" answers a
// question nobody asked.
type outboxView struct {
	ID             int64   `json:"id"`
	SubmittedBy    string  `json:"submitted_by"`
	EnvelopeFrom   string  `json:"envelope_from"`
	Size           int64   `json:"size"`
	State          string  `json:"state"`
	Attempts       int     `json:"attempts"`
	NextAttemptAt  string  `json:"next_attempt_at"`
	LastError      string  `json:"last_error,omitempty"`
	RecipientCount int     `json:"recipient_count"`
	DeliveredCount int     `json:"delivered_count"`
	CreatedAt      string  `json:"created_at"`
	SentAt         *string `json:"sent_at,omitempty"`
	FailedAt       *string `json:"failed_at,omitempty"`
	DSNAt          *string `json:"dsn_at,omitempty"`
	// Recipients is filled in by the detail view only. A listing carrying
	// every recipient of every message would be a query per row, and the two
	// counts above are what a list is read for.
	Recipients []outboxRecipientView `json:"recipients,omitempty"`
}

type outboxRecipientView struct {
	Position    int     `json:"position"`
	Address     string  `json:"address"`
	Kind        string  `json:"kind"`
	State       string  `json:"state"`
	Attempts    int     `json:"attempts"`
	LastError   string  `json:"last_error,omitempty"`
	DeliveredAt *string `json:"delivered_at,omitempty"`
}

func outboxTime(valid bool, t time.Time) *string {
	if !valid {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func viewOutbox(item delivery.OutboxQueueItem) outboxView {
	return outboxView{
		ID:             item.ID,
		SubmittedBy:    item.SubmittedBy,
		EnvelopeFrom:   item.EnvelopeFrom,
		Size:           item.Size,
		State:          string(item.State),
		Attempts:       item.Attempts,
		NextAttemptAt:  item.NextAttemptAt.UTC().Format(time.RFC3339),
		LastError:      item.LastError,
		RecipientCount: item.RecipientCount,
		DeliveredCount: item.DeliveredCount,
		CreatedAt:      item.CreatedAt.UTC().Format(time.RFC3339),
		SentAt:         outboxTime(item.SentAt.Valid, item.SentAt.Time),
		FailedAt:       outboxTime(item.FailedAt.Valid, item.FailedAt.Time),
		DSNAt:          outboxTime(item.DSNAt.Valid, item.DSNAt.Time),
	}
}

// viewOutboxMessage is the detail view: the same row, plus the recipients
// themselves — which is the whole point of opening one message, since "who did
// not get it, and what did the server say" cannot be answered by the counts.
func viewOutboxMessage(msg delivery.OutboxMessage) outboxView {
	view := outboxView{
		ID:            msg.ID,
		SubmittedBy:   msg.SubmittedBy,
		EnvelopeFrom:  msg.EnvelopeFrom,
		Size:          msg.Size,
		State:         string(msg.State),
		Attempts:      msg.Attempts,
		NextAttemptAt: msg.NextAttemptAt.UTC().Format(time.RFC3339),
		LastError:     msg.LastError,
		CreatedAt:     msg.CreatedAt.UTC().Format(time.RFC3339),
		SentAt:        outboxTime(msg.SentAt.Valid, msg.SentAt.Time),
		FailedAt:      outboxTime(msg.FailedAt.Valid, msg.FailedAt.Time),
		DSNAt:         outboxTime(msg.DSNAt.Valid, msg.DSNAt.Time),
		Recipients:    make([]outboxRecipientView, 0, len(msg.Recipients)),
	}
	for _, r := range msg.Recipients {
		view.RecipientCount++
		if r.State == delivery.RecipientDelivered {
			view.DeliveredCount++
		}
		view.Recipients = append(view.Recipients, outboxRecipientView{
			Position:    r.Position,
			Address:     r.Address,
			Kind:        string(r.Kind),
			State:       string(r.State),
			Attempts:    r.Attempts,
			LastError:   r.LastError,
			DeliveredAt: outboxTime(r.DeliveredAt.Valid, r.DeliveredAt.Time),
		})
	}
	return view
}

// listOutbox answers what is in the outbound queue, newest first, optionally
// narrowed to one state or one sender.
func (s *Server) listOutbox(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := delivery.QueueFilter{
		Submitter: strings.TrimSpace(q.Get("submitter")),
	}
	if state := strings.TrimSpace(q.Get("state")); state != "" {
		switch state {
		case "queued", "sending", "sent", "deferred", "failed", "bounced":
			filter.State = delivery.OutboxState(state)
		default:
			// Refused rather than quietly matching nothing. An unknown filter
			// on the page meant to explain missing mail would otherwise
			// explain an empty queue instead.
			s.writeError(w, http.StatusBadRequest, "unknown state")
			return
		}
	}
	opts, err := adminListOptionsFrom(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter.Search = opts.Search
	filter.Limit = opts.Limit
	filter.Offset = opts.Offset

	items, err := s.admin.OutboxQueue(r.Context(), Principal(r.Context()), filter)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	total, err := s.admin.OutboxQueueCount(r.Context(), Principal(r.Context()), filter)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	out := make([]outboxView, 0, len(items))
	for _, item := range items {
		out = append(out, viewOutbox(item))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"messages": out, "total": total, "limit": opts.Limit, "offset": opts.Offset,
	})
}

// getOutbox returns one message with its recipients: what was submitted, each
// address's own state and error, and when the message finally settled.
func (s *Server) getOutbox(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid message id")
		return
	}
	msg, err := s.admin.OutboxMessage(r.Context(), Principal(r.Context()), id)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewOutboxMessage(msg))
}

// requeueOutbox tries a stuck message again now.
func (s *Server) requeueOutbox(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid message id")
		return
	}
	if err := s.admin.RequeueOutbox(r.Context(), Principal(r.Context()), id); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- small helpers ---

func stringOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

func int64Or(p *int64, fallback int64) int64 {
	if p == nil {
		return fallback
	}
	return *p
}

// optionalInt64 tells a PATCH field that was left out apart from one set to
// null.
//
// A *int64 cannot: both arrive as nil, so "leave the column as it is" and
// "clear the column" would be indistinguishable and one of them would quietly
// happen instead of the other. For a domain's organization both are real
// requests — a domain that belongs to one is not a domain whose organization was
// never sent — so the distinction is worth a type.
type optionalInt64 struct {
	set bool
	v   *int64
}

func (o *optionalInt64) UnmarshalJSON(data []byte) error {
	o.set = true
	if string(data) == "null" {
		o.v = nil
		return nil
	}
	var v int64
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	o.v = &v
	return nil
}

// or keeps the current value when the client did not mention the field, and
// takes what it said otherwise, null included.
func (o optionalInt64) or(current *int64) *int64 {
	if !o.set {
		return current
	}
	return o.v
}

func addrOrNil(address string) *string {
	if address == "" {
		return nil
	}
	return &address
}

func parseOptionalTime(raw *string) (*time.Time, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// The wire types below are the API's own. They are written out rather than
// reusing the store's row structs, because a row struct is an implementation
// detail of the schema: serialising one directly would publish Go field names,
// and renaming a column would silently rename a field in the public API. These
// are the contract, and they change when the API should.
type domainView struct {
	Name              string  `json:"name"`
	Status            string  `json:"status"`
	IsLocal           bool    `json:"is_local"`
	Description       string  `json:"description"`
	DefaultQuotaBytes int64   `json:"default_quota_bytes"`
	DailySendLimit    *int    `json:"daily_send_limit,omitempty"`
	DKIMSelector      *string `json:"dkim_selector,omitempty"`
	ContactGroupID    *int64  `json:"contact_group_id,omitempty"`
	WorkgroupID       *int64  `json:"workgroup_id,omitempty"`
}

func viewDomain(d store.Domain) domainView {
	return domainView{
		Name:              d.Name,
		Status:            string(d.Status),
		IsLocal:           d.IsLocal,
		Description:       d.Description,
		DefaultQuotaBytes: d.DefaultQuotaBytes,
		DailySendLimit:    d.DailySendLimit,
		DKIMSelector:      d.DKIMSelector,
		ContactGroupID:    d.ContactGroupID,
		WorkgroupID:       d.WorkgroupID,
	}
}

type userView struct {
	Address            string `json:"address"`
	AddressLocal       string `json:"address_local"`
	Source             string `json:"source"`
	DisplayName        string `json:"display_name"`
	Enabled            bool   `json:"enabled"`
	MailboxEnabled     bool   `json:"mailbox_enabled"`
	IsDomainAdmin      bool   `json:"is_domain_admin"`
	QuotaBytes         *int64 `json:"quota_bytes"`
	DailySendLimit     *int   `json:"daily_send_limit"`
	Timezone           string `json:"timezone"`
	Language           string `json:"language"`
	MustChangePassword bool   `json:"must_change_password"`
	CreatedAt          string `json:"created_at"`
	QuotaUsedBytes     int64  `json:"quota_used_bytes"`
	QuotaMessageCount  int64  `json:"quota_message_count"`
}

func viewUser(u admin.UserView) userView {
	return userView{
		Address:            u.Address,
		AddressLocal:       u.AddressLocal,
		Source:             string(u.Source),
		DisplayName:        u.DisplayName,
		Enabled:            u.Enabled,
		MailboxEnabled:     u.MailboxEnabled,
		IsDomainAdmin:      u.IsDomainAdmin,
		QuotaBytes:         u.QuotaBytes,
		DailySendLimit:     u.DailySendLimit,
		Timezone:           u.Timezone,
		Language:           u.Language,
		MustChangePassword: u.MustChangePassword,
		CreatedAt:          u.CreatedAt.UTC().Format(time.RFC3339),
		QuotaUsedBytes:     u.QuotaUsedBytes,
		QuotaMessageCount:  u.QuotaMessageCount,
	}
}

type aliasTargetView struct {
	TargetKind    string  `json:"target_kind"`
	TargetAddress *string `json:"target_address"`
	WorkgroupID   *int64  `json:"workgroup_id"`
	Position      int     `json:"position"`
}

type aliasView struct {
	Address     string            `json:"address"`
	Targets     []aliasTargetView `json:"targets"`
	DisplayName string            `json:"display_name"`
	Description string            `json:"description"`
	Enabled     bool              `json:"enabled"`
}

func viewAlias(a store.Alias) aliasView {
	targets := make([]aliasTargetView, len(a.Targets))
	for i, t := range a.Targets {
		targets[i] = aliasTargetView{
			TargetKind:    string(t.TargetKind),
			TargetAddress: t.TargetAddress,
			WorkgroupID:   t.WorkgroupID,
			Position:      t.Position,
		}
	}
	return aliasView{
		Address:     a.Address,
		Targets:     targets,
		DisplayName: a.DisplayName,
		Description: a.Description,
		Enabled:     a.Enabled,
	}
}

type sharedMailboxView struct {
	Address            string      `json:"address"`
	DisplayName        string      `json:"display_name"`
	Description        string      `json:"description"`
	Colour             *string     `json:"colour"`
	OpenAccess         bool        `json:"open_access"`
	AutoReplySubject   *string     `json:"auto_reply_subject"`
	AutoReplyBody      *string     `json:"auto_reply_body"`
	AutoReplyExpiresAt *string     `json:"auto_reply_expires_at"`
	Members            []grantView `json:"members"`
}

func viewGrant(g store.SharedMailboxGrant) grantView {
	return grantView{
		SharedAddress: g.SharedAddress,
		MemberAddress: g.MemberAddress,
		Level:         string(g.Level),
	}
}

type grantView struct {
	SharedAddress string `json:"shared_address"`
	MemberAddress string `json:"member_address"`
	Level         string `json:"level"`
}

func viewSharedMailbox(s admin.SharedMailboxView) sharedMailboxView {
	out := sharedMailboxView{
		Address:          s.Address,
		DisplayName:      s.DisplayName,
		Description:      s.Description,
		Colour:           s.Colour,
		OpenAccess:       s.OpenAccess,
		AutoReplySubject: s.AutoReplySubject,
		AutoReplyBody:    s.AutoReplyBody,
		Members:          make([]grantView, 0, len(s.Members)),
	}
	if s.AutoReplyExpiresAt != nil {
		formatted := s.AutoReplyExpiresAt.UTC().Format(time.RFC3339)
		out.AutoReplyExpiresAt = &formatted
	}
	for _, m := range s.Members {
		out.Members = append(out.Members, viewGrant(m))
	}
	return out
}

type impersonationView struct {
	ActorAddress  string  `json:"actor_address"`
	TargetAddress string  `json:"target_address"`
	Reason        string  `json:"reason"`
	Active        bool    `json:"active"`
	ExpiresAt     *string `json:"expires_at"`
	CreatedAt     string  `json:"created_at"`
}

func viewImpersonation(g admin.ImpersonationView) impersonationView {
	out := impersonationView{
		ActorAddress:  g.ActorAddress,
		TargetAddress: g.TargetAddress,
		Reason:        g.Reason,
		Active:        g.Active,
		CreatedAt:     g.CreatedAt.UTC().Format(time.RFC3339),
	}
	if g.ExpiresAt != nil {
		formatted := g.ExpiresAt.UTC().Format(time.RFC3339)
		out.ExpiresAt = &formatted
	}
	return out
}

type auditView struct {
	ID            int64           `json:"id"`
	ActorAddress  *string         `json:"actor_address"`
	TargetAddress *string         `json:"target_address"`
	Action        string          `json:"action"`
	TargetType    string          `json:"target_type"`
	TargetID      string          `json:"target_id"`
	IP            *string         `json:"ip"`
	Detail        json.RawMessage `json:"detail"`
	CreatedAt     string          `json:"created_at"`
}

func viewAudit(e store.AuditEntry) auditView {
	out := auditView{
		ID:            e.ID,
		ActorAddress:  e.ActorAddress,
		TargetAddress: e.TargetAddress,
		Action:        e.Action,
		TargetType:    e.TargetType,
		TargetID:      e.TargetID,
		Detail:        e.Detail,
		CreatedAt:     e.CreatedAt.UTC().Format(time.RFC3339),
	}
	if e.IP != nil {
		ip := e.IP.String()
		out.IP = &ip
	}
	return out
}

// --- workgroups (organizations) ---

func (s *Server) listWorkgroups(w http.ResponseWriter, r *http.Request) {
	opts, err := adminListOptionsFrom(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	workgroups, err := s.admin.ListWorkgroups(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	page, total := pageAdmin(workgroups, opts)
	out := make([]workgroupView, 0, len(page))
	for _, wg := range page {
		out = append(out, s.workgroupViewOf(r.Context(), Principal(r.Context()), wg))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"workgroups": out, "total": total, "limit": opts.Limit, "offset": opts.Offset,
	})
}

func (s *Server) createWorkgroup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Domain      string `json:"domain"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		OpenBooking *bool  `json:"open_booking"`
		Members     []struct {
			Address string `json:"address"`
			Role    string `json:"role"`
		} `json:"members"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if strings.TrimSpace(in.Domain) == "" {
		s.writeError(w, http.StatusBadRequest, "domain is required")
		return
	}
	members := make([]store.WorkgroupMember, len(in.Members))
	for i, m := range in.Members {
		members[i] = store.WorkgroupMember{
			Address: m.Address,
			Role:    m.Role,
		}
	}
	wg, err := s.admin.CreateWorkgroup(r.Context(), Principal(r.Context()), admin.CreateWorkgroupRequest{
		Name:        in.Name,
		Domain:      in.Domain,
		DisplayName: in.DisplayName,
		Description: in.Description,
		OpenBooking: in.OpenBooking,
		Members:     members,
	})
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, s.workgroupViewOf(r.Context(), Principal(r.Context()), wg))
}

func (s *Server) getWorkgroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	wg, err := s.admin.GetWorkgroup(r.Context(), Principal(r.Context()), id)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.workgroupViewOf(r.Context(), Principal(r.Context()), wg))
}

func (s *Server) updateWorkgroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	var in struct {
		Name        string  `json:"name"`
		DisplayName string  `json:"display_name"`
		Description string  `json:"description"`
		OpenBooking *bool   `json:"open_booking"`
		Theme       *string `json:"theme"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if in.Theme != nil && !s.validTheme(r.Context(), *in.Theme) {
		s.writeError(w, http.StatusBadRequest, "unknown theme")
		return
	}
	wg, err := s.admin.UpdateWorkgroup(r.Context(), Principal(r.Context()), id, in.Name, in.DisplayName, in.Description, in.OpenBooking)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	if in.Theme != nil {
		if err := s.workgroups.SetTheme(r.Context(), s.db, id, *in.Theme); err != nil {
			adminErrors(w, s, err)
			return
		}
		wg, err = s.workgroups.Get(r.Context(), s.db, id)
		if err != nil {
			adminErrors(w, s, err)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, s.workgroupViewOf(r.Context(), Principal(r.Context()), wg))
}

func (s *Server) deleteWorkgroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	if err := s.admin.DeleteWorkgroup(r.Context(), Principal(r.Context()), id); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxLogoBytes bounds an organization's logo. A header image has no reason to
// be larger, and keeping it small keeps the top bar fast.
const maxLogoBytes = 1 << 20

// uploadWorkgroupLogo stores a raster logo for an organization. SVG is
// deliberately not accepted: a logo is displayed on every page, and an SVG can
// carry script.
func (s *Server) uploadWorkgroupLogo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	ctx := r.Context()
	if _, err := s.admin.GetWorkgroup(ctx, Principal(ctx), id); err != nil {
		adminErrors(w, s, err)
		return
	}
	if s.blobs == nil {
		s.writeError(w, http.StatusInternalServerError, "the blob store is not configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxLogoBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		s.writeError(w, http.StatusBadRequest, "expected a multipart image upload")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "a file field is required")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxLogoBytes+1))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "could not read the image")
		return
	}
	if len(data) == 0 {
		s.writeError(w, http.StatusBadRequest, "the image is empty")
		return
	}
	if len(data) > maxLogoBytes {
		s.writeError(w, http.StatusRequestEntityTooLarge, "the image is too large")
		return
	}
	mimeType := header.Header.Get("Content-Type")
	if strings.TrimSpace(mimeType) == "" {
		mimeType = http.DetectContentType(data)
	}
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		s.writeError(w, http.StatusBadRequest, "the logo must be a PNG, JPEG, GIF or WebP image")
		return
	}

	digest, _, err := s.blobs.Put(data)
	if err != nil {
		s.log.Error("storing organization logo", "error", err, "workgroup", id)
		s.writeError(w, http.StatusInternalServerError, "could not store the image")
		return
	}
	wg, err := s.workgroups.Get(ctx, s.db, id)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	uid := wg.LogoUID
	if uid == "" {
		uid, err = newLogoUID()
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "could not generate a logo id")
			return
		}
	}
	if err := s.workgroups.SetLogo(ctx, s.db, id, uid, mimeType, digest[:]); err != nil {
		adminErrors(w, s, err)
		return
	}
	wg, err = s.workgroups.Get(ctx, s.db, id)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.workgroupViewOf(ctx, Principal(ctx), wg))
}

// deleteWorkgroupLogo removes the reference; the top bar falls back to the
// organization's name.
func (s *Server) deleteWorkgroupLogo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	ctx := r.Context()
	if _, err := s.admin.GetWorkgroup(ctx, Principal(ctx), id); err != nil {
		adminErrors(w, s, err)
		return
	}
	if err := s.workgroups.ClearLogo(ctx, s.db, id); err != nil {
		adminErrors(w, s, err)
		return
	}
	wg, err := s.workgroups.Get(ctx, s.db, id)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.workgroupViewOf(ctx, Principal(ctx), wg))
}

func (s *Server) listWorkgroupMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	opts, err := adminListOptionsFrom(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	members, err := s.admin.ListWorkgroupMembers(r.Context(), Principal(r.Context()), id)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	views := make([]workgroupMemberView, 0, len(members))
	for _, m := range members {
		views = append(views, viewWorkgroupMember(m))
	}
	out, total := pageAdmin(views, opts)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"members": out, "total": total, "limit": opts.Limit, "offset": opts.Offset,
	})
}

func (s *Server) addWorkgroupMember(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	var in struct {
		Address string `json:"address"`
		Role    string `json:"role"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if strings.TrimSpace(in.Address) == "" {
		s.writeError(w, http.StatusBadRequest, "address is required")
		return
	}
	role := in.Role
	if role == "" {
		role = "member"
	}
	if err := s.admin.AddWorkgroupMember(r.Context(), Principal(r.Context()), id, in.Address, role); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeWorkgroupMember(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	address := pathParam(r, "address")
	if err := s.admin.RemoveWorkgroupMember(r.Context(), Principal(r.Context()), id, address); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- organization groups ---

// listWorkgroupGroups answers with the organization's groups: the sets an
// administrator addresses, Everyone leading because it is the one that is
// always right for "the whole organization".
func (s *Server) listWorkgroupGroups(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	opts, err := adminListOptionsFrom(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	groups, err := s.admin.ListWorkgroupGroups(r.Context(), Principal(r.Context()), id)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	out, total := pageAdmin(viewWorkgroupGroups(groups), opts)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"groups": out, "total": total, "limit": opts.Limit, "offset": opts.Offset,
	})
}

// createWorkgroupGroup adds a group to an organization.
func (s *Server) createWorkgroupGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	group, err := s.admin.CreateWorkgroupGroup(r.Context(), Principal(r.Context()), id, in.Name, in.Description)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, viewWorkgroupGroup(group))
}

// updateWorkgroupGroup renames a group or changes its description.
func (s *Server) updateWorkgroupGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	groupID, ok := pathID(r, "groupID")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	group, err := s.admin.UpdateWorkgroupGroup(r.Context(), Principal(r.Context()), id, groupID, in.Name, in.Description)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewWorkgroupGroup(group))
}

// deleteWorkgroupGroup removes a group. The organization's own group cannot be
// deleted, and says so rather than failing as if it had never existed.
func (s *Server) deleteWorkgroupGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	groupID, ok := pathID(r, "groupID")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	if err := s.admin.DeleteWorkgroupGroup(r.Context(), Principal(r.Context()), id, groupID); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listWorkgroupGroupMembers answers who is in a group. For the organization's
// own group that is every address in its domains, which is worth showing rather
// than hiding behind a count: it is the group somebody picks to reach everyone.
func (s *Server) listWorkgroupGroupMembers(w http.ResponseWriter, r *http.Request) {
	id, groupID, ok := s.workgroupGroupPath(w, r)
	if !ok {
		return
	}
	members, err := s.admin.WorkgroupGroupMembers(r.Context(), Principal(r.Context()), id, groupID)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

// addWorkgroupGroupMember adds an address to a group.
func (s *Server) addWorkgroupGroupMember(w http.ResponseWriter, r *http.Request) {
	id, groupID, ok := s.workgroupGroupPath(w, r)
	if !ok {
		return
	}
	var in struct {
		Address string `json:"address"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if strings.TrimSpace(in.Address) == "" {
		s.writeError(w, http.StatusBadRequest, "address is required")
		return
	}
	if err := s.admin.AddWorkgroupGroupMember(r.Context(), Principal(r.Context()), id, groupID, in.Address); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeWorkgroupGroupMember takes an address out of a group.
func (s *Server) removeWorkgroupGroupMember(w http.ResponseWriter, r *http.Request) {
	id, groupID, ok := s.workgroupGroupPath(w, r)
	if !ok {
		return
	}
	address := pathParam(r, "address")
	removed, err := s.admin.RemoveWorkgroupGroupMember(r.Context(), Principal(r.Context()), id, groupID, address)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	if !removed {
		s.writeError(w, http.StatusNotFound, "that address is not in the group")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// workgroupGroupPath reads the organization and group ids every group route
// carries, writing the failure itself so the handlers above do not each spend
// five lines saying the same thing badly.
func (s *Server) workgroupGroupPath(w http.ResponseWriter, r *http.Request) (workgroupID, groupID int64, ok bool) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return 0, 0, false
	}
	group, ok := pathID(r, "groupID")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid group id")
		return 0, 0, false
	}
	return id, group, true
}

// --- organization calendars ---

// listWorkgroupCalendars answers with the organization's calendars.
func (s *Server) listWorkgroupCalendars(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	opts, err := adminListOptionsFrom(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cals, err := s.admin.WorkgroupCalendars(r.Context(), Principal(r.Context()), id)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	out, total := pageAdmin(cals, opts)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"calendars": out, "total": total, "limit": opts.Limit, "offset": opts.Offset,
	})
}

// createWorkgroupCalendar creates a calendar the whole organization owns.
func (s *Server) createWorkgroupCalendar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	var in createCalendarRequest
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	cal, err := s.admin.CreateWorkgroupCalendar(r.Context(), Principal(r.Context()), id,
		store.CreateCalendarRequest{
			Name:         in.Name,
			Color:        in.Color,
			Timezone:     in.Timezone,
			Description:  in.Description,
			ShowFreeBusy: in.ShowFreeBusy,
		})
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, cal)
}

// updateWorkgroupCalendar renames or recolours one of the organization's
// calendars.
func (s *Server) updateWorkgroupCalendar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	calendarID, ok := pathID(r, "calendarID")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}
	var in updateCalendarRequest
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := s.admin.UpdateWorkgroupCalendar(r.Context(), Principal(r.Context()), id, calendarID,
		store.UpdateCalendarRequest{
			Name:         in.Name,
			Color:        in.Color,
			Timezone:     in.Timezone,
			Description:  in.Description,
			ShowFreeBusy: in.ShowFreeBusy,
		}); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteWorkgroupCalendar removes one of the organization's calendars and
// every event in it.
func (s *Server) deleteWorkgroupCalendar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	calendarID, ok := pathID(r, "calendarID")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}
	if err := s.admin.DeleteWorkgroupCalendar(r.Context(), Principal(r.Context()), id, calendarID); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listWorkgroupCalendarShares lists the shares on an organization's calendar.
func (s *Server) listWorkgroupCalendarShares(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	calendarID, ok := pathID(r, "calendarID")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}
	shares, err := s.admin.WorkgroupCalendarShares(r.Context(), Principal(r.Context()), id, calendarID)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	// Reuse the shareView from the calendar API.
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

// shareWorkgroupCalendar grants a user or group access to an organization's calendar.
func (s *Server) shareWorkgroupCalendar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	calendarID, ok := pathID(r, "calendarID")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}
	var in struct {
		SubjectKind    string `json:"subject_kind"`
		SubjectAddress string `json:"subject_address"`
		SubjectGroupID int64  `json:"subject_group_id"`
		Level          string `json:"level"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
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
		CalendarID:  calendarID,
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

	created, err := s.admin.ShareWorkgroupCalendar(r.Context(), Principal(r.Context()), id, calendarID, share)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	names, err := s.groupNames(r, []store.CalendarShare{created})
	if err != nil {
		s.log.Warn("could not name the group on a calendar share", "error", err)
	}
	s.writeJSON(w, http.StatusCreated, viewCalendarShare(created, names[created.GroupID()]))
}

// unshareWorkgroupCalendar removes a share from an organization's calendar.
func (s *Server) unshareWorkgroupCalendar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	calendarID, ok := pathID(r, "calendarID")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid calendar id")
		return
	}
	shareID, ok := pathID(r, "shareID")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid share id")
		return
	}
	if err := s.admin.UnshareWorkgroupCalendar(r.Context(), Principal(r.Context()), id, calendarID, shareID); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// workgroupGroupView is one group as the organization page draws it: its name,
// and how many addresses it reaches — the number that tells somebody choosing
// between "the organization" and "the sales team" what they are choosing.
type workgroupGroupView struct {
	ID          int64  `json:"id"`
	WorkgroupID int64  `json:"workgroup_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Everyone says this is the organization's own group, whose membership is
	// every address in its domains rather than a list somebody maintains. It
	// cannot be deleted and takes no members, which the client shows as a
	// disabled control rather than as a button that fails.
	Everyone    bool   `json:"everyone"`
	MemberCount int    `json:"member_count"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func viewWorkgroupGroup(g store.WorkgroupGroup) workgroupGroupView {
	return workgroupGroupView{
		ID:          g.ID,
		WorkgroupID: g.WorkgroupID,
		Name:        g.Name,
		Description: g.Description,
		Everyone:    g.Everyone,
		MemberCount: g.MemberCount,
		CreatedAt:   g.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   g.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func viewWorkgroupGroups(groups []store.WorkgroupGroup) []workgroupGroupView {
	out := make([]workgroupGroupView, 0, len(groups))
	for _, g := range groups {
		out = append(out, viewWorkgroupGroup(g))
	}
	return out
}

// --- view helpers for workgroups ---

type workgroupView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Domain      string `json:"domain"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	OpenBooking bool   `json:"open_booking"`
	// LogoURL is the public image path, empty when the organization has no
	// logo and the top bar should show its name instead.
	LogoURL string `json:"logo_url,omitempty"`
	// Theme is the organization's default theme key, empty when it has none.
	Theme     string `json:"theme,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// Members ride along with the group. Every screen that shows an organization
	// also shows who belongs to it, so making the client fetch them one group at
	// a time is a request per row for no benefit.
	Members []workgroupMemberView `json:"members"`
}

func viewWorkgroup(w store.Workgroup) workgroupView {
	return workgroupView{
		ID:          w.ID,
		Name:        w.Name,
		Domain:      w.Domain,
		DisplayName: w.DisplayName,
		Description: w.Description,
		OpenBooking: w.OpenBooking,
		LogoURL:     w.LogoURL(),
		Theme:       w.Theme,
		CreatedAt:   w.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   w.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

type workgroupMemberView struct {
	WorkgroupID int64  `json:"workgroup_id"`
	Address     string `json:"address"`
	Role        string `json:"role"`
	// Source says how the address came to be in the group: "member" is a row
	// somebody added, "domain" is an address in one of the group's own domains,
	// which is a membership nobody maintains and nobody can remove. The client
	// shows the second kind without a remove control for exactly that reason.
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
}

func viewWorkgroupMember(m store.WorkgroupMember) workgroupMemberView {
	return workgroupMemberView{
		WorkgroupID: m.WorkgroupID,
		Address:     m.Address,
		Role:        m.Role,
		Source:      "member",
		CreatedAt:   m.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// workgroupMemberFromDomain is a member the address makes rather than a row in
// the member table. It has no role and no creation time because there was no
// grant: the person is in the group because of where their mail lives.
func workgroupMemberFromDomain(workgroupID int64, address string) workgroupMemberView {
	return workgroupMemberView{
		WorkgroupID: workgroupID,
		Address:     address,
		Role:        "",
		Source:      "domain",
		CreatedAt:   "",
	}
}

// workgroupViewOf returns a group with its members attached.
//
// The member list is fetched here rather than left to the client because every
// screen that shows an organization also lists who is in it. A member lookup
// that fails does not fail the response: the group exists and is worth showing,
// and an empty list is closer to the truth than a 500 for a page whose subject
// is the group.
//
// The list holds both kinds of member: the rows added by hand and everybody in
// the group's domains. The second kind is what makes the first kind make sense,
// since the question an administrator is usually asking is "who is in this
// organization", and most of the answer is the domains.
func (s *Server) workgroupViewOf(ctx context.Context, actor policy.Principal, wg store.Workgroup) workgroupView {
	view := viewWorkgroup(wg)
	members, err := s.admin.ListWorkgroupMembers(ctx, actor, wg.ID)
	if err != nil {
		s.log.Warn("could not load workgroup members", "workgroup_id", wg.ID, "error", err)
		return view
	}
	view.Members = make([]workgroupMemberView, 0, len(members))
	listed := make(map[string]bool, len(members))
	for _, m := range members {
		listed[strings.ToLower(m.Address)] = true
		view.Members = append(view.Members, viewWorkgroupMember(m))
	}

	people, err := s.admin.WorkgroupPeople(ctx, actor, wg.ID)
	if err != nil {
		s.log.Warn("could not load workgroup addresses", "workgroup_id", wg.ID, "error", err)
		return view
	}
	for _, p := range people {
		if listed[strings.ToLower(p.Address)] {
			// Already listed as an explicit member, and the row carries the
			// role, which is the more specific of the two answers.
			continue
		}
		view.Members = append(view.Members, workgroupMemberFromDomain(wg.ID, p.Address))
	}
	return view
}

func viewImpersonations(gs []admin.ImpersonationView) []impersonationView {
	out := make([]impersonationView, 0, len(gs))
	for _, g := range gs {
		out = append(out, viewImpersonation(g))
	}
	return out
}

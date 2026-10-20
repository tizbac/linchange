package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/admin"
	"github.com/linchange/linchange/internal/auth/policy"
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
			r.Get("/members", s.listWorkgroupMembers)
			r.Post("/members", s.addWorkgroupMember)
			r.Delete("/members/{address}", s.removeWorkgroupMember)
		})
	})

	r.Get("/audit", s.listAudit)
}

// --- domains ---

func (s *Server) listDomains(w http.ResponseWriter, r *http.Request) {
	domains, err := s.admin.ListDomains(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	out := make([]domainView, 0, len(domains))
	for _, d := range domains {
		out = append(out, viewDomain(d))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"domains": out})
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
	domain, err := s.admin.GetDomain(r.Context(), Principal(r.Context()), chi.URLParam(r, "name"))
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
	name := chi.URLParam(r, "name")
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
		chi.URLParam(r, "name"), in.Status); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- users ---

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.admin.ListUsers(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	out := make([]userView, 0, len(users))
	for _, u := range users {
		out = append(out, viewUser(u))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Address            string        `json:"address"`
		Source             policy.Source `json:"source"`
		DisplayName        string        `json:"display_name"`
		Password           string        `json:"password"`
		QuotaBytes         *int64        `json:"quota_bytes"`
		MustChangePassword *bool         `json:"must_change_password"`
		DailySendLimit     *int          `json:"daily_send_limit"`
		Timezone           string        `json:"timezone"`
		Language           string        `json:"language"`
		IsDomainAdmin      bool          `json:"is_domain_admin"`
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
	// The password is in the request and is not echoed back: a response body
	// ends up in logs and browser history.
	s.writeJSON(w, http.StatusCreated, viewUser(admin.UserView{User: user}))
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	user, err := s.admin.GetUser(r.Context(), Principal(r.Context()), chi.URLParam(r, "address"))
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
	user, err := s.admin.UpdateUser(r.Context(), Principal(r.Context()), chi.URLParam(r, "address"),
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
		chi.URLParam(r, "address"), in.Enabled); err != nil {
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
		chi.URLParam(r, "address"), in.QuotaBytes); err != nil {
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
		chi.URLParam(r, "address"), in.Password); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if err := s.admin.DeleteUser(r.Context(), Principal(r.Context()), chi.URLParam(r, "address")); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- aliases ---

func (s *Server) listAliases(w http.ResponseWriter, r *http.Request) {
	aliases, err := s.admin.ListAliases(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	out := make([]aliasView, 0, len(aliases))
	for _, a := range aliases {
		out = append(out, viewAlias(a))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"aliases": out})
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
		chi.URLParam(r, "address"), in.Enabled); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	if err := s.admin.DeleteAlias(r.Context(), Principal(r.Context()), chi.URLParam(r, "address")); err != nil {
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
		chi.URLParam(r, "address"), req); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- shared mailboxes ---

func (s *Server) listSharedMailboxes(w http.ResponseWriter, r *http.Request) {
	boxes, err := s.admin.ListSharedMailboxes(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	out := make([]sharedMailboxView, 0, len(boxes))
	for _, b := range boxes {
		out = append(out, viewSharedMailbox(b))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"shared_mailboxes": out})
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
	shared, err := url.PathUnescape(chi.URLParam(r, "address"))
	if err != nil {
		shared = chi.URLParam(r, "address")
	}
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
	shared, err := url.PathUnescape(chi.URLParam(r, "address"))
	if err != nil {
		shared = chi.URLParam(r, "address")
	}
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
	address, err := url.PathUnescape(chi.URLParam(r, "address"))
	if err != nil {
		address = chi.URLParam(r, "address")
	}
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
	shared, err := url.PathUnescape(chi.URLParam(r, "address"))
	if err != nil {
		shared = chi.URLParam(r, "address")
	}
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
	shared, err := url.PathUnescape(chi.URLParam(r, "address"))
	if err != nil {
		shared = chi.URLParam(r, "address")
	}
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
	grants, err := s.admin.ListImpersonationGrants(r.Context(), Principal(r.Context()), actorAddr)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"grants": viewImpersonations(grants)})
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
		chi.URLParam(r, "actor"), chi.URLParam(r, "target")); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) whoCanImpersonate(w http.ResponseWriter, r *http.Request) {
	who, err := s.admin.WhoCanImpersonate(r.Context(), Principal(r.Context()), chi.URLParam(r, "target"))
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

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.AuditFilter{
		Actor:  q.Get("actor"),
		Target: q.Get("target"),
		Action: q.Get("action"),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			s.writeError(w, http.StatusBadRequest, "limit is not a number")
			return
		}
		f.Limit = n
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			s.writeError(w, http.StatusBadRequest, "offset is not a number")
			return
		}
		f.Offset = n
	}
	// The window is read as a number of days back, which is the only shape an
	// audit page needs and avoids agreeing on a date format here.
	if raw := q.Get("since_days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			s.writeError(w, http.StatusBadRequest, "since_days is not a number")
			return
		}
		since := time.Now().AddDate(0, 0, -n)
		f.Since = &since
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
	s.writeJSON(w, http.StatusOK, map[string]any{"entries": out})
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
	workgroups, err := s.admin.ListWorkgroups(r.Context(), Principal(r.Context()))
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	out := make([]workgroupView, 0, len(workgroups))
	for _, w := range workgroups {
		out = append(out, s.workgroupViewOf(r.Context(), Principal(r.Context()), w))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"workgroups": out})
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
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		OpenBooking *bool  `json:"open_booking"`
	}
	if err := decodeAdmin(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	wg, err := s.admin.UpdateWorkgroup(r.Context(), Principal(r.Context()), id, in.Name, in.DisplayName, in.Description, in.OpenBooking)
	if err != nil {
		adminErrors(w, s, err)
		return
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

func (s *Server) listWorkgroupMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid workgroup id")
		return
	}
	members, err := s.admin.ListWorkgroupMembers(r.Context(), Principal(r.Context()), id)
	if err != nil {
		adminErrors(w, s, err)
		return
	}
	out := make([]workgroupMemberView, 0, len(members))
	for _, m := range members {
		out = append(out, viewWorkgroupMember(m))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"members": out})
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
	address := chi.URLParam(r, "address")
	if err := s.admin.RemoveWorkgroupMember(r.Context(), Principal(r.Context()), id, address); err != nil {
		adminErrors(w, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- view helpers for workgroups ---

type workgroupView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Domain      string `json:"domain"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	OpenBooking bool   `json:"open_booking"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
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
		CreatedAt:   w.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   w.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

type workgroupMemberView struct {
	WorkgroupID int64  `json:"workgroup_id"`
	Address     string `json:"address"`
	Role        string `json:"role"`
	CreatedAt   string `json:"created_at"`
}

func viewWorkgroupMember(m store.WorkgroupMember) workgroupMemberView {
	return workgroupMemberView{
		WorkgroupID: m.WorkgroupID,
		Address:     m.Address,
		Role:        m.Role,
		CreatedAt:   m.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// workgroupViewOf returns a group with its members attached.
//
// The member list is fetched here rather than left to the client because every
// screen that shows an organization also lists who is in it. A member lookup
// that fails does not fail the response: the group exists and is worth showing,
// and an empty list is closer to the truth than a 500 for a page whose subject
// is the group.
func (s *Server) workgroupViewOf(ctx context.Context, actor policy.Principal, wg store.Workgroup) workgroupView {
	view := viewWorkgroup(wg)
	members, err := s.admin.ListWorkgroupMembers(ctx, actor, wg.ID)
	if err != nil {
		s.log.Warn("could not load workgroup members", "workgroup_id", wg.ID, "error", err)
		return view
	}
	view.Members = make([]workgroupMemberView, 0, len(members))
	for _, m := range members {
		view.Members = append(view.Members, viewWorkgroupMember(m))
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

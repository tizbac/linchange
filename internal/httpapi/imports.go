package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/imapimport"
	"github.com/linchange/linchange/internal/store"
)

// maxImportBody bounds an import request. The largest field is a password;
// nothing legitimate is anywhere near this.
const maxImportBody = 8 * 1024

// importRequest is the wire shape for creating an import job. The password is
// accepted once and stored encrypted; it is never returned.
type importRequest struct {
	Owner            string `json:"owner,omitempty"`
	RemoteHost       string `json:"remote_host"`
	RemotePort       int    `json:"remote_port,omitempty"`
	RemoteTLS        string `json:"remote_tls,omitempty"`
	RemoteUsername   string `json:"remote_username"`
	RemotePassword   string `json:"remote_password"`
	AcceptSelfSigned bool   `json:"accept_self_signed,omitempty"`
	SourceRoot       string `json:"source_root,omitempty"`
	TargetRoot       string `json:"target_root,omitempty"`
}

type remoteFoldersRequest struct {
	RemoteHost       string `json:"remote_host"`
	RemotePort       int    `json:"remote_port,omitempty"`
	RemoteTLS        string `json:"remote_tls,omitempty"`
	RemoteUsername   string `json:"remote_username"`
	RemotePassword   string `json:"remote_password"`
	AcceptSelfSigned bool   `json:"accept_self_signed,omitempty"`
}

func decodeImportBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBody)
	defer r.Body.Close()
	return strictJSON(r).Decode(v)
}

func importErrors(w http.ResponseWriter, s *Server, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, imapimport.ErrInvalid):
		s.writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, imapimport.ErrDenied):
		s.writeError(w, http.StatusForbidden, "not permitted")
	case errors.Is(err, store.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "no such account")
	default:
		s.log.Error("import request failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "the request could not be completed")
	}
}

// mountImportRoutes registers the user-facing import routes under /mail.
func (s *Server) mountImportRoutes(r chi.Router) {
	if s.imports == nil {
		return
	}
	r.Route("/imports", func(r chi.Router) {
		r.Get("/", s.handleListImports)
		r.Post("/", s.handleCreateImport)
		r.Post("/remote-folders", s.handleRemoteFolders)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", s.handleGetImport)
			r.Delete("/", s.handleCancelImport)
		})
	})
}

// mountAdminImportRoutes registers the admin import routes under /admin.
func (s *Server) mountAdminImportRoutes(r chi.Router) {
	if s.imports == nil {
		return
	}
	r.Route("/imports", func(r chi.Router) {
		r.Get("/", s.handleAdminListImports)
		r.Post("/", s.handleAdminCreateImport)
		r.Post("/remote-folders", s.handleAdminRemoteFolders)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", s.handleAdminGetImport)
			r.Delete("/", s.handleAdminCancelImport)
		})
	})
}

// requireImportAdmin is the local mirror of admin.Service.requireAdmin: it
// refuses anything that is not a domain admin before a handler touches the
// import service.
func requireImportAdmin(actor policy.Principal) error {
	if err := actor.Can(policy.ActionAdminister); err != nil {
		return imapimport.ErrDenied
	}
	return nil
}

// refuseSharedPrincipal blocks a shared mailbox from launching its own import.
// Shared mailboxes are managed from the admin panel only.
func refuseSharedPrincipal(actor policy.Principal) error {
	if actor.Source == policy.SourceShared {
		return imapimport.ErrDenied
	}
	return nil
}

func (s *Server) importParams(in importRequest, owner, startedBy string) imapimport.CreateJobParams {
	return imapimport.CreateJobParams{
		Owner:            owner,
		StartedBy:        startedBy,
		RemoteHost:       in.RemoteHost,
		RemotePort:       in.RemotePort,
		RemoteTLS:        in.RemoteTLS,
		RemoteUsername:   in.RemoteUsername,
		RemotePassword:   in.RemotePassword,
		AcceptSelfSigned: in.AcceptSelfSigned,
		SourceRoot:       in.SourceRoot,
		TargetRoot:       in.TargetRoot,
	}
}

// --- user routes ---

func (s *Server) handleListImports(w http.ResponseWriter, r *http.Request) {
	owner := mailOwner(r)
	jobs, err := s.imports.ListJobs(r.Context(), &owner)
	if err != nil {
		importErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"imports": jobs})
}

func (s *Server) handleCreateImport(w http.ResponseWriter, r *http.Request) {
	actor := Principal(r.Context())
	if err := refuseSharedPrincipal(actor); err != nil {
		importErrors(w, s, err)
		return
	}
	var in importRequest
	if err := decodeImportBody(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	job, err := s.imports.CreateJob(r.Context(), s.importParams(in, mailOwner(r), actor.Address))
	if err != nil {
		importErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleGetImport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := s.imports.GetJob(r.Context(), id)
	if err != nil {
		importErrors(w, s, err)
		return
	}
	if job.Owner != mailOwner(r) {
		s.writeError(w, http.StatusNotFound, "no such job")
		return
	}
	s.writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelImport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := s.imports.CancelJob(r.Context(), id)
	if err != nil {
		importErrors(w, s, err)
		return
	}
	if job.Owner != mailOwner(r) {
		s.writeError(w, http.StatusNotFound, "no such job")
		return
	}
	s.writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleRemoteFolders(w http.ResponseWriter, r *http.Request) {
	var in remoteFoldersRequest
	if err := decodeImportBody(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	folders, err := s.imports.ListRemoteFolders(r.Context(), imapimport.RemoteParams{
		Host:             in.RemoteHost,
		Port:             in.RemotePort,
		TLS:              in.RemoteTLS,
		Username:         in.RemoteUsername,
		Password:         in.RemotePassword,
		AcceptSelfSigned: in.AcceptSelfSigned,
	})
	if err != nil {
		importErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

// --- admin routes ---

func (s *Server) handleAdminListImports(w http.ResponseWriter, r *http.Request) {
	if err := requireImportAdmin(Principal(r.Context())); err != nil {
		importErrors(w, s, err)
		return
	}
	var owner *string
	if v := r.URL.Query().Get("owner"); v != "" {
		owner = &v
	}
	jobs, err := s.imports.ListJobs(r.Context(), owner)
	if err != nil {
		importErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"imports": jobs})
}

func (s *Server) handleAdminCreateImport(w http.ResponseWriter, r *http.Request) {
	actor := Principal(r.Context())
	if err := requireImportAdmin(actor); err != nil {
		importErrors(w, s, err)
		return
	}
	var in importRequest
	if err := decodeImportBody(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	owner := in.Owner
	if owner == "" {
		s.writeError(w, http.StatusBadRequest, "target address is required")
		return
	}
	job, err := s.imports.CreateJob(r.Context(), s.importParams(in, owner, actor.Address))
	if err != nil {
		importErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleAdminGetImport(w http.ResponseWriter, r *http.Request) {
	if err := requireImportAdmin(Principal(r.Context())); err != nil {
		importErrors(w, s, err)
		return
	}
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := s.imports.GetJob(r.Context(), id)
	if err != nil {
		importErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleAdminCancelImport(w http.ResponseWriter, r *http.Request) {
	if err := requireImportAdmin(Principal(r.Context())); err != nil {
		importErrors(w, s, err)
		return
	}
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := s.imports.CancelJob(r.Context(), id)
	if err != nil {
		importErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleAdminRemoteFolders(w http.ResponseWriter, r *http.Request) {
	if err := requireImportAdmin(Principal(r.Context())); err != nil {
		importErrors(w, s, err)
		return
	}
	var in remoteFoldersRequest
	if err := decodeImportBody(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	folders, err := s.imports.ListRemoteFolders(r.Context(), imapimport.RemoteParams{
		Host:             in.RemoteHost,
		Port:             in.RemotePort,
		TLS:              in.RemoteTLS,
		Username:         in.RemoteUsername,
		Password:         in.RemotePassword,
		AcceptSelfSigned: in.AcceptSelfSigned,
	})
	if err != nil {
		importErrors(w, s, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

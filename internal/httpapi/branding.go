package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/blobstore"
)

// mountBranding adds the public image route. A logo is shown in the top bar and
// in a meeting room, including to guests who have no account, so it cannot sit
// behind requireAuth; the unguessable handle in the URL is what selects it.
func (s *Server) mountBranding(r chi.Router) {
	r.Get("/branding/logo", s.handleOrganizationLogo)
	r.Get("/branding/themes/{uid}/*", s.handleThemeAsset)
	r.With(s.requireAuth).Get("/theme-options", s.handleThemeOptions)
}

// handleOrganizationLogo serves an organization's logo by its public handle.
func (s *Server) handleOrganizationLogo(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("org")
	wg, err := s.workgroups.ByLogoUID(r.Context(), s.db, uid)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "no logo")
		return
	}
	var digest blobstore.Digest
	if s.blobs == nil || len(wg.LogoSHA256) != len(digest) {
		s.writeError(w, http.StatusNotFound, "no logo")
		return
	}
	copy(digest[:], wg.LogoSHA256)
	data, err := s.blobs.Get(digest)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "no logo")
		return
	}
	mimeType := wg.LogoMIME
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	// The URL carries the digest prefix, so a new image is a new URL and the
	// old one may be cached for a long time.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// newLogoUID returns the public handle a logo URL names.
func newLogoUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

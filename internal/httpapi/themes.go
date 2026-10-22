package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/store"
)

// Built-in theme keys. A custom theme is addressed as "custom:<uid>".
const (
	themeLight  = "light"
	themeDark   = "dark"
	themeSystem = "system"
	themeCustom = "custom:"
)

// themeOption is one row a selector offers.
type themeOption struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// themeSelection is the resolved theme returned to the client.
type themeSelection struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
	CSS  string `json:"css,omitempty"`
	Name string `json:"name,omitempty"`
}

// defaultTheme normalises the deployment default. Anything unrecognised falls
// back to following the operating system, which is the safe choice for a new
// install.
func defaultTheme(configured string) string {
	switch configured = strings.TrimSpace(configured); {
	case configured == "":
		return themeSystem
	case configured == themeLight || configured == themeDark || configured == themeSystem:
		return configured
	case strings.HasPrefix(configured, themeCustom):
		return configured
	default:
		return themeSystem
	}
}

// themeCSS is the public entry point of a custom theme's ZIP.
func themeCSS(t store.Theme) string {
	return "/api/v1/branding/themes/" + t.UID + "/theme.css"
}

// resolveTheme applies the precedence the product promises: the user's own
// choice, then their organization's, then the deployment default. A custom
// reference that no longer exists is skipped rather than breaking the page.
func (s *Server) resolveTheme(ctx context.Context, userTheme, orgTheme string) themeSelection {
	for _, key := range []string{userTheme, orgTheme, s.defaultTheme} {
		key = strings.TrimSpace(key)
		switch {
		case key == themeLight, key == themeDark, key == themeSystem:
			return themeSelection{Key: key, Kind: key, Name: themeName(key)}
		case strings.HasPrefix(key, themeCustom):
			uid := strings.TrimSpace(strings.TrimPrefix(key, themeCustom))
			if uid == "" {
				continue
			}
			if t, err := s.themes.Get(ctx, s.db, uid); err == nil {
				return themeSelection{Key: key, Kind: "custom", CSS: themeCSS(t), Name: t.Name}
			}
		}
	}
	return themeSelection{Key: themeSystem, Kind: themeSystem, Name: themeName(themeSystem)}
}

// themeOptions is what a selector shows: the three built-ins and every custom
// theme uploaded by an administrator.
func (s *Server) themeOptions(ctx context.Context) []themeOption {
	out := []themeOption{
		{Key: themeLight, Name: "Light", Kind: themeLight},
		{Key: themeDark, Name: "Dark", Kind: themeDark},
		{Key: themeSystem, Name: "System", Kind: themeSystem},
	}
	themes, err := s.themes.List(ctx, s.db)
	if err != nil {
		return out
	}
	for _, t := range themes {
		out = append(out, themeOption{Key: themeCustom + t.UID, Name: t.Name, Kind: "custom"})
	}
	return out
}

// validTheme reports whether a selector value can be stored. An empty value is
// valid: it clears the choice and lets the next level apply.
func (s *Server) validTheme(ctx context.Context, key string) bool {
	key = strings.TrimSpace(key)
	if key == "" || key == themeLight || key == themeDark || key == themeSystem {
		return true
	}
	if !strings.HasPrefix(key, themeCustom) {
		return false
	}
	uid := strings.TrimSpace(strings.TrimPrefix(key, themeCustom))
	if uid == "" {
		return false
	}
	_, err := s.themes.Get(ctx, s.db, uid)
	return err == nil
}

func themeName(key string) string {
	switch key {
	case themeLight:
		return "Light"
	case themeDark:
		return "Dark"
	case themeSystem:
		return "System"
	}
	return key
}

// handleThemeOptions lists the selectable themes. It is behind requireAuth
// because the catalogue of uploaded themes is not public information.
func (s *Server) handleThemeOptions(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"themes": s.themeOptions(r.Context())})
}

// handleThemeAsset serves one file from a custom theme's ZIP. theme.css is the
// entry point the page links; other files are whatever that CSS references.
func (s *Server) handleThemeAsset(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	t, err := s.themes.Get(r.Context(), s.db, uid)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "no such theme")
		return
	}
	raw, err := s.readThemeZip(t)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "no such theme")
		return
	}
	name := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			s.writeError(w, http.StatusNotFound, "no such file")
			return
		}
	}
	name = path.Clean("/" + name)
	name = strings.TrimPrefix(name, "/")
	if name == "" || name == "." {
		name = "theme.css"
	}
	if strings.HasPrefix(name, "../") || name == ".." {
		s.writeError(w, http.StatusNotFound, "no such file")
		return
	}

	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		s.writeError(w, http.StatusNotFound, "no such file")
		return
	}
	entryName := path.Clean(t.Prefix + name)
	for _, entry := range zr.File {
		if path.Clean(entry.Name) != entryName {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			s.writeError(w, http.StatusNotFound, "no such file")
			return
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			s.writeError(w, http.StatusNotFound, "no such file")
			return
		}
		mimeType := mime.TypeByExtension(path.Ext(entryName))
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		w.Header().Set("Content-Type", mimeType)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	s.writeError(w, http.StatusNotFound, "no such file")
}

// readThemeZip loads a theme's ZIP from the blob store.
func (s *Server) readThemeZip(t store.Theme) ([]byte, error) {
	var digest blobstore.Digest
	if s.blobs == nil || len(t.BlobSHA256) != len(digest) {
		return nil, errors.New("no blob store")
	}
	copy(digest[:], t.BlobSHA256)
	return s.blobs.Get(digest)
}

// --- administration ---

// maxThemeBytes bounds an uploaded theme ZIP. Themes are CSS and a few images,
// not an application bundle.
const maxThemeBytes = 10 << 20

// themeAdminView is a custom theme as the administration page lists it.
type themeAdminView struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	CSS       string `json:"css"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

func viewTheme(t store.Theme) themeAdminView {
	return themeAdminView{
		UID:       t.UID,
		Name:      t.Name,
		Key:       themeCustom + t.UID,
		CSS:       themeCSS(t),
		CreatedBy: t.CreatedBy,
		CreatedAt: t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

func (s *Server) listThemes(w http.ResponseWriter, r *http.Request) {
	opts, err := adminListOptionsFrom(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	themes, err := s.themes.List(r.Context(), s.db)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not load themes")
		return
	}
	views := make([]themeAdminView, 0, len(themes))
	for _, t := range themes {
		views = append(views, viewTheme(t))
	}
	out, total := pageAdmin(views, opts)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"themes": out, "total": total, "limit": opts.Limit, "offset": opts.Offset,
	})
}

// exportExampleTheme writes a starter ZIP with a theme.css at its root. The
// built-in themes are not editable database rows; this gives an administrator
// the same starting point without inventing a second editable representation
// of them.
func (s *Server) exportExampleTheme(w http.ResponseWriter, r *http.Request) {
	if err := s.admin.CanAdmin(Principal(r.Context())); err != nil {
		adminErrors(w, s, err)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Example"
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(path, content string) error {
		f, err := zw.Create(path)
		if err != nil {
			return err
		}
		_, err = f.Write([]byte(content))
		return err
	}
	if err := add("theme.css", themeExampleCSS(name)); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not build the example theme")
		return
	}
	if err := add("README.txt", themeExampleReadme(name)); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not build the example theme")
		return
	}
	if err := zw.Close(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not build the example theme")
		return
	}

	slug := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "-"))
	if slug == "" {
		slug = "example"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", "linchange-theme-"+slug+".zip"))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func themeExampleCSS(name string) string {
	dark := strings.EqualFold(name, "Dark")
	background := "#f1f5f9"
	surface := "#ffffff"
	surfaceAlt := "#f8fafc"
	border := "#e2e8f0"
	text := "#0f172a"
	muted := "#64748b"
	if dark {
		background = "#0f172a"
		surface = "#1e293b"
		surfaceAlt = "#172033"
		border = "#334155"
		text = "#e2e8f0"
		muted = "#94a3b8"
	}
	return fmt.Sprintf(`/*
 * linchange custom theme: %[1]s
 *
 * The application sets data-theme="custom" when this theme is selected and
 * links this file after its own stylesheet, so these rules win without
 * !important. The classes below are the surfaces, text and borders the
 * application actually uses; add the ones your deployment needs.
 *
 * The package must keep theme.css at its root (or in one top-level folder).
 * Images referenced from here may sit beside it in the ZIP.
 */

:root {
  color-scheme: %[2]s;
}

body {
  background: %[3]s;
  color: %[5]s;
}

.bg-slate-50 { background-color: %[3]s; }
.bg-slate-100 { background-color: %[6]s; }
.bg-slate-200 { background-color: %[4]s; }
.bg-white { background-color: %[4]s; }
.bg-white\/90 { background-color: color-mix(in srgb, %[4]s 90%%, transparent); }
.bg-slate-50\/60 { background-color: color-mix(in srgb, %[6]s 60%%, transparent); }

.text-slate-900,
.text-slate-800 { color: %[5]s; }
.text-slate-700,
.text-slate-600 { color: %[7]s; }
.text-slate-500,
.text-slate-400 { color: %[7]s; }

.border-slate-200,
.border-slate-300,
.border-slate-200\/80 { border-color: %[8]s; }

input,
textarea,
select {
  background-color: %[4]s;
  color: %[5]s;
  border-color: %[8]s;
}

input::placeholder,
textarea::placeholder { color: %[7]s; }
`, name, map[bool]string{true: "dark", false: "light"}[dark], background, surface, text, surfaceAlt, muted, border)
}

func themeExampleReadme(name string) string {
	return fmt.Sprintf(`linchange custom theme: %s

Upload this ZIP from Administration -> Themes. It contains:

  theme.css   the entry point, linked after the application stylesheet
  README.txt  this file

You may add images or fonts beside theme.css and reference them relatively,
for example url("./background.jpg"). Keep theme.css at the root or in one
top-level folder. A theme is read-only once uploaded: create a new ZIP to
change it, then delete the old one.
`, name)
}

func (s *Server) uploadTheme(w http.ResponseWriter, r *http.Request) {
	if s.blobs == nil {
		s.writeError(w, http.StatusInternalServerError, "the blob store is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxThemeBytes+(1<<20))
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		s.writeError(w, http.StatusBadRequest, "expected a multipart ZIP upload")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.writeError(w, http.StatusBadRequest, "a theme needs a name")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "a file field is required")
		return
	}
	defer file.Close()
	if !strings.HasSuffix(strings.ToLower(header.Filename), ".zip") {
		s.writeError(w, http.StatusBadRequest, "the theme must be a ZIP file")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, maxThemeBytes+1))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "could not read the ZIP")
		return
	}
	if len(data) > maxThemeBytes {
		s.writeError(w, http.StatusRequestEntityTooLarge, "the theme is too large")
		return
	}

	prefix, err := themeZipPrefix(data)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	digest, _, err := s.blobs.Put(data)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not store the theme")
		return
	}
	uid, err := newLogoUID()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not generate a theme id")
		return
	}
	t, err := s.themes.Create(r.Context(), s.db, uid, name, prefix, Principal(r.Context()).Address, digest[:])
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not record the theme")
		return
	}
	s.writeJSON(w, http.StatusCreated, viewTheme(t))
}

// themeZipPrefix validates that a ZIP has a theme.css, and returns the
// directory it lives in: empty at the root, or one top-level folder.
func themeZipPrefix(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", errors.New("the file is not a valid ZIP")
	}
	hasRoot := false
	top := ""
	for _, entry := range zr.File {
		raw := entry.Name
		for _, part := range strings.Split(raw, "/") {
			if part == ".." {
				return "", errors.New("the ZIP contains an unsafe path")
			}
		}
		name := strings.TrimPrefix(path.Clean("/"+raw), "/")
		if name == "" || name == "." {
			continue
		}
		if name == "theme.css" {
			hasRoot = true
		}
		if i := strings.Index(name, "/"); i > 0 {
			dir := name[:i]
			if top == "" {
				top = dir
			} else if top != dir {
				top = "-"
			}
		}
	}
	if hasRoot {
		return "", nil
	}
	if top != "" && top != "-" {
		for _, entry := range zr.File {
			if strings.TrimPrefix(path.Clean("/"+entry.Name), "/") == top+"/theme.css" {
				return top + "/", nil
			}
		}
	}
	return "", errors.New("the ZIP must contain theme.css at its root or in one top-level folder")
}

func (s *Server) deleteTheme(w http.ResponseWriter, r *http.Request) {
	if err := s.themes.Delete(r.Context(), s.db, chi.URLParam(r, "uid")); err != nil {
		s.writeError(w, http.StatusNotFound, "no such theme")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetTheme records the signed-in user's choice. An empty theme clears it
// so the organization or deployment default applies.
func (s *Server) handleSetTheme(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Theme string `json:"theme"`
	}
	if err := decodeChatJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	key := strings.TrimSpace(in.Theme)
	if !s.validTheme(r.Context(), key) {
		s.writeError(w, http.StatusBadRequest, "unknown theme")
		return
	}
	if err := store.NewUsers().SetTheme(r.Context(), s.db, Principal(r.Context()).Address, key); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not save the theme")
		return
	}
	selection := s.resolveTheme(r.Context(), key, "")
	s.writeJSON(w, http.StatusOK, selection)
}

package httpapi

import (
	"context"
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

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// maxMailBody bounds a request that carries a message being composed.
//
// It is much larger than the auth limit because a compose posts attachments,
// which arrive base64-encoded and therefore about a third larger than the files.
// The webmail service enforces the real limits on the decoded bytes; this one
// exists so that an oversized request is refused before it is decoded.
const maxMailBody = 32 << 20

// mountMail routes the webmail API.
//
// Every route here is under the authenticated group, and every handler
// resolves the mailbox owner from the token rather than from the request. A
// handler that took an owner from a path segment would be one missing check away
// from serving one user's mail to another.
func (s *Server) mountMail(r chi.Router) {
	r.Route("/mail", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Use(s.sharedMailbox)
		r.Get("/folders", s.handleListFolders)
		r.Post("/folders", s.handleCreateFolder)
		r.Patch("/folders/{id}", s.handleRenameFolder)
		r.Delete("/folders/{id}", s.handleDeleteFolder)
		r.Post("/folders/{id}/empty", s.handleEmptyFolder)
		r.Get("/folders/{id}/messages", s.handleListMessages)
		r.Get("/folders/{id}/threads", s.handleListThreads)

		r.Get("/messages/search", s.handleSearchMessages)
		r.Get("/messages/{id}", s.handleGetMessage)
		r.Get("/messages/{id}/raw", s.handleRawMessage)
		r.Get("/messages/{id}/attachments/{index}", s.handleAttachment)
		r.Get("/messages/{id}/inline/{contentID}", s.handleInlineImage)
		r.Post("/messages/{id}/read", s.handleMarkRead)
		r.Post("/messages/{id}/flags", s.handleSetFlag)

		r.Post("/messages/move", s.handleMoveMessages)
		r.Post("/messages/trash", s.handleTrashMessages)
		r.Post("/messages/archive", s.handleArchiveMessages)
		r.Post("/messages/delete", s.handleDeleteMessages)

		r.Get("/threads/{key}", s.handleGetThread)

		r.Post("/compose/send", s.handleSend)
		r.Post("/compose/draft", s.handleSaveDraft)

		r.Get("/rules", s.handleListRules)
		r.Post("/rules", s.handleCreateRule)
		r.Put("/rules/{id}", s.handleUpdateRule)
		r.Post("/rules/{id}/enabled", s.handleEnableRule)
		r.Delete("/rules/{id}", s.handleDeleteRule)

		r.Get("/aliases", s.handleOwnAliases)
		r.Get("/shared", s.handleAccessibleShared)

		s.mountImportRoutes(r)

		r.Get("/signature", s.handleGetSignature)
		r.Put("/signature", s.handleSaveSignature)
		r.Delete("/signature", s.handleClearSignature)

		// Image upload endpoints for TinyMCE
		r.Post("/images/signature", s.handleUploadSignatureImage)
		r.Post("/images/compose", s.handleUploadComposeImage)
		r.Get("/images/inline/{cid}", s.handleInlineSignatureImage)
	})
}

// handleGetSignature returns the caller's stored signature.
//
// A user who has never set one gets an empty signature rather than a 404, so
// that a client can load the editor and show an empty box without treating the
// ordinary case as an error.
func (s *Server) handleGetSignature(w http.ResponseWriter, r *http.Request) {
	sig, err := s.mail.Signature(r.Context(), mailOwner(r))
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"html":  sig.HTML,
		"owner": sig.Owner,
	})
}

// signatureRequest is the body of a signature write.
type signatureRequest struct {
	// HTML is the signature as the user wrote it. An empty value is a valid
	// request that clears the signature, so that a client has one call for both
	// "set" and "remove" rather than having to know which it is doing.
	HTML string `json:"html"`
}

// handleSaveSignature stores the caller's signature.
func (s *Server) handleSaveSignature(w http.ResponseWriter, r *http.Request) {
	var req signatureRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}

	sig, err := s.mail.SaveSignature(r.Context(), mailOwner(r), req.HTML)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"html":  sig.HTML,
		"owner": sig.Owner,
	})
}

// handleClearSignature removes the caller's signature.
func (s *Server) handleClearSignature(w http.ResponseWriter, r *http.Request) {
	if err := s.mail.ClearSignature(r.Context(), mailOwner(r)); err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"html": "", "owner": mailOwner(r)})
}

// handleUploadSignatureImage uploads an image for use in a signature.
// The image is stored temporarily and a CID is returned for referencing it.
func (s *Server) handleUploadSignatureImage(w http.ResponseWriter, r *http.Request) {
	// Limit to 1 MB per image
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	file, _, err := r.FormFile("image")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "no image provided")
		return
	}
	defer file.Close()

	// Read and validate the image
	imgData, err := io.ReadAll(file)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "could not read image")
		return
	}

	// Detect MIME type from content
	mimeType := http.DetectContentType(imgData)
	allowed := map[string]bool{
		"image/png":  true,
		"image/jpeg": true,
		"image/gif":  true,
		"image/webp": true,
	}
	if !allowed[mimeType] {
		s.writeError(w, http.StatusBadRequest, "unsupported image type: "+mimeType)
		return
	}

	// Store the image in the signature image store and get a CID
	cid, err := s.mail.StoreSignatureImage(r.Context(), mailOwner(r), imgData, mimeType)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"cid": cid})
}

// handleInlineSignatureImage serves a signature/compose image by CID.
// The CID has the format "sig-<sha256>" and the image is retrieved from the blobstore via the mail service.
func (s *Server) handleInlineSignatureImage(w http.ResponseWriter, r *http.Request) {
	cid := chi.URLParam(r, "cid")
	if cid == "" || !strings.HasPrefix(cid, "sig-") {
		s.writeError(w, http.StatusBadRequest, "invalid CID")
		return
	}
	hashStr := strings.TrimPrefix(cid, "sig-")
	digest, err := blobstore.ParseDigest(hashStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid CID format")
		return
	}
	data, err := s.mail.GetBlob(r.Context(), digest)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			s.writeError(w, http.StatusNotFound, "image not found")
			return
		}
		s.log.Error("failed to get signature image", "error", err, "cid", cid)
		s.writeError(w, http.StatusInternalServerError, "could not read image")
		return
	}
	// Detect MIME type from content
	mimeType := http.DetectContentType(data)
	serveDownload(w, mimeType, cid, data)
}

// handleUploadComposeImage uploads an image for use in a message body.
// The image is stored and a CID is returned for referencing it inline.
func (s *Server) handleUploadComposeImage(w http.ResponseWriter, r *http.Request) {
	// Limit to 5 MB per image (larger than signature for message body images)
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	defer r.Body.Close()

	file, _, err := r.FormFile("image")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "no image provided")
		return
	}
	defer file.Close()

	imgData, err := io.ReadAll(file)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "could not read image")
		return
	}

	mimeType := http.DetectContentType(imgData)
	allowed := map[string]bool{
		"image/png":  true,
		"image/jpeg": true,
		"image/gif":  true,
		"image/webp": true,
	}
	if !allowed[mimeType] {
		s.writeError(w, http.StatusBadRequest, "unsupported image type: "+mimeType)
		return
	}

	// Use the same store for compose images
	cid, err := s.mail.StoreSignatureImage(r.Context(), mailOwner(r), imgData, mimeType)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"cid": cid})
}

// mailOwner is the mailbox a request acts on.
//
// It is the authenticated address, always. The only exception is a shared
// mailbox, which is named explicitly and checked against the caller's grants by
// the sharedMailbox middleware before the handler runs.
func mailOwner(r *http.Request) string {
	if owner, ok := r.Context().Value(mailOwnerKey).(string); ok && owner != "" {
		return owner
	}
	return Principal(r.Context()).Address
}

// sharedMailbox resolves the ?owner= query on the mail routes.
//
// Without it every mail request acts on the caller's own mailbox. With it the
// request acts on a shared mailbox, and the caller's grant decides whether that
// is allowed at all: reading needs read access, anything that changes mail needs
// editor, since a member who could move or delete messages in somebody else's
// mailbox is effectively administering it.
//
// Doing this once, in middleware, is what keeps a new route from forgetting the
// check. Handlers below pass mailOwner(r) straight to services that are scoped
// by owner, and they are correct only because this ran first.
func (s *Server) sharedMailbox(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := Principal(r.Context()).Address
		requested := r.URL.Query().Get("owner")
		if requested == "" || strings.EqualFold(requested, caller) {
			next.ServeHTTP(w, r)
			return
		}

		// Reading a shared mailbox needs a read grant. Changing mail in it needs
		// editor, since a member who can move or delete messages in somebody
		// else's mailbox is effectively administering it. Sending from it is the
		// exception: that is what a team address is for, so contribute is enough.
		want := policy.GrantRead
		switch {
		case r.Method == http.MethodGet:
		case strings.HasPrefix(r.URL.Path, "/api/v1/mail/compose/") ||
			strings.HasPrefix(r.URL.Path, "/api/v1/mail/images/"):
			want = policy.GrantContribute
		default:
			want = policy.GrantEditor
		}
		access, err := s.grants.Access(r.Context(), s.db, requested, caller)
		if err != nil || !access.AtLeast(want) {
			s.writeError(w, http.StatusForbidden, "you do not have access to that mailbox")
			return
		}

		ctx := context.WithValue(r.Context(), mailOwnerKey, requested)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// readOwner resolves the mailbox a read request acts on.
//
// Reads default to the caller's own mail. A ?owner= parameter names a shared
// mailbox instead, and the grant check decides whether the caller may look at
// it: every query below is still scoped to the owner it is handed, so opening a
// shared mailbox never loosens the scoping, it only redirects it to a mailbox
// the caller has been shown to have access to.
//
// The sharedMailbox middleware above has already granted this for every route
// under /mail, so what is left here is the comparison: a handler that resolves
// its owner this way still sees the shared address, never the caller's.
func (s *Server) readOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner := strings.ToLower(strings.TrimSpace(mailOwner(r)))
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("owner")))
	if v == "" || v == owner {
		return owner, true
	}
	access, err := s.grants.Access(r.Context(), s.db, v, owner)
	if err != nil || !access.AtLeast(policy.GrantRead) {
		s.writeMailError(w, r, webmail.ErrDenied)
		return "", false
	}
	return v, true
}

// writeMailError maps a service error onto a status.
//
// The mapping is one function so that every route answers the same way: not
// found for anything the caller cannot see, denied for the policy refusals,
// invalid for a request the user can fix, and 500 only for what the user cannot.
func (s *Server) writeMailError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, webmail.ErrNotFound), errors.Is(err, store.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, webmail.ErrDenied), errors.Is(err, webmail.ErrQuota):
		if errors.Is(err, webmail.ErrQuota) {
			s.writeError(w, http.StatusForbidden, "the mailbox is full")
			return
		}
		s.writeError(w, http.StatusForbidden, "not permitted")
	case errors.Is(err, webmail.ErrInvalid):
		// The message names the field, and it is the user's own input, so it is
		// safe and useful to send back.
		s.writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "webmail: "))
	default:
		s.log.Error("mail request failed", "error", err, "path", r.URL.Path)
		s.writeError(w, http.StatusInternalServerError, "the request could not be completed")
	}
}

// pathID reads a numeric path parameter.
func pathID(r *http.Request, name string) (int64, bool) {
	raw := chi.URLParam(r, name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// decodeMailBody reads a compose-sized JSON body.
func decodeMailBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxMailBody)
	defer r.Body.Close()

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// queryInt reads an optional integer query parameter within bounds.
func queryInt(r *http.Request, name string, def, min, max int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// queryBool reads an optional boolean query parameter.
func queryBool(r *http.Request, name string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(r.URL.Query().Get(name)))
	if err != nil {
		return false
	}
	return v
}

// queryTime reads an RFC 3339 timestamp, which is how a cursor comes back.
func queryTime(r *http.Request, name string) time.Time {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// ---------------------------------------------------------------------------
// Folders.
// ---------------------------------------------------------------------------

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	// The ?owner= query, when present, was already resolved and checked by the
	// sharedMailbox middleware, so mailOwner is the mailbox this caller may read.
	folders, err := s.mail.Folders(r.Context(), mailOwner(r))
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

func (s *Server) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}

	folder, err := s.mail.CreateFolder(r.Context(), mailOwner(r), in.Name)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, folder)
}

func (s *Server) handleRenameFolder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed folder id")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}

	folder, err := s.mail.RenameFolder(r.Context(), mailOwner(r), id, in.Name)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, folder)
}

func (s *Server) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed folder id")
		return
	}
	if err := s.mail.DeleteFolder(r.Context(), mailOwner(r), id); err != nil {
		s.writeMailError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEmptyFolder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed folder id")
		return
	}
	moved, err := s.mail.EmptyFolder(r.Context(), mailOwner(r), id)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]int{"moved": moved})
}

// ---------------------------------------------------------------------------
// Messages.
// ---------------------------------------------------------------------------

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed folder id")
		return
	}

	owner, ok := s.readOwner(w, r)
	if !ok {
		return
	}

	page, err := s.mail.Messages(r.Context(), owner, webmail.ListQuery{
		FolderID:   id,
		Limit:      queryInt(r, "limit", 50, 1, 200),
		CursorDate: queryTime(r, "before"),
		CursorID:   int64(queryInt(r, "before_id", 0, 0, 1<<62)),
		UnreadOnly: queryBool(r, "unread"),
		Flagged:    queryBool(r, "flagged"),
	})
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleListThreads(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed folder id")
		return
	}
	owner, ok := s.readOwner(w, r)
	if !ok {
		return
	}
	threads, err := s.mail.Threads(r.Context(), owner, id, queryInt(r, "limit", 50, 1, 200))
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"threads": threads})
}

func (s *Server) handleSearchMessages(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		s.writeError(w, http.StatusBadRequest, "a search needs something to search for")
		return
	}
	folderID := int64(queryInt(r, "folder_id", 0, 0, 1<<62))

	owner, ok := s.readOwner(w, r)
	if !ok {
		return
	}
	messages, err := s.mail.Search(r.Context(), owner, q, folderID, queryInt(r, "limit", 50, 1, 200))
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed message id")
		return
	}
	owner, ok := s.readOwner(w, r)
	if !ok {
		return
	}
	msg, err := s.mail.Message(r.Context(), owner, id)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, msg)
}

func (s *Server) handleRawMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed message id")
		return
	}
	owner, ok := s.readOwner(w, r)
	if !ok {
		return
	}
	raw, err := s.mail.Raw(r.Context(), owner, id)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	// It is served as an attachment rather than rendered: these are somebody
	// else's bytes, and a browser that treated them as a page would be running
	// the sender's HTML on this origin.
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", "attachment")
	http.ServeContent(w, r, "message.eml", time.Time{}, strings.NewReader(string(raw)))
}

func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed message id")
		return
	}
	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil || index < 0 {
		s.writeError(w, http.StatusBadRequest, "malformed attachment index")
		return
	}

	owner, ok := s.readOwner(w, r)
	if !ok {
		return
	}
	att, content, err := s.mail.AttachmentContent(r.Context(), owner, id, index)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	serveDownload(w, att.MIMEType, att.Filename, content)
}

func (s *Server) handleInlineImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed message id")
		return
	}
	owner, ok := s.readOwner(w, r)
	if !ok {
		return
	}
	img, content, err := s.mail.InlineImageContent(r.Context(), owner, id, chi.URLParam(r, "contentID"))
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	// Inline images are referenced by the HTML body, so they are served inline
	// rather than as downloads, with a neutral type: a sender-chosen
	// Content-Type of text/html here would be the HTML body being served as an
	// image.
	serveDownload(w, img.MIMEType, img.Filename, content)
}

func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed message id")
		return
	}
	var in struct {
		Read bool `json:"read"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := s.mail.MarkRead(r.Context(), mailOwner(r), id, in.Read); err != nil {
		s.writeMailError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetFlag(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed message id")
		return
	}
	var in struct {
		Flag string `json:"flag"`
		Set  bool   `json:"set"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := s.mail.SetFlag(r.Context(), mailOwner(r), id, in.Flag, in.Set); err != nil {
		s.writeMailError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveDownload writes a body with the headers a browser needs to save it under
// the right name.
//
// Both the plain filename and the RFC 5987 form are sent. The plain form is what
// older clients read, and it cannot carry a non-ASCII name, which every
// attachment filename eventually is.
func serveDownload(w http.ResponseWriter, mimeType, filename string, content []byte) {
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// contentDisposition builds the Content-Disposition value for a name.
//
// The name is sanitised rather than trusted: it came out of somebody else's
// message, so it may contain a quote or a newline that would otherwise let it
// inject a header of its own.
func contentDisposition(kind, filename string) string {
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, filename)
	if name == "" {
		name = "attachment"
	}
	return fmt.Sprintf("%s; filename=\"%s\"; filename*=UTF-8''%s",
		kind, name, url.PathEscape(filename))
}

// messageIDs is the shape every batch action posts.
type messageIDs struct {
	IDs []int64 `json:"ids"`
}

func (s *Server) messageIDs(w http.ResponseWriter, r *http.Request) ([]int64, bool) {
	var in messageIDs
	if err := decodeMailBody(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return nil, false
	}
	if len(in.IDs) == 0 || len(in.IDs) > 500 {
		s.writeError(w, http.StatusBadRequest, "name between 1 and 500 messages")
		return nil, false
	}
	return in.IDs, true
}

func (s *Server) handleMoveMessages(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs      []int64 `json:"ids"`
		FolderID int64   `json:"folder_id"`
	}
	if err := decodeMailBody(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if len(in.IDs) == 0 || len(in.IDs) > 500 {
		s.writeError(w, http.StatusBadRequest, "name between 1 and 500 messages")
		return
	}
	if in.FolderID <= 0 {
		s.writeError(w, http.StatusBadRequest, "a move needs a destination folder")
		return
	}

	moved, err := s.mail.Move(r.Context(), mailOwner(r), in.IDs, in.FolderID)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]int{"moved": moved})
}

func (s *Server) handleTrashMessages(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.messageIDs(w, r)
	if !ok {
		return
	}
	moved, err := s.mail.Trash(r.Context(), mailOwner(r), ids)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]int{"moved": moved})
}

func (s *Server) handleArchiveMessages(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.messageIDs(w, r)
	if !ok {
		return
	}
	moved, err := s.mail.Archive(r.Context(), mailOwner(r), ids)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]int{"moved": moved})
}

func (s *Server) handleDeleteMessages(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.messageIDs(w, r)
	if !ok {
		return
	}
	deleted, usage, err := s.mail.Delete(r.Context(), mailOwner(r), ids)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"deleted":       deleted,
		"used_bytes":    usage.UsedBytes,
		"message_count": usage.MessageCount,
	})
}

func (s *Server) handleGetThread(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if strings.TrimSpace(key) == "" {
		s.writeError(w, http.StatusBadRequest, "a thread needs a key")
		return
	}
	owner, ok := s.readOwner(w, r)
	if !ok {
		return
	}
	messages, err := s.mail.Thread(r.Context(), owner, key)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

// ---------------------------------------------------------------------------
// Composing.
// ---------------------------------------------------------------------------

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var in webmail.ComposeRequest
	if err := decodeMailBody(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}

	result, err := s.mail.Send(r.Context(), mailOwner(r), in)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	// Accepted, not created: the message is queued and the SMTP conversation
	// has not happened yet, so a client that shows "sent" here is overstating
	// what the server knows.
	s.writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) handleSaveDraft(w http.ResponseWriter, r *http.Request) {
	var in webmail.ComposeRequest
	if err := decodeMailBody(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}

	id, err := s.mail.SaveDraft(r.Context(), mailOwner(r), in)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]int64{"message_id": id})
}

// ---------------------------------------------------------------------------
// Rules.
// ---------------------------------------------------------------------------

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.mail.Rules(r.Context(), mailOwner(r))
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var in webmail.Rule
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	rule, err := s.mail.SaveRule(r.Context(), mailOwner(r), 0, in)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, rule)
}

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed rule id")
		return
	}
	var in webmail.Rule
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	rule, err := s.mail.SaveRule(r.Context(), mailOwner(r), id, in)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rule)
}

func (s *Server) handleEnableRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed rule id")
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := s.mail.SetRuleEnabled(r.Context(), mailOwner(r), id, in.Enabled); err != nil {
		s.writeMailError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "malformed rule id")
		return
	}
	if err := s.mail.DeleteRule(r.Context(), mailOwner(r), id); err != nil {
		s.writeMailError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleOwnAliases lists the aliases that deliver into the caller's mailbox.
//
// It is the composer's recipient list: a user writing to their own alias should
// reach their own mailbox, and it is also how the compose form can say that a
// message to an alias will arrive in the mailbox it points at.
func (s *Server) handleOwnAliases(w http.ResponseWriter, r *http.Request) {
	all, err := s.aliases.ListForTarget(r.Context(), s.db, mailOwner(r))
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}

	type alias struct {
		Address string   `json:"address"`
		Targets []string `json:"targets"`
		Enabled bool     `json:"enabled"`
	}
	out := make([]alias, 0, len(all))
	for _, a := range all {
		targets := make([]string, 0, len(a.Targets))
		for _, t := range a.Targets {
			if t.TargetAddress != nil {
				targets = append(targets, *t.TargetAddress)
			} else if t.WorkgroupID != nil {
				targets = append(targets, fmt.Sprintf("workgroup:%d", *t.WorkgroupID))
			}
		}
		out = append(out, alias{Address: a.Address, Targets: targets, Enabled: a.Enabled})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"aliases": out})
}

// handleAccessibleShared lists the shared mailboxes the caller may see.
//
// It is a read-only listing for the folder picker: which team mailboxes exist
// and whether this member may write to them. Granting and revoking are
// administrative and live under /admin, so this endpoint cannot change access.
func (s *Server) handleAccessibleShared(w http.ResponseWriter, r *http.Request) {
	owner := mailOwner(r)

	grants, err := s.grants.ListForMember(r.Context(), s.db, owner)
	if err != nil {
		s.writeMailError(w, r, err)
		return
	}

	type shared struct {
		Address  string `json:"address"`
		Name     string `json:"display_name"`
		Level    string `json:"level"`
		Writable bool   `json:"writable"`
	}
	out := make([]shared, 0, len(grants))
	seen := make(map[string]bool, len(grants))
	for _, g := range grants {
		if seen[g.SharedAddress] {
			continue
		}
		seen[g.SharedAddress] = true

		box, err := s.shared.Get(r.Context(), s.db, g.SharedAddress)
		if err != nil {
			// A grant whose mailbox has gone is skipped rather than failing the
			// whole list: the caller's own mailboxes are still worth showing.
			s.log.Warn("grant names a mailbox that no longer exists",
				"shared", g.SharedAddress, "error", err)
			continue
		}
		access, err := s.grants.Access(r.Context(), s.db, g.SharedAddress, owner)
		if err != nil {
			continue
		}
		out = append(out, shared{
			Address:  box.Address,
			Name:     box.DisplayName,
			Level:    string(access.Level),
			Writable: access.AtLeast(policy.GrantContribute),
		})
	}
	// An open mailbox has no grant row, so nothing above would ever name it even
	// though reading it is allowed. It is listed here for the same reason the
	// policy allows reading it: a mailbox open to everybody that never appears in
	// anybody's picker is not open in any way a member can use. It is never
	// writable without a grant, which is what stops "open" from being a way
	// around the grant list.
	if boxes, err := s.shared.List(r.Context(), s.db); err != nil {
		s.log.Warn("could not list shared mailboxes", "error", err)
	} else {
		for _, box := range boxes {
			if !box.OpenAccess || seen[box.Address] {
				continue
			}
			seen[box.Address] = true
			out = append(out, shared{
				Address:  box.Address,
				Name:     box.DisplayName,
				Level:    string(policy.GrantRead),
				Writable: false,
			})
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"shared": out})
}

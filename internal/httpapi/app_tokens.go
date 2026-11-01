package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/secret"
	"github.com/linchange/linchange/internal/store"
)

// Application passwords are credentials of their own for mail, calendar and
// contact clients. The account password signs in to the web UI (with the second
// factor where enrolled); a client uses a password made here, named by whoever
// made it, and can have it revoked without changing the account password.
//
// A client that has no account password (MailCraft, or a phone) starts a short
// approval, opens the browser at the address it is given, signs in, and agrees
// to the description. It then polls with its device code and receives the new
// password once. The device code never reaches the browser and the account
// password never reaches the client.

// Application-password device flow lifetimes.
const (
	appTokenTTL      = 10 * time.Minute
	appTokenInterval = 5 * time.Second
	appTokenMaxDesc  = 120
)

// applicationPassword is what a client presents as its password. The prefix is
// public and names the row; the secret is compared against its digest.
const applicationPasswordPrefix = "lcapp_"

// handleAppTokenStart begins an approval. It is unauthenticated: a client that
// cannot sign in yet is the whole reason it exists. It sends no mail and reads
// nothing of the owner's.
func (s *Server) handleAppTokenStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Description string `json:"description"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if !s.appTokenIPLimits.Allow(clientIP(r).String()) {
		s.writeError(w, http.StatusTooManyRequests, "too many attempts; try again later")
		return
	}
	description := oneLine(in.Description, appTokenMaxDesc)
	if description == "" {
		description = "Application password"
	}

	deviceCode, err := secret.Generate(32)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not prepare the request")
		return
	}
	userCode, err := newUserCode()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not prepare the request")
		return
	}
	request, err := store.NewApplicationTokenRequests().Create(
		r.Context(), s.db, hex.EncodeToString(secret.Hash(deviceCode)), userCode,
		description, appTokenInterval, appTokenTTL,
	)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not prepare the request")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"device_code":      deviceCode,
		"user_code":        request.UserCode,
		"verification_url": s.appTokenVerificationURL(r, request.UserCode),
		"expires_in":       int(time.Until(request.ExpiresAt).Seconds()),
		"interval":         request.IntervalSeconds,
	})
}

// handleAppTokenPoll answers a client that is waiting for its approval. It
// returns the new password exactly once, after the browser has approved.
func (s *Server) handleAppTokenPoll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DeviceCode string `json:"device_code"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	deviceCode := strings.TrimSpace(in.DeviceCode)
	if deviceCode == "" {
		s.writeError(w, http.StatusBadRequest, "device_code is required")
		return
	}
	hash := hex.EncodeToString(secret.Hash(deviceCode))
	requests := store.NewApplicationTokenRequests()
	request, err := requests.ByDeviceHash(r.Context(), s.db, hash)
	if err != nil {
		if errors.Is(err, store.ErrNoApplicationRequest) {
			s.writeError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "could not read the request")
		return
	}
	if request.Expired(time.Now()) && request.Status == "pending" {
		s.writeError(w, http.StatusBadRequest, "expired_token")
		return
	}
	switch request.Status {
	case "pending":
		w.Header().Set("Cache-Control", "no-store")
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "authorization_pending"})
		return
	case "denied":
		s.writeError(w, http.StatusBadRequest, "access_denied")
		return
	case "consumed":
		s.writeError(w, http.StatusBadRequest, "invalid_grant")
		return
	case "approved":
	default:
		s.writeError(w, http.StatusBadRequest, "invalid_grant")
		return
	}

	tx, err := s.db.BeginTxx(r.Context(), nil)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not issue the password")
		return
	}
	defer func() { _ = tx.Rollback() }()
	approved, ok, err := requests.Redeem(r.Context(), tx, hash)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not issue the password")
		return
	}
	if !ok || approved.Address == nil {
		s.writeError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	password, row, err := s.createApplicationToken(r.Context(), tx, *approved.Address, approved.Description)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not issue the password")
		return
	}
	if err := tx.Commit(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not issue the password")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"token":       password,
		"username":    row.Address,
		"description": row.Description,
		"created_at":  row.CreatedAt,
	})
}

// handleListApplicationTokens lists the caller's live application passwords.
// Digests and prefixes are not secrets, but only the description and times are
// useful to the page, and the prefix is shown so a person can match a row with
// a client that has lost its label.
func (s *Server) handleListApplicationTokens(w http.ResponseWriter, r *http.Request) {
	address := Principal(r.Context()).Address
	tokens, err := store.NewApplicationTokens().List(r.Context(), s.db, address)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not read the application passwords")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// handleCreateApplicationToken makes one from the account page. The password is
// returned once and never stored in a readable form.
func (s *Server) handleCreateApplicationToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Description string `json:"description"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := Principal(r.Context()).Can(policy.ActionIMAP); err != nil {
		s.writeError(w, http.StatusForbidden, "this account cannot use mail clients")
		return
	}
	description := oneLine(in.Description, appTokenMaxDesc)
	if description == "" {
		description = "Application password"
	}
	password, row, err := s.createApplicationToken(r.Context(), s.db, Principal(r.Context()).Address, description)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not create the application password")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"token":       password,
		"description": row.Description,
		"created_at":  row.CreatedAt,
	})
}

// handleRevokeApplicationToken ends one of the caller's own passwords.
func (s *Server) handleRevokeApplicationToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, http.StatusBadRequest, "that is not an application password")
		return
	}
	if err := store.NewApplicationTokens().Revoke(r.Context(), s.db, Principal(r.Context()).Address, id); err != nil {
		if errors.Is(err, store.ErrNoApplicationToken) {
			s.writeError(w, http.StatusNotFound, "no such application password")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "could not revoke the application password")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleApplicationTokenRequest shows a pending approval to the signed-in
// person, so the page can say who is asking before they agree.
func (s *Server) handleApplicationTokenRequest(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	request, err := store.NewApplicationTokenRequests().ByUserCode(r.Context(), s.db, code)
	if err != nil {
		if errors.Is(err, store.ErrNoApplicationRequest) {
			s.writeError(w, http.StatusNotFound, "no such request")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "could not read the request")
		return
	}
	if request.Expired(time.Now()) && request.Status == "pending" {
		s.writeError(w, http.StatusGone, "this request has expired")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"user_code":   request.UserCode,
		"description": request.Description,
		"status":      request.Status,
		"expires_at":  request.ExpiresAt,
	})
}

// handleApplicationTokenDecision approves or denies the request a browser is
// showing. The signed-in account is the one the application password will
// belong to.
func (s *Server) handleApplicationTokenDecision(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Approve bool `json:"approve"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	principal := Principal(r.Context())
	if err := principal.Can(policy.ActionIMAP); err != nil {
		s.writeError(w, http.StatusForbidden, "this account cannot use mail clients")
		return
	}
	code := chi.URLParam(r, "code")
	requests := store.NewApplicationTokenRequests()
	if in.Approve {
		if err := requests.Approve(r.Context(), s.db, code, principal.Address); err != nil {
			if errors.Is(err, store.ErrNoApplicationRequest) {
				s.writeError(w, http.StatusGone, "this request cannot be answered any more")
				return
			}
			s.writeError(w, http.StatusInternalServerError, "could not approve the request")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
		return
	}
	if err := requests.Deny(r.Context(), s.db, code); err != nil {
		if errors.Is(err, store.ErrNoApplicationRequest) {
			s.writeError(w, http.StatusGone, "this request cannot be answered any more")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "could not deny the request")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
}

// createApplicationToken makes the credential and stores only its digest. The
// prefix names the row in the database; the secret is what the client sends.
func (s *Server) createApplicationToken(ctx context.Context, q store.Querier, address, description string) (string, store.ApplicationToken, error) {
	prefix, err := randomText(12, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	if err != nil {
		return "", store.ApplicationToken{}, fmt.Errorf("creating an application password: %w", err)
	}
	secretPart, err := secret.Generate(32)
	if err != nil {
		return "", store.ApplicationToken{}, fmt.Errorf("creating an application password: %w", err)
	}
	row, err := store.NewApplicationTokens().Create(
		ctx, q, address, prefix, hex.EncodeToString(secret.Hash(secretPart)), description,
	)
	if err != nil {
		return "", store.ApplicationToken{}, err
	}
	return applicationPasswordPrefix + prefix + "_" + secretPart, row, nil
}

// appTokenVerificationURL is the page the person opens to approve. It uses the
// origin the client reached this server at, so a deployment on a non-standard
// port or behind a proxy sends the browser to the address that works. It is an
// absolute address because the client hands it to a browser.
func (s *Server) appTokenVerificationURL(r *http.Request, userCode string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "https" || forwarded == "http" {
		scheme = forwarded
	}
	return scheme + "://" + r.Host + "/app-auth?code=" + userCode
}

// newUserCode makes the short code the page shows. It avoids characters that
// are easy to misread when somebody copies it by hand.
func newUserCode() (string, error) {
	const alphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"
	const length = 8
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading randomness: %w", err)
	}
	out := make([]byte, 0, length+1)
	for i, b := range buf {
		if i == length/2 {
			out = append(out, '-')
		}
		out = append(out, alphabet[int(b)%len(alphabet)])
	}
	return string(out), nil
}

// randomText makes a non-secret identifier from a chosen alphabet. It is used
// for the public prefix, which must not contain the underscore that separates
// it from the secret.
func randomText(length int, alphabet string) (string, error) {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading randomness: %w", err)
	}
	out := make([]byte, length)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

// oneLine keeps a description to one short line.
func oneLine(text string, max int) string {
	flat := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, text)
	flat = strings.Join(strings.Fields(flat), " ")
	if len([]rune(flat)) > max {
		flat = string([]rune(flat)[:max])
	}
	return strings.TrimSpace(flat)
}

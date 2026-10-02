package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/linchange/linchange/internal/auth/login"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/reset"
	"github.com/linchange/linchange/internal/auth/token"
	"github.com/linchange/linchange/internal/store"
)

// loginRequest is what the sign-in form posts.
type loginRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	// Impersonating and ImpersonationReason are set only by a domain admin who
	// signs in to act as somebody else.
	Impersonating       string `json:"impersonating,omitempty"`
	ImpersonationReason string `json:"impersonation_reason,omitempty"`
}

// authResponse is the single shape both a completed login, a completed second
// factor, and a refresh return, so the client stores the same credentials no
// matter which door it came in through.
type authResponse struct {
	// RequiresSecondFactor is true when the login stopped at a challenge.
	RequiresSecondFactor bool   `json:"requires_second_factor"`
	ChallengeID          string `json:"challenge_id,omitempty"`
	ChallengeExpiresAt   string `json:"challenge_expires_at,omitempty"`
	// The rest are set when the sign-in completed.
	TokenType          string `json:"token_type,omitempty"`
	AccessToken        string `json:"access_token,omitempty"`
	RefreshToken       string `json:"refresh_token,omitempty"`
	ExpiresIn          int64  `json:"expires_in,omitempty"`
	MustChangePassword bool   `json:"must_change_password"`
}

// grantedToResponse folds a completed login into the response the client keeps.
//
// The refresh token is the JWT, and the session it names is what the refresh
// endpoint checks for liveness; the plaintext exists only in this response and
// in the client's storage.
func grantedToResponse(g login.Granted) authResponse {
	return authResponse{
		TokenType:          "bearer",
		AccessToken:        g.Access.Token,
		RefreshToken:       g.Refresh.Token,
		ExpiresIn:          secondsUntil(g.Access.ExpiresAt),
		MustChangePassword: g.Session.Principal.MustChangePassword,
	}
}

// health answers the liveness probe with nothing sensitive in it.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in loginRequest
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}

	res, err := s.auth.Password(r.Context(), s.db, login.Request{
		Login:               in.Login,
		Password:            in.Password,
		IP:                  clientIP(r),
		UserAgent:           r.UserAgent(),
		Impersonating:       in.Impersonating,
		ImpersonationReason: in.ImpersonationReason,
	})
	if err != nil {
		switch {
		case errors.Is(err, login.ErrDirectoryUnavailable):
			s.writeError(w, http.StatusServiceUnavailable, "the directory is unavailable, please try again later")
		case errors.Is(err, login.ErrEnrolmentRequired):
			s.writeError(w, http.StatusForbidden, "a second factor must be enrolled before signing in")
		default:
			// Invalid credentials, a locked account, a disabled account and an
			// unknown address all collapse to one answer, or this endpoint is a
			// user list.
			s.writeError(w, http.StatusUnauthorized, "sign-in was not accepted")
		}
		return
	}

	if res.ChallengeID != "" {
		s.writeJSON(w, http.StatusOK, authResponse{
			RequiresSecondFactor: true,
			ChallengeID:          res.ChallengeID,
			ChallengeExpiresAt:   res.ExpiresAt.UTC().Format(time.RFC3339),
		})
		return
	}
	if res.Granted == nil {
		// The login package guarantees one of the two; reaching here is a bug.
		s.writeError(w, http.StatusInternalServerError, "sign-in did not complete")
		return
	}
	s.writeJSON(w, http.StatusOK, grantedToResponse(*res.Granted))
}

func (s *Server) handleSecondFactor(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChallengeID string `json:"challenge_id"`
		Code        string `json:"code"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}

	granted, err := s.auth.SecondFactor(r.Context(), s.db, login.SecondFactorRequest{
		ChallengeID: in.ChallengeID,
		Code:        in.Code,
		IP:          clientIP(r),
	})
	if err != nil {
		// A wrong code, a spent challenge and an expired one all read the same
		// to the caller, for the same anti-oracle reason as a bad password.
		s.writeError(w, http.StatusUnauthorized, "the code was not accepted")
		return
	}
	s.writeJSON(w, http.StatusOK, grantedToResponse(granted))
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}

	_, sessionID, err := s.tokens.Parse(in.RefreshToken, token.TypeRefresh)
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "an active sign-in is required")
		return
	}
	session, err := s.sessions.Load(r.Context(), s.db, sessionID)
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "an active sign-in is required")
		return
	}

	// The session's principal is the authority, not the token's cached copy.
	principal := session.Principal
	principal.Authenticated = true
	access, err := s.tokens.IssueAccess(principal, sessionID, session.Impersonating)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not issue a new token")
		return
	}
	refresh, err := s.tokens.IssueRefresh(principal, sessionID, session.Impersonating)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not issue a new token")
		return
	}

	s.writeJSON(w, http.StatusOK, authResponse{
		TokenType:          "bearer",
		AccessToken:        access.Token,
		RefreshToken:       refresh.Token,
		ExpiresIn:          secondsUntil(access.ExpiresAt),
		MustChangePassword: principal.MustChangePassword,
	})
}

// meResponse is what the client needs to render itself: who it is acting as,
// and whether that is somebody else.
//
// The impersonation target is here rather than left for the client to dig out
// of the access token. A JWT is a signed blob, not an API, and making the web
// UI decode one to decide whether to draw the "acting as" banner means the
// banner and the server can disagree about the same session.
type meResponse struct {
	Address        string `json:"address"`
	DisplayName    string `json:"display_name,omitempty"`
	Source         string `json:"source"`
	Enabled        bool   `json:"enabled"`
	MailboxEnabled bool   `json:"mailbox_enabled"`
	// IsDomainAdmin lets the client decide whether to offer administration at
	// all, without a failing request being the way it finds out.
	IsDomainAdmin bool `json:"is_domain_admin"`
	// Impersonating is the user a domain admin is acting as, or empty. It is
	// read from the session row rather than the token because the session is
	// the authority: revoking the session is what ends an impersonation.
	Impersonating      string `json:"impersonating,omitempty"`
	MustChangePassword bool   `json:"must_change_password"`
	// ImpersonationRequiresGrant reports whether this deployment demands an
	// impersonation_grants row. The administration page needs it: when it is
	// true the grant list is the allowlist of who may act as whom, and when it
	// is false the same list is only a record of intentions. An operator shown
	// the wrong one of those two will trust the wrong thing.
	ImpersonationRequiresGrant bool `json:"impersonation_requires_grant"`
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	principal := Principal(r.Context())
	session, err := s.sessions.Load(r.Context(), s.db, SessionID(r.Context()))
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "this sign-in is no longer active")
		return
	}
	out := meResponse{
		Address:                    principal.Address,
		Source:                     string(principal.Source),
		Enabled:                    principal.Enabled,
		MailboxEnabled:             principal.MailboxEnabled,
		IsDomainAdmin:              principal.Source == policy.SourceDomainAdmin,
		Impersonating:              session.Impersonating,
		MustChangePassword:         principal.MustChangePassword,
		ImpersonationRequiresGrant: s.policies.Impersonation.RequireGrant,
	}
	// The display name is decoration, so a lookup that fails must not fail the
	// request: the client can render an address on its own.
	if u, err := store.NewUsers().Get(r.Context(), s.db, principal.Address); err == nil {
		out.DisplayName = u.DisplayName
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleStopImpersonating ends an impersonating session.
//
// It revokes the session rather than issuing a new one, so the tokens the
// client is holding stop working immediately instead of being quietly swapped
// for a pair that acts as the admin. The client signs in again as itself,
// which is the only outcome where the admin is certainly acting as themselves.
func (s *Server) handleStopImpersonating(w http.ResponseWriter, r *http.Request) {
	session, err := s.sessions.Load(r.Context(), s.db, SessionID(r.Context()))
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "this sign-in is no longer active")
		return
	}
	if session.Impersonating == "" {
		// Already acting as themselves. Saying so is more useful than a 204
		// that leaves the client guessing whether it needed to sign in again.
		s.writeError(w, http.StatusConflict, "this sign-in is not impersonating anybody")
		return
	}
	if err := s.sessions.Revoke(r.Context(), s.db, session.ID); err != nil {
		s.writeError(w, http.StatusInternalServerError, "the session could not be ended")
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	id := SessionID(r.Context())
	if id == "" {
		s.writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := s.sessions.Revoke(r.Context(), s.db, id); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not end the session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResetRequest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Login string `json:"login"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}

	if _, err := s.reset.Request(r.Context(), s.db, reset.Request{Address: in.Login, IP: clientIP(r)}); err != nil {
		// A fault here is a server error, not "no email sent", so it is a 500
		// rather than a silent success that delivers nothing.
		s.log.ErrorContext(r.Context(), "reset request failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "could not process this request")
		return
	}
	// The same answer whether or not a link went out, or a reset form becomes
	// an account-enumeration oracle.
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleResetComplete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}

	if _, err := s.reset.Complete(r.Context(), s.db, reset.CompleteRequest{Token: in.Token, NewPassword: in.Password, IP: clientIP(r)}); err != nil {
		// Whoever completes a reset already holds the link, so naming the exact
		// problem leaks nothing and is what lets them correct it and retry.
		switch {
		case errors.Is(err, reset.ErrInvalidToken), errors.Is(err, reset.ErrExpired), errors.Is(err, reset.ErrAlreadyUsed):
			s.writeError(w, http.StatusBadRequest, err.Error())
		default:
			s.writeError(w, http.StatusBadRequest, "the password could not be set")
		}
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

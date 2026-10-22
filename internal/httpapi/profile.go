package httpapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image/png"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/qr"
	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/secret"
	"github.com/linchange/linchange/internal/store"
)

// mountProfile adds the signed-in user's own account routes. They sit outside
// /admin because every account owns its password, recovery address and second
// factor; an administrator changing somebody else's password is a different
// act and lives under /admin.
func (s *Server) mountProfile(r chi.Router) {
	r.Route("/profile", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/", s.handleProfile)
		r.Put("/password", s.handleProfilePassword)
		r.Put("/recovery-email", s.handleProfileRecoveryEmail)
		r.Post("/2fa/start", s.handleProfile2FAStart)
		r.Post("/2fa/confirm", s.handleProfile2FAConfirm)
		r.Post("/2fa/disable", s.handleProfile2FADisable)
	})
}

type profileTwoFactor struct {
	// Available is false when the deployment has second factors switched off.
	Available bool `json:"available"`
	Enabled   bool `json:"enabled"`
	// Pending is a secret generated but not yet confirmed with a code.
	Pending bool `json:"pending"`
}

type profileResponse struct {
	Address       string `json:"address"`
	DisplayName   string `json:"display_name,omitempty"`
	Source        string `json:"source"`
	RecoveryEmail string `json:"recovery_email,omitempty"`
	// MinPasswordLength is what a new password must reach, so the form can say
	// the real number instead of a guess.
	MinPasswordLength int              `json:"min_password_length"`
	TwoFactor         profileTwoFactor `json:"two_factor"`
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	address := Principal(r.Context()).Address
	out := profileResponse{
		Address:           address,
		Source:            string(Principal(r.Context()).Source),
		MinPasswordLength: s.minPasswordLength,
		TwoFactor: profileTwoFactor{
			Available: s.totpVerifier != nil,
		},
	}
	if u, err := store.NewUsers().Get(r.Context(), s.db, address); err == nil {
		out.DisplayName = u.DisplayName
		out.RecoveryEmail = u.RecoveryEmail
	}
	if rec, err := store.NewTOTP().Get(r.Context(), s.db, address); err == nil {
		out.TwoFactor.Enabled = rec.Enabled
		out.TwoFactor.Pending = !rec.Enabled
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleProfilePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeChatJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	address := Principal(r.Context()).Address

	cred, err := store.NewCredentials().Get(r.Context(), s.db, address)
	if errors.Is(err, store.ErrNoCredential) {
		s.writeError(w, http.StatusBadRequest, "this account's password is managed by your directory")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not read the account")
		return
	}
	ok, err := password.Verify(in.CurrentPassword, cred.Hash)
	if err != nil || !ok {
		s.writeError(w, http.StatusBadRequest, "the current password is incorrect")
		return
	}
	if err := password.Validate(in.NewPassword, s.minPasswordLength); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := password.Hash(in.NewPassword, s.argon2)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not set the password")
		return
	}
	if err := store.NewCredentials().Set(r.Context(), s.db, address, hash); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not set the password")
		return
	}
	// A forced change is satisfied by changing the password.
	_ = store.SetMustChangePassword(r.Context(), s.db, address, false)
	// Other devices opened under the old password are signed out. This tab
	// keeps its session, so the user is not logged out of the page they are on.
	_, _ = s.sessions.RevokeAllExcept(r.Context(), s.db, address, SessionID(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleProfileRecoveryEmail(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if err := decodeChatJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email != "" {
		addr, err := mail.ParseAddress(email)
		if err != nil || !strings.EqualFold(addr.Address, email) {
			s.writeError(w, http.StatusBadRequest, "that is not a valid email address")
			return
		}
	}
	if err := store.NewUsers().SetRecoveryEmail(r.Context(), s.db, Principal(r.Context()).Address, email); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not save the recovery email")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type twoFactorStartResponse struct {
	// QR is a data URL for an authenticator-compatible QR code.
	QR            string   `json:"qr"`
	Secret        string   `json:"secret"`
	URI           string   `json:"uri"`
	RecoveryCodes []string `json:"recovery_codes"`
}

func (s *Server) handleProfile2FAStart(w http.ResponseWriter, r *http.Request) {
	if s.totpVerifier == nil {
		s.writeError(w, http.StatusBadRequest, "two-factor authentication is disabled on this server")
		return
	}
	address := Principal(r.Context()).Address
	if rec, err := store.NewTOTP().Get(r.Context(), s.db, address); err == nil && rec.Enabled {
		// Overwriting a live enrolment with an unconfirmed one would turn the
		// second factor off until the new code is entered.
		s.writeError(w, http.StatusBadRequest, "two-factor authentication is already enabled")
		return
	}
	enrolment, err := s.totpVerifier.Enrol(address)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not create a secret")
		return
	}
	enc, err := s.totpVerifier.Encrypt(enrolment.Secret, enrolment.RecoveryDigests)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not prepare the secret")
		return
	}
	// Stored disabled. A secret nobody has proved they hold must not count as
	// a factor.
	if err := store.NewTOTP().Store(r.Context(), s.db, address, enc.Ciphertext, enc.RecoveryDigests, false); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not store the secret")
		return
	}
	qrData, err := qrDataURL(enrolment.URI)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not draw the QR code")
		return
	}
	s.writeJSON(w, http.StatusOK, twoFactorStartResponse{
		QR:            qrData,
		Secret:        enrolment.Secret,
		URI:           enrolment.URI,
		RecoveryCodes: enrolment.RecoveryCodes,
	})
}

func (s *Server) handleProfile2FAConfirm(w http.ResponseWriter, r *http.Request) {
	if s.totpVerifier == nil {
		s.writeError(w, http.StatusBadRequest, "two-factor authentication is disabled on this server")
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeChatJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	address := Principal(r.Context()).Address
	rec, err := store.NewTOTP().Get(r.Context(), s.db, address)
	if errors.Is(err, store.ErrNoTOTP) {
		s.writeError(w, http.StatusBadRequest, "start the setup first")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not read the enrolment")
		return
	}
	if rec.Enabled {
		s.writeError(w, http.StatusBadRequest, "two-factor authentication is already enabled")
		return
	}
	step, err := s.totpVerifier.Verify(rec.SecretEncrypted, in.Code, time.Time{}, time.Now())
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "that code is not valid")
		return
	}
	if err := store.NewTOTP().Store(r.Context(), s.db, address, rec.SecretEncrypted, rec.RecoveryDigests, true); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not enable two-factor authentication")
		return
	}
	_ = store.NewTOTP().MarkUsed(r.Context(), s.db, address, step)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleProfile2FADisable(w http.ResponseWriter, r *http.Request) {
	if s.totpVerifier == nil {
		s.writeError(w, http.StatusBadRequest, "two-factor authentication is disabled on this server")
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeChatJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	address := Principal(r.Context()).Address
	rec, err := store.NewTOTP().Get(r.Context(), s.db, address)
	if errors.Is(err, store.ErrNoTOTP) {
		s.writeError(w, http.StatusBadRequest, "two-factor authentication is not enabled")
		return
	}
	if err != nil || !rec.Enabled {
		s.writeError(w, http.StatusBadRequest, "two-factor authentication is not enabled")
		return
	}

	now := time.Now()
	step, verr := s.totpVerifier.Verify(rec.SecretEncrypted, in.Code, rec.LastUsedAt.Time, now)
	if verr == nil {
		_ = store.NewTOTP().MarkUsed(r.Context(), s.db, address, step)
	} else {
		index, ok := secret.VerifyRecoveryCode(in.Code, rec.RecoveryDigests)
		if !ok {
			s.writeError(w, http.StatusBadRequest, "that code is not valid")
			return
		}
		spent, err := store.NewTOTP().ConsumeRecoveryCode(r.Context(), s.db, address, rec.RecoveryDigests[index])
		if err != nil || !spent {
			s.writeError(w, http.StatusBadRequest, "that recovery code has already been used")
			return
		}
	}
	if err := store.NewTOTP().Delete(r.Context(), s.db, address); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not disable two-factor authentication")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// qrDataURL renders an otpauth:// URI as a PNG QR code the page can show
// without a second request or a third-party image service.
func qrDataURL(content string) (string, error) {
	code, err := qr.Encode(content, qr.M, qr.Auto)
	if err != nil {
		return "", err
	}
	code, err = barcode.Scale(code, 256, 256)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, code); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

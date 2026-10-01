// Package httpapi is the chi router behind the REST API and the web interface.
//
// It is deliberately a thin seam: it knows how to speak HTTP and how to turn a
// request into a call on the login and reset orchestrators, and nothing more.
// The security decisions live in those packages, so a rule is enforced once
// regardless of which listener a request arrives on.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/lockout"
	"github.com/linchange/linchange/internal/auth/login"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/reset"
	"github.com/linchange/linchange/internal/auth/token"
	"github.com/linchange/linchange/internal/auth/totp"
	"github.com/linchange/linchange/internal/store"
)

// maxAuthBody bounds a request body to what an auth payload can plausibly be,
// so a client cannot stream gigabytes into a decode.
const maxAuthBody = 4096

// context keys for the principal and session the middleware resolves.
type contextKey int

const (
	principalKey contextKey = iota
	sessionIDKey
)

// Server holds the long-lived dependencies the handlers share.
type Server struct {
	db       *sqlx.DB
	log      *slog.Logger
	auth     *login.Authenticator
	reset    *reset.Resetter
	tokens   *token.Manager
	sessions *store.Sessions
}

// Options are the inputs to New.
type Options struct {
	DB     *sqlx.DB
	Config *config.Config
	Logger *slog.Logger
}

// New builds a Server from an open database and a loaded config.
//
// It wires the login and reset orchestrators the same way every other listener
// does, which is the point of keeping authentication in one place. A concrete
// LDAP provider is not wired yet: a nil Directory makes the login package deny
// directory sign-ins rather than fall through to a stored hash.
func New(opts Options) (*Server, error) {
	if opts.DB == nil {
		return nil, errors.New("httpapi: a database handle is required")
	}
	cfg := opts.Config
	if cfg == nil {
		return nil, errors.New("httpapi: config is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	tokens, err := token.NewManager(cfg.Auth.JWT)
	if err != nil {
		return nil, err
	}

	// A deployment that has not enabled a second factor has no verifier, and
	// the login package treats a nil verifier as "no second factor here".
	var verifier *totp.Verifier
	if cfg.Auth.TOTP.Enabled {
		verifier, err = totp.New(cfg.Auth.TOTP, cfg.Auth.EncryptionKey)
		if err != nil {
			return nil, err
		}
	}

	auth, err := login.New(login.Options{
		Credentials: store.NewCredentials(),
		Enrolments:  store.NewTOTP(),
		Attempts:    store.NewAuthAttempts(),
		Challenges:  store.NewChallenges(),
		Sessions:    store.NewSessions(),
		Tokens:      tokens,
		TOTP:        verifier,
		Argon2:      cfg.Auth.Local.Argon2,
		Lockout: lockout.Both{
			Account: lockout.Policy{
				MaxAttempts: cfg.Auth.Lockout.Account.MaxAttempts,
				Window:      cfg.Auth.Lockout.Account.Window,
				Duration:    cfg.Auth.Lockout.Account.Duration,
			},
			IP: lockout.Policy{
				MaxAttempts: cfg.Auth.Lockout.IP.MaxAttempts,
				Window:      cfg.Auth.Lockout.IP.Window,
				Duration:    cfg.Auth.Lockout.IP.Duration,
			},
		},
		Policy: policy.Policy{
			Impersonation: policy.Impersonation{
				Allowed:       cfg.Auth.Impersonation.Allowed,
				RequireReason: cfg.Auth.Impersonation.RequireReason,
			},
		},
		ChallengeTTL: cfg.Auth.Challenge.TTL,
	})
	if err != nil {
		return nil, err
	}

	// The reset link email is an outbox send that arrives with P3; until then a
	// nil sinker means the token is stored and treated as sent, which keeps the
	// flow exercisable end to end.
	r, err := reset.New(reset.Options{
		SecondaryEmails:   store.NewSecondaryEmails(),
		Tokens:            store.NewResetTokens(),
		Credentials:       store.NewCredentials(),
		Sessions:          store.NewSessions(),
		Sinker:            nil,
		Enabled:           cfg.Auth.Reset.Enabled,
		Argon2:            cfg.Auth.Local.Argon2,
		MinPasswordLength: cfg.Auth.Local.MinPasswordLength,
		TokenTTL:          cfg.Auth.Reset.TokenTTL,
		Logger:            log,
	})
	if err != nil {
		return nil, err
	}

	return &Server{
		db:       opts.DB,
		log:      log,
		auth:     auth,
		reset:    r,
		tokens:   tokens,
		sessions: store.NewSessions(),
	}, nil
}

// Handler builds the chi router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// No IP middleware runs by default, so the client IP is the direct peer.
	// Behind a reverse proxy, add middleware.ClientIPFromXFF(trusted CIDRs)
	// here: clientIP then prefers the value it publishes over RemoteAddr.
	r.Use(s.recover)
	r.NotFound(notFound)
	r.MethodNotAllowed(methodNotAllowed)

	r.Get("/healthz", s.health)

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/second-factor", s.handleSecondFactor)
		r.Post("/auth/refresh", s.handleRefresh)
		r.Post("/auth/reset/request", s.handleResetRequest)
		r.Post("/auth/reset/complete", s.handleResetComplete)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Post("/auth/logout", s.handleLogout)
		})
	})

	return r
}

// requireAuth validates the bearer access token and confirms the session it
// names is still live before the handler runs.
//
// The token proves the caller minted it; the session check proves it still may
// be used. Access tokens are a cache, and this is where the cache is checked.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := token.BearerToken(r.Header.Get("Authorization"))
		if err != nil {
			s.writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		principal, sessionID, err := s.tokens.Parse(raw, token.TypeAccess)
		if err != nil {
			s.writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if _, err := s.sessions.Load(r.Context(), s.db, sessionID); err != nil {
			s.writeError(w, http.StatusUnauthorized, "this sign-in is no longer active")
			return
		}

		ctx := context.WithValue(r.Context(), principalKey, principal)
		ctx = context.WithValue(ctx, sessionIDKey, sessionID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recover turns a panic into a 500 instead of killing the connection, and keeps
// the request going if the handler recovers cleanly.
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("handler panicked", "error", rec, "path", r.URL.Path)
				s.writeError(w, http.StatusInternalServerError, "the server hit an unexpected error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Principal returns the authenticated principal the middleware resolved, or the
// zero value if the handler ran outside it.
func Principal(ctx context.Context) policy.Principal {
	p, _ := ctx.Value(principalKey).(policy.Principal)
	return p
}

// SessionID returns the session the authenticated request belongs to.
func SessionID(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey).(string)
	return id
}

// clientIP resolves the caller's address. It prefers the value a proxy
// middleware (middleware.ClientIPFromXFF) has published, and otherwise parses
// the direct peer from RemoteAddr. When nothing resolves it fails closed to the
// zero address, which relaxes per-address lockout rather than inventing an IP.
func clientIP(r *http.Request) netip.Addr {
	if ip := middleware.GetClientIPAddr(r.Context()); ip.IsValid() {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ip
}

// decodeJSON reads a small JSON body into v, rejecting unknown fields so a
// typo'd request fails loudly instead of being silently half-applied.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// writeJSON encodes v as the response body.
func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError is the one shape every failure response takes.
func (s *Server) writeError(w http.ResponseWriter, code int, message string) {
	s.writeJSON(w, code, map[string]string{"error": message})
}

func notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusMethodNotAllowed)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
}

// secondsUntil is a duration-to-seconds helper for the expires_in fields.
func secondsUntil(t time.Time) int64 {
	return int64(time.Until(t).Seconds())
}

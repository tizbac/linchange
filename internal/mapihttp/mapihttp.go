package mapihttp

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/mapihttp/transport"
)

// Server is the MAPI/HTTP server (MS-OXCMAPIHTTP).
// This is the native protocol used by Outlook for Windows.
//
// MAPI/HTTP is a binary RPC-over-HTTP protocol that carries MAPI
// (Messaging Application Programming Interface) calls over HTTP.
// It consists of two main parts:
// 1. MAPI/HTTP transport - HTTP framing with specific headers (transport package)
// 2. MAPI RPC layer - MAPI method calls with ROP processing (rop package)
// 3. Property marshaling per MS-OXPROPS (props package)
//
// Key references:
// - [MS-OXCMAPIHTTP] - MAPI/HTTP Protocol
// - [MS-OXCRPC] - MAPI RPC Extensions
// - [MS-OXCROPS] - MAPI Remote Operations (ROPs)
// - [MS-OXPROPS] - MAPI Properties
// - gromox reference implementation
type Server struct {
	transport *transport.Transport
	cfg       *Config
	policy    policy.Policy
}

// Config holds MAPI/HTTP server configuration
type Config struct {
	VersionMajor   int
	VersionMinor   int
	MaxPayloadSize int64
	SessionTimeout int // seconds
}

// New creates a new MAPI/HTTP server
func New(cfg *Config, pol policy.Policy) *Server {
	if cfg == nil {
		cfg = &Config{
			VersionMajor:   1,
			VersionMinor:   0,
			MaxPayloadSize: 10 * 1024 * 1024, // 10MB
			SessionTimeout: 30 * 60,          // 30 minutes
		}
	}

	transportCfg := &transport.Config{
		VersionMajor:   cfg.VersionMajor,
		VersionMinor:   cfg.VersionMinor,
		MaxPayloadSize: cfg.MaxPayloadSize,
		SessionTimeout: time.Duration(cfg.SessionTimeout) * time.Second,
	}

	// Note: store parameter is nil for now - will be wired up when store interface is defined
	t := transport.NewTransport(transportCfg, nil, pol)

	return &Server{
		transport: t,
		cfg:       cfg,
		policy:    pol,
	}
}

// Handler returns the HTTP handler for MAPI/HTTP endpoints
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	// Main MAPI/HTTP endpoints
	r.Post("/emsmdb", s.handleRequest)
	r.Post("/emsmdbd", s.handleRequest)
	r.Post("/nspi", s.handleRequest)
	r.Post("/ab", s.handleRequest)

	// Health check
	r.Get("/health", s.handleHealth)

	return r
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.transport.HandleRequest(ctx, w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok","service":"mapihttp"}`))
}

// For testing and debugging
func (s *Server) Transport() *transport.Transport {
	return s.transport
}

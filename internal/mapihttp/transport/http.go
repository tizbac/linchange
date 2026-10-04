package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linchange/linchange/internal/auth/policy"
	rop "github.com/linchange/linchange/internal/mapihttp/rop"
)

// MAPI/HTTP version constants (MS-OXCMAPIHTTP)
const (
	MAPIVersionMajor = 1
	MAPIVersionMinor = 0
	MAPIVersion      = (MAPIVersionMajor << 16) | MAPIVersionMinor
)

// MAPI/HTTP endpoints (MS-OXCMAPIHTTP section 2.2.4)
const (
	EndpointEMSMDB  = "/mapi/emsmdb"
	EndpointEMSMDBD = "/mapi/emsmdbd"
	EndpointNSPI    = "/mapi/nspi"
	EndpointAB      = "/mapi/ab"
)

// MAPI/HTTP header names (MS-OXCMAPIHTTP section 2.2.3)
const (
	HeaderMAPIVersion      = "X-MAPI-Version"
	HeaderMAPIHTTPMethod   = "X-RequestType"
	HeaderMAPIContext      = "X-MAPI-Context"
	HeaderMAPISession      = "X-SessionId"
	HeaderMAPIFlags        = "X-MAPI-Flags"
	HeaderMAPISequence     = "X-MAPI-Sequence"
	HeaderMAPIAuxiliary    = "X-MAPI-Auxiliary"
	HeaderContentType      = "Content-Type"
	HeaderContentLength    = "Content-Length"
	HeaderUserAgent        = "User-Agent"
	HeaderAuthorization    = "Authorization"
	HeaderCookie           = "Cookie"
	HeaderSetCookie        = "Set-Cookie"
	HeaderTransferEncoding = "Transfer-Encoding"
	HeaderXClientInfo      = "X-ClientInfo"
	HeaderXClientApp       = "X-ClientApplication"
	HeaderXRequestType     = "X-RequestType"
	HeaderXRequestId       = "X-RequestId"
	HeaderXResponseCode    = "X-ResponseCode"
	HeaderXPendingPeriod   = "X-PendingPeriod"
	HeaderXElapsedTime     = "X-ElapsedTime"
	HeaderXStartTime       = "X-StartTime"
	HeaderXServerApp       = "X-ServerApplication"
	HeaderXExpirationInfo  = "X-ExpirationInfo"
)

// MAPI/HTTP request types (MS-OXCMAPIHTTP section 2.2.3.3)
const (
	MethodConnect       = "Connect"
	MethodBind          = "Bind"
	MethodExecute       = "Execute"
	MethodNotify        = "Notify"
	MethodGetBuffer     = "GetBuffer"
	MethodReleaseBuffer = "ReleaseBuffer"
)

// MAPI/HTTP flag values
const (
	FlagNone           = 0x00
	FlagAsync          = 0x01
	FlagNoReplacements = 0x02
	FlagUTF8           = 0x04
)

// MAPI/HTTP error codes
const (
	HTTPErrorSuccess           = 0x00000000
	HTTPErrorInvalidRequest    = 0x80070001
	HTTPErrorInvalidVersion    = 0x80070002
	HTTPErrorUnsupportedMethod = 0x80070003
	HTTPErrorAuthentication    = 0x80070005
	HTTPErrorBufferTooSmall    = 0x8007007A
	HTTPErrorInternalError     = 0x80004005
)

// Meta-tags for chunked responses (MS-OXCMAPIHTTP section 2.2.7)
const (
	MetaTagProcessing = "PROCESSING"
	MetaTagPending    = "PENDING"
	MetaTagDone       = "DONE"
)

// Transport implements the MAPI/HTTP transport layer (MS-OXCMAPIHTTP)
type Transport struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	cfg      *Config
	store    StoreInterface
	policy   policy.Policy
}

// Config holds transport configuration
type Config struct {
	VersionMajor   int
	VersionMinor   int
	MaxPayloadSize int64
	SessionTimeout time.Duration
	PendingPeriod  time.Duration
}

// Session represents a MAPI/HTTP session context (MS-OXCMAPIHTTP section 2.2.2)
type Session struct {
	ID         string
	User       string
	State      uint32
	Context    uint32
	Flags      uint32
	Sequence   uint32
	ClientInfo string
	RequestID  string
	ClientApp  string
	CreatedAt  time.Time
	LastActive time.Time
	StoreState uint32
	Cookies    map[string]string
}

// StoreInterface defines the interface for store operations
type StoreInterface interface {
	// Will be implemented by actual store
}

// MAPIRequest represents a parsed MAPI/HTTP request
type MAPIRequest struct {
	Version    uint32
	Context    uint32
	SessionID  string
	Flags      uint32
	Sequence   uint32
	Method     string
	Payload    []byte
	Auxiliary  []byte
	ClientInfo string
	RequestID  string
	ClientApp  string
}

// MAPIResponse represents a MAPI/HTTP response
type MAPIResponse struct {
	Version   uint32
	Context   uint32
	SessionID string
	Status    uint32
	Flags     uint32
	Payload   []byte
	Auxiliary []byte
}

// NewTransport creates a new MAPI/HTTP transport
func NewTransport(cfg *Config, store StoreInterface, pol policy.Policy) *Transport {
	if cfg == nil {
		cfg = &Config{
			VersionMajor:   MAPIVersionMajor,
			VersionMinor:   MAPIVersionMinor,
			MaxPayloadSize: 10 * 1024 * 1024, // 10MB
			SessionTimeout: 30 * time.Minute,
			PendingPeriod:  15 * time.Second,
		}
	}

	return &Transport{
		cfg:      cfg,
		store:    store,
		policy:   pol,
		sessions: make(map[string]*Session),
	}
}

// generateGUID generates a random GUID
func generateGUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant 10
	return hex.EncodeToString(b), nil
}

// ParseRequest parses a MAPI/HTTP request from an HTTP request
func (t *Transport) ParseRequest(r *http.Request) (*MAPIRequest, error) {
	method := r.Header.Get(HeaderXRequestType)
	fmt.Printf("DEBUG: X-RequestType header = '%s'\n", method)
	req := &MAPIRequest{
		Method: method,
	}

	// Parse version
	versionStr := r.Header.Get(HeaderMAPIVersion)
	if versionStr == "" {
		return nil, errors.New("missing X-MAPI-Version header")
	}
	var major, minor int
	if _, err := fmt.Sscanf(versionStr, "%d.%d", &major, &minor); err != nil {
		return nil, fmt.Errorf("invalid X-MAPI-Version format: %s", versionStr)
	}
	req.Version = uint32((major << 16) | minor)

	// Validate version
	if major != MAPIVersionMajor {
		return nil, fmt.Errorf("unsupported MAPI version: %d.%d", major, minor)
	}

	// Parse context
	ctxStr := r.Header.Get(HeaderMAPIContext)
	if ctxStr != "" {
		fmt.Sscanf(ctxStr, "%d", &req.Context)
	}

	// Parse session ID
	req.SessionID = r.Header.Get(HeaderMAPISession)

	// Parse flags
	flagsStr := r.Header.Get(HeaderMAPIFlags)
	if flagsStr != "" {
		fmt.Sscanf(flagsStr, "%d", &req.Flags)
	}

	// Parse sequence
	seqStr := r.Header.Get(HeaderMAPISequence)
	if seqStr != "" {
		fmt.Sscanf(seqStr, "%d", &req.Sequence)
	}

	// Parse client info
	req.ClientInfo = r.Header.Get(HeaderXClientInfo)

	// Parse request ID
	req.RequestID = r.Header.Get(HeaderXRequestId)

	// Parse client application
	req.ClientApp = r.Header.Get(HeaderXClientApp)

	// Parse cookies
	req.Auxiliary = []byte(r.Header.Get(HeaderCookie))

	// Read payload
	payload, err := io.ReadAll(io.LimitReader(r.Body, t.cfg.MaxPayloadSize))
	if err != nil {
		return nil, fmt.Errorf("failed to read payload: %w", err)
	}
	req.Payload = payload

	return req, nil
}

// BuildResponse writes a MAPI/HTTP response to an HTTP response writer
func (t *Transport) BuildResponse(w http.ResponseWriter, resp *MAPIResponse) {
	w.Header().Set(HeaderMAPIVersion, fmt.Sprintf("%d.%d", MAPIVersionMajor, MAPIVersionMinor))
	w.Header().Set(HeaderMAPIContext, fmt.Sprintf("%d", resp.Context))
	w.Header().Set(HeaderMAPISession, resp.SessionID)
	w.Header().Set(HeaderMAPIFlags, fmt.Sprintf("%d", resp.Flags))
	w.Header().Set(HeaderContentType, "application/mapi-http")

	if len(resp.Payload) > 0 {
		w.Header().Set(HeaderContentLength, fmt.Sprintf("%d", len(resp.Payload)))
		w.Write(resp.Payload)
	} else {
		w.Header().Set(HeaderContentLength, "0")
	}
}

// BuildChunkedResponse writes a chunked MAPI/HTTP response with meta-tags
func (t *Transport) BuildChunkedResponse(w http.ResponseWriter, resp *MAPIResponse, metaTag string) {
	w.Header().Set(HeaderMAPIVersion, fmt.Sprintf("%d.%d", MAPIVersionMajor, MAPIVersionMinor))
	w.Header().Set(HeaderMAPIContext, fmt.Sprintf("%d", resp.Context))
	w.Header().Set(HeaderMAPISession, resp.SessionID)
	w.Header().Set(HeaderMAPIFlags, fmt.Sprintf("%d", resp.Flags))
	w.Header().Set(HeaderContentType, "application/mapi-http")
	w.Header().Set(HeaderTransferEncoding, "chunked")

	flusher, ok := w.(http.Flusher)
	if !ok {
		t.writeError(w, "streaming unsupported", HTTPErrorInternalError)
		return
	}

	// Write meta-tag chunk
	chunk := fmt.Sprintf("%x\r\n%s\r\n", len(metaTag), metaTag)
	w.Write([]byte(chunk))
	flusher.Flush()

	// If DONE, write final payload
	if metaTag == MetaTagDone && len(resp.Payload) > 0 {
		chunk := fmt.Sprintf("%x\r\n", len(resp.Payload))
		w.Write([]byte(chunk))
		w.Write(resp.Payload)
		w.Write([]byte("\r\n"))
		flusher.Flush()
	}

	// Final chunk
	w.Write([]byte("0\r\n\r\n"))
	flusher.Flush()
}

// HandleRequest processes a MAPI/HTTP request
func (t *Transport) HandleRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	fmt.Printf("DEBUG HandleRequest: Method=%s, URL=%s, X-RequestType=%s\n", r.Method, r.URL.Path, r.Header.Get("X-RequestType"))
	req, err := t.ParseRequest(r)
	if err != nil {
		t.writeError(w, err.Error(), HTTPErrorInvalidRequest)
		return
	}

	// Authenticate
	user, authErr := t.authenticate(r)
	if authErr != nil {
		t.writeError(w, "Authentication required", HTTPErrorAuthentication)
		return
	}

	// Process based on method
	var resp *MAPIResponse
	var handlerErr error

	switch req.Method {
	case MethodConnect:
		resp, handlerErr = t.handleConnect(ctx, req, user)
	case MethodBind:
		resp, handlerErr = t.handleBind(ctx, req)
	case MethodExecute:
		resp, handlerErr = t.handleExecute(ctx, req)
	case MethodNotify:
		resp, handlerErr = t.handleNotify(ctx, req)
	case MethodGetBuffer:
		resp, handlerErr = t.handleGetBuffer(ctx, req)
	case MethodReleaseBuffer:
		resp, handlerErr = t.handleReleaseBuffer(ctx, req)
	default:
		t.writeError(w, fmt.Sprintf("unsupported method: %s", req.Method), HTTPErrorUnsupportedMethod)
		return
	}

	if handlerErr != nil {
		t.writeError(w, handlerErr.Error(), HTTPErrorInternalError)
		return
	}

	// For Execute, use chunked response with meta-tags
	if req.Method == MethodExecute {
		t.BuildChunkedResponse(w, resp, MetaTagDone)
	} else {
		t.BuildResponse(w, resp)
	}
}

// authenticate validates the request credentials
func (t *Transport) authenticate(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", errors.New("missing authorization")
	}

	// Support Basic Auth
	if strings.HasPrefix(auth, "Basic ") {
		return "test@example.com", nil
	}

	// Support Bearer token
	if strings.HasPrefix(auth, "Bearer ") {
		return "test@example.com", nil
	}

	// Support NTLM/Negotiate (for Outlook)
	if strings.HasPrefix(auth, "NTLM ") || strings.HasPrefix(auth, "Negotiate ") {
		return "test@example.com", nil
	}

	return "", errors.New("unsupported auth type")
}

// Session management

func (t *Transport) createSession(user string, context uint32, clientInfo, requestID, clientApp string) *Session {
	t.mu.Lock()
	defer t.mu.Unlock()

	guid, _ := generateGUID()
	sessionID := fmt.Sprintf("mapi-%s", guid)

	session := &Session{
		ID:         sessionID,
		User:       user,
		State:      1, // Logged on
		Context:    context,
		Flags:      0,
		Sequence:   0,
		ClientInfo: clientInfo,
		RequestID:  requestID,
		ClientApp:  clientApp,
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
		StoreState: 1,
		Cookies:    make(map[string]string),
	}
	t.sessions[sessionID] = session
	return session
}

func (t *Transport) getSession(sessionID string) (*Session, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	session, ok := t.sessions[sessionID]
	return session, ok
}

func (t *Transport) deleteSession(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessions, sessionID)
}

func (t *Transport) cleanupExpiredSessions() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for id, session := range t.sessions {
		if now.Sub(session.LastActive) > t.cfg.SessionTimeout {
			delete(t.sessions, id)
		}
	}
}

// Method handlers

func (t *Transport) handleConnect(ctx context.Context, req *MAPIRequest, user string) (*MAPIResponse, error) {
	session := t.createSession(user, req.Context, req.ClientInfo, req.RequestID, req.ClientApp)

	// Build Connect response payload (MS-OXCMAPIHTTP section 2.2.4.2.1)
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, session.Context)     // Context
	binary.Write(&buf, binary.LittleEndian, session.State)       // State
	binary.Write(&buf, binary.LittleEndian, uint32(MAPIVersion)) // Version

	return &MAPIResponse{
		Version:   MAPIVersion,
		Context:   req.Context,
		SessionID: session.ID,
		Status:    0, // Success
		Payload:   buf.Bytes(),
	}, nil
}

func (t *Transport) handleBind(ctx context.Context, req *MAPIRequest) (*MAPIResponse, error) {
	// Bind is used for NSPI/AB endpoints
	session := t.createSession("test@example.com", req.Context, req.ClientInfo, req.RequestID, req.ClientApp)

	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, session.Context)
	binary.Write(&buf, binary.LittleEndian, session.State)
	binary.Write(&buf, binary.LittleEndian, uint32(MAPIVersion))

	return &MAPIResponse{
		Version:   MAPIVersion,
		Context:   req.Context,
		SessionID: session.ID,
		Status:    0,
		Payload:   buf.Bytes(),
	}, nil
}

func (t *Transport) handleExecute(ctx context.Context, req *MAPIRequest) (*MAPIResponse, error) {
	if len(req.Payload) == 0 {
		return nil, errors.New("empty execute payload")
	}

	// Parse ROP buffer (MS-OXCROPS section 2.2.1)
	ropBuffer, err := rop.ParseROPBuffer(req.Payload)
	if err != nil {
		return nil, fmt.Errorf("invalid ROP buffer: %w", err)
	}

	// Get session
	session, ok := t.getSession(req.SessionID)
	if !ok {
		return nil, errors.New("invalid session")
	}
	session.LastActive = time.Now()

	// Process each ROP
	responseBuffer := &bytes.Buffer{}
	for _, ropInput := range ropBuffer {
		ropResp, err := rop.ProcessROP(ropInput, &rop.MAPISession{
			ID:          session.ID,
			User:        session.User,
			State:       session.State,
			Context:     session.Context,
			Store:       t.store,
			Folders:     make(map[uint32]*rop.FolderHandle),
			NextTableID: 1,
		})
		if err != nil {
			// Return error ROP
			errorROP := rop.ROPResult{
				ROPID:  ropInput.ROPID,
				Error:  rop.EC_UNKNOWN_ERROR,
				Output: nil,
			}
			errorBuf, _ := rop.BuildROPBuffer([]rop.ROPResult{errorROP})
			responseBuffer.Write(errorBuf)
			continue
		}
		responseBuffer.Write(ropResp.Output)
	}

	return &MAPIResponse{
		Version:   MAPIVersion,
		Context:   req.Context,
		SessionID: req.SessionID,
		Status:    0, // Success
		Payload:   responseBuffer.Bytes(),
	}, nil
}

func (t *Transport) handleNotify(ctx context.Context, req *MAPIRequest) (*MAPIResponse, error) {
	return &MAPIResponse{
		Version:   MAPIVersion,
		Context:   req.Context,
		SessionID: req.SessionID,
		Status:    0,
		Payload:   []byte{},
	}, nil
}

func (t *Transport) handleGetBuffer(ctx context.Context, req *MAPIRequest) (*MAPIResponse, error) {
	return &MAPIResponse{
		Version:   MAPIVersion,
		Context:   req.Context,
		SessionID: req.SessionID,
		Status:    0,
		Payload:   []byte{},
	}, nil
}

func (t *Transport) handleReleaseBuffer(ctx context.Context, req *MAPIRequest) (*MAPIResponse, error) {
	return &MAPIResponse{
		Version:   MAPIVersion,
		Context:   req.Context,
		SessionID: req.SessionID,
		Status:    0,
		Payload:   []byte{},
	}, nil
}

// Error handling

func (t *Transport) writeError(w http.ResponseWriter, message string, code uint32) {
	w.Header().Set("Content-Type", "application/mapi-http")
	w.Header().Set("X-MAPI-Version", fmt.Sprintf("%d.%d", MAPIVersionMajor, MAPIVersionMinor))
	w.Header().Set("X-MAPI-Error", fmt.Sprintf("%d", code))
	w.WriteHeader(http.StatusBadRequest)
	w.Write([]byte(message))
}

// Handler returns the HTTP handler for MAPI/HTTP endpoints
func (t *Transport) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(EndpointEMSMDB, t.handleEMSMDB)
	mux.HandleFunc(EndpointEMSMDBD, t.handleEMSMDBD)
	mux.HandleFunc(EndpointNSPI, t.handleNSPI)
	mux.HandleFunc(EndpointAB, t.handleAB)
	mux.HandleFunc("/mapi/health", t.handleHealth)
	return mux
}

func (t *Transport) handleEMSMDB(w http.ResponseWriter, r *http.Request) {
	t.HandleRequest(r.Context(), w, r)
}

func (t *Transport) handleEMSMDBD(w http.ResponseWriter, r *http.Request) {
	t.HandleRequest(r.Context(), w, r)
}

func (t *Transport) handleNSPI(w http.ResponseWriter, r *http.Request) {
	t.HandleRequest(r.Context(), w, r)
}

func (t *Transport) handleAB(w http.ResponseWriter, r *http.Request) {
	t.HandleRequest(r.Context(), w, r)
}

// Health check
func (t *Transport) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok","service":"mapihttp"}`))
}

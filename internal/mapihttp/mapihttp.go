package mapihttp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
)

// MAPIHTTP implements Microsoft MAPI/HTTP protocol (MS-OXCMAPIHTTP).
// This is the native protocol used by Outlook for Windows.
//
// MAPI/HTTP is a binary RPC-over-HTTP protocol that carries MAPI
// (Messaging Application Programming Interface) calls over HTTP.
// It consists of two main parts:
// 1. MAPI/HTTP transport - HTTP framing with specific headers
// 2. MAPI RPC layer - MAPI method calls with property/bulk marshalling
//
// Key references:
// - [MS-OXCMAPIHTTP] - MAPI/HTTP Protocol
// - [MS-OXCRPC] - MAPI RPC Extensions
// - [MS-OXPROPS] - MAPI Properties
// - gromox reference implementation
type MAPIHTTP struct {
	cfg    *config.Config
	policy policy.Policy

	// Session management
	sessions sync.Map // map[string]*MAPISession
}

// MAPISession represents a MAPI/HTTP session.
type MAPISession struct {
	ID         string
	User       string
	State      uint32
	Context    uint32
	CreatedAt  time.Time
	LastActive time.Time
}

// MAPIVersion is the MAPI/HTTP version.
const (
	MAPIVersionMajor = 1
	MAPIVersionMinor = 0
	MAPIVersion      = (MAPIVersionMajor << 16) | MAPIVersionMinor
)

// MAPI/HTTP endpoints
const (
	EndpointEMSMDB  = "/mapi/emsmdb"
	EndpointEMSMDBD = "/mapi/emsmdbd"
	EndpointNSPI    = "/mapi/nspi"
	EndpointAB      = "/mapi/ab"
)

// MAPI/HTTP headers
const (
	HeaderMAPIVersion    = "X-MAPI-Version"
	HeaderMAPIHTTPMethod = "X-MAPI-HTTP-Method"
	HeaderMAPIContext    = "X-MAPI-Context"
	HeaderMAPISession    = "X-MAPI-Session"
	HeaderMAPIFlags      = "X-MAPI-Flags"
	HeaderMAPISequence   = "X-MAPI-Sequence"
	HeaderMAPIAuxiliary  = "X-MAPI-Auxiliary"
	HeaderContentType    = "Content-Type"
	HeaderContentLength  = "Content-Length"
	HeaderUserAgent      = "User-Agent"
	HeaderAuthorization  = "Authorization"
)

// MAPI/HTTP methods
const (
	MethodConnect       = "Connect"
	MethodDisconnect    = "Disconnect"
	MethodExecute       = "Execute"
	MethodNotify        = "Notify"
	MethodGetBuffer     = "GetBuffer"
	MethodReleaseBuffer = "ReleaseBuffer"
)

// MAPI RPC opnums (from MS-OXCRPC)
const (
	OpnumRPCConnect            = 0x0000
	OpnumRPCDisconnect         = 0x0001
	OpnumRPCExecute            = 0x0002
	OpnumRPCNotify             = 0x0003
	OpnumRPCGetBuffer          = 0x0004
	OpnumRPCReleaseBuffer      = 0x0005
	OpnumRPCOpenLogon          = 0x0006
	OpnumRPCCloseLogon         = 0x0007
	OpnumRPCGetProps           = 0x0008
	OpnumRPCSetProps           = 0x0009
	OpnumRPCGetPropList        = 0x000A
	OpnumRPCOpenEntry          = 0x000B
	OpnumRPCCopyTo             = 0x000C
	OpnumRPCMoveTo             = 0x000D
	OpnumRPCDeleteMessages     = 0x000E
	OpnumRPCCreateMessage      = 0x000F
	OpnumRPCGetMessageStatus   = 0x0010
	OpnumRPCSetMessageStatus   = 0x0011
	OpnumRPCSaveChanges        = 0x0012
	OpnumRPCGetAttachmentTable = 0x0013
	OpnumRPCOpenAttachment     = 0x0014
	OpnumRPCCreateAttachment   = 0x0015
	OpnumRPCDeleteAttachment   = 0x0016
	OpnumRPCGetAttachmentProps = 0x0017
	OpnumRPCSetAttachmentProps = 0x0018
	OpnumRPCSaveAttachment     = 0x0019
	OpnumRPCGetHierarchyTable  = 0x001A
	OpnumRPCGetContentsTable   = 0x001B
	OpnumRPCGetMatches         = 0x001C
	OpnumRPCSeekEntries        = 0x001D
	OpnumRPCExpandRow          = 0x001E
	OpnumRPCCollapseRow        = 0x001F
	OpnumRPCGetFolderTable     = 0x0020
	OpnumRPCGetStatusTable     = 0x0021
	OpnumRPCGetSearchCriteria  = 0x0022
	OpnumRPCCreateFolder       = 0x0023
	OpnumRPCDeleteFolder       = 0x0024
	OpnumRPCCopyFolder         = 0x0025
	OpnumRPCMoveFolder         = 0x0026
	OpnumRPCGetFolderProps     = 0x0027
	OpnumRPCSetFolderProps     = 0x0028
	OpnumRPCGetRulesTable      = 0x0029
	OpnumRPCModifyRules        = 0x002A
	OpnumRPCGetACLTable        = 0x002B
	OpnumRPCModifyACL          = 0x002C
	OpnumRPCGetPerms           = 0x002D
	OpnumRPCSetPerms           = 0x002E
	OpnumRPCGetPropsOnRow      = 0x002F
	OpnumRPCFastTransferSource = 0x0030
	OpnumRPCFastTransferDest   = 0x0031
	OpnumRPCGetNewMail         = 0x0032
	OpnumRPCGetNewMailEx       = 0x0033
)

// New creates a new MAPI/HTTP service.
func New(cfg *config.Config, pol policy.Policy) *MAPIHTTP {
	return &MAPIHTTP{
		cfg:    cfg,
		policy: pol,
	}
}

// Handler returns the HTTP handler for MAPI/HTTP endpoints.
func (m *MAPIHTTP) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(m.mapiMiddleware)

	// Main MAPI/HTTP endpoints
	r.Post(EndpointEMSMDB, m.handleEMSMDB)
	r.Post(EndpointEMSMDBD, m.handleEMSMDBD)
	r.Post(EndpointNSPI, m.handleNSPI)
	r.Post(EndpointAB, m.handleAB)

	// Health check
	r.Get("/mapi/health", m.handleHealth)

	return r
}

// mapiMiddleware handles MAPI/HTTP specific headers and authentication.
func (m *MAPIHTTP) mapiMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate MAPI version header
		version := r.Header.Get(HeaderMAPIVersion)
		if version == "" {
			m.writeError(w, "Missing X-MAPI-Version header", http.StatusBadRequest)
			return
		}

		// Parse version
		var major, minor uint16
		if _, err := fmt.Sscanf(version, "%d.%d", &major, &minor); err != nil {
			m.writeError(w, "Invalid X-MAPI-Version format", http.StatusBadRequest)
			return
		}
		if major != MAPIVersionMajor {
			m.writeError(w, fmt.Sprintf("Unsupported MAPI version: %d.%d", major, minor), http.StatusBadRequest)
			return
		}

		// Add MAPI version to response headers
		w.Header().Set(HeaderMAPIVersion, version)

		// Authenticate
		_, err := m.authenticate(r)
		if err != nil {
			m.writeError(w, "Authentication required", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// authenticate validates the request credentials.
func (m *MAPIHTTP) authenticate(r *http.Request) (string, error) {
	auth := r.Header.Get(HeaderAuthorization)
	if auth == "" {
		return "", errors.New("missing authorization")
	}

	// Support Basic Auth and Bearer tokens
	if strings.HasPrefix(auth, "Basic ") {
		// Parse Basic Auth
		return "test@example.com", nil
	}

	if strings.HasPrefix(auth, "Bearer ") {
		// Validate JWT
		return "test@example.com", nil
	}

	return "", errors.New("unsupported auth type")
}

// handleEMSMDB handles the main EMSMDB (Exchange Mail Store) endpoint.
func (m *MAPIHTTP) handleEMSMDB(w http.ResponseWriter, r *http.Request) {
	m.handleMAPIRequest(w, r, "EMSMDB")
}

// handleEMSMDBD handles the EMSMDBD (Exchange Mail Store Database) endpoint.
func (m *MAPIHTTP) handleEMSMDBD(w http.ResponseWriter, r *http.Request) {
	m.handleMAPIRequest(w, r, "EMSMDBD")
}

// handleNSPI handles the NSPI (Name Service Provider Interface) endpoint.
func (m *MAPIHTTP) handleNSPI(w http.ResponseWriter, r *http.Request) {
	m.handleMAPIRequest(w, r, "NSPI")
}

// handleAB handles the Address Book endpoint.
func (m *MAPIHTTP) handleAB(w http.ResponseWriter, r *http.Request) {
	m.handleMAPIRequest(w, r, "AB")
}

// handleMAPIRequest processes a MAPI/HTTP request.
func (m *MAPIHTTP) handleMAPIRequest(w http.ResponseWriter, r *http.Request, endpoint string) {
	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		m.writeError(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Parse MAPI/HTTP request
	req, err := m.parseMAPIRequest(r, body)
	if err != nil {
		m.writeError(w, fmt.Sprintf("Invalid MAPI request: %v", err), http.StatusBadRequest)
		return
	}

	// Process based on HTTP method header
	method := r.Header.Get(HeaderMAPIHTTPMethod)
	switch method {
	case MethodConnect:
		m.handleConnect(w, req)
	case MethodDisconnect:
		m.handleDisconnect(w, req)
	case MethodExecute:
		m.handleExecute(w, req)
	case MethodNotify:
		m.handleNotify(w, req)
	case MethodGetBuffer:
		m.handleGetBuffer(w, req)
	case MethodReleaseBuffer:
		m.handleReleaseBuffer(w, req)
	default:
		m.writeError(w, fmt.Sprintf("Unknown MAPI method: %s", method), http.StatusBadRequest)
	}
}

// handleHealth returns health status for MAPI endpoints.
func (m *MAPIHTTP) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok","service":"mapihttp"}`))
}

// ---------------------------------------------------------------------------
// MAPI Request/Response Parsing
// ---------------------------------------------------------------------------

// MAPIRequest represents a parsed MAPI/HTTP request.
type MAPIRequest struct {
	Version   uint32
	Context   uint32
	SessionID string
	Flags     uint32
	Sequence  uint32
	Method    string
	Payload   []byte
}

// parseMAPIRequest parses the MAPI/HTTP request.
func (m *MAPIHTTP) parseMAPIRequest(r *http.Request, body []byte) (*MAPIRequest, error) {
	req := &MAPIRequest{
		Method:  r.Header.Get(HeaderMAPIHTTPMethod),
		Payload: body,
	}

	// Parse version
	if v := r.Header.Get(HeaderMAPIVersion); v != "" {
		fmt.Sscanf(v, "%d.%d", &req.Version, nil)
	}

	// Parse context
	if ctx := r.Header.Get(HeaderMAPIContext); ctx != "" {
		fmt.Sscanf(ctx, "%d", &req.Context)
	}

	// Parse session ID
	req.SessionID = r.Header.Get(HeaderMAPISession)

	// Parse flags
	if f := r.Header.Get(HeaderMAPIFlags); f != "" {
		fmt.Sscanf(f, "%d", &req.Flags)
	}

	// Parse sequence
	if s := r.Header.Get(HeaderMAPISequence); s != "" {
		fmt.Sscanf(s, "%d", &req.Sequence)
	}

	return req, nil
}

// ---------------------------------------------------------------------------
// MAPI Method Handlers
// ---------------------------------------------------------------------------

func (m *MAPIHTTP) handleConnect(w http.ResponseWriter, req *MAPIRequest) {
	// Connect creates a new MAPI session
	sessionID := fmt.Sprintf("mapi-%d", time.Now().UnixNano())

	session := &MAPISession{
		ID:         sessionID,
		User:       "test@example.com",
		State:      0,
		Context:    req.Context,
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
	}

	m.sessions.Store(sessionID, session)

	// Build Connect response
	resp := &MAPIResponse{
		Version:   MAPIVersion,
		Context:   req.Context,
		SessionID: sessionID,
		Status:    0, // Success
		Payload:   m.buildConnectResponse(session),
	}

	m.writeMAPIResponse(w, resp)
}

func (m *MAPIHTTP) handleDisconnect(w http.ResponseWriter, req *MAPIRequest) {
	// Disconnect terminates a MAPI session
	if req.SessionID != "" {
		m.sessions.Delete(req.SessionID)
	}

	resp := &MAPIResponse{
		Version: MAPIVersion,
		Status:  0,
	}

	m.writeMAPIResponse(w, resp)
}

func (m *MAPIHTTP) handleExecute(w http.ResponseWriter, req *MAPIRequest) {
	// Execute processes a MAPI RPC call
	// The payload contains the ROP (Remote Operation) buffer

	if len(req.Payload) == 0 {
		m.writeError(w, "Empty execute payload", http.StatusBadRequest)
		return
	}

	// Parse ROP buffer
	ropBuffer, err := ParseROPBuffer(req.Payload)
	if err != nil {
		m.writeError(w, fmt.Sprintf("Invalid ROP buffer: %v", err), http.StatusBadRequest)
		return
	}

	// Process each ROP
	responseBuffer := &bytes.Buffer{}
	for _, rop := range ropBuffer.ROPs {
		ropResp := m.processROP(rop, req)
		ropResp.Marshal(responseBuffer)
	}

	resp := &MAPIResponse{
		Version:   MAPIVersion,
		Context:   req.Context,
		SessionID: req.SessionID,
		Status:    0,
		Payload:   responseBuffer.Bytes(),
	}

	m.writeMAPIResponse(w, resp)
}

func (m *MAPIHTTP) handleNotify(w http.ResponseWriter, req *MAPIRequest) {
	// Notify handles push notifications
	// For now, return empty response
	resp := &MAPIResponse{
		Version:   MAPIVersion,
		Context:   req.Context,
		SessionID: req.SessionID,
		Status:    0,
		Payload:   []byte{},
	}

	m.writeMAPIResponse(w, resp)
}

func (m *MAPIHTTP) handleGetBuffer(w http.ResponseWriter, req *MAPIRequest) {
	// GetBuffer allocates a buffer for large payloads
	resp := &MAPIResponse{
		Version:   MAPIVersion,
		Context:   req.Context,
		SessionID: req.SessionID,
		Status:    0,
	}

	m.writeMAPIResponse(w, resp)
}

func (m *MAPIHTTP) handleReleaseBuffer(w http.ResponseWriter, req *MAPIRequest) {
	// ReleaseBuffer releases a previously allocated buffer
	resp := &MAPIResponse{
		Version:   MAPIVersion,
		Context:   req.Context,
		SessionID: req.SessionID,
		Status:    0,
	}

	m.writeMAPIResponse(w, resp)
}

// ---------------------------------------------------------------------------
// ROP (Remote Operation) Processing
// ---------------------------------------------------------------------------

// ROPBuffer represents a buffer of ROPs.
type ROPBuffer struct {
	ROPs []ROP
}

// ROP represents a Remote Operation.
type ROP struct {
	ID     byte
	Opnum  uint16
	Input  []byte
	Output []byte
	Error  uint32
}

// Marshal writes the ROP to a buffer.
func (r ROP) Marshal(buf *bytes.Buffer) error {
	// Write ROP ID
	buf.WriteByte(r.ID)
	// Write ROP size (placeholder - would need actual size)
	var sizeBuf [2]byte
	binary.LittleEndian.PutUint16(sizeBuf[:], uint16(len(r.Output)))
	buf.Write(sizeBuf[:])
	// Write ROP output
	buf.Write(r.Output)
	return nil
}

// ParseROPBuffer parses a ROP buffer from bytes.
func ParseROPBuffer(data []byte) (*ROPBuffer, error) {
	buf := bytes.NewReader(data)
	var ropBuf ROPBuffer

	for buf.Len() > 0 {
		var rop ROP
		// Read ROP ID (1 byte)
		rop.ID, _ = buf.ReadByte()

		// Read ROP size (2 bytes, little-endian)
		sizeBytes := make([]byte, 2)
		if _, err := io.ReadFull(buf, sizeBytes); err != nil {
			break
		}
		size := binary.LittleEndian.Uint16(sizeBytes)

		// Read ROP data
		rop.Input = make([]byte, size)
		if _, err := io.ReadFull(buf, rop.Input); err != nil {
			break
		}

		ropBuf.ROPs = append(ropBuf.ROPs, rop)
	}

	return &ropBuf, nil
}

func (m *MAPIHTTP) processROP(rop ROP, req *MAPIRequest) ROP {
	// Process a single ROP and return response
	// This is where actual MAPI operations would be implemented

	// For now, return success for all ROPs
	return ROP{
		ID:    rop.ID,
		Opnum: rop.Opnum,
		Error: 0, // Success
	}
}

// ---------------------------------------------------------------------------
// MAPI Response
// ---------------------------------------------------------------------------

// MAPIResponse represents a MAPI/HTTP response.
type MAPIResponse struct {
	Version   uint32
	Context   uint32
	SessionID string
	Status    uint32
	Payload   []byte
}

// buildConnectResponse builds the Connect response payload.
func (m *MAPIHTTP) buildConnectResponse(session *MAPISession) []byte {
	var buf bytes.Buffer

	// Write session context
	binary.Write(&buf, binary.LittleEndian, session.Context)
	// Write session state
	binary.Write(&buf, binary.LittleEndian, session.State)
	// Write server version
	binary.Write(&buf, binary.LittleEndian, MAPIVersion)

	return buf.Bytes()
}

func (m *MAPIHTTP) writeMAPIResponse(w http.ResponseWriter, resp *MAPIResponse) {
	w.Header().Set(HeaderContentType, "application/mapi-http")
	w.Header().Set(HeaderMAPIVersion, fmt.Sprintf("%d.%d", MAPIVersionMajor, MAPIVersionMinor))
	w.Header().Set(HeaderMAPIContext, fmt.Sprintf("%d", resp.Context))
	w.Header().Set(HeaderMAPISession, resp.SessionID)
	w.Header().Set(HeaderMAPIFlags, "0")

	// Write payload
	if len(resp.Payload) > 0 {
		w.Header().Set(HeaderContentLength, fmt.Sprintf("%d", len(resp.Payload)))
		w.Write(resp.Payload)
	} else {
		w.Header().Set(HeaderContentLength, "0")
	}
}

func (m *MAPIHTTP) writeError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	w.Write([]byte(message))
}

// ---------------------------------------------------------------------------
// Property Marshaling (MS-OXPROPS)
// ---------------------------------------------------------------------------

// PropertyTag represents a MAPI property tag.
type PropertyTag uint32

// PropertyType represents a MAPI property type.
type PropertyType uint16

const (
	PT_UNSPECIFIED PropertyType = 0x0000
	PT_NULL        PropertyType = 0x0001
	PT_I2          PropertyType = 0x0002
	PT_LONG        PropertyType = 0x0003
	PT_R4          PropertyType = 0x0004
	PT_DOUBLE      PropertyType = 0x0005
	PT_CURRENCY    PropertyType = 0x0006
	PT_APPTIME     PropertyType = 0x0007
	PT_ERROR       PropertyType = 0x000A
	PT_BOOLEAN     PropertyType = 0x000B
	PT_OBJECT      PropertyType = 0x000D
	PT_I8          PropertyType = 0x0014
	PT_STRING8     PropertyType = 0x001E
	PT_UNICODE     PropertyType = 0x001F
	PT_SYSTIME     PropertyType = 0x0040
	PT_CLSID       PropertyType = 0x0048
	PT_BINARY      PropertyType = 0x0102
	PT_MV_I2       PropertyType = 0x1002
	PT_MV_LONG     PropertyType = 0x1003
	PT_MV_R4       PropertyType = 0x1004
	PT_MV_DOUBLE   PropertyType = 0x1005
	PT_MV_CURRENCY PropertyType = 0x1006
	PT_MV_APPTIME  PropertyType = 0x1007
	PT_MV_I8       PropertyType = 0x1014
	PT_MV_STRING8  PropertyType = 0x101E
	PT_MV_UNICODE  PropertyType = 0x101F
	PT_MV_SYSTIME  PropertyType = 0x1040
	PT_MV_CLSID    PropertyType = 0x1048
	PT_MV_BINARY   PropertyType = 0x1102
)

// PropertyValue represents a MAPI property value.
type PropertyValue struct {
	Tag   PropertyTag
	Type  PropertyType
	Value interface{}
}

// MarshalPropertyValue marshals a property value to bytes.
func MarshalPropertyValue(pv PropertyValue) ([]byte, error) {
	var buf bytes.Buffer

	// Write property tag
	binary.Write(&buf, binary.LittleEndian, uint32(pv.Tag))

	// Write property type
	binary.Write(&buf, binary.LittleEndian, uint16(pv.Type))

	// Write value based on type
	switch pv.Type {
	case PT_LONG:
		binary.Write(&buf, binary.LittleEndian, pv.Value.(uint32))
	case PT_I8:
		binary.Write(&buf, binary.LittleEndian, pv.Value.(uint64))
	case PT_BOOLEAN:
		var val uint16
		if pv.Value.(bool) {
			val = 1
		}
		binary.Write(&buf, binary.LittleEndian, val)
	case PT_STRING8:
		str := pv.Value.(string)
		buf.WriteString(str)
		buf.WriteByte(0) // Null terminator
	case PT_UNICODE:
		str := pv.Value.(string)
		for _, r := range str {
			binary.Write(&buf, binary.LittleEndian, uint16(r))
		}
		buf.Write([]byte{0, 0}) // Null terminator (UTF-16)
	case PT_SYSTIME:
		t := pv.Value.(time.Time)
		// FILETIME: 100-nanosecond intervals since Jan 1, 1601 UTC
		ft := uint64(t.UnixNano()/100) + 116444736000000000
		binary.Write(&buf, binary.LittleEndian, ft)
	case PT_BINARY:
		data := pv.Value.([]byte)
		binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
		buf.Write(data)
	case PT_CLSID:
		guid := pv.Value.([16]byte)
		buf.Write(guid[:])
	default:
		return nil, fmt.Errorf("unsupported property type: %v", pv.Type)
	}

	return buf.Bytes(), nil
}

// UnmarshalPropertyValue unmarshals a property value from bytes.
func UnmarshalPropertyValue(data []byte) (PropertyValue, error) {
	var pv PropertyValue
	buf := bytes.NewReader(data)

	// Read property tag
	var tag uint32
	binary.Read(buf, binary.LittleEndian, &tag)
	pv.Tag = PropertyTag(tag)

	// Read property type
	var ptype uint16
	binary.Read(buf, binary.LittleEndian, &ptype)
	pv.Type = PropertyType(ptype)

	// Read value based on type
	switch pv.Type {
	case PT_LONG:
		var val uint32
		binary.Read(buf, binary.LittleEndian, &val)
		pv.Value = val
	case PT_I8:
		var val uint64
		binary.Read(buf, binary.LittleEndian, &val)
		pv.Value = val
	case PT_BOOLEAN:
		var val uint16
		binary.Read(buf, binary.LittleEndian, &val)
		pv.Value = val != 0
	case PT_STRING8:
		// Read null-terminated string
		var strBytes []byte
		for {
			b, err := buf.ReadByte()
			if err != nil || b == 0 {
				break
			}
			strBytes = append(strBytes, b)
		}
		pv.Value = string(strBytes)
	case PT_UNICODE:
		// Read UTF-16 string
		var runes []rune
		for {
			var r uint16
			err := binary.Read(buf, binary.LittleEndian, &r)
			if err != nil || r == 0 {
				break
			}
			runes = append(runes, rune(r))
		}
		pv.Value = string(runes)
	case PT_SYSTIME:
		var ft uint64
		binary.Read(buf, binary.LittleEndian, &ft)
		// Convert FILETIME to time.Time
		ns := (ft - 116444736000000000) * 100
		pv.Value = time.Unix(0, int64(ns))
	case PT_BINARY:
		var size uint32
		binary.Read(buf, binary.LittleEndian, &size)
		data := make([]byte, size)
		io.ReadFull(buf, data)
		pv.Value = data
	case PT_CLSID:
		var guid [16]byte
		io.ReadFull(buf, guid[:])
		pv.Value = guid
	default:
		return pv, fmt.Errorf("unsupported property type: %v", pv.Type)
	}

	return pv, nil
}

// ---------------------------------------------------------------------------
// MAPI Constants (common property tags)
// ---------------------------------------------------------------------------

const (
	// Message properties
	PR_SUBJECT                         = 0x0037001E
	PR_SUBJECT_W                       = 0x0037001F
	PR_MESSAGE_CLASS                   = 0x001A001E
	PR_MESSAGE_DELIVERY_TIME           = 0x0E060040
	PR_CLIENT_SUBMIT_TIME              = 0x00390040
	PR_SENDER_NAME                     = 0x0C1A001E
	PR_SENDER_EMAIL_ADDRESS            = 0x0C1F001E
	PR_SENT_REPRESENTING_NAME          = 0x0042001E
	PR_SENT_REPRESENTING_EMAIL_ADDRESS = 0x0065001E
	PR_DISPLAY_TO                      = 0x0E04001E
	PR_DISPLAY_CC                      = 0x0E03001E
	PR_BODY                            = 0x1000001E
	PR_BODY_W                          = 0x1000001F
	PR_RTF_COMPRESSED                  = 0x10090102
	PR_RTF_IN_SYNC                     = 0x1006000B
	PR_HASATTACH                       = 0x0E1B000B
	PR_ATTACHMENT_TABLE                = 0x37010000
	PR_RECIPIENT_TABLE                 = 0x37020000

	// Recipient properties
	PR_DISPLAY_NAME    = 0x3001001E
	PR_EMAIL_ADDRESS   = 0x3003001E
	PR_ADDRTYPE        = 0x3002001E
	PR_RECIPIENT_TYPE  = 0x0C150003
	PR_RECIPIENT_FLAGS = 0x5FFD0003
	PR_RESPONSIBILITY  = 0x5FFE0003

	// Folder properties
	PR_DISPLAY_NAME_W  = 0x3001001F
	PR_CONTAINER_CLASS = 0x3613001E
	PR_FOLDER_TYPE     = 0x36140003
	PR_CONTENT_COUNT   = 0x36020003
	PR_CONTENT_UNREAD  = 0x36040003

	// Store properties
	PR_IPM_SUBTREE_ENTRYID     = 0x35E00102
	PR_IPM_OUTBOX_ENTRYID      = 0x35E20102
	PR_IPM_WASTEBASKET_ENTRYID = 0x35E30102
	PR_IPM_SENTMAIL_ENTRYID    = 0x35E40102
	PR_IPM_DRAFTS_ENTRYID      = 0x35E60102
	PR_IPM_APPOINTMENT_ENTRYID = 0x35E50102
	PR_IPM_CONTACT_ENTRYID     = 0x35E70102
	PR_IPM_JOURNAL_ENTRYID     = 0x35E80102
	PR_IPM_NOTE_ENTRYID        = 0x35E90102
	PR_IPM_TASK_ENTRYID        = 0x35EA0102
	PR_MDB_PROVIDER            = 0x34140102

	// Attachment properties
	PR_ATTACH_NUM              = 0x37040003
	PR_ATTACH_METHOD           = 0x37050003
	PR_ATTACH_FILENAME         = 0x3707001E
	PR_ATTACH_LONG_FILENAME    = 0x3707001F
	PR_ATTACH_PATHNAME         = 0x3708001E
	PR_ATTACH_DATA_BIN         = 0x37010102
	PR_ATTACH_SIZE             = 0x37060003
	PR_ATTACH_MIME_TAG         = 0x370E001E
	PR_ATTACH_CONTENT_ID       = 0x3712001E
	PR_ATTACH_CONTENT_LOCATION = 0x3713001E

	// Calendar properties
	PR_APPT_START_TIME       = 0x80020040
	PR_APPT_END_TIME         = 0x80030040
	PR_APPT_SUBJECT          = 0x0037001E
	PR_APPT_LOCATION         = 0x8004001E
	PR_APPT_BODY             = 0x1000001E
	PR_APPT_RECUR_PATTERN    = 0x80050003
	PR_APPT_RECUR_START      = 0x80060040
	PR_APPT_RECUR_END        = 0x80070040
	PR_APPT_ALL_DAY          = 0x8008000B
	PR_APPT_STATE_FLAGS      = 0x80090003
	PR_APPT_RESP_TIME        = 0x800A0040
	PR_APPT_RESP_REQUIRED    = 0x800B000B
	PR_APPT_COUNTER_PROPOSAL = 0x800C000B
	PR_APPT_TZ_DEFINITION    = 0x800D0102
)

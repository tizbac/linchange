package eas

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
)

// EAS implements Microsoft Exchange ActiveSync protocol (MS-ASCMD).
// It uses WBXML (Wireless Binary XML) for efficient binary encoding.
//
// Key commands implemented:
// - Provision - device security policy
// - FolderSync - folder hierarchy synchronization
// - Sync - folder/item synchronization
// - Ping - push notification (long-poll)
// - SendMail - send message
// - MoveItems - move messages between folders
// - GetItemEstimate - get item counts
// - FolderCreate/Delete/Update - folder management
// - MeetingResponse - calendar meeting responses
// - Search - GAL search
type EAS struct {
	cfg    *config.Config
	policy policy.Policy
}

// New creates a new ActiveSync service.
func New(cfg *config.Config, pol policy.Policy) *EAS {
	return &EAS{
		cfg:    cfg,
		policy: pol,
	}
}

// Handler returns the HTTP handler for ActiveSync endpoints.
func (e *EAS) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(e.wbxmlMiddleware)

	// Main ActiveSync endpoint
	r.Post("/eas", e.handleEAS)
	r.Get("/eas", e.handleEAS) // For OPTIONS/provisioning discovery

	return r
}

// wbxmlMiddleware handles WBXML content type and decoding.
func (e *EAS) wbxmlMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		if strings.Contains(ct, "application/vnd.ms-sync.wbxml") ||
			strings.Contains(ct, "application/vnd.ms-sync") {
			// WBXML request - decode
			next.ServeHTTP(w, r)
			return
		}
		// Also accept XML for debugging
		if strings.Contains(ct, "xml") {
			next.ServeHTTP(w, r)
			return
		}
		// Default to WBXML for ActiveSync
		next.ServeHTTP(w, r)
	})
}

// handleEAS processes an ActiveSync command.
func (e *EAS) handleEAS(w http.ResponseWriter, r *http.Request) {
	// Parse MS-ASProtocolVersion header
	version := r.Header.Get("MS-ASProtocolVersion")
	if version == "" {
		version = "16.0" // Default to Exchange 2016
	}

	// Parse MS-ASProtocolCommands header to know the command
	command := r.Header.Get("MS-ASProtocolCommands")
	if command == "" {
		// Try to infer from body
		body, _ := io.ReadAll(r.Body)
		if len(body) > 0 {
			command = e.inferCommand(body)
		}
	}

	if command == "" {
		e.writeError(w, "Missing command", http.StatusBadRequest)
		return
	}

	// Authenticate the request
	// ActiveSync uses Basic Auth or OAuth in Authorization header
	// For now, we'll extract from the request
	user, err := e.authenticate(r)
	if err != nil {
		e.writeError(w, "Authentication required", http.StatusUnauthorized)
		return
	}

	// Process command
	var resp []byte
	var err2 error

	switch strings.ToUpper(command) {
	case "PROVISION":
		resp, err2 = e.handleProvision(r, user, version)
	case "FOLDERSYNC":
		resp, err2 = e.handleFolderSync(r, user, version)
	case "SYNC":
		resp, err2 = e.handleSync(r, user, version)
	case "PING":
		resp, err2 = e.handlePing(r, user, version)
	case "SENDMAIL":
		resp, err2 = e.handleSendMail(r, user, version)
	case "MOVEITEMS":
		resp, err2 = e.handleMoveItems(r, user, version)
	case "GETITEM ESTIMATE":
		resp, err2 = e.handleGetItemEstimate(r, user, version)
	case "FOLDERCREATE":
		resp, err2 = e.handleFolderCreate(r, user, version)
	case "FOLDERDELETE":
		resp, err2 = e.handleFolderDelete(r, user, version)
	case "FOLDERUPDATE":
		resp, err2 = e.handleFolderUpdate(r, user, version)
	case "MEETINGRESPONSE":
		resp, err2 = e.handleMeetingResponse(r, user, version)
	case "SEARCH":
		resp, err2 = e.handleSearch(r, user, version)
	case "SETTINGS":
		resp, err2 = e.handleSettings(r, user, version)
	case "ITEMOPERATIONS":
		resp, err2 = e.handleItemOperations(r, user, version)
	default:
		e.writeError(w, fmt.Sprintf("Unsupported command: %s", command), http.StatusNotImplemented)
		return
	}

	if err2 != nil {
		e.writeError(w, err2.Error(), http.StatusInternalServerError)
		return
	}

	// Write WBXML response
	w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
	w.Header().Set("MS-Server-ActiveSync", version)
	w.Write(resp)
}

// inferCommand tries to determine the command from the WBXML body.
func (e *EAS) inferCommand(body []byte) string {
	// For now, return empty - would need WBXML decoder
	return ""
}

// authenticate validates the request credentials.
// ActiveSync uses Basic Auth in Authorization header.
func (e *EAS) authenticate(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", fmt.Errorf("missing authorization")
	}

	// Parse Basic Auth
	if strings.HasPrefix(auth, "Basic ") {
		// Decode base64
		// For now, return a test user
		return "test@example.com", nil
	}

	// OAuth Bearer token
	if strings.HasPrefix(auth, "Bearer ") {
		// Validate JWT
		return "test@example.com", nil
	}

	return "", fmt.Errorf("unsupported auth type")
}

// ---------------------------------------------------------------------------
// Command Handlers (stubs - need full implementation)
// ---------------------------------------------------------------------------

func (e *EAS) handleProvision(r *http.Request, user, version string) ([]byte, error) {
	// Provision command - returns device security policies
	// Response includes: DevicePasswordEnabled, MinDevicePasswordLength, etc.
	return e.buildResponse("Provision", map[string]string{
		"Status": "1", // Success
		"Policies": `<Policies>
			<DevicePasswordEnabled>1</DevicePasswordEnabled>
			<MinDevicePasswordLength>4</MinDevicePasswordLength>
			<MaxInactivityTimeDeviceLock>900</MaxInactivityTimeDeviceLock>
			<DevicePasswordExpiration>0</DevicePasswordExpiration>
			<DevicePasswordHistory>0</DevicePasswordHistory>
			<AllowSimpleDevicePassword>1</AllowSimpleDevicePassword>
			<RequireDeviceEncryption>0</RequireDeviceEncryption>
			<AllowNonProvisionableDevices>1</AllowNonProvisionableDevices>
			<AlphanumericDevicePasswordRequired>0</AlphanumericDevicePasswordRequired>
		</Policies>`,
	})
}

func (e *EAS) handleFolderSync(r *http.Request, user, version string) ([]byte, error) {
	// FolderSync - returns folder hierarchy
	return e.buildResponse("FolderSync", map[string]string{
		"Status": "1",
		"Changes": `<Changes>
			<Add>
				<ServerId>1</ServerId>
				<ParentId>0</ParentId>
				<DisplayName>Inbox</DisplayName>
				<Type>2</Type>
			</Add>
			<Add>
				<ServerId>2</ServerId>
				<ParentId>0</ParentId>
				<DisplayName>Sent Items</DisplayName>
				<Type>3</Type>
			</Add>
			<Add>
				<ServerId>3</ServerId>
				<ParentId>0</ParentId>
				<DisplayName>Deleted Items</DisplayName>
				<Type>4</Type>
			</Add>
			<Add>
				<ServerId>4</ServerId>
				<ParentId>0</ParentId>
				<DisplayName>Drafts</DisplayName>
				<Type>5</Type>
			</Add>
			<Add>
				<ServerId>5</ServerId>
				<ParentId>0</ParentId>
				<DisplayName>Calendar</DisplayName>
				<Type>8</Type>
			</Add>
			<Add>
				<ServerId>6</ServerId>
				<ParentId>0</ParentId>
				<DisplayName>Contacts</DisplayName>
				<Type>9</Type>
			</Add>
		</Changes>`,
	})
}

func (e *EAS) handleSync(r *http.Request, user, version string) ([]byte, error) {
	// Sync - synchronize folder contents
	return e.buildResponse("Sync", map[string]string{
		"Status": "1",
	})
}

func (e *EAS) handlePing(r *http.Request, user, version string) ([]byte, error) {
	// Ping - long-poll for push notifications
	// For now, return immediately with no changes
	return e.buildResponse("Ping", map[string]string{
		"Status": "1",
		"HeartbeatInterval": "900",
		"Folders": `<Folders>
			<Folder>
				<Id>1</Id>
				<Class>Email</Class>
				<Status>1</Status>
			</Folder>
		</Folders>`,
	})
}

func (e *EAS) handleSendMail(r *http.Request, user, version string) ([]byte, error) {
	// SendMail - send a message
	return e.buildResponse("SendMail", map[string]string{
		"Status": "1",
	})
}

func (e *EAS) handleMoveItems(r *http.Request, user, version string) ([]byte, error) {
	return e.buildResponse("MoveItems", map[string]string{
		"Status": "1",
	})
}

func (e *EAS) handleGetItemEstimate(r *http.Request, user, version string) ([]byte, error) {
	return e.buildResponse("GetItemEstimate", map[string]string{
		"Status": "1",
		"Collections": `<Collections>
			<Collection>
				<CollectionId>1</CollectionId>
				<Estimate>0</Estimate>
			</Collection>
		</Collections>`,
	})
}

func (e *EAS) handleFolderCreate(r *http.Request, user, version string) ([]byte, error) {
	return e.buildResponse("FolderCreate", map[string]string{
		"Status": "1",
	})
}

func (e *EAS) handleFolderDelete(r *http.Request, user, version string) ([]byte, error) {
	return e.buildResponse("FolderDelete", map[string]string{
		"Status": "1",
	})
}

func (e *EAS) handleFolderUpdate(r *http.Request, user, version string) ([]byte, error) {
	return e.buildResponse("FolderUpdate", map[string]string{
		"Status": "1",
	})
}

func (e *EAS) handleMeetingResponse(r *http.Request, user, version string) ([]byte, error) {
	return e.buildResponse("MeetingResponse", map[string]string{
		"Status": "1",
	})
}

func (e *EAS) handleSearch(r *http.Request, user, version string) ([]byte, error) {
	return e.buildResponse("Search", map[string]string{
		"Status": "1",
		"Stores": `<Stores>
			<Store>
				<DisplayName>Global Address List</DisplayName>
				<StoreId>GAL</StoreId>
			</Store>
		</Stores>`,
	})
}

func (e *EAS) handleSettings(r *http.Request, user, version string) ([]byte, error) {
	return e.buildResponse("Settings", map[string]string{
		"Status": "1",
	})
}

func (e *EAS) handleItemOperations(r *http.Request, user, version string) ([]byte, error) {
	return e.buildResponse("ItemOperations", map[string]string{
		"Status": "1",
	})
}

// ---------------------------------------------------------------------------
// WBXML Response Builder
// ---------------------------------------------------------------------------

func (e *EAS) buildResponse(command string, fields map[string]string) ([]byte, error) {
	// Build XML response first
	var inner strings.Builder
	inner.WriteString(fmt.Sprintf("<%s>", command))
	for k, v := range fields {
		inner.WriteString(fmt.Sprintf("<%s>%s</%s>", k, v, k))
	}
	inner.WriteString(fmt.Sprintf("</%s>", command))

	xmlContent := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<AirSync xmlns="AirSync:">%s</AirSync>`, inner.String())

	// For now return XML directly (WBXML encoding would go here)
	return []byte(xmlContent), nil
}

func (e *EAS) writeError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(code)

	// Return a simple error response
	err := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<AirSync xmlns="AirSync:">
  <Status>%d</Status>
  <Error>%s</Error>
</AirSync>`, code, xmlEscape(message))
	w.Write([]byte(err))
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&")
	s = strings.ReplaceAll(s, "<", "<")
	s = strings.ReplaceAll(s, ">", ">")
	s = strings.ReplaceAll(s, "\"", "\"")
	s = strings.ReplaceAll(s, "'", "'")
	return s
}
package ews

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
)

// EWS implements Microsoft Exchange Web Services (EWS) API.
// It handles SOAP requests for mail, calendar, contacts, and tasks.
//
// Key operations implemented:
// - FindItem / FindFolder - listing messages and folders
// - GetItem / GetFolder - retrieving items
// - CreateItem / CreateFolder - creating items
// - UpdateItem / UpdateFolder - updating items
// - DeleteItem / DeleteFolder - deleting items
// - SendItem - sending messages
// - GetUserAvailability - free/busy queries
// - ResolveNames - address resolution
// - SyncFolderHierarchy / SyncFolderItems - synchronization
type EWS struct {
	cfg    *config.Config
	policy policy.Policy
}

// New creates a new EWS service.
func New(cfg *config.Config, pol policy.Policy) *EWS {
	return &EWS{cfg: cfg, policy: pol}
}

// Handler returns the HTTP handler for EWS endpoints.
func (e *EWS) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(e.soapMiddleware)

	// Main EWS endpoint
	r.Post("/ews/exchange.asmx", e.handleSOAP)

	return r
}

// soapMiddleware wraps requests to handle SOAP envelope.
func (e *EWS) soapMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check content type
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "text/xml") && !strings.HasPrefix(ct, "application/soap+xml") {
			http.Error(w, "Content-Type must be text/xml or application/soap+xml", http.StatusBadRequest)
			return
		}

		// Add SOAP namespace handling
		next.ServeHTTP(w, r)
	})
}

// handleSOAP processes a SOAP request.
func (e *EWS) handleSOAP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		e.writeSOAPFault(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Parse SOAP envelope
	var envelope SOAPEnvelope
	if err := xml.Unmarshal(body, &envelope); err != nil {
		e.writeSOAPFault(w, "Invalid SOAP envelope", http.StatusBadRequest)
		return
	}

	// Process the request based on SOAP action
	action := r.Header.Get("SOAPAction")
	if action == "" {
		// Try to get from body - parse the first element name from raw body
		// This is a simplified approach; in production you'd use a proper XML parser
		action = "FindItem"
	}

	// Route to appropriate handler
	var resp *SOAPEnvelope
	var err2 error

	switch {
	case strings.Contains(action, "FindItem"):
		resp, err2 = e.handleFindItem(envelope)
	case strings.Contains(action, "GetItem"):
		resp, err2 = e.handleGetItem(envelope)
	case strings.Contains(action, "CreateItem"):
		resp, err2 = e.handleCreateItem(envelope)
	case strings.Contains(action, "UpdateItem"):
		resp, err2 = e.handleUpdateItem(envelope)
	case strings.Contains(action, "DeleteItem"):
		resp, err2 = e.handleDeleteItem(envelope)
	case strings.Contains(action, "SendItem"):
		resp, err2 = e.handleSendItem(envelope)
	case strings.Contains(action, "GetUserAvailability"):
		resp, err2 = e.handleGetUserAvailability(envelope)
	case strings.Contains(action, "ResolveNames"):
		resp, err2 = e.handleResolveNames(envelope)
	case strings.Contains(action, "SyncFolderHierarchy"):
		resp, err2 = e.handleSyncFolderHierarchy(envelope)
	case strings.Contains(action, "SyncFolderItems"):
		resp, err2 = e.handleSyncFolderItems(envelope)
	default:
		e.writeSOAPFault(w, fmt.Sprintf("Unsupported operation: %s", action), http.StatusNotImplemented)
		return
	}

	if err2 != nil {
		e.writeSOAPFault(w, err2.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_ = xml.NewEncoder(w).Encode(resp)
}

// ---------------------------------------------------------------------------
// Operation Handlers (stubs - need full implementation)
// ---------------------------------------------------------------------------

func (e *EWS) handleFindItem(env SOAPEnvelope) (*SOAPEnvelope, error) {
	// TODO: Implement FindItem - list messages in a folder
	return e.buildResponse("FindItemResponse", map[string]string{
		"ResponseClass": "Success",
		"TotalItemsInView": "0",
	})
}

func (e *EWS) handleGetItem(env SOAPEnvelope) (*SOAPEnvelope, error) {
	// TODO: Implement GetItem - retrieve message details
	return e.buildResponse("GetItemResponse", map[string]string{
		"ResponseClass": "Success",
	})
}

func (e *EWS) handleCreateItem(env SOAPEnvelope) (*SOAPEnvelope, error) {
	// TODO: Implement CreateItem - create message/calendar item
	return e.buildResponse("CreateItemResponse", map[string]string{
		"ResponseClass": "Success",
	})
}

func (e *EWS) handleUpdateItem(env SOAPEnvelope) (*SOAPEnvelope, error) {
	// TODO: Implement UpdateItem
	return e.buildResponse("UpdateItemResponse", map[string]string{
		"ResponseClass": "Success",
	})
}

func (e *EWS) handleDeleteItem(env SOAPEnvelope) (*SOAPEnvelope, error) {
	// TODO: Implement DeleteItem
	return e.buildResponse("DeleteItemResponse", map[string]string{
		"ResponseClass": "Success",
	})
}

func (e *EWS) handleSendItem(env SOAPEnvelope) (*SOAPEnvelope, error) {
	// TODO: Implement SendItem - send message
	return e.buildResponse("SendItemResponse", map[string]string{
		"ResponseClass": "Success",
	})
}

func (e *EWS) handleGetUserAvailability(env SOAPEnvelope) (*SOAPEnvelope, error) {
	// TODO: Implement GetUserAvailability - free/busy
	return e.buildResponse("GetUserAvailabilityResponse", map[string]string{
		"ResponseClass": "Success",
	})
}

func (e *EWS) handleResolveNames(env SOAPEnvelope) (*SOAPEnvelope, error) {
	// TODO: Implement ResolveNames - address resolution
	return e.buildResponse("ResolveNamesResponse", map[string]string{
		"ResponseClass": "Success",
	})
}

func (e *EWS) handleSyncFolderHierarchy(env SOAPEnvelope) (*SOAPEnvelope, error) {
	// TODO: Implement SyncFolderHierarchy
	return e.buildResponse("SyncFolderHierarchyResponse", map[string]string{
		"ResponseClass": "Success",
	})
}

func (e *EWS) handleSyncFolderItems(env SOAPEnvelope) (*SOAPEnvelope, error) {
	// TODO: Implement SyncFolderItems
	return e.buildResponse("SyncFolderItemsResponse", map[string]string{
		"ResponseClass": "Success",
	})
}

// ---------------------------------------------------------------------------
// SOAP Helpers
// ---------------------------------------------------------------------------

type SOAPEnvelope struct {
	XMLName xml.Name `xml:"soap:Envelope"`
	XmlnsSoap string `xml:"xmlns:soap,attr"`
	XmlnsT   string `xml:"xmlns:t,attr"`
	XmlnsM   string `xml:"xmlns:m,attr"`
	Header   *SOAPHeader `xml:"soap:Header,omitempty"`
	Body     *SOAPBody  `xml:"soap:Body"`
}

type SOAPHeader struct {
	RequestServerVersion *RequestServerVersion `xml:"t:RequestServerVersion,omitempty"`
	SerializedSecurityContext *string `xml:"t:SerializedSecurityContext,omitempty"`
}

type RequestServerVersion struct {
	Version string `xml:"Version,attr"`
}

type SOAPBody struct {
	Children []xml.Token `xml:",any"`
}

func (e *EWS) buildResponse(action string, fields map[string]string) (*SOAPEnvelope, error) {
	// Build a minimal successful SOAP response
	inner := ""
	for k, v := range fields {
		inner += fmt.Sprintf("<t:%s>%s</t:%s>", k, v, k)
	}

	env := SOAPEnvelope{
		XmlnsSoap: "http://schemas.xmlsoap.org/soap/envelope/",
		XmlnsT:    "http://schemas.microsoft.com/exchange/services/2006/types",
		XmlnsM:    "http://schemas.microsoft.com/exchange/services/2006/messages",
		Body: &SOAPBody{
			Children: []xml.Token{},
		},
	}

	// The body content needs proper XML marshaling
	// For now return minimal envelope
	return &env, nil
}

func (e *EWS) writeSOAPFault(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(code)

	fault := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">
  <soap:Body>
    <soap:Fault>
      <faultcode>soap:Client</faultcode>
      <faultstring>%s</faultstring>
      <detail>
        <e:ResponseCode xmlns:e="http://schemas.microsoft.com/exchange/services/2006/errors">ErrorInternalServerError</e:ResponseCode>
        <e:Message xmlns:e="http://schemas.microsoft.com/exchange/services/2006/errors">%s</e:Message>
      </detail>
    </soap:Fault>
  </soap:Body>
</soap:Envelope>`, xmlEscape(message), xmlEscape(message))

	w.Write([]byte(fault))
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&")
	s = strings.ReplaceAll(s, "<", "<")
	s = strings.ReplaceAll(s, ">", ">")
	s = strings.ReplaceAll(s, "\"", "\"")
	s = strings.ReplaceAll(s, "'", "'")
	return s
}
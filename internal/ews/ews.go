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
//
// The route is relative to the mount point: the server mounts this handler at
// /ews, so "/ews/exchange.asmx" here would only answer at
// /ews/ews/exchange.asmx, which is not where Exchange clients - or the URL
// Autodiscover hands them - look.
func (e *EWS) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(e.soapMiddleware)

	// Main EWS endpoint, at the path MS-OXECWS specifies.
	r.Post("/exchange.asmx", e.handleSOAP)

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
	action := soapActionName(r.Header.Get("SOAPAction"))
	if action == "" {
		// Not every client sets SOAPAction. MS-OXECWS identifies the operation by
		// the element in the body, so fall back to that rather than guessing.
		action = envelope.Body.Operation()
	}
	if action == "" {
		e.writeSOAPFault(w, "Invalid SOAP envelope", http.StatusBadRequest)
		return
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

// soapActionName extracts the operation from a SOAPAction header. The header
// holds a URI - typically
// "http://schemas.microsoft.com/exchange/services/2006/messages/FindItem" - and
// sometimes the empty string, so the last path segment is the operation name.
func soapActionName(header string) string {
	header = strings.TrimSpace(header)
	header = strings.Trim(header, `"`)
	if header == "" {
		return ""
	}
	if i := strings.LastIndex(header, "/"); i >= 0 {
		return header[i+1:]
	}
	return header
}

// ---------------------------------------------------------------------------
// Operation Handlers (stubs - need full implementation)
// ---------------------------------------------------------------------------

func (e *EWS) handleFindItem(env SOAPEnvelope) (*SOAPEnvelope, error) {
	// TODO: Implement FindItem - list messages in a folder
	return e.buildResponse("FindItemResponse", map[string]string{
		"ResponseClass":    "Success",
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

// soapEnvelopeNS is the SOAP 1.1 envelope namespace. Struct tags have to name
// it in full: "soap:Envelope" in a tag is read as the namespace "soap", which
// no envelope declares, so every request failed to unmarshal and was answered
// "Invalid SOAP envelope".
const soapEnvelopeNS = "http://schemas.xmlsoap.org/soap/envelope/"

// messagesNS is the EWS operations namespace (MS-OXECWS).
const messagesNS = "http://schemas.microsoft.com/exchange/services/2006/messages"

type SOAPEnvelope struct {
	XMLName   xml.Name    `xml:"http://schemas.xmlsoap.org/soap/envelope/ Envelope"`
	XmlnsSoap string      `xml:"xmlns:soap,attr"`
	XmlnsT    string      `xml:"xmlns:t,attr"`
	XmlnsM    string      `xml:"xmlns:m,attr"`
	Header    *SOAPHeader `xml:"http://schemas.xmlsoap.org/soap/envelope/ Header,omitempty"`
	Body      *SOAPBody   `xml:"http://schemas.xmlsoap.org/soap/envelope/ Body"`
}

type SOAPHeader struct {
	RequestServerVersion      *RequestServerVersion `xml:"http://schemas.microsoft.com/exchange/services/2006/types RequestServerVersion,omitempty"`
	SerializedSecurityContext *string               `xml:"http://schemas.microsoft.com/exchange/services/2006/types SerializedSecurityContext,omitempty"`
}

type RequestServerVersion struct {
	Version string `xml:"Version,attr"`
}

type SOAPBody struct {
	// Inner is the raw XML of the body's children, which is where the operation
	// and its arguments live. Holding it as a string is what lets the operation
	// handlers decode the shape each operation needs without every envelope
	// field being spelled out here.
	Inner string `xml:",innerxml"`
}

// Operation returns the name of the requested operation: the local name of the
// first element inside s:Body. That element is the operation in MS-OXECWS -
// FindItem, GetItem, CreateItem and the rest - and it is what dispatch keys on
// when the SOAPAction header is absent.
func (b *SOAPBody) Operation() string {
	if b == nil {
		return ""
	}
	dec := xml.NewDecoder(strings.NewReader(b.Inner))
	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF here just means the body held nothing but whitespace.
			return ""
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local
		}
	}
}

// typesNS is the EWS types namespace (MS-OXECWS).
const typesNS = "http://schemas.microsoft.com/exchange/services/2006/types"

// buildResponse wraps an operation's response element in a SOAP envelope. The
// body is assembled as text rather than as a struct because each operation
// returns a different element shape, and the element name is the one thing the
// transport has to get exactly right.
func (e *EWS) buildResponse(action string, fields map[string]string) (*SOAPEnvelope, error) {
	inner := ""
	for k, v := range fields {
		inner += fmt.Sprintf("<t:%s>%s</t:%s>", k, xmlEscape(v), k)
	}

	return &SOAPEnvelope{
		XmlnsSoap: soapEnvelopeNS,
		XmlnsT:    typesNS,
		XmlnsM:    messagesNS,
		Body: &SOAPBody{
			Inner: fmt.Sprintf(`<m:%s>%s</m:%s>`, action, inner, action),
		},
	}, nil
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

// xmlEscape escapes a string for inclusion in element content.
//
// This has to be real escaping, not decoration: the fault text quotes back the
// operation name, which arrives in the SOAPAction header and is therefore under
// the caller's control. A caller sending &lt; in that header would otherwise
// get markup of their choosing reflected into the response.
func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		// EscapeText only fails if the writer fails, and strings.Builder does not.
		return ""
	}
	return b.String()
}

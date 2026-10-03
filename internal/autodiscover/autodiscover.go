package autodiscover

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/store"
)

// Autodiscover implements the Microsoft Autodiscover service (MS-OXDISCO)
// using the POX (Plain Old XML) protocol. It allows Outlook and other
// Exchange clients to automatically discover server settings.
//
// The service handles two request types:
// 1. POST /autodiscover/autodiscover.xml with XML body (POX request)
// 2. GET /autodiscover/autodiscover.xml with query parameters (legacy)
//
// Key endpoints exposed:
// - IMAP/SMTP settings for mail
// - EWS endpoint for Exchange Web Services
// - EAS endpoint for ActiveSync
// - MAPI/HTTP endpoint (when available)
type Autodiscover struct {
	cfg *config.Config
	db  *sqlx.DB
}

// New creates a new Autodiscover service.
func New(cfg *config.Config, db *sqlx.DB) *Autodiscover {
	return &Autodiscover{cfg: cfg, db: db}
}

// Handler returns the HTTP handler for Autodiscover endpoints.
func (a *Autodiscover) Handler() http.Handler {
	r := chi.NewRouter()

	// POX endpoint - handles both POST (XML) and GET (legacy)
	r.Post("/autodiscover.xml", a.handlePOX)
	r.Get("/autodiscover.xml", a.handlePOX)

	// Legacy redirect
	r.Get("/autodiscover.svc", a.handleLegacyRedirect)
	r.Get("/Autodiscover.xml", a.handlePOX)

	return r
}

// handlePOX processes a POX Autodiscover request.
func (a *Autodiscover) handlePOX(w http.ResponseWriter, r *http.Request) {
	var req POXRequest
	var err error

	if r.Method == http.MethodPost {
		err = xml.NewDecoder(r.Body).Decode(&req)
	} else {
		// GET request - parse query parameters
		req = POXRequest{
			Email: r.URL.Query().Get("email"),
		}
	}

	if err != nil {
		a.writeError(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Normalize email
	email := strings.TrimSpace(req.Email)
	if email == "" {
		a.writeError(w, "Email required", http.StatusBadRequest)
		return
	}

	// Validate domain is hosted here
	localPart, domain, ok := strings.Cut(email, "@")
	if !ok || domain == "" {
		a.writeError(w, "Invalid email address", http.StatusBadRequest)
		return
	}

	if !a.isHostedDomain(r.Context(), domain) {
		a.writeError(w, "Domain not hosted", http.StatusNotFound)
		return
	}

	// Build response
	resp := a.buildResponse(localPart, domain, req.RequestType)

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_ = xml.NewEncoder(w).Encode(resp)
}

// handleLegacyRedirect redirects legacy Autodiscover URLs.
func (a *Autodiscover) handleLegacyRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/autodiscover/autodiscover.xml", http.StatusMovedPermanently)
}

// isHostedDomain checks if a domain is hosted by this server.
func (a *Autodiscover) isHostedDomain(ctx context.Context, domain string) bool {
	dom, err := store.NewDomains().Get(ctx, a.db, domain)
	if err != nil {
		return false
	}
	return dom.Status == store.DomainStatusActive
}

// buildResponse constructs the Autodiscover response XML.
func (a *Autodiscover) buildResponse(localPart, domain, requestType string) *POXResponse {
	hostname := a.cfg.Server.Hostname
	internalHostname := hostname
	if a.cfg.Server.PublicIP != "" {
		internalHostname = a.cfg.Server.PublicIP
	}

	resp := &POXResponse{
		Response: &Response{
			User: &User{
				DisplayName:  localPart,
				LegacyDN:     fmt.Sprintf("/o=linchange/ou=Exchange Administrative Group (FYDIBOHF23SPDLT)/cn=Recipients/cn=%s", localPart),
				DeploymentID: "linchange",
			},
			Account: &Account{
				AccountType: "email",
				Action:      "settings",
				Protocol:    a.buildProtocols(hostname, internalHostname),
				PublicFolderInfo: &PublicFolderInfo{
					Server: hostname,
				},
			},
		},
	}
	return resp
}

// buildProtocols constructs the protocol configuration for the response.
func (a *Autodiscover) buildProtocols(hostname, internalHostname string) []Protocol {
	protocols := []Protocol{
		// IMAP
		{
			Type:        "IMAP",
			Server:      hostname,
			Port:        993,
			SSL:         "On",
			AuthPackage: "NTLM",
			LoginName:   "{{EmailAddress}}",
		},
		{
			Type:        "IMAP",
			Server:      internalHostname,
			Port:        143,
			SSL:         "Off",
			AuthPackage: "NTLM",
			LoginName:   "{{EmailAddress}}",
		},
		// SMTP
		{
			Type:        "SMTP",
			Server:      hostname,
			Port:        587,
			SSL:         "On",
			AuthPackage: "NTLM",
			LoginName:   "{{EmailAddress}}",
		},
		{
			Type:        "SMTP",
			Server:      internalHostname,
			Port:        25,
			SSL:         "Off",
			AuthPackage: "NTLM",
			LoginName:   "{{EmailAddress}}",
		},
		// POP3
		{
			Type:        "POP3",
			Server:      hostname,
			Port:        995,
			SSL:         "On",
			AuthPackage: "NTLM",
			LoginName:   "{{EmailAddress}}",
		},
		{
			Type:        "POP3",
			Server:      internalHostname,
			Port:        110,
			SSL:         "Off",
			AuthPackage: "NTLM",
			LoginName:   "{{EmailAddress}}",
		},
	}

	// Add EWS if enabled
	if a.cfg.Exchange.Enabled {
		protocols = append(protocols, Protocol{
			Type:        "EXCH",
			Server:      hostname,
			ASUrl:       fmt.Sprintf("https://%s/ews/exchange.asmx", hostname),
			OABUrl:      fmt.Sprintf("https://%s/oab", hostname),
			SSL:         "On",
			AuthPackage: "NTLM",
			LoginName:   "{{EmailAddress}}",
		})

		// Add ActiveSync
		protocols = append(protocols, Protocol{
			Type:        "EXPR",
			Server:      hostname,
			ASUrl:       fmt.Sprintf("https://%s/eas", hostname),
			SSL:         "On",
			AuthPackage: "NTLM",
			LoginName:   "{{EmailAddress}}",
		})
	}

	return protocols
}

// writeError writes an error response.
func (a *Autodiscover) writeError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(code)

	resp := &POXResponse{
		Error: &Error{
			Code:    code,
			Message: message,
		},
	}
	_ = xml.NewEncoder(w).Encode(resp)
}

// ---------------------------------------------------------------------------
// XML Structures for POX Autodiscover (MS-OXDISCO)
// ---------------------------------------------------------------------------

type POXRequest struct {
	XMLName     xml.Name `xml:"Autodiscover"`
	Email       string   `xml:"Request>EmailAddress"`
	RequestType string   `xml:"Request>AcceptableResponseSchema,omitempty"`
}

type POXResponse struct {
	XMLName  xml.Name  `xml:"Autodiscover"`
	Response *Response `xml:"Response,omitempty"`
	Error    *Error    `xml:"Error,omitempty"`
}

type Response struct {
	User    *User    `xml:"User,omitempty"`
	Account *Account `xml:"Account,omitempty"`
}

type User struct {
	DisplayName  string `xml:"DisplayName,omitempty"`
	LegacyDN     string `xml:"LegacyDN,omitempty"`
	DeploymentID string `xml:"DeploymentId,omitempty"`
}

type Account struct {
	AccountType      string            `xml:"AccountType,omitempty"`
	Action           string            `xml:"Action,omitempty"`
	Protocol         []Protocol        `xml:"Protocol,omitempty"`
	PublicFolderInfo *PublicFolderInfo `xml:"PublicFolderInformation,omitempty"`
}

type PublicFolderInfo struct {
	Server string `xml:"Server,omitempty"`
}

type Protocol struct {
	Type        string `xml:"Type,omitempty"`
	Server      string `xml:"Server,omitempty"`
	Port        int    `xml:"Port,omitempty"`
	SSL         string `xml:"SSL,omitempty"`
	AuthPackage string `xml:"AuthPackage,omitempty"`
	LoginName   string `xml:"LoginName,omitempty"`
	ASUrl       string `xml:"ASUrl,omitempty"`
	OABUrl      string `xml:"OABUrl,omitempty"`
	ADUrl       string `xml:"ADUrl,omitempty"`
	EwsUrl      string `xml:"EwsUrl,omitempty"`
	OOFUrl      string `xml:"OOFUrl,omitempty"`
	UMUrl       string `xml:"UMUrl,omitempty"`
}

type Error struct {
	Code    int    `xml:"ErrorCode,omitempty"`
	Message string `xml:"ErrorMessage,omitempty"`
}

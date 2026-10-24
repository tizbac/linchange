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

const (
	// activeSyncPath is the canonical Exchange ActiveSync endpoint. Outlook
	// and mobile clients fall back to it when Autodiscover does not answer,
	// and the URL handed to a client must be the same path the server mounts.
	activeSyncPath                = "/Microsoft-Server-ActiveSync"
	mobileSyncResponseSchema      = "schemas.microsoft.com/exchange/autodiscover/mobilesync/responseschema/2006"
	mobileSyncResponseSchemaHTTPS = "https://" + mobileSyncResponseSchema
	mobileSyncResponseSchemaHTTP  = "http://" + mobileSyncResponseSchema
)

// mobileSyncNamespace uses the scheme the client asked for. Older ActiveSync
// clients name the http namespace in AcceptableResponseSchema and compare it
// literally; the current one uses https.
func mobileSyncNamespace(requestType string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(requestType)), "http://") {
		return mobileSyncResponseSchemaHTTP
	}
	return mobileSyncResponseSchemaHTTPS
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
	email := strings.TrimSpace(req.Address())
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

	// ActiveSync clients ask for the MobileSync schema, not the Outlook one.
	// They do not understand an EXCH/EXPR response: the missing MobileSync
	// server entry is what Outlook reports as "the server cannot be found".
	if strings.Contains(strings.ToLower(req.RequestType), "mobilesync") {
		if !a.cfg.Exchange.Enabled || !a.cfg.Exchange.EAS.Enabled {
			a.writeError(w, "ActiveSync is not enabled", http.StatusNotFound)
			return
		}
		a.writeMobileSync(w, email, localPart, req.RequestType)
		return
	}

	// Build response
	resp := a.buildResponse(localPart, domain, req.RequestType)

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_ = xml.NewEncoder(w).Encode(resp)
}

// writeMobileSync answers an Exchange ActiveSync Autodiscover request.
//
// The MobileSync schema is deliberately smaller than the Outlook schema: it
// carries one server entry whose type is "MobileSync" and whose URL is the
// ActiveSync endpoint. Outlook and mobile clients look for that entry; an
// Autodiscover response that only describes EWS is a successful lookup with no
// ActiveSync server in it.
func (a *Autodiscover) writeMobileSync(w http.ResponseWriter, email, localPart, requestType string) {
	url := a.exchangeURL(a.cfg.Server.Hostname, activeSyncPath)
	resp := &mobileSyncResponse{
		XMLNS: mobileSyncNamespace(requestType),
		Response: mobileSyncBody{
			Culture: "en:us",
			User: mobileSyncUser{
				DisplayName:  localPart,
				EMailAddress: email,
			},
			Action: mobileSyncAction{
				Settings: mobileSyncSettings{
					Server: mobileSyncServer{
						Type: "MobileSync",
						URL:  url,
						Name: url,
					},
				},
			},
		},
	}
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
//
// The ports come from the advertised set rather than the standard ones, because
// a client reads this to decide where to connect. A deployment that publishes
// IMAP on 5143 - which is what the demo container does - has to be described
// accurately here or the client is sent to a port nothing is listening on.
func (a *Autodiscover) buildProtocols(hostname, internalHostname string) []Protocol {
	adv := a.cfg.Exchange.Advertised
	protocols := []Protocol{
		// IMAP
		{
			Type:        "IMAP",
			Server:      hostname,
			Port:        adv.Or(adv.IMAPS, 993),
			SSL:         "On",
			AuthPackage: "Basic",
			LoginName:   "{{EmailAddress}}",
		},
		{
			Type:        "IMAP",
			Server:      internalHostname,
			Port:        adv.Or(adv.IMAP, 143),
			SSL:         "Off",
			AuthPackage: "Basic",
			LoginName:   "{{EmailAddress}}",
		},
		// SMTP
		{
			Type:        "SMTP",
			Server:      hostname,
			Port:        adv.Or(adv.Submit, 587),
			SSL:         "On",
			AuthPackage: "Basic",
			LoginName:   "{{EmailAddress}}",
		},
		{
			Type:        "SMTP",
			Server:      internalHostname,
			Port:        adv.Or(adv.SMTP, 25),
			SSL:         "Off",
			AuthPackage: "Basic",
			LoginName:   "{{EmailAddress}}",
		},
		// POP3
		{
			Type:        "POP3",
			Server:      hostname,
			Port:        adv.Or(adv.POP3S, 995),
			SSL:         "On",
			AuthPackage: "Basic",
			LoginName:   "{{EmailAddress}}",
		},
		{
			Type:        "POP3",
			Server:      internalHostname,
			Port:        adv.Or(adv.POP3, 110),
			SSL:         "Off",
			AuthPackage: "Basic",
			LoginName:   "{{EmailAddress}}",
		},
	}

	// Add EWS if enabled
	if a.cfg.Exchange.Enabled {
		ssl := "Off"
		if a.servesTLS() {
			ssl = "On"
		}
		protocols = append(protocols, Protocol{
			Type:        "EXCH",
			Server:      hostname,
			ASUrl:       a.exchangeURL(hostname, "/ews/exchange.asmx"),
			EwsUrl:      a.exchangeURL(hostname, "/ews/exchange.asmx"),
			OABUrl:      a.exchangeURL(hostname, "/oab"),
			SSL:         ssl,
			AuthPackage: "Basic",
			LoginName:   "{{EmailAddress}}",
		})

		// Add ActiveSync
		protocols = append(protocols, Protocol{
			Type:        "EXPR",
			Server:      hostname,
			ASUrl:       a.exchangeURL(hostname, "/eas"),
			SSL:         ssl,
			AuthPackage: "Basic",
			LoginName:   "{{EmailAddress}}",
		})
	}

	return protocols
}

// exchangeURL builds the URL of an Exchange endpoint.
//
// The scheme follows whether this server is answering with TLS itself, because
// that is what decides whether a client can complete the handshake there. The
// advertised HTTPS port alone says nothing about it: a deployment that runs
// the HTTP listener only, and advertises a published port besides, used to be
// described as https://host:58443/eas, and a client sent to a port where no
// TLS handshake can complete reports exactly what the Outlook setup screen
// showed: "server not found". A deployment terminated at a TLS-terminating
// proxy serves TLS through the HTTPS listener configured with the same
// certificates, and states its ports in the advertised set.
func (a *Autodiscover) exchangeURL(hostname, path string) string {
	adv := a.cfg.Exchange.Advertised
	scheme, standard := "http", 80
	port := adv.Or(adv.HTTP, 80)
	if a.servesTLS() {
		scheme, standard, port = "https", 443, adv.Or(adv.HTTPS, 443)
	}
	if port == standard {
		return fmt.Sprintf("%s://%s%s", scheme, hostname, path)
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, hostname, port, path)
}

// servesTLS reports whether this process answers with TLS. Only an HTTPS
// listener does; a certificate on its own is never reached.
func (a *Autodiscover) servesTLS() bool {
	return strings.TrimSpace(a.cfg.Listener.HTTPS.Addr) != ""
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
	XMLName xml.Name `xml:"Autodiscover"`
	// The address is read under both spellings: MS-OXDISCO names the element
	// EMailAddress, and a client that learned it from an older example spells
	// it EmailAddress. Getting this wrong makes every request arrive with no
	// address and be refused as incomplete.
	Email       string `xml:"Request>EMailAddress"`
	EmailPlain  string `xml:"Request>EmailAddress"`
	RequestType string `xml:"Request>AcceptableResponseSchema,omitempty"`
}

// Address is the request's email address, whichever spelling carried it.
func (p POXRequest) Address() string {
	if p.Email != "" {
		return p.Email
	}
	return p.EmailPlain
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

// The MobileSync response is a separate schema from the Outlook POX response.
// Its namespace and element names are what an ActiveSync client parses.
type mobileSyncResponse struct {
	XMLName  xml.Name       `xml:"Autodiscover"`
	XMLNS    string         `xml:"xmlns,attr"`
	Response mobileSyncBody `xml:"Response"`
}

type mobileSyncBody struct {
	Culture string           `xml:"Culture,omitempty"`
	User    mobileSyncUser   `xml:"User"`
	Action  mobileSyncAction `xml:"Action"`
}

type mobileSyncUser struct {
	DisplayName  string `xml:"DisplayName,omitempty"`
	EMailAddress string `xml:"EMailAddress,omitempty"`
}

type mobileSyncAction struct {
	Settings mobileSyncSettings `xml:"Settings"`
}

type mobileSyncSettings struct {
	Server mobileSyncServer `xml:"Server"`
}

type mobileSyncServer struct {
	Type string `xml:"Type"`
	URL  string `xml:"Url"`
	Name string `xml:"Name"`
}

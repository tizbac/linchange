package autodiscover

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linchange/linchange/config"
)

// find returns the protocol of the given type listening on the given port, or
// nil. Tests care about the pairing, so the lookup is by both fields.
func find(protocols []Protocol, typ string, port int) *Protocol {
	for i := range protocols {
		if protocols[i].Type == typ && protocols[i].Port == port {
			return &protocols[i]
		}
	}
	return nil
}

// Without an advertised set the response has to carry the standard ports, which
// is what a server on the public internet answers with.
func TestBuildProtocolsReportsStandardPortsByDefault(t *testing.T) {
	a := &Autodiscover{cfg: config.New()}
	protocols := a.buildProtocols("mail.example.com", "203.0.113.10")

	for _, want := range []struct {
		typ  string
		port int
	}{
		{"IMAP", 993},
		{"IMAP", 143},
		{"SMTP", 587},
		{"SMTP", 25},
		{"POP3", 995},
		{"POP3", 110},
	} {
		if find(protocols, want.typ, want.port) == nil {
			t.Errorf("no %s protocol on port %d in the response", want.typ, want.port)
		}
	}
}

// A deployment that publishes the listeners elsewhere - the demo container maps
// 143 to 5143 - has to be described accurately, or the client is handed a port
// nothing is listening on.
func TestBuildProtocolsReportsAdvertisedPorts(t *testing.T) {
	cfg := config.New()
	cfg.Exchange.Advertised = config.Ports{
		IMAP:   5143,
		IMAPS:  5993,
		POP3:   5110,
		POP3S:  5995,
		SMTP:   5025,
		SMTPS:  5465,
		Submit: 5587,
		HTTP:   58080,
		HTTPS:  58443,
	}
	a := &Autodiscover{cfg: cfg}
	protocols := a.buildProtocols("localhost", "127.0.0.1")

	for _, want := range []struct {
		typ  string
		port int
	}{
		{"IMAP", 5143},
		{"IMAP", 5993},
		{"SMTP", 5587},
		{"SMTP", 5025},
		{"POP3", 5110},
		{"POP3", 5995},
	} {
		if find(protocols, want.typ, want.port) == nil {
			t.Errorf("no %s protocol on advertised port %d in the response", want.typ, want.port)
		}
	}

	// The standard ports must not also be advertised, or a client may pick
	// either one and only one of them works.
	for _, stale := range []struct {
		typ  string
		port int
	}{
		{"IMAP", 143},
		{"IMAP", 993},
		{"SMTP", 25},
		{"SMTP", 587},
		{"POP3", 110},
		{"POP3", 995},
	} {
		if p := find(protocols, stale.typ, stale.port); p != nil {
			t.Errorf("advertised ports should replace %s %d, but it is still offered", stale.typ, stale.port)
		}
	}
}

// The EWS and ActiveSync URLs are absolute, so a deployment that publishes TLS
// on another port has to see that port in them: the port is what the client
// connects to, and the scheme is the handshake it performs there.
func TestExchangeURLUsesAdvertisedHTTPSPort(t *testing.T) {
	cfg := config.New()
	cfg.Exchange.Enabled = true
	cfg.Listener.HTTPS.Addr = ":8443"
	cfg.Exchange.Advertised = config.Ports{HTTPS: 58443}
	a := &Autodiscover{cfg: cfg}

	protocols := a.buildProtocols("localhost", "127.0.0.1")

	var ews, eas *Protocol
	for i := range protocols {
		switch protocols[i].Type {
		case "EXCH":
			ews = &protocols[i]
		case "EXPR":
			eas = &protocols[i]
		}
	}
	if ews == nil {
		t.Fatal("no EXCH protocol when exchange is enabled")
	}
	if eas == nil {
		t.Fatal("no EXPR protocol when exchange is enabled")
	}
	if want := "https://localhost:58443/ews/exchange.asmx"; ews.ASUrl != want {
		t.Errorf("EWS URL = %q, want %q", ews.ASUrl, want)
	}
	if want := "https://localhost:58443/eas"; eas.ASUrl != want {
		t.Errorf("ActiveSync URL = %q, want %q", eas.ASUrl, want)
	}
}

// A URL must not promise TLS where no listener answers it. The demo and a
// fresh checkout serve HTTP only, and a client sent to an https:// URL on such
// a deployment reports the endpoint as not found -- which is what a person
// setting up Outlook saw, because the advertised URL was dead.
func TestExchangeURLFollowsTheServingListener(t *testing.T) {
	cfg := config.New()
	cfg.Exchange.Enabled = true
	cfg.Exchange.Advertised = config.Ports{HTTP: 18080, HTTPS: 58443}
	a := &Autodiscover{cfg: cfg}

	protocols := a.buildProtocols("localhost", "127.0.0.1")

	for _, p := range protocols {
		switch p.Type {
		case "EXCH":
			if want := "http://localhost:18080/ews/exchange.asmx"; p.ASUrl != want {
				t.Errorf("EWS URL = %q, want %q", p.ASUrl, want)
			}
		case "EXPR":
			if want := "http://localhost:18080/eas"; p.ASUrl != want {
				t.Errorf("ActiveSync URL = %q, want %q", p.ASUrl, want)
			}
		}
	}
}

// A deployment on the standard HTTPS port must not carry an explicit port in
// the URL, since some clients are literal about it.
func TestExchangeURLOmitsStandardPorts(t *testing.T) {
	cfg := config.New()
	cfg.Exchange.Enabled = true
	cfg.Listener.HTTPS.Addr = ":443"
	a := &Autodiscover{cfg: cfg}

	protocols := a.buildProtocols("mail.example.com", "203.0.113.10")

	for _, p := range protocols {
		if p.Type == "EXCH" && p.ASUrl != "https://mail.example.com/ews/exchange.asmx" {
			t.Errorf("EWS URL = %q, want no explicit port", p.ASUrl)
		}
	}
}

// ActiveSync clients ask for the MobileSync schema; a POX response that only
// describes EWS is not an answer they can use. The response must name the
// canonical ActiveSync path, which is also where the server mounts the EAS
// handler.
func TestMobileSyncResponseNamesTheActiveSyncEndpoint(t *testing.T) {
	cfg := config.New()
	cfg.Server.Hostname = "mail.example.com"
	cfg.Exchange.Enabled = true
	cfg.Exchange.EAS.Enabled = true
	cfg.Listener.HTTPS.Addr = ":443"
	a := &Autodiscover{cfg: cfg}

	rr := httptest.NewRecorder()
	a.writeMobileSync(rr, "alice@example.com", "alice", "https://schemas.microsoft.com/exchange/autodiscover/mobilesync/responseschema/2006")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"https://schemas.microsoft.com/exchange/autodiscover/mobilesync/responseschema/2006",
		"<Type>MobileSync</Type>",
		"<Url>https://mail.example.com/Microsoft-Server-ActiveSync</Url>",
		"<EMailAddress>alice@example.com</EMailAddress>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("MobileSync response does not contain %q:\n%s", want, body)
		}
	}
}

// The request's AcceptableResponseSchema is what selects the MobileSync
// response. Reading the element under the wrong name leaves RequestType empty
// and sends the client a POX response it cannot use.
func TestMobileSyncRequestIsRecognizedBySchema(t *testing.T) {
	const request = `<?xml version="1.0" encoding="utf-8"?>
<Autodiscover xmlns="https://schemas.microsoft.com/exchange/autodiscover/mobilesync/requestschema/2006">
  <Request>
    <EMailAddress>alice@example.com</EMailAddress>
    <AcceptableResponseSchema>https://schemas.microsoft.com/exchange/autodiscover/mobilesync/responseschema/2006</AcceptableResponseSchema>
  </Request>
</Autodiscover>`
	var req POXRequest
	if err := xml.Unmarshal([]byte(request), &req); err != nil {
		t.Fatalf("decoding the request: %v", err)
	}
	if req.Address() != "alice@example.com" {
		t.Errorf("address = %q, want alice@example.com", req.Address())
	}
	if !strings.Contains(strings.ToLower(req.RequestType), "mobilesync") {
		t.Errorf("RequestType = %q, want a MobileSync schema", req.RequestType)
	}
}

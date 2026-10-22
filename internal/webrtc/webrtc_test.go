package webrtc

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/turn/v4"

	"github.com/linchange/linchange/config"
)

// TestExternalICEServers: naming an external server replaces the built-in one,
// so the process does not bind 3478 as well.
func TestExternalICEServers(t *testing.T) {
	cfg := config.WebRTC{ICEServers: []config.ICEServer{{
		URLs:       []string{"stun:turn.example.com:3478", "turn:turn.example.com:3478"},
		Username:   "linchange",
		Credential: "secret",
	}}}

	svc, err := New(cfg, "127.0.0.1", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.Enabled() {
		t.Error("the built-in server started even though an external one was configured")
	}

	got := svc.ICEServers()
	if len(got) != 1 {
		t.Fatalf("ICE servers = %+v, want the external one", got)
	}
	if strings.Join(got[0].URLs, ",") != "stun:turn.example.com:3478,turn:turn.example.com:3478" {
		t.Errorf("urls = %v", got[0].URLs)
	}
	if got[0].Username != "linchange" || got[0].Credential != "secret" {
		t.Errorf("credentials = %q/%q", got[0].Username, got[0].Credential)
	}
}

// TestInternalICEServers: the default path advertises the server's own STUN
// and TURN, with a credential that expires.
func TestInternalICEServers(t *testing.T) {
	secret := []byte("a-test-secret")
	svc := &Service{
		cfg: config.WebRTC{TURN: config.TURN{
			Enabled:       true,
			Realm:         "linchange",
			Listen:        config.Addr{Addr: ":3478"},
			CredentialTTL: time.Hour,
		}},
		secret: secret,
		host:   "turn.example.com",
		port:   "3478",
		realm:  "linchange",
	}

	servers := svc.ICEServers()
	if len(servers) != 1 {
		t.Fatalf("ICE servers = %+v, want one", servers)
	}
	want := []string{
		"stun:turn.example.com:3478",
		"turn:turn.example.com:3478?transport=udp",
		"turn:turn.example.com:3478?transport=tcp",
	}
	if strings.Join(servers[0].URLs, ",") != strings.Join(want, ",") {
		t.Errorf("urls = %v, want %v", servers[0].URLs, want)
	}

	// The credential is a real TURN password for the username it came with,
	// and the same HMAC the auth handler will recompute.
	username := servers[0].Username
	if servers[0].Credential != svc.credential(username) {
		t.Error("credential does not match the server's HMAC")
	}
	expiry, err := strconv.ParseInt(username, 10, 64)
	if err != nil {
		t.Fatalf("username %q is not an expiry timestamp: %v", username, err)
	}
	if expiry <= time.Now().Unix() {
		t.Errorf("credential expires at %d, which is not in the future", expiry)
	}
}

func TestAuthHandlerAcceptsFreshAndRefusesExpired(t *testing.T) {
	svc := &Service{secret: []byte("a-test-secret"), realm: "linchange"}

	fresh := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	key, ok := svc.authHandler(fresh, "linchange", nil)
	if !ok {
		t.Fatal("a fresh credential was refused")
	}
	want := turn.GenerateAuthKey(fresh, "linchange", svc.credential(fresh))
	if !bytes.Equal(key, want) {
		t.Error("auth key is not the one the client computes")
	}

	expired := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	if _, ok := svc.authHandler(expired, "linchange", nil); ok {
		t.Error("an expired credential was accepted")
	}
	if _, ok := svc.authHandler("not-a-number", "linchange", nil); ok {
		t.Error("a malformed credential was accepted")
	}
}

// TestBuiltInServerStarts binds a real socket pair and shuts it down again, so
// the Pion wiring and the advertised port are exercised rather than only the
// credential arithmetic.
func TestBuiltInServerStarts(t *testing.T) {
	port := freePort(t)
	cfg := config.WebRTC{TURN: config.TURN{
		Enabled:       true,
		Realm:         "linchange",
		Listen:        config.Addr{Addr: fmt.Sprintf("127.0.0.1:%d", port)},
		RelayMinPort:  50000,
		RelayMaxPort:  50010,
		ExternalIP:    "127.0.0.1",
		Secret:        "test-secret",
		CredentialTTL: time.Hour,
	}}

	svc, err := New(cfg, "127.0.0.1", slog.Default())
	if err != nil {
		t.Fatalf("starting the built-in server: %v", err)
	}
	defer func() {
		if err := svc.Close(); err != nil {
			t.Errorf("closing: %v", err)
		}
	}()

	if !svc.Enabled() {
		t.Fatal("the server is not running")
	}
	servers := svc.ICEServers()
	if len(servers) != 1 {
		t.Fatalf("ICE servers = %+v", servers)
	}
	wantHostPort := fmt.Sprintf("127.0.0.1:%d", port)
	if !strings.Contains(servers[0].URLs[0], wantHostPort) {
		t.Errorf("STUN url = %q, want %q in it", servers[0].URLs[0], wantHostPort)
	}

	// The credential the browser would receive must actually be accepted, and
	// the relay must allocate. This is the whole point of the server: a URL
	// that cannot authenticate is worse than no TURN at all.
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("opening a TURN client socket: %v", err)
	}
	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: wantHostPort,
		TURNServerAddr: wantHostPort,
		Username:       servers[0].Username,
		Password:       servers[0].Credential,
		Realm:          "linchange",
		Conn:           conn,
		LoggerFactory:  quietLogging(),
	})
	if err != nil {
		t.Fatalf("building the TURN client: %v", err)
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		t.Fatalf("starting the TURN client: %v", err)
	}
	relay, err := client.Allocate()
	if err != nil {
		t.Fatalf("allocating a relay with the issued credential: %v", err)
	}
	_ = relay.Close()
}

// freePort reserves a port by binding it and releasing it again. The TURN
// server needs the same number on UDP and TCP, so the test cannot leave the
// port to the kernel's choice on one of them.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

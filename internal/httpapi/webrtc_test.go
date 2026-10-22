package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/httpapi"
	"github.com/linchange/linchange/internal/webrtc"
)

// TestICEServersHandOutTheConfiguredRelay checks the endpoint a browser calls
// before a call. The server owns the choice between its own STUN/TURN and an
// external one; the browser only receives the resulting list.
func TestICEServersHandOutTheConfiguredRelay(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")

	// An external configuration is used here because it needs no listening
	// socket; the built-in server is covered by its own package.
	ice, err := webrtc.New(config.WebRTC{ICEServers: []config.ICEServer{{
		URLs:       []string{"stun:turn.example.com:3478", "turn:turn.example.com:3478"},
		Username:   "linchange",
		Credential: "secret",
	}}}, "", nil)
	if err != nil {
		t.Fatalf("building the ICE service: %v", err)
	}

	srv, err := httpapi.New(httpapi.Options{
		DB: db, Config: testConfig(), Blobs: testBlobs(t), WebRTC: ice,
	})
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}

	// The relay addresses are a deployment detail, not public data.
	if code, _ := do(t, srv, http.MethodGet, "/api/v1/webrtc/ice", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated ICE request = %d, want 401", code)
	}

	auth := adminToken(t, srv, "alice@example.com")
	code, body := do(t, srv, http.MethodGet, "/api/v1/webrtc/ice", nil, auth)
	if code != http.StatusOK {
		t.Fatalf("ICE request = %d %v, want 200", code, body)
	}
	servers, ok := body["iceServers"].([]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("iceServers = %v, want one server", body["iceServers"])
	}
	first, _ := servers[0].(map[string]any)
	urls, _ := first["urls"].([]any)
	if len(urls) != 2 || urls[0] != "stun:turn.example.com:3478" {
		t.Errorf("urls = %v", first["urls"])
	}
	if first["username"] != "linchange" || first["credential"] != "secret" {
		t.Errorf("credentials = %v/%v", first["username"], first["credential"])
	}
}

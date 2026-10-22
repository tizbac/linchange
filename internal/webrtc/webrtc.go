// Package webrtc runs the STUN/TURN service the browser uses to reach the
// other participants in a call.
//
// A call is peer-to-peer, but two peers behind NATs cannot always find a direct
// path. TURN is the fallback: the browser relays its media through this server
// when it cannot connect directly. It is what makes a call work across a
// symmetric NAT or a corporate firewall, and running it here is what makes a
// single linchange install enough.
//
// The server is Pion's TURN implementation. Authentication is the short-lived
// credential scheme coturn popularised: the username is an expiry timestamp and
// the password is an HMAC of it under a server secret. The browser is given a
// credential that expires, so it never holds the secret, and the server can
// authenticate it without storing per-user TURN accounts.
package webrtc

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v4"

	"github.com/linchange/linchange/config"
)

// Service is the process's STUN/TURN server and the ICE configuration it hands
// to browsers.
type Service struct {
	cfg    config.WebRTC
	log    *slog.Logger
	server *turn.Server

	secret []byte
	host   string
	realm  string
	// port is the port the listener actually bound, which is what the URLs
	// handed to browsers must name.
	port string
}

// New builds the service. It starts the built-in server when TURN is enabled
// and no external ICE servers were configured.
//
// fallbackIP is the address to advertise when neither turn.external_ip nor
// turn.host says otherwise; it is the server's public address.
func New(cfg config.WebRTC, fallbackIP string, log *slog.Logger) (*Service, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.TURN.Realm == "" {
		cfg.TURN.Realm = "linchange"
	}
	if cfg.TURN.CredentialTTL <= 0 {
		cfg.TURN.CredentialTTL = 2 * time.Hour
	}
	if cfg.TURN.RelayMinPort == 0 {
		cfg.TURN.RelayMinPort = 49152
	}
	if cfg.TURN.RelayMaxPort == 0 {
		cfg.TURN.RelayMaxPort = 49251
	}

	s := &Service{cfg: cfg, log: log, realm: cfg.TURN.Realm}

	// An explicit external configuration replaces the built-in server: a
	// deployment that already runs TURN should not also bind 3478 here.
	if len(cfg.ICEServers) > 0 {
		s.log.Info("webrtc: using external ICE servers", "count", len(cfg.ICEServers))
		return s, nil
	}
	if !cfg.TURN.Enabled {
		s.log.Info("webrtc: built-in STUN/TURN is disabled")
		return s, nil
	}

	externalIP := strings.TrimSpace(cfg.TURN.ExternalIP)
	if externalIP == "" {
		externalIP = strings.TrimSpace(fallbackIP)
	}
	relayIP := net.ParseIP(externalIP)
	if relayIP == nil {
		return nil, fmt.Errorf("webrtc: %q is not an IP address to advertise as the relay", externalIP)
	}

	// The URL the browser is given may name a host while the relay itself
	// advertises an address; IP literals are the common case.
	s.host = strings.TrimSpace(cfg.TURN.Host)
	if s.host == "" {
		s.host = externalIP
	}

	listen := cfg.TURN.Listen.Addr
	if listen == "" {
		listen = ":3478"
	}

	if cfg.TURN.Secret != "" {
		s.secret = []byte(cfg.TURN.Secret)
	} else {
		// A random secret makes every issued credential expire with the
		// process. That is safe, and the alternative - a weak fixed default -
		// is not.
		s.secret = randomSecret()
		s.log.Info("webrtc: generated a TURN secret; set webrtc.turn.secret to keep credentials valid across restarts")
	}

	relay := &turn.RelayAddressGeneratorPortRange{
		RelayAddress: relayIP,
		// Bind the relay on every interface; the advertised address is the
		// one clients are told to use.
		Address: "0.0.0.0",
		MinPort: uint16(cfg.TURN.RelayMinPort),
		MaxPort: uint16(cfg.TURN.RelayMaxPort),
	}

	udpAddr, err := net.ResolveUDPAddr("udp4", listen)
	if err != nil {
		return nil, fmt.Errorf("webrtc: resolving TURN address %q: %w", listen, err)
	}
	udpConn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("webrtc: listening for TURN over UDP on %s: %w", listen, err)
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp4", listen)
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("webrtc: resolving TURN address %q: %w", listen, err)
	}
	tcpListener, err := net.ListenTCP("tcp4", tcpAddr)
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("webrtc: listening for TURN over TCP on %s: %w", listen, err)
	}

	server, err := turn.NewServer(turn.ServerConfig{
		Realm:       s.realm,
		AuthHandler: s.authHandler,
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn:            udpConn,
			RelayAddressGenerator: relay,
		}},
		ListenerConfigs: []turn.ListenerConfig{{
			Listener:              tcpListener,
			RelayAddressGenerator: relay,
		}},
		// Pion logs every allocation at info level. The server's own log
		// already says it started, and per-allocation lines would drown a
		// mail server's log, so Pion is kept to errors here.
		LoggerFactory: quietLogging(),
	})
	if err != nil {
		_ = udpConn.Close()
		_ = tcpListener.Close()
		return nil, fmt.Errorf("webrtc: starting the TURN server: %w", err)
	}

	s.server = server
	_, port, err := net.SplitHostPort(tcpListener.Addr().String())
	if err == nil {
		s.port = port
	}
	if s.port == "" {
		s.port = "3478"
	}
	s.cfg.TURN.Listen.Addr = listen

	s.log.Info("webrtc: STUN/TURN listening",
		"addr", listen,
		"relay_ip", externalIP,
		"relay_ports", fmt.Sprintf("%d-%d", cfg.TURN.RelayMinPort, cfg.TURN.RelayMaxPort))
	return s, nil
}

// Close stops the built-in server and releases its allocations.
func (s *Service) Close() error {
	if s == nil || s.server == nil {
		return nil
	}
	return s.server.Close()
}

// Enabled reports whether the built-in server is running.
func (s *Service) Enabled() bool { return s != nil && s.server != nil }

// ICEServers returns what the browser's RTCConfiguration should use.
//
// An external configuration is passed through. Otherwise the built-in server is
// described as STUN plus TURN over UDP and TCP, with a credential valid for the
// configured lifetime.
func (s *Service) ICEServers() []config.ICEServer {
	if s == nil {
		return nil
	}
	if len(s.cfg.ICEServers) > 0 {
		out := make([]config.ICEServer, 0, len(s.cfg.ICEServers))
		for _, server := range s.cfg.ICEServers {
			out = append(out, config.ICEServer{
				URLs:       append([]string(nil), server.URLs...),
				Username:   server.Username,
				Credential: server.Credential,
			})
		}
		return out
	}
	if !s.cfg.TURN.Enabled || s.host == "" {
		return nil
	}

	ttl := s.cfg.TURN.CredentialTTL
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	username := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	hostPort := s.hostPort()
	return []config.ICEServer{{
		URLs: []string{
			"stun:" + hostPort,
			"turn:" + hostPort + "?transport=udp",
			"turn:" + hostPort + "?transport=tcp",
		},
		Username:   username,
		Credential: s.credential(username),
	}}
}

// hostPort is the advertised STUN/TURN endpoint.
func (s *Service) hostPort() string {
	port := s.port
	if port == "" {
		if _, configured, err := net.SplitHostPort(s.cfg.TURN.Listen.Addr); err == nil && configured != "" {
			port = configured
		} else {
			port = "3478"
		}
	}
	return net.JoinHostPort(s.host, port)
}

// authHandler authenticates a TURN request against the credential scheme.
//
// The username is "<unix expiry>": there is no per-user account to look up, so
// a signature over the username is the whole proof. An expired username is
// refused even if its signature is valid.
func (s *Service) authHandler(username, realm string, _ net.Addr) ([]byte, bool) {
	expiryPart, _, _ := strings.Cut(username, ":")
	expiry, err := strconv.ParseInt(expiryPart, 10, 64)
	if err != nil || time.Now().Unix() >= expiry {
		return nil, false
	}
	return turn.GenerateAuthKey(username, realm, s.credential(username)), true
}

// credential is the password for a username: base64(HMAC-SHA1(secret,
// username)), the scheme coturn deployments and WebRTC clients expect.
func (s *Service) credential(username string) string {
	mac := hmac.New(sha1.New, s.secret)
	_, _ = mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// randomSecret returns 32 bytes of randomness for the credential signature.
func randomSecret() []byte {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// A TURN server without a usable secret would issue credentials it
		// cannot verify; failing loudly is safer than starting degraded.
		panic("webrtc: cannot read randomness: " + err.Error())
	}
	return []byte(hex.EncodeToString(buf[:]))
}

// quietLogging keeps Pion to errors; the service logs its own lifecycle.
func quietLogging() logging.LoggerFactory {
	factory := logging.NewDefaultLoggerFactory()
	factory.Writer = io.Discard
	factory.DefaultLogLevel = logging.LogLevelError
	return factory
}

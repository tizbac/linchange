// Package sfu is the server-side media router for meetings.
//
// Every participant opens one PeerConnection to this server, sends its
// microphone and camera once, and receives only what it asked to see. That is
// what turns a mesh of n(n-1) connections into n connections and makes a
// hundred-person room possible in one process.
package sfu

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/linchange/linchange/config"
)

// MaxSubscriptions is the number of video streams a participant may receive:
// cameras and screen shares together. Audio is not counted here; it follows
// speaking, not viewing.
const MaxSubscriptions = 10

// MaxSpeakers is the number of simultaneous non-silent audio streams a room
// forwards. The eleventh is dropped until one of the ten stops speaking.
const MaxSpeakers = 10

const midExtensionURI = "urn:ietf:params:rtp-hdrext:sdes:mid"

// Signal is one message the media server needs to send to a browser. The chat
// hub puts it on the same socket as the peer-to-peer signalling.
type Signal struct {
	Type      string          `json:"type"`
	SDP       json.RawMessage `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
	Streams   []string        `json:"streams,omitempty"`
	Allowed   bool            `json:"allowed,omitempty"`
	Member    string          `json:"member,omitempty"`
}

// Options configures the media server.
type Options struct {
	PublicIP string
	// ICEServers are the STUN/TURN servers the browser is also given. The
	// media server uses the same ones so a call works across a NAT even when
	// neither side has a reachable host candidate.
	ICEServers []config.ICEServer
	Logger     *slog.Logger
}

// Manager owns the rooms and their PeerConnections.
type Manager struct {
	api        *webrtc.API
	iceServers []webrtc.ICEServer
	log        *slog.Logger

	mu    sync.Mutex
	rooms map[string]*room
}

type room struct {
	uid          string
	participants map[string]*participant // member -> connection
	sources      map[string]*source      // "member" (camera), "member:screen", "member:audio"
	allowed      map[string]bool         // members whose audio is among the ten
}

type participant struct {
	room   *room
	member string
	pc     *webrtc.PeerConnection
	send   func(Signal)

	mu         sync.Mutex
	streams    map[string]bool
	videos     int // first video is the camera, second is the screen
	videoSinks map[string]*sink
	closed     bool
}

type sink struct {
	track       *webrtc.TrackLocalStaticRTP
	sender      *webrtc.RTPSender
	transceiver *webrtc.RTPTransceiver
	midID       uint8
	midChecked  bool
}

type source struct {
	key    string
	member string
	screen bool
	audio  bool
	track  *webrtc.TrackRemote
	codec  webrtc.RTPCodecCapability
	// pc is the publisher's connection. A new subscriber needs a keyframe
	// now, not whenever the camera happens to produce the next one.
	pc *webrtc.PeerConnection

	mu      sync.Mutex
	manager *Manager
	room    *room
	// sinks is keyed by subscriber member. Each subscriber gets its own local
	// track because two senders cannot share one TrackLocal safely.
	sinks map[string]*sink
}

// New builds a media server. A public IP is used as the host candidate when
// the process is behind NAT; without one, LAN and same-host deployments use
// the interface addresses Pion discovers.
func New(opts Options) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	settings := webrtc.SettingEngine{}
	if ip := strings.TrimSpace(opts.PublicIP); ip != "" {
		settings.SetNAT1To1IPs([]string{ip}, webrtc.ICECandidateTypeHost)
	}
	ice := make([]webrtc.ICEServer, 0, len(opts.ICEServers))
	for _, server := range opts.ICEServers {
		ice = append(ice, webrtc.ICEServer{
			URLs:       append([]string(nil), server.URLs...),
			Username:   server.Username,
			Credential: server.Credential,
		})
	}
	return &Manager{
		api:        webrtc.NewAPI(webrtc.WithSettingEngine(settings)),
		iceServers: ice,
		log:        log,
		rooms:      make(map[string]*room),
	}
}

func (m *Manager) room(uid string) *room {
	r := m.rooms[uid]
	if r == nil {
		r = &room{
			uid:          uid,
			participants: make(map[string]*participant),
			sources:      make(map[string]*source),
			allowed:      make(map[string]bool),
		}
		m.rooms[uid] = r
	}
	return r
}

// Join adds a browser to a room, creates its PeerConnection and sends the
// first offer. The answer and later ICE candidates arrive through Answer and
// AddCandidate.
func (m *Manager) Join(uid, member string, send func(Signal)) error {
	m.mu.Lock()
	r := m.room(uid)
	if old := r.participants[member]; old != nil {
		old.close()
		delete(r.participants, member)
	}
	m.mu.Unlock()

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{ICEServers: m.iceServers})
	if err != nil {
		return fmt.Errorf("sfu: creating peer connection: %w", err)
	}
	p := &participant{
		room:       r,
		member:     member,
		pc:         pc,
		send:       send,
		streams:    make(map[string]bool),
		videoSinks: make(map[string]*sink),
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		raw, err := json.Marshal(c.ToJSON())
		if err == nil {
			send(Signal{Type: "sfu-ice", Candidate: raw})
		}
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		m.onTrack(r, p, track)
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			m.Leave(uid, member)
		}
	})

	// Three receive lines: microphone, camera and screen share. The client
	// answers with its local tracks on these.
	for _, kind := range []webrtc.RTPCodecType{
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPCodecTypeVideo,
	} {
		if _, err := pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			pc.Close()
			return fmt.Errorf("sfu: adding receive line: %w", err)
		}
	}

	m.mu.Lock()
	r.participants[member] = p
	m.mu.Unlock()
	if err := m.offer(p); err != nil {
		m.Leave(uid, member)
		return err
	}
	// A joiner needs the streams that already exist, not only the ones that
	// appear after it arrived.
	m.broadcastSources(r)
	return nil
}

// Answer applies the browser's answer to the current offer.
func (m *Manager) Answer(uid, member string, raw json.RawMessage) error {
	m.mu.Lock()
	r := m.rooms[uid]
	m.mu.Unlock()
	if r == nil {
		return fmt.Errorf("sfu: no such room")
	}
	m.mu.Lock()
	p := r.participants[member]
	m.mu.Unlock()
	if p == nil {
		return fmt.Errorf("sfu: no such participant")
	}
	var desc webrtc.SessionDescription
	if err := json.Unmarshal(raw, &desc); err != nil {
		return fmt.Errorf("sfu: decoding answer: %w", err)
	}
	return p.pc.SetRemoteDescription(desc)
}

// AddCandidate applies one ICE candidate from the browser.
func (m *Manager) AddCandidate(uid, member string, raw json.RawMessage) error {
	m.mu.Lock()
	r := m.rooms[uid]
	m.mu.Unlock()
	if r == nil {
		return fmt.Errorf("sfu: no such room")
	}
	m.mu.Lock()
	p := r.participants[member]
	m.mu.Unlock()
	if p == nil {
		return nil
	}
	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal(raw, &candidate); err != nil {
		return fmt.Errorf("sfu: decoding candidate: %w", err)
	}
	return p.pc.AddICECandidate(candidate)
}

// Subscribe selects the video streams this participant wants to receive.
// Audio is not part of the list: it follows speaking.
func (m *Manager) Subscribe(uid, member string, streams []string) {
	m.mu.Lock()
	r := m.rooms[uid]
	m.mu.Unlock()
	if r == nil {
		return
	}
	m.mu.Lock()
	p := r.participants[member]
	m.mu.Unlock()
	if p == nil {
		return
	}

	m.mu.Lock()
	wanted := make(map[string]bool, MaxSubscriptions)
	for _, key := range streams {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" || len(wanted) >= MaxSubscriptions {
			continue
		}
		if src := r.sources[key]; src != nil && !src.audio {
			wanted[key] = true
		}
	}
	m.mu.Unlock()
	p.mu.Lock()
	p.streams = wanted
	p.mu.Unlock()
	m.reconcileVideo(r, p)
}

// Speaking marks a member as non-silent or silent. Only ten may be marked at
// once; the eleventh is refused, and its browser is told to stop sending.
func (m *Manager) Speaking(uid, member string, speaking bool) {
	m.mu.Lock()
	r := m.rooms[uid]
	m.mu.Unlock()
	if r == nil {
		return
	}
	m.mu.Lock()
	p := r.participants[member]
	if p == nil {
		m.mu.Unlock()
		return
	}
	allowed := r.allowed[member]
	if speaking && !allowed {
		if len(r.allowed) >= MaxSpeakers {
			m.mu.Unlock()
			p.send(Signal{Type: "sfu-audio", Allowed: false})
			return
		}
		r.allowed[member] = true
		allowed = true
	}
	if !speaking && allowed {
		delete(r.allowed, member)
	}
	m.mu.Unlock()
	p.send(Signal{Type: "sfu-audio", Allowed: speaking})
}

// Leave removes a participant and its published tracks.
func (m *Manager) Leave(uid, member string) {
	m.mu.Lock()
	r := m.rooms[uid]
	if r == nil {
		m.mu.Unlock()
		return
	}
	p := r.participants[member]
	delete(r.participants, member)
	delete(r.allowed, member)
	for key, src := range r.sources {
		if src.member == member {
			src.close()
			delete(r.sources, key)
		}
	}
	if len(r.participants) == 0 {
		delete(m.rooms, uid)
	}
	m.mu.Unlock()
	if p != nil {
		p.close()
	}
}

// Participants returns the live members of a room, sorted.
func (m *Manager) Participants(uid string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[uid]
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.participants))
	for member := range r.participants {
		out = append(out, member)
	}
	sort.Strings(out)
	return out
}

// SourceKeys returns the currently published video sources ("member" for a
// camera, "member:screen" for a share). It is sent to the room so each client
// can subscribe by name rather than by guessing what exists.
func (m *Manager) SourceKeys(uid string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[uid]
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.sources))
	for key, src := range r.sources {
		if !src.audio {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func (m *Manager) offer(p *participant) error {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("sfu: creating offer: %w", err)
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("sfu: setting local description: %w", err)
	}
	raw, err := json.Marshal(offer)
	if err != nil {
		return err
	}
	p.send(Signal{Type: "sfu-offer", SDP: raw})
	return nil
}

func (m *Manager) onTrack(r *room, p *participant, track *webrtc.TrackRemote) {
	if track == nil || track.Kind() == webrtc.RTPCodecTypeUnknown {
		return
	}
	audio := track.Kind() == webrtc.RTPCodecTypeAudio
	key := p.member
	screen := false
	if audio {
		key = p.member + ":audio"
	} else {
		p.mu.Lock()
		if p.videos > 0 {
			screen = true
			key = p.member + ":screen"
		}
		p.videos++
		p.mu.Unlock()
	}
	if !audio && !screen {
		key = p.member
	}

	codec := track.Codec()
	capability := webrtc.RTPCodecCapability{
		MimeType:     codec.MimeType,
		ClockRate:    codec.ClockRate,
		Channels:     codec.Channels,
		SDPFmtpLine:  codec.SDPFmtpLine,
		RTCPFeedback: nil,
	}
	src := &source{
		key:     key,
		member:  p.member,
		screen:  screen,
		audio:   audio,
		track:   track,
		codec:   capability,
		pc:      p.pc,
		manager: m,
		room:    r,
		sinks:   make(map[string]*sink),
	}

	m.mu.Lock()
	if old := r.sources[key]; old != nil {
		old.close()
	}
	r.sources[key] = src
	participants := make([]*participant, 0, len(r.participants))
	for _, sub := range r.participants {
		participants = append(participants, sub)
	}
	m.mu.Unlock()

	go src.readLoop()
	if !audio {
		for _, sub := range participants {
			m.reconcileVideo(r, sub)
		}
	} else {
		for _, sub := range participants {
			if sub.member == src.member {
				continue
			}
			if m.addSourceToParticipant(src, sub) {
				if err := m.offer(sub); err != nil {
					m.log.Warn("sfu: offer after audio track failed", "member", sub.member, "error", err)
				}
			}
		}
	}
	m.broadcastSources(r)
}

func (m *Manager) reconcileVideo(r *room, p *participant) {
	m.mu.Lock()
	sources := make([]*source, 0, len(r.sources))
	for _, src := range r.sources {
		sources = append(sources, src)
	}
	m.mu.Unlock()

	p.mu.Lock()
	wanted := make(map[string]bool, len(p.streams))
	for key := range p.streams {
		wanted[key] = true
	}
	p.mu.Unlock()

	changed := false
	for _, src := range sources {
		if src.audio || src.member == p.member {
			continue
		}
		if wanted[src.key] {
			changed = m.addSourceToParticipant(src, p) || changed
		} else {
			changed = m.removeSourceFromParticipant(src, p) || changed
		}
	}
	if changed {
		if err := m.offer(p); err != nil {
			m.log.Warn("sfu: offer after subscription failed", "member", p.member, "error", err)
		}
	}
}

func (m *Manager) addSourceToParticipant(src *source, p *participant) bool {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	if _, exists := p.videoSinks[src.key]; exists {
		p.mu.Unlock()
		return false
	}
	p.mu.Unlock()

	streamID := src.member
	if src.screen {
		streamID = src.member + ":screen"
	}
	track, err := webrtc.NewTrackLocalStaticRTP(src.codec, src.key, streamID)
	if err != nil {
		m.log.Warn("sfu: creating local track failed", "key", src.key, "error", err)
		return false
	}
	transceiver, err := p.pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	})
	if err != nil {
		m.log.Warn("sfu: adding track failed", "member", p.member, "key", src.key, "error", err)
		return false
	}
	sender := transceiver.Sender()
	// Drain RTCP so Pion's interceptors keep sender reports flowing.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()

	out := &sink{track: track, sender: sender, transceiver: transceiver}
	p.mu.Lock()
	p.videoSinks[src.key] = out
	p.mu.Unlock()
	src.mu.Lock()
	src.sinks[p.member] = out
	src.mu.Unlock()
	if !src.audio {
		// A subscriber joins mid-stream and cannot decode until the camera
		// produces a keyframe. Ask more than once: the first request can race
		// the sender's encoder setup, and fake or mobile encoders sometimes
		// ignore a request that arrives too early.
		go func() {
			for attempt := 0; attempt < 10; attempt++ {
				if err := src.pc.WriteRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{MediaSSRC: uint32(src.track.SSRC())},
				}); err != nil {
					m.log.Warn("sfu: requesting keyframe failed", "source", src.key, "error", err)
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
		}()
	}
	return true
}

func (m *Manager) removeSourceFromParticipant(src *source, p *participant) bool {
	p.mu.Lock()
	sink := p.videoSinks[src.key]
	delete(p.videoSinks, src.key)
	p.mu.Unlock()
	if sink != nil {
		_ = p.pc.RemoveTrack(sink.sender)
	}
	src.mu.Lock()
	delete(src.sinks, p.member)
	src.mu.Unlock()
	return sink != nil
}

func (m *Manager) broadcastSources(r *room) {
	m.mu.Lock()
	keys := make([]string, 0, len(r.sources))
	for key, src := range r.sources {
		if !src.audio {
			keys = append(keys, key)
		}
	}
	members := make([]*participant, 0, len(r.participants))
	for _, p := range r.participants {
		members = append(members, p)
	}
	m.mu.Unlock()

	sort.Strings(keys)
	for _, p := range members {
		p.send(Signal{Type: "sfu-sources", Streams: keys})
	}
}

func (s *source) readLoop() {
	for {
		packet, _, err := s.track.ReadRTP()
		if err != nil {
			break
		}
		if s.audio {
			s.manager.mu.Lock()
			allowed := s.room.allowed[s.member]
			s.manager.mu.Unlock()
			if !allowed {
				continue
			}
		}
		s.mu.Lock()
		sinks := make([]*sink, 0, len(s.sinks))
		for _, sink := range s.sinks {
			sinks = append(sinks, sink)
		}
		s.mu.Unlock()
		for _, sink := range sinks {
			// The publisher's MID names its own m-line. The subscriber's
			// m-line has a different MID, and with BUNDLE the browser routes
			// RTP by that extension. SSRC and payload type are rewritten by
			// TrackLocalStaticRTP; the MID has to be rewritten here.
			if !sink.midChecked {
				sink.midChecked = true
				for _, ext := range sink.sender.GetParameters().HeaderExtensions {
					if ext.URI == midExtensionURI {
						sink.midID = uint8(ext.ID)
						break
					}
				}
			}
			forward := *packet
			forward.Header = packet.Header.Clone()
			forward.Header.ClearExtensions()
			if sink.midID != 0 {
				if mid := sink.transceiver.Mid(); mid != "" {
					_ = forward.Header.SetExtension(sink.midID, []byte(mid))
				}
			}
			if err := sink.track.WriteRTP(&forward); err != nil {
				s.manager.log.Warn("sfu: write rtp failed", "source", s.key, "error", err)
			}
		}
	}
}

func (s *source) close() {
	s.mu.Lock()
	s.sinks = make(map[string]*sink)
	s.mu.Unlock()
}

func (p *participant) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	_ = p.pc.Close()
}

// Close stops every room.
func (m *Manager) Close() {
	m.mu.Lock()
	rooms := make([]*room, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	m.rooms = make(map[string]*room)
	m.mu.Unlock()
	for _, r := range rooms {
		for _, p := range r.participants {
			p.close()
		}
	}
}

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Hub carries live chat over WebSocket: messages, typing, read receipts, and
// the signalling a meeting's WebRTC call needs.
//
// It is deliberately thin. Anything durable goes through Service and the
// database first, and only then is the resulting event fanned out; a socket
// that drops must never be the reason a message was lost. Typing and call
// membership are the exceptions: they describe a moment, not a fact, so they
// live in memory and disappear with the connection.
type Hub struct {
	svc *Service
	log *slog.Logger

	mu      sync.RWMutex
	clients map[string]map[*client]struct{} // member -> live connections
	calls   map[int64]*callRoom             // conversation id -> live call
	// grants remembers which attendee a one-way meeting's presenter has let
	// speak, and hands remembers who has asked. Both describe a moment, so
	// they live with the connections rather than in the database.
	grants map[int64]string
	hands  map[int64]map[string]struct{}
}

// callRoom is the set of members currently in one meeting's call, kept with the
// room's public uid so a departure can be announced without a database round
// trip.
type callRoom struct {
	uid     string
	members map[string]struct{}
}

// client is one live socket.
type client struct {
	hub    *Hub
	member string
	conn   *websocket.Conn

	// writeMu serialises writes: a WebSocket connection permits only one
	// writer at a time, and broadcasts arrive from other clients' goroutines.
	writeMu sync.Mutex
}

// frame is one client-to-server message.
type frame struct {
	Type         string          `json:"type"`
	Conversation string          `json:"conversation,omitempty"`
	Body         string          `json:"body,omitempty"`
	MessageID    int64           `json:"messageId,omitempty"`
	To           string          `json:"to,omitempty"`
	SDP          json.RawMessage `json:"sdp,omitempty"`
	Candidate    json.RawMessage `json:"candidate,omitempty"`
	ClientID     string          `json:"clientId,omitempty"`
}

// outFrame is one server-to-client message. One struct serves every event type
// so the wire shape stays predictable as events are added.
type outFrame struct {
	Type          string          `json:"type"`
	Conversation  string          `json:"conversation,omitempty"`
	Message       *Message        `json:"message,omitempty"`
	Member        string          `json:"member,omitempty"`
	From          string          `json:"from,omitempty"`
	MessageID     int64           `json:"messageId,omitempty"`
	Peers         []string        `json:"peers,omitempty"`
	SDP           json.RawMessage `json:"sdp,omitempty"`
	Candidate     json.RawMessage `json:"candidate,omitempty"`
	ClientID      string          `json:"clientId,omitempty"`
	Error         string          `json:"error,omitempty"`
	Conversations []Summary       `json:"conversations,omitempty"`
	// Allowed is set on an allow-speak frame: true grants the floor, false
	// takes it back. Speaker names who currently holds it, if anyone.
	Allowed *bool    `json:"allowed,omitempty"`
	Speaker string   `json:"speaker,omitempty"`
	Hands   []string `json:"hands,omitempty"`
}

// NewHub builds the hub over a chat service.
func NewHub(svc *Service, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		svc:     svc,
		log:     log,
		clients: make(map[string]map[*client]struct{}),
		calls:   make(map[int64]*callRoom),
		grants:  make(map[int64]string),
		hands:   make(map[int64]map[string]struct{}),
	}
}

// Service returns the chat service the hub writes through.
func (h *Hub) Service() *Service { return h.svc }

// Accept upgrades an authenticated request and serves it until the socket
// closes. The caller has already established who the member is.
func (h *Hub) Accept(w http.ResponseWriter, r *http.Request, member string) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The identity is a bearer token, not an ambient cookie, so a
		// cross-origin request carries no authority a same-origin one lacks.
		// Origin checking would only turn a working reverse proxy into an
		// outage.
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.log.Warn("chat: websocket upgrade failed", "error", err)
		return
	}
	h.Serve(r.Context(), member, conn)
}

// Serve runs one connection: it announces the member, then reads frames until
// the socket closes.
func (h *Hub) Serve(ctx context.Context, member string, conn *websocket.Conn) {
	member = normalise(member)
	c := &client{hub: h, member: member, conn: conn}

	h.add(c)
	defer h.remove(c)

	conversations, err := h.svc.ListFor(ctx, member)
	if err != nil {
		h.log.Warn("chat: loading conversations for a new socket failed", "member", member, "error", err)
	}
	if err := c.send(outFrame{
		Type:          "hello",
		Member:        member,
		Conversations: conversations,
	}); err != nil {
		return
	}

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var in frame
		if err := json.Unmarshal(raw, &in); err != nil {
			_ = c.send(outFrame{Type: "error", Error: "malformed frame"})
			continue
		}
		if err := h.handle(ctx, c, in); err != nil {
			_ = c.send(outFrame{
				Type:         "error",
				Conversation: in.Conversation,
				Error:        err.Error(),
			})
		}
	}
}

// handle dispatches one frame.
func (h *Hub) handle(ctx context.Context, c *client, in frame) error {
	switch strings.ToLower(strings.TrimSpace(in.Type)) {
	case "ping":
		return c.send(outFrame{Type: "pong"})
	case "send":
		return h.sendMessage(ctx, c, in)
	case "typing":
		return h.typing(ctx, c, in)
	case "read":
		return h.markRead(ctx, c, in)
	case "rtc-join":
		return h.joinCall(ctx, c, in)
	case "rtc-leave":
		return h.leaveCall(ctx, c, in)
	case "rtc-offer", "rtc-answer", "rtc-ice":
		return h.relay(ctx, c, in)
	case "hand-raise":
		return h.raiseHand(ctx, c, in, true)
	case "hand-lower":
		return h.raiseHand(ctx, c, in, false)
	case "screen-start", "screen-stop":
		return h.screenShare(ctx, c, in)
	case "allow-speak":
		return h.grantFloor(ctx, c, in, true)
	case "revoke-speak":
		return h.grantFloor(ctx, c, in, false)
	default:
		return errors.New("unknown frame type")
	}
}

// conversation resolves the frame's conversation and confirms the client may
// use it. Every durable operation starts here, so no path can act on a room the
// member is not in.
func (h *Hub) conversation(ctx context.Context, c *client, uid string) (Conversation, error) {
	conv, err := h.svc.ByUID(ctx, h.svc.db, uid)
	if err != nil {
		return Conversation{}, errors.New("no such conversation")
	}
	member, err := h.svc.IsMember(ctx, conv.ID, c.member)
	if err != nil {
		return Conversation{}, errors.New("could not check membership")
	}
	if !member {
		return Conversation{}, errors.New("not a member of this conversation")
	}
	return conv, nil
}

func (h *Hub) sendMessage(ctx context.Context, c *client, in frame) error {
	conv, err := h.conversation(ctx, c, in.Conversation)
	if err != nil {
		return err
	}
	_, err = h.SendMessage(ctx, conv, c.member, in.Body, in.ClientID)
	return err
}

// SendMessage stores a message and fans it out to the room. It is the one path
// a message takes, whether it arrived over a socket or over HTTP, so a client
// reading the socket sees an HTTP-sent message just like any other.
func (h *Hub) SendMessage(ctx context.Context, conv Conversation, sender, body, clientID string) (Message, error) {
	msg, err := h.svc.Append(ctx, conv.ID, sender, body)
	if err != nil {
		return Message{}, err
	}
	members, err := h.svc.MemberAddresses(ctx, conv.ID)
	if err != nil {
		return Message{}, errors.New("could not list the room's members")
	}
	h.broadcast(members, outFrame{
		Type:         "message",
		Conversation: conv.UID,
		Message:      &msg,
		ClientID:     clientID,
	})
	return msg, nil
}

func (h *Hub) typing(ctx context.Context, c *client, in frame) error {
	conv, err := h.conversation(ctx, c, in.Conversation)
	if err != nil {
		return err
	}
	members, err := h.svc.MemberAddresses(ctx, conv.ID)
	if err != nil {
		return errors.New("could not list the room's members")
	}
	h.broadcast(members, outFrame{
		Type:         "typing",
		Conversation: conv.UID,
		Member:       c.member,
	}, c.member)
	return nil
}

func (h *Hub) markRead(ctx context.Context, c *client, in frame) error {
	conv, err := h.conversation(ctx, c, in.Conversation)
	if err != nil {
		return err
	}
	if err := h.svc.MarkRead(ctx, conv.ID, c.member, in.MessageID); err != nil {
		return err
	}
	members, err := h.svc.MemberAddresses(ctx, conv.ID)
	if err != nil {
		return errors.New("could not list the room's members")
	}
	h.broadcast(members, outFrame{
		Type:         "read",
		Conversation: conv.UID,
		Member:       c.member,
		MessageID:    in.MessageID,
	})
	return nil
}

// joinCall puts the member into the meeting's call and tells them who is
// already there. The list is the peers they must open a WebRTC connection to;
// the server only introduces them and relays the signalling.
func (h *Hub) joinCall(ctx context.Context, c *client, in frame) error {
	conv, err := h.conversation(ctx, c, in.Conversation)
	if err != nil {
		return err
	}
	if conv.Kind != KindMeeting {
		return errors.New("calls are only available in meeting rooms")
	}

	h.mu.Lock()
	room := h.calls[conv.ID]
	if room == nil {
		room = &callRoom{uid: conv.UID, members: make(map[string]struct{})}
		h.calls[conv.ID] = room
	}
	room.members[c.member] = struct{}{}
	others := make([]string, 0, len(room.members))
	for member := range room.members {
		if member != c.member {
			others = append(others, member)
		}
	}
	h.mu.Unlock()

	if err := c.send(outFrame{
		Type:         "rtc-peers",
		Conversation: conv.UID,
		Peers:        others,
	}); err != nil {
		return err
	}

	// Hand/floor state is a moment, not history, so a late joiner is told
	// what it is now rather than replaying every raise that came before.
	h.mu.RLock()
	hands := make([]string, 0, len(h.hands[conv.ID]))
	for member := range h.hands[conv.ID] {
		hands = append(hands, member)
	}
	speaker := h.grants[conv.ID]
	h.mu.RUnlock()
	if err := c.send(outFrame{
		Type:         "hands",
		Conversation: conv.UID,
		Hands:        hands,
		Speaker:      speaker,
	}); err != nil {
		return err
	}

	h.broadcast(others, outFrame{
		Type:         "rtc-join",
		Conversation: conv.UID,
		Member:       c.member,
	})
	return nil
}

func (h *Hub) leaveCall(ctx context.Context, c *client, in frame) error {
	conv, err := h.conversation(ctx, c, in.Conversation)
	if err != nil {
		return err
	}
	h.dropFromCall(conv.ID, c.member)
	return nil
}

// dropFromCall removes the member and tells the rest of the call.
func (h *Hub) dropFromCall(conversationID int64, member string) {
	h.mu.Lock()
	room := h.calls[conversationID]
	if room == nil {
		h.mu.Unlock()
		return
	}
	delete(room.members, member)
	remaining := make([]string, 0, len(room.members))
	for other := range room.members {
		remaining = append(remaining, other)
	}
	uid := room.uid
	// A member who leaves gives up the floor and any raised hand: neither
	// outlives the connection that held it.
	revoked := h.grants[conversationID] == member
	if revoked {
		delete(h.grants, conversationID)
	}
	if hands := h.hands[conversationID]; hands != nil {
		delete(hands, member)
	}
	if len(room.members) == 0 {
		delete(h.calls, conversationID)
		delete(h.hands, conversationID)
		delete(h.grants, conversationID)
	}
	h.mu.Unlock()

	h.broadcast(remaining, outFrame{
		Type:         "rtc-leave",
		Conversation: uid,
		Member:       member,
	})
	if revoked {
		h.broadcast(remaining, outFrame{
			Type:         "speaker",
			Conversation: uid,
			Speaker:      "",
		})
	}
}

// relay carries one WebRTC signalling frame to one other member of the room.
//
// The server does not inspect the SDP or the candidate: it checks that both
// ends belong to the conversation, adds who it is from, and passes it on. That
// keeps media negotiation in the browsers while the room's membership stays the
// authorisation.
func (h *Hub) relay(ctx context.Context, c *client, in frame) error {
	conv, err := h.conversation(ctx, c, in.Conversation)
	if err != nil {
		return err
	}
	to := normalise(in.To)
	if to == "" {
		return errors.New("a signalling frame needs a recipient")
	}
	member, err := h.svc.IsMember(ctx, conv.ID, to)
	if err != nil {
		return errors.New("could not check membership")
	}
	if !member {
		return errors.New("the recipient is not in this conversation")
	}

	delivered := h.sendTo(to, outFrame{
		Type:         strings.ToLower(strings.TrimSpace(in.Type)),
		Conversation: conv.UID,
		From:         c.member,
		SDP:          in.SDP,
		Candidate:    in.Candidate,
	})
	if !delivered {
		return errors.New("the recipient is not connected")
	}
	return nil
}

// raiseHand asks for the floor in a one-way meeting, or withdraws the request.
//
// It is broadcast to the room so the presenter (and everybody else) can see
// who is waiting to speak; the grant itself is a separate, presenter-only act.
func (h *Hub) raiseHand(ctx context.Context, c *client, in frame, up bool) error {
	conv, err := h.conversation(ctx, c, in.Conversation)
	if err != nil {
		return err
	}
	if !conv.OneWay {
		return errors.New("hand raising is only for one-way meetings")
	}

	h.mu.Lock()
	hands := h.hands[conv.ID]
	if hands == nil {
		hands = make(map[string]struct{})
		h.hands[conv.ID] = hands
	}
	if up {
		hands[c.member] = struct{}{}
	} else {
		delete(hands, c.member)
	}
	h.mu.Unlock()

	members, err := h.svc.MemberAddresses(ctx, conv.ID)
	if err != nil {
		return errors.New("could not list the room's members")
	}
	kind := "hand-lower"
	if up {
		kind = "hand-raise"
	}
	h.broadcast(members, outFrame{
		Type:         kind,
		Conversation: conv.UID,
		Member:       c.member,
	})
	return nil
}

// screenShare tells the room that one participant started or stopped sharing
// a screen. In a webinar only the presenter may share; elsewhere anyone can,
// and the media itself is the separate screen m-line of the call.
func (h *Hub) screenShare(ctx context.Context, c *client, in frame) error {
	conv, err := h.conversation(ctx, c, in.Conversation)
	if err != nil {
		return err
	}
	if in.Type == "screen-start" && conv.OneWay && normalise(conv.Presenter) != c.member {
		return errors.New("only the presenter can share a screen in a webinar")
	}
	members, err := h.svc.MemberAddresses(ctx, conv.ID)
	if err != nil {
		return errors.New("could not list the room's members")
	}
	h.broadcast(members, outFrame{
		Type:         strings.ToLower(strings.TrimSpace(in.Type)),
		Conversation: conv.UID,
		Member:       c.member,
	})
	return nil
}

// grantFloor lets one attendee speak, or takes the floor back. Only the
// presenter may call it, and only one guest holds the floor at a time: granting
// it to somebody new revokes it from whoever held it before.
func (h *Hub) grantFloor(ctx context.Context, c *client, in frame, allow bool) error {
	conv, err := h.conversation(ctx, c, in.Conversation)
	if err != nil {
		return err
	}
	if !conv.OneWay {
		return errors.New("only one-way meetings grant the floor")
	}
	if normalise(conv.Presenter) != c.member {
		return errors.New("only the presenter can grant the floor")
	}
	to := normalise(in.To)
	if to == "" {
		return errors.New("a floor grant needs a recipient")
	}
	member, err := h.svc.IsMember(ctx, conv.ID, to)
	if err != nil {
		return errors.New("could not check membership")
	}
	if !member {
		return errors.New("the recipient is not in this conversation")
	}

	h.mu.Lock()
	previous := h.grants[conv.ID]
	if allow {
		h.grants[conv.ID] = to
	} else if previous == to {
		delete(h.grants, conv.ID)
	}
	if hands := h.hands[conv.ID]; hands != nil {
		delete(hands, to)
	}
	h.mu.Unlock()

	yes := allow
	h.sendTo(to, outFrame{
		Type:         "allow-speak",
		Conversation: conv.UID,
		Member:       to,
		Allowed:      &yes,
	})
	if allow && previous != "" && previous != to {
		no := false
		h.sendTo(previous, outFrame{
			Type:         "allow-speak",
			Conversation: conv.UID,
			Member:       previous,
			Allowed:      &no,
		})
	}

	speaker := ""
	if allow {
		speaker = to
	}
	members, err := h.svc.MemberAddresses(ctx, conv.ID)
	if err != nil {
		return errors.New("could not list the room's members")
	}
	h.broadcast(members, outFrame{
		Type:         "speaker",
		Conversation: conv.UID,
		Speaker:      speaker,
	})
	return nil
}

// add registers a connection.
func (h *Hub) add(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[c.member] == nil {
		h.clients[c.member] = make(map[*client]struct{})
	}
	h.clients[c.member][c] = struct{}{}
}

// remove unregisters a connection, closing any call it was in so the remaining
// participants learn about the departure rather than waiting on a peer that
// will never answer.
func (h *Hub) remove(c *client) {
	h.mu.Lock()
	conns := h.clients[c.member]
	delete(conns, c)
	if len(conns) == 0 {
		delete(h.clients, c.member)
	}
	// Collect the calls to leave only if this was the member's last socket:
	// a second tab must not end the call the first one is still in.
	lastSocket := len(conns) == 0
	var conversations []int64
	if lastSocket {
		for id, room := range h.calls {
			if _, ok := room.members[c.member]; ok {
				conversations = append(conversations, id)
			}
		}
	}
	h.mu.Unlock()

	for _, id := range conversations {
		h.dropFromCall(id, c.member)
	}
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

// sendTo delivers a frame to every live socket of one member and reports
// whether at least one accepted it.
func (h *Hub) sendTo(member string, out outFrame) bool {
	h.mu.RLock()
	conns := make([]*client, 0, len(h.clients[member]))
	for c := range h.clients[member] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	delivered := false
	for _, c := range conns {
		if err := c.send(out); err == nil {
			delivered = true
		}
	}
	return delivered
}

// broadcast delivers a frame to the members, optionally skipping one. The
// skip is what keeps a sender from receiving its own typing indicator.
func (h *Hub) broadcast(members []string, out outFrame, skip ...string) {
	skipSet := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipSet[s] = true
	}
	for _, member := range members {
		if skipSet[member] {
			continue
		}
		h.sendTo(member, out)
	}
}

// send writes one frame, serialised with every other write on the connection.
func (c *client) send(out outFrame) error {
	payload, err := json.Marshal(out)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(ctx, websocket.MessageText, payload)
}

// CallMembers reports who is currently in a meeting's call. It is exported for
// tests and for a future presence view; the socket path does not need it.
func (h *Hub) CallMembers(conversationID int64) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	room := h.calls[conversationID]
	if room == nil {
		return nil
	}
	out := make([]string, 0, len(room.members))
	for member := range room.members {
		out = append(out, member)
	}
	return out
}

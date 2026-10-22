package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/auth/token"
	"github.com/linchange/linchange/internal/chat"
)

// maxChatBody bounds a JSON chat request. A message is capped at 16 KiB by the
// service, so this leaves room for a create request's member list as well.
const maxChatBody = 64 << 10

// mountChat adds the native chat routes.
//
// The REST surface lists conversations and reads history, which is what a page
// needs on first paint. Everything live - messages, typing, receipts, WebRTC
// signalling - travels over the socket at /chat/ws.
func (s *Server) mountChat(r chi.Router) {
	r.Route("/chat", func(r chi.Router) {
		// The socket is mounted outside requireAuth because a browser cannot
		// set an Authorization header on a WebSocket. It authenticates the
		// access token from the query string itself, below.
		r.Get("/ws", s.handleChatSocket)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Get("/conversations", s.handleListConversations)
			r.Post("/conversations", s.handleCreateConversation)
			r.Get("/conversations/{uid}/messages", s.handleChatHistory)
			r.Post("/conversations/{uid}/messages", s.handleSendChatMessage)
			r.Get("/conversations/{uid}/members", s.handleChatMembers)
			r.Post("/conversations/{uid}/read", s.handleMarkConversationRead)
		})
	})
}

// handleChatSocket upgrades an authenticated request to the chat socket.
//
// The token arrives in the query string because the browser's WebSocket API
// offers no way to send a header. It carries the same weight as a bearer token
// anywhere else: it is parsed as an access token and checked against a live
// session before the upgrade, so a revoked sign-in cannot open a socket.
func (s *Server) handleChatSocket(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("access_token")
	if raw == "" {
		raw = r.URL.Query().Get("token")
	}
	if raw == "" {
		s.writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	principal, sessionID, err := s.tokens.Parse(raw, token.TypeAccess)
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if _, err := s.sessions.Load(r.Context(), s.db, sessionID); err != nil {
		s.writeError(w, http.StatusUnauthorized, "this sign-in is no longer active")
		return
	}
	s.chatHub.Accept(w, r, principal.Address)
}

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	conversations, err := s.chat.ListFor(r.Context(), Principal(r.Context()).Address)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not list conversations")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"conversations": conversations})
}

// createConversationRequest opens a direct, ad-hoc group, or organization
// group conversation. Meeting rooms are opened by the meetings service when the
// meeting is created, not here.
type createConversationRequest struct {
	Kind    string   `json:"kind"`
	Title   string   `json:"title"`
	Members []string `json:"members"`
	// GroupID opens the room of an existing organization group; its members are
	// the group's.
	GroupID int64 `json:"groupId"`
}

func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	var in createConversationRequest
	if err := decodeChatJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	conv, err := s.chat.Create(r.Context(), s.db, chat.CreateRequest{
		Kind:      in.Kind,
		Title:     in.Title,
		Members:   in.Members,
		CreatedBy: Principal(r.Context()).Address,
		GroupID:   in.GroupID,
	})
	if err != nil {
		s.writeChatError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, conv)
}

func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.chatConversationFor(w, r)
	if !ok {
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	messages, err := s.chat.History(r.Context(), conv.ID, before, limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not load the conversation")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"conversation": conv,
		"messages":     messages,
	})
}

// handleSendChatMessage stores a message over HTTP, for a client that is not
// holding a socket open. It takes the same path as a socket message, so a
// browser in the room receives it live.
func (s *Server) handleSendChatMessage(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.chatConversationFor(w, r)
	if !ok {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := decodeChatJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	msg, err := s.chatHub.SendMessage(r.Context(), conv, Principal(r.Context()).Address, in.Body, "")
	if err != nil {
		s.writeChatError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, msg)
}

func (s *Server) handleChatMembers(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.chatConversationFor(w, r)
	if !ok {
		return
	}
	members, err := s.chat.Members(r.Context(), conv.ID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not load the members")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

func (s *Server) handleMarkConversationRead(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.chatConversationFor(w, r)
	if !ok {
		return
	}
	var in struct {
		MessageID int64 `json:"messageId"`
	}
	if err := decodeChatJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}

	address := Principal(r.Context()).Address
	var err error
	if in.MessageID > 0 {
		err = s.chat.MarkRead(r.Context(), conv.ID, address, in.MessageID)
	} else {
		_, err = s.chat.MarkAllRead(r.Context(), conv.ID, address)
	}
	if err != nil {
		s.writeChatError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// chatConversationFor resolves the conversation in the path and confirms the
// caller is a member. A non-member and a missing room answer the same way, so a
// caller cannot enumerate valid room ids.
func (s *Server) chatConversationFor(w http.ResponseWriter, r *http.Request) (chat.Conversation, bool) {
	conv, err := s.chat.ByUID(r.Context(), s.db, chi.URLParam(r, "uid"))
	if err != nil {
		s.writeError(w, http.StatusNotFound, "no such conversation")
		return chat.Conversation{}, false
	}
	member, err := s.chat.IsMember(r.Context(), conv.ID, Principal(r.Context()).Address)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not check the conversation")
		return chat.Conversation{}, false
	}
	if !member {
		s.writeError(w, http.StatusNotFound, "no such conversation")
		return chat.Conversation{}, false
	}
	return conv, true
}

func (s *Server) writeChatError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chat.ErrInvalid):
		s.writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, chat.ErrNotMember), errors.Is(err, chat.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "no such conversation")
	default:
		s.writeError(w, http.StatusInternalServerError, "the chat request failed")
	}
}

// decodeChatJSON decodes a chat request with the larger body cap chat needs.
func decodeChatJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBody)
	defer r.Body.Close()
	return strictJSON(r).Decode(v)
}

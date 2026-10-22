package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/meetings"
)

// maxMeetingBody bounds a meeting draft. A title, a description and a guest
// list are kilobytes; a megabyte is generous headroom, and a body larger than
// that is a bug or an attack rather than a meeting.
const maxMeetingBody = 1 << 20

// mountMeetings adds the meeting routes.
//
// Two surfaces, split by who is standing in front of them. A signed-in user
// works with meetings they organise or are invited to, and mints their chat
// session. An invitee who followed the link in their invitation has no
// account here, so the two meeting-invites routes are outside requireAuth
// and are gated entirely by the per-guest token the invitation carried -
// which is what an external guest answering "yes" without registering
// anywhere is supposed to cost.
func (s *Server) mountMeetings(r chi.Router) {
	r.Route("/meeting-invites", func(r chi.Router) {
		r.Get("/{uid}", s.handleMeetingInviteView)
		r.Post("/{uid}/rsvp", s.handleMeetingInviteRSVP)
		// A guest with no linchange account still has a call and a chat:
		// these three routes exchange the invitation token for the room's
		// socket, history and ICE configuration. The token is checked before
		// anything is upgraded or returned.
		r.Get("/{uid}/ws", s.handleMeetingInviteSocket)
		r.Get("/{uid}/chat", s.handleMeetingInviteChat)
		r.Post("/{uid}/chat/attachments", s.handleMeetingInviteUploadAttachment)
		r.Get("/{uid}/chat/attachments/{attachment}", s.handleMeetingInviteAttachment)
		r.Get("/{uid}/ice", s.handleMeetingInviteICE)
	})

	r.Route("/meetings", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/", s.handleListMeetings)
		r.Post("/", s.handleCreateMeeting)
		r.Get("/{uid}", s.handleGetMeeting)
		r.Post("/{uid}/resend", s.handleResendMeeting)
		r.Post("/{uid}/cancel", s.handleCancelMeeting)
		r.Post("/{uid}/rsvp", s.handleMeetingRSVP)
	})
}

// meetingUID reads the UID out of the path. A meeting's UID is an event UID,
// which carries an "@", and clients that encode their path segments correctly
// send that as %40; pathParam is the decoding rule every string parameter in
// this package goes through, and this name just keeps its call sites saying
// what they mean. Both spellings reach the same meeting.
func meetingUID(r *http.Request) string {
	return pathParam(r, "uid")
}

// decodeMeetingBody reads a meeting-sized JSON body with unknown fields
// rejected, so a typo'd field fails loudly instead of being silently ignored
// by a server that then creates the wrong meeting.
func decodeMeetingBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxMeetingBody)
	defer r.Body.Close()

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// handleCreateMeeting creates a meeting and mails every guest.
func (s *Server) handleCreateMeeting(w http.ResponseWriter, r *http.Request) {
	var req meetings.Draft
	if err := decodeMeetingBody(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.meetings.Create(r.Context(), Principal(r.Context()), req)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, result)
}

// handleListMeetings lists the caller's meetings, nearest first. Expired
// meetings are hidden unless include_expired is set; the list is paged so a
// long history does not arrive in one response.
func (s *Server) handleListMeetings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	includeExpired := q.Get("include_expired") == "1" || q.Get("include_expired") == "true"
	limit, offset := 20, 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.writeError(w, http.StatusBadRequest, "limit is not a number")
			return
		}
		limit = n
	}
	if limit > 100 {
		limit = 100
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			s.writeError(w, http.StatusBadRequest, "offset is not a number")
			return
		}
		offset = n
	}
	items, total, err := s.meetings.List(r.Context(), Principal(r.Context()), includeExpired, limit, offset)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"meetings": items,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	})
}

// handleGetMeeting returns one meeting: content, guest list, answers, and
// the links that point back here.
//
// The UID is the authorisation - 128 random bits that travel only inside the
// invitation - so there is no owner check on top of it. A stranger who does
// not hold the UID gets a 404 from the lookup itself.
func (s *Server) handleGetMeeting(w http.ResponseWriter, r *http.Request) {
	view, err := s.meetings.Get(r.Context(), meetingUID(r))
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleResendMeeting mails the invitation to every guest again.
func (s *Server) handleResendMeeting(w http.ResponseWriter, r *http.Request) {
	result, err := s.meetings.Resend(r.Context(), Principal(r.Context()), meetingUID(r))
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

// handleCancelMeeting calls a meeting off and mails the CANCEL.
func (s *Server) handleCancelMeeting(w http.ResponseWriter, r *http.Request) {
	view, err := s.meetings.Cancel(r.Context(), Principal(r.Context()), meetingUID(r))
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleMeetingRSVP records the signed-in caller's answer.
func (s *Server) handleMeetingRSVP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PartStat string `json:"partStat"`
	}
	if err := decodeMeetingBody(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	view, err := s.meetings.RSVP(r.Context(), Principal(r.Context()),
		meetingUID(r), req.PartStat)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleMeetingInviteView answers the link a guest followed from their
// invitation: the meeting, and their own standing on the guest list.
func (s *Server) handleMeetingInviteView(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	view, err := s.meetings.GetByInvite(r.Context(), meetingUID(r),
		q.Get("to"), q.Get("token"))
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

// handleMeetingInviteRSVP records an answer from a guest holding their
// invitation's token and nothing else.
func (s *Server) handleMeetingInviteRSVP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To       string `json:"to"`
		Token    string `json:"token"`
		PartStat string `json:"partStat"`
	}
	if err := decodeMeetingBody(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	partstat, err := s.meetings.InviteRSVP(r.Context(), meetingUID(r),
		req.To, req.Token, req.PartStat)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"partStat": partstat})
}

// handleMeetingInviteSocket upgrades an invited guest's chat/WebRTC socket.
//
// The token in the link is the whole identity: it names the address on the
// invitation, and GuestRoom verifies that the address and token belong
// together before the connection is accepted. No linchange session is needed.
func (s *Server) handleMeetingInviteSocket(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, err := s.meetings.GuestRoom(r.Context(), meetingUID(r), q.Get("to"), q.Get("token")); err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.chatHub.Accept(w, r, q.Get("to"))
}

// handleMeetingInviteChat returns the room history to a guest holding their
// invitation link.
func (s *Server) handleMeetingInviteChat(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	messages, err := s.meetings.GuestChatHistory(r.Context(), meetingUID(r),
		q.Get("to"), q.Get("token"), before, limit)
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

// handleMeetingInviteICE gives a guest the same STUN/TURN configuration a
// signed-in participant fetches, after checking the invitation token.
func (s *Server) handleMeetingInviteICE(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, err := s.meetings.GetByInvite(r.Context(), meetingUID(r),
		q.Get("to"), q.Get("token")); err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeICEServers(w)
}

// handleMeetingInviteUploadAttachment stores a file for a guest's room. The
// invitation token is the identity, exactly as it is for the guest socket.
func (s *Server) handleMeetingInviteUploadAttachment(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	conv, err := s.meetings.GuestRoom(r.Context(), meetingUID(r), q.Get("to"), q.Get("token"))
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	name, mimeType, data, ok := s.readChatUpload(w, r)
	if !ok {
		return
	}
	att, err := s.chat.Attach(r.Context(), conv.ID, q.Get("to"), name, mimeType, data)
	if err != nil {
		s.writeChatError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, att)
}

// handleMeetingInviteAttachment serves a guest a file from their room.
func (s *Server) handleMeetingInviteAttachment(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// GuestRoom verifies the token and makes sure the address is on the room,
	// which is the same membership AttachmentFor then checks.
	if _, err := s.meetings.GuestRoom(r.Context(), meetingUID(r), q.Get("to"), q.Get("token")); err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	att, data, err := s.chat.AttachmentFor(r.Context(), chi.URLParam(r, "attachment"), q.Get("to"))
	if err != nil {
		s.writeChatError(w, err)
		return
	}
	serveDownload(w, att.MIME, att.Name, data)
}

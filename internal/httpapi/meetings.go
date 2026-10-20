package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"

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
	})

	r.Route("/meetings", func(r chi.Router) {
		r.Use(s.requireAuth)
		// The static session route is registered before the parameterised
		// ones for the same reason /messages/search is: a wildcard that
		// swallowed it would turn "im" into a meeting UID nobody has.
		r.Post("/im/session", s.handleMeetingIMSession)

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
// send that as %40; chi reports the segment as it arrived, so a raw "%40"
// would look up a UID no meeting has. Decoding here means both spellings
// reach the same meeting.
func meetingUID(r *http.Request) string {
	uid := chi.URLParam(r, "uid")
	if decoded, err := url.PathUnescape(uid); err == nil {
		return decoded
	}
	return uid
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

// handleListMeetings lists the caller's meetings, nearest first.
func (s *Server) handleListMeetings(w http.ResponseWriter, r *http.Request) {
	items, err := s.meetings.List(r.Context(), Principal(r.Context()))
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"meetings": items})
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

// handleMeetingIMSession mints the OpenIM SDK session for the signed-in
// caller - provisioning their user first, so an account that has never
// chatted before exists by the time the browser logs in.
func (s *Server) handleMeetingIMSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.meetings.IMSession(r.Context(), Principal(r.Context()))
	if err != nil {
		s.writeError(w, mapErrorCode(err), err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, sess)
}

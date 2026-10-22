// Package meetings turns a calendar event into a meeting: an invitation that
// anybody can answer, a link back to this server, and a chat room the
// conversation can happen in.
//
// The design keeps the calendar where the calendar belongs. A meeting is
// still a row in events with its guest list in attendees, so every existing
// client - CalDAV, EWS, EAS, the web calendar - sees it and answers it with
// the protocol it already speaks. What this package adds is what those
// protocols do not carry: the invitation email with a per-guest reply link,
// and the native chat room a meeting's conversation lives in.
//
// The room's members are the organiser and every guest with an account here.
// An external invitee gets the invitation and the reply link, but no room,
// because they have no account to open it with.
package meetings

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"log/slog"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/chat"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// ErrInvalid means the request asked for something that cannot be done: a
// meeting with no guests, an address that is not an address, a title that is
// a header injection attempt. The caller can fix it; nothing was written.
var ErrInvalid = errors.New("meetings: invalid request")

// ErrDenied means the caller may not do this to this meeting: answering an
// invitation for somebody else, calling off a meeting one does not organise.
var ErrDenied = errors.New("meetings: denied")

// maxTitle bounds the title, which becomes the email subject: RFC 5322 caps a
// header line at 998 octets and "Invitation: " is already spent.
const maxTitle = 200

// Attendee is one guest as the client names them.
type Attendee struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
}

// Draft is a meeting as the client describes it.
type Draft struct {
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Location    string    `json:"location,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	// Timezone names the zone the times are displayed in. Empty inherits the
	// organiser's own zone, and then UTC, so a client that does not say
	// still gets a meeting at the hour it asked for.
	Timezone  string     `json:"timezone,omitempty"`
	Attendees []Attendee `json:"attendees"`
	// Groups are whole groups to invite alongside the addresses above.
	//
	// An invitation is a list of ATTENDEE addresses — there is no address-shaped
	// place for "the sales team" — so the expansion happens on the server,
	// where the group's membership is the real one. A client that expanded it
	// itself would invite whoever the list said a moment ago, which is exactly
	// the set of people this field exists to stop going stale.
	Groups []int64 `json:"groups,omitempty"`
	// OneWay makes this a webinar: the organiser is the only speaker, and a
	// guest asks for the floor by raising a hand.
	OneWay bool `json:"oneWay,omitempty"`
}

// AttendeeStatus is one guest and everything known about them: the answer
// they have given, and whether they are a user of this server (which is what
// decides whether they get a chat account alongside the invitation).
type AttendeeStatus struct {
	Address     string `json:"address"`
	DisplayName string `json:"displayName,omitempty"`
	Role        string `json:"role"`
	PartStat    string `json:"partStat"`
	RSVP        bool   `json:"rsvp"`
	Local       bool   `json:"local"`
}

// Meeting is the whole meeting as the web client draws it: the event's
// content, the guest list with every answer, and the links that point back
// here.
type Meeting struct {
	UID         string    `json:"uid"`
	EventID     int64     `json:"eventId"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Location    string    `json:"location,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Timezone    string    `json:"timezone"`
	Status      string    `json:"status"`
	Cancelled   bool      `json:"cancelled"`
	Organizer   Attendee  `json:"organizer"`
	JoinURL     string    `json:"joinUrl"`
	// ChatConversation is the uid of the meeting's native chat room, empty
	// when the room could not be opened. It is withheld from the
	// unauthenticated view: an invitee answers by link and has no account to
	// open the room with.
	ChatConversation string `json:"chatConversation,omitempty"`
	// OneWay marks a webinar: only the presenter speaks unless the presenter
	// grants a guest the floor.
	OneWay    bool             `json:"oneWay,omitempty"`
	Attendees []AttendeeStatus `json:"attendees"`
	CreatedAt time.Time        `json:"createdAt"`
}

// MeetingListItem is one row of the meeting list: everything the list shows,
// without the guest list the detail page fetches.
type MeetingListItem struct {
	UID       string    `json:"uid"`
	Title     string    `json:"title"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Timezone  string    `json:"timezone"`
	Status    string    `json:"status"`
	Cancelled bool      `json:"cancelled"`
	Organizer string    `json:"organizer"`
	Location  string    `json:"location,omitempty"`
	// Role is "organizer" or "attendee": what the caller is to this meeting,
	// which is what decides which controls the row shows.
	Role             string    `json:"role"`
	ChatConversation string    `json:"chatConversation,omitempty"`
	OneWay           bool      `json:"oneWay,omitempty"`
	AttendeeCount    int       `json:"attendeeCount"`
	AcceptedCount    int       `json:"acceptedCount"`
	MyPartStat       string    `json:"myPartStat,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
}

// CreateResult reports what creating a meeting did, including which guests
// could not be mailed. A meeting that exists with three invitations queued
// and one failed is a success the organiser needs to see the failure in, not
// an error that hides the other three.
type CreateResult struct {
	Meeting
	InviteFailures []string `json:"inviteFailures,omitempty"`
}

// ResendResult reports an invitation resend.
type ResendResult struct {
	Sent           int      `json:"sent"`
	InviteFailures []string `json:"inviteFailures,omitempty"`
}

// Options are the inputs to New.
type Options struct {
	DB       *sqlx.DB
	Mail     *webmail.Service
	Calendar *calendar.Service
	// Chat is the native chat backend. Nil means the service runs without chat
	// rooms, which tests do; invitations and answers work the same either way.
	Chat *chat.Service
	// PublicURL is the base address invitation links are built from, with no
	// trailing slash.
	PublicURL string
	Logger    *slog.Logger
}

// Service creates, lists and answers meetings.
type Service struct {
	db        *sqlx.DB
	mail      *webmail.Service
	calendar  *calendar.Service
	events    *store.Events
	meetings  *store.Meetings
	users     *store.Users
	chat      *chat.Service
	publicURL string
	log       *slog.Logger
}

// New builds the meetings service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil {
		return nil, errors.New("meetings: a database handle is required")
	}
	if opts.Calendar == nil {
		return nil, errors.New("meetings: a calendar service is required")
	}
	if opts.Mail == nil {
		return nil, errors.New("meetings: a mail service is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		db:       opts.DB,
		mail:     opts.Mail,
		calendar: opts.Calendar,
		events:   store.NewEvents(),
		meetings: store.NewMeetings(),
		users:    store.NewUsers(),
		chat:     opts.Chat,
		// One trailing slash is removed so joining a path does not double
		// the separator. Only one: trimming every trailing slash would turn
		// a base address that somehow arrived without a host ("https://")
		// into "https:", and a repair must not be able to mangle further.
		publicURL: strings.TrimSuffix(opts.PublicURL, "/"),
		log:       log,
	}, nil
}

// ChatEnabled reports whether this deployment has a chat backend at all, so a
// handler can answer "there is no chat here" without attempting a call.
func (s *Service) ChatEnabled() bool { return s.chat != nil }

// JoinURL is the link every invitation carries: the meeting's page on this
// server.
func (s *Service) JoinURL(uid string) string {
	return s.publicURL + "/meetings/" + uid
}

// Create stores the meeting, mails every guest, and opens the chat room.
//
// The event and the meeting rows are written in one transaction - a meeting
// whose reply tokens were never stored has invite links nobody can answer,
// which is worse than no meeting. The emails and the room come after the
// commit: both are things a resend can retry, while a half-written meeting
// cannot be retried at all.
func (s *Service) Create(ctx context.Context, who policy.Principal, d Draft) (*CreateResult, error) {
	if who.Address == "" {
		return nil, fmt.Errorf("%w: no organiser", ErrInvalid)
	}
	organizer := strings.ToLower(strings.TrimSpace(who.Address))

	title, err := normaliseTitle(d.Title)
	if err != nil {
		return nil, err
	}
	if d.Start.IsZero() || d.End.IsZero() {
		return nil, fmt.Errorf("%w: a meeting needs a start and an end", ErrInvalid)
	}
	if !d.End.After(d.Start) {
		return nil, fmt.Errorf("%w: the meeting ends before it starts", ErrInvalid)
	}
	// A group is invited whole by naming it, and it becomes people before any
	// of the checks below run — so a group that expands to nobody ends up in
	// "a meeting needs at least one guest" rather than in a meeting with no
	// guests and a room full of people wondering why.
	if len(d.Groups) > 0 {
		addresses, err := s.expandGroups(ctx, organizer, d.Groups)
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			d.Attendees = append(d.Attendees, Attendee{Address: address})
		}
	}
	guests, err := normaliseAttendees(d.Attendees, organizer)
	if err != nil {
		return nil, err
	}

	// The organiser's own profile decides the display zone when the client
	// did not say, because the zone the organiser works in is the one their
	// guests were probably told about too.
	timezone := strings.TrimSpace(d.Timezone)
	if timezone == "" {
		if u, err := s.users.Get(ctx, s.db, organizer); err == nil {
			timezone = u.Timezone
		}
	}
	if timezone == "" {
		timezone = "UTC"
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return nil, fmt.Errorf("%w: %q is not a time zone", ErrInvalid, timezone)
	}

	orgName := ""
	if u, err := s.users.Get(ctx, s.db, organizer); err == nil {
		orgName = u.DisplayName
	}

	uid, err := newUID()
	if err != nil {
		return nil, err
	}
	joinURL := s.JoinURL(uid)

	ev := ical.Event{
		UID:         uid,
		Summary:     title,
		Description: strings.TrimSpace(d.Description),
		Location:    strings.TrimSpace(d.Location),
		URL:         joinURL,
		Start:       d.Start,
		End:         d.End,
		Timezone:    timezone,
		Organizer:   ical.Organizer{Address: organizer, Name: orgName},
	}
	// Outlook lights its Join button off the LOCATION property, not off URL,
	// and an online meeting with nowhere visible to go is exactly the case
	// that matters. A room the organiser named stays their name; the URL
	// property carries the link either way.
	if ev.Location == "" {
		ev.Location = joinURL
	}
	for _, g := range guests {
		ev.Attendees = append(ev.Attendees, ical.Attendee{
			Address:  g.Address,
			Name:     g.Name,
			Role:     ical.RoleRequired,
			PartStat: ical.PartStatNeedsAction,
			RSVP:     true,
		})
	}

	var (
		meeting  store.Meeting
		invitees []store.MeetingInvite
	)
	err = store.InTx(ctx, s.db, func(tx *sqlx.Tx) error {
		row, err := s.calendar.Create(ctx, tx, who, calendar.Draft{Event: ev})
		if err != nil {
			return err
		}
		invitees = invitees[:0]
		for _, g := range guests {
			// A guest can open the meeting's chat room exactly when they are a
			// user of this server. An external address is invited by mail and
			// answers by link, and has no account to chat with.
			inv := store.MeetingInvite{Address: g.Address}
			if _, err := store.LoadPrincipal(ctx, tx, g.Address); err == nil {
				inv.IsLocal = true
			}
			if inv.RSVPToken, err = randomToken(); err != nil {
				return err
			}
			invitees = append(invitees, inv)
		}
		meeting, err = s.meetings.Create(ctx, tx, store.Meeting{
			UID:       uid,
			EventID:   row.ID,
			Organizer: organizer,
			OneWay:    d.OneWay,
		}, invitees)
		return err
	})
	if err != nil {
		return nil, err
	}

	failures, err := s.mailInvites(ctx, who, meeting, invitees, title)
	if err != nil {
		return nil, err
	}

	s.openChatRoomBestEffort(ctx, &meeting, organizer, invitees, title)

	view, err := s.view(ctx, meeting, invitees)
	if err != nil {
		return nil, err
	}
	return &CreateResult{Meeting: *view, InviteFailures: failures}, nil
}

// List returns every meeting the caller organises or is invited to, nearest
// first.
func (s *Service) List(ctx context.Context, who policy.Principal) ([]MeetingListItem, error) {
	rows, err := s.meetings.ListFor(ctx, s.db, strings.ToLower(strings.TrimSpace(who.Address)))
	if err != nil {
		return nil, err
	}
	out := make([]MeetingListItem, 0, len(rows))
	for _, row := range rows {
		role := "attendee"
		if strings.EqualFold(row.Organizer, who.Address) {
			role = "organizer"
		}
		out = append(out, MeetingListItem{
			UID:              row.UID,
			Title:            row.Summary,
			Start:            row.DTStart,
			End:              row.DTEnd,
			Timezone:         row.Timezone,
			Status:           row.Status,
			Cancelled:        row.Cancelled,
			Organizer:        row.Organizer,
			Location:         row.Location,
			Role:             role,
			ChatConversation: row.ChatConversation,
			OneWay:           row.OneWay,
			AttendeeCount:    row.AttendeeCount,
			AcceptedCount:    row.AcceptedCount,
			MyPartStat:       row.MyPartStat,
			CreatedAt:        row.CreatedAt,
		})
	}
	return out, nil
}

// Get returns one meeting.
//
// Knowing the UID is the authorisation. The UID is 128 bits of randomness and
// travels only inside the invitation and to the people on the guest list, so
// it is a capability rather than an address: the same thing that lets an
// invitee open their link without an account here keeps a stranger from
// reading a meeting they were not told about.
func (s *Service) Get(ctx context.Context, uid string) (*Meeting, error) {
	meeting, err := s.meetings.ByUID(ctx, s.db, uid)
	if err != nil {
		return nil, err
	}
	invitees, err := s.meetings.Invites(ctx, s.db, meeting.ID)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, meeting, invitees)
}

// GetByInvite returns a meeting to an invitee answering by link.
//
// The difference from Get is the second check: the address and token have to
// belong together, so a forwarded link answers for the person it was sent to
// and for nobody else. The chat room is withheld - the visitor has no chat
// account, and handing out the group id to whoever holds any token would be
// handing it out to the internet.
func (s *Service) GetByInvite(ctx context.Context, uid, address, token string) (*Meeting, error) {
	meeting, err := s.meetings.ByUID(ctx, s.db, uid)
	if err != nil {
		return nil, err
	}
	if err := s.verifyInvite(ctx, meeting, address, token); err != nil {
		return nil, err
	}
	invitees, err := s.meetings.Invites(ctx, s.db, meeting.ID)
	if err != nil {
		return nil, err
	}
	view, err := s.view(ctx, meeting, invitees)
	if err != nil {
		return nil, err
	}
	view.ChatConversation = ""
	return view, nil
}

// Resend mails every guest the invitation again.
//
// The reply links are the ones issued at creation, so a guest who already
// answered keeps answering as themselves and a guest whose mail went nowhere
// gets a second copy of the same meeting rather than a second meeting.
func (s *Service) Resend(ctx context.Context, who policy.Principal, uid string) (*ResendResult, error) {
	meeting, err := s.meetings.ByUID(ctx, s.db, uid)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(meeting.Organizer, who.Address) {
		return nil, fmt.Errorf("%w: only the organiser can resend invitations", ErrDenied)
	}
	row, err := s.events.Get(ctx, s.db, meeting.EventID)
	if err != nil {
		return nil, err
	}
	if row.Cancelled {
		return nil, fmt.Errorf("%w: the meeting is cancelled", ErrInvalid)
	}
	invitees, err := s.meetings.Invites(ctx, s.db, meeting.ID)
	if err != nil {
		return nil, err
	}
	s.openChatRoomBestEffort(ctx, &meeting, meeting.Organizer, invitees, row.Summary)

	failures, err := s.mailInvites(ctx, who, meeting, invitees, row.Summary)
	if err != nil {
		return nil, err
	}
	return &ResendResult{Sent: len(invitees) - len(failures), InviteFailures: failures}, nil
}

// Cancel calls the meeting off: the event is marked cancelled and every guest
// is mailed the CANCEL the protocol says they should get.
func (s *Service) Cancel(ctx context.Context, who policy.Principal, uid string) (*Meeting, error) {
	meeting, err := s.meetings.ByUID(ctx, s.db, uid)
	if err != nil {
		return nil, err
	}
	// CancelFor checks the organiser as part of building the document, which
	// keeps the check next to the thing being protected rather than in front
	// of a different call.
	out, err := s.calendar.CancelFor(ctx, s.db, who, meeting.EventID)
	if err != nil {
		return nil, err
	}
	if _, err := s.calendar.Cancel(ctx, s.db, who, meeting.EventID); err != nil {
		return nil, err
	}
	// One message to the whole guest list: a cancellation is the same fact
	// for everybody, and it carries no per-guest link because there is
	// nothing left to answer.
	if len(out.Recipients) > 0 {
		body := s.cancelBody(ctx, meeting, out)
		req := webmail.ComposeRequest{
			From:       meeting.Organizer,
			Subject:    out.Subject,
			Text:       body,
			Attachment: []webmail.AttachmentInput{calendarAttachment(out.Document)},
		}
		for _, addr := range out.Recipients {
			req.To = append(req.To, webmail.Address{Address: addr})
		}
		if _, err := s.mail.Send(ctx, meeting.Organizer, req); err != nil {
			// The meeting is cancelled either way. The organiser sees the
			// failure in the log rather than in a meeting that looks live.
			s.log.Error("sending meeting cancellation",
				"uid", uid, "error", err)
		}
	}
	invitees, err := s.meetings.Invites(ctx, s.db, meeting.ID)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, meeting, invitees)
}

// RSVP records the caller's answer to a meeting they are on the guest list
// of.
func (s *Service) RSVP(ctx context.Context, who policy.Principal, uid, partstat string) (*Meeting, error) {
	if !validAnswer(partstat) {
		return nil, fmt.Errorf("%w: %q is not an answer", ErrInvalid, partstat)
	}
	meeting, err := s.meetings.ByUID(ctx, s.db, uid)
	if err != nil {
		return nil, err
	}
	address := strings.ToLower(strings.TrimSpace(who.Address))
	// Being on the guest list is the whole of the authorisation: not having a
	// meeting at all and not being invited to it look the same from out here,
	// so a stranger guessing UIDs learns nothing from the difference.
	if _, err := s.meetings.Invite(ctx, s.db, meeting.ID, address); err != nil {
		return nil, fmt.Errorf("%w: this address is not invited", ErrDenied)
	}
	if _, err := s.events.SetPartStat(ctx, s.db, meeting.UID, address, partstat); err != nil {
		return nil, err
	}
	return s.Get(ctx, uid)
}

// InviteRSVP records an answer from an invitee who holds the link from their
// invitation and has no account here.
func (s *Service) InviteRSVP(ctx context.Context, uid, address, token, partstat string) (string, error) {
	if !validAnswer(partstat) {
		return "", fmt.Errorf("%w: %q is not an answer", ErrInvalid, partstat)
	}
	meeting, err := s.meetings.ByUID(ctx, s.db, uid)
	if err != nil {
		return "", err
	}
	if err := s.verifyInvite(ctx, meeting, address, token); err != nil {
		return "", err
	}
	if _, err := s.events.SetPartStat(ctx, s.db, meeting.UID,
		strings.ToLower(strings.TrimSpace(address)), partstat); err != nil {
		return "", err
	}
	return partstat, nil
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// verifyInvite checks an address and token against a meeting's issued
// invitation.
//
// Every failure is ErrNotFound on purpose. A caller who can distinguish
// "wrong token" from "no such meeting" can enumerate meetings by UID and then
// learn which addresses were invited, one probe at a time; a caller who
// cannot distinguish them learns only that the pair they guessed is wrong.
func (s *Service) verifyInvite(ctx context.Context, meeting store.Meeting, address, token string) error {
	address = strings.ToLower(strings.TrimSpace(address))
	if address == "" || token == "" {
		return store.ErrNotFound
	}
	invite, err := s.meetings.Invite(ctx, s.db, meeting.ID, address)
	if err != nil {
		return store.ErrNotFound
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(invite.RSVPToken)) != 1 {
		return store.ErrNotFound
	}
	return nil
}

// mailInvites sends one invitation per guest, each carrying that guest's own
// reply link, and reports the guests it could not mail.
//
// One message per guest rather than one for the list: the reply link is
// personal, and a guest list in a To header is a guest list every guest can
// mail each other about afterwards.
func (s *Service) mailInvites(ctx context.Context, who policy.Principal, meeting store.Meeting,
	invitees []store.MeetingInvite, title string) ([]string, error) {
	out, err := s.calendar.RequestFor(ctx, s.db, who, meeting.EventID)
	if err != nil {
		return nil, err
	}
	row, err := s.events.Get(ctx, s.db, meeting.EventID)
	if err != nil {
		return nil, err
	}

	var failures []string
	for _, inv := range invitees {
		body := s.inviteBody(ctx, meeting, row, inv, title)
		req := webmail.ComposeRequest{
			From: meeting.Organizer,
			To: []webmail.Address{{
				Address: inv.Address,
				Name:    s.displayName(ctx, inv.Address),
			}},
			Subject:    out.Subject,
			Text:       body,
			Attachment: []webmail.AttachmentInput{calendarAttachment(out.Document)},
		}
		if _, err := s.mail.Send(ctx, meeting.Organizer, req); err != nil {
			// A guest who could not be mailed still has a row and a working
			// reply link: the organiser resends rather than the meeting
			// failing, because one bad address must not cost the others.
			s.log.Error("sending a meeting invitation",
				"meeting", meeting.UID, "guest", inv.Address, "error", err)
			failures = append(failures, inv.Address)
		}
	}
	return failures, nil
}

// inviteBody writes the invitation text: the fact of the meeting, and the
// one link that serves as both the way in and the way to answer.
func (s *Service) inviteBody(ctx context.Context, meeting store.Meeting, row store.Event,
	inv store.MeetingInvite, title string) string {
	var b strings.Builder
	from := s.displayName(ctx, meeting.Organizer)
	if from == "" {
		from = meeting.Organizer
	}
	fmt.Fprintf(&b, "%s invited you to a meeting.\n\n", from)
	fmt.Fprintf(&b, "%s\n\n", title)
	fmt.Fprintf(&b, "When:  %s\n", formatWhen(row.DTStart, row.DTEnd, row.Timezone))
	if loc := strings.TrimSpace(row.Location); loc != "" && loc != s.JoinURL(meeting.UID) {
		fmt.Fprintf(&b, "Where: %s\n", loc)
	}
	if desc := strings.TrimSpace(eventDescription(row.ICalBlob)); desc != "" {
		fmt.Fprintf(&b, "\n%s\n", desc)
	}
	fmt.Fprintf(&b, "\nJoin or answer: %s\n", s.inviteURL(meeting.UID, inv))
	b.WriteString("\nThe attached invitation file can be opened by your calendar " +
		"client to add this meeting. If your mail client offers to answer, " +
		"answer there - the organiser receives it either way.\n")
	return b.String()
}

// cancelBody writes the cancellation text.
func (s *Service) cancelBody(ctx context.Context, meeting store.Meeting, out calendar.Outgoing) string {
	row, err := s.events.Get(ctx, s.db, meeting.EventID)
	if err != nil {
		row = store.Event{}
	}
	var b strings.Builder
	from := s.displayName(ctx, meeting.Organizer)
	if from == "" {
		from = meeting.Organizer
	}
	fmt.Fprintf(&b, "%s cancelled this meeting:\n\n", from)
	fmt.Fprintf(&b, "%s\n\n", row.Summary)
	if !row.DTStart.IsZero() {
		fmt.Fprintf(&b, "When: %s\n", formatWhen(row.DTStart, row.DTEnd, row.Timezone))
	}
	b.WriteString("\nThe attached file updates the invitation in your calendar client.\n")
	return b.String()
}

// inviteURL is the per-guest link: it opens the meeting page already knowing
// who is arriving, which is what lets an accountless invitee answer.
func (s *Service) inviteURL(uid string, inv store.MeetingInvite) string {
	return s.JoinURL(uid) + "?to=" + url.QueryEscape(inv.Address) +
		"&token=" + url.QueryEscape(inv.RSVPToken)
}

// displayName is the human name of an address, best effort: a decoration
// that falls back to the address rather than failing a send.
func (s *Service) displayName(ctx context.Context, address string) string {
	if u, err := s.users.Get(ctx, s.db, address); err == nil {
		return u.DisplayName
	}
	return ""
}

// openChatRoomBestEffort opens the meeting's native chat room. Every failure is
// logged and swallowed: a meeting works without its room, and a chat problem
// must not take the invitations with it.
//
// The room's members are the organiser and every guest who has an account here.
// An external guest gets the invitation and the RSVP link but no chat identity,
// because there is no account to have one with.
func (s *Service) openChatRoomBestEffort(ctx context.Context, meeting *store.Meeting,
	organizer string, invitees []store.MeetingInvite, title string) {
	if s.chat == nil || meeting.ChatConversation != "" {
		return
	}
	organizer = strings.ToLower(strings.TrimSpace(organizer))
	members := []string{organizer}
	for _, inv := range invitees {
		if !inv.IsLocal {
			continue
		}
		address := strings.ToLower(strings.TrimSpace(inv.Address))
		if address != "" && address != organizer {
			members = append(members, address)
		}
	}

	conv, err := s.chat.Create(ctx, s.db, chat.CreateRequest{
		Kind:       chat.KindMeeting,
		Title:      title,
		Members:    members,
		CreatedBy:  organizer,
		MeetingUID: meeting.UID,
		OneWay:     meeting.OneWay,
		Presenter:  organizer,
	})
	if err != nil {
		s.log.Warn("meeting chat: opening the room failed",
			"meeting", meeting.UID, "error", err)
		return
	}
	if err := s.meetings.SetChatConversation(ctx, s.db, meeting.ID, conv.UID); err != nil {
		s.log.Warn("meeting chat: recording the room failed",
			"meeting", meeting.UID, "error", err)
		return
	}
	meeting.ChatConversation = conv.UID
}

// view assembles the whole meeting from its rows.
func (s *Service) view(ctx context.Context, meeting store.Meeting, invitees []store.MeetingInvite) (*Meeting, error) {
	row, err := s.events.Get(ctx, s.db, meeting.EventID)
	if err != nil {
		return nil, err
	}
	byAddress := make(map[string]store.MeetingInvite, len(invitees))
	for _, inv := range invitees {
		byAddress[strings.ToLower(inv.Address)] = inv
	}
	attendees := make([]AttendeeStatus, 0, len(row.Attendees))
	for _, a := range row.Attendees {
		status := AttendeeStatus{
			Address:     strings.ToLower(a.Address),
			DisplayName: a.DisplayName,
			Role:        a.Role,
			PartStat:    a.PartStat,
			RSVP:        a.RSVP,
			Local:       byAddress[strings.ToLower(a.Address)].Local(),
		}
		attendees = append(attendees, status)
	}
	return &Meeting{
		UID:              meeting.UID,
		EventID:          meeting.EventID,
		Title:            row.Summary,
		Description:      eventDescription(row.ICalBlob),
		Location:         row.Location,
		Start:            row.DTStart,
		End:              row.DTEnd,
		Timezone:         row.Timezone,
		Status:           row.Status,
		Cancelled:        row.Cancelled,
		Organizer:        Attendee{Address: strings.ToLower(meeting.Organizer), Name: row.Organizer},
		JoinURL:          s.JoinURL(meeting.UID),
		ChatConversation: meeting.ChatConversation,
		OneWay:           meeting.OneWay,
		Attendees:        attendees,
		CreatedAt:        meeting.CreatedAt,
	}, nil
}

// calendarAttachment is an iCalendar document queued as an attachment.
func calendarAttachment(doc []byte) webmail.AttachmentInput {
	return webmail.AttachmentInput{
		Filename: "invitation.ics",
		MIMEType: "text/calendar",
		Content:  base64.StdEncoding.EncodeToString(doc),
	}
}

// normaliseTitle validates and cleans the title.
//
// The control-character strip is not decoration: the title becomes the Subject
// header, and a title carrying a CR, a LF or a NUL is a header line the
// organiser did not mean to write. What survives is the title with its
// whitespace runs collapsed, so the check that follows sees the length that
// will actually be sent.
func normaliseTitle(title string) (string, error) {
	title = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, title)
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return "", fmt.Errorf("%w: a meeting needs a title", ErrInvalid)
	}
	if n := utf8.RuneCountInString(title); n > maxTitle {
		return "", fmt.Errorf("%w: the title is %d characters, over the %d limit",
			ErrInvalid, n, maxTitle)
	}
	return title, nil
}

// normaliseAttendees validates the guest list: addresses are parsed to
// whatever a mail server would accept, lowercased because every comparison
// in this product is case-insensitive, deduplicated, and stripped of the
// organiser, who does not invite themselves.
func normaliseAttendees(in []Attendee, organizer string) ([]Attendee, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: a meeting needs at least one guest", ErrInvalid)
	}
	seen := make(map[string]bool, len(in))
	out := make([]Attendee, 0, len(in))
	for _, a := range in {
		parsed, err := mail.ParseAddress(strings.TrimSpace(a.Address))
		if err != nil || parsed.Address == "" {
			return nil, fmt.Errorf("%w: %q is not a mail address", ErrInvalid, a.Address)
		}
		address := strings.ToLower(parsed.Address)
		if address == organizer || seen[address] {
			continue
		}
		seen[address] = true
		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = strings.TrimSpace(parsed.Name)
		}
		out = append(out, Attendee{Address: address, Name: name})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: the organiser cannot be the only guest", ErrInvalid)
	}
	return out, nil
}

// expandGroups resolves the groups an organiser named into the addresses the
// invitation will carry.
func (s *Service) expandGroups(ctx context.Context, organizer string, ids []int64) ([]string, error) {
	if err := s.allowGroups(ctx, organizer, ids); err != nil {
		return nil, err
	}
	return store.NewGroups().Expand(ctx, s.db, ids)
}

// allowGroups refuses any group belonging to an organization the organiser is
// not part of.
//
// Only groups of the organiser's own organization resolve. A group id is a
// number anybody can guess, and answering a guess with a list of addresses
// would turn this route into a directory of every organization on the server —
// which is what /organization/people deliberately is not for somebody outside
// the domain.
func (s *Service) allowGroups(ctx context.Context, organizer string, ids []int64) error {
	workgroups := store.NewWorkgroups()
	groups := store.NewGroups()

	// Resolved once: whether an address is in an organization is a fact about
	// the address, not about which group was asked for.
	mine, mineErr := workgroups.ForAddress(ctx, s.db, organizer)
	checked := make(map[int64]bool, len(ids))
	for _, id := range ids {
		group, err := groups.Get(ctx, s.db, id)
		if err != nil {
			// An unknown group is a mistake worth reporting; skipping it would
			// build a meeting that quietly leaves people out.
			return err
		}
		if checked[group.WorkgroupID] {
			continue
		}
		checked[group.WorkgroupID] = true
		if mineErr == nil && mine.ID == group.WorkgroupID {
			continue
		}
		// Somebody added to an organization from outside its domains is in it
		// all the same: the membership row, not the address, is what says so.
		if _, err := workgroups.Member(ctx, s.db, group.WorkgroupID, organizer); err == nil {
			continue
		}
		return fmt.Errorf("%w: %s may not invite that organization's groups", ErrDenied, organizer)
	}
	return nil
}

// validAnswer reports whether a string is one of the four answers iTIP
// defines for an attendee.
func validAnswer(partstat string) bool {
	switch partstat {
	case ical.PartStatAccepted, ical.PartStatDeclined,
		ical.PartStatTentative, ical.PartStatNeedsAction:
		return true
	default:
		return false
	}
}

// eventDescription reads the description out of a stored document. The
// events table has no description column - it is a property of the document,
// not an index over it - so the document is the only place it lives.
func eventDescription(blob []byte) string {
	doc, err := ical.Parse(blob)
	if err != nil || len(doc.Events) == 0 {
		return ""
	}
	return doc.Events[0].Description
}

// formatWhen renders a meeting's time range in its own zone: the zone name
// is always shown, because "14:00" without one is a promise nobody can keep.
func formatWhen(start, end time.Time, timezone string) string {
	loc, err := time.LoadLocation(timezone)
	if err != nil || timezone == "" {
		loc = time.UTC
		timezone = "UTC"
	}
	s, e := start.In(loc), end.In(loc)
	const day = "Mon, 02 Jan 2006"
	const clock = "15:04"
	// The same-day test compares calendar dates rather than truncating the
	// instants: a zone whose midnight is not a whole number of hours from UTC
	// would put two times in the same local day in different UTC days.
	if s.Format("2006-01-02") == e.Format("2006-01-02") {
		return fmt.Sprintf("%s %s - %s %s",
			s.Format(day), s.Format(clock), e.Format(clock), timezone)
	}
	return fmt.Sprintf("%s %s - %s %s %s",
		s.Format(day), s.Format(clock), e.Format(day), e.Format(clock), timezone)
}

// newUID returns a fresh event UID in the same shape the calendar package
// mints, so a UID looks like one UID wherever it is read.
//
// The random part is 16 bytes. That matters more here than in an ordinary
// calendar event: the UID is the capability the invitation link carries, and
// a short one would be a meeting anybody could find by guessing.
func newUID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("meetings: generating a UID: %w", err)
	}
	return hex.EncodeToString(buf[:]) + "@linchange", nil
}

// randomToken returns the per-guest secret an RSVP link carries.
func randomToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("meetings: generating a reply token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

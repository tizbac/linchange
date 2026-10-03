package calendar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/store"
)

// Scheduling messages, RFC 5546 iTIP, are how two calendar servers tell each
// other about a meeting. This file is both halves of that conversation: applying
// what arrives, and building what to send.
//
// The rules that are easy to get wrong, and that the tests here pin:
//
//   - A message is applied when it is newer, where "newer" is the sequence first
//     and the timestamp only as a tiebreaker. That second half is not pedantry: a
//     CANCEL deliberately carries the same SEQUENCE as the REQUEST it withdraws,
//     so a comparison that ignores DTSTAMP discards every cancellation.
//   - A copy addressed to this server that concerns somebody else's meeting is
//     ignored rather than stored. A team alias puts a dozen people on a
//     distribution list, and only the addressee acts.
//   - Answering an invitation is not automatic. Accepting a meeting is a
//     statement about somebody's time, and a server that makes it unasked is
//     wrong, so it takes an explicit decision.

// Incoming is one scheduling message to apply.
type Incoming struct {
	// Document is the iCalendar payload as it arrived.
	Document []byte
	// AutoAccept answers an invitation immediately when the recipient is an
	// attendee being asked to reply.
	//
	// It is off by default. Every mainstream server accepts on arrival, which
	// makes that the convenient choice and also the wrong one: an unattended
	// server saying yes to every meeting it is copied on commits its owner to
	// meetings they never saw. A caller that wants the conventional behaviour
	// opts in.
	AutoAccept bool
}

// Action is what applying a scheduling message did.
type Action string

const (
	// ActionCreated stored a meeting that was not here.
	ActionCreated Action = "created"
	// ActionUpdated replaced a stored meeting with a newer revision.
	ActionUpdated Action = "updated"
	// ActionUnchanged means the message was not newer than what is stored, so
	// it was discarded. This is the normal outcome of a duplicate delivery and
	// is not an error.
	ActionUnchanged Action = "unchanged"
	// ActionIgnored means the message was understood but is not this server's
	// business: it concerns somebody else's meeting, or is a method that needs
	// no action.
	ActionIgnored Action = "ignored"
	// ActionCancelled withdrew a meeting.
	ActionCancelled Action = "cancelled"
)

// Applied reports what applying a scheduling message did.
type Applied struct {
	Method ical.Method `json:"method"`
	Action Action      `json:"action"`
	UID    string      `json:"uid,omitempty"`
	// Sequence is the revision now stored, when one is.
	Sequence int `json:"sequence,omitempty"`
	// Reply is the document to send back, when the message calls for an answer
	// and the caller asked for one to be built.
	Reply []byte `json:"-"`
	// Note explains an ignore, for the log. It is never shown to a client.
	Note string `json:"-"`
}

// IsNewer reports whether an arriving revision supersedes a stored one.
//
// RFC 5546 §3.2.5: the sequence number decides, and the timestamp only breaks a
// tie. The tie is not hypothetical — a CANCEL carries the same SEQUENCE as the
// REQUEST it withdraws, so a comparison on the sequence alone throws every
// cancellation away, and a cancellation thrown away is a meeting that everyone
// thinks is still on.
func IsNewer(newSequence int, newStamp time.Time, oldSequence int, oldStamp time.Time) bool {
	if newSequence != oldSequence {
		return newSequence > oldSequence
	}
	return newStamp.After(oldStamp)
}

// Apply handles one scheduling message on behalf of a principal.
//
// Every path returns a Result rather than an error for anything the message did
// or did not ask of this server. A mail handler receiving a scheduling message
// has already decided the message is deliverable; refusing to deliver it because
// the recipient is not an attendee would bounce mail that should have been
// filed.
func (s *Service) Apply(ctx context.Context, q store.Querier, who policy.Principal, in Incoming) (Applied, error) {
	doc, err := ical.Parse(in.Document)
	if err != nil {
		return Applied{}, fmt.Errorf("%w: parsing the scheduling message: %v", ErrBadRequest, err)
	}
	if !doc.Method.IsKnown() {
		return Applied{
			Method: doc.Method,
			Action: ActionIgnored,
			Note:   "no method, or a method this server does not act on",
		}, nil
	}
	if len(doc.Events) == 0 {
		return Applied{
			Method: doc.Method,
			Action: ActionIgnored,
			Note:   "the message carries no event",
		}, nil
	}

	// Every event in one message shares the method, and the first is
	// representative. A file with a REQUEST and a CANCEL for different events is
	// legal but is not something any client produces.
	ev := doc.Events[0]
	result := Applied{Method: doc.Method, UID: ev.UID}

	if !concerns(ev, who.Address) {
		result.Action = ActionIgnored
		result.Note = "the recipient is neither the organiser nor an attendee"
		return result, nil
	}

	switch doc.Method {
	case ical.MethodRequest:
		return s.applyRequest(ctx, q, who, ev, in)
	case ical.MethodReply:
		return s.applyReply(ctx, q, ev)
	case ical.MethodCancel:
		return s.applyCancel(ctx, q, who, ev)
	case ical.MethodAdd:
		// An ADD names one extra occurrence of a series. Storing it as a
		// detached instance is exactly what it is.
		return s.applyRequest(ctx, q, who, asDetached(ev), in)
	default:
		result.Action = ActionIgnored
		result.Note = "no action is defined for this method"
		return result, nil
	}
}

// concerns reports whether a principal has any business acting on an event.
//
// A copy to a distribution list is not a request to act. Without this check, a
// message about a meeting between two other people and merely copied to a team
// alias would create a third copy on this server, attributed to nobody.
func concerns(ev ical.Event, address string) bool {
	if strings.EqualFold(ev.Organizer.Address, address) {
		return true
	}
	for _, a := range ev.Attendees {
		if strings.EqualFold(a.Address, address) {
			return true
		}
	}
	return false
}

// asDetached gives an ADD a slot to be stored under, so that it becomes a
// detached instance rather than replacing the series.
//
// The slot is the occurrence it names. An ADD that names nothing is not
// distinguishable from a REQUEST, so it is treated as one.
func asDetached(ev ical.Event) ical.Event {
	if ev.RecurrenceID != nil {
		return ev
	}
	slot := ev.Start
	ev.RecurrenceID = &slot
	// The rule belongs to the series and must not be stored on the instance: an
	// override carrying an RRULE would expand into the whole series again.
	ev.RRULE = ""
	return ev
}

// applyRequest stores an invitation, if it is newer than what is here.
func (s *Service) applyRequest(ctx context.Context, q store.Querier, who policy.Principal, ev ical.Event, in Incoming) (Applied, error) {
	result := Applied{Method: ical.MethodRequest, UID: ev.UID}

	cal, err := s.calendars.EnsureDefault(ctx, q, who.Address, calTimezone(calFallback))
	if err != nil {
		return result, err
	}

	existing, err := s.events.ByUID(ctx, q, cal.ID, ev.UID, ev.RecurrenceID)
	switch {
	case err == nil:
		if !IsNewer(ev.Sequence, stampOf(ev, existing.DTStamp), existing.Sequence, existing.DTStamp) {
			result.Action = ActionUnchanged
			result.Sequence = existing.Sequence
			result.Note = "the message is not newer than the stored revision"
			return result, nil
		}
	case errors.Is(err, store.ErrNotFound):
		// The normal case for a first invitation.
	default:
		return result, err
	}

	// An invitation is stored in the form it arrived in, with only the fields
	// this server insists on filled in. Re-serialising it from the model would
	// quietly normalise away properties this server does not model, and the
	// recipient's client is the one that has to understand them.
	stored := ev
	stored.Status = defaultStatus(stored.Status)
	stored.ShowAs = defaultShowAs(stored.ShowAs)
	stored.DTStamp = stampOf(ev, time.Time{})

	req := writeRequest(cal.ID, who.Address, stored)
	row, err := s.events.Upsert(ctx, q, req)
	if err != nil {
		return result, err
	}

	result.Action = ActionUpdated
	result.Sequence = row.Sequence
	if existing.ID == 0 {
		result.Action = ActionCreated
	}

	// Only answer if the caller asked for it, and only when a reply is actually
	// called for: an attendee nobody asked a question of has nothing to say.
	if in.AutoAccept {
		if answer, ok := autoAnswer(stored, who.Address); ok {
			accept := answer
			accept.PartStat = ical.PartStatAccepted
			accept.RSVP = false
			stored.Attendees = replaceAttendee(stored.Attendees, accept)
			result.Reply = ical.BuildReply(stored, accept)
		}
	}
	return result, nil
}

// autoAnswer finds the answer a principal owes on an invitation, if any.
func autoAnswer(ev ical.Event, address string) (ical.Attendee, bool) {
	for _, a := range ev.Attendees {
		if !strings.EqualFold(a.Address, address) {
			continue
		}
		// A tentative or delegated answer stands. Re-answering would overwrite
		// a decision the recipient had already made deliberately.
		if a.PartStat == ical.PartStatTentative || a.PartStat == ical.PartStatAccepted {
			return ical.Attendee{}, false
		}
		if !a.RSVP {
			return ical.Attendee{}, false
		}
		return ical.Attendee{Address: a.Address, Name: a.Name, Role: a.Role}, true
	}
	return ical.Attendee{}, false
}

// applyReply records an answer to an invitation.
func (s *Service) applyReply(ctx context.Context, q store.Querier, ev ical.Event) (Applied, error) {
	result := Applied{Method: ical.MethodReply, UID: ev.UID}

	// A REPLY names the answering party as its sole attendee.
	if len(ev.Attendees) == 0 {
		result.Action = ActionIgnored
		result.Note = "the reply names nobody"
		return result, nil
	}
	answer := ev.Attendees[0]
	if answer.PartStat == "" {
		result.Action = ActionIgnored
		result.Note = "the reply carries no answer"
		return result, nil
	}

	_, err := s.events.SetPartStat(ctx, q, ev.UID, answer.Address, answer.PartStat)
	switch {
	case err == nil:
		result.Action = ActionUpdated
		result.Sequence = ev.Sequence
	case errors.Is(err, store.ErrNotFound):
		// A reply for an invitation this server does not hold is normal: the
		// meeting may have been one this server only copied. Filing it is the
		// mail layer's business, not this one's.
		result.Action = ActionIgnored
		result.Note = "no stored invitation matches that UID and attendee"
	default:
		return result, err
	}
	return result, nil
}

// applyCancel withdraws a whole meeting or one occurrence of it.
func (s *Service) applyCancel(ctx context.Context, q store.Querier, who policy.Principal, ev ical.Event) (Applied, error) {
	result := Applied{Method: ical.MethodCancel, UID: ev.UID}

	cal, err := s.calendars.EnsureDefault(ctx, q, who.Address, calFallback)
	if err != nil {
		return result, err
	}

	existing, err := s.events.ByUID(ctx, q, cal.ID, ev.UID, ev.RecurrenceID)
	switch {
	case err == nil:
		if !IsNewer(ev.Sequence, stampOf(ev, existing.DTStamp), existing.Sequence, existing.DTStamp) {
			result.Action = ActionUnchanged
			result.Sequence = existing.Sequence
			result.Note = "the cancellation is not newer than the stored revision"
			return result, nil
		}
	case errors.Is(err, store.ErrNotFound):
		// Cancelling something that was never here has achieved what was asked,
		// and a copy on a distribution list will produce many of these.
		result.Action = ActionCancelled
		result.Note = "nothing was stored to withdraw"
		return result, nil
	default:
		return result, err
	}

	row, err := s.events.Cancel(ctx, q, existing.ID)
	if err != nil {
		return result, err
	}
	result.Action = ActionCancelled
	result.Sequence = row.Sequence
	return result, nil
}

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

// Outgoing is a scheduling message this server wants to deliver.
type Outgoing struct {
	// Recipients are the addresses that should be sent to. The organizer is
	// included only where a copy is genuinely wanted; sending a REPLY back to
	// the person who just replied to you is the classic way to start a loop.
	Recipients []string
	Document   []byte
	Subject    string
}

// RequestFor builds the REQUEST to send after an organiser creates or changes an
// meeting.
//
// Only the organiser may do this. An attendee changing a meeting is expressing a
// wish, and the way to express it is a REPLY with a counter-proposal, not a
// REQUEST that overwrites the organiser's copy.
func (s *Service) RequestFor(ctx context.Context, q store.Querier, who policy.Principal, id int64) (Outgoing, error) {
	row, err := s.events.Get(ctx, q, id)
	if err != nil {
		return Outgoing{}, err
	}
	if err := s.checkOrganizer(row, who); err != nil {
		return Outgoing{}, err
	}

	ev, err := ical.Parse(row.ICalBlob)
	if err != nil {
		// The blob is what was written, so a parse failure means the stored
		// bytes are damaged. Rebuilding from the columns is possible and wrong:
		// it would replace the record with a partial one.
		return Outgoing{}, fmt.Errorf("%w: the stored document does not parse: %v", ErrBadRequest, err)
	}
	if len(ev.Events) == 0 {
		return Outgoing{}, fmt.Errorf("%w: the stored document has no event", ErrBadRequest)
	}

	return Outgoing{
		Recipients: attendeeAddresses(ev.Events[0], ""),
		Document:   ical.BuildRequest(ev.Events[0]),
		Subject:    subject(ev.Events[0].Summary),
	}, nil
}

// ReplyFor builds the REPLY one attendee sends about a meeting.
func (s *Service) ReplyFor(ctx context.Context, q store.Querier, who policy.Principal, id int64, partstat string) (Outgoing, error) {
	if !validPartStat(partstat) {
		return Outgoing{}, fmt.Errorf("%w: %q is not a participation status", ErrBadRequest, partstat)
	}
	row, err := s.events.Get(ctx, q, id)
	if err != nil {
		return Outgoing{}, err
	}
	doc, ev, err := parseStored(row.ICalBlob)
	if err != nil {
		return Outgoing{}, err
	}
	_ = doc

	answer, ok := attendee(ev, who.Address)
	if !ok {
		return Outgoing{}, fmt.Errorf("%w: %s is not invited to this meeting",
			ErrDenied, who.Address)
	}
	answer.PartStat = partstat
	// Once answered, the question has been dealt with; leaving RSVP set makes
	// the recipient's client keep asking.
	answer.RSVP = false
	ev.Attendees = []ical.Attendee{answer}

	// A reply goes to the organiser, and to nobody else. Sending it to the whole
	// guest list is how one reply becomes five, each of which draws another.
	recipients := []string{}
	if ev.Organizer.Address != "" && !strings.EqualFold(ev.Organizer.Address, who.Address) {
		recipients = append(recipients, ev.Organizer.Address)
	}
	return Outgoing{
		Recipients: recipients,
		Document:   ical.BuildReply(ev, answer),
		Subject:    replySubject(ev),
	}, nil
}

// CancelFor builds the CANCEL for a meeting, or for one occurrence of it.
//
// The sequence is not advanced: RFC 5546 has a CANCEL carry the same SEQUENCE as
// the REQUEST it withdraws, and IsNewer falls back to the timestamp to tell them
// apart. Bumping it would make the two indistinguishable to anyone comparing
// sequences alone.
func (s *Service) CancelFor(ctx context.Context, q store.Querier, who policy.Principal, id int64) (Outgoing, error) {
	row, err := s.events.Get(ctx, q, id)
	if err != nil {
		return Outgoing{}, err
	}
	if err := s.checkOrganizer(row, who); err != nil {
		return Outgoing{}, err
	}
	_, ev, err := parseStored(row.ICalBlob)
	if err != nil {
		return Outgoing{}, err
	}

	ev.Status = ical.StatusCancelled
	ev.ShowAs = ical.ShowAsFree
	// The timestamp is what tells a cancellation apart from the request it
	// withdraws, so it is the present moment rather than the stored one.
	ev.DTStamp = time.Now().UTC().Truncate(time.Second)

	// Every attendee is told, including the ones who never answered: a meeting
	// being called off is exactly the message the people who declined least need
	// to be left out of.
	return Outgoing{
		Recipients: attendeeAddresses(ev, ""),
		Document:   ical.BuildCancel(ev),
		Subject:    cancelSubject(ev),
	}, nil
}

// checkOrganizer refuses a request to act as the organiser of somebody else's
// meeting.
func (s *Service) checkOrganizer(row store.Event, who policy.Principal) error {
	if strings.EqualFold(row.OrganizerAddress, who.Address) {
		return nil
	}
	return fmt.Errorf("%w: %s did not organise this meeting", ErrDenied, who.Address)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ErrBadRequest means the caller — or a message it forwarded — asked for
// something that cannot be done, as opposed to something that is not permitted.
var ErrBadRequest = errors.New("calendar: bad request")

// calFallback is the zone a default calendar is created in when the recipient has
// none. It is UTC rather than the host's local zone, because a calendar whose
// times depend on which machine created it changes meaning when the machine
// moves.
const calFallback = "UTC"

// calTimezone is kept as a function so the fallback has a name at every use site.
func calTimezone(zone string) string {
	if zone == "" {
		return calFallback
	}
	return zone
}

func defaultStatus(status string) string {
	if status == "" {
		return ical.StatusConfirmed
	}
	return status
}

func defaultShowAs(showAs string) string {
	if showAs == "" {
		return ical.ShowAsBusy
	}
	return showAs
}

// stampOf returns an arriving event's DTSTAMP, falling back to a supplied time
// when the message carries none. An undated message is treated as older than one
// that is dated, which is the safe direction: it loses a comparison it could not
// legitimately win.
func stampOf(ev ical.Event, fallback time.Time) time.Time {
	if !ev.DTStamp.IsZero() {
		return ev.DTStamp
	}
	return fallback
}

// parseStored parses a stored document and returns its first event.
func parseStored(blob []byte) (*ical.Document, ical.Event, error) {
	doc, err := ical.Parse(blob)
	if err != nil {
		return nil, ical.Event{}, fmt.Errorf("%w: the stored document does not parse: %v", ErrBadRequest, err)
	}
	if len(doc.Events) == 0 {
		return nil, ical.Event{}, fmt.Errorf("%w: the stored document has no event", ErrBadRequest)
	}
	return doc, doc.Events[0], nil
}

// attendeeAddresses lists an event's attendees, optionally excluding one.
func attendeeAddresses(ev ical.Event, exclude string) []string {
	out := make([]string, 0, len(ev.Attendees))
	for _, a := range ev.Attendees {
		if a.Address == "" || strings.EqualFold(a.Address, exclude) {
			continue
		}
		out = append(out, a.Address)
	}
	return out
}

func attendee(ev ical.Event, address string) (ical.Attendee, bool) {
	for _, a := range ev.Attendees {
		if strings.EqualFold(a.Address, address) {
			return a, true
		}
	}
	return ical.Attendee{}, false
}

func replaceAttendee(list []ical.Attendee, answer ical.Attendee) []ical.Attendee {
	out := make([]ical.Attendee, 0, len(list))
	for _, a := range list {
		if strings.EqualFold(a.Address, answer.Address) {
			out = append(out, answer)
			continue
		}
		out = append(out, a)
	}
	return out
}

func validPartStat(partstat string) bool {
	switch partstat {
	case ical.PartStatAccepted, ical.PartStatDeclined,
		ical.PartStatTentative, ical.PartStatNeedsAction:
		return true
	default:
		return false
	}
}

// The subjects are plain rather than clever. A scheduling message that arrives
// quoted and with a subject a client cannot parse is a message some clients will
// file as a note instead of an invitation.
func subject(summary string) string {
	if strings.TrimSpace(summary) == "" {
		return "Invitation"
	}
	return "Invitation: " + summary
}

func replySubject(ev ical.Event) string {
	if strings.TrimSpace(ev.Summary) == "" {
		return "Re: your invitation"
	}
	return "Re: " + ev.Summary
}

func cancelSubject(ev ical.Event) string {
	if strings.TrimSpace(ev.Summary) == "" {
		return "Cancelled"
	}
	return "Cancelled: " + ev.Summary
}

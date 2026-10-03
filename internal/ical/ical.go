// Package ical reads and writes iCalendar documents, RFC 5545.
//
// The server stores the document it was given and hands that same document
// back to the client that understands it, which is why the parsed model here
// is deliberately a *subset* of VEVENT: a property this package does not know
// about still reaches another client unchanged because the original bytes were
// kept. The model exists to answer questions — who is the organizer, does this
// recur, when is it busy — not to be a second source of truth.
//
// Parsing is delegated to github.com/arran4/golang-ical, which handles line
// unfolding and text escaping correctly. Writing is done here instead, for two
// reasons the library does not cover: it emits bare LF where RFC 5545 §3.1
// requires CRLF, and its SetStartAt always converts to UTC, which would turn a
// 09:00 Europe/London meeting into a 09:00 UTC one. Outlook rejects the first
// outright and shows the wrong hour for the second.
package ical

import (
	"errors"
	"strings"
	"time"
)

// Errors returned by this package. They are all "this document is not usable",
// as opposed to "this document is malformed", which is a caller decision: mail
// carrying an unparseable calendar part is usually still worth delivering.
var (
	// ErrNoCalendar means the bytes are not a VCALENDAR at all.
	ErrNoCalendar = errors.New("ical: not a VCALENDAR")
	// ErrNoEvent means the document is a calendar but carries no VEVENT, so
	// there is nothing to act on.
	ErrNoEvent = errors.New("ical: the calendar carries no VEVENT")
)

// Method is the iTIP METHOD property, which says why a calendar message was
// sent and therefore what the receiver is being asked to do.
type Method string

const (
	// MethodNone is a document with no METHOD: a bare calendar being
	// imported or exported rather than a scheduling message.
	MethodNone Method = ""
	// MethodPublish announces an event with no expectation of a reply.
	MethodPublish Method = "PUBLISH"
	// MethodRequest invites attendees. This is also what carries an update,
	// distinguished by a higher SEQUENCE.
	MethodRequest Method = "REQUEST"
	// MethodReply is an attendee answering a REQUEST.
	MethodReply Method = "REPLY"
	// MethodAdd puts one extra occurrence into an existing recurring event.
	MethodAdd Method = "ADD"
	// MethodCancel withdraws an event or one occurrence of it.
	MethodCancel Method = "CANCEL"
	// MethodRefresh asks for the latest copy after a manual edit.
	MethodRefresh Method = "REFRESH"
)

// Known METHOD values. An unrecognised one is preserved rather than rejected:
// refusing to understand a scheduling message is not a reason to lose the mail.
var knownMethods = map[Method]bool{
	MethodPublish: true, MethodRequest: true, MethodReply: true,
	MethodAdd: true, MethodCancel: true, MethodRefresh: true,
}

// IsKnown reports whether m is a method this server acts on. PUBLISH and the
// iTIP set are handled; anything else is carried but not interpreted.
func (m Method) IsKnown() bool { return knownMethods[m] }

// STATUS values for VEVENT, as far as this server acts on them.
const (
	StatusTentative = "TENTATIVE"
	StatusConfirmed = "CONFIRMED"
	StatusCancelled = "CANCELLED"
)

// PARTICIPATION ROLE values.
const (
	RoleChair          = "CHAIR"
	RoleRequired       = "REQ-PARTICIPANT"
	RoleOptional       = "OPT-PARTICIPANT"
	RoleNonParticipant = "NON-PARTICIPANT"
)

// PARTSTAT values.
const (
	PartStatNeedsAction = "NEEDS-ACTION"
	PartStatAccepted    = "ACCEPTED"
	PartStatDeclined    = "DECLINED"
	PartStatTentative   = "TENTATIVE"
	PartStatDelegated   = "DELEGATED"
)

// TRANSP-derived availability, the property a free/busy query actually reads.
// An event with TRANSP:TRANSPARENT is marked FREE here, which is the one place
// the distinction is recorded.
const (
	ShowAsFree      = "FREE"
	ShowAsTentative = "TENTATIVE"
	ShowAsBusy      = "BUSY"
	ShowAsOOF       = "OOF"
)

// Organizer is the person or calendar that owns an event.
type Organizer struct {
	// Address is a bare email address; the "mailto:" URI scheme is stripped
	// on the way in and restored on the way out.
	Address string
	Name    string
	// SentBy is the ORGANIZER's SENT-BY parameter: a delegate actually acting
	// on the organizer's behalf.
	SentBy string
}

// Attendee is one invited participant.
type Attendee struct {
	Address  string
	Name     string
	Role     string
	PartStat string
	// RSVP asks for an explicit answer, which is what makes a REPLY arrive.
	RSVP bool
	// CUTYPE is INDIVIDUAL, GROUP or RESOURCE, which decides whether a
	// booking consumes one attendee or the whole group's.
	CUTYPE string
	// DelegatedFrom and DelegatedTo record a delegation, so that an answer
	// can be attributed to the delegate rather than lost.
	DelegatedFrom string
	DelegatedTo   string
}

// Event is the part of a VEVENT this server models. Anything not here is still
// present in the stored document.
type Event struct {
	UID         string
	Summary     string
	Description string
	Location    string
	URL         string
	// Start and End are absolute instants. For an all-day event they are
	// midnight UTC on the first and last day, because an all-day event has
	// no instant — it has dates.
	Start time.Time
	End   time.Time
	// AllDay marks a DATE-valued DTSTART, which is excluded from DTEND per
	// RFC 5545: the end date is exclusive, not inclusive.
	AllDay bool
	// Timezone is the TZID the event was written in, kept so the hour a
	// client sees survives a round trip. Empty means UTC.
	Timezone string

	// RRULE is kept as the literal string it arrived as rather than a parsed
	// form, because the server does not need to understand it in order to
	// store and re-serve it, and re-serializing a parsed rule would quietly
	// normalise away the parts it does not model.
	RRULE   string
	ExDates []time.Time
	RDates  []time.Time

	// RecurrenceID is set on an override that replaces one occurrence of a
	// recurring event, and on the CANCEL of a single occurrence.
	RecurrenceID *time.Time

	Sequence int
	Status   string
	// ShowAs is FREE, TENTATIVE, BUSY or OOF.
	ShowAs     string
	Categories []string

	Organizer Organizer
	Attendees []Attendee

	DTStamp      time.Time
	Created      time.Time
	LastModified time.Time
}

// Duration is how long the event lasts. It is derived rather than stored so
// that the two can never disagree.
func (e Event) Duration() time.Duration { return e.End.Sub(e.Start) }

// OrganizerAddress is the organizer's address, which is the routing key for a
// REPLY and the address a REQUEST must come from.
func (e Event) OrganizerAddress() string { return e.Organizer.Address }

// Attendee looks up one invited address.
func (e Event) Attendee(address string) (Attendee, bool) {
	for _, a := range e.Attendees {
		if strings.EqualFold(a.Address, address) {
			return a, true
		}
	}
	return Attendee{}, false
}

// IsCancelled reports whether the event has been withdrawn, which for a stored
// event means the row survives but no longer occupies time.
func (e Event) IsCancelled() bool {
	return e.Status == StatusCancelled
}

// IsFree reports whether the event should be ignored by a free/busy query.
// Transparency, a cancelled event and an out-of-office hint all change the
// answer, and getting it wrong shows a colleague as busy when they are not.
func (e Event) IsFree() bool {
	if e.IsCancelled() {
		return true
	}
	switch e.ShowAs {
	case ShowAsFree, "":
		return true
	default:
		return false
	}
}

// TrimMailto strips the URI scheme from an iCalendar CAL-ADDRESS, which is
// written "mailto:someone@example.com" but stored bare. Comparison is
// case-insensitive because the scheme is.
func TrimMailto(s string) string {
	const prefix = "mailto:"
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):]
	}
	return s
}

// mailto restores the scheme, unless the value is already a different URI,
// which happens with SENT-BY pointing at a directory object.
func mailto(address string) string {
	if address == "" {
		return ""
	}
	if strings.Contains(address, ":") {
		return address
	}
	return "mailto:" + address
}

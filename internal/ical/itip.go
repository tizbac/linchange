package ical

import "time"

// This file builds the three scheduling messages the server sends.
//
// The iTIP rules that shape them are worth stating, because they are the
// difference between an invitation that works and one that lands as a mystery
// duplicate in the attendee's calendar:
//
//   - A REQUEST carries the whole VEVENT, including every attendee, so that a
//     recipient who missed the original can still see who else is invited.
//   - A REPLY carries the whole VEVENT but only the replying attendee. That is
//     deliberate: the organizer needs the event to place the answer, and must
//     not be told what the other attendees said.
//   - A CANCEL carries the event with STATUS:CANCELLED. The row is kept so the
//     message can be sent and so history survives.
//   - SEQUENCE rises on every change and is how a recipient decides which copy
//     is newer. Two messages with the same SEQUENCE are the same revision, so
//     re-sending one must not bump it.

// BuildRequest renders an invitation, or an update to an existing invitation.
//
// An update is the same message with a higher SEQUENCE; there is no separate
// method, which is why an update that arrives with a lower sequence must be
// ignored rather than applied.
func BuildRequest(event Event) []byte {
	e := event
	// A request always names the current revision of the event. An event
	// built by the caller that never set one is treated as the first.
	if e.Sequence < 0 {
		e.Sequence = 0
	}
	return e.MarshalWithMethod(MethodRequest)
}

// BuildReply renders one attendee's answer to an invitation.
//
// Only the answering attendee is included, and their PARTSTAT carries the
// answer. The organizer learns the rest from the other attendees' own replies.
func BuildReply(event Event, answer Attendee) []byte {
	e := event
	e.Attendees = []Attendee{answer}
	return e.MarshalWithMethod(MethodReply)
}

// BuildCancel renders the withdrawal of an event.
//
// Cancelling a single occurrence of a recurring event is done by setting
// RecurrenceID on the copy passed in; the rest of the series is untouched.
func BuildCancel(event Event) []byte {
	e := event
	e.Status = StatusCancelled
	// A cancelled event is not a modification of the invite, so the message
	// reuses the event's own sequence. Bumping it would make some clients
	// treat the cancellation as a newer, different meeting.
	return e.MarshalWithMethod(MethodCancel)
}

// BuildAdd renders the addition of one extra occurrence to a recurring event,
// which is how an attendee splits a single meeting out of a series they have
// declined the rest of.
func BuildAdd(event Event, occurrence time.Time) []byte {
	e := event
	// The occurrence is identified by the RECURRENCE-ID of the slot it fills,
	// which for an addition is the start of that slot.
	rid := occurrence
	e.RecurrenceID = &rid
	return e.MarshalWithMethod(MethodAdd)
}

// Occurrence returns a copy of the event moved to one occurrence of its series,
// for handing to a client that wants discrete occurrences rather than a rule.
//
// The UID is preserved, because the occurrence *is* the same event; it is
// RECURRENCE-ID that tells the client which slot this is. The rule is dropped,
// since an expanded instance must not expand again.
func (e Event) Occurrence(start time.Time, allDay bool) Event {
	out := e
	out.RRULE = ""
	out.RDates = nil
	out.ExDates = nil
	out.Start = start
	out.End = start.Add(e.Duration())
	if !e.End.IsZero() && !e.Start.IsZero() {
		out.End = start.Add(e.Duration())
	}
	rid := start
	out.RecurrenceID = &rid
	out.AllDay = allDay
	return out
}

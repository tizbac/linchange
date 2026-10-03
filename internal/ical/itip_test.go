package ical_test

import (
	"strings"
	"testing"
	"time"

	"github.com/linchange/linchange/internal/ical"
)

func baseEvent() ical.Event {
	start := time.Date(2026, 3, 10, 14, 0, 0, 0, time.UTC)
	return ical.Event{
		UID:       "meeting@example.com",
		Summary:   "Design review",
		Location:  "Room 3",
		Start:     start,
		End:       start.Add(time.Hour),
		DTStamp:   start.Add(-24 * time.Hour),
		Sequence:  1,
		Status:    ical.StatusConfirmed,
		ShowAs:    ical.ShowAsBusy,
		Organizer: ical.Organizer{Address: "alice@example.com", Name: "Alice"},
		Attendees: []ical.Attendee{
			{Address: "bob@example.com", Name: "Bob", Role: ical.RoleRequired,
				PartStat: ical.PartStatAccepted, RSVP: true},
			{Address: "carol@example.com", Name: "Carol", Role: ical.RoleRequired,
				PartStat: ical.PartStatNeedsAction, RSVP: true},
		},
	}
}

func parseOne(t *testing.T, data []byte) (ical.Method, ical.Event) {
	t.Helper()
	doc, err := ical.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, data)
	}
	if len(doc.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(doc.Events))
	}
	return doc.Method, doc.Events[0]
}

// TestBuildRequest checks the invitation carries everything an attendee needs to
// show the meeting, including who else is invited — which is how they recognise
// it as the same meeting they were told about by word of mouth.
func TestBuildRequest(t *testing.T) {
	method, ev := parseOne(t, ical.BuildRequest(baseEvent()))

	if method != ical.MethodRequest {
		t.Errorf("method = %q, want REQUEST", method)
	}
	if ev.UID != "meeting@example.com" {
		t.Errorf("uid = %q", ev.UID)
	}
	if ev.Summary != "Design review" || ev.Location != "Room 3" {
		t.Errorf("summary/location = %q/%q", ev.Summary, ev.Location)
	}
	if ev.Organizer.Address != "alice@example.com" {
		t.Errorf("organizer = %q", ev.Organizer.Address)
	}
	if len(ev.Attendees) != 2 {
		t.Errorf("a REQUEST must list every attendee, got %d", len(ev.Attendees))
	}
	// DTSTAMP is mandatory on anything that travels; a client that receives a
	// scheduling message without one rejects the whole thing.
	if ev.DTStamp.IsZero() {
		t.Error("a REQUEST must carry a DTSTAMP")
	}
}

// TestBuildRequestDefaultsDTStamp covers an event the caller never stamped,
// which must still produce a message clients will accept.
func TestBuildRequestDefaultsDTStamp(t *testing.T) {
	ev := baseEvent()
	ev.DTStamp = time.Time{}
	_, parsed := parseOne(t, ical.BuildRequest(ev))
	if parsed.DTStamp.IsZero() {
		t.Error("a missing DTSTAMP should be filled in, not left absent")
	}
}

// TestBuildReplyCarriesOnlyTheAnswer is the rule that makes replies work: the
// organizer must be told who answered, and must not be handed the other
// attendees' answers as though they were their own.
func TestBuildReplyCarriesOnlyTheAnswer(t *testing.T) {
	answer := ical.Attendee{
		Address:  "bob@example.com",
		Name:     "Bob",
		Role:     ical.RoleRequired,
		PartStat: ical.PartStatAccepted,
	}
	method, ev := parseOne(t, ical.BuildReply(baseEvent(), answer))

	if method != ical.MethodReply {
		t.Errorf("method = %q, want REPLY", method)
	}
	if ev.UID != "meeting@example.com" {
		t.Errorf("a REPLY must identify the event, got %q", ev.UID)
	}
	if !ev.Start.Equal(baseEvent().Start) {
		t.Errorf("a REPLY must carry the event's time, got %v", ev.Start)
	}
	if len(ev.Attendees) != 1 {
		t.Fatalf("a REPLY must name only the replying attendee, got %d", len(ev.Attendees))
	}
	if ev.Attendees[0].Address != "bob@example.com" {
		t.Errorf("attendee = %q", ev.Attendees[0].Address)
	}
	if ev.Attendees[0].PartStat != ical.PartStatAccepted {
		t.Errorf("partstat = %q, want the answer", ev.Attendees[0].PartStat)
	}
	// The organizer is still named: that is where the reply is routed.
	if ev.Organizer.Address != "alice@example.com" {
		t.Errorf("a REPLY must still name the organizer, got %q", ev.Organizer.Address)
	}
}

// TestBuildCancelWithdrawsWithoutLosingTheEvent checks that a cancellation is
// still the same event, marked cancelled. Deleting it instead would leave
// attendees with a meeting that silently vanished.
func TestBuildCancelWithdrawsWithoutLosingTheEvent(t *testing.T) {
	method, ev := parseOne(t, ical.BuildCancel(baseEvent()))

	if method != ical.MethodCancel {
		t.Errorf("method = %q, want CANCEL", method)
	}
	if !ev.IsCancelled() {
		t.Error("a CANCEL must set STATUS:CANCELLED")
	}
	if ev.UID != "meeting@example.com" {
		t.Errorf("a CANCEL must identify the event, got %q", ev.UID)
	}
	if !ev.Start.Equal(baseEvent().Start) {
		t.Errorf("a CANCEL must carry the original time, got %v", ev.Start)
	}
	// The sequence is not bumped: a cancellation withdraws the event rather
	// than proposing a different one, and a higher sequence makes some clients
	// treat it as an update they should refuse.
	if ev.Sequence != baseEvent().Sequence {
		t.Errorf("sequence = %d, want the original %d", ev.Sequence, baseEvent().Sequence)
	}
	if !ev.IsFree() {
		t.Error("a cancelled event must not occupy time")
	}
}

// TestBuildCancelSingleOccurrence covers withdrawing one slot of a series,
// which is done by naming the slot rather than the whole meeting.
func TestBuildCancelSingleOccurrence(t *testing.T) {
	ev := baseEvent()
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=5"
	slot := ev.Start.AddDate(0, 0, 14)
	rid := slot
	ev.RecurrenceID = &rid

	_, got := parseOne(t, ical.BuildCancel(ev))
	if got.RecurrenceID == nil {
		t.Fatal("the cancelled slot must be named by RECURRENCE-ID")
	}
	if !got.RecurrenceID.Equal(slot) {
		t.Errorf("recurrence-id = %v, want %v", got.RecurrenceID, slot)
	}
	// The series itself survives: only the named occurrence goes.
	if got.RRULE != "FREQ=WEEKLY;BYDAY=TU;COUNT=5" {
		t.Errorf("rrule = %q, the series must be preserved", got.RRULE)
	}
}

// TestOccurrenceExpandsOneSlot covers handing a client a single expanded
// instance of a recurring event, which is how a week view gets its Tuesday.
func TestOccurrenceExpandsOneSlot(t *testing.T) {
	ev := baseEvent()
	ev.RRULE = "FREQ=WEEKLY;BYDAY=TU;COUNT=5"
	start := ev.Start.AddDate(0, 0, 7)

	one := ev.Occurrence(start, false)

	if !one.Start.Equal(start) {
		t.Errorf("start = %v, want %v", one.Start, start)
	}
	if got := one.Duration(); got != time.Hour {
		t.Errorf("duration = %v, an occurrence keeps the series' duration", got)
	}
	if one.UID != ev.UID {
		t.Error("an occurrence is the same event, so the UID must not change")
	}
	if one.RecurrenceID == nil || !one.RecurrenceID.Equal(start) {
		t.Error("an occurrence must name the slot it fills")
	}
	// An expanded instance that still carried the rule would expand again.
	if one.RRULE != "" {
		t.Errorf("rrule = %q, an expanded occurrence must not recur further", one.RRULE)
	}
}

// TestMarshalOmitsMethodForAPlainExport keeps a downloaded .ics from looking
// like a scheduling message, which is what makes clients show import prompts
// for a file that is really just a backup.
func TestMarshalOmitsMethodForAPlainExport(t *testing.T) {
	out := string(baseEvent().Marshal())
	if strings.Contains(out, "METHOD:") {
		t.Errorf("a plain export must carry no METHOD:\n%s", out)
	}
	if !strings.HasPrefix(out, "BEGIN:VCALENDAR\r\n") {
		t.Error("a document must begin with BEGIN:VCALENDAR")
	}
}

// TestEveryBuilderIsAValidDocument holds the three builders to the same
// well-formedness bar, since each one is mail a client has to parse.
func TestEveryBuilderIsAValidDocument(t *testing.T) {
	builders := map[string]func() []byte{
		"REQUEST": func() []byte { return ical.BuildRequest(baseEvent()) },
		"REPLY": func() []byte {
			return ical.BuildReply(baseEvent(), ical.Attendee{
				Address: "bob@example.com", PartStat: ical.PartStatDeclined,
			})
		},
		"CANCEL": func() []byte { return ical.BuildCancel(baseEvent()) },
		"ADD":    func() []byte { return ical.BuildAdd(baseEvent(), baseEvent().Start.AddDate(0, 0, 21)) },
		"PLAIN":  func() []byte { return baseEvent().Marshal() },
		"ALL-DAY": func() []byte {
			e := baseEvent()
			e.AllDay = true
			e.Timezone = ""
			e.Start = time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
			e.End = time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)
			return ical.BuildRequest(e)
		},
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			out := build()
			// CRLF, no bare LF.
			if !strings.Contains(string(out), "\r\n") {
				t.Error("output has no CRLF")
			}
			for i, r := range string(out) {
				if r == '\n' && (i == 0 || out[i-1] != '\r') {
					t.Fatalf("bare LF at %d", i)
				}
			}
			// No physical line over the limit, and every continuation is
			// introduced by exactly one space.
			for _, line := range strings.Split(string(out), "\r\n") {
				if len(line) > 75 && !strings.HasPrefix(line, " ") {
					t.Errorf("line of %d octets is not folded: %q", len(line), line)
				}
			}
			// And it parses back.
			if _, err := ical.Parse(out); err != nil {
				t.Errorf("output does not parse: %v", err)
			}
		})
	}
}

// TestUnknownMethodIsCarriedNotRejected covers a client inventing a method:
// the mail is still a calendar and must still be deliverable.
func TestUnknownMethodIsCarriedNotRejected(t *testing.T) {
	doc, err := ical.Parse([]byte(
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nMETHOD:COUNTERPROPOSE\r\n" +
			"BEGIN:VEVENT\r\nUID:x@example.com\r\nDTSTAMP:20260201T120000Z\r\n" +
			"DTSTART:20260210T090000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatalf("an unknown METHOD must not fail the parse: %v", err)
	}
	if doc.Method != ical.Method("COUNTERPROPOSE") {
		t.Errorf("method = %q", doc.Method)
	}
	if doc.Method.IsKnown() {
		t.Error("COUNTERPROPOSE is not a method this server acts on")
	}
	if !ical.MethodRequest.IsKnown() {
		t.Error("REQUEST should be known")
	}
}

// TestMethodIsCaseInsensitive: RFC 5545 tokens are case-insensitive, and some
// clients write "Method:Request".
func TestMethodIsCaseInsensitive(t *testing.T) {
	doc, err := ical.Parse([]byte(
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nmethod:request\r\n" +
			"BEGIN:VEVENT\r\nUID:x@example.com\r\nDTSTAMP:20260201T120000Z\r\n" +
			"DTSTART:20260210T090000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Method != ical.MethodRequest {
		t.Errorf("method = %q, want REQUEST", doc.Method)
	}
}

// TestRecurrenceOverridesArePreserved covers the detached instance form a
// client sends when one occurrence of a series has moved.
func TestRecurrenceOverridesArePreserved(t *testing.T) {
	slot := time.Date(2026, 3, 17, 14, 0, 0, 0, time.UTC)
	ev := onlyEvent(t, mustParse(t,
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nBEGIN:VEVENT\r\n"+
			"UID:series@example.com\r\nDTSTAMP:20260201T120000Z\r\nSEQUENCE:3\r\n"+
			"DTSTART:20260310T140000Z\r\nDTEND:20260310T150000Z\r\n"+
			"RRULE:FREQ=WEEKLY;BYDAY=TU;COUNT=5\r\n"+
			"RECURRENCE-ID:20260317T140000Z\r\n"+
			"SUMMARY:Moved to the 17th\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))

	if ev.RecurrenceID == nil {
		t.Fatal("RECURRENCE-ID was dropped")
	}
	if !ev.RecurrenceID.Equal(slot) {
		t.Errorf("recurrence-id = %v, want %v", ev.RecurrenceID, slot)
	}
	if ev.Sequence != 3 {
		t.Errorf("sequence = %d, an override carries its own revision", ev.Sequence)
	}
}

// TestExAndRDateArePreserved covers a series with holes and additions, which is
// how a client expresses "skip the 24th but add the 31st".
func TestExAndRDateArePreserved(t *testing.T) {
	ev := onlyEvent(t, mustParse(t,
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nBEGIN:VEVENT\r\n"+
			"UID:holes@example.com\r\nDTSTAMP:20260201T120000Z\r\n"+
			"DTSTART:20260310T140000Z\r\nDTEND:20260310T150000Z\r\n"+
			"RRULE:FREQ=WEEKLY;BYDAY=TU\r\n"+
			"EXDATE:20260324T140000Z\r\n"+
			"RDATE:20260331T140000Z\r\n"+
			"SUMMARY:With holes\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))

	if len(ev.ExDates) != 1 {
		t.Errorf("exdates = %d, want 1", len(ev.ExDates))
	}
	if len(ev.RDates) != 1 {
		t.Errorf("rdates = %d, want 1", len(ev.RDates))
	}
	if !ev.RDates[0].Equal(time.Date(2026, 3, 31, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("rdate = %v", ev.RDates[0])
	}

	again := onlyEvent(t, mustParse(t, string(ev.Marshal())))
	if len(again.ExDates) != 1 || len(again.RDates) != 1 {
		t.Errorf("ex/rdates lost on rewrite: %d/%d", len(again.ExDates), len(again.RDates))
	}
}

// TestLongTextIsFoldedNotTruncated guards against a summary being silently
// shortened to fit the line limit, which is the failure mode a naive fold
// produces.
func TestLongTextIsFoldedNotTruncated(t *testing.T) {
	long := strings.Repeat("Quarterly planning and review notes. ", 20)
	ev := baseEvent()
	ev.Summary = long

	parsed := onlyEvent(t, mustParse(t, string(ev.Marshal())))
	if parsed.Summary != long {
		t.Errorf("a long summary was altered: got %d chars, want %d",
			len(parsed.Summary), len(long))
	}
}

// TestMultiByteTextIsFoldedOnCharacterBoundaries covers the case that makes a
// byte-wise fold corrupt the document: a fold that lands inside a UTF-8
// sequence produces bytes that are not valid UTF-8 and are lost on parse.
func TestMultiByteTextIsFoldedOnCharacterBoundaries(t *testing.T) {
	// Emoji are four bytes each, so a naive 75-byte cut lands mid-character.
	long := strings.Repeat("planning \U0001F4C5 ", 30)
	ev := baseEvent()
	ev.Description = long

	parsed := onlyEvent(t, mustParse(t, string(ev.Marshal())))
	if parsed.Description != long {
		t.Errorf("multi-byte text was corrupted: got %d chars, want %d",
			len([]rune(parsed.Description)), len([]rune(long)))
	}
}

// TestRSVPIsWrittenAsBareTrue pins the parameter form. A quoted or lowercased
// RSVP is ignored by Outlook, which then never sends a reply.
func TestRSVPIsWrittenAsBareTrue(t *testing.T) {
	out := string(ical.BuildRequest(baseEvent()))
	if !strings.Contains(out, "RSVP=TRUE") {
		t.Errorf("RSVP must be a bare TRUE:\n%s", out)
	}
	if strings.Contains(out, `RSVP="TRUE"`) {
		t.Error("RSVP must not be quoted")
	}
	if strings.Contains(out, "RSVP=true") {
		t.Error("RSVP is a case-insensitive token but TRUE is conventional")
	}
}

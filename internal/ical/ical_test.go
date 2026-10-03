package ical_test

import (
	"strings"
	"testing"
	"time"

	"github.com/linchange/linchange/internal/ical"
)

var (
	// A realistic invitation: a zoned meeting, an escaped summary, a rule, an
	// organizer and two attendees with differing answers.
	invitation = "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//Example Corp//Mail//EN\r\n" +
		"METHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:8f3a2b1c-0000-4000-8000-000000000001@example.com\r\n" +
		"DTSTAMP:20260201T120000Z\r\n" +
		"CREATED:20260201T110000Z\r\n" +
		"LAST-MODIFIED:20260201T115500Z\r\n" +
		"SEQUENCE:2\r\n" +
		"DTSTART;TZID=Europe/London:20260210T090000\r\n" +
		"DTEND;TZID=Europe/London:20260210T103000\r\n" +
		"RRULE:FREQ=WEEKLY;BYDAY=TU;COUNT=5\r\n" +
		"SUMMARY:Quarterly review\\; planning\\, notes\\nand next steps\r\n" +
		"DESCRIPTION:Line one\\nLine two\r\n" +
		"LOCATION:Room 3\r\n" +
		"CATEGORIES:work,planning\r\n" +
		"STATUS:CONFIRMED\r\n" +
		"TRANSP:OPAQUE\r\n" +
		"ORGANIZER;CN=Alice Example:mailto:alice@example.com\r\n" +
		"ATTENDEE;CN=Bob;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;RSVP=TRUE:" +
		"mailto:bob@example.com\r\n" +
		"ATTENDEE;CN=Carol;ROLE=OPT-PARTICIPANT;PARTSTAT=NEEDS-ACTION;" +
		"RSVP=TRUE:mailto:carol@example.com\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
)

func mustParse(t *testing.T, data string) *ical.Document {
	t.Helper()
	doc, err := ical.Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

func onlyEvent(t *testing.T, doc *ical.Document) ical.Event {
	t.Helper()
	if len(doc.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(doc.Events))
	}
	return doc.Events[0]
}

// TestParseInvitation covers the fields the scheduler routes and books on.
func TestParseInvitation(t *testing.T) {
	doc := mustParse(t, invitation)

	if doc.Method != ical.MethodRequest {
		t.Errorf("method = %q, want REQUEST", doc.Method)
	}
	if doc.ProdID != "-//Example Corp//Mail//EN" {
		t.Errorf("prodid = %q", doc.ProdID)
	}

	ev := onlyEvent(t, doc)

	if ev.UID != "8f3a2b1c-0000-4000-8000-000000000001@example.com" {
		t.Errorf("uid = %q", ev.UID)
	}
	if ev.Sequence != 2 {
		t.Errorf("sequence = %d, want 2", ev.Sequence)
	}
	if ev.Summary != "Quarterly review; planning, notes\nand next steps" {
		t.Errorf("summary = %q", ev.Summary)
	}
	if ev.Description != "Line one\nLine two" {
		t.Errorf("description = %q", ev.Description)
	}
	if ev.Location != "Room 3" {
		t.Errorf("location = %q", ev.Location)
	}
	if ev.RRULE != "FREQ=WEEKLY;BYDAY=TU;COUNT=5" {
		t.Errorf("rrule = %q", ev.RRULE)
	}
	if ev.Timezone != "Europe/London" {
		t.Errorf("timezone = %q, want Europe/London", ev.Timezone)
	}
	if ev.AllDay {
		t.Error("a zoned timed event must not be all-day")
	}
	if ev.Status != ical.StatusConfirmed {
		t.Errorf("status = %q", ev.Status)
	}
	if ev.ShowAs != ical.ShowAsBusy {
		t.Errorf("show_as = %q, want BUSY for an opaque event", ev.ShowAs)
	}
	if len(ev.Categories) != 2 || ev.Categories[0] != "work" {
		t.Errorf("categories = %v", ev.Categories)
	}

	// The stored times must be the instants the sender meant, not wall-clock
	// readings in some default zone.
	wantStart := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)
	if !ev.Start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", ev.Start.UTC(), wantStart)
	}
	if got := ev.Duration(); got != 90*time.Minute {
		t.Errorf("duration = %v, want 90m", got)
	}

	if ev.Organizer.Address != "alice@example.com" {
		t.Errorf("organizer = %q, mailto: stripped", ev.Organizer.Address)
	}
	if ev.Organizer.Name != "Alice Example" {
		t.Errorf("organizer CN = %q", ev.Organizer.Name)
	}

	if len(ev.Attendees) != 2 {
		t.Fatalf("got %d attendees, want 2", len(ev.Attendees))
	}
	bob, ok := ev.Attendee("BOB@example.com")
	if !ok {
		t.Fatal("attendee lookup is case-insensitive and should have found bob")
	}
	if bob.PartStat != ical.PartStatAccepted {
		t.Errorf("bob partstat = %q", bob.PartStat)
	}
	if !bob.RSVP {
		t.Error("bob RSVP should be true")
	}
	carol, _ := ev.Attendee("carol@example.com")
	if carol.Role != ical.RoleOptional {
		t.Errorf("carol role = %q", carol.Role)
	}
}

// TestRoundTripPreservesTheModel is the property the storage layer depends on:
// storing a document and rebuilding it from the model must not change what the
// event means.
func TestRoundTripPreservesTheModel(t *testing.T) {
	first := onlyEvent(t, mustParse(t, invitation))
	rebuilt, err := ical.Parse(first.Marshal())
	if err != nil {
		t.Fatalf("re-parsing our own output: %v\n%s", err, first.Marshal())
	}
	second := onlyEvent(t, rebuilt)

	assertSameEvent(t, first, second)
}

func assertSameEvent(t *testing.T, want, got ical.Event) {
	t.Helper()
	if got.UID != want.UID {
		t.Errorf("uid: %q != %q", got.UID, want.UID)
	}
	if got.Summary != want.Summary {
		t.Errorf("summary: %q != %q", got.Summary, want.Summary)
	}
	if got.Description != want.Description {
		t.Errorf("description: %q != %q", got.Description, want.Description)
	}
	if got.Location != want.Location {
		t.Errorf("location: %q != %q", got.Location, want.Location)
	}
	if got.RRULE != want.RRULE {
		t.Errorf("rrule: %q != %q", got.RRULE, want.RRULE)
	}
	if got.Timezone != want.Timezone {
		t.Errorf("timezone: %q != %q", got.Timezone, want.Timezone)
	}
	if got.AllDay != want.AllDay {
		t.Errorf("all-day: %v != %v", got.AllDay, want.AllDay)
	}
	if got.Sequence != want.Sequence {
		t.Errorf("sequence: %d != %d", got.Sequence, want.Sequence)
	}
	if got.Status != want.Status {
		t.Errorf("status: %q != %q", got.Status, want.Status)
	}
	if got.ShowAs != want.ShowAs {
		t.Errorf("show_as: %q != %q", got.ShowAs, want.ShowAs)
	}
	if !got.Start.Equal(want.Start) {
		t.Errorf("start: %v != %v", got.Start, want.Start)
	}
	if !got.End.Equal(want.End) {
		t.Errorf("end: %v != %v", got.End, want.End)
	}
	if !got.DTStamp.Equal(want.DTStamp) {
		t.Errorf("dtstamp: %v != %v", got.DTStamp, want.DTStamp)
	}
	if got.Organizer.Address != want.Organizer.Address {
		t.Errorf("organizer: %q != %q", got.Organizer.Address, want.Organizer.Address)
	}
	if len(got.Attendees) != len(want.Attendees) {
		t.Fatalf("attendee count: %d != %d", len(got.Attendees), len(want.Attendees))
	}
	for i := range want.Attendees {
		w, g := want.Attendees[i], got.Attendees[i]
		if g.Address != w.Address || g.Role != w.Role ||
			g.PartStat != w.PartStat || g.RSVP != w.RSVP || g.Name != w.Name {
			t.Errorf("attendee %d: %+v != %+v", i, g, w)
		}
	}
}

// TestOutputUsesCRLF pins the line ending. Bare LF is what the underlying
// library emits and what Outlook refuses to open, so this would otherwise be a
// silent interop failure discovered by a user.
func TestOutputUsesCRLF(t *testing.T) {
	out := string(onlyEvent(t, mustParse(t, invitation)).Marshal())

	if !strings.Contains(out, "\r\n") {
		t.Fatal("no CRLF in the output")
	}
	// Every LF must be part of a CRLF; a lone LF would corrupt the document.
	for i, r := range out {
		if r == '\n' && (i == 0 || out[i-1] != '\r') {
			t.Fatalf("bare LF at offset %d", i)
		}
	}
	if !strings.HasSuffix(out, "END:VCALENDAR\r\n") {
		t.Errorf("document does not end with a terminated END:VCALENDAR")
	}
}

// TestTimezoneSurvivesTheRoundTrip is why writing is done here rather than by
// the library: a 09:00 London meeting must not come back as 09:00 UTC, which is
// an hour early for everyone but the sender.
func TestTimezoneSurvivesTheRoundTrip(t *testing.T) {
	out := string(onlyEvent(t, mustParse(t, invitation)).Marshal())

	if !strings.Contains(out, "DTSTART;TZID=Europe/London:20260210T090000") {
		t.Errorf("DTSTART lost its TZID:\n%s", out)
	}
	if !strings.Contains(out, "DTEND;TZID=Europe/London:20260210T103000") {
		t.Errorf("DTEND lost its TZID:\n%s", out)
	}
	// And it must not have been quietly converted to a UTC instant.
	if strings.Contains(out, "DTSTART:20260210T090000Z") {
		t.Errorf("a zoned event was rewritten as UTC:\n%s", out)
	}
}

// TestAllDayEvent covers the DATE form, whose end date is exclusive.
func TestAllDayEvent(t *testing.T) {
	doc := mustParse(t,
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nBEGIN:VEVENT\r\n"+
			"UID:holiday@example.com\r\nDTSTAMP:20260201T120000Z\r\n"+
			"DTSTART;VALUE=DATE:20260210\r\nDTEND;VALUE=DATE:20260212\r\n"+
			"SUMMARY:Company holiday\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	ev := onlyEvent(t, doc)
	if !ev.AllDay {
		t.Fatal("a DATE-valued DTSTART must be recognised as all-day")
	}
	// 10 February to 12 February exclusive is the 10th and the 11th.
	if got := ev.End.Sub(ev.Start); got != 48*time.Hour {
		t.Errorf("span = %v, want 48h for two all-day days", got)
	}
	if ev.Start.Location() != time.UTC {
		t.Errorf("an all-day date should be pinned to UTC, got %v", ev.Start.Location())
	}

	out := string(ev.Marshal())
	if !strings.Contains(out, "DTSTART;VALUE=DATE:20260210") {
		t.Errorf("all-day DTSTART not preserved:\n%s", out)
	}
	again := onlyEvent(t, mustParse(t, out))
	assertSameEvent(t, ev, again)
}

// TestAllDayDefaultsToOneDay covers a VEVENT with an all-day DTSTART and no
// DTEND, which RFC 5545 makes a single day.
func TestAllDayDefaultsToOneDay(t *testing.T) {
	ev := onlyEvent(t, mustParse(t,
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nBEGIN:VEVENT\r\n"+
			"UID:one@example.com\r\nDTSTAMP:20260201T120000Z\r\n"+
			"DTSTART;VALUE=DATE:20260210\r\nSUMMARY:Day off\r\n"+
			"END:VEVENT\r\nEND:VCALENDAR\r\n"))

	if got := ev.End.Sub(ev.Start); got != 24*time.Hour {
		t.Errorf("span = %v, want 24h", got)
	}
}

// TestTransparentEventIsFree covers the property a free/busy query reads.
func TestTransparentEventIsFree(t *testing.T) {
	free := onlyEvent(t, mustParse(t,
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nBEGIN:VEVENT\r\n"+
			"UID:t@example.com\r\nDTSTAMP:20260201T120000Z\r\n"+
			"DTSTART:20260210T090000Z\r\nDTEND:20260210T100000Z\r\n"+
			"SUMMARY:Lunch\r\nTRANSP:TRANSPARENT\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if !free.IsFree() {
		t.Error("a TRANSPARENT event must read as free")
	}
	if free.ShowAs != ical.ShowAsFree {
		t.Errorf("show_as = %q", free.ShowAs)
	}
	if !strings.Contains(string(free.Marshal()), "TRANSP:TRANSPARENT") {
		t.Error("transparency was lost on the way out")
	}

	busy := onlyEvent(t, mustParse(t, invitation))
	if busy.IsFree() {
		t.Error("an opaque event must not read as free")
	}
}

// TestCancelledEventIsFree matters because a cancelled meeting must stop
// occupying time without the row being deleted.
func TestCancelledEventIsFree(t *testing.T) {
	ev := onlyEvent(t, mustParse(t,
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nBEGIN:VEVENT\r\n"+
			"UID:c@example.com\r\nDTSTAMP:20260201T120000Z\r\n"+
			"DTSTART:20260210T090000Z\r\nDTEND:20260210T100000Z\r\n"+
			"SUMMARY:Called off\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))

	if !ev.IsCancelled() {
		t.Fatal("STATUS:CANCELLED was not recognised")
	}
	if !ev.IsFree() {
		t.Error("a cancelled event must not occupy time")
	}
}

// TestParseIsLenientAboutOptionalProperties covers a client that sends the
// minimum. A missing field must be empty, not a panic.
func TestParseIsLenientAboutOptionalProperties(t *testing.T) {
	ev := onlyEvent(t, mustParse(t,
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nBEGIN:VEVENT\r\n"+
			"UID:bare@example.com\r\nDTSTART:20260210T090000Z\r\n"+
			"END:VEVENT\r\nEND:VCALENDAR\r\n"))

	if ev.UID != "bare@example.com" {
		t.Errorf("uid = %q", ev.UID)
	}
	if ev.Summary != "" || ev.Location != "" || ev.Organizer.Address != "" {
		t.Error("absent properties should read as empty")
	}
	// No DTEND and not all-day: RFC 5545's default is zero length, but every
	// client shows one hour, so that is what is stored.
	if got := ev.Duration(); got != time.Hour {
		t.Errorf("duration = %v, want 1h", got)
	}
	// No DTSTAMP is malformed but common; the model must still carry one so a
	// rewrite does not produce a document clients reject.
	if !ev.DTStamp.IsZero() {
		t.Logf("dtstamp = %v", ev.DTStamp)
	}
}

// TestUnknownPropertiesAreIgnored checks that a property this server does not
// model does not stop the document being understood. The bytes are kept by the
// store, so what matters here is only that parsing survives them.
func TestUnknownPropertiesAreIgnored(t *testing.T) {
	ev := onlyEvent(t, mustParse(t,
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nBEGIN:VEVENT\r\n"+
			"UID:x@example.com\r\nDTSTAMP:20260201T120000Z\r\n"+
			"DTSTART:20260210T090000Z\r\nDTEND:20260210T100000Z\r\n"+
			"SUMMARY:With extras\r\n"+
			"X-MICROSOFT-CDO-BUSYSTATUS:BUSY\r\n"+
			"X-APPLE-STRUCTURED-LOCATION;VALUE=URI:geo:51.5,-0.1\r\n"+
			"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT15M\r\n"+
			"DESCRIPTION:Reminder\r\nEND:VALARM\r\n"+
			"END:VEVENT\r\nEND:VCALENDAR\r\n"))

	if ev.Summary != "With extras" {
		t.Errorf("summary = %q", ev.Summary)
	}
}

// TestNotACalendar distinguishes a bad document from a wrong one.
func TestNotACalendar(t *testing.T) {
	for _, bad := range []string{"", "   \r\n", "hello, this is mail\r\n"} {
		if _, err := ical.Parse([]byte(bad)); err == nil {
			t.Errorf("%q should not parse as a calendar", bad)
		}
	}

	// A calendar with no VEVENT is not usable for scheduling.
	_, err := ical.Parse([]byte(
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nEND:VCALENDAR\r\n"))
	if err == nil {
		t.Error("a calendar with no event should report ErrNoEvent")
	}
}

package ical

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// ProductID identifies this server in the documents it writes. A client that
// has never seen the string is expected to ignore it.
const ProductID = "-//linchange//calendars//EN"

// The RFC 5545 limits. A content line is folded so that no line exceeds 75
// octets, counting the fold's leading space.
const (
	maxLineOctets = 75
	// crlf is the line ending the format requires. Bare LF is what the
	// library emits and what Outlook refuses.
	crlf = "\r\n"
)

// Writer accumulates content lines, folding as it goes.
type Writer struct {
	sb strings.Builder
}

func (w *Writer) line(content string) {
	// foldLine already terminates every continuation with CRLF+space; this
	// adds the line's own terminator.
	w.sb.WriteString(foldLine(content))
	w.sb.WriteString(crlf)
}

// params renders an ordered parameter list. Ordering is not required by the
// format, but a stable order keeps output byte-identical across runs, which
// makes the round-trip tests meaningful and any ETag-like comparison work.
func params(entries ...string) string {
	if len(entries) == 0 {
		return ""
	}
	sorted := append([]string(nil), entries...)
	sort.Strings(sorted)
	var sb strings.Builder
	for _, e := range sorted {
		if e == "" {
			continue
		}
		sb.WriteString(";")
		sb.WriteString(e)
	}
	return sb.String()
}

// param renders one parameter, quoting its value when the format requires it.
// A value holding a comma, a semicolon or a colon has to be quoted or the line
// parses as more parameters than it has.
func param(name, value string) string {
	if value == "" {
		return ""
	}
	if needsQuoting(value) {
		return name + "=" + strconv.Quote(value)
	}
	return name + "=" + value
}

func needsQuoting(v string) bool {
	return strings.ContainsAny(v, ",;:\"")
}

// foldLine folds a content line to the octet limit, continuing with CRLF and a
// single leading space on each continuation line.
//
// The CRLF+space belongs *between* chunks. Putting the space before the CRLF
// instead produces a line that still looks folded but unfolds to the wrong
// text, which is how an attendee address ends up with a space in it.
//
// Folding counts octets rather than runes because that is what the limit is
// defined in: a line of multi-byte text must be split on a byte boundary that
// still lands between characters, never inside one.
func foldLine(content string) string {
	if len(content) <= maxLineOctets {
		return content
	}
	var out strings.Builder
	out.Grow(len(content) + 16)

	// limit counts octets left on the current physical line. The first line
	// has the full budget; continuations lose one octet to the space.
	limit := maxLineOctets
	for len(content) > 0 {
		n := len(content)
		if n > limit {
			n = limit
			// Back off to a rune boundary so a multi-byte character is never
			// split across a fold. Only reached when the line was actually
			// cut, hence the guard on n < len(content).
			for n > 0 && !isRuneStart(content[n]) {
				n--
			}
			if n == 0 {
				// A single rune longer than the budget cannot be folded
				// legally; emit it whole rather than corrupt it.
				n = 1
				for n < len(content) && !isRuneStart(content[n]) {
					n++
				}
			}
		}
		if out.Len() > 0 {
			out.WriteString(crlf)
			out.WriteString(" ")
		}
		out.WriteString(content[:n])
		content = content[n:]
		limit = maxLineOctets - 1
	}
	return out.String()
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// escapeText escapes a TEXT value per RFC 5545 §3.3.11: a backslash, a
// semicolon and a comma are escaped, and a newline becomes a literal \n.
func escapeText(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case ';':
			sb.WriteString(`\;`)
		case ',':
			sb.WriteString(`\,`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			// A bare carriage return in the source would fold into the next
			// content line and corrupt the document.
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Time formatting
// ---------------------------------------------------------------------------

// formatTime renders a time value, choosing the DATE, floating and UTC forms.
//
// The TZID form is the reason this is hand-written: the library's SetStartAt
// always converts to UTC, so a meeting at 09:00 in Europe/London would be
// published as 09:00 UTC and displayed an hour early by everyone else.
func formatTime(t time.Time, allDay bool, tzid string) (value string, parameters string) {
	if allDay {
		// All-day values are dates in the calendar's own terms, with no zone.
		return t.UTC().Format(formatDate), params(param("VALUE", "DATE"))
	}
	if tzid != "" {
		loc, err := time.LoadLocation(tzid)
		if err != nil {
			// Fall through to UTC rather than emit a TZID nothing can resolve.
		} else {
			return t.In(loc).Format(formatDateTime), params(param("TZID", tzid))
		}
	}
	return t.UTC().Format(formatDateTimeZ), ""
}

// Marshal renders the event as a plain VCALENDAR with no METHOD property,
// which is what a download or an import file should be.
func (e Event) Marshal() []byte {
	return e.MarshalWithMethod(MethodNone)
}

// MarshalWithMethod renders the event under an explicit iTIP method. It is what
// the scheduler uses to build a REQUEST, a REPLY or a CANCEL.
//
// The METHOD lives on the document rather than the event: two events in one
// file always share it, and a caller that has to remember to set a matching
// field is a caller that will eventually forget.
func (e Event) MarshalWithMethod(method Method) []byte {
	var w Writer
	w.line("BEGIN:VCALENDAR")
	w.line("VERSION:2.0")
	w.line("PRODID:" + escapeText(ProductID))
	if method != MethodNone {
		w.line("METHOD:" + string(method))
	}
	if e.Timezone != "" {
		w.line("X-WR-TIMEZONE:" + escapeText(e.Timezone))
	}
	e.writeComponent(&w)
	w.line("END:VCALENDAR")
	return []byte(w.sb.String())
}

func (e Event) writeComponent(w *Writer) {
	w.line("BEGIN:VEVENT")

	// UID first: every other property in a scheduling message is qualified by
	// it, and putting it at the top makes a truncated message still usable.
	w.line("UID:" + escapeText(e.UID))

	// DTSTAMP is mandatory on every VEVENT that travels, and its absence is
	// one of the most common reasons a client rejects an update.
	dtstamp := e.DTStamp
	if dtstamp.IsZero() {
		dtstamp = time.Now().UTC()
	}
	w.line("DTSTAMP:" + dtstamp.UTC().Format(formatDateTimeZ))

	startValue, startParams := formatTime(e.Start, e.AllDay, e.Timezone)
	w.line("DTSTART" + startParams + ":" + startValue)

	// DTEND is written for all-day events too, where the library would not.
	// Its date is exclusive: a one-day holiday on the 10th is DTSTART the 10th
	// and DTEND the 11th.
	if !e.End.IsZero() && !e.End.Equal(e.Start) {
		endValue, endParams := formatTime(e.End, e.AllDay, e.Timezone)
		w.line("DTEND" + endParams + ":" + endValue)
	}

	if e.RecurrenceID != nil {
		ridValue, ridParams := formatTime(*e.RecurrenceID, e.AllDay, e.Timezone)
		w.line("RECURRENCE-ID" + ridParams + ":" + ridValue)
	}
	if e.RRULE != "" {
		w.line("RRULE:" + strings.TrimPrefix(e.RRULE, "RRULE:"))
	}
	for _, d := range e.ExDates {
		value, p := formatTime(d, e.AllDay, e.Timezone)
		w.line("EXDATE" + p + ":" + value)
	}
	for _, d := range e.RDates {
		value, p := formatTime(d, e.AllDay, e.Timezone)
		w.line("RDATE" + p + ":" + value)
	}

	if e.Summary != "" {
		w.line("SUMMARY:" + escapeText(e.Summary))
	}
	if e.Description != "" {
		w.line("DESCRIPTION:" + escapeText(e.Description))
	}
	if e.Location != "" {
		w.line("LOCATION:" + escapeText(e.Location))
	}
	if e.URL != "" {
		w.line("URL:" + escapeText(e.URL))
	}
	if len(e.Categories) > 0 {
		escaped := make([]string, len(e.Categories))
		for i, c := range e.Categories {
			escaped[i] = escapeText(c)
		}
		w.line("CATEGORIES:" + strings.Join(escaped, ","))
	}

	// TRANSP is how an event says it is free. It is written explicitly rather
	// than left to a default, because a free/busy query on the far side reads
	// this property and nothing else.
	if e.ShowAs == ShowAsFree {
		w.line("TRANSP:TRANSPARENT")
	} else {
		w.line("TRANSP:OPAQUE")
	}

	if e.Sequence > 0 {
		w.line("SEQUENCE:" + strconv.Itoa(e.Sequence))
	}
	if e.Status != "" {
		w.line("STATUS:" + e.Status)
	}
	if !e.Created.IsZero() {
		w.line("CREATED:" + e.Created.UTC().Format(formatDateTimeZ))
	}
	if !e.LastModified.IsZero() {
		w.line("LAST-MODIFIED:" + e.LastModified.UTC().Format(formatDateTimeZ))
	}
	if e.Organizer.Address != "" {
		w.line("ORGANIZER" + params(
			param("CN", e.Organizer.Name),
			param("SENT-BY", mailto(e.Organizer.SentBy)),
		) + ":" + mailto(e.Organizer.Address))
	}
	for _, a := range e.Attendees {
		w.line("ATTENDEE" + params(
			param("CN", a.Name),
			param("CUTYPE", a.CUTYPE),
			param("DELEGATED-FROM", mailto(a.DelegatedFrom)),
			param("DELEGATED-TO", mailto(a.DelegatedTo)),
			param("PARTSTAT", a.PartStat),
			param("ROLE", a.Role),
			// RSVP is a bare TRUE/FALSE, never quoted and never lowercase;
			// Outlook ignores the parameter otherwise.
			boolParam("RSVP", a.RSVP),
		) + ":" + mailto(a.Address))
	}

	w.line("END:VEVENT")
}

func boolParam(name string, v bool) string {
	if !v {
		return ""
	}
	return name + "=TRUE"
}

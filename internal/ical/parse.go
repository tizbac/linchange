package ical

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"
)

// Document is a parsed VCALENDAR: the envelope properties plus its events.
type Document struct {
	Method Method
	ProdID string
	// Name is X-WR-CALNAME, the calendar's display name.
	Name string
	// Timezone is X-WR-TIMEZONE, the calendar's default zone.
	Timezone string
	Events   []Event
}

// Parse reads a VCALENDAR.
//
// An empty METHOD is not an error: a bare .ics export carries none, and it is
// still a calendar. Requiring one would mean refusing to import files that
// every other client accepts.
func Parse(data []byte) (*Document, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, ErrNoCalendar
	}
	cal, err := ics.ParseCalendar(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("ical: parsing: %w", err)
	}
	// A parser that tolerates arbitrary text may still hand back no
	// components at all; that is a text file, not a calendar.
	if len(cal.Components) == 0 {
		return nil, ErrNoCalendar
	}

	doc := &Document{}
	for _, prop := range cal.CalendarProperties {
		switch strings.ToUpper(prop.IANAToken) {
		case "METHOD":
			doc.Method = Method(strings.ToUpper(strings.TrimSpace(prop.Value)))
		case "PRODID":
			doc.ProdID = prop.Value
		case "X-WR-CALNAME":
			doc.Name = prop.Value
		case "X-WR-TIMEZONE":
			doc.Timezone = prop.Value
		}
	}

	for _, comp := range cal.Components {
		// VTODO, VJOURNAL and VFREEBUSY are parsed and dropped. They are not
		// this server's business yet, and dropping them here keeps them out
		// of the model without pretending they do not exist.
		event, ok := comp.(*ics.VEvent)
		if !ok {
			continue
		}
		ev, err := parseEvent(event)
		if err != nil {
			return nil, err
		}
		doc.Events = append(doc.Events, *ev)
	}
	return doc, nil
}

// parseEvent converts one parsed VEVENT into the model.
func parseEvent(event *ics.VEvent) (*Event, error) {
	ev := &Event{
		Summary:     propValue(event, ics.PropertySummary),
		Description: propValue(event, ics.PropertyDescription),
		Location:    propValue(event, ics.PropertyLocation),
		URL:         propValue(event, ics.PropertyUrl),
		UID:         propValue(event, ics.PropertyUid),
		Organizer:   parseOrganizer(event),
		Attendees:   parseAttendees(event),
		Categories:  parseCategories(event),
		Sequence:    propInt(event, ics.PropertySequence),
		Status:      strings.ToUpper(propValue(event, ics.PropertyStatus)),
	}

	// DTSTART carries the all-day marker as a VALUE=DATE parameter rather
	// than in its shape, so it has to be read before the value is interpreted.
	allDay := propIsDate(event, ics.PropertyDtstart)
	start, hasStart, err := eventTime(event, ics.PropertyDtstart)
	if err != nil {
		return nil, fmt.Errorf("ical: DTSTART: %w", err)
	}
	end, hasEnd, err := eventTime(event, ics.PropertyDtend)
	if err != nil {
		return nil, fmt.Errorf("ical: DTEND: %w", err)
	}
	if !hasStart && !hasEnd {
		// A VEVENT with neither is either a VTODO-shaped mistake or a
		// recurrence set member. Either way there is no time to store, and
		// the events table requires one.
		return nil, fmt.Errorf("%w: the event has no DTSTART or DTEND", ErrNoEvent)
	}
	if !hasStart {
		start = end
	}
	if !hasEnd {
		// RFC 5545 §3.6.1: a DATE-valued DTEND is exclusive, and a DTSTART
		// with no DTEND is zero-length. A timed event with no DTEND gets one
		// hour, which is what every client shows.
		if allDay {
			end = start.AddDate(0, 0, 1)
		} else {
			end = start.Add(time.Hour)
		}
	}

	ev.AllDay = allDay
	ev.Start = normaliseAllDay(start, allDay)
	ev.End = normaliseAllDay(end, allDay)
	if end.Before(start) {
		// The schema refuses dtend < dtstart, so a document claiming an
		// event that ends before it starts is corrected here rather than
		// becoming a rejected write at some unrelated layer.
		ev.End = ev.Start
	}
	ev.Timezone = propParam(event, ics.PropertyDtstart, "TZID")

	if rules, err := event.GetRRules(); err == nil && len(rules) > 0 {
		// RRULE is kept verbatim. See Event.RRULE for why.
		if p := event.GetProperty(ics.ComponentProperty(ics.PropertyRrule)); p != nil {
			ev.RRULE = strings.TrimSpace(p.Value)
		}
	}
	if dates, err := event.GetExDates(); err == nil {
		ev.ExDates = dates
	}
	if dates, err := event.GetRDates(); err == nil {
		ev.RDates = dates
	}
	if rid, err := event.GetRecurrenceID(); err == nil && !rid.IsZero() {
		r := rid
		ev.RecurrenceID = &r
	}

	// DTSTAMP, CREATED and LAST-MODIFIED are read through the same helper as
	// DTSTART so that an unknown TZID or a missing property is handled the
	// same way everywhere rather than only for the two that matter most.
	if ts, ok, err := eventTime(event, ics.PropertyDtstamp); err == nil && ok {
		ev.DTStamp = ts.UTC()
	}
	if c, ok, err := eventTime(event, ics.PropertyCreated); err == nil && ok {
		ev.Created = c.UTC()
	}
	if m, ok, err := eventTime(event, ics.PropertyLastModified); err == nil && ok {
		ev.LastModified = m.UTC()
	}

	// Transparency is how an event declares itself free. It is the property
	// a free/busy query reads, so it is promoted into the model.
	switch strings.ToUpper(propValue(event, ics.PropertyTransp)) {
	case "TRANSPARENT":
		ev.ShowAs = ShowAsFree
	default:
		ev.ShowAs = ShowAsBusy
		if ev.Status == StatusTentative {
			ev.ShowAs = ShowAsTentative
		}
	}
	// An explicitly absent STATUS is CONFIRMED, per RFC 5545 §3.8.8.3, and
	// leaving it empty would make every stored event look tentative.
	if ev.Status == "" {
		ev.Status = StatusConfirmed
	}
	return ev, nil
}

func parseOrganizer(event *ics.VEvent) Organizer {
	org := Organizer{
		Address: TrimMailto(propValue(event, ics.PropertyOrganizer)),
		Name:    propParam(event, ics.PropertyOrganizer, "CN"),
		SentBy:  TrimMailto(propParam(event, ics.PropertyOrganizer, "SENT-BY")),
	}
	return org
}

func parseAttendees(event *ics.VEvent) []Attendee {
	props := event.GetProperties(ics.ComponentProperty(ics.PropertyAttendee))
	out := make([]Attendee, 0, len(props))
	for _, p := range props {
		a := Attendee{
			Address:       TrimMailto(p.Value),
			Name:          paramValue(p.ICalParameters, "CN"),
			Role:          strings.ToUpper(paramValue(p.ICalParameters, "ROLE")),
			PartStat:      strings.ToUpper(paramValue(p.ICalParameters, "PARTSTAT")),
			RSVP:          strings.EqualFold(paramValue(p.ICalParameters, "RSVP"), "TRUE"),
			CUTYPE:        strings.ToUpper(paramValue(p.ICalParameters, "CUTYPE")),
			DelegatedFrom: TrimMailto(paramValue(p.ICalParameters, "DELEGATED-FROM")),
			DelegatedTo:   TrimMailto(paramValue(p.ICalParameters, "DELEGATED-TO")),
		}
		if a.Address == "" {
			// An ATTENDEE with no address cannot be replied to, so keeping it
			// would create an attendee row that no scheduler can ever answer.
			continue
		}
		if a.Role == "" {
			a.Role = RoleRequired
		}
		if a.PartStat == "" {
			a.PartStat = PartStatNeedsAction
		}
		out = append(out, a)
	}
	return out
}

func parseCategories(event *ics.VEvent) []string {
	raw := propValue(event, ics.PropertyCategories)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// normaliseAllDay pins an all-day date to midnight UTC.
//
// The parser resolves a DATE-valued DTSTART into whatever default zone the
// library has, so the same 10 February arrives as 10 February at +01:00 and 10
// February at +00:00 depending on the document. Storing the date rather than
// the instant is what makes an all-day event mean the same thing everywhere.
func normaliseAllDay(t time.Time, allDay bool) time.Time {
	if !allDay {
		return t
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// Nil-safe property access
//
// GetProperty returns nil for an absent property, and every field of VEVENT is
// optional except DTSTART in practice. A client that omits LOCATION must get an
// empty string, not a panic.
// ---------------------------------------------------------------------------

func propValue(event *ics.VEvent, prop ics.Property) string {
	p := event.GetProperty(ics.ComponentProperty(prop))
	if p == nil {
		return ""
	}
	return p.Value
}

func propParam(event *ics.VEvent, prop ics.Property, name string) string {
	p := event.GetProperty(ics.ComponentProperty(prop))
	if p == nil {
		return ""
	}
	return paramValue(p.ICalParameters, name)
}

func propInt(event *ics.VEvent, prop ics.Property) int {
	raw := strings.TrimSpace(propValue(event, prop))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

func propIsDate(event *ics.VEvent, prop ics.Property) bool {
	p := event.GetProperty(ics.ComponentProperty(prop))
	if p == nil {
		return false
	}
	return strings.EqualFold(paramValue(p.ICalParameters, "VALUE"), "DATE")
}

// paramValue reads the first value of a parameter. Parameter names are compared
// case-insensitively: RFC 5545 says they are case-insensitive, and Outlook
// writes CN where other clients write Cn.
func paramValue(params map[string][]string, name string) string {
	if len(params) == 0 {
		return ""
	}
	for k, vals := range params {
		if strings.EqualFold(k, name) && len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

// eventTime reads a time-valued property, reporting whether it was present.
func eventTime(event *ics.VEvent, prop ics.Property) (time.Time, bool, error) {
	p := event.GetProperty(ics.ComponentProperty(prop))
	if p == nil {
		return time.Time{}, false, nil
	}
	raw := strings.TrimSpace(p.Value)
	if raw == "" {
		return time.Time{}, false, nil
	}
	t, err := parseICalTime(raw, paramValue(p.ICalParameters, "TZID"))
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// The four time forms RFC 5545 §3.3.4 defines.
const (
	formatDate      = "20060102"
	formatDateUTC   = "20060102Z"
	formatDateTime  = "20060102T150405"
	formatDateTimeZ = "20060102T150405Z"
)

// parseICalTime interprets a DATE or DATE-TIME value.
//
// A value with no Z and no TZID is a floating time: a wall-clock reading with no
// zone attached, which means "nine in the morning wherever the reader is". It
// is interpreted as UTC here. That is a choice, and the alternative is to keep
// it zone-less and make every consumer guess; UTC at least round-trips.
func parseICalTime(raw, tzid string) (time.Time, error) {
	if len(raw) == len(formatDate) {
		return time.Parse(formatDate, raw)
	}
	loc := time.UTC
	if tzid != "" {
		l, err := time.LoadLocation(tzid)
		if err != nil {
			// An unknown zone must not lose the event. UTC keeps the instant
			// the sender meant, which is the part that cannot be recovered.
			loc = time.UTC
		} else {
			loc = l
		}
	}
	if strings.HasSuffix(raw, "Z") {
		return time.ParseInLocation(formatDateTimeZ, raw, time.UTC)
	}
	return time.ParseInLocation(formatDateTime, raw, loc)
}

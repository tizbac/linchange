package caldav

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/store"
)

// ---------------------------------------------------------------------------
// REPORT
// ---------------------------------------------------------------------------

// compFilter is a CalDAV component filter: which components, over what time
// range, matching which properties.
type compFilter struct {
	// name is VCALENDAR, VEVENT or VTODO.
	name string
	// timeRange restricts to events overlapping a window. A nil range means no
	// time restriction, which is a real and common request: "give me this
	// calendar" rather than "give me this month".
	timeRange *timeWindow
	// isNotDefinedTimeRange is a time-range with no start and no end, which RFC
	// 4791 §9.7.1 defines as "properties whose value is not a date". It is
	// distinct from no time-range at all and this server has no such property, so
	// it matches nothing rather than everything.
	isNotDefinedTimeRange bool
	// propFilters restrict on the value of a property.
	propFilters []propFilter
	// paramFilters restrict on a property's parameters, which is how a client
	// asks for the events it is an attendee at.
	paramFilters []paramFilter
	// children are the nested component filters.
	children []*compFilter
}

// timeWindow is a filter's start/end, in the form RFC 4791 uses.
type timeWindow struct {
	start time.Time
	end   time.Time
	// hasStart and hasEnd are separate because a window may be open at either
	// end, and collapsing that into a zero time would make "everything before
	// March" mean "everything between year 1 and March".
	hasStart bool
	hasEnd   bool
}

// propFilter is a filter on one property's text value.
type propFilter struct {
	// name is the property's local name, compared folded.
	name string
	// test is "anyof", "allof" or "present". RFC 4791 §9.7.5 defines these, and
	// an absent test is "anyof" for a prop-filter on a property with a value.
	test string
	// isNotDefined matches properties that are not present at all.
	isNotDefined bool
	// textMatch is the value to match, for a text-match filter.
	textMatch *textMatch
}

// paramFilter is a filter on a property's parameters.
type paramFilter struct {
	name string
	// isNotDefined matches a property that carries none of this parameter.
	isNotDefined bool
	textMatch    *textMatch
}

// textMatch is a substring, collation and negation condition.
type textMatch struct {
	value     string
	collation string
	negate    bool
	// isCaseSensitive defaults to false, because RFC 4791 §9.7.5 says the
	// default is case-insensitive and a client that wants exactness says so.
	isCaseSensitive bool
}

// parseCompFilter reads a C:comp-filter subtree.
func parseCompFilter(n *node) *compFilter {
	if n == nil {
		return nil
	}
	f := &compFilter{name: n.attrValue("name")}

	if tr := n.child(nsCalDAV, "time-range"); tr != nil {
		if !tr.hasAttr("start") && !tr.hasAttr("end") {
			// A time-range with neither bound is the "not a date" condition.
			f.isNotDefinedTimeRange = true
		} else {
			w := &timeWindow{}
			if v := tr.attrValue("start"); v != "" {
				if t, ok := parseICalTime(v); ok {
					w.start, w.hasStart = t, true
				}
			}
			if v := tr.attrValue("end"); v != "" {
				if t, ok := parseICalTime(v); ok {
					w.end, w.hasEnd = t, true
				}
			}
			// A bound that would not parse leaves the window open on that side
			// rather than failing the request. A client that sent a malformed
			// bound wants the events it can match, and refusing the whole REPORT
			// because one of four bounds was odd is the same failure for all
			// four.
			f.timeRange = w
		}
	}

	for _, pf := range n.childrenNamed(nsCalDAV, "prop-filter") {
		entry := propFilter{
			name:         pf.attrValue("name"),
			test:         pf.attrValue("test"),
			isNotDefined: strings.EqualFold(pf.attrValue("test"), "is-not-defined"),
		}
		entry.textMatch = parseTextMatch(pf.child(nsCalDAV, "text-match"))
		f.propFilters = append(f.propFilters, entry)
	}
	for _, paf := range n.childrenNamed(nsCalDAV, "param-filter") {
		entry := paramFilter{
			name:         paf.attrValue("name"),
			isNotDefined: strings.EqualFold(paf.attrValue("test"), "is-not-defined"),
		}
		entry.textMatch = parseTextMatch(paf.child(nsCalDAV, "text-match"))
		f.paramFilters = append(f.paramFilters, entry)
	}
	for _, kid := range n.childrenNamed(nsCalDAV, "comp-filter") {
		if child := parseCompFilter(kid); child != nil {
			f.children = append(f.children, child)
		}
	}
	return f
}

// parseTextMatch reads a text-match condition.
func parseTextMatch(n *node) *textMatch {
	if n == nil {
		return nil
	}
	tm := &textMatch{
		value:     n.text,
		collation: n.attrValue("collation"),
		negate:    strings.EqualFold(n.attrValue("negate-condition"), "yes"),
	}
	// RFC 4791 §9.7.5 makes the default collation case-insensitive, so an
	// absent collation means folded and nothing has to be said about it.
	switch strings.ToLower(tm.collation) {
	case "i;ascii-case-insensitive", "":
		tm.isCaseSensitive = false
	default:
		// i;octet and i;unicode-casemap are both case-sensitive. So is anything
		// else, which is the conservative reading: a collation this server does
		// not know, it treats as exact rather than guessing at a match that would
		// return the wrong events.
		tm.isCaseSensitive = true
	}
	return tm
}

// parseICalTime reads a time value from a filter attribute.
//
// CalDAV uses iCalendar formats here, not RFC 3339: a UTC time, a floating local
// time, or a date. The distinction between the last two matters -- a floating
// time is in the reader's own zone, and a date covers the whole day -- so a bare
// date is widened to the day's end rather than treated as midnight, which would
// make a whole-day event invisible to a query for the day it is on.
func parseICalTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	layouts := []string{
		"20060102T150405Z", // UTC
		"20060102T150405",  // floating local
		"20060102T1504Z",   // UTC, no seconds
		"20060102",         // date
		time.RFC3339,       // some clients send this anyway
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// matchWindow applies a filter's time range to a stored event.
//
// An event matches when it overlaps the window, not when it starts inside it. A
// meeting that began yesterday and ends tomorrow is in this week's view, and
// matching on DTSTART alone would drop every multi-day event from every view that
// does not start in it.
func (f *compFilter) matchWindow(ev store.Event) bool {
	if f.isNotDefinedTimeRange {
		return false
	}
	if f.timeRange == nil {
		return true
	}
	w := f.timeRange
	// The same overlap predicate the store uses, so a REPORT and a range query
	// cannot disagree about which events are in a window.
	if w.hasEnd && !ev.DTStart.Before(w.end) {
		return false
	}
	if w.hasStart {
		// Zero-length events sit inside the window or outside it, never across a
		// boundary, so they are not excluded by the end test.
		if ev.DTEnd.After(ev.DTStart) && !ev.DTEnd.After(w.start) {
			return false
		}
	}
	return true
}

// match applies a filter to one event, returning whether it is in the result set.
//
// The component-name check is the filter's own level: a VEVENT filter does not
// match a VTODO, and a VCALENDAR filter matches the container rather than the
// event inside it. Nested filters are evaluated against the same event because
// CalDAV filters do not cross component boundaries within a stored object -- this
// server stores one component per object resource.
func (f *compFilter) match(ev store.Event, doc ical.Event) bool {
	switch strings.ToUpper(f.name) {
	case "", "VCALENDAR":
		// The container matches everything; the interesting part is in the
		// children.
	case "VEVENT":
		// The only component this server stores.
	default:
		return false
	}

	if !f.matchWindow(ev) {
		return false
	}
	if !f.matchProps(doc) {
		return false
	}
	if !f.matchParams(doc) {
		return false
	}
	// A nested filter is an additional condition, not a disjunction. RFC 4791
	// §9.7.1 makes the child filters conjunctive.
	for _, child := range f.children {
		if !child.match(ev, doc) {
			return false
		}
	}
	return true
}

// matchProps applies the property filters to a parsed event.
//
// RFC 4791 §9.7.5 defines two tests, and the difference between them is only
// visible for a property that can hold several values. anyof -- the default --
// needs one value that matches; allof needs every one of them. CATEGORIES is the
// only property this server stores that has more than one, so for almost every
// filter the two tests agree, which is why this reads as more care than it is.
func (f *compFilter) matchProps(doc ical.Event) bool {
	for _, pf := range f.propFilters {
		values, present := propertyValues(doc, pf.name)
		if pf.isNotDefined {
			// is-not-defined matches a property that is not there. Note that a
			// property present but empty is not the same as absent: a client
			// filtering for "events with no location" gets the ones with no
			// LOCATION property, not the ones with LOCATION:
			if present {
				return false
			}
			continue
		}
		if !present {
			// A filter on a property the event does not have does not match it.
			// This is the case that matters in practice: a client filtering on
			// SUMMARY gets the events with a summary, and matching every untitled
			// event too would fill a filtered view with what the filter excluded.
			return false
		}
		if pf.textMatch == nil {
			// A prop-filter with a text-match missing and no is-not-defined is a
			// presence test, which the checks above have already answered.
			continue
		}
		matched := 0
		for _, v := range values {
			if pf.textMatch.matches(v) {
				matched++
			}
		}
		if strings.EqualFold(pf.test, "allof") {
			if matched != len(values) {
				return false
			}
			continue
		}
		if matched == 0 {
			return false
		}
	}
	return true
}

// propertyValues reads a named property out of a parsed event, as the list of
// values it holds.
//
// A slice rather than a single string because that is what RFC 4791's filters are
// written against, and returning one value with a made-up count would have every
// caller special-case the difference between CATEGORIES and everything else.
func propertyValues(doc ical.Event, name string) (values []string, present bool) {
	switch strings.ToUpper(name) {
	case "UID":
		return one(doc.UID), doc.UID != ""
	case "SUMMARY":
		// A SUMMARY is allowed to be absent and allowed to be empty, and a
		// client filtering on it wants whichever is here.
		return one(doc.Summary), true
	case "DESCRIPTION":
		return one(doc.Description), true
	case "LOCATION":
		return one(doc.Location), true
	case "STATUS":
		return one(doc.Status), doc.Status != ""
	case "CLASS":
		// Not modelled, so reported as absent rather than as an empty value. A
		// client filtering on CLASS gets nothing, which is honest: this server
		// does not know its value.
		return nil, false
	case "CATEGORIES":
		return doc.Categories, len(doc.Categories) > 0
	case "DTSTART":
		return one(doc.Start.UTC().Format("20060102T150405Z")), true
	case "DTEND":
		return one(doc.End.UTC().Format("20060102T150405Z")), true
	case "RRULE":
		return one(doc.RRULE), doc.RRULE != ""
	}
	return nil, false
}

// one wraps a value as a one-element slice, or an empty slice when it is empty.
//
// An empty property and an absent one differ for a filter, so this does not return
// a one-element slice holding "": the caller would then see a property with a
// value it does not have.
func one(v string) []string {
	if v == "" {
		return nil
	}
	return []string{v}
}

// matchParams applies the parameter filters.
func (f *compFilter) matchParams(doc ical.Event) bool {
	for _, paf := range f.paramFilters {
		values := parameterValues(doc, paf.name)
		if paf.isNotDefined {
			if len(values) > 0 {
				return false
			}
			continue
		}
		if paf.textMatch == nil {
			if len(values) == 0 {
				return false
			}
			continue
		}
		hit := false
		for _, v := range values {
			if paf.textMatch.matches(v) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// parameterValues reads the values of one parameter name across a property.
//
// The two parameters a CalDAV client filters on are ATTENDEE and ORGANIZER, and
// both are per-property values rather than properties in their own right, so
// "does this event have an ATTENDEE parameter whose value is me" is answered here
// rather than in propertyValue.
func parameterValues(doc ical.Event, name string) []string {
	switch strings.ToUpper(name) {
	case "ATTENDEE":
		out := make([]string, 0, len(doc.Attendees))
		for _, a := range doc.Attendees {
			out = append(out, a.Address)
		}
		return out
	case "ORGANIZER":
		if doc.Organizer.Address == "" {
			return nil
		}
		return []string{doc.Organizer.Address}
	}
	return nil
}

// matches applies a text-match condition to a value.
func (tm *textMatch) matches(value string) bool {
	var hit bool
	if tm.isCaseSensitive {
		hit = strings.Contains(value, tm.value)
	} else {
		hit = strings.Contains(strings.ToLower(value), strings.ToLower(tm.value))
	}
	if tm.negate {
		return !hit
	}
	return hit
}

// reportRequest is a parsed REPORT, whichever kind.
type reportRequest struct {
	// kind is the root element's local name.
	kind string
	// filter is set for calendar-query.
	filter *compFilter
	// hrefs is set for calendar-multiget, in the order the client asked.
	hrefs []string
	// syncToken and limit are set for sync-collection.
	syncToken string
	limit     int
	// props is the property list. An empty slice with allprop set means every
	// live property.
	props   []propName
	allprop bool
}

// parseReport reads a REPORT body.
func parseReport(body []byte) (reportRequest, *davError) {
	var out reportRequest
	if len(strings.TrimSpace(string(body))) == 0 {
		return out, &davError{
			Status: http.StatusBadRequest,
			Detail: "REPORT needs a body describing what to report on",
		}
	}
	root, err := parseXML(body)
	if err != nil {
		return out, &davError{Status: http.StatusBadRequest, Detail: "the REPORT body is not well-formed XML"}
	}
	out.kind = root.localOf()

	switch {
	case root.is(nsCalDAV, "calendar-query"):
		filterNode := root.child(nsCalDAV, "filter")
		if filterNode == nil {
			// RFC 4791 §7.8 makes the filter element required. Without one the
			// query means "every event ever", which a client would then have to
			// page through; accepting it would be a way for a malformed request
			// to ask for the whole collection.
			return out, &davError{
				Status: http.StatusBadRequest,
				Code:   "DAV:valid-filter",
				Detail: "a calendar-query needs a CalDAV:filter",
			}
		}
		// The filter's outermost element is a comp-filter for VCALENDAR.
		outer := parseCompFilter(filterNode.child(nsCalDAV, "comp-filter"))
		if outer == nil {
			// A filter with no comp-filter at all. RFC 4791 requires one, and
			// there is no way to guess what a filter with no component in mind
			// was meant to select.
			return out, &davError{
				Status: http.StatusBadRequest,
				Code:   "DAV:valid-filter",
				Detail: "a calendar-query filter needs a CalDAV:comp-filter",
			}
		}
		out.filter = outer

	case root.is(nsCalDAV, "calendar-multiget"):
		hrefs := root.childrenNamed(nsDAV, "href")
		if len(hrefs) == 0 {
			return out, &davError{
				Status: http.StatusBadRequest,
				Detail: "a calendar-multiget needs at least one DAV:href",
			}
		}
		for _, h := range hrefs {
			out.hrefs = append(out.hrefs, strings.TrimSpace(h.text))
		}

	case root.is(nsDAV, "sync-collection"):
		token := root.child(nsDAV, "sync-token")
		if token == nil {
			return out, &davError{
				Status: http.StatusBadRequest,
				Detail: "a sync-collection needs a DAV:sync-token, even an empty one for the first synchronisation",
			}
		}
		out.syncToken = strings.TrimSpace(token.text)
		if lim := root.child(nsDAV, "limit"); lim != nil {
			if n, err := strconv.Atoi(strings.TrimSpace(lim.child(nsDAV, "nresults").text)); err == nil && n > 0 {
				out.limit = n
			}
		}
		// RFC 6578 §3.2 makes sync-level mandatory, and allows "1" or "infinite".
		// Only "1" is answered as such here, because a calendar collection holds
		// calendar object resources and no sub-collections -- so the two spellings
		// describe the same set, and refusing "infinite" would be refusing a
		// request this server can in fact satisfy. A calendar collection is not a
		// place a client can put a sub-calendar, because the store has no such
		// idea.
		switch level := root.child(nsDAV, "sync-level"); {
		case level == nil:
			return out, &davError{
				Status: http.StatusBadRequest,
				Detail: "a sync-collection needs a DAV:sync-level",
			}
		case level.text == "1" || level.text == "infinite":
			// Accepted.
		default:
			return out, &davError{
				Status: http.StatusBadRequest,
				Detail: "a sync-level can only be 1 or infinite, not " + level.text,
			}
		}

	case root.is(nsDAV, "principal-property-search"):
		// Nothing to parse beyond the property name; the search is a lookup of
		// one principal.

	default:
		return out, &davError{
			Status: http.StatusBadRequest,
			Code:   "DAV:supported-report",
			Detail: "this server does not implement the " + root.localOf() + " report",
		}
	}

	// The property list, for every report kind that has one.
	if prop := root.child(nsDAV, "prop"); prop != nil {
		names := make([]propName, 0, len(prop.kids))
		for _, kid := range prop.kids {
			names = append(names, knownProp(kid.space, kid.local))
		}
		out.props = names
	} else {
		out.allprop = true
	}
	return out, nil
}

// handleCalendarReport answers a REPORT on a calendar collection.
//
// Three reports are implemented, and they are the three a client uses between
// them: calendar-query for a date range, calendar-multiget for a named set of
// resources, and sync-collection for an incremental update. Advertising a fourth
// in supported-report-set and then answering 400 for it would make a client pick
// it and fail, so the advertisement and the implementation are the same list.
func (s *Server) handleCalendarReport(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)

	body, derr := readBody(w, r, 4<<20)
	if derr != nil {
		fail(w, derr)
		return
	}
	req, derr := parseReport(body)
	if derr != nil {
		fail(w, derr)
		return
	}

	cal, derr := s.resolveCalendar(r, who)
	if derr != nil {
		fail(w, derr)
		return
	}
	if derr := s.requireRead(r, who, cal); derr != nil {
		fail(w, derr)
		return
	}

	propReq := propfindRequest{props: req.props, allprop: req.allprop}
	base := propResource{kind: kindCalendar, who: who, server: s, calendar: &cal, href: s.calendarURL(cal)}

	if req.kind == "sync-collection" {
		result, derr := s.runSync(r, cal, req)
		if derr != nil {
			fail(w, derr)
			return
		}
		// The DAV:sync-token property on the collection carries the position too,
		// so a client that PROPFINDs rather than REPORTs still learns where the
		// collection stands.
		base.syncToken = result.token

		ms := &multiStatus{}
		ms.syncToken = result.token
		if result.truncated {
			// RFC 6578 §6.5: a truncated answer says so on the collection itself,
			// with 507 and the condition element, alongside the members it did
			// return. The token still advances, so the next synchronisation
			// continues from here rather than starting over.
			coll := ms.add(s.calendarURL(cal))
			coll.status = &davError{
				Status: http.StatusInsufficientStorage,
				Code:   "DAV:number-of-matches-within-limits",
				Detail: "there are more changes than this synchronisation asked for",
			}
		}
		for _, item := range result.items {
			href := s.calendarURL(cal) + escapeSegment(item.name)
			resp := ms.add(href)
			if item.removed {
				// The href and a 404, and no propstat and no condition element:
				// RFC 6578 §3.6 says a removed member is reported as a status and
				// nothing else, and reporting properties for a resource the server
				// has just said it does not have is worse than useless.
				resp.status = &davError{Status: http.StatusNotFound}
				continue
			}
			objBase := base
			objBase.kind = kindObject
			objBase.event = item.ev
			objBase.href = href
			answer(resp, propReq, objBase)
		}
		ms.write(w)
		return
	}

	var events []store.Event
	switch req.kind {
	case "calendar-query":
		events = s.runQuery(req, cal)
	case "calendar-multiget":
		events = s.runMultiget(r, cal, req)
	}

	ms := &multiStatus{}
	// The collection itself is described only for a calendar-query, which is a
	// query about the collection as well as about its contents. A multiget is about
	// named resources.
	if req.kind == "calendar-query" {
		coll := ms.add(s.calendarURL(cal))
		answer(coll, propfindRequest{allprop: true}, base)
	}
	for i := range events {
		ev := events[i]
		objBase := base
		objBase.kind = kindObject
		objBase.event = &ev
		objBase.href = s.objectURL(cal, ev)
		resp := ms.add(objBase.href)
		answer(resp, propReq, objBase)
	}
	ms.write(w)
}

// handleCalendarHomeReport answers a REPORT on a principal's calendar home set.
//
// A client that wants "everything in my home" rather than one calendar sends
// calendar-query at the home set. It is answered by running the query across
// every calendar the principal may read, which is what the client would do
// itself if the server only spoke one collection.
func (s *Server) handleCalendarHomeReport(w http.ResponseWriter, r *http.Request) {
	// A PROPFIND on the home set is the common request and has its own handler,
	// so this one only sees REPORTs.
	who, derr := s.resolvePrincipal(r)
	if derr != nil {
		fail(w, derr)
		return
	}

	body, derr := readBody(w, r, 4<<20)
	if derr != nil {
		fail(w, derr)
		return
	}
	req, derr := parseReport(body)
	if derr != nil {
		fail(w, derr)
		return
	}
	calendars, derr := s.readableCalendars(r, who)
	if derr != nil {
		fail(w, derr)
		return
	}

	propReq := propfindRequest{props: req.props, allprop: req.allprop}
	ms := &multiStatus{}
	homeURL := s.calendarHomeURL(who.Address)
	homeResp := ms.add(homeURL)
	answer(homeResp, propReq, propResource{
		kind: kindCalendarHome, who: who, server: s,
		calendars: calendars, href: homeURL,
	})

	for _, cal := range calendars {
		base := propResource{
			kind: kindObject, who: who, server: s,
			calendar: &cal, href: s.calendarURL(cal),
		}
		var events []store.Event
		switch req.kind {
		case "calendar-query":
			events = s.runQuery(req, cal)
		case "calendar-multiget":
			events = s.runMultiget(r, cal, req)
		default:
			// A sync-collection across a whole home set has no defined token:
			// the token is per collection, and a client that wanted one has to
			// synchronise each collection. Refusing is better than inventing a
			// token that would not be accepted back.
			fail(w, &davError{
				Status: http.StatusBadRequest,
				Code:   "DAV:supported-report",
				Detail: "synchronise a calendar collection rather than a whole home set",
			})
			return
		}
		for i := range events {
			ev := events[i]
			base.event = &ev
			base.href = s.objectURL(cal, ev)
			resp := ms.add(base.href)
			answer(resp, propReq, base)
		}
	}
	ms.write(w)
}

// runQuery evaluates a calendar-query filter against a calendar's events.
func (s *Server) runQuery(req reportRequest, cal store.Calendar) []store.Event {
	// The window is taken from the outermost VEVENT filter, because that is where
	// clients put it and because the range query needs a window to bound the
	// rows it reads. A filter with no window anywhere reads the whole calendar,
	// which is a legitimate request and is answered by reading the whole calendar.
	from, to, hasWindow := s.queryWindow(req.filter)
	var candidates []store.Event
	if hasWindow {
		rows, err := s.events.ListInRange(context.Background(), s.db, []int64{cal.ID}, from, to)
		if err != nil {
			return nil
		}
		candidates = rows
	} else {
		rows, err := s.allEventsIn(cal)
		if err != nil {
			return nil
		}
		candidates = rows
	}
	return s.applyFilter(req.filter, candidates)
}

// queryWindow finds the time range in a filter.
//
// It descends to the deepest filter that has one, because the VEVENT filter is
// nested inside a VCALENDAR filter and the window belongs to the event. A filter
// with several different windows is one this server does not narrow to a single
// range query; it reads everything and evaluates the conditions, which is slower
// and correct.
func (s *Server) queryWindow(f *compFilter) (from, to time.Time, found bool) {
	var walk func(*compFilter)
	walk = func(f *compFilter) {
		if f == nil {
			return
		}
		if f.timeRange != nil && !f.isNotDefinedTimeRange && !found {
			from, to = rangeOf(f.timeRange)
			found = true
		}
		for _, kid := range f.children {
			walk(kid)
		}
	}
	walk(f)
	return from, to, found
}

// rangeOf widens a filter's window to a pair of instants for the range query.
//
// An open bound is widened to the extremes rather than dropped: a query for
// "everything from March onwards" has no end, and the range index needs one. The
// extremes are the values the schema already accepts, so this does not invent a
// limit the database could not store.
func rangeOf(w *timeWindow) (time.Time, time.Time) {
	min := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	max := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	from, to := min, max
	if w.hasStart && !w.start.Before(min) {
		from = w.start
	}
	if w.hasEnd && w.end.Before(max) {
		to = w.end
	}
	return from, to
}

// applyFilter keeps the candidates a filter accepts.
func (s *Server) applyFilter(f *compFilter, candidates []store.Event) []store.Event {
	if f == nil {
		return candidates
	}
	out := make([]store.Event, 0, len(candidates))
	for i := range candidates {
		ev := candidates[i]
		doc, err := ical.Parse(ev.ICalBlob)
		if err != nil {
			// A stored document this server cannot read is not something a
			// filter can be applied to. It is skipped rather than reported: a
			// client asking for events matching a filter does not want a
			// document it cannot match, and one unreadable row should not fail
			// the whole REPORT.
			continue
		}
		if len(doc.Events) == 0 {
			continue
		}
		if f.match(ev, doc.Events[0]) {
			out = append(out, ev)
		}
	}
	return out
}

// runMultiget resolves a list of hrefs to events in one calendar.
//
// An href naming a resource in a different collection is skipped rather than
// resolved. The client is a multiget against this collection, and following an
// absolute URL to another collection would turn a scoped request into a way to
// read whatever else the same principal can reach.
func (s *Server) runMultiget(r *http.Request, cal store.Calendar, req reportRequest) []store.Event {
	href := hrefOf(s.calendarURL(cal))
	// Both spellings of the collection's own path are accepted. An "@" is legal
	// unescaped in a URI path, so "/caldav/a@b.example.com/Work/one.ics" names the
	// same resource as the escaped form this server reports, and a client which
	// composes the URL itself rather than echoing back what it was given sends
	// the first. Comparing the two spellings and taking the name from whichever
	// matched keeps the name escaped exactly once, which matters for a resource
	// whose name contains a percent sign.
	spelling := []string{href, unescapePath(href)}

	var out []store.Event
	for _, raw := range req.hrefs {
		name, found := memberName(raw, spelling)
		if !found {
			continue
		}
		ev, err := s.eventByObjectName(r, cal, name)
		if err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// memberName reads the resource name out of one href of a multiget.
//
// spellings is the collection's path as this server writes it and as it reads it;
// the href has to start with one of them and have exactly one further segment, so
// an href naming the collection itself or a resource in some other collection is
// refused rather than resolved.
func memberName(href string, spellings []string) (string, bool) {
	for _, spelling := range spellings {
		if !strings.HasPrefix(href, spelling) {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(href, spelling), "/")
		if rest == "" || strings.Contains(rest, "/") {
			// Either the collection itself or something below it. Both are
			// outside what a multiget is for.
			continue
		}
		return rest, true
	}
	return "", false
}

// unescapePath decodes an escaped URL path, leaving it alone if it is not valid
// escaping -- in which case it was never escaped in the first place.
func unescapePath(p string) string {
	decoded, err := url.PathUnescape(p)
	if err != nil {
		return p
	}
	return decoded
}

// syncItem is one change a sync-collection reports.
//
// A removed item is one the client must forget, and it is reported as a 404 on the
// resource rather than as a property set: RFC 6578 §3.6 requires the href and a
// status, and a propstat would be claiming the server has properties for a
// resource it has just said it does not have.
type syncItem struct {
	// name is the resource's name within the collection, which is what a client
	// is told to forget when removed is set.
	name string
	// ev is the changed resource. Nil when removed is set, and also when the row a
	// change named has been deleted since: what the client needs to know is that
	// the resource is gone, whichever of the two happened last.
	ev      *store.Event
	removed bool
}

// syncResult is the answer to a sync-collection.
type syncResult struct {
	// items are the changes, in the order they happened.
	items []syncItem
	// token is the position the client has synchronised to.
	token string
	// truncated is whether more changes were waiting than the client allowed.
	truncated bool
}

// errInvalidSyncToken is the answer to a token this server will not honour.
//
// RFC 6578 §3.6 names the condition element for it, and the element is the whole
// point of the answer: it tells the client to discard what it holds and start a new
// synchronisation, which is the opposite of what a bare 403 leaves it guessing.
var errInvalidSyncToken = &davError{
	Status: http.StatusForbidden,
	Code:   "DAV:valid-sync-token",
	Detail: "this synchronisation token is no longer valid; start a new synchronisation",
}

// The synchronisation token.
//
// RFC 6578 §3.6 requires a token to be an opaque URI and requires one the server
// cannot honour to be refused rather than guessed at. Having the calendar's id and
// the change position in the path is what makes both checkable: a token from one
// collection offered to another is refused, rather than being read as a position in
// the wrong collection and answered with a plausible set of the wrong changes.
//
// It is a URI rather than a bare number so that a client which stores it as one --
// which they all do -- cannot confuse it with anything else, and so the value is
// recognisable in a log line as a token rather than as a row id.
const syncTokenPrefix = "http://linchange.example/ns/sync/"

func syncTokenAt(calendarID, position int64) string {
	return syncTokenPrefix + strconvItoa(calendarID) + "/" + strconvItoa(position)
}

// parseSyncToken reads back a token this server issued for this collection.
//
// ok is false for anything else, including a token from a different calendar: a
// token names the state of one collection, and applying one to another would
// report changes from the wrong place as if they were this one's.
func parseSyncToken(token string, calendarID int64) (int64, bool) {
	rest, found := strings.CutPrefix(token, syncTokenPrefix)
	if !found {
		return 0, false
	}
	calText, positionText, found := strings.Cut(rest, "/")
	if !found {
		return 0, false
	}
	cal, err := strconv.ParseInt(calText, 10, 64)
	if err != nil || cal != calendarID {
		return 0, false
	}
	position, err := strconv.ParseInt(positionText, 10, 64)
	if err != nil || position < 0 {
		return 0, false
	}
	return position, true
}

// runSync answers a sync-collection.
//
// The report is a delta, so the work is in knowing what changed. RFC 6578 §3.2
// defines it in terms of the token the client holds, and that means the server has
// to have kept a record of what it did: the events themselves cannot answer it,
// because a deleted event leaves nothing behind and because a sequence number is
// per row -- a row created after a token was issued would have a lower sequence
// than the token and never be reported at all. The change log is that record.
//
// The head is read *before* the changes it covers. Reading it afterwards would hand
// out a token naming changes the answer does not contain, and the client would
// apply them never: it would believe it had caught up, and the next
// synchronisation from that token would start after them.
func (s *Server) runSync(r *http.Request, cal store.Calendar, req reportRequest) (syncResult, *davError) {
	var out syncResult

	head, err := s.changes.Head(r.Context(), s.db, cal.ID)
	if err != nil {
		return out, s.databaseError(r.Context(), "reading the calendar's change position", err)
	}

	if req.syncToken == "" {
		// RFC 6578 §3.2: an empty token means "everything". The answer is the whole
		// collection, and the token is where the collection stands.
		events, err := s.allEventsIn(cal)
		if err != nil {
			return out, s.databaseError(r.Context(), "reading the calendar's events", err)
		}
		for i := range events {
			ev := events[i]
			out.items = append(out.items, syncItem{name: objectName(ev), ev: &ev})
		}
		out.token = syncTokenAt(cal.ID, head)
		return out, nil
	}

	position, ok := parseSyncToken(req.syncToken, cal.ID)
	if !ok {
		return out, errInvalidSyncToken
	}
	if stale, err := s.changesBelow(r.Context(), cal.ID, position); err != nil {
		return out, s.databaseError(r.Context(), "checking the synchronisation token", err)
	} else if stale {
		// A token from a client that has fallen so far behind that the changes it
		// has not seen have been pruned. The log is bounded, so this is a real case
		// rather than a theoretical one, and RFC 6578 §3.6's answer is the one that
		// makes the client start over instead of carrying on with a gap.
		return out, errInvalidSyncToken
	}

	// One change past the limit is enough to know the answer is truncated, and
	// reading one less than that would mean querying twice to find out.
	limit := 0
	if req.limit > 0 {
		limit = req.limit + 1
	}
	changes, err := s.changes.Since(r.Context(), s.db, cal.ID, position, limit)
	if err != nil {
		return out, s.databaseError(r.Context(), "reading the calendar's changes", err)
	}
	out.truncated = req.limit > 0 && len(changes) > req.limit
	if out.truncated {
		changes = changes[:req.limit]
	}

	// A member is reported once. RFC 6578 §3.6 says a URL must not appear twice in
	// one sync response, and a resource changed twice since the client's token is
	// reported as it now stands: the earlier change is in the same answer as its
	// replacement, so reporting it separately would say less about the resource and
	// say it twice.
	order := make([]string, 0, len(changes))
	latest := make(map[string]store.CalendarChange, len(changes))
	for _, change := range changes {
		name := changeName(change)
		if _, seen := latest[name]; !seen {
			order = append(order, name)
		}
		latest[name] = change
	}

	for _, name := range order {
		change := latest[name]
		item := syncItem{name: name, removed: change.Removed}
		if !change.Removed {
			ev, err := s.events.ByUID(r.Context(), s.db, cal.ID, change.UID, change.RecurrenceID)
			switch {
			case err == nil:
				item.ev = &ev
			case store.IsNotFound(err):
				// Changed and then deleted again since the client's token. What the
				// client needs is that the resource is gone, which is the state it
				// is actually in.
				item.removed = true
			default:
				return syncResult{}, s.databaseError(r.Context(), "reading the changed event", err)
			}
		}
		out.items = append(out.items, item)
	}

	out.token = syncTokenAt(cal.ID, head)
	return out, nil
}

// changesBelow reports whether a client's position has fallen behind the log.
//
// The check is against the oldest change still kept rather than against a date,
// because what matters is whether the changes between the client's position and
// now are still there to report. A calendar with no log at all has pruned
// everything, which for position zero is correct -- there is nothing between the
// beginning and the head to be missing.
func (s *Server) changesBelow(ctx context.Context, calendarID, position int64) (bool, error) {
	var oldest sql.NullInt64
	err := s.db.GetContext(ctx, &oldest,
		`SELECT MIN(id) FROM calendar_changes WHERE calendar_id = $1`, calendarID)
	if err != nil {
		return false, err
	}
	return oldest.Valid && position+1 < oldest.Int64, nil
}

// syncTokenFor is where a collection stands, as a PROPFIND reports it.
//
// RFC 6578 §3.1 makes DAV:sync-token a live property of a collection that
// supports synchronisation, so a client that asks gets the same position a sync
// report would have given it.
//
// A collection whose position cannot be read reports the beginning rather than
// the end: the answer is then "everything has changed", which costs the client a
// full listing, and the alternative is a token asserting that nothing ever
// changes in a collection this server cannot see.
func (s *Server) syncTokenFor(r *http.Request, cal store.Calendar) string {
	head, err := s.changes.Head(r.Context(), s.db, cal.ID)
	if err != nil {
		return syncTokenAt(cal.ID, 0)
	}
	return syncTokenAt(cal.ID, head)
}

// changeName is the resource name a change refers to, derived from the event's
// identity the same way a stored resource's name is.
func changeName(change store.CalendarChange) string {
	return objectName(store.Event{UID: change.UID, RecurrenceID: change.RecurrenceID})
}

// displayNameOf is the name a principal is shown by.
//
// The policy carries an address and what the account may do, not what it is
// called, so the display name is read from the user record. It falls back to the
// address because a principal whose display name cannot be read still has one
// this server can report, and a DAV:displayname of the address is better than a
// missing property that a client renders as a blank row.
func (s *Server) displayNameOf(ctx context.Context, who policy.Principal) string {
	user, err := store.NewUsers().Get(ctx, s.db, who.Address)
	if err != nil || strings.TrimSpace(user.DisplayName) == "" {
		return who.Address
	}
	return user.DisplayName
}

// allEvents reads every event in a calendar.
func (s *Server) allEvents(r *http.Request, cal store.Calendar) ([]store.Event, error) {
	return s.allEventsIn(cal)
}

// allEventsIn reads every event in a calendar.
//
// The range is the widest one the store accepts rather than an unbounded query,
// so the same index serves it and the predicate cannot be forgotten. A calendar
// with an event dated year 1 would fall outside it, which is a calendar whose
// contents are not what anyone means by a calendar.
func (s *Server) allEventsIn(cal store.Calendar) ([]store.Event, error) {
	return s.events.ListInRange(context.Background(), s.db, []int64{cal.ID},
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))
}

// eventByObjectName finds the event a resource name refers to.
//
// The name is the UID with a .ics suffix, or the UID, a marker and a recurrence
// stamp for an override. The UID is read back out of the name and the row looked
// up by it, rather than an id being encoded into the name, so a URL stays valid
// when the row it names is replaced -- which is what a client expects, since it
// never learns the id.
func (s *Server) eventByObjectName(r *http.Request, cal store.Calendar, name string) (store.Event, error) {
	decoded, err := url.PathUnescape(name)
	if err != nil {
		decoded = name
	}
	decoded = strings.TrimSuffix(decoded, ".ics")

	uid := decoded
	var recurrenceID *time.Time
	if at := strings.LastIndex(decoded, "_"); at > 0 {
		if t, ok := parseICalTime(decoded[at+1:]); ok {
			uid = decoded[:at]
			recurrenceID = &t
		}
	}
	return s.events.ByUID(r.Context(), s.db, cal.ID, uid, recurrenceID)
}

// ---------------------------------------------------------------------------
// GET, PUT, DELETE on an object resource
// ---------------------------------------------------------------------------

// handleGetObject serves a stored iCalendar document.
func (s *Server) handleGetObject(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	_, ev, derr := s.resolveObject(w, r, who)
	if derr != nil {
		fail(w, derr)
		return
	}

	w.Header().Set("Content-Type", "text/calendar; charset=utf-8; component=vevent")
	w.Header().Set("ETag", etagOf(ev))
	w.Header().Set("Last-Modified", httpTime(ev.UpdatedAt))
	w.Header().Set("Content-Length", strconvItoa(int64(len(ev.ICalBlob))))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(ev.ICalBlob)
}

// resolveObject resolves a path to a calendar and the event it names.
func (s *Server) resolveObject(w http.ResponseWriter, r *http.Request, who policy.Principal) (store.Calendar, store.Event, *davError) {
	cal, derr := s.resolveCalendar(r, who)
	if derr != nil {
		return store.Calendar{}, store.Event{}, derr
	}
	if derr := s.requireRead(r, who, cal); derr != nil {
		return store.Calendar{}, store.Event{}, derr
	}
	name := chi.URLParam(r, "object")
	if decoded, err := decodeSegment(name); err == nil {
		name = decoded
	}
	ev, err := s.eventByObjectName(r, cal, name)
	if err != nil {
		return store.Calendar{}, store.Event{}, &davError{
			Status: http.StatusNotFound,
			Code:   "DAV:not-found",
			Detail: "there is no event at that URL",
		}
	}
	return cal, ev, nil
}

// handlePutObject stores a new or revised iCalendar document.
//
// The preconditions are the substance of this handler. CalDAV's whole safety story
// is that a client which has been editing an event since it last synchronised
// says so, and the server refuses if somebody changed it in between. Skipping
// them produces silent data loss: two clients, two edits, one surviving, and
// neither of them told.
func (s *Server) handlePutObject(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	cal, derr := s.resolveCalendar(r, who)
	if derr != nil {
		fail(w, derr)
		return
	}
	if derr := s.requireWrite(r, who, cal); derr != nil {
		fail(w, derr)
		return
	}

	name := chi.URLParam(r, "object")
	if decoded, err := decodeSegment(name); err == nil {
		name = decoded
	}

	body, derr := readBody(w, r, s.maxObjectBytes)
	if derr != nil {
		fail(w, derr)
		return
	}

	// An empty body is a DELETE, per RFC 4918 §9.7.4. A client sending one means
	// "remove this", and parsing it as a document would fail and leave the
	// resource in place while the client believed it was gone.
	if len(strings.TrimSpace(string(body))) == 0 {
		s.deleteObject(w, r, who, cal, name)
		return
	}

	doc, err := ical.Parse(body)
	if err != nil {
		fail(w, &davError{
			Status: http.StatusBadRequest,
			Detail: "the request body is not a readable iCalendar document",
		})
		return
	}
	ev, err := icalFromDocument(doc)
	if err != nil {
		fail(w, &davError{Status: http.StatusBadRequest, Detail: err.Error()})
		return
	}

	// The name in the path and the UID in the body must agree. A client that PUT
	// to the wrong URL would otherwise leave a resource that the sync reports
	// under one name and that holds the document for another, and neither half of
	// that is recoverable by the client.
	if nameUID := uidOfObjectName(name); nameUID != "" && nameUID != ev.UID {
		fail(w, &davError{
			Status: http.StatusConflict,
			Code:   "DAV:no-uid-conflict",
			Detail: "the UID in the document is not the one this URL names",
		})
		return
	}

	existing, existingErr := s.eventByObjectName(r, cal, name)
	isNew := existingErr != nil
	if derr := s.checkWritePreconditions(w, r, existing, isNew); derr != nil {
		fail(w, derr)
		return
	}

	// The SEQUENCE in the body is the client's claim about which revision it is
	// writing. One behind the stored document's is a client writing from a copy it
	// made before somebody else's change, and storing it would undo that change
	// without either of them finding out.
	//
	// It is compared with the SEQUENCE of the stored *document*, not with the
	// revision counter in the database. The counter is this server's own: it
	// advances on every write whatever the client sent, so a client whose events
	// carry SEQUENCE:2 from another server would be permanently "behind" a counter
	// that is already at 90, and would be refused for ever. The document's own
	// SEQUENCE is what the client was last given and what it is answering.
	//
	// A document with no SEQUENCE expresses no opinion at all, so nothing is
	// compared: many clients omit it, and treating its absence as SEQUENCE:0 would
	// refuse them every write to an event they had already changed once.
	if !isNew {
		if written, stated := sequenceIn(body); stated {
			if stored, had := sequenceIn(existing.ICalBlob); had && written < stored {
				fail(w, &davError{
					Status: http.StatusConflict,
					Code:   "DAV:no-uid-conflict",
					Detail: "this document is older than the one stored; read the event again and try once more",
				})
				return
			}
		}
	}

	// A STATUS:CANCELLED in a PUT is a cancellation rather than a deletion, and
	// RFC 5546 §3.2.11 sends the CANCEL to the attendees. It goes out through the
	// calendar service, which is what knows how to address them.
	//
	// If the cancellation cannot be sent it is stored anyway. The two failures are
	// not equal: the attendees not being told is bad, and the event still looking
	// live to the person who cancelled it is worse, because that person has acted
	// on a belief the system contradicts.
	if ev.IsCancelled() && !isNew {
		if cancelled, err := s.calSvc.Cancel(r.Context(), s.db, who, existing.ID); err == nil {
			w.Header().Set("ETag", etagOf(cancelled))
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	stored, derr := s.storeEvent(r, who, cal, ev, body, existing, isNew)
	if derr != nil {
		fail(w, derr)
		return
	}

	w.Header().Set("ETag", etagOf(stored))
	w.Header().Set("Location", hrefOf(s.objectURL(cal, stored)))
	if isNew {
		w.WriteHeader(http.StatusCreated)
		return
	}
	// 204, not 200. RFC 4918 §9.7.2: the resource already existed and was
	// replaced, so there is no representation to send. A client given 200 with an
	// empty body has been told to expect one.
	w.WriteHeader(http.StatusNoContent)
}

// checkWritePreconditions applies If-None-Match and If-Match.
//
// They are two halves of one question -- is the resource in the state the client
// thinks it is -- and a client that sends only one of them has asked half of it.
// Treated separately they would have to be reasoned about separately, and the
// combination of a create-if-absent with a conditional update is where the races
// live.
//
// A header that is absent asks nothing, so the write proceeds: RFC 4918 §10.4
// makes an absent If-Match no precondition at all, and a client that never sends
// one has asked for last-writer-wins.
func (s *Server) checkWritePreconditions(w http.ResponseWriter, r *http.Request, existing store.Event, isNew bool) *davError {
	ifNone := r.Header.Get("If-None-Match")
	ifMatch := r.Header.Get("If-Match")

	// If-None-Match: * is "create this, and refuse if it is already there". It is
	// how two clients can each create the same event without one of them
	// overwriting the other's.
	if ifNone != "" {
		if matchesAnyETag(ifNone, "*") {
			if !isNew {
				w.Header().Set("ETag", etagOf(existing))
				return &davError{
					Status: http.StatusPreconditionFailed,
					Code:   "DAV:resource-must-be-null",
					Detail: "there is already an event at that URL",
				}
			}
			return nil
		}
		// A specific tag in If-None-Match is the other way round: proceed only if
		// the resource has *that* tag, so it is an If-Match in disguise. Both are
		// honoured here rather than the second being ignored, because a client that
		// sent it has said what it wants and is entitled to an answer.
		if !isNew && matchesAnyETag(ifNone, etagOf(existing)) {
			return &davError{
				Status: http.StatusPreconditionFailed,
				Code:   "DAV:resource-must-be-null",
				Detail: "the event already has the revision this request names",
			}
		}
	}

	if ifMatch != "" {
		if isNew {
			return &davError{
				Status: http.StatusPreconditionFailed,
				Code:   "DAV:preconditions-failed",
				Detail: "If-Match names a revision, and there is no event here to have one",
			}
		}
		if !matchesAnyETag(ifMatch, etagOf(existing)) {
			w.Header().Set("ETag", etagOf(existing))
			return &davError{
				Status: http.StatusPreconditionFailed,
				Code:   "DAV:no-uid-conflict",
				Detail: "the event has changed since it was read; the stored copy is at " + etagOf(existing),
			}
		}
	}
	return nil
}

// uidOfObjectName reads the UID back out of a resource name.
func uidOfObjectName(name string) string {
	decoded, err := url.PathUnescape(name)
	if err != nil {
		decoded = name
	}
	decoded = strings.TrimSuffix(decoded, ".ics")
	if at := strings.LastIndex(decoded, "_"); at > 0 {
		if _, ok := parseICalTime(decoded[at+1:]); ok {
			return decoded[:at]
		}
	}
	return decoded
}

// sequenceIn reads a document's SEQUENCE, and whether it had one at all.
//
// The parsed event carries the value but not the fact of its presence, and the
// difference is the whole point: a document with no SEQUENCE is a client saying
// nothing about which revision it is writing, while one with SEQUENCE:0 is a
// client saying it is writing the first revision. Confusing the two refuses the
// write of every client that omits the property, which is most of them.
//
// The document is scanned rather than the parse because the scan is also the only
// way to see a property the parser does not model. Lines are unfolded as they are
// read: a folded line continues the previous one, so a name split across a fold is
// not a property name, and reading one as a property would attribute a value to a
// property the document does not have.
func sequenceIn(body []byte) (int, bool) {
	// Component nesting, so that a SEQUENCE belonging to some other component
	// -- a vendor property on a VTIMEZONE, say -- is not read as the event's.
	var inEvent bool
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			// A continuation of the line above, so it cannot start one.
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		// Parameters follow the name, so the part before them is the name.
		property, _, _ := strings.Cut(name, ";")
		switch strings.ToUpper(strings.TrimSpace(property)) {
		case "BEGIN":
			inEvent = strings.EqualFold(strings.TrimSpace(value), "VEVENT")
		case "END":
			inEvent = false
		case "SEQUENCE":
			if !inEvent {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				// An unparseable SEQUENCE is the client's mistake, and it is not
				// read as "no opinion" either: a document this server cannot read
				// a sequence out of is one the iCalendar parser will have already
				// complained about.
				return 0, false
			}
			return n, true
		}
	}
	return 0, false
}

// storeEvent writes a document through the calendar service.
//
// The service is used rather than the store directly so that the write is
// authorised, given a UID when one is missing, and stamped with an organiser in
// the same way a write from the web interface is. A CalDAV client and the web
// interface creating the same event must produce the same row, or the two views
// of it disagree.
func (s *Server) storeEvent(
	r *http.Request, who policy.Principal,
	cal store.Calendar, ev ical.Event, body []byte,
	existing store.Event, isNew bool,
) (store.Event, *davError) {
	draft := calendar.Draft{CalendarID: cal.ID, Event: ev}
	// The document the client sent is what gets stored, not a re-serialisation of
	// the parsed event, so a property this server does not model survives the
	// round trip. The parsed form is what the index columns are derived from.
	draft.Event.UID = ev.UID
	if draft.Event.Organizer.Address == "" {
		draft.Event.Organizer.Address = who.Address
	}

	var (
		stored store.Event
		err    error
	)
	if isNew {
		stored, err = s.calSvc.Create(r.Context(), s.db, who, draft)
	} else {
		// The sequence the client was working from. A mismatch has already been
		// caught by the If-Match and SEQUENCE checks above, so this is a
		// last-line check inside the write rather than a gate.
		stored, err = s.calSvc.Update(r.Context(), s.db, who, existing.ID, draft, existing.Sequence)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Event{}, &davError{
				Status: http.StatusNotFound,
				Code:   "DAV:not-found",
				Detail: "the event to replace is no longer there",
			}
		}
		return store.Event{}, s.databaseError(r.Context(), "storing the event", err)
	}

	// The client wrote a document, and the document is what it will read back.
	// The service wrote the parsed form, so where the two differ -- a property
	// the parser drops, a parameter it normalises -- the client's own bytes are
	// what get stored, keeping the round trip lossless.
	if err := s.replaceBlob(r, stored.ID, body); err != nil {
		return store.Event{}, s.databaseError(r.Context(), "storing the event document", err)
	}
	return stored, nil
}

// replaceBlob rewrites an event's stored document.
//
// The columns derived from it are left alone. The parsed event and the raw
// document describe the same event, and the columns are an index over the parsed
// form; rewriting them from a parse of the client's bytes would be the same work
// done twice with a chance of the two disagreeing.
func (s *Server) replaceBlob(r *http.Request, eventID int64, body []byte) error {
	_, err := s.db.ExecContext(r.Context(),
		`UPDATE events SET ical_blob = $2 WHERE id = $1`, eventID, body)
	return err
}

// checkIfMatch applies the If-Match header to a DELETE.
//
// It is the same rule as on a PUT -- an absent header is no precondition -- split
// out only because a delete has nothing to store afterwards, so the whole of its
// conditional handling is the check. The refusal names the stored tag, so a client
// can re-read and retry without having to guess what it should have sent.
func (s *Server) checkIfMatch(w http.ResponseWriter, r *http.Request, existing store.Event) *davError {
	header := r.Header.Get("If-Match")
	if header == "" {
		return nil
	}
	if matchesAnyETag(header, etagOf(existing)) {
		return nil
	}
	w.Header().Set("ETag", etagOf(existing))
	return &davError{
		Status: http.StatusPreconditionFailed,
		Code:   "DAV:no-uid-conflict",
		Detail: "the event has changed since it was read; the stored copy is at " + etagOf(existing),
	}
}

// matchesAnyETag reports whether an If-Match or If-None-Match header matches.
//
// A header is a comma-separated list, and the list may hold "*" meaning "any
// resource exists". Weak comparison is used, as RFC 7232 §3.2 requires for
// If-Match: two tags that differ only in strength are the same entity.
func matchesAnyETag(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if candidate == "*" {
			return true
		}
		if weakETag(candidate) == weakETag(etag) {
			return true
		}
	}
	return false
}

// weakETag drops the weakness marker, so a weak and a strong form of one tag
// compare equal.
func weakETag(tag string) string {
	return strings.TrimPrefix(strings.TrimSpace(tag), "W/")
}

// handleDeleteObject removes a calendar object resource.
func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	cal, derr := s.resolveCalendar(r, who)
	if derr != nil {
		fail(w, derr)
		return
	}
	if derr := s.requireWrite(r, who, cal); derr != nil {
		fail(w, derr)
		return
	}
	s.deleteObject(w, r, who, cal, chi.URLParam(r, "object"))
}

// deleteObject removes one event.
//
// A DELETE on a recurring event's master removes the whole series, which is what
// RFC 4791 §4.2 says and what a client means by deleting a repeating meeting. A
// DELETE on an override removes that occurrence and leaves the rest, and a DELETE
// naming a recurrence id does the same without the client having to know which
// occurrence it is looking at.
func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, who policy.Principal, cal store.Calendar, name string) {
	if decoded, err := decodeSegment(name); err == nil {
		name = decoded
	}
	ev, err := s.eventByObjectName(r, cal, name)
	if err != nil {
		// RFC 4918 §9.6.1: a DELETE of something that is not there succeeds.
		// A client retrying a delete after a dropped connection must not be told
		// it failed, or it will keep retrying forever.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if header := r.Header.Get("If-Match"); header != "" {
		if derr := s.checkIfMatch(w, r, ev); derr != nil {
			fail(w, derr)
			return
		}
	}

	if ev.RecurrenceID != nil {
		// An override: this row is a replacement for one occurrence, so the
		// series needs an EXDATE for that slot. The master is rewritten rather
		// than deleted, and the occurrence goes from it.
		if err := s.excludeOccurrence(r, ev); err != nil {
			fail(w, s.databaseError(r.Context(), "removing the occurrence", err))
			return
		}
		if err := s.events.Delete(r.Context(), s.db, ev.ID); err != nil {
			fail(w, s.databaseError(r.Context(), "removing the occurrence", err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// A master. If it is not recurring there is nothing else to do; if it is, the
	// whole series and every override of it go.
	series, err := s.events.SeriesByUID(r.Context(), s.db, cal.ID, ev.UID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(w, s.databaseError(r.Context(), "reading the series", err))
		return
	}
	for _, row := range series {
		if err := s.events.Delete(r.Context(), s.db, row.ID); err != nil {
			fail(w, s.databaseError(r.Context(), "removing the series", err))
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// excludeOccurrence adds an EXDATE to a series for one occurrence.
//
// The master is rewritten with the exclusion rather than being deleted, because
// deleting the master would take the rest of the series with it, and the client
// asked to remove one occurrence.
func (s *Server) excludeOccurrence(r *http.Request, override store.Event) error {
	master, err := s.events.ByUID(r.Context(), s.db, override.CalendarID, override.UID, nil)
	if err != nil {
		// An override with no master. There is no series to exclude from, so
		// removing the override is the whole of the deletion.
		return nil
	}
	doc, err := ical.Parse(master.ICalBlob)
	if err != nil || len(doc.Events) == 0 {
		return nil
	}
	ev := doc.Events[0]
	ev.ExDates = append(ev.ExDates, *override.RecurrenceID)
	return s.rewriteMaster(r, master, ev)
}

// rewriteMaster stores a modified master document.
func (s *Server) rewriteMaster(r *http.Request, master store.Event, ev ical.Event) error {
	blob := ev.Marshal()
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE events
		SET ical_blob = $2, sequence = sequence + 1, last_modified = now()
		WHERE id = $1`, master.ID, blob); err != nil {
		return err
	}
	return nil
}

// handlePrincipalSearch answers a principal-property-search.
//
// It is here because a client that has been told the principal collection exists
// will ask it for the principal matching an address, and a server that answers 400
// leaves the client with a principal URL it cannot fill in. The search matches
// only the caller's own address: this service has no address book to search, and
// answering 403 for anyone else would at least be honest about it.
func (s *Server) handlePrincipalSearch(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	body, derr := readBody(w, r, 1<<20)
	if derr != nil {
		fail(w, derr)
		return
	}
	root, err := parseXML(body)
	if err != nil || !root.is(nsDAV, "principal-property-search") {
		fail(w, &davError{
			Status: http.StatusBadRequest,
			Detail: "a principal search needs a DAV:principal-property-search body",
		})
		return
	}
	search := root.child(nsDAV, "property-search")
	needle := ""
	if search != nil {
		needle = strings.TrimSpace(search.child(nsDAV, "match").text)
	}
	name := s.displayNameOf(r.Context(), who)
	if needle != "" && !strings.EqualFold(needle, who.Address) &&
		!strings.EqualFold(needle, name) {
		// An empty multistatus is the right answer for "no match", and it is not
		// a 403: a search that finds nothing and a search that is not allowed are
		// different, and a client that cannot tell them apart concludes the
		// server is refusing rather than that the address is not here.
		(&multiStatus{}).write(w)
		return
	}

	ms := &multiStatus{}
	resp := ms.add(s.principalURL(who.Address))
	answer(resp, propfindRequest{allprop: true}, propResource{
		kind: kindPrincipal, who: who, server: s,
		displayName: name,
		href:        s.principalURL(who.Address),
	})
	ms.write(w)
}

package caldav

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// propfindRequest is what a client asked for.
type propfindRequest struct {
	// allprop is <allprop/>: every live property. It is also what an empty body
	// means, because RFC 4918 §9.1 says a PROPFIND with no body is a request
	// for all properties.
	allprop bool
	// propname is <propname/>: the names of the properties, with no values. A
	// client uses it to discover what a resource has before asking for values,
	// and answering it with values wastes a full property set on every resource.
	propname bool
	// props are the qualified names asked for by name.
	props []propName
}

// parsePropfind reads a PROPFIND body.
//
// A body that is not a propfind document is refused rather than treated as
// allprop. The distinction matters: allprop is a request to return everything,
// and a request this server could not parse is not a request for everything. A
// client that sent a propfind with a misspelled element would be told the
// request was bad, which is true, rather than handed a full property set and no
// indication that it had asked the wrong question.
func parsePropfind(body []byte) (propfindRequest, *davError) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return propfindRequest{allprop: true}, nil
	}
	root, err := parseXML(body)
	if err != nil {
		return propfindRequest{}, &davError{
			Status: http.StatusBadRequest,
			Detail: "the PROPFIND body is not well-formed XML",
		}
	}
	if !root.is(nsDAV, "propfind") {
		return propfindRequest{}, &davError{
			Status: http.StatusBadRequest,
			Detail: "a PROPFIND body must be a DAV:propfind element, not " + root.localOf(),
		}
	}
	switch {
	case root.child(nsDAV, "allprop") != nil:
		return propfindRequest{allprop: true}, nil
	case root.child(nsDAV, "propname") != nil:
		return propfindRequest{propname: true}, nil
	}
	names := root.propElements()
	if names == nil {
		// A propfind with a prop element that is empty is a request for no
		// properties. RFC 4918 §9.1 does not define it, and answering allprop
		// would be the more expensive of the two plausible readings.
		return propfindRequest{}, &davError{
			Status: http.StatusBadRequest,
			Detail: "the PROPFIND asks for no properties and does not say allprop",
		}
	}
	return propfindRequest{props: names}, nil
}

// depth reads the Depth header, and refuses the value RFC 4918 §9.1 forbids.
//
// "infinity" is not merely expensive: a server that honours it walks the whole
// collection, and a calendar collection holds every event a user has ever kept.
// The refusal is a 403 with DAV:propfind-finite-depth rather than a 400, because
// the request is well formed and the problem is what it asks for.
func depth(r *http.Request) (int, *davError) {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Depth"))) {
	case "", "0":
		return 0, nil
	case "1":
		return 1, nil
	case "infinity":
		return 0, &davError{
			Status: http.StatusForbidden,
			Code:   "DAV:propfind-finite-depth",
			Detail: "Depth: infinity is not supported; ask for Depth: 1",
		}
	default:
		return 0, &davError{
			Status: http.StatusBadRequest,
			Detail: "the Depth header must be 0, 1 or infinity",
		}
	}
}

// answer fills a response with the properties a request asked for.
func answer(resp *response, req propfindRequest, r propResource) {
	switch {
	case req.propname:
		// Names only. Every property that applies is listed, with no value, so a
		// client can see what exists. A property that does not apply is not
		// listed at all rather than listed as missing, because the client asked
		// what is here and it is not here.
		for _, name := range propNamesInOrder() {
			if !propTable[name].applies(r) {
				continue
			}
			space, local := splitProp(name)
			resp.found = append(resp.found, el(space, local))
		}
	case req.allprop:
		for _, name := range propNamesInOrder() {
			def := propTable[name]
			if !def.applies(r) {
				continue
			}
			if !def.live {
				// RFC 4918 §14.2: allprop returns dead properties as empty, since
				// the server has no stored value for them. This server keeps
				// nothing outside the properties below, so every property is
				// live and this branch is unreachable in practice; it is here
				// because dropping the distinction would be a bug the day a dead
				// property is added.
				space, local := splitProp(name)
				resp.found = append(resp.found, el(space, local))
				continue
			}
			if v := def.value(r); v != nil {
				resp.found = append(resp.found, v)
			}
		}
	default:
		for _, name := range req.props {
			def, known := propTable[name]
			if !known || !def.applies(r) {
				space, local := splitProp(name)
				resp.missing = append(resp.missing, el(space, local))
				continue
			}
			if v := def.value(r); v != nil {
				resp.found = append(resp.found, v)
			} else {
				space, local := splitProp(name)
				resp.missing = append(resp.missing, el(space, local))
			}
		}
	}
}

// readBody reads a request body under a limit.
//
// A DAV body is a document a client chose to send, and a client can always send
// more than it meant to. The limit is enforced here so an oversized request is a
// 413 with an explanation rather than a connection that closes while the server
// is still reading.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, *davError) {
	if r.ContentLength > limit {
		return nil, &davError{
			Status: http.StatusRequestEntityTooLarge,
			Detail: "the request body is larger than this server accepts",
		}
	}
	// The reader is capped as well as the declared length, because a chunked
	// request declares no length at all and ContentLength is -1.
	body := http.MaxBytesReader(w, r.Body, limit)
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 32*1024)
	for {
		n, err := body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if strings.Contains(err.Error(), "too large") {
				return nil, &davError{
					Status: http.StatusRequestEntityTooLarge,
					Detail: "the request body is larger than this server accepts",
				}
			}
			// io.EOF and anything else at the end of a body is normal here: the
			// loop is how a limit is enforced on a stream whose length was not
			// declared. A read that failed part way through is indistinguishable
			// from one that finished, and a truncated PROPFIND is reported by the
			// XML parser with a better message than this could invent.
			break
		}
	}
	return buf, nil
}

// ---------------------------------------------------------------------------
// PROPFIND
// ---------------------------------------------------------------------------

// handleRootPropfind answers a PROPFIND on the service root.
//
// The root is a real resource and clients read properties from it, so it is
// answered rather than 404'd: a client that cannot read the root assumes the URL
// it was configured with is wrong and moves on to the next candidate.
func (s *Server) handleRootPropfind(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	req, derr := s.propfindFor(w, r)
	if derr != nil {
		fail(w, derr)
		return
	}

	ms := &multiStatus{}
	resp := ms.add(s.prefix + "/")
	answer(resp, req, propResource{kind: kindRoot, who: who, server: s, href: s.prefix + "/"})
	ms.write(w)
}

// handlePrincipalCollection answers a PROPFIND on /principals/.
//
// The collection holds exactly one principal: the caller's. A DAV server that
// listed every account in the domain here would be publishing the address list to
// anyone who authenticated, and the whole point of the collection is that a
// client can find its own principal without an address book.
// handlePrincipalCollection answers a PROPFIND on the principal collection.
//
// At depth 1 it holds the caller's own principal and nobody else's. The collection
// is a collection, so RFC 3744 §3 expects its members to be listed, and clients do
// rely on it: a mail client with two accounts for the same domain asks the
// directory what principals exist here to decide whether to offer the second
// account, and a server that answers with the collection alone reads as a
// directory with nobody in it.
//
// Listing only the caller is what makes that answer safe. This server hosts one
// person's calendars and does not know the organisation's directory, so the only
// principal it can vouch for is the one asking.
func (s *Server) handlePrincipalCollection(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	req, derr := s.propfindFor(w, r)
	if derr != nil {
		fail(w, derr)
		return
	}
	d, derr := depth(r)
	if derr != nil {
		fail(w, derr)
		return
	}

	ms := &multiStatus{}
	resp := ms.add(s.prefix + "/principals/")
	answer(resp, req, propResource{kind: kindPrincipalCollection, who: who, server: s})

	if d >= 1 {
		mine := ms.add(s.principalURL(who.Address))
		answer(mine, req, propResource{
			kind: kindPrincipal, who: who, server: s,
			displayName: s.displayNameOf(r.Context(), who),
			href:        s.principalURL(who.Address),
		})
	}
	ms.write(w)
}

// handlePrincipal answers a PROPFIND on one principal's resource.
func (s *Server) handlePrincipal(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	req, derr := s.propfindFor(w, r)
	if derr != nil {
		fail(w, derr)
		return
	}
	if _, derr := depth(r); derr != nil {
		fail(w, derr)
		return
	}

	// A principal may only read its own resource. There is no proxy or
	// delegation model in this service, so reading another's would be reading
	// another user's account details with no authorisation behind it.
	raw := chi.URLParam(r, "principal")
	if decoded, err := decodeSegment(raw); err == nil {
		raw = decoded
	}
	if !strings.EqualFold(strings.TrimSpace(raw), who.Address) {
		fail(w, &davError{
			Status: http.StatusForbidden,
			Code:   "DAV:need-privileges",
			Detail: "a principal may only read its own properties",
		})
		return
	}

	ms := &multiStatus{}
	resp := ms.add(s.principalURL(who.Address))
	answer(resp, req, propResource{
		kind: kindPrincipal, who: who, server: s,
		displayName: s.displayNameOf(r.Context(), who),
		href:        s.principalURL(who.Address),
	})
	ms.write(w)
}

// handleCalendarHome answers a PROPFIND on a principal's calendar home set.
//
// At depth 0 it describes the collection. At depth 1, which is what every client
// actually sends to discover a calendar list, it also describes every calendar in
// it -- which is the entire point of the request, and the reason a client that
// receives only the home set has nothing to show the user.
func (s *Server) handleCalendarHome(w http.ResponseWriter, r *http.Request) {
	who, derr := s.resolvePrincipal(r)
	if derr != nil {
		fail(w, derr)
		return
	}
	req, derr := s.propfindFor(w, r)
	if derr != nil {
		fail(w, derr)
		return
	}
	d, derr := depth(r)
	if derr != nil {
		fail(w, derr)
		return
	}

	calendars, derr := s.readableCalendars(r, who)
	if derr != nil {
		fail(w, derr)
		return
	}

	homeURL := s.calendarHomeURL(who.Address)
	ms := &multiStatus{}
	resp := ms.add(homeURL)
	answer(resp, req, propResource{
		kind: kindCalendarHome, who: who, server: s,
		calendars: calendars, href: homeURL,
	})

	if d >= 1 {
		for i := range calendars {
			cal := calendars[i]
			calResp := ms.add(s.calendarURL(cal))
			answer(calResp, req, propResource{
				kind: kindCalendar, who: who, server: s,
				calendars: calendars, calendar: &cal, href: s.calendarURL(cal),
			})
		}
	}
	ms.write(w)
}

// handleWorkgroupHome answers a PROPFIND on a workgroup's calendar collection.
func (s *Server) handleWorkgroupHome(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	req, derr := s.propfindFor(w, r)
	if derr != nil {
		fail(w, derr)
		return
	}
	d, derr := depth(r)
	if derr != nil {
		fail(w, derr)
		return
	}
	id, ok := s.pathID(r, "group")
	if !ok {
		fail(w, &davError{Status: http.StatusBadRequest, Detail: "the workgroup in the path is not a number"})
		return
	}
	calendars, err := s.calendars.ByWorkgroup(r.Context(), s.db, id)
	if err != nil {
		fail(w, s.databaseError(r.Context(), "looking up the workgroup calendars", err))
		return
	}

	homeURL := s.prefix + "/workgroup/" + strconvItoa(id) + "/"
	ms := &multiStatus{}
	resp := ms.add(homeURL)
	answer(resp, req, propResource{kind: kindCalendarHome, who: who, server: s, calendars: calendars, href: homeURL})
	if d >= 1 {
		for i := range calendars {
			cal := calendars[i]
			calResp := ms.add(s.calendarURL(cal))
			answer(calResp, req, propResource{
				kind: kindCalendar, who: who, server: s,
				calendars: calendars, calendar: &cal, href: s.calendarURL(cal),
			})
		}
	}
	ms.write(w)
}

// handleCalendarPropfind answers a PROPFIND on a calendar collection.
//
// At depth 0 it describes the calendar. At depth 1 it also describes every object
// in it, which is how a client that has never synchronised before gets the
// contents of a calendar it has just been told about.
func (s *Server) handleCalendarPropfind(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	req, derr := s.propfindFor(w, r)
	if derr != nil {
		fail(w, derr)
		return
	}
	d, derr := depth(r)
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
	siblings, derr := s.siblingCalendars(r, who, cal)
	if derr != nil {
		fail(w, derr)
		return
	}

	ms := &multiStatus{}
	calResp := ms.add(s.calendarURL(cal))
	answer(calResp, req, propResource{
		kind: kindCalendar, who: who, server: s,
		calendars: siblings, calendar: &cal,
		syncToken: s.syncTokenFor(r, cal), href: s.calendarURL(cal),
	})

	if d >= 1 {
		events, err := s.allEvents(r, cal)
		if err != nil {
			fail(w, s.databaseError(r.Context(), "reading the calendar's events", err))
			return
		}
		for i := range events {
			ev := events[i]
			objResp := ms.add(s.objectURL(cal, ev))
			answer(objResp, req, propResource{
				kind: kindObject, who: who, server: s,
				calendar: &cal, event: &ev, href: s.objectURL(cal, ev),
			})
		}
	}
	ms.write(w)
}

// handleCalendarGet answers a GET on a calendar collection.
//
// RFC 4918 does not define GET on a collection, and a server that invented
// something for it would be answering a question nobody asked. This one reports
// the same body as a PROPFIND at depth 0, which is a document that describes the
// calendar and that a client probing for the collection can read.
//
// The alternative, 405, is what this originally did. Several clients send the GET
// as a liveness check before subscribing, and a 405 makes them conclude the
// calendar has been deleted, which is a conclusion a user then reports as "my
// calendar disappeared".
func (s *Server) handleCalendarGet(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)
	cal, derr := s.resolveCalendar(r, who)
	if derr != nil {
		fail(w, derr)
		return
	}
	if derr := s.requireRead(r, who, cal); derr != nil {
		fail(w, derr)
		return
	}
	siblings, derr := s.siblingCalendars(r, who, cal)
	if derr != nil {
		fail(w, derr)
		return
	}

	req := propfindRequest{allprop: true}
	ms := &multiStatus{}
	resp := ms.add(s.calendarURL(cal))
	answer(resp, req, propResource{
		kind: kindCalendar, who: who, server: s,
		calendars: siblings, calendar: &cal,
		syncToken: s.syncTokenFor(r, cal), href: s.calendarURL(cal),
	})
	ms.write(w)
}

// propfindFor parses the body of a PROPFIND, and answers the request itself when
// the body cannot be read.
//
// It exists so the seven PROPFIND handlers do not each repeat the same four lines
// of body handling, and so the depth and body rules are enforced in one place: a
// rule applied in seven places is a rule that will be applied in six of them.
func (s *Server) propfindFor(w http.ResponseWriter, r *http.Request) (propfindRequest, *davError) {
	body, derr := readBody(w, r, 1<<20)
	if derr != nil {
		return propfindRequest{}, derr
	}
	return parsePropfind(body)
}

// readableCalendars is the set of calendars a principal may read in its own home
// set.
//
// It is the principal's own calendars, and nothing else. A workgroup calendar is
// readable by anybody who addresses it directly, but listing every team calendar
// in every user's home set would put the whole organisation's calendar list in
// front of every user, which is not the same thing as letting them book a meeting.
func (s *Server) readableCalendars(r *http.Request, who policy.Principal) ([]store.Calendar, *davError) {
	rows, err := s.calendars.ByOwner(r.Context(), s.db, who.Address)
	if err != nil {
		return nil, s.databaseError(r.Context(), "listing the calendars", err)
	}
	out := make([]store.Calendar, 0, len(rows))
	for _, cal := range rows {
		if derr := s.requireRead(r, who, cal); derr != nil {
			continue
		}
		out = append(out, cal)
	}
	return out, nil
}

// siblingCalendars is the set a calendar's own position is reported against.
func (s *Server) siblingCalendars(r *http.Request, who policy.Principal, cal store.Calendar) ([]store.Calendar, *davError) {
	if cal.Owner != nil {
		return s.readableCalendars(r, who)
	}
	rows, err := s.calendars.ByWorkgroup(r.Context(), s.db, *cal.WorkgroupID)
	if err != nil {
		return nil, s.databaseError(r.Context(), "listing the workgroup calendars", err)
	}
	return rows, nil
}

// requireRead refuses a principal that may not read a calendar.
//
// The check is made against the calendar service rather than against the row, so
// CalDAV and the web interface cannot come to disagree about who may see a
// calendar. A rule implemented twice is a rule that will be implemented twice.
func (s *Server) requireRead(r *http.Request, who policy.Principal, cal store.Calendar) *davError {
	target, err := s.calSvc.Target(r.Context(), s.db, cal, who)
	if err != nil {
		return s.databaseError(r.Context(), "resolving the calendar's access", err)
	}
	if err := s.calSvc.Authorize(who, policy.ActionReadCalendar, cal, target); err != nil {
		return &davError{
			Status: http.StatusForbidden,
			Code:   "DAV:need-privileges",
			Detail: "this calendar may not be read by this account",
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// MKCALENDAR
// ---------------------------------------------------------------------------

// mkCalendarRequest is an RFC 5689 extended MKCALENDAR body.
type mkCalendarRequest struct {
	displayName string
	description string
	color       string
	timezone    string
	// components are the component names claimed by supported-calendar-component-set.
	// It is checked rather than ignored, because a client that asked for VTODO
	// support and was given a server that silently stores them without ever
	// showing them has been told something false.
	components []string
	// resourceType is the DAV:resourcetype, which must include DAV:collection and
	// a CalDAV calendar. Anything else is a request to create something this
	// service does not host, and 403 is the answer RFC 5689 gives.
	resourceTypeChecked bool
	resourceTypeOK      bool
}

// parseMkCalendar reads an extended MKCALENDAR body.
func parseMkCalendar(body []byte) (mkCalendarRequest, *davError) {
	var out mkCalendarRequest
	if len(strings.TrimSpace(string(body))) == 0 {
		return out, &davError{
			Status: http.StatusBadRequest,
			Detail: "MKCALENDAR needs a body describing the calendar to create",
		}
	}
	root, err := parseXML(body)
	if err != nil {
		return out, &davError{Status: http.StatusBadRequest, Detail: "the MKCALENDAR body is not well-formed XML"}
	}
	if !root.is(nsCalDAV, "mkcalendar") {
		return out, &davError{
			Status: http.StatusBadRequest,
			Detail: "the MKCALENDAR body must be a CalDAV:mkcalendar element, not " + root.localOf(),
		}
	}
	set := root.child(nsDAV, "set")
	prop := set.child(nsDAV, "prop")
	if prop == nil {
		return out, &davError{
			Status: http.StatusBadRequest,
			Detail: "the MKCALENDAR body has no DAV:set of properties to apply",
		}
	}
	for _, kid := range prop.kids {
		name := knownProp(kid.space, kid.local)
		switch name {
		case propDisplayName:
			out.displayName = strings.TrimSpace(kid.text)
		case propCalendarDescription:
			out.description = kid.text
		case propCalendarColor:
			out.color = colourOf(kid)
		case propCalendarTimezone:
			out.timezone = strings.TrimSpace(kid.text)
		case propSupportedComponents:
			for _, comp := range kid.childrenNamed(nsCalDAV, "comp") {
				out.components = append(out.components, comp.attrValue("name"))
			}
		case propResourcetype:
			out.resourceTypeChecked = true
			out.resourceTypeOK = kid.child(nsDAV, "collection") != nil && kid.child(nsCalDAV, "calendar") != nil
		}
	}
	return out, nil
}

// colourOf reads a colour out of either spelling Apple clients use.
func colourOf(n *node) string {
	if v := n.attrValue("color"); v != "" {
		return v
	}
	// The element-text form, which is what some clients send and what older
	// versions of this server's own tests wrote.
	return strings.TrimSpace(n.text)
}

// handleMkCalendar creates a calendar.
//
// MKCALENDAR is a WebDAV MKCOL that takes a body, and it is the only way RFC 4791
// lets a client create a calendar. Its status is 201 with no body, and a client
// that receives 200 has been told nothing happened.
func (s *Server) handleMkCalendar(w http.ResponseWriter, r *http.Request) {
	who := principalOf(r)

	// A calendar is created in a home set, and only in one the caller owns.
	// There is no way to name a workgroup in the body, so a team calendar is
	// created through the administration API where membership can be checked.
	if chi.URLParam(r, "group") != "" {
		fail(w, &davError{
			Status: http.StatusForbidden,
			Code:   "DAV:need-privileges",
			Detail: "a workgroup calendar is created by an administrator, not by a client",
		})
		return
	}
	if _, derr := s.resolvePrincipal(r); derr != nil {
		fail(w, derr)
		return
	}
	name := calendarName(r)
	if strings.TrimSpace(name) == "" {
		fail(w, &davError{Status: http.StatusBadRequest, Detail: "the request names no calendar to create"})
		return
	}

	body, derr := readBody(w, r, 1<<20)
	if derr != nil {
		fail(w, derr)
		return
	}
	req, derr := parseMkCalendar(body)
	if derr != nil {
		fail(w, derr)
		return
	}
	if req.resourceTypeChecked && !req.resourceTypeOK {
		fail(w, &davError{
			Status: http.StatusForbidden,
			Code:   "DAV:resourcetype",
			Detail: "this service hosts calendars, and that is not what this request asks to create",
		})
		return
	}
	for _, comp := range req.components {
		if !strings.EqualFold(comp, "VEVENT") {
			fail(w, &davError{
				Status: http.StatusForbidden,
				Code:   "DAV:supported-calendar-component-set",
				Detail: "this server stores events only, and does not support " + comp,
			})
			return
		}
	}

	// The display name is the calendar's name, and the name is a path segment, so
	// the two cannot be different values. A request that sets one and not the
	// other is asking for a calendar whose name differs from its display name,
	// which this server has nowhere to record.
	display := strings.TrimSpace(req.displayName)
	if display == "" {
		display = name
	}
	if !strings.EqualFold(display, name) {
		fail(w, &davError{
			Status: http.StatusConflict,
			Code:   "DAV:displayname",
			Detail: "the calendar's display name and its name in the path must be the same",
		})
		return
	}

	// A name that already exists in this home set is 405 with DAV:allowed-mock-
	// behaviour-adjacent semantics: RFC 5689 §4.2 says 405, and it is the only
	// answer that tells a client "there is already one there, use it" rather than
	// "you may not do that".
	existing, err := s.calendars.ByOwner(r.Context(), s.db, who.Address)
	if err != nil {
		fail(w, s.databaseError(r.Context(), "listing the calendars", err))
		return
	}
	if _, derr := findByName(existing, name); derr == nil {
		fail(w, &davError{
			Status: http.StatusMethodNotAllowed,
			Code:   "DAV:no-conflicting-calendar-home",
			Detail: "there is already a calendar called " + name + " here",
		})
		return
	}

	tz := strings.TrimSpace(req.timezone)
	if tz == "" {
		tz = "UTC"
	}
	if !validTimezoneName(tz) {
		fail(w, &davError{Status: http.StatusBadRequest, Detail: "that is not a timezone this server will store"})
		return
	}
	colour := req.color
	if colour == "" {
		colour = "#3b82f6"
	} else if !validHexColour(colour) {
		fail(w, &davError{Status: http.StatusBadRequest, Detail: "that is not a colour this server will store"})
		return
	}

	owner := who.Address
	cal, err := s.calendars.Create(r.Context(), s.db, store.CreateCalendarRequest{
		Owner:       &owner,
		Name:        name,
		Color:       normaliseColour(colour),
		Timezone:    tz,
		Description: req.description,
	})
	if err != nil {
		// The unique index now enforces one calendar per name, so a duplicate
		// that slipped past the check above is a constraint violation, and the
		// honest answer is the one the check would have given.
		if strings.Contains(err.Error(), "calendars_owner_name_idx") {
			fail(w, &davError{
				Status: http.StatusMethodNotAllowed,
				Code:   "DAV:no-conflicting-calendar-home",
				Detail: "there is already a calendar called " + name + " here",
			})
			return
		}
		fail(w, s.databaseError(r.Context(), "creating the calendar", err))
		return
	}

	// A newly created calendar has no default component set, no timezone
	// definition to report, and an empty sync token. Those are the values the
	// first PROPFIND will see, and reporting them now means the client's next
	// PROPFIND agrees with the MKCALENDAR it just did.
	w.Header().Set("Location", hrefOf(s.calendarURL(cal)))
	w.WriteHeader(http.StatusCreated)
}

// validTimezoneName reports whether a timezone identifier is storable.
//
// The check is deliberately about the shape of the name rather than about whether
// this server holds a definition of it. A TZID is written into every event
// created under the calendar, and a value that is not a plausible identifier
// -- one holding a newline, a slash-traversal sequence or a semicolon -- ends up
// inside iCalendar property lines where it can change the structure of a document
// rather than only its content. Whether the zone is a real one is the client's
// problem: it may well be sending a zone this server has never heard of and which
// the client will resolve correctly.
func validTimezoneName(tz string) bool {
	if tz == "" || len(tz) > 64 {
		return false
	}
	if tz == "UTC" || tz == "GMT" || tz == "Z" {
		return true
	}
	// Area/Location, optionally with a numeric offset: Europe/Lisbon, and the
	// Etc/GMT+5 and US/Eastern spellings clients actually send.
	slash := strings.Index(tz, "/")
	if slash <= 0 || slash == len(tz)-1 {
		return false
	}
	for i := 0; i < len(tz); i++ {
		c := tz[i]
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
			c >= '0' && c <= '9' || c == '/' || c == '_' || c == '-' || c == '+' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// PROPPATCH
// ---------------------------------------------------------------------------

// proppatchRequest is a parsed PROPPATCH.
type proppatchRequest struct {
	// set and remove are the qualified names in each instruction, in order. A
	// name may appear in both, and the last instruction for a name is the one
	// that applies, which is why they are kept in one ordered list rather than
	// as two sets.
	set    []propName
	remove []propName
	values map[propName]string
}

// parseProppatch reads a PROPPATCH body.
func parseProppatch(body []byte) (proppatchRequest, *davError) {
	out := proppatchRequest{values: map[propName]string{}}
	if len(strings.TrimSpace(string(body))) == 0 {
		return out, &davError{Status: http.StatusBadRequest, Detail: "PROPPATCH needs a body of changes"}
	}
	root, err := parseXML(body)
	if err != nil {
		return out, &davError{Status: http.StatusBadRequest, Detail: "the PROPPATCH body is not well-formed XML"}
	}
	if !root.is(nsDAV, "propertyupdate") {
		return out, &davError{
			Status: http.StatusBadRequest,
			Detail: "a PROPPATCH body must be a DAV:propertyupdate element, not " + root.localOf(),
		}
	}
	for _, instr := range root.kids {
		var removing bool
		switch {
		case instr.is(nsDAV, "set"):
			removing = false
		case instr.is(nsDAV, "remove"):
			removing = true
		default:
			continue
		}
		prop := instr.child(nsDAV, "prop")
		if prop == nil {
			continue
		}
		for _, kid := range prop.kids {
			name := knownProp(kid.space, kid.local)
			if removing {
				out.remove = append(out.remove, name)
				continue
			}
			out.set = append(out.set, name)
			switch name {
			case propDisplayName, propCalendarDescription, propCalendarTimezone:
				out.values[name] = kid.text
			case propCalendarColor:
				out.values[name] = colourOf(kid)
			default:
				// The value is not extracted for properties this server will
				// refuse anyway. A property that is protected or unknown does not
				// need its value read to be reported 403 or 424, and reading it
				// would mean guessing a shape for it.
			}
		}
	}
	return out, nil
}

// handlePropPatch applies property changes to a calendar.
//
// The result is a 207 even when every change failed. RFC 4918 §9.2 makes
// PROPPATCH a per-property operation, and a client that changed three properties
// needs to know which succeeded: reporting one status for the request would leave
// it unable to tell which half of its change it has to retry.
func (s *Server) handlePropPatch(w http.ResponseWriter, r *http.Request) {
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

	body, derr := readBody(w, r, 1<<20)
	if derr != nil {
		fail(w, derr)
		return
	}
	req, derr := parseProppatch(body)
	if derr != nil {
		fail(w, derr)
		return
	}
	if len(req.set) == 0 && len(req.remove) == 0 {
		fail(w, &davError{Status: http.StatusBadRequest, Detail: "PROPPATCH carries no property changes"})
		return
	}

	// A removal that is followed by a set for the same name is the set, because a
	// PROPPATCH's instructions are applied in order and this is the one order
	// clients actually send. Resolving it here rather than in the store keeps the
	// order rule in one place.
	removed := map[propName]bool{}
	for _, name := range req.remove {
		removed[name] = true
	}
	for _, name := range req.set {
		delete(removed, name)
	}

	resp := response{href: hrefOf(s.calendarURL(cal))}
	res := propResource{kind: kindCalendar, who: who, server: s, calendar: &cal, href: s.calendarURL(cal)}

	update := store.UpdateCalendarRequest{}
	var applied []propName

	for _, name := range req.set {
		def, known := propTable[name]
		if !known {
			resp.missing = append(resp.missing, unknownPropNode(name))
			continue
		}
		if def.live {
			resp.found = append(resp.found, def.value(res))
			resp.status = errProtectedProperty
			continue
		}
		raw := req.values[name]
		if def.validate != nil {
			if err := def.validate(raw); err != nil {
				resp.status = asDAVError(err)
				continue
			}
		}
		value := raw
		if def.set != nil {
			stored, err := def.set(res, raw)
			if err != nil {
				resp.status = asDAVError(err)
				continue
			}
			value = stored
		}
		applied = append(applied, name)
		assignCalendarField(&update, name, value)
	}

	// Nothing to write means nothing to write. Reporting 207 with the properties
	// echoed is right, and skipping the store call avoids a pointless UPDATE that
	// would bump updated_at for a request that changed nothing.
	if len(applied) > 0 {
		if err := s.calendars.Update(r.Context(), s.db, cal.ID, update); err != nil {
			fail(w, s.databaseError(r.Context(), "saving the calendar's properties", err))
			return
		}
		// The values echoed below are the stored ones, not the ones that were
		// asked for, so the row is read back. Without this a client that set a
		// displayname of 60 characters is told 60 characters back even if the
		// column truncated it, and the two disagree until the next PROPFIND.
		if refreshed, err := s.calendars.Get(r.Context(), s.db, cal.ID); err == nil {
			cal = refreshed
			res.calendar = &cal
		}
	}

	// Every property the request touched is echoed, with its stored value, whether
	// or not it was stored. RFC 4918 §9.2 requires the response to describe the
	// state of the resource after the change, and a client that cannot read that
	// state has to re-PROPFIND the whole calendar to find out what happened.
	for _, name := range req.set {
		space, local := splitProp(name)
		if def, known := propTable[name]; known && def.value != nil {
			if v := def.value(res); v != nil {
				resp.found = append(resp.found, v)
				continue
			}
		}
		resp.found = append(resp.found, el(space, local))
	}
	// A removal that was applied takes the property's value away. There is no
	// value that means "removed" for a property the server computes, so a
	// removal of a settable property is reported as an empty element, which is
	// what RFC 4918 §14.2 defines it as.
	for name := range removed {
		space, local := splitProp(name)
		resp.found = append(resp.found, el(space, local))
	}

	ms := &multiStatus{responses: []response{resp}}
	ms.write(w)
}

// unknownPropNode is how a property this server does not have is reported.
func unknownPropNode(name propName) *node {
	space, local := splitProp(name)
	return el(space, local)
}

// assignCalendarField maps a property name onto a store update field.
func assignCalendarField(u *store.UpdateCalendarRequest, name propName, value string) {
	switch name {
	case propDisplayName:
		// The label only. Writing the name would move the collection's URL, and a
		// client that has been subscribing to that URL for a year would find it
		// gone after somebody renamed a calendar in a list -- which reads as the
		// calendar having been deleted rather than renamed.
		v := value
		u.DisplayName = &v
	case propCalendarDescription:
		v := value
		u.Description = &v
	case propCalendarColor:
		v := normaliseColour(value)
		u.Color = &v
	case propCalendarTimezone:
		v := value
		u.Timezone = &v
	}
}

// ---------------------------------------------------------------------------
// DELETE a calendar
// ---------------------------------------------------------------------------

// handleDeleteCalendar removes a calendar and everything in it.
//
// The events go with it. That is not a decision this handler makes: the schema
// cascades, and a calendar with no events in it but a resource row pointing at it
// is the state a client would then have to reason about. A client that wants the
// events kept should copy them out first, which MKCALENDAR followed by a
// calendar-multiget is for.
func (s *Server) handleDeleteCalendar(w http.ResponseWriter, r *http.Request) {
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

	// A calendar collection is a collection, so RFC 4918 §9.6 makes DELETE
	// recursive by definition. There is no Depth: 0 form worth having here,
	// because a calendar with no events is not something a client asks for.
	if err := s.calendars.Delete(r.Context(), s.db, cal.ID); err != nil {
		fail(w, s.databaseError(r.Context(), "deleting the calendar", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requireWrite refuses a principal that may not modify a calendar.
func (s *Server) requireWrite(r *http.Request, who policy.Principal, cal store.Calendar) *davError {
	target, err := s.calSvc.Target(r.Context(), s.db, cal, who)
	if err != nil {
		return s.databaseError(r.Context(), "resolving the calendar's access", err)
	}
	if err := s.calSvc.Authorize(who, policy.ActionWriteCalendar, cal, target); err != nil {
		return &davError{
			Status: http.StatusForbidden,
			Code:   "DAV:need-privileges",
			Detail: "this calendar may not be changed by this account",
		}
	}
	return nil
}

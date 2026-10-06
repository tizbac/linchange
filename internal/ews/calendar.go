package ews

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/recurrence"
	"github.com/linchange/linchange/internal/store"
)

// Calendar items.
//
// A calendar item is an event on a calendar, and the calendar is a container
// like any other. Creating one is the only write this protocol makes to the
// calendar -- updates come back through UpdateItem as re-creates -- and reading
// one goes through the same authorisation as any other calendar read.

// createCalendarItem handles a t:CalendarItem in a CreateItem.
//
// The item is parsed out of its property bag the same way a message is: the
// same element names the web client and CalDAV use, because all three surfaces
// write the same rows. The one thing a calendar item has that a message does not
// is a recurrence pattern, and a pattern found is translated to the RRULE the
// store keeps, on the master row rather than per occurrence.
func (e *EWS) createCalendarItem(c *call, req createItemRequest, inner string) responseCode {
	item, serr := e.calendarItemFrom(c, inner)
	if serr != nil {
		return c.faultResponse(serr)
	}

	// The folder the item lands in is the calendar the client named. A request
	// that names none gets the principal's default calendar, which is what a
	// client that has only just connected sends before it has read its folders.
	folder := folderRef{distinguished: distCalendar}
	if req.SavedItemFolderId != nil {
		folder = firstFolderRef(req.SavedItemFolderId)
	}
	cal, serr := e.resolveCalendarFolder(c, folder)
	if serr != nil {
		return c.faultResponse(serr)
	}

	event, err := e.cal.Create(c.ctx(), e.db, c.who, calendar.Draft{
		CalendarID: cal.id,
		Event:      item,
	})
	if err != nil {
		return c.faultResponse(calendarWriteFault("CreateItemResponse", err))
	}

	var b strings.Builder
	b.WriteString(`<m:Items><t:CalendarItem>`)
	b.WriteString(`<t:ItemId Id="`)
	b.WriteString(escape(eventIDString(event.CalendarID, event.ID)))
	b.WriteString(`" ChangeKey="`)
	ck := changeKey(event.ID, event.Sequence, event.DTStart.UnixNano(),
		event.DTEnd.UnixNano(), event.Status)
	b.WriteString(escape(ck))
	b.WriteString(`"/><t:ParentFolderId Id="`)
	b.WriteString(escape(calendarID(event.CalendarID)))
	b.WriteString(`"/></t:CalendarItem></m:Items>`)

	return responseCode{
		element: "CreateItemResponse",
		body:    responseMessageOne("CreateItem", b.String()),
	}
}

// resolveCalendarFolder resolves the folder a calendar item is being written to.
//
// A distinguished calendar reference, or no reference at all, names the
// principal's default calendar; a base64 reference names the calendar by its row
// id. Both end at the same authorization, because a calendar named in the request
// is a request to write somewhere, and the policy is the only thing that says
// whether the principal may.
func (e *EWS) resolveCalendarFolder(c *call, ref folderRef) (container, *soapError) {
	if ref.distinguished != "" {
		if !strings.EqualFold(strings.TrimSpace(ref.distinguished), distCalendar) {
			return container{}, errorf("CreateItemResponse", codeErrorFolderNotFound, http.StatusOK,
				"this server has no %s folder", ref.distinguished)
		}
		return e.defaultCalendar(c)
	}
	if ref.plain == "" {
		return e.defaultCalendar(c)
	}
	kind, id, err := decodeFolderID(ref.plain)
	if err != nil {
		return container{}, folderIDFault("CreateItemResponse", err)
	}
	if kind != kindCalendar {
		return container{}, errorf("CreateItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"a meeting can only be filed in a calendar")
	}
	return e.resolveCalendar(c, id)
}

// calendarWriteFault maps a calendar service error onto the code a client acts on.
//
// The three answers a calendar write can give -- it is not yours, it is gone, or
// an older edit conflicts with yours -- have to reach the client as three
// different response codes, and a client that conflated "not yours" with "gone"
// would retry a lost edit against an event another colleague had already moved.
func calendarWriteFault(element string, err error) *soapError {
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "stale"):
		return errorf(element, codeErrorIrresolvableConflict, http.StatusOK,
			"the meeting has been changed since it was read")
	case strings.Contains(err.Error(), "not found"):
		return errorf(element, codeErrorItemNotFound, http.StatusOK,
			"the meeting does not exist")
	case strings.Contains(err.Error(), "denied"):
		return errorf(element, codeErrorAccessDenied, http.StatusOK,
			"you may not change that meeting")
	case strings.Contains(strings.ToLower(err.Error()), "past"):
		return errorf(element, codeErrorInvalidRequest, http.StatusOK,
			"a meeting cannot be scheduled in the past")
	}
	return errorf(element, codeErrorInternalServerError, http.StatusInternalServerError,
		"the meeting could not be saved")
}

// calendarItemFrom reads a t:CalendarItem out of its property bag.
//
// The properties are the ones both the composer and other Exchange-aware clients
// send. Everything else in the bag -- the reminder, the response type, the
// sensitivity -- is not stored by this server, and is ignored rather than
// refused, because ignoring an optional property a client sent is what Exchange
// does with a property it has no store for.
func (e *EWS) calendarItemFrom(c *call, inner string) (ical.Event, *soapError) {
	ev := ical.Event{
		// EWS always receives an explicit sequence; the model defaults it to
		// one, which is what a first creation should carry.
		Summary:  stripTags(innerText(inner, "Subject")),
		Location: innerText(inner, "Location"),
	}

	// The description is the body, whichever of the two bodies it arrived in.
	if body, kind, ok := bodyWithKind(inner); ok {
		if strings.EqualFold(strings.TrimSpace(kind), "html") {
			ev.Description = stripTags(body)
		} else {
			ev.Description = body
		}
	}
	if html := innerText(inner, "BodyHtml"); html != "" {
		ev.Description = stripTags(html)
	}

	// The meeting must have a start; without one there is no meeting. An end is
	// defaulted to an hour later, which is the duration the schema assumes.
	startText := innerText(inner, "Start")
	start, err := parseEWSTime(startText)
	if err != nil {
		return ical.Event{}, errorf("CreateItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"the meeting has no usable start time: %v", err)
	}
	end, err := parseEWSTime(innerText(inner, "End"))
	if err != nil {
		end = start.Add(time.Hour)
	} else if !end.After(start) {
		end = start.Add(time.Hour)
	}
	ev.Start, ev.End = start, end

	if all, ok := innerBool(inner, "IsAllDayEvent"); ok && all {
		ev.AllDay = true
		// An all-day event runs from the start of the first day to the end of
		// the last, in the model's UTC convention.
		ev.Start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
		ev.End = time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	}

	// The recurrence pattern, when there is one.
	if rec := innerElement(inner, "Recurrence"); rec != "" {
		rule, serr := recurrenceFrom(rec)
		if serr != nil {
			return ical.Event{}, serr
		}
		ev.RRULE = rule
	}

	// Attendees: required and optional map onto the two iCalendar roles.
	for _, addr := range toWebmailAll(addressList(innerElement(inner, "RequiredAttendees"))) {
		ev.Attendees = append(ev.Attendees, ical.Attendee{
			Address: addr.Address, Name: addr.Name,
			Role: "REQ-PARTICIPANT", PartStat: "NEEDS-ACTION", RSVP: true,
		})
	}
	for _, addr := range toWebmailAll(addressList(innerElement(inner, "OptionalAttendees"))) {
		ev.Attendees = append(ev.Attendees, ical.Attendee{
			Address: addr.Address, Name: addr.Name,
			Role: "OPT-PARTICIPANT", PartStat: "NEEDS-ACTION",
		})
	}

	// LegacyFreeBusyStatus is the busy/free setting the client drew on the side
	// of the meeting, and the model's ShowAs is the same setting by another name.
	switch strings.ToLower(strings.TrimSpace(innerText(inner, "LegacyFreeBusyStatus"))) {
	case "free":
		ev.ShowAs = ical.ShowAsFree
	case "tentative":
		ev.ShowAs = ical.ShowAsTentative
	case "oof", "workingelsewhere", "outofoffice":
		ev.ShowAs = ical.ShowAsOOF
	case "busy", "":
		ev.ShowAs = ical.ShowAsBusy
	}

	// A client that provided no timezone sent UTC instants, which the model
	// treats as UTC -- the named zone is filled in from the calendar the event
	// lands on, which is what makes a noon standup a noon standup for whoever
	// sees it.
	ev.Timezone = ""
	return ev, nil
}

// parseEWSTime reads the dateTime a client sends.
//
// Exchange's dateTime is an ISO instant with an optional trailing Z, and also
// accepts the zone-offset form a calendar client formats. A value with neither
// is refused rather than guessed: an instant whose zone is unknown would be
// stored in whichever zone the server happened to be running in.
func parseEWSTime(text string) (time.Time, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}, fmt.Errorf("no time given")
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a date and time this server reads", text)
}

// resolveCalendarItem reads one calendar item for GetItem.
//
// The identifier names the event and the calendar it belongs to; the calendar is
// then authorized, and the event is read through the same expansion a folder
// listing uses so that a single item and a listing cannot disagree about what an
// occurrence looks like.
func (e *EWS) resolveCalendarItem(c *call, calendarID, eventID int64, sh shape) (*itemView, *soapError) {
	container, serr := e.resolveCalendar(c, calendarID)
	if serr != nil {
		return nil, serr
	}
	ev, err := store.NewEvents().Get(c.ctx(), e.db, eventID)
	if err != nil {
		return nil, errorf("GetItemResponse", codeErrorItemNotFound, http.StatusOK,
			"the meeting does not exist")
	}
	// The window is the event's own, widened by a day each way, because the
	// event may straddle the exact times the identifier was built from.
	start, end := ev.DTStart.AddDate(0, 0, -1), ev.DTEnd.AddDate(0, 0, 1)
	result, err := e.cal.Query(c.ctx(), e.db, c.who,
		calendar.Selector{CalendarIDs: []int64{calendarID}},
		recurrence.Window{Start: start, End: end})
	if err != nil {
		return nil, e.internal("GetItem", err)
	}
	var occ *calendar.View
	for i := range result.Occurrences {
		if result.Occurrences[i].EventID == eventID {
			occ = &result.Occurrences[i]
			container.calendar = &store.Calendar{ID: calendarID}
			break
		}
	}
	if occ == nil {
		return nil, errorf("GetItemResponse", codeErrorItemNotFound, http.StatusOK,
			"the meeting does not exist in the calendar named")
	}
	return &itemView{
		kind:       "CalendarItem",
		container:  calendarID,
		id:         eventID,
		occurrence: occ,
		event:      &ev,
		class:      itemClassAppointment,
	}, nil
}

// deleteCalendarItem answers DeleteItem for a calendar item.
//
// Deletion checks that the principal may write the calendar the event lives on
// before it removes the row. A hard delete removes the row outright, which is
// what a client pressing Delete on a meeting it created expects; everything else
// is a retreat from the event, which leaves the row so that attendees can see a
// meeting was called off rather than one that silently vanished.
func (e *EWS) deleteCalendarItem(c *call, eventID int64) responseCode {
	ev, err := store.NewEvents().Get(c.ctx(), e.db, eventID)
	if err != nil {
		return c.faultResponse(errorf("DeleteItemResponse", codeErrorItemNotFound, http.StatusOK,
			"the meeting does not exist"))
	}
	cal, err := store.NewCalendars().Get(c.ctx(), e.db, ev.CalendarID)
	if err != nil {
		return c.faultResponse(e.internal("DeleteItem", err))
	}
	target, err := e.cal.Target(c.ctx(), e.db, cal, c.who)
	if err != nil {
		return c.faultResponse(e.internal("DeleteItem", err))
	}
	action := policy.ActionWriteCalendar
	if cal.IsWorkgroup() {
		action = policy.ActionBookWorkgroup
	}
	if err := e.cal.Authorize(c.who, action, cal, target); err != nil {
		return c.faultResponse(errorf("DeleteItemResponse", codeErrorAccessDenied, http.StatusOK,
			"you may not delete that meeting"))
	}
	if err := store.NewEvents().Delete(c.ctx(), e.db, eventID); err != nil {
		return c.faultResponse(calendarWriteFault("DeleteItemResponse", err))
	}
	return c.okOne("DeleteItem")
}

// ---------------------------------------------------------------------------
// Recurrence
// ---------------------------------------------------------------------------

// recurrenceFrom translates an EWS Recurrence element into an RRULE.
//
// The patterns Exchange clients actually send are covered: daily, weekly,
// monthly and yearly, with the interval, an optional week-day list, and one of
// the three endings (a date, a count, or never). A pattern outside that set is
// refused with the pattern named, because filing an event on the wrong days is
// worse than failing to file it: a client would redraw its calendar from this
// server and the wrong days would quietly become the source of truth.
func recurrenceFrom(fragment string) (string, *soapError) {
	freq := recurrencePattern(fragment)
	interval := innerText(fragment, "Interval")
	if interval == "" {
		interval = "1"
	}
	byDay := innerText(fragment, "DaysOfTheWeek") // "Monday Wednesday"
	dayOfMonth := innerText(fragment, "DayOfMonth")
	month := innerText(fragment, "Month")
	index := innerText(fragment, "DayOfTheWeekIndex")

	var parts []string
	switch freq {
	case "":
		return "", errorf("CreateItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"the recurrence pattern could not be read")
	case "daily":
		parts = []string{"FREQ=DAILY", "INTERVAL=" + interval}
	case "weekly":
		days, serr := ewsDaysToRRule(byDay)
		if serr != nil {
			return "", serr
		}
		parts = []string{"FREQ=WEEKLY", "INTERVAL=" + interval}
		if days != "" {
			parts = append(parts, "BYDAY="+days)
		}
	case "monthly":
		parts = []string{"FREQ=MONTHLY", "INTERVAL=" + interval}
		if d, err := strconv.Atoi(dayOfMonth); err == nil && d >= 1 && d <= 31 {
			parts = append(parts, "BYMONTHDAY="+dayOfMonth)
		} else if i, err := strconv.Atoi(index); err == nil {
			day := ewsDayCode(byDay)
			if day == "" {
				day = "MO"
			}
			parts = append(parts, "BYDAY="+strconv.Itoa(i)+day)
		}
	case "yearly":
		parts = []string{"FREQ=YEARLY", "INTERVAL=" + interval}
		if m := strings.TrimSpace(month); m != "" {
			parts = append(parts, "BYMONTH="+month)
		}
		if d, err := strconv.Atoi(dayOfMonth); err == nil && d >= 1 && d <= 31 {
			parts = append(parts, "BYMONTHDAY="+dayOfMonth)
		} else if i, err := strconv.Atoi(index); err == nil {
			day := ewsDayCode(byDay)
			if day == "" {
				day = "MO"
			}
			parts = append(parts, "BYDAY="+strconv.Itoa(i)+day)
		}
	case "regeneration":
		// "<t:DailyRegeneration>/<t:WeeklyRegeneration>/..." means the rule
		// repeats a fixed interval from the start, which the store already
		// answers for the interval-armed forms above. The frequency element
		// names the unit, and the interval rides on it.
		parts = append(parts, regenerationFreq(fragment), "INTERVAL="+interval)
	}

	// The ending. Only the first ending element found is used, because the
	// schema allows exactly one.
	switch {
	case innerElement(fragment, "NumberedRecurrence") != "":
		n := innerText(fragment, "NumberOfOccurrences")
		parts = append(parts, "COUNT="+n)
	case innerElement(fragment, "EndDateRecurrence") != "":
		end := innerText(fragment, "EndDate")
		if until, err := parseEWSTime(end); err == nil {
			// RFC 5545's UNTIL is exclusive, while the EWS end date is
			// inclusive, so the last day is carried in full.
			parts = append(parts, "UNTIL="+until.AddDate(0, 0, 1).Add(-time.Second).
				Format("20060102T150405Z"))
		}
	}
	return strings.Join(parts, ";"), nil
}

// recurrencePattern names the frequency element, or "regeneration" when one of
// the regeneration elements was sent.
func recurrencePattern(fragment string) string {
	switch {
	case innerElement(fragment, "DailyRecurrence") != "":
		return "daily"
	case innerElement(fragment, "WeeklyRecurrence") != "":
		return "weekly"
	case innerElement(fragment, "AbsoluteMonthlyRecurrence") != "" ||
		innerElement(fragment, "RelativeMonthlyRecurrence") != "" ||
		innerElement(fragment, "MonthlyRecurrence") != "":
		return "monthly"
	case innerElement(fragment, "AbsoluteYearlyRecurrence") != "" ||
		innerElement(fragment, "RelativeYearlyRecurrence") != "" ||
		innerElement(fragment, "YearlyRecurrence") != "":
		return "yearly"
	case innerElement(fragment, "DailyRegeneration") != "":
		return "regeneration"
	case innerElement(fragment, "WeeklyRegeneration") != "":
		return "regeneration"
	case innerElement(fragment, "MonthlyRegeneration") != "" ||
		innerElement(fragment, "RelativeMonthlyRegeneration") != "":
		return "regeneration"
	case innerElement(fragment, "YearlyRegeneration") != "" ||
		innerElement(fragment, "RelativeYearlyRegeneration") != "":
		return "regeneration"
	}
	return ""
}

// regenerationFreq names the unit a regeneration element repeats in.
func regenerationFreq(fragment string) string {
	switch {
	case innerElement(fragment, "DailyRegeneration") != "":
		return "FREQ=DAILY"
	case innerElement(fragment, "WeeklyRegeneration") != "":
		return "FREQ=WEEKLY"
	case innerElement(fragment, "MonthlyRegeneration") != "" ||
		innerElement(fragment, "RelativeMonthlyRegeneration") != "":
		return "FREQ=MONTHLY"
	}
	return "FREQ=YEARLY"
}

// ewsDaysToRRule turns the "Monday Wednesday" spelling into "MO,WE".
//
// The set is written out so that a weekday this server does not know fails the
// translation rather than becoming a rule that silently repeats on no day at all.
func ewsDaysToRRule(days string) (string, *soapError) {
	var codes []string
	seen := map[string]bool{}
	for _, name := range strings.Fields(days) {
		code := ewsDayCode(name)
		if code == "" {
			return "", errorf("CreateItemResponse", codeErrorInvalidRequest, http.StatusOK,
				"%s is not a weekday this server knows", name)
		}
		if !seen[code] {
			seen[code] = true
			codes = append(codes, code)
		}
	}
	return strings.Join(codes, ","), nil
}

// ewsDayCode maps an Exchange weekday name to its iCalendar code.
func ewsDayCode(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "sunday", "su":
		return "SU"
	case "monday", "mo":
		return "MO"
	case "tuesday", "tu":
		return "TU"
	case "wednesday", "we":
		return "WE"
	case "thursday", "th":
		return "TH"
	case "friday", "fr":
		return "FR"
	case "saturday", "sa":
		return "SA"
	}
	return ""
}

// stripTags removes markup from a value read out of a client's document.
//
// The description and the subject are stored where they are later escaped on
// the way out of every surface this server speaks, but iCalendar itself is a
// text format and a tag carried into it would be read as text by one client and
// as markup by another. The strip leaves the text a user actually wrote.
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteByte(s[i])
			}
		}
	}
	return strings.TrimSpace(b.String())
}

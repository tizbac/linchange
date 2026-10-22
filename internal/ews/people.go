package ews

import (
	"net/http"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/recurrence"
	"github.com/linchange/linchange/internal/store"
)

// People and time.
//
// These operations are the address book: resolving a name to a mailbox,
// expanding a group into its members, answering "when is somebody busy", and
// declaring what time zones the server knows. None of them writes state and all
// of them answer from the directory -- the users and workgroup tables -- which
// is the only address book this server has.

// resolveNamesRequest is the body of ResolveNames.
type resolveNamesRequest struct {
	UnresolvedEntry       string        `xml:"UnresolvedEntry"`
	ReturnFullContactData bool          `xml:"ReturnFullContactData"`
	ContactDataShape      *shapeRequest `xml:"ContactDataShape"`
}

// resolveNames answers ResolveNames (MS-OXECWS §2.2.10.28).
//
// The directory is the user table: an entry is matched against the account's
// address and display name, and every match whose mailbox is enabled is a
// resolution. Exactly one match is the common answer; several are reported with
// the multiple-matches code, which is what tells a client to show a chooser; none
// is reported as no results, which is what tells it to show "did you mean
// someone else".
func (e *EWS) resolveNames(c *call) responseCode {
	var req resolveNamesRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	entry := strings.ToLower(strings.TrimSpace(req.UnresolvedEntry))
	if entry == "" {
		return c.faultResponse(errorf("ResolveNamesResponse", codeErrorInvalidRequest, http.StatusOK,
			"ResolveNames needs an UnresolvedEntry"))
	}

	users, err := store.NewUsers().List(c.ctx(), e.db)
	if err != nil {
		return c.faultResponse(e.internal("ResolveNames", err))
	}

	type match struct {
		address, name, title, company, business, mobile string
	}
	var matches []match
	seen := make(map[string]int)
	add := func(m match) {
		key := strings.ToLower(m.address)
		if i, ok := seen[key]; ok {
			// The user row gives the identity; the directory card gives the
			// fields the user row does not have.
			if matches[i].title == "" {
				matches[i].title = m.title
			}
			if matches[i].company == "" {
				matches[i].company = m.company
			}
			if matches[i].business == "" {
				matches[i].business = m.business
			}
			if matches[i].mobile == "" {
				matches[i].mobile = m.mobile
			}
			return
		}
		seen[key] = len(matches)
		matches = append(matches, m)
	}
	for _, u := range users {
		if !u.Enabled || !u.MailboxEnabled {
			continue
		}
		address := strings.ToLower(u.Address)
		name := strings.ToLower(u.DisplayName)
		// The entry is matched however it was written: an address resolves by
		// exact address first, then by being a prefix of one, and a name
		// resolves by containing the entry.
		if address == entry || name == entry ||
			strings.HasPrefix(address, entry) || strings.Contains(name, entry) {
			add(match{address: u.Address, name: u.DisplayName})
		}
	}
	// The organization directory holds people who are not users of this
	// mailbox server as well as titles and telephone numbers for those who
	// are, so it is part of the same resolution answer.
	if e.contacts != nil {
		entries, err := e.contacts.DirectoryEntries(c.ctx())
		if err != nil {
			e.log.Warn("EWS: could not read the organization directory", "error", err)
		} else {
			for _, d := range entries {
				address := strings.ToLower(d.Address)
				name := strings.ToLower(d.Name)
				if address == entry || name == entry ||
					strings.HasPrefix(address, entry) || strings.Contains(name, entry) {
					add(match{
						address: d.Address, name: d.Name,
						title: d.Title, company: d.Company,
						business: d.BusinessPhone, mobile: d.MobilePhone,
					})
				}
			}
		}
	}

	sh := parseShape(req.ContactDataShape)
	var b strings.Builder
	b.WriteString(`<m:ResolutionSet>`)
	for i, m := range matches {
		b.WriteString(`<t:Resolution Index="`)
		b.WriteString(itoa(i))
		b.WriteString(`">`)
		writeMailbox(&b, m.name, m.address)
		if req.ReturnFullContactData || sh.all() {
			b.WriteString(`<t:Contact><t:DisplayName>`)
			b.WriteString(escape(m.name))
			b.WriteString(`</t:DisplayName>`)
			if m.company != "" {
				b.WriteString(`<t:CompanyName>`)
				b.WriteString(escape(m.company))
				b.WriteString(`</t:CompanyName>`)
			}
			if m.title != "" {
				b.WriteString(`<t:JobTitle>`)
				b.WriteString(escape(m.title))
				b.WriteString(`</t:JobTitle>`)
			}
			if m.business != "" || m.mobile != "" {
				b.WriteString(`<t:PhoneNumbers>`)
				if m.business != "" {
					b.WriteString(`<t:Entry Key="BusinessPhone">`)
					b.WriteString(escape(m.business))
					b.WriteString(`</t:Entry>`)
				}
				if m.mobile != "" {
					b.WriteString(`<t:Entry Key="MobilePhone">`)
					b.WriteString(escape(m.mobile))
					b.WriteString(`</t:Entry>`)
				}
				b.WriteString(`</t:PhoneNumbers>`)
			}
			b.WriteString(`<t:EmailAddresses><t:Entry Key="EmailAddress1">`)
			b.WriteString(escape(m.address))
			b.WriteString(`</t:Entry></t:EmailAddresses></t:Contact>`)
		}
		b.WriteString(`</t:Resolution>`)
	}
	b.WriteString(`</m:ResolutionSet>`)

	code := ""
	switch len(matches) {
	case 0:
		// No match is an answer, not an error in the transport sense: the
		// response message still carries the no-results code, which is how the
		// client tells "no such person" from "the request was nonsense".
		return c.okPeople("ResolveNames", codeErrorNameResolutionNoResults, b.String())
	case 1:
		code = "NoError"
	default:
		// Several matches for one entry is a chooser, not an error. The code
		// and the candidates travel together, because a client told only that
		// there were several would have to ask again without them.
		code = codeErrorNameResolutionMultipleMatches
	}
	return c.okPeople("ResolveNames", code, b.String())
}

// expandDLRequest is the body of ExpandDL.
type expandDLRequest struct {
	Mailbox *mailboxXML `xml:"Mailbox"`
}

// expandDL answers ExpandDL (MS-OXECWS §2.2.10.7).
//
// A distribution list here is a workgroup: a group address whose members hold
// mailboxes. The address names the group by its local part and domain, matching
// the workgroup's name and its address table, and the members are returned the
// same way a resolved contact is -- as mailboxes a client can address.
func (e *EWS) expandDL(c *call) responseCode {
	var req expandDLRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.Mailbox == nil {
		return c.faultResponse(errorf("ExpandDLResponse", codeErrorInvalidRequest, http.StatusOK,
			"ExpandDL needs a Mailbox"))
	}
	address := strings.ToLower(req.Mailbox.address())
	local, domain, ok := splitAddress(address)
	if !ok {
		return c.faultResponse(errorf("ExpandDLResponse", codeErrorItemNotFound, http.StatusOK,
			"%s is not a group address", req.Mailbox.address()))
	}

	group, err := store.NewWorkgroups().ByName(c.ctx(), e.db, domain, local)
	if err != nil {
		if store.IsNotFound(err) {
			return c.faultResponse(errorf("ExpandDLResponse", codeErrorNonExistentMailbox, http.StatusOK,
				"there is no group at %s", req.Mailbox.address()))
		}
		return c.faultResponse(e.internal("ExpandDL", err))
	}
	members, err := store.NewWorkgroups().Members(c.ctx(), e.db, group.ID)
	if err != nil {
		return c.faultResponse(e.internal("ExpandDL", err))
	}

	var b strings.Builder
	b.WriteString(`<m:DLExpansion TotalItemsInView="`)
	b.WriteString(itoa(len(members)))
	b.WriteString(`" IncludesLastItemInRange="true">`)
	for _, m := range members {
		// A member is named by the account table rather than redacted: the
		// member is somebody the principal can already see in the directory.
		user, uerr := store.NewUsers().Get(c.ctx(), e.db, m.Address)
		if uerr != nil {
			writeMailbox(&b, "", m.Address)
		} else {
			writeMailbox(&b, user.DisplayName, m.Address)
		}
	}
	b.WriteString(`</m:DLExpansion>`)
	return c.okPeople("ExpandDL", "NoError", b.String())
}

// getUserAvailabilityRequest is the body of GetUserAvailability.
type getUserAvailabilityRequest struct {
	MailboxDataArray *struct {
		MailboxData []struct {
			Email *mailboxXML `xml:"Email"`
			// AttendeeType is Required, Optional or Resource.
			AttendeeType string `xml:"AttendeeType"`
		} `xml:"MailboxData"`
	} `xml:"MailboxDataArray"`
	FreeBusyViewOptions *struct {
		TimeWindow *struct {
			StartTime string `xml:"StartTime"`
			EndTime   string `xml:"EndTime"`
		} `xml:"TimeWindow"`
		// MergedFreeBusyIntervalInMinutes is the width of one bit of the
		// merged string.
		MergedFreeBusyIntervalInMinutes int `xml:"MergedFreeBusyIntervalInMinutes"`
		// RequestedView is FreeBusy, FreeBusyMerged, Detailed, DetailedMerged,
		// MergedOnly or None.
		RequestedView string `xml:"RequestedView"`
	} `xml:"FreeBusyViewOptions"`
}

// getUserAvailability answers GetUserAvailability (MS-OXECWS §2.2.10.13).
//
// The free/busy answers come from the calendar service, which already resolves
// who may see whom busy (a calendar the caller may not read is reported hidden
// rather than free, so a client does not double-book against it). The workflow
// is one call per mailbox, then the view the client asked for: the merged string
// when it asked for a merged view, the per-address event lists otherwise.
func (e *EWS) getUserAvailability(c *call) responseCode {
	var req getUserAvailabilityRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if e.cal == nil || req.FreeBusyViewOptions == nil || req.FreeBusyViewOptions.TimeWindow == nil {
		return c.faultResponse(errorf("GetUserAvailabilityResponse", codeErrorInvalidRequest, http.StatusOK,
			"GetUserAvailability needs a time window"))
	}
	start, err := parseEWSTime(req.FreeBusyViewOptions.TimeWindow.StartTime)
	if err != nil {
		return c.faultResponse(errorf("GetUserAvailabilityResponse", codeErrorInvalidRequest, http.StatusOK,
			"the start time could not be read: %v", err))
	}
	end, err := parseEWSTime(req.FreeBusyViewOptions.TimeWindow.EndTime)
	if err != nil {
		return c.faultResponse(errorf("GetUserAvailabilityResponse", codeErrorInvalidRequest, http.StatusOK,
			"the end time could not be read: %v", err))
	}

	// The free/busy window is clamped against the recurrence engine's own
	// bounds, so a window spanning a decade cannot make the expansion explode
	// into a response nobody can receive.
	start, end = clampWindow(start, end)

	var addresses []string
	if req.MailboxDataArray != nil {
		for _, md := range req.MailboxDataArray.MailboxData {
			if md.Email != nil {
				addresses = append(addresses, md.Email.address())
			}
		}
	}
	availability, err := e.cal.FreeBusy(c.ctx(), e.db, c.who, calendar.FreeBusyRequest{
		Addresses: addresses,
		Window:    recurrence.Window{Start: start, End: end},
	})
	if err != nil {
		if isCalendarDenied(err) {
			return c.faultResponse(errorf("GetUserAvailabilityResponse", codeErrorAccessDenied, http.StatusOK,
				"the free/busy data cannot be read"))
		}
		return c.faultResponse(e.internal("GetUserAvailability", err))
	}

	interval := req.FreeBusyViewOptions.MergedFreeBusyIntervalInMinutes
	if interval <= 0 {
		interval = 30
	}
	merged := strings.EqualFold(req.FreeBusyViewOptions.RequestedView, "FreeBusyMerged") ||
		strings.EqualFold(req.FreeBusyViewOptions.RequestedView, "DetailedMerged") ||
		strings.EqualFold(req.FreeBusyViewOptions.RequestedView, "MergedOnly")

	var b strings.Builder
	b.WriteString(`<m:FreeBusyResponseArray>`)
	for _, av := range availability {
		b.WriteString(`<m:FreeBusyResponse>`)
		if av.Hidden {
			b.WriteString(`<m:ResponseMessage><m:ResponseCode>`)
			b.WriteString(codeErrorAccessDenied)
			b.WriteString(`</m:ResponseCode><m:MessageText>the free/busy data for this mailbox is not shared with you</m:MessageText></m:ResponseMessage>`)
		} else {
			b.WriteString(`<m:ResponseMessage><m:ResponseCode>NoError</m:ResponseCode></m:ResponseMessage>`)
		}
		b.WriteString(`<m:FreeBusyView>`)
		viewType := "None"
		if !av.Hidden {
			if merged {
				viewType = "Merged"
			} else {
				viewType = "Detailed"
			}
		}
		b.WriteString(`<m:FreeBusyViewType>`)
		b.WriteString(escape(viewType))
		b.WriteString(`</m:FreeBusyViewType>`)
		if merged && !av.Hidden {
			b.WriteString(`<m:MergedFreeBusy>`)
			b.WriteString(escape(mergedFreeBusy(av.Busy, start, end, interval)))
			b.WriteString(`</m:MergedFreeBusy>`)
		} else if !merged && !av.Hidden {
			b.WriteString(`<m:CalendarEventArray>`)
			for _, blk := range av.Busy {
				b.WriteString(`<m:CalendarEvent><m:StartTime>`)
				b.WriteString(ewsTime(blk.Start))
				b.WriteString(`</m:StartTime><m:EndTime>`)
				b.WriteString(ewsTime(blk.End))
				b.WriteString(`</m:EndTime><m:BusyType>Busy</m:BusyType></m:CalendarEvent>`)
			}
			b.WriteString(`</m:CalendarEventArray>`)
		}
		b.WriteString(`<m:WorkingHours><m:TimeZone><t:Bias>0</t:Bias></m:TimeZone><m:WorkingPeriodArray><m:WorkingPeriod><m:DayOfWeek>Monday Tuesday Wednesday Thursday Friday</m:DayOfWeek><m:StartTimeInMinutes>480</m:StartTimeInMinutes><m:EndTimeInMinutes>1020</m:EndTimeInMinutes></m:WorkingPeriod></m:WorkingPeriodArray></m:WorkingHours>`)
		b.WriteString(`</m:FreeBusyView>`)
		b.WriteString(`</m:FreeBusyResponse>`)
	}
	b.WriteString(`</m:FreeBusyResponseArray>`)
	return c.okPeople("GetUserAvailability", "NoError", b.String())
}

// getServerTimeZones answers GetServerTimeZones (MS-OXECWS §2.2.10.14).
//
// This server stores every instant in UTC and renders it in UTC, so the only
// time zone it can honestly define is UTC. A client that asked for another one
// is told about the one the server actually uses rather than handed a story --
// which is what Exchange itself does when asked for a zone it does not know, by
// answering with the set it does know.
func (e *EWS) getServerTimeZones(c *call) responseCode {
	var req struct {
		Ids []string `xml:"Ids>Id"`
	}
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	var b strings.Builder
	b.WriteString(`<m:TimeZoneDefinitions><t:TimeZoneDefinition Id="UTC" Name="(UTC) Coordinated Universal Time"><t:Periods><t:Period Bias="0" Name="Standard" Id="Std"/></t:Periods></t:TimeZoneDefinition></m:TimeZoneDefinitions>`)
	return c.okPeople("GetServerTimeZones", "NoError", b.String())
}

// okPeople renders a success response message for these operations.
//
// The code travels inside the response message rather than at the transport
// level, which is the EWS convention every client reads: NoError, or the
// resolution code a reply carries along with its data. A resolution that found
// several candidates is still a well-formed answer, but its response message is
// marked Error so a client weights it as the protocol says a non-success reply
// is weighted.
func (c *call) okPeople(operation, code, body string) responseCode {
	if code == "" {
		code = codeNoError
	}
	class := "Success"
	if code != codeNoError {
		class = "Error"
	}
	var b strings.Builder
	b.WriteString(`<m:ResponseCode>`)
	b.WriteString(escape(code))
	b.WriteString(`</m:ResponseCode>`)
	b.WriteString(body)
	return responseCode{
		element: operation + "Response",
		body: `<m:ResponseMessages><m:` + operation + `ResponseMessage ResponseClass="` + class + `">` +
			b.String() +
			`</m:` + operation + `ResponseMessage></m:ResponseMessages>`,
	}
}

// itoa is strconv.Itoa without the import in a file that otherwise has none.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}

// splitAddress splits an address into its local part and its domain.
func splitAddress(address string) (local, domain string, ok bool) {
	address = strings.TrimSpace(address)
	i := strings.LastIndexByte(address, '@')
	if i <= 0 || i == len(address)-1 {
		return "", "", false
	}
	return strings.ToLower(address[:i]), strings.ToLower(address[i+1:]), true
}

// clampWindow bounds a free/busy window to a workable range.
//
// The calendar engine itself clamps to its own maximum, but answering with a
// window so wide that the response must span it is how a request for a decade of
// free/busy becomes a megabyte of '0's; both are bounded here, in the protocol
// layer, which is the layer entitled to say "no".
func clampWindow(start, end time.Time) (time.Time, time.Time) {
	const maxWindow = 62 * 24 * time.Hour
	if end.IsZero() || start.IsZero() || end.Sub(start) <= maxWindow {
		if end.IsZero() {
			end = start.Add(maxWindow)
		}
		return start, end
	}
	return start, start.Add(maxWindow)
}

// mergedFreeBusy builds the digit string of a merged free/busy view.
//
// The string is one digit per interval across the window, in order, as the
// protocol writes it: '0' for free, then the busy levels. A busy block wider than
// one interval marks every interval it spans, which is what lets a client draw
// the block back without knowing where each block began.
func mergedFreeBusy(blocks []calendar.Busy, start, end time.Time, intervalMinutes int) string {
	interval := time.Duration(intervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	n := int(end.Sub(start) / interval)
	if n > 5000 {
		// The window was clamped, but a tiny interval over a wide window can
		// still produce a string a client cannot receive. Half-second steps are
		// not a real free/busy question.
		n = 5000
	}
	out := make([]byte, n)
	levels := make([]int, n)
	for _, blk := range blocks {
		first := int(blk.Start.Sub(start) / interval)
		last := int(blk.End.Sub(start) / interval)
		if last > n {
			last = n
		}
		// Every block the calendar reported is a busy block (a cancelled or
		// transparent event is excluded before it ever reaches a busy query),
		// so every interval a block spans is marked busy.
		for i := first; i < last; i++ {
			if i >= 0 && levels[i] < 2 {
				levels[i] = 2
			}
		}
	}
	for i, lv := range levels {
		out[i] = byte('0' + lv)
	}
	return string(out)
}

// busyTypeName is the name of a busy type for a calendar event element.
func busyTypeName(showAs string) string {
	switch strings.ToLower(showAs) {
	case "free":
		return "Free"
	case "tentative":
		return "Tentative"
	case "oof":
		return "OOF"
	default:
		return "Busy"
	}
}

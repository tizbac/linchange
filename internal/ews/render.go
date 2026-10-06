package ews

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// Rendering items.
//
// One function renders both kinds of item, because the shape contract is the same
// for both and the alternative is two renderers that disagree about what IdOnly
// means. The kind decides which element is written and which properties exist;
// the shape decides which of them are written.
//
// Every value written here is escaped on the way in. A subject is caller data, and
// a response that echoed it unescaped would be a reflected injection in every
// client that renders the XML -- and a client that renders the XML is what a mail
// client does for a living.

// itemView is one item as a renderer needs it.
type itemView struct {
	// kind is the element name: "Message" or "CalendarItem".
	kind string
	// container is the mailbox or the calendar holding the item.
	container int64
	// id is the item's row id within its container.
	id int64
	// message is the stored row. It is nil for a calendar item.
	message *webmail.Message
	// detail is the parsed message, present only when the shape asked for
	// something that reads the blob. Reading it for a listing would mean reading
	// every body in the folder, which is the request a listing exists to avoid.
	detail *webmail.MessageDetail
	// addresses are the header addresses, read for the whole page in one query.
	addresses webmail.HeaderAddresses
	// occurrence is the calendar occurrence. It is nil for a mail item.
	occurrence *calendar.View
	// event is the stored row the occurrence came from. It carries the rule and
	// the free/busy setting, which a View does not: the wire model describes an
	// occurrence and the row describes the series it came out of, and a client
	// needs both to know whether editing this one edits the others.
	event *store.Event
	// class is the MS-OXPROPS item class.
	class string
}

// changeKeyOf returns the item's change key.
//
// A change key is what a client sends back with an update to say which revision it
// saw. For a message it is derived from the row's own mutable columns, so any
// change to them changes it: a counter would have to be stored per item and this
// store's columns are chosen for IMAP.
func (v *itemView) changeKey() string {
	if v.message != nil {
		return changeKey(v.message.ID, v.message.UID, v.message.Size,
			v.message.Date.UTC().UnixNano(),
			strings.Join(v.message.Flags, "\x00"))
	}
	if v.occurrence != nil {
		// The sequence is the revision a calendar client compares on, and the
		// times and status are what a change to the event would alter. A series
		// edit changes the sequence; a detached instance changes both.
		return changeKey(v.occurrence.EventID, v.occurrence.Sequence, v.occurrence.Start.UnixNano(),
			v.occurrence.End.UnixNano(), v.occurrence.Status)
	}
	return changeKey(v.id)
}

// idOf returns the item's identifier.
func (v *itemView) idOf() string {
	if v.occurrence != nil {
		return eventIDString(v.container, v.occurrence.EventID)
	}
	return messageIDString(v.container, v.id)
}

// renderItem writes one <t:Message> or <t:CalendarItem>.
func renderItem(b *strings.Builder, v *itemView, sh shape) {
	switch v.kind {
	case "CalendarItem":
		renderCalendarItem(b, v, sh)
		return
	}
	renderMessageItem(b, v, sh)
}

// renderMessageItem writes a <t:Message>.
//
// The identifier and the parent come first in every response, including an IdOnly
// one, because a client reads them before anything else and a document that put
// the subject first would force it to parse the whole element to find its own
// identifier.
func renderMessageItem(b *strings.Builder, v *itemView, sh shape) {
	b.WriteString(`<t:Message>`)
	writeIdentity(b, v, sh, "Message")

	m := v.message
	if m == nil || sh.identity() {
		b.WriteString(`</t:Message>`)
		return
	}

	asked := func(key string) bool { return sh.asked(key, defaultItemFields) }

	if asked("item:itemclass") || sh.all() {
		b.WriteString(`<t:ItemClass>`)
		b.WriteString(escape(v.class))
		b.WriteString(`</t:ItemClass>`)
	}
	if asked("item:subject") {
		b.WriteString(`<t:Subject>`)
		b.WriteString(escape(m.Subject))
		b.WriteString(`</t:Subject>`)
	}
	if asked("item:size") {
		fmt.Fprintf(b, `<t:Size>%d</t:Size>`, m.Size)
		fmt.Fprintf(b, `<t:AttachmentSize>%d</t:AttachmentSize>`, m.Size)
	}
	if asked("item:hasattachments") {
		fmt.Fprintf(b, `<t:HasAttachments>%t</t:HasAttachments>`, m.Attachments)
		fmt.Fprintf(b, `<t:IsAttachment>%t</t:IsAttachment>`, false)
	}
	if asked("item:datetime") || asked("item:datetimereceived") {
		b.WriteString(`<t:DateTimeCreated>`)
		b.WriteString(ewsTime(m.Date))
		b.WriteString(`</t:DateTimeCreated>`)
	}
	if asked("item:importance") {
		b.WriteString(`<t:Importance>Normal</t:Importance>`)
	}
	if asked("item:isassociated") {
		b.WriteString(`<t:IsAssociated>false</t:IsAssociated>`)
	}

	// The message's own flags are read from the row's IMAP system flags, which
	// is where they live: a message marked read here and in an IMAP client shows
	// the same state in all three because there is one state, not three.
	unread := !hasFlag(m.Flags, `\Seen`)
	if asked("message:isread") {
		fmt.Fprintf(b, `<t:IsRead>%t</t:IsRead>`, !unread)
	}
	if asked("message:isflagged") {
		fmt.Fprintf(b, `<t:IsFlagged>%t</t:IsFlagged>`, hasFlag(m.Flags, `\Flagged`))
	}
	if asked("message:isdraft") {
		fmt.Fprintf(b, `<t:IsDraft>%t</t:IsDraft>`, hasFlag(m.Flags, `\Draft`))
	}
	if asked("message:isresend") {
		b.WriteString(`<t:IsResend>false</t:IsResend>`)
	}
	if asked("message:isfromme") {
		fmt.Fprintf(b, `<t:IsFromMe>%t</t:IsFromMe>`, false)
	}
	if asked("message:isreply") {
		b.WriteString(`<t:IsReply>false</t:IsReply>`)
	}
	if asked("message:isreadreceiptrequested") {
		b.WriteString(`<t:IsReadReceiptRequested>false</t:IsReadReceiptRequested>`)
	}

	// The sender.
	if asked("message:sender") || asked("message:from") {
		b.WriteString(`<t:Sender>`)
		writeMailbox(b, m.FromName, m.From)
		b.WriteString(`</t:Sender>`)
	}
	if asked("message:from") {
		b.WriteString(`<t:From>`)
		writeMailbox(b, m.FromName, m.From)
		b.WriteString(`</t:From>`)
	}
	if asked("message:replyto") && len(v.addresses.ReplyTo) > 0 {
		b.WriteString(`<t:ReplyTo>`)
		for _, a := range v.addresses.ReplyTo {
			writeMailbox(b, a.Name, a.Address)
		}
		b.WriteString(`</t:ReplyTo>`)
	}

	// Recipients, both as mailboxes for the client to work with and as display
	// strings, because it draws the second and edits the first.
	if asked("message:to") || asked("message:displayto") || sh.all() {
		b.WriteString(`<t:ToRecipients>`)
		for _, a := range v.addresses.To {
			writeMailbox(b, a.Name, a.Address)
		}
		b.WriteString(`</t:ToRecipients>`)
		b.WriteString(`<t:DisplayTo>`)
		b.WriteString(escape(displayList(v.addresses.To)))
		b.WriteString(`</t:DisplayTo>`)
	}
	if asked("message:cc") || asked("message:displaycc") || sh.all() {
		b.WriteString(`<t:CcRecipients>`)
		for _, a := range v.addresses.Cc {
			writeMailbox(b, a.Name, a.Address)
		}
		b.WriteString(`</t:CcRecipients>`)
		b.WriteString(`<t:DisplayCc>`)
		b.WriteString(escape(displayList(v.addresses.Cc)))
		b.WriteString(`</t:DisplayCc>`)
	}
	if asked("message:bcc") || asked("message:displaybcc") || sh.all() {
		b.WriteString(`<t:BccRecipients>`)
		for _, a := range v.addresses.Bcc {
			writeMailbox(b, a.Name, a.Address)
		}
		b.WriteString(`</t:BccRecipients>`)
		b.WriteString(`<t:DisplayBcc>`)
		b.WriteString(escape(displayList(v.addresses.Bcc)))
		b.WriteString(`</t:DisplayBcc>`)
	}

	// Dates. DateTimeReceived is when this server took delivery, which is the
	// arrival a server can vouch for; DateTimeSent is what the sender wrote.
	if asked("message:datetimereceived") {
		b.WriteString(`<t:DateTimeReceived>`)
		b.WriteString(ewsTime(m.Date))
		b.WriteString(`</t:DateTimeReceived>`)
	}
	if asked("message:timestamp") {
		b.WriteString(`<t:Timestamp>`)
		b.WriteString(ewsTime(m.Date))
		b.WriteString(`</t:Timestamp>`)
	}
	if asked("message:internetmessageid") {
		b.WriteString(`<t:InternetMessageId>`)
		b.WriteString(escape(m.MessageID))
		b.WriteString(`</t:InternetMessageId>`)
	}
	if asked("item:conversationtopic") || asked("message:conversationtopic") {
		b.WriteString(`<t:ConversationTopic>`)
		b.WriteString(escape(m.Subject))
		b.WriteString(`</t:ConversationTopic>`)
	}

	// The body is read from the parsed message, which exists only if the shape
	// asked for it. Both forms are written: a client that asked for HTML gets
	// HTML, and a client that asked for nothing specific gets the text part,
	// because text is what every client can render.
	if v.detail != nil {
		if asked("item:body") || asked("item:uniquebody") || asked("item:preview") {
			writeBody(b, v.detail, sh)
		}
		if asked("item:subject") && asked("item:normalizedsubject") {
			b.WriteString(`<t:NormalizedSubject>`)
			b.WriteString(escape(strings.TrimPrefix(m.Subject, "Re: ")))
			b.WriteString(`</t:NormalizedSubject>`)
		}
		if asked("item:attachment") || asked("item:attachments") {
			writeAttachments(b, v.detail)
		}
	}
	b.WriteString(`</t:Message>`)
}

// writeIdentity writes the ItemId and ParentFolderId.
//
// It is shared by both kinds of item, and it writes both elements even for an
// IdOnly shape: the parent is what a client uses to know where the item came from,
// and a response that omitted it would make a client guess the folder from the
// request it sent -- which is wrong whenever the item was moved between the
// request and the response.
func writeIdentity(b *strings.Builder, v *itemView, sh shape, element string) {
	b.WriteString(`<t:ItemId Id="`)
	b.WriteString(escape(v.idOf()))
	b.WriteString(`" ChangeKey="`)
	b.WriteString(escape(v.changeKey()))
	b.WriteString(`"/>`)
	if v.container > 0 {
		b.WriteString(`<t:ParentFolderId Id="`)
		if v.kind == "CalendarItem" {
			b.WriteString(escape(calendarID(v.container)))
		} else {
			b.WriteString(escape(folderID(v.container)))
		}
		b.WriteString(`"/>`)
	}
}

// writeBody writes the message body.
//
// A client sends a body in HTML or in plain text and renders whichever one it
// can. Sending HTML to a client that asked for plain text puts markup in front of
// the user; sending text to one that asked for HTML loses the formatting. So the
// request decides: HTML when it was named, text when it was not -- and text is
// also the fallback for a message that has only HTML, because a plain-text pane
// showing markup is worse than one showing a stripped rendering of it.
//
// Exactly one <t:Body> is written. Two of them is a document that parses and is
// ambiguous, and a client that read only the first would silently lose the body
// the second one carried.
func writeBody(b *strings.Builder, detail *webmail.MessageDetail, sh shape) {
	text := detail.TextBody
	if text == "" {
		text = stripHTML(detail.HTMLBody)
	}
	if text == "" {
		text = detail.Snippet
	}

	wantHTML := sh.asked("item:bodyhtml", nil) || sh.asked("item:bodytexthtml", nil)
	if wantHTML && detail.HTMLBody != "" {
		b.WriteString(`<t:Body BodyType="HTML">`)
		b.WriteString(escape(detail.HTMLBody))
		b.WriteString(`</t:Body>`)
	} else {
		b.WriteString(`<t:Body BodyType="Text">`)
		b.WriteString(escape(text))
		b.WriteString(`</t:Body>`)
	}
	if sh.asked("item:preview", nil) || sh.asked("item:snippet", nil) {
		b.WriteString(`<t:Preview>`)
		b.WriteString(escape(detail.Snippet))
		b.WriteString(`</t:Preview>`)
	}
}

// writeAttachments writes the message's attachments.
//
// The content is left out. An attachment in a GetItem response is the whole file
// base64-encoded, and a client that wanted it would ask for it specifically; a
// listing that asked for the attachment list and got every file would be an
// unbounded response on a request that is otherwise a page of rows.
func writeAttachments(b *strings.Builder, detail *webmail.MessageDetail) {
	for _, att := range detail.Attachments {
		b.WriteString(`<t:FileAttachment>`)
		b.WriteString(`<t:AttachmentId Id="`)
		b.WriteString(escape(strconv.Itoa(att.Index)))
		b.WriteString(`"/>`)
		b.WriteString(`<t:Name>`)
		b.WriteString(escape(att.Filename))
		b.WriteString(`</t:Name>`)
		fmt.Fprintf(b, `<t:Size>%d</t:Size>`, att.Size)
		b.WriteString(`<t:ContentType>`)
		b.WriteString(escape(att.MIMEType))
		b.WriteString(`</t:ContentType>`)
		fmt.Fprintf(b, `<t:IsInline>%t</t:IsInline>`, att.Inline)
		b.WriteString(`<t:IsEmbedded>false</t:IsEmbedded>`)
		b.WriteString(`<t:IsContact>false</t:IsContact>`)
		b.WriteString(`</t:FileAttachment>`)
	}
}

// writeMailbox writes a t:Mailbox.
//
// The element is written even when the address is empty. A message with no From
// header exists -- a mailing list that stripped it, a message written by a client
// that allowed a blank sender -- and omitting the whole element would leave a
// client showing an empty line where it expects an address, which is a narrower
// failure than showing no sender element at all and inventing one.
func writeMailbox(b *strings.Builder, name, address string) {
	b.WriteString(`<t:Mailbox>`)
	if name != "" {
		b.WriteString(`<t:Name>`)
		b.WriteString(escape(name))
		b.WriteString(`</t:Name>`)
	}
	b.WriteString(`<t:EmailAddress>`)
	b.WriteString(escape(address))
	b.WriteString(`</t:EmailAddress>`)
	b.WriteString(`<t:RoutingType>SMTP</t:RoutingType>`)
	b.WriteString(`<t:MailboxType>Mailbox</t:MailboxType>`)
	b.WriteString(`</t:Mailbox>`)
}

// renderCalendarItem writes a <t:CalendarItem>.
func renderCalendarItem(b *strings.Builder, v *itemView, sh shape) {
	occ := v.occurrence
	b.WriteString(`<t:CalendarItem>`)
	writeIdentity(b, v, sh, "CalendarItem")
	if occ == nil || sh.identity() {
		b.WriteString(`</t:CalendarItem>`)
		return
	}
	asked := func(key string) bool { return sh.asked(key, defaultCalendarFields) }

	if asked("item:itemclass") {
		b.WriteString(`<t:ItemClass>IPM.Appointment</t:ItemClass>`)
	}
	if asked("calendar:start") {
		b.WriteString(`<t:Start>`)
		b.WriteString(ewsTime(occ.Start))
		b.WriteString(`</t:Start>`)
	}
	if asked("calendar:end") {
		b.WriteString(`<t:End>`)
		b.WriteString(ewsTime(occ.End))
		b.WriteString(`</t:End>`)
	}
	if asked("calendar:isalldayevent") {
		fmt.Fprintf(b, `<t:IsAllDayEvent>%t</t:IsAllDayEvent>`, occ.AllDay)
	}
	if asked("item:subject") {
		b.WriteString(`<t:Subject>`)
		b.WriteString(escape(occ.Summary))
		b.WriteString(`</t:Subject>`)
	}
	if asked("calendar:location") {
		b.WriteString(`<t:Location>`)
		b.WriteString(escape(occ.Location))
		b.WriteString(`</t:Location>`)
	}
	if asked("item:datetime") {
		b.WriteString(`<t:DateTimeCreated>`)
		b.WriteString(ewsTime(occ.Start))
		b.WriteString(`</t:DateTimeCreated>`)
	}
	if asked("calendar:isrecurring") {
		// A recurring event is one whose row carries a rule. The rule lives on
		// the row rather than on the occurrence, because the occurrence is one
		// of fifty and the rule is one: a client that asked "is this one part of
		// a series?" needs the series' answer, not the occurrence's.
		fmt.Fprintf(b, `<t:IsRecurring>%t</t:IsRecurring>`, v.isRecurring())
	}
	if asked("calendar:calendaritemtype") {
		b.WriteString(`<t:CalendarItemType>`)
		b.WriteString(escape(v.calendarItemType()))
		b.WriteString(`</t:CalendarItemType>`)
	}
	if asked("calendar:myresponseto") {
		b.WriteString(`<t:MyResponseTo>Accept</t:MyResponseTo>`)
	}
	if asked("calendar:reminderissubmitted") {
		b.WriteString(`<t:ReminderIsSubmitted>false</t:ReminderIsSubmitted>`)
	}
	if asked("calendar:responsestate") {
		b.WriteString(`<t:ResponseState>None</t:ResponseState>`)
	}
	if asked("calendar:isreceived") {
		b.WriteString(`<t:IsReceived>false</t:IsReceived>`)
	}
	if asked("item:importance") {
		b.WriteString(`<t:Importance>Normal</t:Importance>`)
	}
	if asked("calendar:busytype") {
		b.WriteString(`<t:BusyType>`)
		b.WriteString(escape(v.busyType()))
		b.WriteString(`</t:BusyType>`)
	}
	if asked("calendar:organizer") {
		b.WriteString(`<t:Organizer>`)
		writeMailbox(b, occ.Organizer, occ.OrganizerAddress)
		b.WriteString(`</t:Organizer>`)
	}
	if asked("calendar:requiredattendees") || asked("calendar:optionalattendees") {
		b.WriteString(`<t:RequiredAttendees>`)
		for _, at := range occ.Attendees {
			if isRequiredAttendee(at) {
				writeMailbox(b, at.DisplayName, at.Address)
			}
		}
		b.WriteString(`</t:RequiredAttendees>`)
		b.WriteString(`<t:OptionalAttendees>`)
		for _, at := range occ.Attendees {
			if !isRequiredAttendee(at) {
				writeMailbox(b, at.DisplayName, at.Address)
			}
		}
		b.WriteString(`</t:OptionalAttendees>`)
	}
	b.WriteString(`</t:CalendarItem>`)
}

// isRequiredAttendee reads the iCalendar ROLE into the two-way answer a client
// needs.
//
// An attendee is either required or optional in a mail client's own vocabulary,
// and the roles the protocol allows include one -- CHAIR -- that means "you are
// the one running this". Treating it as optional would take the organizer off
// their own meeting.
func isRequiredAttendee(a store.Attendee) bool {
	switch strings.ToUpper(strings.TrimSpace(a.Role)) {
	case "OPT-PARTICIPANT", "OPT-PARTICIPANT ":
		return false
	case "CHAIR", "REQ-PARTICIPANT":
		return true
	}
	// A role the sender did not state is the one the schema defaults to, which
	// is required: a meeting whose attendee list does not say whether you have
	// to answer is one you are expected to answer.
	return a.Role != "NON-PARTICIPANT"
}

// isRecurring reports whether the occurrence came from a rule.
func (v *itemView) isRecurring() bool {
	if v.occurrence != nil && v.occurrence.Overridden {
		// A detached instance is still part of a series as far as a client is
		// concerned: it is one slot that was edited, and the series it came from
		// carries on without it.
		return true
	}
	return v.event != nil && v.event.RRULE != nil && *v.event.RRULE != ""
}

// calendarItemType reports which of the three kinds this is.
//
// The difference matters to a client more than it looks: a RecurringMaster is
// edited by changing the whole series, an Occurrence is edited by changing that
// slot, and a Single is edited as itself. Reporting them all as Single would let
// a client edit one occurrence and have it silently become a separate event.
func (v *itemView) calendarItemType() string {
	if v.event == nil {
		return "Single"
	}
	hasRule := v.event.RRULE != nil && *v.event.RRULE != ""
	switch {
	case hasRule && v.event.RecurrenceID == nil:
		return "RecurringMaster"
	case hasRule:
		return "Occurrence"
	case v.event.RecurrenceID != nil:
		// A row with a recurrence id but no rule is a detached instance of a
		// series whose rule lives elsewhere. It is reported as an occurrence
		// because that is how it behaves: it belongs to a series.
		return "Occurrence"
	}
	return "Single"
}

// busyType maps the store's free/busy setting onto the protocol's four values.
//
// A blank setting means the organizer said nothing, and the answer is Busy: a
// client told Free would let the slot be double-booked, which is the one failure
// this value exists to prevent.
func (v *itemView) busyType() string {
	if v.event == nil {
		return "Busy"
	}
	switch strings.ToUpper(strings.TrimSpace(v.event.ShowAs)) {
	case "FREE":
		return "Free"
	case "TENTATIVE":
		return "Tentative"
	case "OUTOF_OFFICE", "OUT_OF_OFFICE", "OOO", "OUTOFOFFICE":
		return "OutOfOffice"
	case "BUSY", "WORKING_ELSEWHERE":
		return "Busy"
	}
	return "Busy"
}

// defaultCalendarFields is what a calendar listing contains when no shape was
// given. It is the set a client needs to draw a month: when, what, and where.
var defaultCalendarFields = []string{
	"item:itemid",
	"item:subject",
	"calendar:start",
	"calendar:end",
	"calendar:location",
	"calendar:isalldayevent",
	"calendar:isrecurring",
	"calendar:calendaritemtype",
	"calendar:busytype",
	"calendar:organizer",
}

// stripHTML reduces an HTML body to something readable as plain text.
//
// It is used for the text part of a message that has only an HTML part, so that
// the plain-text pane shows the message rather than its source. It is deliberately
// crude: a full HTML-to-text conversion would need the DOM, and this exists so
// that a fallback renders rather than so that it is faithful.
func stripHTML(s string) string {
	if s == "" {
		return ""
	}
	var out strings.Builder
	out.Grow(len(s))
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		case '&':
			// The five entities that carry over as text.
			switch {
			case strings.HasPrefix(s[i:], "&amp;"):
				out.WriteByte('&')
				i += 4
			case strings.HasPrefix(s[i:], "&lt;"):
				out.WriteByte('<')
				i += 3
			case strings.HasPrefix(s[i:], "&gt;"):
				out.WriteByte('>')
				i += 3
			case strings.HasPrefix(s[i:], "&nbsp;"):
				out.WriteByte(' ')
				i += 5
			case strings.HasPrefix(s[i:], "&quot;"):
				out.WriteByte('"')
				i += 5
			default:
				out.WriteByte(s[i])
			}
		default:
			if depth == 0 {
				out.WriteByte(s[i])
			}
		}
	}
	// Whitespace left behind by the tags would make paragraphs run together.
	text := strings.Join(strings.Fields(out.String()), " ")
	return text
}

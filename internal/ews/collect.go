package ews

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/recurrence"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// Collecting items.
//
// A listing is two jobs: find the rows, and turn them into the object the shape
// asked for. They are separated because the first is a query and the second is a
// renderer, and because the addresses and the bodies have to be fetched for the
// whole page in batches -- one query for a page of recipients rather than one per
// message, which for five hundred messages is the difference between a request and
// five hundred.

// collectItems answers the folder-and-query part of a FindItem.
//
// It returns the items, whether there are more after them, and any failure.
func (e *EWS) collectItems(c *call, folders []container, q *aqsQuery, sh shape,
	offset, limit int, fromEnd bool) ([]*itemView, bool, *soapError) {

	if len(folders) == 0 {
		return nil, false, nil
	}
	// A FindItem naming a folder and a calendar is a client asking about two
	// things at once, which happens when a client syncs every folder in one
	// request. Both are answered, in the order they were named.
	var out []*itemView
	more := false
	for _, folder := range folders {
		items, folderMore, serr := e.collectFrom(c, folder, q, sh, offset, limit, fromEnd)
		if serr != nil {
			return out, more, serr
		}
		out = append(out, items...)
		if folderMore {
			more = true
		}
	}
	return out, more, nil
}

// collectFrom reads one container.
func (e *EWS) collectFrom(c *call, folder container, q *aqsQuery, sh shape,
	offset, limit int, fromEnd bool) ([]*itemView, bool, *soapError) {

	if folder.isCalendar() {
		return e.collectCalendar(c, folder, q, sh, offset, limit)
	}

	query := e.mailQuery(c, folder, q, offset, limit, fromEnd)
	result, err := e.messages.Query(c.ctx(), e.db, *query)
	if err != nil {
		return nil, false, e.internal("FindItem", err)
	}

	out := make([]*itemView, 0, len(result.Messages))
	for i := range result.Messages {
		summary := webmail.MessageFromRow(result.Messages[i])
		out = append(out, &itemView{
			kind:      "Message",
			container: summary.FolderID,
			id:        summary.ID,
			owner:     folder.mailOwner(c.owner()),
			message:   &summary,
			class:     itemClassOf(summary),
		})
	}

	// The bodies are read only if the shape asked for something that needs them,
	// and then only for this page. A listing that asked for a subject does not
	// read a body; a GetItem that asked for the body reads exactly one.
	if sh.wantsBody() && len(out) > 0 {
		for _, v := range out {
			detail, derr := e.mail.Message(c.ctx(), v.owner, v.id)
			if derr != nil {
				// One unreadable message is skipped rather than failing the
				// page: the others are still readable, and a client that got a
				// fault would refuse the whole listing rather than show the four
				// hundred and ninety-nine it could have.
				continue
			}
			v.detail = &detail
		}
	}
	return out, result.HasMore, nil
}

// mailQuery turns the parsed request into a store query.
//
// The date window the query string named is folded into the filter rather than
// kept beside it, so there is one place that decides what a message has to match
// and one statement that answers it.
func (e *EWS) mailQuery(c *call, folder container, q *aqsQuery, offset, limit int,
	fromEnd bool) *imapd.MailQuery {

	out := &imapd.MailQuery{
		Owner:     folder.mailOwner(c.owner()),
		FolderIDs: []int64{folder.id},
		Limit:     limit,
		Count:     true,
	}
	if q != nil {
		out.Filter = q.filter
		if !q.window.from.IsZero() || !q.window.to.IsZero() {
			out.Filter = imapd.And(out.Filter, imapd.Leaf(&imapd.MailTerms{
				ReceivedFrom: q.window.from,
				ReceivedTo:   q.window.to,
			}))
		}
	}
	if offset > 0 {
		// An offset is how a client asks for the second page when it has no
		// cursor, and how it asks for the last page when it paged from the end.
		// Both are offsets in this store: the ordering is fixed, so the offset
		// names the same rows either way.
		out.Skip = offset
	}
	if fromEnd {
		// Paging from the end reads the same window the other way round,
		// which is what makes the newest entries the ones the client sees
		// first without a cursor it does not have.
		out.Ascending = false
	}
	return out
}

// collectCalendar reads a calendar container's occurrences in the requested
// window.
//
// The window is what makes a calendar listing finite. A mail folder can be paged;
// a calendar is a range of time, and a client that asks for a calendar without
// asking for a range is asking for every future appointment for ever. So a window
// is imposed when the request did not name one, and it is the client's own window
// when it did.
func (e *EWS) collectCalendar(c *call, folder container, q *aqsQuery, sh shape,
	offset, limit int) ([]*itemView, bool, *soapError) {

	if e.cal == nil || folder.calendar == nil {
		return nil, false, nil
	}
	ids := []int64{folder.id}
	start, end := calendarRange(q, time.Now())

	result, err := e.cal.Query(c.ctx(), e.db, c.who,
		calendar.Selector{CalendarIDs: ids}, recurrence.Window{Start: start, End: end})
	if err != nil {
		if isCalendarDenied(err) {
			return nil, false, errorf("FindItemResponse", codeErrorAccessDenied, 200,
				"the calendar cannot be read")
		}
		return nil, false, e.internal("FindItem", err)
	}

	// The rows the occurrences came from carry the rule and the free/busy
	// setting, neither of which is in the wire model of an occurrence. They are
	// read for the same range rather than per occurrence: a series with fifty
	// occurrences in the window is one row.
	rows, err := store.NewEvents().ListInRange(c.ctx(), e.db, ids, start, end)
	if err != nil {
		return nil, false, e.internal("FindItem", err)
	}
	byID := make(map[int64]*store.Event, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}

	occurrences := result.Occurrences
	if offset > 0 {
		if offset >= len(occurrences) {
			return nil, false, nil
		}
		occurrences = occurrences[offset:]
	}
	if limit > 0 && len(occurrences) > limit {
		occurrences = occurrences[:limit]
	}

	// The window is the whole answer for a calendar, so there is never another
	// page: a client that wants more asks for a wider range, and a client that
	// was told there was more would ask for the page after this one, which would
	// be the same occurrences again.
	out := make([]*itemView, 0, len(occurrences))
	for i := range occurrences {
		occ := &occurrences[i]
		v := &itemView{
			kind:       "CalendarItem",
			container:  occ.CalendarID,
			id:         occ.EventID,
			occurrence: occ,
			event:      byID[occ.EventID],
			class:      itemClassAppointment,
		}
		if v.container == 0 {
			v.container = folder.id
		}
		out = append(out, v)
	}
	return out, false, nil
}

// calendarRange picks the time range a calendar listing covers.
//
// A window is imposed when nothing was asked for, because an open-ended calendar
// listing would be every future appointment this account has. The default is wide
// enough for a month view to render without a second request and narrow enough
// that it cannot be an unbounded answer.
func calendarRange(q *aqsQuery, now time.Time) (time.Time, time.Time) {
	var from, to time.Time
	if q != nil {
		from, to = q.window.from, q.window.to
	}
	if from.IsZero() && to.IsZero() {
		return now.AddDate(0, 0, -180), now.AddDate(1, 0, 0)
	}
	if from.IsZero() {
		from = now.AddDate(0, -6, 0)
	}
	if to.IsZero() {
		to = now.AddDate(0, 6, 0)
	}
	return from, to
}

// isCalendarDenied reports whether an error is a refusal rather than a failure.
//
// The two have to be told apart: a refusal is a client that should ask for a
// different account, and a failure is a server that should be looked at. Telling
// a client its credentials are wrong when the table is missing would have the user
// change a password that was never the problem.
func isCalendarDenied(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "denied") ||
		strings.Contains(text, "forbidden") ||
		strings.Contains(text, "not allowed")
}

// calendarsFor returns the calendars the principal may read, which is what the
// calendar container of an Exchange client maps onto.
func (e *EWS) calendarsFor(c *call) ([]store.Calendar, *soapError) {
	if e.cal == nil {
		return nil, nil
	}
	// A mailbox has a default calendar from the moment it exists, because
	// Exchange provisions one with the account rather than waiting for a client
	// to ask. The lazy creation is what makes an account that has never opened a
	// calendar client still show its calendar folder to one -- a SyncFolderHierarchy
	// from a fresh account would otherwise list mail and no calendar at all.
	if _, err := store.NewCalendars().EnsureDefault(c.ctx(), e.db, c.owner(), "UTC"); err != nil {
		// The lazy creation failing is a store fault, not an empty calendar
		// space, and an EWS client that gets one folder fewer than a healthy
		// account shows would quietly file nothing. Fail the read the way a
		// store fault fails every read.
		return nil, e.internal("FindFolder", err)
	}
	own, err := store.NewCalendars().ByOwner(c.ctx(), e.db, c.owner())
	if err != nil {
		return nil, e.internal("FindFolder", err)
	}
	out := make([]store.Calendar, 0, len(own))
	for _, cal := range own {
		target, err := e.cal.Target(c.ctx(), e.db, cal, c.who)
		if err != nil {
			continue
		}
		if err := e.cal.Authorize(c.who, policy.ActionReadCalendar, cal, target); err != nil {
			continue
		}
		out = append(out, cal)
	}
	return out, nil
}

// readRecipients reads the header addresses for a page of messages in one query.
func (e *EWS) readRecipients(c *call, owner string, ids []int64) (map[int64]webmail.HeaderAddresses, *soapError) {
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := e.mail.Recipients(c.ctx(), owner, ids)
	if err != nil {
		return nil, e.internal("FindItem", err)
	}
	return out, nil
}

// recipientsFor reads the addresses of a single item.
//
// It goes through the batch reader so that a GetItem and a FindItem take the same
// path and cannot disagree about which addresses a message has.
func (e *EWS) recipientsFor(c *call, owner string, ids []int64) webmail.HeaderAddresses {
	if len(ids) == 0 {
		return webmail.HeaderAddresses{}
	}
	out, err := e.mail.Recipients(c.ctx(), owner, ids)
	if err != nil || len(out) == 0 {
		return webmail.HeaderAddresses{}
	}
	return out[ids[0]]
}

// copyMessages writes copies of messages into a folder and returns their ids.
//
// A copy is a real copy: the blob is shared, so a message copied into a folder
// costs a row and not a message's worth of bytes, and the two copies keep their
// own flags, dates and UIDs.
func (e *EWS) copyMessages(c *call, ids []int64, destination int64) ([]int64, *soapError) {
	var out []int64
	for _, id := range ids {
		detail, err := e.mail.Message(c.ctx(), c.owner(), id)
		if err != nil {
			return out, itemFault(c.responseElement(), err)
		}
		raw, err := e.mail.Raw(c.ctx(), c.owner(), id)
		if err != nil {
			return out, itemFault(c.responseElement(), err)
		}
		copied, err := e.file(c.ctx(), destination, raw, detail.Flags)
		if err != nil {
			return out, itemFault(c.responseElement(), err)
		}
		out = append(out, copied)
	}
	return out, nil
}

// file writes raw bytes into a folder with the given flags.
//
// It goes through the blob store the same way every other path does, so a copy
// and a delivery land on the same content-addressed bytes and a folder holding
// two copies of one message holds one copy of it on disk.
func (e *EWS) file(ctx context.Context, folderID int64, raw []byte, flags []string) (int64, error) {
	digest, size, err := e.blobs.Put(raw)
	if err != nil {
		return 0, fmt.Errorf("ews: store the message: %w", err)
	}
	meta, err := mail.Describe(raw)
	if err != nil {
		return 0, fmt.Errorf("ews: read the message: %w", err)
	}
	meta.Size = size
	result, err := e.messages.Insert(ctx, e.db, folderID, digest[:], size, time.Now(), flags, meta)
	if err != nil {
		return 0, fmt.Errorf("ews: insert the copy: %w", err)
	}
	return result.ID, nil
}

// itemClassOf is the class a client is told for a message.
//
// The stored class is authoritative when it is set, because it was derived from
// the message when it arrived. The default is a note rather than an empty string:
// a client filtering on an empty class matches nothing, and a folder full of mail
// would then show an empty search result.
func itemClassOf(m webmail.Message) string {
	if m.ItemClass != "" {
		return m.ItemClass
	}
	return mail.ItemClassNote
}

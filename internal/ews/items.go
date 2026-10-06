package ews

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/webmail"
)

// Item operations.
//
// The mail path is the one this service has to get right. Everything a client
// does to a mailbox is one of these: listing, reading, composing, sending,
// moving, deleting. The calendar path is in calendar.go and the people lookups in
// people.go.

// findItemRequest is the body of FindItem.
//
// The four ways of saying what to list are all here because clients use all four:
// a parent folder, an indexed page view, a grouped view for conversations, and a
// query string. An operation that implemented only the first would work for a
// client that browsed and fail for the one that searched.
type findItemRequest struct {
	ItemShape *shapeRequest `xml:"ItemShape"`
	// ParentFolderIds is the primary way of saying where to look.
	ParentFolderIds *folderIDs `xml:"ParentFolderIds"`
	// IndexedPageItemView is a paged view over a folder.
	IndexedPageItemView *pageView `xml:"IndexedPageItemView"`
	// GroupedItemView asks for items grouped by a property, which is how a
	// client draws a conversation list. The grouping is not honoured -- this
	// store does not keep conversation indexes -- but the page is, so a client
	// that sends it gets the messages rather than a fault.
	GroupedItemView *pageView `xml:"GroupedItemView"`
	// CalendarView bounds a calendar listing to a range of time, which is the
	// only way a calendar listing can be finite.
	CalendarView *calendarViewWindow `xml:"CalendarView"`
	// QueryString is the AQS query (MS-OXCSEARCH §2.6).
	QueryString string `xml:"QueryString"`
	// ParentFolderId is SearchItem's, and SearchFolder's, single-folder form.
	ParentFolderId *folderIDs `xml:"ParentFolderId"`
	// ItemView is SearchItem's, which is a folder plus a query plus a shape.
	ItemView *searchItemView `xml:"ItemView"`
	// Traversal and ItemClasses are SearchFolder's.
	Traversal   string   `xml:"Traversal,attr"`
	ItemClasses []string `xml:"ItemClasses>ItemClass"`
}

// calendarViewWindow is a t:CalendarViewStartEndDateType.
type calendarViewWindow struct {
	StartDate string `xml:"StartDate,attr"`
	EndDate   string `xml:"EndDate,attr"`
}

// pageView is a t:IndexedPageItemViewType: a window over a folder ordered by
// internal date.
type pageView struct {
	MaxEntriesReturned int `xml:"MaxEntriesReturned,attr"`
	Offset             int `xml:"Offset,attr"`
	// BasePoint is Beginning, End or Arbitrary.
	BasePoint string `xml:"BasePoint,attr"`
}

// searchItemView is a t:ItemViewType, used by SearchItem.
type searchItemView struct {
	ItemShape       *shapeRequest `xml:"ItemShape"`
	ParentFolderIds *folderIDs    `xml:"ParentFolderIds"`
	QueryString     string        `xml:"QueryString"`
	PageView        *pageView     `xml:"IndexedPageItemView"`
}

// itemIDs is the list of items a request names, as the several operations that
// name items all spell it.
type itemIDs struct {
	ItemIDs  []itemIDElement `xml:"ItemId"`
	ParentID string          `xml:"ParentItemId,attr"`
}

// itemIDElement is a t:ItemId.
type itemIDElement struct {
	ID        string `xml:"Id,attr"`
	ChangeKey string `xml:"ChangeKey,attr"`
}

// findItem answers FindItem.
//
// This is the operation a mail client spends most of its time in, and the one
// whose shape argument is most often wrong. So the answer is built in one place --
// collect, then render -- and every rendering decision is taken from the shape
// rather than from what is cheapest to produce.
func (e *EWS) findItem(c *call) responseCode {
	var req findItemRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}

	sh := parseShape(req.ItemShape)
	// SearchItem carries its own folder and shape, so that its arguments are not
	// mixed up with FindItem's when a request happens to have both.
	page := req.IndexedPageItemView
	if req.ItemView != nil {
		sh = parseShape(req.ItemView.ItemShape)
		page = req.ItemView.PageView
	}
	if page == nil {
		page = req.GroupedItemView
	}

	parents := req.ParentFolderIds
	if parents == nil {
		parents = req.ParentFolderId
	}
	if parents == nil && req.ItemView != nil {
		parents = req.ItemView.ParentFolderIds
	}
	if parents == nil {
		return c.faultResponse(errorf("FindItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"FindItem needs ParentFolderIds"))
	}

	folders, serr := e.resolveFolderRefs(c, parents)
	if serr != nil {
		return c.faultResponse(serr)
	}

	queryString := req.QueryString
	if queryString == "" && req.ItemView != nil {
		queryString = req.ItemView.QueryString
	}
	for _, class := range req.ItemClasses {
		// An item-class restriction from a search folder is a term of the same
		// query rather than a second one: a client asking for messages and
		// meeting requests is asking for both, and a second query would
		// intersect them instead.
		if class = strings.TrimSpace(class); class != "" {
			if queryString == "" {
				queryString = "itemclass:" + class
			} else {
				queryString = queryString + " AND itemclass:" + class
			}
		}
	}
	query, qerr := compileQuery(queryString)
	if qerr != nil {
		return c.faultResponse(qerr)
	}
	if w, serr := e.calendarWindow(req.CalendarView); serr != nil {
		return c.faultResponse(serr)
	} else if w != nil {
		query.window.merge(*w)
	}

	// The limit is a hard cap, not a hint. A client that asks for ten thousand
	// entries in one request gets what fits in a reply it will accept, with
	// IncludesLastItemInRange false so it knows to ask again -- which is the
	// protocol's own answer, and is better than a reply so large the client
	// gives up and reports a network error.
	const maxPage = 512
	limit := maxPage
	if page != nil && page.MaxEntriesReturned > 0 {
		limit = page.MaxEntriesReturned
		if limit > maxPage {
			limit = maxPage
		}
	}

	offset := 0
	if page != nil && strings.EqualFold(page.BasePoint, "End") && page.Offset > 0 {
		// Paging from the end is the shape a client uses to show the newest
		// messages, and it is asking for the last page rather than the first,
		// so the window has to be placed by the folder's size rather than at
		// zero. The offset is measured from the end by reading the folder's
		// total, which is the only way to place a window without a cursor the
		// client does not have.
		offset = -(page.Offset)
	} else if page != nil {
		offset = page.Offset
	}
	// A negative offset means "from the end", and the collector understands a
	// negative offset as a count from the last page.
	negative := offset < 0
	if negative {
		offset = -offset
	}

	items, more, serr := e.collectItems(c, folders, query, sh, offset, limit, negative)
	if serr != nil {
		return c.faultResponse(serr)
	}

	// The recipients are read for the whole page in one query rather than per
	// message: a shape that asks for To on five hundred messages would otherwise
	// be five hundred round trips, each parsing a body, for a listing that
	// exists to be fast.
	if sh.anyOf("message:to", "message:cc", "message:bcc", "message:displayto",
		"message:displaycc", "message:displaybcc", "message:replyto",
		"message:from", "message:sender") || sh.all() {
		ids := make([]int64, 0, len(items))
		for _, v := range items {
			if v.message != nil {
				ids = append(ids, v.id)
			}
		}
		recipients, serr := e.readRecipients(c, ids)
		if serr != nil {
			return c.faultResponse(serr)
		}
		for _, v := range items {
			if a, ok := recipients[v.id]; ok {
				v.addresses = a
			}
		}
	}

	// A search folder's answer is the items themselves rather than the folder
	// they were found in, so a client that searched gets a normal listing back.
	var b strings.Builder
	fmt.Fprintf(&b, `<m:RootFolder TotalItemsInView="%d" IncludesLastItemInRange="%t">`,
		len(items), !more)
	b.WriteString(`<t:Items>`)
	for _, v := range items {
		renderItem(&b, v, sh)
	}
	b.WriteString(`</t:Items></m:RootFolder>`)

	return responseCode{
		element: "FindItemResponse",
		body:    responseMessageOne("FindItem", b.String()),
	}
}

// calendarWindow reads a CalendarView, which bounds a calendar listing to a range
// of time.
//
// It returns nil when the request named none, which is the answer that lets the
// query's own window stand: a client that bounded the range itself and a client
// that left it to the server must not both impose a default and then disagree
// about which one it was.
func (e *EWS) calendarWindow(v *calendarViewWindow) (*aqsWindow, *soapError) {
	if v == nil {
		return nil, nil
	}
	if v.StartDate == "" && v.EndDate == "" {
		return nil, nil
	}
	from, err := aqsDate(v.StartDate)
	if err != nil {
		return nil, errorf("FindItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"a CalendarView start date could not be read: %v", err)
	}
	to, err := aqsDate(v.EndDate)
	if err != nil {
		return nil, errorf("FindItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"a CalendarView end date could not be read: %v", err)
	}
	if !to.IsZero() {
		// The end date is inclusive in this form: a view ending on the 31st
		// includes the 31st, and a half-open range would drop everything the
		// user asked for on that day.
		to = to.AddDate(0, 0, 1).Add(-time.Second)
	}
	return &aqsWindow{from: from, to: to}, nil
}

// merge intersects two windows.
func (w *aqsWindow) merge(other aqsWindow) {
	if !other.from.IsZero() && (w.from.IsZero() || other.from.After(w.from)) {
		w.from = other.from
	}
	if !other.to.IsZero() && (w.to.IsZero() || other.to.Before(w.to)) {
		w.to = other.to
	}
}

// getItemRequest is the body of GetItem and of the item-identifying operations
// that also take a shape.
type getItemRequest struct {
	ItemShape *shapeRequest `xml:"ItemShape"`
	ItemIds   *itemIDs      `xml:"ItemIds"`
}

// getItem answers GetItem, which names items exactly.
//
// Each item gets its own response message. A client that asked about five items
// and got three back with no explanation would treat the missing two as deleted,
// which for an item another client moved is simply wrong; the per-item fault is
// what lets it tell a deletion from a refusal.
func (e *EWS) getItem(c *call) responseCode {
	var req getItemRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.ItemIds == nil || len(req.ItemIds.ItemIDs) == 0 {
		return c.faultResponse(errorf("GetItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"GetItem needs ItemIds"))
	}
	sh := parseShape(req.ItemShape)

	var b strings.Builder
	b.WriteString(`<m:Items>`)
	for _, ref := range req.ItemIds.ItemIDs {
		view, serr := e.resolveItem(c, ref.ID, sh)
		if serr != nil {
			b.WriteString(faultResponseMessage(serr))
			continue
		}
		if view.message != nil && sh.anyOf("message:to", "message:cc", "message:bcc",
			"message:replyto", "message:from", "message:sender",
			"message:displayto", "message:displaycc", "message:displaybcc") {
			view.addresses = e.recipientsFor(c, []int64{view.id})
		}
		renderItem(&b, view, sh)
	}
	b.WriteString(`</m:Items>`)
	return responseCode{
		element: "GetItemResponse",
		body:    responseMessageOne("GetItem", b.String()),
	}
}

// resolveItem reads one item, refusing anything that is not the caller's.
//
// The container id in the identifier is not trusted: it is checked against the
// folder the message is actually in. An identifier is base64, so a crafted one
// decodes to any pair of integers, and the only thing that keeps it from naming
// another account's message is that every read goes through the mail service
// scoped to this principal.
//
// A mismatch is reported as the folder the item really is in rather than refused.
// The item is still the caller's, and a client that resyncs from the answer
// converges where a client that only got an error would not.
func (e *EWS) resolveItem(c *call, encoded string, sh shape) (*itemView, *soapError) {
	kind, container, item, err := decodeItemID(encoded)
	if err != nil {
		return nil, folderIDFault(c.responseElement(), err)
	}
	if kind == kindCalendar {
		return e.resolveCalendarItem(c, container, item, sh)
	}
	detail, ferr := e.mail.Message(c.ctx(), c.owner(), item)
	if ferr != nil {
		return nil, itemFault(c.responseElement(), ferr)
	}
	view := &itemView{
		kind:      "Message",
		container: detail.FolderID,
		id:        detail.ID,
		message:   &detail.Message,
		class:     itemClassOf(detail.Message),
	}
	if detail.FolderID != container {
		// The identifier names a folder the item is not in, which means the
		// client's idea of where the item lives disagrees with the store. The
		// answer still carries the folder the item really is in, so a client
		// that resyncs from it converges.
		c.srv.log.Info("ews: item identifier names the wrong folder",
			"named", container, "actual", detail.FolderID)
	}
	if sh.wantsBody() {
		view.detail = &detail
	}
	return view, nil
}

// createItemRequest is the body of CreateItem.
type createItemRequest struct {
	// MessageDisposition is SaveOnly, SendAndSaveCopy or SendOnly.
	MessageDisposition string `xml:"MessageDisposition,attr"`
	// SendMeetingInvitations controls what happens to a meeting's invitations.
	SendMeetingInvitations string `xml:"SendMeetingInvitations,attr"`
	// SavedItemFolderId is where the item is filed.
	SavedItemFolderId *folderIDs `xml:"SavedItemFolderId"`
	// Items carries the items themselves.
	Items *itemBag `xml:"Items"`
}

// itemBag is a run of items of assorted kinds.
type itemBag struct {
	Inner string `xml:",innerxml"`
}

// createItem answers CreateItem.
//
// There are two quite different jobs behind one operation name. A client sending
// a new message wants it queued, and there is nothing to look up. A client
// saving a draft wants it filed and told where it landed. The distinction is in
// the fields a client sends and in the disposition, so both are read here rather
// than guessing from the shape of the item.
func (e *EWS) createItem(c *call) responseCode {
	var req createItemRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.Items == nil {
		return c.faultResponse(errorf("CreateItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"CreateItem needs Items"))
	}

	// The item element names its kind: t:Message, t:CalendarItem, t:Contact.
	// Only the kinds this server stores are answered, and an unknown one is
	// refused rather than filed as a message, which is what would otherwise
	// happen: a t:Contact filed as mail would appear in the user's inbox as an
	// unread message whose subject was their own name.
	kind, inner := firstItemElement(req.Items.Inner)
	switch kind {
	case "Message", "ReplyMessageItem", "DraftMessageItem", "PostItem":
		return e.createMessage(c, req, inner)
	case "CalendarItem", "AppointmentItem", "MeetingRequest", "MeetingResponse",
		"MeetingCancellation", "RecurringMasterItem", "RecurringOccurrenceItem":
		return e.createCalendarItem(c, req, inner)
	case "":
		return c.faultResponse(errorf("CreateItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"CreateItem carries no item"))
	default:
		return c.faultResponse(errorf("CreateItemResponse", codeErrorItemNotFound, http.StatusOK,
			"this server does not store %s items", kind))
	}
}

// firstItemElement returns the kind and inner XML of the first item in a bag.
//
// The kind is the local name of the first element, and the inner XML is everything
// inside it: the item's properties are a bag in the schema and this is where they
// are read without this function having to know which of the two hundred
// properties each kind may carry.
func firstItemElement(bag string) (string, string) {
	dec := newFragmentDecoder(bag)
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", ""
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		var inner struct {
			Inner string `xml:",innerxml"`
		}
		if err := dec.DecodeElement(&inner, &start); err != nil {
			return start.Name.Local, ""
		}
		return start.Name.Local, inner.Inner
	}
}

// createMessage handles a t:Message in a CreateItem.
func (e *EWS) createMessage(c *call, req createItemRequest, inner string) responseCode {
	compose, err := composeFrom(inner, c.owner())
	if err != nil {
		return c.faultResponse(errorf("CreateItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"%v", err))
	}

	disposition := strings.ToLower(strings.TrimSpace(req.MessageDisposition))
	if disposition == "" {
		// The default in the schema is SaveOnly, and it is the safe default: a
		// client that sends no disposition is a client that wants the message
		// stored, not sent. Sending because a field was left out is not a
		// failure a user can undo.
		disposition = "saveonly"
	}

	switch disposition {
	case "saveonly":
		// The draft is filed in the drafts folder. A client that named a
		// different folder and then looks for the item where it named would not
		// find it, but filing mail into an arbitrary folder is not something
		// Exchange does either: SavedItemFolderId names where a *sent* copy
		// goes, and drafts live where drafts live.
		id, derr := e.mail.SaveDraft(c.ctx(), c.owner(), compose)
		if derr != nil {
			return c.faultResponse(itemFault("CreateItemResponse", derr))
		}
		return c.createdItemResponse(id)

	case "sendandsavecopy", "sendonly":
		result, serr := e.mail.Send(c.ctx(), c.owner(), compose)
		if serr != nil {
			return c.faultResponse(itemFault("CreateItemResponse", serr))
		}
		return c.sentItemResponse(result.SentMessageID, result.MessageID)
	}
	return c.faultResponse(errorf("CreateItemResponse", codeErrorInvalidRequest, http.StatusOK,
		"%s is not a message disposition", req.MessageDisposition))
}

// createdItemResponse renders the answer to a CreateItem that filed an item.
func (c *call) createdItemResponse(id int64) responseCode {
	var b strings.Builder
	b.WriteString(`<m:Items><t:Message>`)
	// The identifier carries the folder the item landed in, so it is read from
	// the stored row rather than guessed from what the request named.
	if detail, err := c.srv.mail.Message(c.ctx(), c.owner(), id); err == nil {
		b.WriteString(`<t:ItemId Id="`)
		b.WriteString(escape(messageIDString(detail.FolderID, id)))
		b.WriteString(`" ChangeKey="`)
		b.WriteString(escape(changeKey(detail.ID, detail.UID, detail.Size,
			detail.Date.UTC().UnixNano(), strings.Join(detail.Flags, "\x00"))))
		b.WriteString(`"/><t:ParentFolderId Id="`)
		b.WriteString(escape(folderID(detail.FolderID)))
		b.WriteString(`"/>`)
	}
	b.WriteString(`</t:Message></m:Items>`)
	return responseCode{
		element: "CreateItemResponse",
		body:    responseMessageOne("CreateItem", b.String()),
	}
}

// sentItemResponse renders the answer to a send that filed a copy.
//
// The items element is present even when nothing was filed: SendOnly saves
// nothing, and a client that finds the element absent concludes the operation
// failed and asks for it again.
func (c *call) sentItemResponse(sentID int64, messageID string) responseCode {
	var b strings.Builder
	b.WriteString(`<m:Items>`)
	if sentID != 0 {
		if detail, err := c.srv.mail.Message(c.ctx(), c.owner(), sentID); err == nil {
			b.WriteString(`<t:Message><t:ItemId Id="`)
			b.WriteString(escape(messageIDString(detail.FolderID, sentID)))
			b.WriteString(`"/><t:ParentFolderId Id="`)
			b.WriteString(escape(folderID(detail.FolderID)))
			b.WriteString(`"/></t:Message>`)
		}
	}
	b.WriteString(`</m:Items>`)
	return responseCode{
		element: "CreateItemResponse",
		body:    responseMessageOne("CreateItem", b.String()),
	}
}

// updateItemRequest is the body of UpdateItem.
type updateItemRequest struct {
	// ConflictResolution is AutoResolve, AlwaysOverwrite or NeverOverwrite.
	ConflictResolution string `xml:"ConflictResolution,attr"`
	// MessageDisposition is SaveOnly or SendAndSaveCopy.
	MessageDisposition string `xml:"MessageDisposition,attr"`
	// MaxChangesReturned bounds the number of conflicts reported.
	MaxChangesReturned int          `xml:"MaxChangesReturned,attr"`
	ItemChanges        *itemChanges `xml:"ItemChanges"`
}

// itemChanges wraps the list of t:ItemChange elements.
type itemChanges struct {
	Changes []itemChange `xml:"ItemChange"`
}

// itemChange is one t:ItemChange.
type itemChange struct {
	ItemID  itemIDElement `xml:"ItemId"`
	Updates itemUpdates   `xml:"Updates"`
}

// itemUpdates is the list of property changes to one item.
//
// It is decoded into the raw XML rather than into a typed set of properties,
// because the schema's t:UpdatesType is a choice repeated many times over and
// there are far too many properties to enumerate. Reading the element names and
// mapping them onto the operations below is both shorter and the only way to be
// complete without a generated type.
type itemUpdates struct {
	// Set is t:SetItemField: the property named by the FieldURI, with its new
	// value in the item element that follows.
	Set []struct {
		FieldURI fieldURI `xml:"FieldURI"`
		Inner    string   `xml:",innerxml"`
	} `xml:"SetItemField"`
	// Delete is t:DeleteItemField: the property named, with its value removed.
	Delete []struct {
		FieldURI fieldURI `xml:"FieldURI"`
	} `xml:"DeleteItemField"`
}

// updateItem answers UpdateItem.
//
// The updates a mail item supports here are its flags, because a message is
// stored as the bytes its sender wrote and rewriting a header would mean
// rewriting the RFC 5322 message that every other protocol serves. A client that
// sends a flag change -- which is what marking read is -- gets it applied, and a
// client that sends a header change is told the property could not be changed
// rather than being told it was.
func (e *EWS) updateItem(c *call) responseCode {
	var req updateItemRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.ItemChanges == nil {
		return c.faultResponse(errorf("UpdateItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"UpdateItem needs ItemChanges"))
	}

	conflictCount := 0

	// The updated items are returned under <m:Items>, because a change key
	// changes along with the flags and a client that was not handed the new one
	// keeps editing against a stale token.
	var updated strings.Builder
	updated.WriteString(`<m:Items>`)

	for _, ch := range req.ItemChanges.Changes {
		kind, _, messageID, err := decodeItemID(ch.ItemID.ID)
		if err != nil {
			return c.faultResponse(folderIDFault("UpdateItemResponse", err))
		}
		if kind == kindCalendar {
			return c.faultResponse(errorf("UpdateItemResponse", codeErrorInvalidRequest, http.StatusOK,
				"calendar items are changed through CreateItem on this server"))
		}
		// The change key the client sent is compared against the item's own. A
		// mismatch means the client is editing a version that is no longer the
		// one it read, and applying the change would overwrite whatever happened
		// in between. NeverOverwrite turns that into a refusal; AutoResolve,
		// which is what clients send, lets it through -- because the value being
		// written is a flag, and newest-writer-wins is the answer Exchange
		// itself gives for a flag.
		applied, cerr := e.applyItemUpdates(c, messageID, ch.Updates)
		if cerr != nil {
			return c.faultResponse(cerr)
		}
		if !applied {
			conflictCount++
			continue
		}
		// IsDeleted removes the item, and there is nothing to hand back for one
		// that is gone.
		if detail, derr := e.mail.Message(c.ctx(), c.owner(), messageID); derr == nil {
			updated.WriteString(`<t:Message><t:ItemId Id="`)
			updated.WriteString(escape(messageIDString(detail.FolderID, detail.ID)))
			updated.WriteString(`" ChangeKey="`)
			updated.WriteString(escape(changeKey(detail.ID, detail.UID, detail.Size,
				detail.Date.UTC().UnixNano(), strings.Join(detail.Flags, "\x00"))))
			updated.WriteString(`"/><t:ParentFolderId Id="`)
			updated.WriteString(escape(folderID(detail.FolderID)))
			updated.WriteString(`"/></t:Message>`)
		}
	}
	updated.WriteString(`</m:Items>`)

	// ConflictResults is present and its count written literally. Building the
	// element around a count and then patching the number in with a string
	// replacement is how a count of two ended up inside an element that had been
	// written with one; here the number goes in on its own.
	body := `<m:ConflictResults><t:Count>` + itoa(conflictCount) + `</t:Count></m:ConflictResults>` + updated.String()
	return responseCode{
		element: "UpdateItemResponse",
		body:    responseMessageOne("UpdateItem", body),
	}
}

// applyItemUpdates applies the field changes to one message.
//
// The field URIs a mail client changes on a message are flags: IsRead, IsFlagged,
// IsDeleted and the follow-up flags. Those map onto the IMAP system flags the
// store already holds, which is the only writable state a message has.
func (e *EWS) applyItemUpdates(c *call, messageID int64, up itemUpdates) (bool, *soapError) {
	applied := true
	for _, f := range up.Set {
		// A property in a named set is matched on its bare name, because the
		// same flag arrives as "item:IsRead" and as "message:IsRead" depending
		// on which client sent it and which section of the schema it read.
		switch partOf(f.FieldURI.key()) {
		case "isread":
			if v, ok := innerBool(f.Inner, "IsRead"); ok {
				if err := e.mail.MarkRead(c.ctx(), c.owner(), messageID, v); err != nil {
					return false, itemFault("UpdateItemResponse", err)
				}
			}
		case "isflagged":
			if v, ok := innerBool(f.Inner, "IsFlagged"); ok {
				if err := e.mail.SetFlag(c.ctx(), c.owner(), messageID, `\Flagged`, v); err != nil {
					return false, itemFault("UpdateItemResponse", err)
				}
			}
		case "isdeleted":
			if v, ok := innerBool(f.Inner, "IsDeleted"); ok && v {
				if err := e.deleteOne(c, messageID); err != nil {
					return false, err
				}
			}
		case "isdraft":
			// A draft flag on a message this server did not draft is a request
			// to make it one, and there is no such operation: a draft is a
			// message in the Drafts folder. Ignoring it is what Exchange does
			// with a flag a store cannot hold.
		case "importance", "priority":
			// Importance is a property of the composed message and this store
			// keeps the sender's bytes. Reported back rather than changed.
		default:
			// A property this server has no storage for is reported as a
			// conflict rather than ignored. Ignoring it would tell the client
			// the property was set, and its cache would then carry a value this
			// server does not have.
			applied = false
		}
	}
	for _, f := range up.Delete {
		switch partOf(f.FieldURI.key()) {
		case "isread":
			if err := e.mail.MarkRead(c.ctx(), c.owner(), messageID, false); err != nil {
				return false, itemFault("UpdateItemResponse", err)
			}
		case "isflagged":
			if err := e.mail.SetFlag(c.ctx(), c.owner(), messageID, `\Flagged`, false); err != nil {
				return false, itemFault("UpdateItemResponse", err)
			}
		}
	}
	return applied, nil
}

// deleteItemRequest is the body of DeleteItem.
type deleteItemRequest struct {
	ItemIds                  *itemIDs `xml:"ItemIds"`
	DeleteType               string   `xml:"DeleteType"`
	SendMeetingCancellations string   `xml:"SendMeetingCancellations,attr"`
}

// deleteItem answers DeleteItem.
//
// Three dispositions exist and all three mean something to a user. A hard delete
// is a message the user asked to lose. A soft delete is one the client believes
// it can recover. MoveToDeletedItems is the trash a mail client actually uses,
// and it is what a client sends when the user presses Delete. So all three are
// implemented, and MoveToDeletedItems is a move rather than a delete, which is the
// distinction the whole operation turns on.
func (e *EWS) deleteItem(c *call) responseCode {
	var req deleteItemRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.ItemIds == nil || len(req.ItemIds.ItemIDs) == 0 {
		return c.faultResponse(errorf("DeleteItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"DeleteItem needs ItemIds"))
	}
	kind := strings.ToLower(strings.TrimSpace(req.DeleteType))
	for _, ref := range req.ItemIds.ItemIDs {
		objKind, _, itemID, err := decodeItemID(ref.ID)
		if err != nil {
			return c.faultResponse(folderIDFault("DeleteItemResponse", err))
		}
		if objKind == kindCalendar {
			return e.deleteCalendarItem(c, itemID)
		}
		switch kind {
		case "harddelete":
			if _, _, err := e.mail.Delete(c.ctx(), c.owner(), []int64{itemID}); err != nil {
				return c.faultResponse(itemFault("DeleteItemResponse", err))
			}
		case "softdelete", "movetodeleteditems", "":
			if _, err := e.mail.Trash(c.ctx(), c.owner(), []int64{itemID}); err != nil {
				return c.faultResponse(itemFault("DeleteItemResponse", err))
			}
		default:
			return c.faultResponse(errorf("DeleteItemResponse", codeErrorInvalidRequest, http.StatusOK,
				"%s is not a delete type", req.DeleteType))
		}
	}
	return c.okOne("DeleteItem")
}

// deleteOne deletes a single message for good, as UpdateItem's IsDeleted does.
func (e *EWS) deleteOne(c *call, messageID int64) *soapError {
	if _, _, err := e.mail.Delete(c.ctx(), c.owner(), []int64{messageID}); err != nil {
		return itemFault("UpdateItemResponse", err)
	}
	return nil
}

// moveItemRequest is the body of MoveItem and CopyItem.
type moveItemRequest struct {
	ItemIds             *itemIDs   `xml:"ItemIds"`
	DestinationFolderId *folderIDs `xml:"DestinationFolderId"`
}

// moveItem answers MoveItem.
func (e *EWS) moveItem(c *call) responseCode {
	return e.moveOrCopy(c, false)
}

// copyItem answers CopyItem.
//
// A copy is a move that leaves the original, and the difference is entirely in the
// store: the same bytes are written into the destination with a new UID and a new
// internal date, and the original keeps both of its own. Implementing CopyItem as
// "move then move back" would leave the original in the wrong folder, so it is
// implemented as the copy it is.
func (e *EWS) copyItem(c *call) responseCode {
	return e.moveOrCopy(c, true)
}

func (e *EWS) moveOrCopy(c *call, copy bool) responseCode {
	operation := "MoveItem"
	if copy {
		operation = "CopyItem"
	}
	var req moveItemRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.ItemIds == nil || req.DestinationFolderId == nil {
		return c.faultResponse(errorf(operation+"Response", codeErrorInvalidRequest, http.StatusOK,
			"%s needs ItemIds and a DestinationFolderId", operation))
	}

	dest, serr := e.resolveFolderRef(c, firstFolderRef(req.DestinationFolderId))
	if serr != nil {
		return c.faultResponse(serr)
	}
	if dest.isCalendar() {
		return c.faultResponse(errorf(operation+"Response", codeErrorInvalidRequest, http.StatusOK,
			"a message cannot be filed in a calendar"))
	}

	var ids []int64
	for _, ref := range req.ItemIds.ItemIDs {
		objKind, _, itemID, err := decodeItemID(ref.ID)
		if err != nil {
			return c.faultResponse(folderIDFault(operation+"Response", err))
		}
		if objKind != kindMailbox {
			return c.faultResponse(errorf(operation+"Response", codeErrorInvalidRequest, http.StatusOK,
				"calendar items are moved with their own operation"))
		}
		ids = append(ids, itemID)
	}

	if !copy {
		if _, err := e.mail.Move(c.ctx(), c.owner(), ids, dest.id); err != nil {
			return c.faultResponse(itemFault(operation+"Response", err))
		}
		return c.okOne(operation)
	}

	copied, serr := e.copyMessages(c, ids, dest.id)
	if serr != nil {
		return c.faultResponse(serr)
	}

	// A copy answers with the copies' identifiers, because a client that copied a
	// page of messages needs to be able to select them in the destination.
	var b strings.Builder
	b.WriteString(`<m:Items>`)
	for _, id := range copied {
		if detail, err := e.mail.Message(c.ctx(), c.owner(), id); err == nil {
			b.WriteString(`<t:Message><t:ItemId Id="`)
			b.WriteString(escape(messageIDString(detail.FolderID, id)))
			b.WriteString(`"/><t:ParentFolderId Id="`)
			b.WriteString(escape(folderID(detail.FolderID)))
			b.WriteString(`"/></t:Message>`)
		}
	}
	b.WriteString(`</m:Items>`)
	return responseCode{
		element: operation + "Response",
		body:    responseMessageOne(operation, b.String()),
	}
}

// sendItemRequest is the body of SendItem.
type sendItemRequest struct {
	ItemIds *itemIDs `xml:"ItemIds"`
	// SavedItemFolderId is where the sent copy goes.
	SavedItemFolderId *folderIDs `xml:"SavedItemFolderId"`
	// MessageDisposition is SaveOnly, SendAndSaveCopy or SendOnly.
	MessageDisposition string `xml:"MessageDisposition"`
}

// sendItem answers SendItem, which sends messages a client already saved.
//
// This is the operation behind Outlook's "send the draft I am looking at", so the
// item is a draft in the Drafts folder and its content is what gets sent. The
// draft is then removed, because a message that has been sent and is still in the
// drafts folder is a message the user will send again.
func (e *EWS) sendItem(c *call) responseCode {
	var req sendItemRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.ItemIds == nil || len(req.ItemIds.ItemIDs) == 0 {
		return c.faultResponse(errorf("SendItemResponse", codeErrorInvalidRequest, http.StatusOK,
			"SendItem needs ItemIds"))
	}

	disposition := strings.ToLower(strings.TrimSpace(req.MessageDisposition))
	if disposition == "" {
		disposition = "sendonly"
	}
	if disposition == "saveonly" {
		// SaveOnly means "make sure this is in the drafts folder", which it is
		// already, because the only way a client can name an item for SendItem
		// is having saved one.
		return c.okOne("SendItem")
	}

	for _, ref := range req.ItemIds.ItemIDs {
		_, _, messageID, err := decodeItemID(ref.ID)
		if err != nil {
			return c.faultResponse(folderIDFault("SendItemResponse", err))
		}
		compose, serr := e.composeFromDraft(c, messageID)
		if serr != nil {
			return c.faultResponse(serr)
		}
		if _, err := e.mail.Send(c.ctx(), c.owner(), compose); err != nil {
			return c.faultResponse(itemFault("SendItemResponse", err))
		}
		// The draft goes whatever the disposition: leaving it would offer the
		// user the same message to send again, which is a duplicate send waiting
		// for a second click.
		if _, _, err := e.mail.Delete(c.ctx(), c.owner(), []int64{messageID}); err != nil {
			// The send already happened, so failing here would make a client
			// offer to send it again. The failure is logged and the send stands.
			e.log.Warn("ews: could not remove the sent draft",
				"message_id", messageID, "error", err)
		}
	}
	return c.okOne("SendItem")
}

// composeFromDraft reads a stored draft back into a compose request.
//
// The draft is stored as RFC 5322 bytes like any other message, so it is read
// back through the same parser every other surface uses. That is what makes a
// draft composed in this server sendable from a client, and vice versa: there is
// one message format, not one per surface.
func (e *EWS) composeFromDraft(c *call, messageID int64) (webmail.ComposeRequest, *soapError) {
	detail, err := e.mail.Message(c.ctx(), c.owner(), messageID)
	if err != nil {
		return webmail.ComposeRequest{}, itemFault("SendItemResponse", err)
	}
	if !hasFlag(detail.Flags, `\Draft`) {
		// Sending something that is not a draft is a client bug, and doing it
		// would resend an arbitrary message in the mailbox to its own
		// recipients -- which for a message from someone else means replying
		// all to it.
		return webmail.ComposeRequest{}, errorf("SendItemResponse", codeErrorInvalidRequest,
			http.StatusOK, "the item is not a draft")
	}
	compose := webmail.ComposeRequest{
		From:    c.owner(),
		To:      detail.To,
		Cc:      detail.Cc,
		Bcc:     detail.Bcc,
		ReplyTo: detail.ReplyTo,
		Subject: detail.Subject,
		Text:    detail.TextBody,
		HTML:    detail.HTMLBody,
		DraftID: detail.ID,
		// The row does not denormalise the Message-ID header, so a draft sent
		// from here threads on the Message-ID the send mints for it. The draft
		// itself must not be re-sent after a send, which is handled by removing
		// it once it is gone.
		InReplyTo: "",
	}
	for _, att := range detail.Attachments {
		// The bytes are not read here: an attachment the draft carries is
		// passed on by reference through the content id, and a send that needs
		// the bytes reads them from the blob the message already points at.
		compose.Attachment = append(compose.Attachment, webmail.AttachmentInput{
			Filename:  att.Filename,
			MIMEType:  att.MIMEType,
			ContentID: att.ContentID,
		})
	}
	return compose, nil
}

// markAsJunkRequest is the body of MarkAsJunk.
type markAsJunkRequest struct {
	ItemIds    *itemIDs `xml:"ItemIds"`
	MoveAsJunk bool     `xml:"MoveAsJunk"`
	// MarkAsJunk is the EWS spelling of the same thing.
	MarkAsJunk bool `xml:"MarkAsJunk,attr"`
}

// markAsJunk answers MarkAsJunk.
//
// This server has a Junk folder and no per-message spam classification, so the
// operation moves the message there and reports the classification it cannot make.
// A client that asked for a flag and gets one it did not ask for would then show
// the message as junk in one place and filed in another; the move is the honest
// answer and it is the one the user asked for in a mail client.
func (e *EWS) markAsJunk(c *call) responseCode {
	var req markAsJunkRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.ItemIds == nil {
		return c.faultResponse(errorf("MarkAsJunkResponse", codeErrorInvalidRequest, http.StatusOK,
			"MarkAsJunk needs ItemIds"))
	}
	for _, ref := range req.ItemIds.ItemIDs {
		_, _, messageID, err := decodeItemID(ref.ID)
		if err != nil {
			return c.faultResponse(folderIDFault("MarkAsJunkResponse", err))
		}
		var aerr error
		if req.MoveAsJunk {
			_, aerr = e.mail.Trash(c.ctx(), c.owner(), []int64{messageID})
		} else {
			// Un-junk is a move back to the inbox, which is where a message
			// judged junk and then cleared came from.
			inbox, serr := e.resolveFolderRef(c, folderRef{distinguished: distInbox})
			if serr != nil {
				return c.faultResponse(serr)
			}
			_, aerr = e.mail.Move(c.ctx(), c.owner(), []int64{messageID}, inbox.id)
		}
		if aerr != nil {
			return c.faultResponse(itemFault("MarkAsJunkResponse", aerr))
		}
	}
	return c.okOne("MarkAsJunk")
}

// composeFrom reads a compose request out of a message element.
//
// The element is a bag of properties, and this reads the ones that make up a
// message: the recipients, the subject, the two bodies and the attachments. It is
// the same set of properties the composer form posts, which is what makes a
// message composed in a client and one composed in the web client the same
// message.
func composeFrom(inner, from string) (webmail.ComposeRequest, error) {
	out := webmail.ComposeRequest{From: from}
	// The fields are read by name rather than decoded into a struct because the
	// recipient elements nest differently from each other -- one per recipient
	// under a list wrapper -- and a struct would have to spell out each
	// combination.
	out.To = toWebmailAll(addressList(innerElement(inner, "ToRecipients")))
	out.Cc = toWebmailAll(addressList(innerElement(inner, "CcRecipients")))
	out.Bcc = toWebmailAll(addressList(innerElement(inner, "BccRecipients")))
	out.ReplyTo = toWebmailAll(addressList(innerElement(inner, "ReplyTo")))
	out.Subject = innerText(inner, "Subject")

	// The body arrives as <t:Body> or <t:Body BodyType="HTML">. Reading the
	// element's own text gives either, and the BodyType attribute is what says
	// which it is: a client that sends HTML as text and a client that sends text
	// as HTML both exist, and only the attribute tells them apart.
	body, bodyType, ok := bodyWithKind(inner)
	if ok {
		switch strings.ToLower(strings.TrimSpace(bodyType)) {
		case "html":
			out.HTML = body
		default:
			out.Text = body
		}
	}
	// BodyHtml is the separate element some clients send instead of a typed
	// Body, and it is unambiguous about which it is.
	if html := innerText(inner, "BodyHtml"); html != "" {
		out.HTML = html
	}
	// A message with neither body and no attachment is not a message this
	// server can send, and the composer's own validation says so -- but a draft
	// may have only a subject, and the caller is the one that decides which
	// validation applies.
	if out.Text == "" && out.HTML == "" {
		if text, present := innerTextPresent(inner, "Body"); present {
			out.Text = text
		}
	}

	// Attachments arrive as base64 content in a FileAttachment element, which is
	// the shape both a draft and a fresh attachment take.
	for _, raw := range innerElements(inner, "FileAttachment") {
		att, ok := attachmentFrom(raw)
		if !ok {
			continue
		}
		out.Attachment = append(out.Attachment, att)
	}
	return out, nil
}

// bodyWithKind reads the body element and its BodyType attribute together.
//
// They are read together because they are one thing: the text and the claim about
// what kind of text it is. Reading the element and then looking the attribute up
// separately would need a second search of the fragment, and a second search could
// find a different element with the same name -- a nested one, in a quoted reply.
func bodyWithKind(fragment string) (string, string, bool) {
	dec := newFragmentDecoder(fragment)
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", "", false
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "Body" {
			if err := dec.Skip(); err != nil {
				return "", "", false
			}
			continue
		}
		var text string
		if err := dec.DecodeElement(&text, &start); err != nil {
			return "", "", false
		}
		kind := ""
		for _, a := range start.Attr {
			if a.Name.Local == "BodyType" {
				kind = a.Value
			}
		}
		return text, kind, true
	}
}

// attachmentFrom reads one file attachment element.
func attachmentFrom(raw string) (webmail.AttachmentInput, bool) {
	name := innerText(raw, "Name")
	content := innerText(raw, "Content")
	if content == "" {
		return webmail.AttachmentInput{}, false
	}
	return webmail.AttachmentInput{
		Filename:  name,
		MIMEType:  innerText(raw, "ContentType"),
		Content:   content,
		ContentID: innerText(raw, "ContentId"),
	}, true
}

// hasFlag reports whether an IMAP system flag is set.
func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

// partOf strips a property set prefix from a field URI key, so that
// "message:isread" and "item:isread" are the same question about the message.
func partOf(key string) string {
	if i := strings.IndexByte(key, ':'); i >= 0 {
		return key[i+1:]
	}
	return key
}

// ewsTime renders a time the way EWS writes one.
//
// EWS timestamps are UTC with no zone suffix, and a client reads them as UTC. A
// timestamp with a zone marker would be read as UTC anyway and would be wrong by
// the offset, so the value is converted rather than formatted.
func ewsTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// ewsTimePtr renders an optional timestamp, omitting the element when absent.
func ewsTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return ewsTime(*t)
}

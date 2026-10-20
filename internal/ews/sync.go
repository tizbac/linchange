package ews

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// Synchronisation.
//
// EWS' two sync operations are incremental: the client sends back an opaque
// SyncState and the server replies with only what changed since that state. The
// state is this server's contract with one client about one folder, and the
// contract is a snapshot: which items the client was told about and the revision
// token of each. Diffing today's folder against that snapshot is the whole
// operation, and the snapshot's format is a private matter -- the protocol says
// only that the string is opaque, which is what lets a snapshot live inside it.

// syncFolderHierarchyRequest is the body of SyncFolderHierarchy.
type syncFolderHierarchyRequest struct {
	FolderShape  *shapeRequest `xml:"FolderShape"`
	SyncFolderId *folderIDs    `xml:"SyncFolderId"`
	SyncState    string        `xml:"SyncState"`
}

// syncFolderHierarchy answers SyncFolderHierarchy (MS-OXECWS §2.2.10.10).
//
// The hierarchy of this server is small -- a flat list of mail folders plus the
// calendars a principal can read -- so the whole tree is diffed and the answer is
// never partial: either nothing changed, or the changes are all of them. The
// IncludesLastFolderInRange flag is always true, which is what tells the client
// it can stop calling.
func (e *EWS) syncFolderHierarchy(c *call) responseCode {
	var req syncFolderHierarchyRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	sh := parseShape(req.FolderShape)

	// The sync is scoped to the whole tree. A SyncFolderId is accepted when it
	// names the root in either of its spellings, because that is what clients
	// send; naming a leaf asks the question "what changed under this one folder",
	// and this server's single-level hierarchy has nothing to say under it.
	fromRoot := false
	if req.SyncFolderId != nil {
		for _, ref := range allFolderRefs(req.SyncFolderId) {
			switch strings.ToLower(strings.TrimSpace(ref.distinguished)) {
			case distRoot, distMsgFolderRoot:
				fromRoot = true
			}
		}
	} else {
		fromRoot = true
	}
	if !fromRoot {
		return c.faultResponse(errorf("SyncFolderHierarchyResponse", codeErrorInvalidRequest,
			http.StatusOK, "SyncFolderHierarchy syncs the whole hierarchy"))
	}

	parents, serr := e.resolveFolderRefs(c, &folderIDs{Distinguished: []distinguishedFolderID{
		{ID: distRoot},
	}})
	if serr != nil {
		return c.faultResponse(serr)
	}
	current, serr := e.collectContainers(c, parents, "Deep", true)
	if serr != nil {
		return c.faultResponse(serr)
	}

	state, present, serr := decodeSyncState[map[string]string](req.SyncState)
	if serr != nil {
		return c.faultResponse(serr)
	}
	if !present {
		state = nil
	}

	snapshot := make(map[string]string, len(current))
	for i := range current {
		snapshot[current[i].identifier()] = current[i].hierarchyKey()
	}

	var b strings.Builder
	b.WriteString(`<m:RootFolder><t:Changes>`)
	written := diffFolders(&b, current, state, sh)
	b.WriteString(`</t:Changes></m:RootFolder>`)

	// Nothing changed and the client already has the snapshot: the answer is an
	// empty change list and the same state back. The state is re-encoded so that
	// every answer is the same string for the same content, which is what a
	// client compares to decide whether to keep its copy.
	if written == 0 && snapshotEquals(state, snapshot) {
		return c.okHierarchy(req.SyncState, "")
	}

	encoded, _ := encodeSyncState(snapshot)
	return c.okHierarchy(encoded, b.String())
}

// diffFolders writes the Create, Update and Delete elements for a folder sync.
//
// A folder is Create when the client never heard of it, Update when its name or
// placement changed, and Delete when it is gone. Deleted folders carry only their
// identifier, which is all a client needs to forget them; created and updated
// ones are rendered with the shape, because the client draws them.
func diffFolders(b *strings.Builder, current []container, known map[string]string, sh shape) int {
	written := 0
	now := map[string]bool{}
	for i := range current {
		f := &current[i]
		key := f.identifier()
		now[key] = true
		ck := f.hierarchyKey()
		switch {
		case known == nil || known[key] == "":
			b.WriteString(`<t:Create>`)
			renderContainer(b, *f, sh)
			b.WriteString(`</t:Create>`)
			written++
		case known[key] != ck:
			b.WriteString(`<t:Update>`)
			renderContainer(b, *f, sh)
			b.WriteString(`</t:Update>`)
			written++
		}
	}
	// Deletion is decided from the previous snapshot, which is the only record of
	// what the client was told about.
	for key := range known {
		if now[key] {
			continue
		}
		b.WriteString(`<t:Delete><t:FolderId Id="`)
		b.WriteString(escape(key))
		b.WriteString(`"/></t:Delete>`)
		written++
	}
	return written
}

// okHierarchy renders the SyncFolderHierarchy response message.
func (c *call) okHierarchy(syncState, changes string) responseCode {
	var b strings.Builder
	b.WriteString(`<m:SyncState>`)
	b.WriteString(escape(syncState))
	b.WriteString(`</m:SyncState>`)
	b.WriteString(changes)
	b.WriteString(`<m:IncludesLastFolderInRange>true</m:IncludesLastFolderInRange>`)
	return responseCode{
		element: "SyncFolderHierarchyResponse",
		body:    responseMessageOne("SyncFolderHierarchy", b.String()),
	}
}

// hierarchyKey is the revision token the hierarchy sync compares.
//
// It deliberately excludes the counts, because a message arriving is not a change
// to the hierarchy: without this distinction every new email would be reported to
// a client syncing its folder list as every folder in the tree having changed.
func (c container) hierarchyKey() string {
	parent := int64(0)
	if c.parent != nil {
		parent = *c.parent
	}
	return changeKey(c.kind, c.id, c.name, parent, c.class,
		c.mail != nil && c.mail.Subscribed)
}

// syncFolderItemsRequest is the body of SyncFolderItems.
type syncFolderItemsRequest struct {
	ItemShape          *shapeRequest `xml:"ItemShape"`
	SyncFolderId       *folderIDs    `xml:"SyncFolderId"`
	SyncState          string        `xml:"SyncState"`
	MaxChangesReturned int           `xml:"MaxChangesReturned"`
}

// syncFolderItems answers SyncFolderItems (MS-OXECWS §2.2.10.24).
//
// The sync state is a snapshot of the client's knowledge of one folder: every
// item identifier it was told about and the change key of each. The request is
// then a diff -- items new, changed, or gone since that snapshot -- bounded by
// MaxChangesReturned. When the bound is hit, the answer says there is more, and
// the client's next SyncFolderItems, carrying the returned state, continues from
// the same folder.
func (e *EWS) syncFolderItems(c *call) responseCode {
	var req syncFolderItemsRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.SyncFolderId == nil {
		return c.faultResponse(errorf("SyncFolderItemsResponse", codeErrorInvalidRequest, http.StatusOK,
			"SyncFolderItems needs SyncFolderId"))
	}
	folder, serr := e.resolveFolderRef(c, firstFolderRef(req.SyncFolderId))
	if serr != nil {
		return c.faultResponse(serr)
	}
	sh := parseShape(req.ItemShape)

	// A calendar folder syncs the same way as a mailbox one, over the events
	// rows instead of the messages rows: a snapshot of change keys diffed
	// against today's. The calendar is where a client's appointments live, and
	// an Outlook that cannot sync it offers a read-only month view that never
	// converges with what was saved in the web app.
	if folder.isCalendar() {
		return e.syncCalendarItems(c, req, folder, sh)
	}

	messages, serr := e.allFolderMessages(c, folder)
	if serr != nil {
		return c.faultResponse(serr)
	}

	state, present, serr := decodeSyncState[folderItemsState](req.SyncState)
	if serr != nil {
		return c.faultResponse(serr)
	}
	var known map[string]string
	if present {
		if state.Folder != 0 && state.Folder != folder.id {
			// A state that belongs to a different folder is a client that synced one
			// folder and asked about another. Diffing against it would be nonsense;
			// the client is told and its next request, without a state, is a fresh
			// sync of the folder it actually asked about.
			return c.faultResponse(errorf("SyncFolderItemsResponse", codeErrorInvalidRequest, http.StatusOK,
				"the sync state belongs to another folder"))
		}
		known = state.Items
	}
	if known == nil {
		known = map[string]string{}
	}

	// The items, newest first, exactly as the store ordered them -- paging walked
	// them from newest to oldest, so the slice is that walk.
	views := make([]*itemView, 0, len(messages))
	for i := range messages {
		summary := messages[i]
		views = append(views, &itemView{
			kind:      "Message",
			container: folder.id,
			id:        summary.ID,
			owner:     folder.mailOwner(c.owner()),
			message:   &summary,
			class:     itemClassOf(summary),
		})
	}

	// Bodies and recipients are fetched once per page in the same shape-driven
	// way a FindItem does it, so a sync asking for body or display addresses
	// does not turn a read of a thousand messages into a thousand reads.
	if sh.wantsBody() {
		for _, v := range views {
			if detail, derr := e.mail.Message(c.ctx(), v.owner, v.id); derr == nil {
				v.detail = &detail
			}
		}
	}
	if sh.anyOf("message:to", "message:cc", "message:bcc", "message:displayto",
		"message:displaycc", "message:displaybcc", "message:replyto") || sh.all() {
		ids := make([]int64, 0, len(views))
		for _, v := range views {
			ids = append(ids, v.id)
		}
		owner := c.owner()
		if len(views) > 0 {
			owner = views[0].owner
		}
		recipients, serr := e.readRecipients(c, owner, ids)
		if serr != nil {
			return c.faultResponse(serr)
		}
		for _, v := range views {
			if a, ok := recipients[v.id]; ok {
				v.addresses = a
			}
		}
	}

	// The changes, old and new, compared against the client's snapshot.
	cap := req.MaxChangesReturned
	if cap <= 0 {
		// The schema default is 512, and the bound exists so that a folder of
		// ten thousand messages does not become one enormous response.
		cap = 512
	}

	currentIDs := make(map[int64]bool, len(views))
	for _, v := range views {
		currentIDs[v.id] = true
	}

	// Deletions are reported only for items the client had been told about, and
	// are found from the whole known set rather than from a window of it.
	var deleted []int64
	for idText := range known {
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil {
			continue
		}
		if !currentIDs[id] {
			deleted = append(deleted, id)
		}
	}
	sort.Slice(deleted, func(i, j int) bool { return deleted[i] < deleted[j] })

	// The response carries created and updated items in the folders' newest-first
	// order, which is also the order the client draws them in, and the deletes
	// after them. Bounding applies to all three kinds together, because a client
	// that received a page of creates and no deletes would mark nothing deleted
	// until the next sync.
	var items strings.Builder
	var changes strings.Builder
	sent := 0

	complete := true
	for _, v := range views {
		if sent >= cap {
			complete = false
			break
		}
		key := v.changeKey()
		idText := strconv.FormatInt(v.id, 10)
		knownID := known[idText]
		switch {
		case knownID == "":
			items.WriteString(`<t:Create>`)
			renderItem(&items, v, sh)
			items.WriteString(`</t:Create>`)
			known[idText] = key
			sent++
		case knownID != key:
			changes.WriteString(`<t:Update>`)
			renderItem(&changes, v, sh)
			changes.WriteString(`</t:Update>`)
			known[idText] = key
			sent++
		}
	}
	for _, id := range deleted {
		if sent >= cap {
			complete = false
			break
		}
		changes.WriteString(`<t:Delete><t:ItemId Id="`)
		changes.WriteString(escape(messageIDString(folder.id, id)))
		changes.WriteString(`"/></t:Delete>`)
		delete(known, strconv.FormatInt(id, 10))
		sent++
	}

	encoded, _ := encodeSyncState(folderItemsState{
		Folder: folder.id,
		Items:  known,
	})

	return okSyncItems(req, encoded, items, changes, sent, complete)
}

// okSyncItems renders the SyncFolderItems response message.
//
// The state is echoed back whenever nothing changed, so the client sees the sync
// is stable and stops. When a bound cut a response short there was something to
// say and more to say after it, and the change count decides which of those two
// answers the response carries.
func okSyncItems(req syncFolderItemsRequest, encoded string, items, changes strings.Builder, sent int, complete bool) responseCode {
	var b strings.Builder
	b.WriteString(`<m:SyncState>`)
	b.WriteString(escape(encoded))
	b.WriteString(`</m:SyncState>`)
	b.WriteString(`<m:RootFolder>`)
	if items.Len() > 0 {
		b.WriteString(`<t:Items>`)
		b.WriteString(items.String())
		b.WriteString(`</t:Items>`)
	}
	b.WriteString(`<t:Changes>`)
	b.WriteString(changes.String())
	b.WriteString(`</t:Changes>`)
	b.WriteString(`<t:ReadFlagChanges></t:ReadFlagChanges>`)
	b.WriteString(`</m:RootFolder>`)
	if sent == 0 {
		if items.Len() == 0 && changes.Len() == 0 {
			b.Reset()
			b.WriteString(`<m:SyncState>`)
			b.WriteString(escape(req.SyncState))
			b.WriteString(`</m:SyncState>`)
			b.WriteString(`<m:RootFolder><t:Changes></t:Changes><t:ReadFlagChanges></t:ReadFlagChanges></m:RootFolder>`)
		}
		b.WriteString(`<m:IncludesLastItemInRange>true</m:IncludesLastItemInRange>`)
	} else {
		b.WriteString(`<m:IncludesLastItemInRange>`)
		b.WriteString(strconv.FormatBool(complete))
		b.WriteString(`</m:IncludesLastItemInRange>`)
	}

	return responseCode{
		element: "SyncFolderItemsResponse",
		body:    responseMessageOne("SyncFolderItems", b.String()),
	}
}

// syncCalendarItems answers SyncFolderItems for a calendar folder.
//
// The diff is the same shape as the mailbox one -- a snapshot of row change keys
// compared against today's rows -- but the rows are the events table rather than
// the messages table. Every event row of the calendar is read, whatever window a
// client would draw it in: a deleted event must still be reported when it was
// outside the window it used to be drawn in.
func (e *EWS) syncCalendarItems(c *call, req syncFolderItemsRequest, folder container, sh shape) responseCode {
	state, present, serr := decodeSyncState[folderItemsState](req.SyncState)
	if serr != nil {
		return c.faultResponse(serr)
	}
	var known map[string]string
	if present {
		if state.Folder != 0 && state.Folder != folder.id {
			return c.faultResponse(errorf("SyncFolderItemsResponse", codeErrorInvalidRequest, http.StatusOK,
				"the sync state belongs to another folder"))
		}
		known = state.Items
	}
	if known == nil {
		known = map[string]string{}
	}

	events, serr := e.allCalendarEvents(c, folder.id)
	if serr != nil {
		return c.faultResponse(serr)
	}

	currentIDs := make(map[int64]bool, len(events))
	for _, ev := range events {
		currentIDs[ev.ID] = true
	}

	var deleted []int64
	for idText := range known {
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil {
			continue
		}
		if !currentIDs[id] {
			deleted = append(deleted, id)
		}
	}
	sort.Slice(deleted, func(i, j int) bool { return deleted[i] < deleted[j] })

	cap := req.MaxChangesReturned
	if cap <= 0 {
		cap = 512
	}

	var items strings.Builder
	var changes strings.Builder
	sent := 0

	complete := true
	for _, ev := range events {
		if sent >= cap {
			complete = false
			break
		}
		key := eventChangeKey(ev)
		idText := strconv.FormatInt(ev.ID, 10)
		knownID := known[idText]
		switch {
		case knownID == "":
			items.WriteString(`<t:Create>`)
			e.renderCalendarSyncItem(&items, c, folder.id, ev, sh)
			items.WriteString(`</t:Create>`)
			known[idText] = key
			sent++
		case knownID != key:
			changes.WriteString(`<t:Update>`)
			e.renderCalendarSyncItem(&changes, c, folder.id, ev, sh)
			changes.WriteString(`</t:Update>`)
			known[idText] = key
			sent++
		}
	}
	for _, id := range deleted {
		if sent >= cap {
			complete = false
			break
		}
		changes.WriteString(`<t:Delete><t:ItemId Id="`)
		changes.WriteString(escape(eventIDString(folder.id, id)))
		changes.WriteString(`"/></t:Delete>`)
		delete(known, strconv.FormatInt(id, 10))
		sent++
	}

	encoded, _ := encodeSyncState(folderItemsState{
		Folder: folder.id,
		Items:  known,
	})

	return okSyncItems(req, encoded, items, changes, sent, complete)
}

// allCalendarEvents reads every event row of one calendar.
//
// The window is the whole range the database will store, because a row outside
// it is one that cannot be either created or fetched, so there is no horizon to
// keep the read inside. The rows are not expanded into occurrences: the sync is
// about the row, and expansion is the rendering step's work.
func (e *EWS) allCalendarEvents(c *call, calendarID int64) ([]store.Event, *soapError) {
	events, err := store.NewEvents().ListInRange(c.ctx(), e.db, []int64{calendarID},
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))
	if err != nil {
		return nil, e.internal("SyncFolderItems", err)
	}
	return events, nil
}

// eventChangeKey is the revision token of one event row.
//
// It hashes the same inputs as the fetched-item renderer, so the change key a
// sync reports and the change key a fetched item carries are the same string,
// and a client that updates what it synced carries a token the server saw.
func eventChangeKey(ev store.Event) string {
	return changeKey(ev.ID, ev.Sequence, ev.DTStart.UnixNano(), ev.DTEnd.UnixNano(), ev.Status)
}

// renderCalendarSyncItem renders one event for a sync change, matching the shape.
//
// The occurrence is built from the row rather than expanded, because the change
// is about the row -- identity and revision -- and the row's own times and
// summary are what the store keeps for it. Expanding every changed event here
// would be GetItem work the sync does not need, and a series with a thousand
// occurrences would turn one sync into a thousand expansions.
//
// The occurrence is built even for an identity-only shape: the identifier and
// the change key come out of it, and without it the renderer would build the
// item as a message -- a calendar event whose identifier says mailbox kind.
func (e *EWS) renderCalendarSyncItem(b *strings.Builder, c *call, calendarID int64, ev store.Event, sh shape) {
	v := &itemView{
		kind:      "CalendarItem",
		container: calendarID,
		id:        ev.ID,
		event:     &ev,
		class:     itemClassAppointment,
	}
	// The occurrence mirrors the way the calendar service builds one from a
	// recurrence instance, minus the work of expansion: the row's own times are
	// the occurrence's. The recurrence id is normalised to UTC, which is the key
	// the service uses to match slots.
	var recurrenceID *time.Time
	if ev.RecurrenceID != nil {
		utc := ev.RecurrenceID.UTC()
		recurrenceID = &utc
	}
	v.occurrence = &calendar.View{
		EventID:          ev.ID,
		CalendarID:       ev.CalendarID,
		UID:              ev.UID,
		RecurrenceID:     recurrenceID,
		Summary:          ev.Summary,
		Location:         ev.Location,
		Start:            ev.DTStart,
		End:              ev.DTEnd,
		AllDay:           ev.AllDay,
		Timezone:         ev.Timezone,
		Status:           ev.Status,
		Organizer:        ev.Organizer,
		OrganizerAddress: ev.OrganizerAddress,
		Sequence:         ev.Sequence,
		Overridden:       ev.IsOverride(),
		Attendees:        ev.Attendees,
	}
	renderCalendarItem(b, v, sh)
}

// allFolderMessages reads every message of a folder, walked in batches.
//
// One request answers at most a page (the store's own bound), so a large folder
// is read as a walk: each page's last row becomes the next page's cursor until
// the walk reports no more pages. The walk is newest-first, which is the store's
// one ordering, and the caller turns it into whatever order it needs.
//
// The owner is the mailbox the folder belongs to, which for a shared mailbox is
// not the caller: the same query machinery that reads a caller's folder reads a
// mailbox the caller has been granted, because access was already decided when
// the folder was resolved.
func (e *EWS) allFolderMessages(c *call, folder container) ([]webmail.Message, *soapError) {
	if folder.kind == kindSharedRoot {
		// The root of a shared mailbox holds no items of its own: the mail is in
		// the folders beneath it. A client syncing Exchange's root folder gets
		// the same empty answer and then syncs the Inbox under it.
		return nil, nil
	}
	var out []webmail.Message
	var cursorDate time.Time
	var cursorID int64
	for {
		q := &imapd.MailQuery{
			Owner:     folder.mailOwner(c.owner()),
			FolderIDs: []int64{folder.id},
			Limit:     imapd.MaxListLimit,
			Count:     false,
		}
		if !cursorDate.IsZero() {
			q.CursorDate = cursorDate
			q.CursorID = cursorID
		}
		result, err := e.messages.Query(c.ctx(), e.db, *q)
		if err != nil {
			return nil, e.internal("SyncFolderItems", err)
		}
		if len(result.Messages) == 0 {
			break
		}
		for i := range result.Messages {
			out = append(out, webmail.MessageFromRow(result.Messages[i]))
		}
		if !result.HasMore || result.CursorID == 0 {
			break
		}
		cursorDate, cursorID = result.CursorDate, result.CursorID
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The sync state envelope
// ---------------------------------------------------------------------------

// folderItemsState is the snapshot a SyncFolderItems state holds.
//
// It is JSON under base64 because it is opaque on the wire but not in here:
// decoding it is how the diff is made. The snapshot is the whole set of items
// the client has been told about and the change key of each, which is all the
// next request needs: the folder is diffed against it and the answer is the
// difference, bounded by MaxChangesReturned when the difference is large.
type folderItemsState struct {
	Folder int64             `json:"f,omitempty"`
	Items  map[string]string `json:"i,omitempty"`
}

// decodeSyncState reads the client's state, or reports a malformed one.
//
// The second result reports whether a state was present at all. A blank state is
// the first synchronisation -- the client has nothing yet. A state that is not
// blank and does not decode is a client or a bug that has corrupted it, and the
// right answer is to refuse rather than to pretend a fresh sync: a client told
// everything again from a corrupt state would re-send every local change it made
// against what it believed was the server's copy.
func decodeSyncState[T any](encoded string) (T, bool, *soapError) {
	var out T
	if strings.TrimSpace(encoded) == "" {
		return out, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	if err != nil {
		var zero T
		return zero, false, errorf("", codeErrorInvalidRequest, http.StatusOK,
			"the sync state could not be read")
	}
	return out, true, nil
}

// encodeSyncState writes our state for the client to return untouched.
func encodeSyncState(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// snapshotEquals reports whether the client already holds the current snapshot.
func snapshotEquals(known, current map[string]string) bool {
	if known == nil {
		return false
	}
	for key, ck := range current {
		if known[key] != ck {
			return false
		}
	}
	for key := range known {
		if _, ok := current[key]; !ok {
			return false
		}
	}
	return true
}

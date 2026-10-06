package ews

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// Folder operations.
//
// A folder in EWS is addressed by identifier, and the identifier is this server's
// row id in a fixed encoding (see ids.go). Nothing else is a folder: a display
// name is what a user sees, and a distinguished name is the protocol's way of
// asking for one of the folders that has to exist under a fixed name.

// folderRequest is the body of FindFolder, GetFolder and SyncFolderHierarchy.
//
// FolderShape is a t:FolderShapeType, the same shape type an ItemShape uses, so
// the type is shared: the two differ only in what they are applied to, and a
// client that gets one wrong would be told its request was schema-invalid rather
// than silently shown the wrong properties.
type folderRequest struct {
	FolderShape     *shapeRequest `xml:"FolderShape"`
	ParentFolderIds *folderIDs    `xml:"ParentFolderIds"`
	FolderIds       *folderIDs    `xml:"FolderIds"`
	SyncFolderId    *folderIDs    `xml:"SyncFolderId"`
	SyncState       string        `xml:"SyncState"`
	Ignore          *folderIDs    `xml:"Ignore"`
	Traversal       string        `xml:"Traversal,attr"`
}

// folderIDs is a list of folder references in the two shapes the protocol allows.
type folderIDs struct {
	Distinguished []distinguishedFolderID `xml:"DistinguishedFolderId"`
	Plain         []folderIDElement       `xml:"FolderId"`
}

// folderIDElement is a t:FolderId.
type folderIDElement struct {
	ID        string `xml:"Id,attr"`
	ChangeKey string `xml:"ChangeKey,attr,omitempty"`
}

// distinguishedFolderID is a t:DistinguishedFolderId.
type distinguishedFolderID struct {
	ID           string `xml:"Id,attr"`
	PublicFolder string `xml:"PublicFolder,attr,omitempty"`
}

// folderRef is one folder named one of the two ways.
type folderRef struct {
	// plain is a base64 identifier, empty when the folder was named by its
	// distinguished name.
	plain string
	// distinguished is the EWS name of a standard folder.
	distinguished string
}

// allFolderRefs flattens the two spellings of a folder reference list.
//
// A client may use either <t:FolderId Id="..."/> or <t:DistinguishedFolderId
// Id="inbox"/>, and the protocol allows both in the same list. Rendering them as
// one sequence is what keeps the "the first unreadable folder fails the request"
// rule the same whichever spelling it arrived in.
func allFolderRefs(ids *folderIDs) []folderRef {
	if ids == nil {
		return nil
	}
	out := make([]folderRef, 0, len(ids.Plain)+len(ids.Distinguished))
	for _, d := range ids.Distinguished {
		out = append(out, folderRef{distinguished: d.ID})
	}
	for _, p := range ids.Plain {
		out = append(out, folderRef{plain: p.ID})
	}
	return out
}

// firstFolderRef reads the one reference a single-folder element names.
//
// An operation that takes one folder -- CreateFolder's parent, MoveItem's
// destination -- is given one, and taking the first of a list that had only one
// element in it is the same answer written twice.
func firstFolderRef(ids *folderIDs) folderRef {
	refs := allFolderRefs(ids)
	if len(refs) == 0 {
		return folderRef{}
	}
	return refs[0]
}

// ---------------------------------------------------------------------------
// Resolving
// ---------------------------------------------------------------------------

// resolveFolderRefs resolves a whole list, failing on the first unreadable
// reference.
//
// FindFolder fails as a whole because a partial folder tree is not something a
// client can sync from: it would store the branch it was given and believe the
// rest was empty. GetFolder does not, because each answer there stands alone.
func (e *EWS) resolveFolderRefs(c *call, ids *folderIDs) ([]container, *soapError) {
	refs := allFolderRefs(ids)
	if len(refs) == 0 {
		return nil, errorf(c.name+"Response", codeErrorInvalidRequest, http.StatusOK,
			"no folder was named")
	}
	out := make([]container, 0, len(refs))
	for _, ref := range refs {
		f, serr := e.resolveFolderRef(c, ref)
		if serr != nil {
			return nil, serr
		}
		out = append(out, f)
	}
	return out, nil
}

// resolveFolderRef resolves one folder reference, refusing anything that is not
// this caller's own.
//
// The ownership check is not an optimisation. A folder identifier is base64, and
// one that decodes to somebody else's row id is a perfectly well-formed
// identifier; the only thing standing between it and another account's folder is
// the check that the folder belongs to a container this principal can reach.
func (e *EWS) resolveFolderRef(c *call, ref folderRef) (container, *soapError) {
	if ref.distinguished != "" {
		return e.resolveDistinguished(c, ref.distinguished)
	}

	kind, id, err := decodeFolderID(ref.plain)
	if err != nil {
		return container{}, folderIDFault(c.name+"Response", err)
	}
	if kind == kindCalendar {
		return e.resolveCalendar(c, id)
	}
	folder, ferr := e.mail.Folder(c.ctx(), c.owner(), id)
	if ferr != nil {
		return container{}, folderWriteFault(c.name+"Response", ferr)
	}
	return mailFolderView(folder), nil
}

// resolveDistinguished maps a standard folder's EWS name onto the container.
//
// The calendars and the mail folders are separate vocabularies because they are
// separate kinds of thing: "calendar" is a kind of container this server has one
// word for, while "inbox" is a mail folder, and a client that asks for a calendar
// and gets mail is worse off than one that gets a fault, because it would file
// appointments as messages.
func (e *EWS) resolveDistinguished(c *call, name string) (container, *soapError) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case distCalendar, distTasks, distContacts:
		if strings.EqualFold(strings.TrimSpace(name), distCalendar) {
			return e.defaultCalendar(c)
		}
		// The tasks and contacts folders do not exist as separate stores here:
		// a task is an appointment and a contact is an address, both of which
		// live in the folder a client already has. Reporting the calendar would
		// put a message in it, so the folder is reported as absent instead.
		return container{}, errorf(c.name+"Response", codeErrorFolderNotFound, http.StatusOK,
			"this server has no %s folder", name)
	case distSearchFolders:
		// A search folder is a saved query. This server has no saved searches,
		// so the answer is an empty folder rather than a fault: a client that
		// finds one there would offer to save one, and saving would have nowhere
		// to save to. An absent folder is what makes it not offer.
		return container{}, errorf(c.name+"Response", codeErrorFolderNotFound, http.StatusOK,
			"this server has no saved search folders")
	}

	folderName, ok := mailFolderByDistinguished[strings.ToLower(name)]
	if !ok {
		return container{}, errorf(c.name+"Response", codeErrorFolderNotFound, http.StatusOK,
			"%s is not a standard folder this server has", name)
	}
	folders, err := e.mail.Folders(c.ctx(), c.owner())
	if err != nil {
		return container{}, e.internal(c.name, err)
	}
	for _, f := range folders {
		if strings.EqualFold(f.Name, folderName) {
			return mailFolderView(f), nil
		}
	}
	return container{}, errorf(c.name+"Response", codeErrorFolderNotFound, http.StatusOK,
		"this account has no %s folder", folderName)
}

// defaultCalendar resolves the calendar a client syncing "the calendar folder"
// gets.
//
// It is the principal's first calendar, which is the one created with the account
// and the one a client that has never seen this server expects. Naming it by
// position rather than by row id is what makes it stable across a database
// restored from a dump, where row ids are not.
func (e *EWS) defaultCalendar(c *call) (container, *soapError) {
	cals, serr := e.calendarsFor(c)
	if serr != nil {
		return container{}, serr
	}
	if len(cals) == 0 {
		return container{}, errorf(c.name+"Response", codeErrorFolderNotFound, http.StatusOK,
			"this account has no calendar")
	}
	for _, cal := range cals {
		if strings.EqualFold(cal.Name, "Calendar") || strings.EqualFold(cal.DisplayName, "Calendar") {
			return calendarView(cal), nil
		}
	}
	return calendarView(cals[0]), nil
}

// resolveCalendar resolves a calendar by row id, refusing one the principal may
// not read.
//
// Reading a calendar by identifier is a read, and the policy decides who may read
// which. An identifier is base64 and one that decodes to a colleague's calendar
// is a perfectly well-formed identifier, so this is the check that stops it.
func (e *EWS) resolveCalendar(c *call, id int64) (container, *soapError) {
	cal, err := store.NewCalendars().Get(c.ctx(), e.db, id)
	if err != nil {
		return container{}, folderWriteFault(c.name+"Response", err)
	}
	target, err := e.cal.Target(c.ctx(), e.db, cal, c.who)
	if err != nil {
		return container{}, e.internal(c.name, err)
	}
	if err := e.cal.Authorize(c.who, policy.ActionReadCalendar, cal, target); err != nil {
		return container{}, errorf(c.name+"Response", codeErrorAccessDenied, http.StatusOK,
			"you do not have access to that calendar")
	}
	return calendarView(cal), nil
}

// ---------------------------------------------------------------------------
// Operations
// ---------------------------------------------------------------------------

// findFolder answers FindFolder.
//
// The operation asks for a folder and everything under it, so the answer is a walk
// from each named folder. A Shallow traversal stops after one level, which is what
// a client draws a folder pane from; a Deep traversal is a full sync.
func (e *EWS) findFolder(c *call) responseCode {
	var req folderRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.ParentFolderIds == nil {
		return c.faultResponse(errorf("FindFolderResponse", codeErrorInvalidRequest, http.StatusOK,
			"FindFolder needs ParentFolderIds"))
	}

	sh := parseShape(req.FolderShape)
	roots, serr := e.resolveFolderRefs(c, req.ParentFolderIds)
	if serr != nil {
		return c.faultResponse(serr)
	}

	// A client asking for "the folder that has the folders" -- root, or the
	// message folder root, which is the spelling the schema keeps for it -- is
	// asking to see the whole tree, and the calendar lives at the top of that
	// tree next to the inbox rather than inside it. Finding the calendar under
	// the inbox would put a client's meetings inside its mail, so the walker is
	// told whether the request was for the tree's root.
	fromRoot := false
	for _, ref := range allFolderRefs(req.ParentFolderIds) {
		switch strings.ToLower(strings.TrimSpace(ref.distinguished)) {
		case distRoot, distMsgFolderRoot:
			fromRoot = true
		}
	}

	views, serr := e.collectContainers(c, roots, req.Traversal, fromRoot)
	if serr != nil {
		return c.faultResponse(serr)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<m:RootFolder TotalItemsInView="%d" IncludesLastItemInRange="true"><t:Folders>`, len(views))
	for _, v := range views {
		renderContainer(&b, v, sh)
	}
	b.WriteString(`</t:Folders></m:RootFolder>`)

	return responseCode{
		element: "FindFolderResponse",
		body:    responseMessageOne("FindFolder", b.String()),
	}
}

// getFolder answers GetFolder, which names folders exactly rather than searching
// from a parent.
func (e *EWS) getFolder(c *call) responseCode {
	var req folderRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.FolderIds == nil {
		return c.faultResponse(errorf("GetFolderResponse", codeErrorInvalidRequest, http.StatusOK,
			"GetFolder needs FolderIds"))
	}

	sh := parseShape(req.FolderShape)
	var b strings.Builder
	b.WriteString(`<m:Folders>`)
	for _, ref := range allFolderRefs(req.FolderIds) {
		f, serr := e.resolveFolderRef(c, ref)
		if serr != nil {
			// One unreadable folder does not fail the batch: the client asked
			// about several and gets an answer about the others. The per-item
			// response message is how a failure is reported inside a batch, and
			// a client that ignored it would be told nothing at all.
			b.WriteString(faultResponseMessage(serr))
			continue
		}
		renderContainer(&b, f, sh)
	}
	b.WriteString(`</m:Folders>`)

	return responseCode{
		element: "GetFolderResponse",
		body:    responseMessageOne("GetFolder", b.String()),
	}
}

// createFolderRequest is the body of CreateFolder.
type createFolderRequest struct {
	ParentFolderId *folderIDs      `xml:"ParentFolderId"`
	Folder         *folderInnerXML `xml:"Folder"`
}

// folderInnerXML is a folder as CreateFolder sends it: a display name and
// whatever else the client chose to set, none of which needs parsing because the
// display name is the only thing a new folder can have.
type folderInnerXML struct {
	Inner string `xml:",innerxml"`
}

// createFolder answers CreateFolder.
//
// The parent is resolved before the folder is created even though this server's
// mail folders are a flat list: an unreadable parent means the client's idea of
// the tree and this server's disagree, and a folder created in the wrong place is
// harder to explain than a refused request.
func (e *EWS) createFolder(c *call) responseCode {
	var req createFolderRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.Folder == nil || req.ParentFolderId == nil {
		return c.faultResponse(errorf("CreateFolderResponse", codeErrorInvalidRequest, http.StatusOK,
			"CreateFolder needs a Folder and a ParentFolderId"))
	}
	name := innerText(req.Folder.Inner, "DisplayName")
	if strings.TrimSpace(name) == "" {
		return c.faultResponse(errorf("CreateFolderResponse", codeErrorInvalidRequest, http.StatusOK,
			"CreateFolder needs a folder display name"))
	}
	if _, serr := e.resolveFolderRefs(c, req.ParentFolderId); serr != nil {
		return c.faultResponse(serr)
	}

	created, err := e.mail.CreateFolder(c.ctx(), c.owner(), name)
	if err != nil {
		return c.faultResponse(errorf("CreateFolderResponse", codeErrorInvalidRequest, http.StatusOK,
			"the folder could not be created: %v", err))
	}

	var b strings.Builder
	b.WriteString(`<m:Folders>`)
	renderContainer(&b, mailFolderView(created), shape{base: shapeAllProperties})
	b.WriteString(`</m:Folders>`)
	return responseCode{
		element: "CreateFolderResponse",
		body:    responseMessageOne("CreateFolder", b.String()),
	}
}

// updateFolderRequest is the body of UpdateFolder, which in practice renames.
type updateFolderRequest struct {
	FolderChanges *folderChanges `xml:"FolderChanges"`
}

type folderChanges struct {
	Changes []folderChange `xml:"ItemChange"`
}

type folderChange struct {
	FolderID folderIDElement `xml:"FolderId"`
	Updates  folderUpdates   `xml:"Updates"`
}

// folderUpdates holds the two ways a folder can be changed.
//
// Both are modelled because both arrive: a client renaming a folder sends a
// SetItemField naming DisplayName, and one applying a whole folder back sends a
// Folder replacement. Recognising the second is what makes a rename through this
// server land the same way as a rename through the web client.
type folderUpdates struct {
	Set []struct {
		FieldURI fieldURI `xml:"FieldURI"`
		Inner    string   `xml:",innerxml"`
	} `xml:"SetItemField"`
	Replace []struct {
		Inner string `xml:",innerxml"`
	} `xml:"Folder"`
}

// updateFolder answers UpdateFolder.
func (e *EWS) updateFolder(c *call) responseCode {
	var req updateFolderRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.FolderChanges == nil {
		return c.faultResponse(errorf("UpdateFolderResponse", codeErrorInvalidRequest, http.StatusOK,
			"UpdateFolder needs FolderChanges"))
	}
	for _, ch := range req.FolderChanges.Changes {
		kind, id, err := decodeFolderID(ch.FolderID.ID)
		if err != nil {
			return c.faultResponse(folderIDFault("UpdateFolderResponse", err))
		}
		if kind != kindMailbox {
			return c.faultResponse(errorf("UpdateFolderResponse", codeErrorInvalidRequest, http.StatusOK,
				"calendars cannot be renamed from an Exchange client"))
		}
		name, ok := updateDisplayName(ch.Updates)
		if !ok {
			// A change that names no folder property is a no-op rather than an
			// error: clients send one routinely, as part of a shape refresh.
			continue
		}
		if _, err := e.mail.RenameFolder(c.ctx(), c.owner(), id, name); err != nil {
			return c.faultResponse(folderWriteFault("UpdateFolderResponse", err))
		}
	}
	return c.okOne("UpdateFolder")
}

// updateDisplayName reads the new display name out of a folder's changes.
func updateDisplayName(up folderUpdates) (string, bool) {
	for _, f := range up.Replace {
		if name := innerText(f.Inner, "DisplayName"); name != "" {
			return name, true
		}
	}
	for _, f := range up.Set {
		key := f.FieldURI.key()
		if key == "displayname" || key == "folder:displayname" {
			return innerText(f.Inner, "DisplayName"), true
		}
	}
	return "", false
}

// deleteFolderRequest is the body of DeleteFolder.
type deleteFolderRequest struct {
	FolderIds  *folderIDs `xml:"FolderIds"`
	DeleteType string     `xml:"DeleteType"`
}

// deleteFolder answers DeleteFolder.
func (e *EWS) deleteFolder(c *call) responseCode {
	var req deleteFolderRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	if req.FolderIds == nil {
		return c.faultResponse(errorf("DeleteFolderResponse", codeErrorInvalidRequest, http.StatusOK,
			"DeleteFolder needs FolderIds"))
	}
	if strings.EqualFold(req.DeleteType, "movetodeleteditems") {
		// A folder moved to a deleted-items folder would be deleted, which is
		// not what was asked for. Refusing rather than doing something else is
		// the difference between a user finding their folder again and not.
		return c.faultResponse(errorf("DeleteFolderResponse", codeErrorNotAllowedInFolder, http.StatusOK,
			"folders cannot be moved to the deleted items folder on this server"))
	}
	for _, ref := range allFolderRefs(req.FolderIds) {
		target, serr := e.resolveFolderRef(c, ref)
		if serr != nil {
			return c.faultResponse(serr)
		}
		if !target.deletable {
			return c.faultResponse(errorf("DeleteFolderResponse", codeErrorCannotDeleteFolder, http.StatusOK,
				"%s is a standard folder and cannot be deleted", target.name))
		}
		if err := e.mail.DeleteFolder(c.ctx(), c.owner(), target.id); err != nil {
			return c.faultResponse(folderWriteFault("DeleteFolderResponse", err))
		}
	}
	return c.okOne("DeleteFolder")
}

// moveFolderRequest is the body of MoveFolder.
type moveFolderRequest struct {
	FolderID           folderIDElement `xml:"FolderId"`
	ConflictResolution string          `xml:"ConflictResolution,attr"`
}

// moveFolder answers MoveFolder.
//
// This server has one folder namespace with no nesting, so there is nowhere for a
// folder to move to. The request is refused rather than answered as a success: a
// client that moved a folder here and was told it had worked would then look for
// the folder in its new place for ever.
func (e *EWS) moveFolder(c *call) responseCode {
	var req moveFolderRequest
	if err := c.decode(&req); err != nil {
		return c.faultResponse(err)
	}
	return c.faultResponse(errorf("MoveFolderResponse", codeErrorInvalidRequest, http.StatusOK,
		"folders cannot be moved: this server has one flat folder list per mailbox"))
}

// collectContainers walks from each root, honouring the traversal.
//
// includeCalendars appends the principal's readable calendars when the request
// was for the whole tree, so the calendar a client then asks about by
// distinguished name is a folder it has actually been told about. It is a flag
// rather than something the walker discovers, because the walker starts from the
// resolved roots and by then the "root" itself is gone.
func (e *EWS) collectContainers(c *call, roots []container, traversal string, fromRoot bool) ([]container, *soapError) {
	softDeleted := strings.EqualFold(traversal, "softdeleted")
	if softDeleted {
		// A soft-deleted folder is one the client moved to its own deleted-items
		// folder. This server does not keep them, so the answer is empty rather
		// than an error: the client asked whether there are any and there are
		// none.
		return nil, nil
	}

	folders, err := e.mail.Folders(c.ctx(), c.owner())
	if err != nil {
		return nil, e.internal("FindFolder", err)
	}
	children := map[int64][]webmail.Folder{}
	for _, f := range folders {
		if f.ParentID == nil {
			continue
		}
		children[*f.ParentID] = append(children[*f.ParentID], f)
	}

	cals, serr := e.calendarsFor(c)
	if serr != nil {
		return nil, serr
	}

	deep := strings.EqualFold(traversal, "deep")
	seen := map[string]bool{}
	var out []container

	// A depth limit keeps a cycle in the parent pointers -- which the schema
	// prevents but a restored dump can contain -- from hanging the request. The
	// client gets a partial tree rather than no answer, which it can reconcile
	// on its next sync.
	var walk func(f container, depth int)
	walk = func(f container, depth int) {
		key := fmt.Sprintf("%d/%d", f.kind, f.id)
		if seen[key] || depth > 8 {
			return
		}
		seen[key] = true
		f.children = 0
		if !f.isCalendar() {
			kids := children[f.id]
			f.children = len(kids)
		}
		out = append(out, f)
		if !deep {
			return
		}
		if f.isCalendar() {
			// A calendar holds no folders. Walking into it would be walking
			// into the calendar's rows as if they were folders, and a client
			// that saw them there would try to rename them.
			return
		}
		for _, k := range kidsOf(children, f) {
			walk(mailFolderView(k), depth+1)
		}
	}

	// The folder list of this server is flat: every standard folder sits at the
	// top, with INBOX first among them, and a tree a client asked for by naming
	// the root is that whole list. Walking only the resolved root would answer
	// with the one folder named; the root of this server's tree is the set, so
	// the walk starts at each top-level folder.
	for _, root := range roots {
		if fromRoot {
			for _, f := range folders {
				if f.ParentID != nil {
					continue
				}
				walk(mailFolderView(f), 0)
			}
			continue
		}
		walk(root, 0)
	}

	if fromRoot {
		// The calendars live beside the mail folders rather than inside the
		// inbox, because finding them under the inbox would put a client's
		// meetings inside its mail. Whether the request was for the root is
		// what decides they belong in the answer at all.
		seenCalendars := map[int64]bool{}
		for _, cal := range cals {
			view := calendarView(cal)
			if seenCalendars[view.id] {
				continue
			}
			seenCalendars[view.id] = true
			walk(view, 0)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// kidsOf returns a mail folder's children, as mail folders.
func kidsOf(children map[int64][]webmail.Folder, f container) []webmail.Folder {
	if f.isCalendar() || f.mail == nil {
		return nil
	}
	return children[f.id]
}

// renderContainer writes one <t:Folder>, honouring the shape.
func renderContainer(b *strings.Builder, f container, sh shape) {
	b.WriteString(`<t:Folder>`)
	b.WriteString(`<t:FolderId Id="`)
	b.WriteString(escape(f.identifier()))
	b.WriteString(`"`)
	if ck := f.changeKey(); ck != "" {
		b.WriteString(` ChangeKey="`)
		b.WriteString(escape(ck))
		b.WriteString(`"`)
	}
	b.WriteString(`/>`)

	if sh.identity() {
		if f.parent != nil {
			b.WriteString(`<t:ParentFolderId Id="`)
			b.WriteString(escape(f.parentIdentifier()))
			b.WriteString(`"/>`)
		}
		b.WriteString(`</t:Folder>`)
		return
	}

	asked := func(key string) bool { return sh.asked(key, defaultFolderFields) }

	if asked("folder:displayname") {
		b.WriteString(`<t:DisplayName>`)
		b.WriteString(escape(f.name))
		b.WriteString(`</t:DisplayName>`)
	}
	if asked("folder:folderclass") {
		b.WriteString(`<t:FolderClass>`)
		b.WriteString(escape(f.class))
		b.WriteString(`</t:FolderClass>`)
	}
	if asked("folder:totalcount") {
		fmt.Fprintf(b, `<t:TotalCount>%d</t:TotalCount>`, f.total)
	}
	if asked("folder:childfoldercount") {
		fmt.Fprintf(b, `<t:ChildFolderCount>%d</t:ChildFolderCount>`, f.children)
	}
	if asked("folder:unreadcount") {
		fmt.Fprintf(b, `<t:UnreadCount>%d</t:UnreadCount>`, f.unread)
	}
	if f.parent != nil && asked("folder:parentfolderid") {
		b.WriteString(`<t:ParentFolderId Id="`)
		b.WriteString(escape(f.parentIdentifier()))
		b.WriteString(`"/>`)
	}
	if asked("folder:extendedproperty") {
		// The extended properties are where a client learns the folder's role
		// ("Sent Items") and the unread count it draws a badge from. Both are
		// read rather than merely tolerated, so both are sent.
		b.WriteString(`<t:ExtendedProperty>`)
		b.WriteString(`<t:ExtendedFieldURI PropertyTag="0x7FFA" PropertyType="Integer"/>`)
		b.WriteString(`<t:FolderType>`)
		b.WriteString(escape(folderTypeOf(f.name)))
		b.WriteString(`</t:FolderType>`)
		b.WriteString(`</t:ExtendedProperty>`)
		b.WriteString(`<t:ExtendedProperty>`)
		b.WriteString(`<t:ExtendedFieldURI PropertyTag="0x0E07" PropertyType="Integer"/>`)
		b.WriteString(`<t:UnreadCount>`)
		b.WriteString(strconv.Itoa(f.unread))
		b.WriteString(`</t:UnreadCount>`)
		b.WriteString(`</t:ExtendedProperty>`)
	}
	b.WriteString(`</t:Folder>`)
}

// changeKey is the container's revision token.
//
// It is derived from the counts and the name rather than from a counter, because
// a counter would have to be stored per folder and every one of these values
// already changes whenever the folder does. A client that caches it is told only
// when something it can see has moved.
func (c container) changeKey() string {
	parts := []any{c.id, c.name, c.total, c.unread, c.children}
	if c.mail != nil {
		parts = append(parts, c.mail.Subscribed)
	}
	return changeKey(parts...)
}

// folderTypeOf is the MS-OXPROPS folder type for a standard folder.
//
// A client uses it to decide what the folder is: a Sent Items folder with the
// wrong type is not shown where a user looks for sent mail. An unrecognised
// folder is reported as a normal folder rather than with no type, because no type
// makes a client treat it as an unknown store entry and refuse to sync it.
func folderTypeOf(name string) string {
	switch strings.ToLower(name) {
	case "inbox":
		return "Inbox"
	case "sent", "sent items":
		return "SentItems"
	case "drafts":
		return "Drafts"
	case "trash", "deleted items":
		return "DeletedItems"
	case "junk", "junk email":
		return "JunkMail"
	case "outbox":
		return "Outbox"
	case "archive":
		return "Archive"
	case "calendar":
		return "Calendar"
	}
	return "Other"
}

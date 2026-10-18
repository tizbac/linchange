package ews

import (
	"net/http"
	"sort"
	"strings"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/webmail"
)

// Shared mailboxes in EWS.
//
// MS-OXWSFOLD §2.1.2.2 lets a distinguished folder id name the mailbox that
// contains it:
//
//	<t:DistinguishedFolderId Id="inbox">
//	  <t:Mailbox><t:EmailAddress>team@example.com</t:EmailAddress></t:Mailbox>
//	</t:DistinguishedFolderId>
//
// That is the documented way a client opens a folder of a mailbox it does not own,
// and it is the mechanism mail clients use for team mailboxes. Access is decided
// by the same grant rows the web API reads: an explicit grant at any level, or the
// mailbox's open-access flag for members of its domain.
//
// A granted shared mailbox also appears in the folder hierarchy as a root folder
// beside the caller's own: the root stands for the mailbox, its children are the
// mailbox's folders, and the root itself holds no items -- exactly what Exchange
// answers for a root folder, so a client that syncs the root and finds nothing
// moves on to the Inbox beneath it rather than concluding the mailbox is empty.

// sharedMailbox is one shared mailbox the caller may open, with the folders it can
// see already loaded.
type sharedMailbox struct {
	address string
	name    string
	access  policy.SharedAccess
	folders []webmail.Folder
	// inboxID is the row id of the mailbox's INBOX folder, which doubles as the
	// identity of the root container that stands for the mailbox.
	inboxID int64
}

// mailboxDomain returns the domain part of an address, or the empty string when
// there is none.
func mailboxDomain(address string) string {
	if i := strings.LastIndex(address, "@"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(address[i+1:]))
	}
	return ""
}

// sharedMailboxes returns every shared mailbox the caller may read, each with its
// folders. The answer is cached per request because one request can ask several
// times -- a hierarchy walk, an identifier resolution, an item read -- and the
// database has one answer for all of them.
func (e *EWS) sharedMailboxes(c *call) ([]sharedMailbox, *soapError) {
	if c.sharedLoaded {
		return c.shared, c.sharedErr
	}
	c.sharedLoaded = true

	ctx := c.ctx()
	candidates := map[string]policy.SharedAccess{}

	grants, err := e.grants.ListForMember(ctx, e.db, c.owner())
	if err != nil {
		c.sharedErr = e.internal("shared mailboxes", err)
		return nil, c.sharedErr
	}
	for _, g := range grants {
		addr := strings.ToLower(strings.TrimSpace(g.SharedAddress))
		if addr != "" {
			candidates[addr] = policy.SharedAccess{Level: g.Level}
		}
	}
	if domain := mailboxDomain(c.owner()); domain != "" {
		open, err := e.shared.OpenAccessFor(ctx, e.db, domain)
		if err != nil {
			c.sharedErr = e.internal("shared mailboxes", err)
			return nil, c.sharedErr
		}
		for _, sm := range open {
			addr := strings.ToLower(sm.Address)
			if _, seen := candidates[addr]; !seen {
				candidates[addr] = policy.SharedAccess{Open: true}
			}
		}
	}

	out := make([]sharedMailbox, 0, len(candidates))
	for addr, access := range candidates {
		if !access.AtLeast(policy.GrantRead) {
			continue
		}
		decl, err := e.shared.Get(ctx, e.db, addr)
		if err != nil {
			// A grant outliving its declaration is a race between two admin
			// actions; the mailbox simply is not there to open.
			continue
		}
		// The folder set may not exist yet: a shared mailbox declared before its
		// first message arrives has no rows until somebody delivers one, and a
		// member shown a mailbox with no folders cannot file anything in it.
		if err := e.mail.EnsureFolders(ctx, addr); err != nil {
			c.sharedErr = e.internal("shared mailboxes", err)
			return nil, c.sharedErr
		}
		folders, err := e.mail.Folders(ctx, addr)
		if err != nil {
			c.sharedErr = e.internal("shared mailboxes", err)
			return nil, c.sharedErr
		}
		inbox := int64(0)
		for _, f := range folders {
			if strings.EqualFold(f.Name, "INBOX") {
				inbox = f.ID
				break
			}
		}
		if inbox == 0 {
			continue
		}
		name := strings.TrimSpace(decl.DisplayName)
		if name == "" {
			name = addr
		}
		out = append(out, sharedMailbox{
			address: addr,
			name:    name,
			access:  access,
			folders: folders,
			inboxID: inbox,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].address < out[j].address })

	c.shared = out
	return out, nil
}

// sharedMailboxFor finds one accessible shared mailbox by address.
func (e *EWS) sharedMailboxFor(c *call, address string) (sharedMailbox, *soapError) {
	list, serr := e.sharedMailboxes(c)
	if serr != nil {
		return sharedMailbox{}, serr
	}
	address = strings.ToLower(strings.TrimSpace(address))
	for _, sm := range list {
		if sm.address == address {
			return sm, nil
		}
	}
	return sharedMailbox{}, errorf(c.name+"Response", codeErrorAccessDenied, http.StatusOK,
		"this account has no access to the mailbox %s", address)
}

// sharedFolderByID finds a folder of an accessible shared mailbox by row id.
//
// Plain folder identifiers are this server's row ids, and a shared mailbox's
// folders have ordinary row ids, so a client that learned an identifier from the
// hierarchy can address it directly; the check is that the id belongs to a mailbox
// this caller may open.
func (e *EWS) sharedFolderByID(c *call, id int64) (container, *soapError) {
	list, serr := e.sharedMailboxes(c)
	if serr != nil {
		return container{}, serr
	}
	for _, sm := range list {
		for _, f := range sm.folders {
			if f.ID == id {
				return sharedFolderView(sm, f), nil
			}
		}
	}
	return container{}, errorf(c.name+"Response", codeErrorFolderNotFound, http.StatusOK,
		"no folder with that identifier is accessible to this account")
}

// resolveSharedRoot answers a plain identifier that names a shared mailbox root.
func (e *EWS) resolveSharedRoot(c *call, inboxID int64) (container, *soapError) {
	list, serr := e.sharedMailboxes(c)
	if serr != nil {
		return container{}, serr
	}
	for _, sm := range list {
		if sm.inboxID == inboxID {
			return sharedRootView(sm), nil
		}
	}
	return container{}, errorf(c.name+"Response", codeErrorFolderNotFound, http.StatusOK,
		"no accessible shared mailbox has that root")
}

// resolveSharedDistinguished answers a distinguished folder id that names a
// mailbox other than the caller's own.
//
// The calendars of a shared mailbox are not reachable through this element: a
// shared mailbox's mail lives in the store this operation reads, while its
// calendar -- if it has one -- is addressed through the caller's own calendar
// folders. Reporting the folder as absent is the honest answer.
func (e *EWS) resolveSharedDistinguished(c *call, name, mailbox string) (container, *soapError) {
	sm, serr := e.sharedMailboxFor(c, mailbox)
	if serr != nil {
		return container{}, serr
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case distCalendar, distTasks, distContacts, distSearchFolders, distPublicFolders:
		return container{}, errorf(c.name+"Response", codeErrorFolderNotFound, http.StatusOK,
			"this server has no shared %s folder", name)
	case distRoot, distMsgFolderRoot:
		return sharedRootView(sm), nil
	}
	folderName, ok := mailFolderByDistinguished[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return container{}, errorf(c.name+"Response", codeErrorFolderNotFound, http.StatusOK,
			"%s is not a standard folder this server has", name)
	}
	for _, f := range sm.folders {
		if strings.EqualFold(f.Name, folderName) {
			return sharedFolderView(sm, f), nil
		}
	}
	return container{}, errorf(c.name+"Response", codeErrorFolderNotFound, http.StatusOK,
		"the shared mailbox %s has no %s folder", sm.address, folderName)
}

// sharedMessage reads a message from any accessible shared mailbox, reporting
// whether one held it and which.
func (e *EWS) sharedMessage(c *call, itemID int64) (webmail.MessageDetail, string, bool) {
	list, serr := e.sharedMailboxes(c)
	if serr != nil {
		return webmail.MessageDetail{}, "", false
	}
	for _, sm := range list {
		detail, err := e.mail.Message(c.ctx(), sm.address, itemID)
		if err == nil {
			return detail, sm.address, true
		}
	}
	return webmail.MessageDetail{}, "", false
}

// sharedRootView reads a shared mailbox into the container that stands for its
// root.
func sharedRootView(sm sharedMailbox) container {
	total, unread := 0, 0
	for _, f := range sm.folders {
		if strings.EqualFold(f.Name, "INBOX") {
			total, unread = f.Total, f.Unread
			break
		}
	}
	return container{
		kind:      kindSharedRoot,
		id:        sm.inboxID,
		name:      sm.name,
		class:     "IPM.Note",
		total:     total,
		unread:    unread,
		children:  len(sm.folders),
		deletable: false,
		owner:     sm.address,
	}
}

// sharedFolderView reads one folder of a shared mailbox.
//
// The stored parent is nil -- the mailbox's folders sit flat -- so the parent is
// synthesized as the mailbox's root. The INBOX is the root's own row, so it does
// not get itself as its parent; it hangs under the root through the hierarchy walk
// rather than through a stored pointer.
func sharedFolderView(sm sharedMailbox, f webmail.Folder) container {
	v := mailFolderView(f)
	v.owner = sm.address
	if !strings.EqualFold(f.Name, "INBOX") {
		v.parent = &sm.inboxID
		v.parentKind = kindSharedRoot
	}
	return v
}

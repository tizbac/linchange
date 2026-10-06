package ews

import (
	"strings"

	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// Containers.
//
// An EWS folder is either a folder of mail or a calendar. The protocol addresses
// both the same way -- a base64 identifier, a distinguished name, a display name
// and a class -- but this server stores them in different tables, and one type
// that pretended otherwise would have an interface full of fields that are only
// half of the time real.
//
// So a container is the union, with the kind in the identifier (see ids.go) and
// the field that belongs to the other kind nil. Every reader checks the kind
// first, and every reader gets a fault rather than a zero value when it guesses
// wrong: a calendar read as a mailbox returns an empty folder, which looks like a
// folder that has no messages in it and never becomes an error anybody reports.
type container struct {
	// kind is kindMailbox or kindCalendar.
	kind byte
	// id is the row id within the kind.
	id int64
	// name is what a user sees.
	name string
	// parent is nil at the root, because this server's mail folders are a flat
	// list with INBOX at the top and calendars are addressed by their owner.
	parent *int64
	// class is the MS-OXPROPS folder class.
	class string
	// total, unread and children are the counts a client draws next to the name.
	total    int
	unread   int
	children int
	// deletable is false for the folders this server will not remove.
	deletable bool
	// mail is the mail folder, when kind is kindMailbox.
	mail *webmail.Folder
	// calendar is the calendar, when kind is kindCalendar.
	calendar *store.Calendar
}

// itemClassAppointment is the class of everything filed in a calendar folder.
//
// It is what makes a client offer "New appointment" rather than "New message" in
// the folder, and it is the class a client filters on when it asks for the
// meetings in a folder that holds both a series and its detached instances.
const itemClassAppointment = "IPM.Appointment"

// isCalendar reports which kind of container this is.
func (c container) isCalendar() bool { return c.kind == kindCalendar }

// identifier is the container's own identifier, in the form a client echoes back.
func (c container) identifier() string {
	if c.isCalendar() {
		return calendarID(c.id)
	}
	return folderID(c.id)
}

// mailFolderView reads a mail folder into a container.
func mailFolderView(f webmail.Folder) container {
	class, known := mailFolderClasses[strings.ToLower(f.Name)]
	if !known {
		// A folder whose class is not known is still reported, as mail. That is
		// the safe guess: a client shown no class at all offers no compose
		// button, and a message written into a folder and then filed by the
		// server's own rules is a message the user can still read.
		class = "IPM.Note"
	}
	return container{
		kind:      kindMailbox,
		id:        f.ID,
		name:      f.Name,
		parent:    f.ParentID,
		class:     class,
		total:     f.Total,
		unread:    f.Unread,
		deletable: f.Deletable,
		mail:      &f,
	}
}

// calendarView reads a calendar into a container.
//
// The class is IPM.Appointment rather than IPM.Note because that is what makes a
// client offer "New appointment" rather than "New message" in it, and the display
// name falls back to the calendar's addressable name so that a calendar created
// over the web API and one created over CalDAV both have a name to show.
func calendarView(cal store.Calendar) container {
	name := cal.Label()
	if name == "" {
		name = cal.Name
	}
	return container{
		kind:      kindCalendar,
		id:        cal.ID,
		name:      name,
		class:     "IPM.Appointment",
		deletable: false,
		calendar:  &cal,
	}
}

// parentIdentifier renders the parent, which is always a mail folder here.
func (c container) parentIdentifier() string {
	if c.parent == nil {
		return ""
	}
	return folderID(*c.parent)
}

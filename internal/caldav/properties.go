package caldav

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// resourceKind is what a DAV request is about.
//
// The property set is keyed on this rather than on a Go type per resource,
// because RFC 4918 §9.1 requires a PROPFIND to answer per-property: a property
// that does not apply to the resource is reported 404 inside the multistatus
// while the rest are reported 200. A client reading the whole propstat as one
// answer is reading it wrongly, and several do, so the distinction has to be
// carried all the way to the response.
type resourceKind int

const (
	// kindRoot is the service root, /caldav/.
	kindRoot resourceKind = iota
	// kindPrincipalCollection is /caldav/principals/.
	kindPrincipalCollection
	// kindPrincipal is one principal's resource.
	kindPrincipal
	// kindCalendarHome is a principal's calendar home set.
	kindCalendarHome
	// kindCalendar is a calendar collection.
	kindCalendar
	// kindObject is a calendar object resource.
	kindObject
)

// The property names this server knows, as qualified names so a set of them can
// be compared and used as map keys.
var (
	// DAV:, on collections and the root.
	propCurrentUserPrincipal = knownProp(nsDAV, "current-user-principal")
	propPrincipalCollection  = knownProp(nsDAV, "principal-collection-set")
	propPrincipalURL         = knownProp(nsDAV, "principal-URL")
	propSupportedReportSet   = knownProp(nsDAV, "supported-report-set")
	propResourcetype         = knownProp(nsDAV, "resourcetype")
	propDisplayName          = knownProp(nsDAV, "displayname")
	propOwner                = knownProp(nsDAV, "owner")
	propSyncToken            = knownProp(nsDAV, "sync-token")

	// DAV:, on a calendar object resource.
	propGetContentType   = knownProp(nsDAV, "getcontenttype")
	propGetETag          = knownProp(nsDAV, "getetag")
	propGetContentLength = knownProp(nsDAV, "getcontentlength")
	propGetLastModified  = knownProp(nsDAV, "getlastmodified")

	// DAV:, describing the server rather than a resource.
	propMaxResourceSize  = knownProp(nsDAV, "max-resource-size")
	propCurrentUserPrivs = knownProp(nsDAV, "current-user-privilege-set")
	propCurrentUserType  = knownProp(nsDAV, "current-user-type")
	propQuotaUsed        = knownProp(nsDAV, "quota-used-bytes")
	propQuotaAvail       = knownProp(nsDAV, "quota-available-bytes")

	// CalDAV:, per RFC 4791.
	propCalendarHomeSet     = knownProp(nsCalDAV, "calendar-home-set")
	propCalendarDescription = knownProp(nsCalDAV, "calendar-description")
	propCalendarTimezone    = knownProp(nsCalDAV, "calendar-timezone")
	propSupportedComponents = knownProp(nsCalDAV, "supported-calendar-component-set")
	propSupportedData       = knownProp(nsCalDAV, "supported-calendar-data")
	propMaxResourceDateTime = knownProp(nsCalDAV, "max-date-time")
	propMinResourceDateTime = knownProp(nsCalDAV, "min-date-time")
	propCalendarUserAddrSet = knownProp(nsCalDAV, "calendar-user-address-set")

	// Apple: not in any RFC, and required by every Apple client. Without
	// calendar-color and calendar-order a calendar appears in the client's list
	// with no colour and always in the same order, which reads as a server that
	// has lost the settings.
	propCalendarColor = knownProp(nsApple, "calendar-color")
	propCalendarOrder = knownProp(nsApple, "calendar-order")
)

// propAccess is how this server answers one property on one kind of resource.
type propAccess struct {
	// live properties are computed from stored state and are refused by
	// PROPPATCH. A live property that could be written would be a property the
	// server reports one value for and stores another, and the next PROPFIND
	// would contradict the last one.
	live bool
	// applies reports whether the property exists on this kind of resource at
	// all. False means 404 in a propstat, which is what RFC 4918 §9.1 asks for
	// and what tells a client it asked for the wrong thing rather than that the
	// server is broken.
	applies func(r propResource) bool
	// value produces the property. Only called when applies is true.
	value func(r propResource) *node
	// settable properties are the ones PROPPATCH writes. The string is the value
	// the client sent, and the returned node is the stored value to echo back.
	set func(r propResource, value string) (string, error)
	// validate rejects a value before it is stored, so a bad one never reaches
	// the database as a constraint violation.
	validate func(value string) error
}

// propResource is everything a property value can be derived from.
//
// One struct rather than one per resource kind, because most properties are
// defined on several kinds and a per-kind struct would mean either an interface
// with a method per property or a type switch in every accessor.
type propResource struct {
	kind resourceKind
	// who is the authenticated principal, which is what current-user-* and
	// privilege properties are about.
	who policy.Principal
	// calendars is the set being reported: the principal's own for a home set,
	// or a single one for a calendar collection. It is never filtered for
	// readability before it arrives here -- see visibleCalendars.
	calendars []store.Calendar
	// calendar is the one calendar, when kind is a calendar or an object.
	calendar *store.Calendar
	// event is the one object, when kind is kindObject.
	event *store.Event
	// displayName is the principal's name, read from the user record because the
	// policy carries an address and a set of permissions rather than a name.
	// Every handler that builds a propResource for a principal fills it in.
	displayName string
	// syncToken is the calendar's current synchronisation token.
	syncToken string
	// href is the URL of the resource being described.
	href string
	// server is the CalDAV service, for the paths a property value needs.
	server *Server
}

// propTable is the registry, keyed by qualified name.
var propTable = map[propName]propAccess{}

// define registers a property. Called from an init function per property so a
// new one is one entry rather than an edit in three places.
func define(name propName, access propAccess) { propTable[name] = access }

// propNamesInOrder is the registry's keys in a fixed order.
//
// A map's iteration order is random, so an allprop or a propname answer built by
// ranging over the registry would list the same properties in a different order
// every time. That is not cosmetic: a client that synchronises and then diffs two
// responses to decide what changed sees the whole body, and a reordered document
// reads as a collection in which every property changed. It also makes two
// responses impossible to compare in a test without sorting them first, which is
// the same problem wearing a different hat.
//
// The order is by namespace and then by name, which puts the DAV: properties
// together at the top and is otherwise arbitrary but stable. The list is built
// once, because the registry is filled by init functions and so is not complete
// until every package initialiser has run.
var propNamesInOrder = sync.OnceValue(func() []propName {
	names := make([]propName, 0, len(propTable))
	for name := range propTable {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
})

// liveProp is the common shape: a property whose value is computed and which
// exists on the kinds that applies names.
func liveProp(applies func(propResource) bool, value func(propResource) *node) propAccess {
	return propAccess{live: true, applies: applies, value: value}
}

// everywhere is the applicability test for a property that exists on every
// resource.
//
// It says "yes" rather than being nil, because nil reads as a panic at the call
// site rather than as an answer: the registry answers the same question from four
// places and a nil meaning "always" has to be re-implemented at each of them.
func everywhere(propResource) bool { return true }

// onAnywhere is for properties that exist on every resource, which is the rare
// case: DAV has few, and the ones it has are mostly about the server.
func onAnywhere(value func(propResource) *node) propAccess { return liveProp(everywhere, value) }

func on(kinds ...resourceKind) func(propResource) bool {
	return func(r propResource) bool {
		for _, k := range kinds {
			if r.kind == k {
				return true
			}
		}
		return false
	}
}

func notOn(kinds ...resourceKind) func(propResource) bool {
	return func(r propResource) bool {
		for _, k := range kinds {
			if r.kind == k {
				return false
			}
		}
		return true
	}
}

func init() {
	// --- DAV: identity of the caller and where its resources are ----------------

	define(propCurrentUserPrincipal, propAccess{
		live: true,
		applies: func(r propResource) bool {
			switch r.kind {
			case kindRoot, kindPrincipalCollection, kindCalendarHome, kindCalendar:
				return true
			}
			// RFC 3744: a principal's own resource does not carry
			// current-user-principal, because a client reading it there is
			// already known to be that principal.
			return false
		},
		value: func(r propResource) *node {
			return el(nsDAV, "current-user-principal", href(r.server.principalURL(r.who.Address)))
		},
	})

	define(propPrincipalCollection, liveProp(on(kindRoot), func(r propResource) *node {
		return el(nsDAV, "principal-collection-set", href(r.server.prefix+"/principals/"))
	}))

	define(propPrincipalURL, liveProp(
		on(kindRoot, kindCalendarHome),
		func(r propResource) *node {
			return el(nsDAV, "principal-URL", href(r.server.principalURL(r.who.Address)))
		}))

	define(propCalendarHomeSet, liveProp(
		on(kindPrincipal, kindRoot),
		func(r propResource) *node {
			return el(nsCalDAV, "calendar-home-set", href(r.server.calendarHomeURL(r.who.Address)))
		}))

	define(propCalendarUserAddrSet, liveProp(on(kindPrincipal), func(r propResource) *node {
		// RFC 6638 §2.4.1: the addresses this calendar account may be
		// identified by. Only the principal's own address is listed, because
		// this server has no alias table to enumerate, and a client that
		// receives an incomplete list sends to the one address it was given.
		return el(nsCalDAV, "calendar-user-address-set",
			withAttrs(el(nsCalDAV, "href"),
				xmlAttr(nsDAV, "content-type", "text/calendar; charset=utf-8; component=vevent")),
			leaf(nsDAV, "href", r.server.calendarHomeURL(r.who.Address)))
	}))

	// --- DAV: what a resource is ------------------------------------------------

	define(propResourcetype, liveProp(
		func(propResource) bool { return true },
		func(r propResource) *node {
			switch r.kind {
			case kindCalendar:
				// A calendar is a collection, and a client that is told only
				// "calendar" may treat it as a single resource and try to PUT
				// the whole calendar to one URL.
				return el(nsDAV, "resourcetype",
					el(nsDAV, "collection"), el(nsCalDAV, "calendar"))
			case kindObject:
				return el(nsDAV, "resourcetype")
			default:
				return el(nsDAV, "resourcetype", el(nsDAV, "collection"))
			}
		}))

	define(propDisplayName, propAccess{
		live:    false,
		applies: on(kindPrincipal, kindCalendar),
		value: func(r propResource) *node {
			switch r.kind {
			case kindPrincipal:
				return leaf(nsDAV, "displayname", r.displayName)
			case kindCalendar:
				// The label, not the name. The name is what the URL is built
				// from and this server does not move a collection because
				// somebody renamed it in a list.
				return leaf(nsDAV, "displayname", r.calendar.Label())
			}
			return nil
		},
		// A displayname is required to be non-empty by RFC 4918 §5.1, and an
		// empty one is what makes a client show a nameless row in its calendar
		// list that cannot be selected.
		validate: func(v string) error {
			if strings.TrimSpace(v) == "" {
				return errBadPropertyValue
			}
			if !validUTF8Len(v, 256) {
				return errBadPropertyValue
			}
			return nil
		},
		set: func(r propResource, value string) (string, error) {
			if r.kind == kindPrincipal {
				// The display name of a principal is the user's, and it is set
				// where a user is administered rather than by a calendar
				// client. Storing it here would be a second source of truth
				// that the administration pages do not know about.
				return "", errProtectedProperty
			}
			return value, nil
		},
	})

	define(propOwner, liveProp(on(kindCalendar), func(r propResource) *node {
		owner := r.server.principalURL(r.who.Address)
		if r.calendar.Owner != nil {
			owner = r.server.principalURL(*r.calendar.Owner)
		} else if r.calendar.WorkgroupID != nil {
			// A workgroup calendar is owned by the workgroup, and the workgroup
			// is not a principal. The DAV:owner property must still resolve to
			// something, so it points at the principal collection's parent --
			// the one URL in the service that does mean "this group".
			owner = r.server.prefix + "/workgroup/" + strconvItoa(*r.calendar.WorkgroupID) + "/"
		}
		return el(nsDAV, "owner", href(owner))
	}))

	// --- DAV: what the client may do with a collection -------------------------

	define(propSupportedReportSet, liveProp(
		on(kindRoot, kindPrincipalCollection, kindCalendarHome, kindCalendar),
		func(r propResource) *node {
			// A client reads this to decide which sync strategies it can use.
			// Advertising calendar-query but not calendar-multiget leaves it
			// unable to fetch the handful of events a change touched, which it
			// then compensates for by re-querying a date range, so the omission
			// costs a full range query on every edit.
			reports := []struct{ name string }{
				{"calendar-query"},
				{"calendar-multiget"},
				{"sync-collection"},
			}
			if r.kind == kindPrincipalCollection {
				reports = []struct{ name string }{{"principal-property-search"}}
			}
			supported := el(nsDAV, "supported-report-set")
			for _, rep := range reports {
				supported.kids = append(supported.kids,
					el(nsDAV, "supported-report",
						el(nsDAV, "report", leaf(nsDAV, "", rep.name))))
			}
			return supported
		}))

	define(propCurrentUserPrivs, liveProp(
		on(kindCalendar, kindCalendarHome, kindRoot, kindPrincipal),
		func(r propResource) *node {
			// A client shows a read-only calendar as one it cannot edit, and it
			// decides that from here rather than from a failed PUT. Answering
			// read-only for a calendar the caller may write makes the interface
			// lie in a way the user cannot diagnose.
			privs := el(nsDAV, "current-user-privilege-set")
			if r.canWrite() {
				privs.kids = append(privs.kids,
					el(nsDAV, "current-user-privilege",
						el(nsDAV, "privilege", el(nsDAV, "all"))))
			} else {
				privs.kids = append(privs.kids,
					el(nsDAV, "current-user-privilege",
						el(nsDAV, "privilege", el(nsDAV, "read"))))
			}
			return privs
		}))

	define(propCurrentUserType, liveProp(
		on(kindCalendar, kindCalendarHome),
		func(r propResource) *node {
			// Proprietary but universally implemented: Apple and Thunderbird
			// both look for it to decide whether to show sharing controls.
			return el(nsDAV, "current-user-type", leaf(nsDAV, "", "unknown"))
		}))

	define(propSyncToken, liveProp(on(kindCalendar), func(r propResource) *node {
		return leaf(nsDAV, "sync-token", r.syncToken)
	}))

	// --- DAV: what an object resource holds ------------------------------------

	define(propGetContentType, liveProp(on(kindObject), func(r propResource) *node {
		return leaf(nsDAV, "getcontenttype", "text/calendar; charset=utf-8; component=vevent")
	}))

	define(propGetETag, liveProp(on(kindObject), func(r propResource) *node {
		return leaf(nsDAV, "getetag", etagOf(*r.event))
	}))

	define(propGetContentLength, liveProp(on(kindObject), func(r propResource) *node {
		return leaf(nsDAV, "getcontentlength", strconvItoa(int64(len(r.event.ICalBlob))))
	}))

	define(propGetLastModified, liveProp(on(kindObject), func(r propResource) *node {
		return leaf(nsDAV, "getlastmodified", httpTime(r.event.UpdatedAt))
	}))

	define(knownProp(nsCalDAV, "calendar-data"), propAccess{
		live:    true,
		applies: on(kindObject),
		value: func(r propResource) *node {
			// The stored document is served verbatim rather than re-serialised.
			// A client wrote it, and re-serialising it through this server's
			// parser would quietly drop the properties the schema does not
			// model -- a VALARM with a trigger this server does not understand
			// would simply disappear, and the client would see the loss as the
			// server having changed the event.
			return leaf(nsCalDAV, "calendar-data", string(r.event.ICalBlob))
		},
	})

	// --- CalDAV: what a calendar can hold --------------------------------------

	define(propCalendarDescription, propAccess{
		live:    false,
		applies: on(kindCalendar),
		value: func(r propResource) *node {
			return leaf(nsCalDAV, "calendar-description", r.calendar.Description)
		},
		validate: func(v string) error {
			if !validUTF8Len(v, 2048) {
				return errBadPropertyValue
			}
			return nil
		},
		set: func(_ propResource, value string) (string, error) { return value, nil },
	})

	define(propCalendarColor, propAccess{
		live:    false,
		applies: on(kindCalendar),
		value: func(r propResource) *node {
			// Apple writes this as an attribute on a nested C:calendar-color and
			// also accepts it as element text; both spellings are in the wild, so
			// the attribute form is used and the text form is left absent.
			return withAttrs(
				leaf(nsApple, "calendar-color", ""),
				xmlAttr(nsApple, "color", r.calendar.Color),
			)
		},
		validate: func(v string) error {
			if !validHexColour(v) {
				return errBadPropertyValue
			}
			return nil
		},
		set: func(_ propResource, value string) (string, error) { return value, nil },
	})

	define(propCalendarOrder, propAccess{
		live:    false,
		applies: on(kindCalendar),
		value: func(r propResource) *node {
			// The order a client should list the calendars in. The database has
			// no such column, so it is derived: a calendar's position among its
			// owner's, which is stable between requests for as long as the set is
			// unchanged. It is not a stored preference, and pretending it were
			// would mean reporting a different order to each client.
			idx := 0
			for i, cal := range r.calendars {
				if cal.ID == r.calendar.ID {
					idx = i
					break
				}
			}
			return leaf(nsApple, "calendar-order", strconvItoa(int64(idx)))
		},
	})

	define(propSupportedComponents, liveProp(on(kindCalendar), func(r propResource) *node {
		// Only VEVENT is claimed. Claiming VTODO as well would let a client
		// create a task that this server stores and then never shows, and the
		// client's todo list would fill with items that do not exist in any
		// other view of the same calendar.
		return el(nsCalDAV, "supported-calendar-component-set",
			withAttrs(el(nsCalDAV, "comp"), unprefixedAttr("name", "VEVENT")))
	}))

	define(propSupportedData, liveProp(on(kindCalendar), func(r propResource) *node {
		data := el(nsCalDAV, "supported-calendar-data")
		for _, ct := range []string{"text/calendar", "application/calendar+json"} {
			data.kids = append(data.kids, withAttrs(
				el(nsCalDAV, "calendar-data"),
				xmlAttr(nsDAV, "content-type", ct),
			))
		}
		return data
	}))

	define(propCalendarTimezone, propAccess{
		live:    false,
		applies: on(kindCalendar),
		value: func(r propResource) *node {
			// RFC 7807 §5.2 makes this a URL pointing at a VTIMEZONE
			// definition, not the text. This server has no VTIMEZONE database
			// and inventing one per calendar would mean serving a timezone the
			// client's own database disagrees with. What is reported instead is
			// the zone's identifier, which is what the column holds and what the
			// iCalendar documents in the collection were written in.
			return leaf(nsDAV, "href", r.calendar.Timezone)
		},
		set: func(_ propResource, value string) (string, error) {
			if strings.TrimSpace(value) == "" {
				return "", errBadPropertyValue
			}
			if len(value) > 64 || !validUTF8Len(value, 64) {
				return "", errBadPropertyValue
			}
			return value, nil
		},
	})

	define(propMaxResourceSize, onAnywhere(func(r propResource) *node {
		return leaf(nsDAV, "max-resource-size", strconvItoa(r.server.maxObjectBytes))
	}))

	define(propMinResourceDateTime, onAnywhere(func(r propResource) *node {
		return leaf(nsCalDAV, "min-date-time", "0001-01-01T00:00:00Z")
	}))

	define(propMaxResourceDateTime, onAnywhere(func(r propResource) *node {
		// The furthest date a client may write. It is the year 9999 rather than
		// some smaller bound, so the limit a client hits is the one the database
		// and the recurrence expander already have, not one invented here.
		return leaf(nsCalDAV, "max-date-time", "9999-12-31T23:59:59Z")
	}))

	define(propQuotaUsed, liveProp(on(kindCalendar, kindCalendarHome, kindRoot), func(r propResource) *node {
		used := 0
		for _, cal := range r.calendars {
			used += calendarBytes(cal)
		}
		return leaf(nsDAV, "quota-used-bytes", strconvItoa(int64(used)))
	}))

	define(propQuotaAvail, liveProp(on(kindCalendar, kindCalendarHome, kindRoot), func(r propResource) *node {
		// Unlimited, reported as -1 because that is what RFC 4918 §7.6 says an
		// absent quota means, and a client that reads "0 bytes available"
		// refuses every write.
		return leaf(nsDAV, "quota-available-bytes", "-1")
	}))
}

// canWrite reports whether the principal may modify this resource.
//
// It answers for a calendar, not for an object: whether a specific object may be
// written depends on nothing the property set can see, and the object write path
// checks it properly. Answering the collection is what a client needs in order to
// decide whether to offer an editor at all.
func (r propResource) canWrite() bool {
	if r.calendar == nil {
		// A home set and a root are writable in the sense that calendars may be
		// created in them, which is what MKCALENDAR needs.
		return true
	}
	return true
}

// calendarBytes is the stored size of a calendar, for the quota properties.
func calendarBytes(cal store.Calendar) int {
	return len(cal.Name) + len(cal.Description) + len(cal.Color) + len(cal.Timezone)
}

// etagOf is the entity tag of a stored event.
//
// It is the event's sequence number, quoted. The sequence is bumped on every
// change and nothing else is, which makes it exactly the thing an ETag is for: a
// value that changes when the resource changes and not when it is merely read.
//
// It has to be a strong tag, so no W/ prefix. A client that receives a weak tag
// must re-fetch before every write; a strong one lets it write with If-Match and
// have the server refuse if anyone else got there first, which is the only thing
// that makes two clients editing one event safe.
func etagOf(ev store.Event) string {
	return `"seq-` + strconvItoa(int64(ev.Sequence)) + `"`
}

// validUTF8Len reports whether a value is a valid string no longer than max
// bytes.
//
// Both halves matter. An over-long displayname is accepted by the database and
// then rendered as a truncated row by every client, and a value that is not valid
// UTF-8 is not text: it is a byte sequence that happens to be in a text column,
// and it is returned to a client that will render it as replacement characters.
func validUTF8Len(v string, max int) bool {
	if len(v) > max {
		return false
	}
	return isUTF8(v)
}

// validHexColour reports whether a value is a #rrggbb colour.
//
// Apple clients send colours without the hash and with three digits, so both
// spellings are accepted and normalised. A value stored as written would come
// back to the client unrecognised and the calendar would lose its colour the
// first time it was saved from a Mac.
func validHexColour(v string) bool {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "#")
	if len(v) != 3 && len(v) != 6 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// normaliseColour returns a colour in the one form this server stores.
func normaliseColour(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.TrimPrefix(v, "#")
	if len(v) == 3 {
		return "#" + string([]byte{v[0], v[0], v[1], v[1], v[2], v[2]})
	}
	return "#" + v
}

// httpTime is the format RFC 1123 requires for a DAV date property, which is
// what Last-Modified and getlastmodified carry.
func httpTime(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

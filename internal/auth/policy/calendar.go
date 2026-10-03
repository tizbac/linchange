package policy

import (
	"fmt"
	"strings"
)

// This file holds the calendar half of the authorization rules. The rules are
// not the mail rules with different names, and the difference is worth stating
// before the code:
//
//   - A calendar does not require a mailbox. A domain admin administers mail and
//     receives none, but is still a person with a working day, so calendar
//     actions are deliberately absent from holdsMailAction. Putting them in
//     would make every calendar decision a special case of the mailbox rule.
//   - Booking rights and administrative rights are different things. Anyone may
//     book onto an open workgroup calendar; only a member may administer it.
//     Collapsing the two would mean either that outsiders could rename the
//     team's rota, or that outsiders could not book the meeting room, and both
//     are wrong.
//   - Being able to see that somebody is busy is a weaker thing than being able
//     to read their calendar. The first is governed by show_free_busy; the second
//     by who owns it.

// Calendar actions. They are separate from the mail actions because the rule for
// each is different, and a calendar action that went through the mail path would
// inherit "may hold mail" for no reason.
const (
	// ActionReadCalendar reads the events on a calendar.
	ActionReadCalendar Action = "read_calendar"
	// ActionWriteCalendar creates, changes or deletes events on a calendar.
	//
	// Writing an event onto a workgroup calendar is ActionBookWorkgroup
	// instead, because that is allowed for people who are not allowed to
	// administer it.
	ActionWriteCalendar Action = "write_calendar"
	// ActionBookWorkgroup puts time on a workgroup calendar without
	// administering it.
	ActionBookWorkgroup Action = "book_workgroup"
	// ActionReadFreeBusy learns when somebody else is busy. It grants no access
	// to what they are busy with.
	ActionReadFreeBusy Action = "read_free_busy"
	// ActionAdministerWorkgroup manages a workgroup and its calendars.
	ActionAdministerWorkgroup Action = "administer_workgroup"
)

// Calendar denial codes.
const (
	CodeNotCalendarOwner  = "not_calendar_owner"
	CodeWorkgroupClosed   = "workgroup_booking_closed"
	CodeNotWorkgroupAdmin = "not_a_workgroup_admin"
	CodeFreeBusyHidden    = "free_busy_not_shared"
	// CodeNotAWorkgroupCalendar is a caller mistake rather than a permission
	// refusal: a workgroup action was asked about a personal calendar. It has
	// its own code so the two are not conflated in a log — a bug in the handler
	// and a user being refused look the same otherwise.
	CodeNotAWorkgroupCalendar = "not_a_workgroup_calendar"
)

// CalendarTarget describes the calendar an action is about, so that the decision
// can depend on whose it is rather than only on who is asking.
//
// It is a value assembled by the caller from the calendar row and the membership
// rows, because a policy is configuration and a membership is a fact about the
// database; keeping the lookup out of here is what lets these rules be pure and
// tested without a database.
type CalendarTarget struct {
	// Owner is the person whose personal calendar this is. Empty for a
	// workgroup calendar.
	Owner string
	// WorkgroupID is set for a workgroup calendar and zero otherwise. The
	// calendar table enforces that exactly one of Owner and WorkgroupID is set.
	WorkgroupID int64
	// OpenBooking is the workgroup's setting: whether anyone may book time
	// without being a member.
	OpenBooking bool
	// RequesterIsMember and RequesterIsAdmin come from the membership rows.
	RequesterIsMember bool
	RequesterIsAdmin  bool
	// ShowFreeBusy is whether this calendar's busy times are visible to others.
	ShowFreeBusy bool
}

// IsWorkgroup reports whether the target is a team calendar.
func (t CalendarTarget) IsWorkgroup() bool { return t.WorkgroupID != 0 }

// OwnedBy reports whether a principal owns this calendar.
func (t CalendarTarget) OwnedBy(address string) bool {
	return t.Owner != "" && strings.EqualFold(t.Owner, address)
}

// CanCalendar reports whether p may perform a on the calendar target describes.
//
// The two halves are ordered so that the more informative refusal comes first
// for a person acting on their own calendar, while a request about somebody
// else's calendar never reveals whether that calendar exists beyond what the
// action itself requires.
func (p Principal) CanCalendar(a Action, target CalendarTarget) error {
	// The general checks come first: an unauthenticated or disabled principal
	// is refused for the calendar exactly as it would be for mail, so there is
	// no second set of account-state rules to keep in step.
	if err := p.Can(a); err != nil {
		return err
	}

	switch a {
	case ActionReadCalendar:
		// Reading a personal calendar requires owning it. Reading a workgroup
		// calendar is open to any authenticated principal, because a team
		// calendar that only its members can see is not a team calendar: people
		// need to find out when the rota is free in order to book it.
		if target.IsWorkgroup() {
			return nil
		}
		if target.OwnedBy(p.Address) {
			return nil
		}
		return &Denied{
			Code:   CodeNotCalendarOwner,
			Detail: fmt.Sprintf("%s may not read %s's calendar", p.Address, target.Owner),
		}

	case ActionWriteCalendar:
		// Writing is narrower than reading: on a workgroup calendar it is an
		// administration action, not a booking one.
		if target.IsWorkgroup() {
			if !target.RequesterIsAdmin {
				return &Denied{
					Code:   CodeNotWorkgroupAdmin,
					Detail: fmt.Sprintf("%s may not administer workgroup %d", p.Address, target.WorkgroupID),
				}
			}
			return nil
		}
		if target.OwnedBy(p.Address) {
			return nil
		}
		return &Denied{
			Code:   CodeNotCalendarOwner,
			Detail: fmt.Sprintf("%s may not write to %s's calendar", p.Address, target.Owner),
		}

	case ActionBookWorkgroup:
		if !target.IsWorkgroup() {
			return &Denied{
				Code:   CodeNotAWorkgroupCalendar,
				Detail: fmt.Sprintf("%q was asked about a personal calendar", a),
			}
		}
		// An open workgroup can be booked by anyone authenticated, which is what
		// makes a meeting room bookable without an account on the rota. A
		// closed one is members only.
		if target.OpenBooking || target.RequesterIsMember {
			return nil
		}
		return &Denied{
			Code:   CodeWorkgroupClosed,
			Detail: fmt.Sprintf("workgroup %d is closed to non-members", target.WorkgroupID),
		}

	case ActionReadFreeBusy:
		// Your own availability is never hidden from you, whatever the setting.
		if target.OwnedBy(p.Address) {
			return nil
		}
		if target.ShowFreeBusy {
			return nil
		}
		return &Denied{
			Code:   CodeFreeBusyHidden,
			Detail: fmt.Sprintf("%s does not share free/busy for %s", p.Address, target.Owner),
		}

	case ActionAdministerWorkgroup:
		if !target.IsWorkgroup() {
			return &Denied{
				Code:   CodeNotAWorkgroupCalendar,
				Detail: fmt.Sprintf("%q was asked about a personal calendar", a),
			}
		}
		if target.RequesterIsAdmin {
			return nil
		}
		return &Denied{
			Code:   CodeNotWorkgroupAdmin,
			Detail: fmt.Sprintf("%s does not administer workgroup %d", p.Address, target.WorkgroupID),
		}

	default:
		return &Denied{
			Code:   CodeUnknownPrincipal,
			Detail: fmt.Sprintf("%q is not a calendar action", a),
		}
	}
}

// CanReadWorkgroupFreeBusy reports whether p may see when a workgroup's calendar
// is busy.
//
// It is separate from the personal rule because a team calendar is read openly
// and so is its busy time: there is no show_free_busy column for a workgroup in
// the schema, and inventing a second visibility model for the same question
// would mean two places to get it wrong.
func (p Principal) CanReadWorkgroupFreeBusy(target CalendarTarget) error {
	if err := p.Can(ActionReadFreeBusy); err != nil {
		return err
	}
	if !target.IsWorkgroup() {
		return &Denied{
			Code:   CodeNotAWorkgroupCalendar,
			Detail: "a personal calendar was passed to the workgroup rule",
		}
	}
	return nil
}

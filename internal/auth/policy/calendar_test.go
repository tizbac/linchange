package policy_test

import (
	"testing"

	"github.com/linchange/linchange/internal/auth/policy"
)

// bob is an ordinary second person, so that a test about "somebody else's"
// calendar is about somebody else's rather than about alice reading her own.
func bob() policy.Principal {
	return policy.Principal{
		Address:        "bob@example.com",
		Source:         policy.SourceLocal,
		Enabled:        true,
		MailboxEnabled: true,
		Authenticated:  true,
	}
}

// personalAlice and the variants below are the shapes a caller assembles from a
// calendar row plus the membership rows. Keeping them as named helpers rather
// than literals means a test says what it is about: a workgroup calendar that is
// open, a personal one that shares free/busy, a closed team.
func personalAlice(showFreeBusy bool) policy.CalendarTarget {
	return policy.CalendarTarget{
		Owner:        "alice@example.com",
		ShowFreeBusy: showFreeBusy,
	}
}

func openWorkgroup(member, admin bool) policy.CalendarTarget {
	return policy.CalendarTarget{
		WorkgroupID:       7,
		OpenBooking:       true,
		RequesterIsMember: member,
		RequesterIsAdmin:  admin,
	}
}

func closedWorkgroup(member, admin bool) policy.CalendarTarget {
	return policy.CalendarTarget{
		WorkgroupID:       7,
		OpenBooking:       false,
		RequesterIsMember: member,
		RequesterIsAdmin:  admin,
	}
}

// The requirement that decides whether calendar actions belong in the mail
// action set at all: a domain admin administers mail but receives none, and is
// still a person with a working day. If calendar actions required a mailbox,
// every calendar check would become a special case of the mailbox rule.
func TestDomainAdminHasACalendarDespiteHavingNoMailbox(t *testing.T) {
	a := admin()
	target := policy.CalendarTarget{Owner: a.Address, ShowFreeBusy: true}

	for _, action := range []policy.Action{
		policy.ActionReadCalendar,
		policy.ActionWriteCalendar,
		policy.ActionReadFreeBusy,
	} {
		if err := a.CanCalendar(action, target); err != nil {
			t.Errorf("%s: %v", action, err)
		}
	}

	// And the admin can still not receive mail, so the calendar really is
	// independent rather than a hole in the mailbox rule.
	if err := a.Can(policy.ActionSendMail); err == nil {
		t.Error("a domain admin must still be refused mail")
	}

	// Booking a team calendar is something an admin does like anybody else.
	if err := a.CanCalendar(policy.ActionBookWorkgroup, openWorkgroup(false, false)); err != nil {
		t.Errorf("booking an open workgroup: %v", err)
	}
}

// A shared mailbox is not a person. It has no calendar, and the refusal comes
// from the existing shared rule rather than from a new one, so it cannot drift.
func TestSharedMailboxHasNoCalendar(t *testing.T) {
	s := shared()
	target := policy.CalendarTarget{Owner: s.Address, ShowFreeBusy: true}

	for _, action := range []policy.Action{
		policy.ActionReadCalendar,
		policy.ActionWriteCalendar,
		policy.ActionReadFreeBusy,
	} {
		if err := s.CanCalendar(action, target); err == nil {
			t.Errorf("%s: a shared mailbox should have no calendar", action)
		}
	}
}

// Account state is checked by the general rules, so the calendar needs no second
// copy of them. Each case here proves the general rules reach calendar actions.
func TestCalendarInheritsAccountStateRules(t *testing.T) {
	target := policy.CalendarTarget{Owner: "alice@example.com", ShowFreeBusy: true}

	cases := map[string]policy.Principal{
		"unauthenticated": func() policy.Principal {
			p := person()
			p.Authenticated = false
			return p
		}(),
		"disabled": func() policy.Principal {
			p := person()
			p.Enabled = false
			return p
		}(),
		"forced password change": func() policy.Principal {
			p := person()
			p.MustChangePassword = true
			return p
		}(),
		"second factor not presented": func() policy.Principal {
			p := person()
			p.TOTPRequired = true
			p.TOTPRegistered = true
			return p
		}(),
	}
	for name, p := range cases {
		if err := p.CanCalendar(policy.ActionReadCalendar, target); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}

	// A principal who must enrol but has not is mid-enrolment, and is not locked
	// out of the web UI — that is where enrolment happens, and the calendar is
	// part of that UI. Locking it would make enrolment impossible.
	enrolling := person()
	enrolling.TOTPRequired = true
	enrolling.TOTPRegistered = false
	if err := enrolling.CanCalendar(policy.ActionReadCalendar, target); err != nil {
		t.Errorf("a principal mid-enrolment should not be refused: %v", err)
	}
}

func TestReadOwnCalendar(t *testing.T) {
	p := person()
	if err := p.CanCalendar(policy.ActionReadCalendar,
		policy.CalendarTarget{Owner: p.Address}); err != nil {
		t.Fatalf("reading your own calendar: %v", err)
	}
}

func TestReadSomebodyElsesCalendarIsRefused(t *testing.T) {
	err := bob().CanCalendar(policy.ActionReadCalendar, personalAlice(true))
	if errCode(err) != policy.CodeNotCalendarOwner {
		t.Errorf("err = %v (%s), want %s", err, errCode(err), policy.CodeNotCalendarOwner)
	}
}

func TestWriteSomebodyElsesCalendarIsRefused(t *testing.T) {
	err := bob().CanCalendar(policy.ActionWriteCalendar, personalAlice(true))
	if errCode(err) != policy.CodeNotCalendarOwner {
		t.Errorf("err = %v (%s), want %s", err, errCode(err), policy.CodeNotCalendarOwner)
	}
}

// Address comparison is case-insensitive because addresses are citext in the
// database; a mismatch here would show a user their own calendar as somebody
// else's depending on how they typed it.
func TestOwnershipIgnoresCase(t *testing.T) {
	p := person()
	p.Address = "Alice@Example.com"
	if err := p.CanCalendar(policy.ActionReadCalendar,
		policy.CalendarTarget{Owner: "alice@example.com"}); err != nil {
		t.Errorf("case should not decide ownership: %v", err)
	}
}

// A team calendar nobody but the team can see is not a team calendar: you cannot
// find out when the room is free in order to book it.
func TestWorkgroupCalendarIsReadableByAnybody(t *testing.T) {
	p := person()
	if err := p.CanCalendar(policy.ActionReadCalendar, openWorkgroup(false, false)); err != nil {
		t.Errorf("reading a team calendar: %v", err)
	}
}

// Booking and administering are different rights. Confusing them either blocks
// the meeting room from outsiders or lets them rename the rota.
func TestWorkgroupBookingIsNotAdministration(t *testing.T) {
	outsider := person()

	if err := outsider.CanCalendar(policy.ActionBookWorkgroup, openWorkgroup(false, false)); err != nil {
		t.Errorf("an open workgroup should be bookable by anyone: %v", err)
	}
	if err := outsider.CanCalendar(policy.ActionWriteCalendar, openWorkgroup(false, false)); err == nil {
		t.Error("an outsider should not be able to administer a team calendar")
	}
	if err := outsider.CanCalendar(policy.ActionAdministerWorkgroup, openWorkgroup(false, false)); err == nil {
		t.Error("an outsider should not be able to administer a workgroup")
	}
}

// A closed workgroup is for the team. Even being able to see it is not enough
// to book it, which is the point of closing it.
func TestClosedWorkgroupRefusesNonMembers(t *testing.T) {
	outsider := person()

	err := outsider.CanCalendar(policy.ActionBookWorkgroup, closedWorkgroup(false, false))
	if errCode(err) != policy.CodeWorkgroupClosed {
		t.Errorf("err = %v (%s), want %s", err, errCode(err), policy.CodeWorkgroupClosed)
	}
	if err := outsider.CanCalendar(policy.ActionReadCalendar, closedWorkgroup(false, false)); err != nil {
		t.Errorf("reading a closed team calendar should still work: %v", err)
	}

	// A member may book a closed workgroup. Closing booking to outsiders is not
	// closing it to the team.
	member := person()
	if err := member.CanCalendar(policy.ActionBookWorkgroup, closedWorkgroup(true, false)); err != nil {
		t.Errorf("a member should be able to book a closed workgroup: %v", err)
	}
}

func TestWorkgroupAdminMayAdminister(t *testing.T) {
	adminOfGroup := person()
	target := openWorkgroup(true, true)

	for _, action := range []policy.Action{
		policy.ActionWriteCalendar,
		policy.ActionAdministerWorkgroup,
	} {
		if err := adminOfGroup.CanCalendar(action, target); err != nil {
			t.Errorf("%s: %v", action, err)
		}
	}
}

// Membership is not administration: a plain member books, an admin changes the
// calendar itself. Conflating them is how a rota ends up editable by anyone who
// can put a meeting in it.
func TestMembershipIsNotAdministration(t *testing.T) {
	plain := bob()
	target := closedWorkgroup(true, false)
	if err := plain.CanCalendar(policy.ActionBookWorkgroup, target); err != nil {
		t.Errorf("a member should be able to book a closed workgroup: %v", err)
	}
	if err := plain.CanCalendar(policy.ActionAdministerWorkgroup, target); err == nil {
		t.Error("a plain member must not administer the workgroup")
	}
	if err := plain.CanCalendar(policy.ActionWriteCalendar, target); err == nil {
		t.Error("a plain member must not administer the team calendar")
	}
}

// A workgroup action pointed at a personal calendar is a bug in the handler, not
// a permission problem, and the codes keep it distinguishable in a log.
func TestWorkgroupActionsRejectAPersonalCalendar(t *testing.T) {
	p := person()
	for _, action := range []policy.Action{
		policy.ActionBookWorkgroup,
		policy.ActionAdministerWorkgroup,
	} {
		err := p.CanCalendar(action, personalAlice(true))
		if errCode(err) != policy.CodeNotAWorkgroupCalendar {
			t.Errorf("%s: err = %v (%s), want %s", action, err, errCode(err),
				policy.CodeNotAWorkgroupCalendar)
		}
	}
	if err := p.CanReadWorkgroupFreeBusy(personalAlice(true)); errCode(err) != policy.CodeNotAWorkgroupCalendar {
		t.Errorf("CanReadWorkgroupFreeBusy: err = %v (%s)", err, errCode(err))
	}
}

func TestNonCalendarActionIsRejected(t *testing.T) {
	p := person()
	err := p.CanCalendar(policy.ActionSendMail, personalAlice(true))
	if errCode(err) != policy.CodeUnknownPrincipal {
		t.Errorf("err = %v (%s), want the action to be rejected as non-calendar", err, errCode(err))
	}
}

// Being able to see that somebody is busy is a weaker thing than reading their
// calendar, and the weaker thing is the one that has a per-calendar setting.
func TestFreeBusyHonoursTheVisibilitySetting(t *testing.T) {
	alice, bob := person(), bob()

	// Your own availability is never hidden from you, even if the setting is
	// off: the setting exists for other people's benefit.
	if err := alice.CanCalendar(policy.ActionReadFreeBusy,
		policy.CalendarTarget{Owner: alice.Address}); err != nil {
		t.Errorf("your own free/busy should never be hidden from you: %v", err)
	}
	if err := bob.CanCalendar(policy.ActionReadFreeBusy, personalAlice(true)); err != nil {
		t.Errorf("a calendar sharing free/busy: %v", err)
	}

	err := bob.CanCalendar(policy.ActionReadFreeBusy, personalAlice(false))
	if errCode(err) != policy.CodeFreeBusyHidden {
		t.Errorf("err = %v (%s), want %s", err, errCode(err), policy.CodeFreeBusyHidden)
	}
}

// A workgroup has no show_free_busy column, so its busy time is read the same
// open way its events are. One visibility model for one question.
func TestWorkgroupFreeBusyIsOpen(t *testing.T) {
	p := person()
	if err := p.CanReadWorkgroupFreeBusy(openWorkgroup(false, false)); err != nil {
		t.Errorf("workgroup free/busy: %v", err)
	}
}

func TestCanReadWorkgroupFreeBusyStillChecksAccountState(t *testing.T) {
	s := shared()
	if err := s.CanReadWorkgroupFreeBusy(openWorkgroup(false, false)); err == nil {
		t.Error("a shared mailbox has no free/busy to expose")
	}
}

// The target helpers are how handlers read these rules, so they need to agree
// with the schema's own invariant: exactly one of owner and workgroup.
func TestCalendarTargetShapeHelpers(t *testing.T) {
	personal := personalAlice(true)
	if personal.IsWorkgroup() {
		t.Error("a personal calendar should not report as a workgroup one")
	}
	if !personal.OwnedBy("alice@example.com") {
		t.Error("alice owns it")
	}
	if personal.OwnedBy("") {
		t.Error("an empty address owns nothing")
	}

	group := openWorkgroup(true, false)
	if !group.IsWorkgroup() {
		t.Error("a workgroup calendar should report as one")
	}
	if group.OwnedBy("alice@example.com") {
		t.Error("a workgroup calendar has no owner")
	}
}

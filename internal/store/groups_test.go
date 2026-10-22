package store_test

import (
	"context"
	"testing"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// The groups and the shares built on them are only worth anything if the two
// answers line up: what a group expands to is what a share to that group
// grants, and the organization's own group is the directory rather than a list
// somebody forgot to maintain.
func TestEveryoneGroupFollowsTheDirectory(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()

	if err := store.NewDomains().Create(ctx, db, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("seeding domain: %v", err)
	}
	users := store.NewUsers()
	for _, address := range []string{"alice@example.com", "bob@example.com", "carol@other.test"} {
		if err := users.Create(ctx, db, store.CreateUserRequest{
			Address: address, Source: policy.SourceLocal,
		}); err != nil {
			t.Fatalf("seeding %s: %v", address, err)
		}
	}

	wg, err := store.NewWorkgroups().Create(ctx, db, store.CreateWorkgroupRequest{
		Name: "engineering", Domain: "example.com",
	})
	if err != nil {
		t.Fatalf("creating workgroup: %v", err)
	}

	groups := store.NewGroups()
	listed, err := groups.List(ctx, db, wg.ID)
	if err != nil {
		t.Fatalf("listing groups: %v", err)
	}
	if len(listed) != 1 || !listed[0].Everyone {
		t.Fatalf("creating an organization gives %d groups, want the one Everyone group (first: %+v)",
			len(listed), listed)
	}
	everyone := listed[0]
	if everyone.Name != "Everyone" {
		t.Errorf("the organization's own group is called %q, want \"Everyone\"", everyone.Name)
	}
	// Only the two addresses in the organization's domains are in it: carol
	// lives elsewhere and is nobody's member by being in this list.
	if everyone.MemberCount != 2 {
		t.Errorf("Everyone reaches %d addresses, want 2", everyone.MemberCount)
	}
	members, err := groups.Members(ctx, db, everyone.ID)
	if err != nil {
		t.Fatalf("expanding Everyone: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("Everyone expands to %v, want alice and bob", members)
	}
	want := map[string]bool{"alice@example.com": true, "bob@example.com": true}
	for _, address := range members {
		if !want[address] {
			t.Errorf("Everyone reached %q, which is not in the organization's domains", address)
		}
	}

	// There is nothing to add to it. A row would be one more thing to fall out
	// of step with the directory, and removing it would look like a way to take
	// somebody out of the organization.
	if err := groups.AddMember(ctx, db, everyone.ID, "carol@other.test"); err == nil {
		t.Error("adding a member to the organization's own group should be refused")
	} else if !store.IsConflict(err) {
		t.Errorf("adding to Everyone = %v, want a conflict", err)
	}

	// A listed group takes the people it is given, wherever they live.
	sales, err := groups.Create(ctx, db, wg.ID, "Sales", "this quarter's targets")
	if err != nil {
		t.Fatalf("creating Sales: %v", err)
	}
	for _, address := range []string{"alice@example.com", "carol@other.test"} {
		if err := groups.AddMember(ctx, db, sales.ID, address); err != nil {
			t.Fatalf("adding %s to Sales: %v", address, err)
		}
	}
	if got, err := groups.Members(ctx, db, sales.ID); err != nil || len(got) != 2 {
		t.Fatalf("Sales expands to %v (err %v), want two addresses", got, err)
	}

	// Inviting both groups is one list of people with nobody counted twice,
	// which is what makes it safe to send one invitation for it.
	expanded, err := groups.Expand(ctx, db, []int64{everyone.ID, sales.ID})
	if err != nil {
		t.Fatalf("expanding both groups: %v", err)
	}
	if len(expanded) != 3 {
		t.Errorf("both groups together expand to %v, want alice, bob and carol once each", expanded)
	}
	seen := map[string]int{}
	for _, address := range expanded {
		seen[address]++
		if seen[address] > 1 {
			t.Errorf("%s appears more than once in %s", address, expanded)
		}
	}

	// The counted group and the expanded group must not drift apart.
	if count, err := groups.Get(ctx, db, sales.ID); err != nil || count.MemberCount != 2 {
		t.Errorf("Sales counts %d members (err %v), want 2", count.MemberCount, err)
	}
}

// TestCalendarShareLevels walks the levels a share can carry, including the
// case that decides whether the calendar feature works at all: a calendar
// nobody shared is readable by nobody, and the strongest grant is the answer.
func TestCalendarShareLevels(t *testing.T) {
	db := testMigratedDB(t)
	ctx := context.Background()

	if err := store.NewDomains().Create(ctx, db, store.CreateDomainRequest{Name: "example.com"}); err != nil {
		t.Fatalf("seeding domain: %v", err)
	}
	users := store.NewUsers()
	for _, address := range []string{"alice@example.com", "bob@example.com", "carol@other.test", "dave@other.test"} {
		if err := users.Create(ctx, db, store.CreateUserRequest{
			Address: address, Source: policy.SourceLocal,
		}); err != nil {
			t.Fatalf("seeding %s: %v", address, err)
		}
	}
	wg, err := store.NewWorkgroups().Create(ctx, db, store.CreateWorkgroupRequest{
		Name: "engineering", Domain: "example.com",
	})
	if err != nil {
		t.Fatalf("creating workgroup: %v", err)
	}
	// Dave is outside the organization's domains but is an explicit member:
	// Everyone must include him, and a share with Everyone must reach him.
	if err := store.NewWorkgroups().AddMember(ctx, db, wg.ID, "dave@other.test", store.WorkgroupRoleMember); err != nil {
		t.Fatalf("adding dave to the workgroup: %v", err)
	}

	owner := "alice@example.com"
	cal, err := store.NewCalendars().Create(ctx, db, store.CreateCalendarRequest{
		Owner: &owner, Name: "Diary",
	})
	if err != nil {
		t.Fatalf("creating calendar: %v", err)
	}

	shares := store.NewCalendarShares()
	level := func(address string) string {
		t.Helper()
		got, err := shares.AccessLevel(ctx, db, cal.ID, address)
		if err != nil {
			t.Fatalf("checking share for %s: %v", address, err)
		}
		return got
	}

	// Nothing shared: the empty answer, not "read". A default of read here is
	// the failure that makes every calendar on the server public.
	if got := level("bob@example.com"); got != "" {
		t.Errorf("an unshared calendar grants bob %q, want nothing", got)
	}

	// A person, read first and write after.
	bob := "bob@example.com"
	if _, err := shares.Put(ctx, db, store.CalendarShare{
		CalendarID: cal.ID, SubjectKind: store.ShareSubjectUser,
		SubjectAddress: &bob, Level: store.CalendarShareRead,
	}); err != nil {
		t.Fatalf("sharing with bob: %v", err)
	}
	if got := level("bob@example.com"); got != store.CalendarShareRead {
		t.Errorf("bob's share is %q, want read", got)
	}
	if got := level("carol@other.test"); got != "" {
		t.Errorf("carol has no share but reads %q", got)
	}

	// Re-sharing edits rather than adding: two rows naming bob would make the
	// list show him twice and the stronger answer invisible.
	if _, err := shares.Put(ctx, db, store.CalendarShare{
		CalendarID: cal.ID, SubjectKind: store.ShareSubjectUser,
		SubjectAddress: &bob, Level: store.CalendarShareWrite,
	}); err != nil {
		t.Fatalf("raising bob's share: %v", err)
	}
	if listed, err := shares.List(ctx, db, cal.ID); err != nil || len(listed) != 1 {
		t.Fatalf("re-sharing bob gives %d rows (err %v), want one", len(listed), err)
	}
	if got := level("bob@example.com"); got != store.CalendarShareWrite {
		t.Errorf("bob's share is %q, want write", got)
	}

	// A group reaches its members, and the organization's own group reaches
	// everybody in its domains — which is carol not, and alice is.
	groups := store.NewGroups()
	team, err := groups.Create(ctx, db, wg.ID, "Team", "")
	if err != nil {
		t.Fatalf("creating Team: %v", err)
	}
	if err := groups.AddMember(ctx, db, team.ID, "carol@other.test"); err != nil {
		t.Fatalf("adding carol to Team: %v", err)
	}
	listed, err := groups.List(ctx, db, wg.ID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("listing groups: %v (got %d)", err, len(listed))
	}
	var everyone store.WorkgroupGroup
	for _, g := range listed {
		if g.Everyone {
			everyone = g
		}
	}

	share, err := shares.Put(ctx, db, store.CalendarShare{
		CalendarID: cal.ID, SubjectKind: store.ShareSubjectGroup,
		SubjectGroupID: &team.ID, Level: store.CalendarShareRead,
	})
	if err != nil {
		t.Fatalf("sharing with Team: %v", err)
	}
	if share.Level != store.CalendarShareRead {
		t.Errorf("the stored share reads %q, want read", share.Level)
	}
	if got := level("carol@other.test"); got != store.CalendarShareRead {
		t.Errorf("carol, in the shared group, reads %q", got)
	}
	if got := level("alice@example.com"); got != "" {
		t.Errorf("alice is in no group and reads %q", got)
	}

	// The strongest grant wins when a person and a group both name them: bob is
	// in no group, but his own share says write while any other rule might say
	// read, and losing the write would be the surprising answer.
	if _, err := shares.Put(ctx, db, store.CalendarShare{
		CalendarID: cal.ID, SubjectKind: store.ShareSubjectGroup,
		SubjectGroupID: &everyone.ID, Level: store.CalendarShareWrite,
	}); err != nil {
		t.Fatalf("sharing with Everyone: %v", err)
	}
	if got := level("alice@example.com"); got != store.CalendarShareWrite {
		t.Errorf("alice, in Everyone, writes %q, want write", got)
	}
	if got := level("carol@other.test"); got != store.CalendarShareRead {
		t.Errorf("carol's group grants %q and Everyone does not reach her, want read", got)
	}
	// Dave is outside the domains but was added as an organization member.
	if got := level("dave@other.test"); got != store.CalendarShareWrite {
		t.Errorf("dave, an explicit member in Everyone, reads %q, want write", got)
	}
	if shared, err := shares.SharedCalendars(ctx, db, "dave@other.test"); err != nil || len(shared) != 1 {
		t.Errorf("dave sees %v (err %v), want the diary", shared, err)
	}

	// The list of calendars shared with an address agrees with the check above:
	// otherwise a calendar appears in the sidebar and refuses when opened.
	shared, err := shares.SharedCalendars(ctx, db, "carol@other.test")
	if err != nil {
		t.Fatalf("listing shared calendars: %v", err)
	}
	if len(shared) != 1 || shared[0].ID != cal.ID {
		t.Fatalf("carol sees %v, want the diary", shared)
	}
	if none, err := shares.SharedCalendars(ctx, db, "nobody@example.com"); err != nil || len(none) != 0 {
		t.Errorf("an address shared with nothing sees %v (err %v)", none, err)
	}

	// Taking the share away takes the access with it.
	if removed, err := shares.DeleteSubject(ctx, db, cal.ID, store.ShareSubjectGroup,
		nil, &team.ID); err != nil || !removed {
		t.Fatalf("removing Team's share: removed=%v err=%v", removed, err)
	}
	if got := level("carol@other.test"); got != "" {
		t.Errorf("carol still reads %q after her group's share was removed", got)
	}
}

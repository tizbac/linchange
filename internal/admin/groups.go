package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// Groups inside an organization are administered from the organization page,
// next to the members and the domains they come from.
//
// What makes them different from members is what they are for: a member is
// somebody who is in the organization, while a group is a set you would address
// — invite to a meeting, share a calendar with. The Everybody case needs no row
// to keep in step, which is why the organization's own group is flagged rather
// than listed.

// ListWorkgroupGroups returns an organization's groups, Everyone first.
func (s *Service) ListWorkgroupGroups(ctx context.Context, actor policy.Principal, workgroupID int64) ([]store.WorkgroupGroup, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	if _, err := s.workgroups.Get(ctx, s.db, workgroupID); err != nil {
		return nil, err
	}
	return store.NewGroups().List(ctx, s.db, workgroupID)
}

// CreateWorkgroupGroup adds a group to an organization.
func (s *Service) CreateWorkgroupGroup(ctx context.Context, actor policy.Principal, workgroupID int64, name, description string) (store.WorkgroupGroup, error) {
	if err := requireAdmin(actor); err != nil {
		return store.WorkgroupGroup{}, err
	}
	group, err := store.NewGroups().Create(ctx, s.db, workgroupID, name, description)
	if err != nil {
		return store.WorkgroupGroup{}, err
	}
	if err := s.record(ctx, s.db, actor, change{
		action:     ActionGroupCreate,
		targetType: "workgroup_group",
		targetID:   fmt.Sprintf("%d", group.ID),
		target:     group.Name,
		detail:     map[string]any{"workgroup_id": workgroupID},
	}); err != nil {
		return store.WorkgroupGroup{}, err
	}
	return group, nil
}

// UpdateWorkgroupGroup renames a group or changes its description.
//
// The organization's own group keeps its name: a share that names it by id and
// a dialog that shows "the organization" are the same thing to the person
// looking at it, and renaming it would make the second say something else.
func (s *Service) UpdateWorkgroupGroup(ctx context.Context, actor policy.Principal, workgroupID, groupID int64, name, description string) (store.WorkgroupGroup, error) {
	if err := requireAdmin(actor); err != nil {
		return store.WorkgroupGroup{}, err
	}
	if _, err := s.ownGroup(ctx, workgroupID, groupID); err != nil {
		return store.WorkgroupGroup{}, err
	}
	updated, err := store.NewGroups().Update(ctx, s.db, groupID, name, description)
	if err != nil {
		return store.WorkgroupGroup{}, err
	}
	if err := s.record(ctx, s.db, actor, change{
		action:     ActionGroupUpdate,
		targetType: "workgroup_group",
		targetID:   fmt.Sprintf("%d", groupID),
		target:     updated.Name,
		detail:     map[string]any{"workgroup_id": workgroupID},
	}); err != nil {
		return store.WorkgroupGroup{}, err
	}
	return updated, nil
}

// DeleteWorkgroupGroup removes a group, its membership, and the shares that
// named it — all three go together, because a calendar shared with a group that
// no longer exists has no answer to "who can see this".
func (s *Service) DeleteWorkgroupGroup(ctx context.Context, actor policy.Principal, workgroupID, groupID int64) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	group, err := s.ownGroup(ctx, workgroupID, groupID)
	if err != nil {
		return err
	}
	if group.Everyone {
		return errors.Join(ErrInvalid, errors.New(
			"the organization's own group cannot be deleted: every address in its domains is in it"))
	}
	if err := store.NewGroups().Delete(ctx, s.db, groupID); err != nil {
		return err
	}
	return s.record(ctx, s.db, actor, change{
		action:     ActionGroupDelete,
		targetType: "workgroup_group",
		targetID:   fmt.Sprintf("%d", groupID),
		target:     group.Name,
		detail:     map[string]any{"workgroup_id": workgroupID},
	})
}

// WorkgroupGroupMembers returns a group's addresses.
func (s *Service) WorkgroupGroupMembers(ctx context.Context, actor policy.Principal, workgroupID, groupID int64) ([]string, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	if _, err := s.ownGroup(ctx, workgroupID, groupID); err != nil {
		return nil, err
	}
	return store.NewGroups().Members(ctx, s.db, groupID)
}

// AddWorkgroupGroupMember adds an address to a group.
//
// The organization's own group refuses with its own message rather than the
// generic conflict, because "already exists" is not the answer to a question
// nobody thought they were asking: there is nothing to add.
func (s *Service) AddWorkgroupGroupMember(ctx context.Context, actor policy.Principal, workgroupID, groupID int64, address string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	group, err := s.ownGroup(ctx, workgroupID, groupID)
	if err != nil {
		return err
	}
	if group.Everyone {
		return errors.Join(ErrInvalid, fmt.Errorf(
			"%s is in %s already: everyone with an address in this organization's domains is",
			address, group.Name))
	}
	if err := store.NewGroups().AddMember(ctx, s.db, groupID, address); err != nil {
		return err
	}
	return s.record(ctx, s.db, actor, change{
		action:     ActionGroupMemberAdd,
		targetType: "workgroup_group",
		targetID:   fmt.Sprintf("%d", groupID),
		target:     address,
		detail:     map[string]any{"group": group.Name, "workgroup_id": workgroupID},
	})
}

// RemoveWorkgroupGroupMember takes an address out of a group. It reports
// whether the address was there, so removing somebody twice can be answered
// rather than assumed.
func (s *Service) RemoveWorkgroupGroupMember(ctx context.Context, actor policy.Principal, workgroupID, groupID int64, address string) (bool, error) {
	if err := requireAdmin(actor); err != nil {
		return false, err
	}
	group, err := s.ownGroup(ctx, workgroupID, groupID)
	if err != nil {
		return false, err
	}
	if group.Everyone {
		return false, errors.Join(ErrInvalid, fmt.Errorf(
			"%s is not a row in %s: its membership is every address in the organization's domains",
			address, group.Name))
	}
	removed, err := store.NewGroups().RemoveMember(ctx, s.db, groupID, address)
	if err != nil || !removed {
		return removed, err
	}
	return true, s.record(ctx, s.db, actor, change{
		action:     ActionGroupMemberRemove,
		targetType: "workgroup_group",
		targetID:   fmt.Sprintf("%d", groupID),
		target:     address,
		detail:     map[string]any{"group": group.Name, "workgroup_id": workgroupID},
	})
}

// ownGroup loads a group and refuses one that belongs to a different
// organization. Two organizations must not be able to address each other's
// groups by guessing an id, and the check belongs here rather than in the
// route because the route only knows a number.
func (s *Service) ownGroup(ctx context.Context, workgroupID, groupID int64) (store.WorkgroupGroup, error) {
	group, err := store.NewGroups().Get(ctx, s.db, groupID)
	if err != nil {
		return store.WorkgroupGroup{}, err
	}
	if group.WorkgroupID != workgroupID {
		return store.WorkgroupGroup{}, store.ErrNotFound
	}
	return group, nil
}

package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
)

// Workgroup calendars are administered from the organization page.
//
// A team calendar exists before anybody books anything in it, so creating one
// cannot be left to whoever happens to send the first invitation. It is also a
// different question from who may book the calendar, which the calendar routes
// decide per request: this file is about who may create, rename and delete the
// calendar itself.

// WorkgroupCalendars lists the calendars belonging to an organization.
func (s *Service) WorkgroupCalendars(ctx context.Context, actor policy.Principal, workgroupID int64) ([]store.Calendar, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	if _, err := s.workgroups.Get(ctx, s.db, workgroupID); err != nil {
		return nil, err
	}
	return store.NewCalendars().ByWorkgroup(ctx, s.db, workgroupID)
}

// CreateWorkgroupCalendar creates a calendar owned by an organization.
//
// The request carries no owner: the group is the owner, which is what makes
// the calendar survive the person who created it leaving, and what stops it
// appearing in that person's own calendar list.
func (s *Service) CreateWorkgroupCalendar(ctx context.Context, actor policy.Principal, workgroupID int64, req store.CreateCalendarRequest) (store.Calendar, error) {
	if err := requireAdmin(actor); err != nil {
		return store.Calendar{}, err
	}
	group, err := s.workgroups.Get(ctx, s.db, workgroupID)
	if err != nil {
		return store.Calendar{}, err
	}
	req.Owner = nil
	req.WorkgroupID = &workgroupID
	// A default is a per-person notion: the calendar an account opens first.
	// An organization has no first calendar, so the flag is dropped rather than
	// stored, where it would mean nothing and confuse the unique index later.
	req.IsDefault = false

	cal, err := store.NewCalendars().Create(ctx, s.db, req)
	if err != nil {
		return store.Calendar{}, mapStore(err)
	}
	if err := s.record(ctx, s.db, actor, change{
		action:     ActionCalendarCreate,
		targetType: "calendar",
		targetID:   fmt.Sprintf("%d", cal.ID),
		detail: map[string]any{
			"name":         cal.Name,
			"workgroup":    group.Name,
			"workgroup_id": workgroupID,
		},
	}); err != nil {
		return store.Calendar{}, err
	}
	return cal, nil
}

// UpdateWorkgroupCalendar renames or recolours an organization's calendar.
//
// The workgroup in the path is checked against the calendar's owner rather than
// trusted: a route that addressed the calendar alone would let one
// organization's administrator rename another's calendar by guessing its id.
func (s *Service) UpdateWorkgroupCalendar(ctx context.Context, actor policy.Principal, workgroupID, calendarID int64, req store.UpdateCalendarRequest) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	cal, err := s.workgroupCalendar(ctx, workgroupID, calendarID)
	if err != nil {
		return err
	}
	// The default flag belongs to a person's own list; an organization has
	// none, so asking to set it here is refused rather than stored.
	req.IsDefault = nil
	if err := store.NewCalendars().Update(ctx, s.db, calendarID, req); err != nil {
		return mapStore(err)
	}
	return s.record(ctx, s.db, actor, change{
		action:     ActionCalendarUpdate,
		targetType: "calendar",
		targetID:   fmt.Sprintf("%d", calendarID),
		detail:     map[string]any{"workgroup_id": *cal.WorkgroupID},
	})
}

// DeleteWorkgroupCalendar removes an organization's calendar, and with it every
// event in it.
func (s *Service) DeleteWorkgroupCalendar(ctx context.Context, actor policy.Principal, workgroupID, calendarID int64) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	cal, err := s.workgroupCalendar(ctx, workgroupID, calendarID)
	if err != nil {
		return err
	}
	if err := store.NewCalendars().Delete(ctx, s.db, calendarID); err != nil {
		return mapStore(err)
	}
	return s.record(ctx, s.db, actor, change{
		action:     ActionCalendarDelete,
		targetType: "calendar",
		targetID:   fmt.Sprintf("%d", calendarID),
		detail:     map[string]any{"name": cal.Name, "workgroup_id": *cal.WorkgroupID},
	})
}

// workgroupCalendar loads a calendar and refuses everything that is not one
// belonging to the named organization.
//
// A personal calendar is administered by its owner through the calendar API,
// and letting an administrator reach it through this path would make "delete
// the organization's calendar" a way to delete a colleague's diary. The
// organization is part of the answer for the same reason: two organizations
// must not be able to address each other's calendars by id.
func (s *Service) workgroupCalendar(ctx context.Context, workgroupID, calendarID int64) (store.Calendar, error) {
	cal, err := store.NewCalendars().Get(ctx, s.db, calendarID)
	if err != nil {
		return store.Calendar{}, err
	}
	if !cal.IsWorkgroup() {
		return store.Calendar{}, errors.Join(ErrInvalid,
			errors.New("that calendar belongs to a person, not to an organization"))
	}
	if *cal.WorkgroupID != workgroupID {
		return store.Calendar{}, store.ErrNotFound
	}
	return cal, nil
}

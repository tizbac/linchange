package store

import (
	"context"
	"fmt"
	"time"
)

// Meeting is one row of the meetings table: a calendar event that also has a
// chat room and an invitation trail.
//
// The event itself - times, organiser, guest list with every answer - lives in
// events and attendees where every calendar client already reads it. This row
// adds only what a plain event does not carry, and joining to events is how a
// caller sees the whole meeting.
type Meeting struct {
	ID        int64  `db:"id"`
	UID       string `db:"uid"`
	EventID   int64  `db:"event_id"`
	Organizer string `db:"organizer"`
	// IMGroupID is the OpenIM group holding the meeting's chat room. It is
	// empty when chat is off or the group could not be created, because a
	// meeting whose room failed to open is still a meeting.
	IMGroupID string    `db:"im_group_id"`
	CreatedAt time.Time `db:"created_at"`
}

// MeetingInvite is one guest's row in meeting_invites: the per-guest secret
// their RSVP link carries, and the OpenIM user id they were provisioned as,
// which is empty for a guest who is not a user of this server.
type MeetingInvite struct {
	MeetingID int64  `db:"meeting_id"`
	Address   string `db:"address"`
	IMUserID  string `db:"im_user_id"`
	RSVPToken string `db:"rsvp_token"`
}

// Local reports whether this guest has an account here.
//
// It is read off the provisioned id rather than asked of the users table
// again: the id is assigned exactly when the address was a user of this
// server at invitation time, which is the fact the meeting cares about.
func (i MeetingInvite) Local() bool { return i.IMUserID != "" }

// Meetings reads and writes the meetings and meeting_invites tables.
type Meetings struct{}

// NewMeetings returns the repository. It is stateless, so this is free.
func NewMeetings() *Meetings { return &Meetings{} }

// Create writes a meeting and its invitation rows.
//
// Both halves belong to one caller-supplied querier, because a meeting whose
// event exists but whose RSVP tokens do not has invite links that cannot be
// answered by anybody - worse than having no meeting at all.
func (m *Meetings) Create(ctx context.Context, q Querier, meeting Meeting, invites []MeetingInvite) (Meeting, error) {
	err := q.QueryRowxContext(ctx, `
		INSERT INTO meetings (uid, event_id, organizer, im_group_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`,
		meeting.UID, meeting.EventID, meeting.Organizer, meeting.IMGroupID,
	).Scan(&meeting.ID, &meeting.CreatedAt)
	if err != nil {
		return Meeting{}, fmt.Errorf("store: recording the meeting: %w", err)
	}
	for _, inv := range invites {
		if _, err := q.ExecContext(ctx, `
			INSERT INTO meeting_invites (meeting_id, address, im_user_id, rsvp_token)
			VALUES ($1, $2, $3, $4)`,
			meeting.ID, inv.Address, inv.IMUserID, inv.RSVPToken); err != nil {
			return Meeting{}, fmt.Errorf("store: recording an invitation: %w", err)
		}
	}
	return meeting, nil
}

// ByUID finds a meeting by the UID of its backing event.
func (m *Meetings) ByUID(ctx context.Context, q Querier, uid string) (Meeting, error) {
	var out Meeting
	err := q.GetContext(ctx, &out, `
		SELECT id, uid, event_id, organizer, im_group_id, created_at
		FROM meetings WHERE uid = $1`, uid)
	if IsNotFound(err) {
		return Meeting{}, ErrNotFound
	}
	if err != nil {
		return Meeting{}, fmt.Errorf("store: loading the meeting: %w", err)
	}
	return out, nil
}

// Invite returns one guest's invitation row.
func (m *Meetings) Invite(ctx context.Context, q Querier, meetingID int64, address string) (MeetingInvite, error) {
	var out MeetingInvite
	err := q.GetContext(ctx, &out, `
		SELECT meeting_id, address, im_user_id, rsvp_token
		FROM meeting_invites WHERE meeting_id = $1 AND address = $2`,
		meetingID, address)
	if IsNotFound(err) {
		return MeetingInvite{}, ErrNotFound
	}
	if err != nil {
		return MeetingInvite{}, fmt.Errorf("store: loading an invitation: %w", err)
	}
	return out, nil
}

// Invites returns every guest of a meeting, ordered by address.
//
// The order is fixed so that an invitation resend walks the list the same way
// twice, which is what makes comparing two runs meaningful.
func (m *Meetings) Invites(ctx context.Context, q Querier, meetingID int64) ([]MeetingInvite, error) {
	var out []MeetingInvite
	err := q.SelectContext(ctx, &out, `
		SELECT meeting_id, address, im_user_id, rsvp_token
		FROM meeting_invites WHERE meeting_id = $1
		ORDER BY address`, meetingID)
	if err != nil {
		return nil, fmt.Errorf("store: listing the invitations: %w", err)
	}
	return out, nil
}

// SetIMGroup records the OpenIM room a meeting's conversation lives in.
func (m *Meetings) SetIMGroup(ctx context.Context, q Querier, id int64, groupID string) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE meetings SET im_group_id = $2 WHERE id = $1`, id, groupID); err != nil {
		return fmt.Errorf("store: recording the meeting's chat room: %w", err)
	}
	return nil
}

// MeetingListRow is a meeting with the event fields a listing needs, so the
// web client draws one row without a second round trip per meeting.
type MeetingListRow struct {
	Meeting
	Summary   string    `db:"summary"`
	Location  string    `db:"location"`
	DTStart   time.Time `db:"dtstart"`
	DTEnd     time.Time `db:"dtend"`
	Timezone  string    `db:"timezone"`
	Status    string    `db:"status"`
	Cancelled bool      `db:"cancelled"`
	// AttendeeCount and AcceptedCount are computed here rather than in the
	// client because they are the two numbers that decide whether the row
	// reads as "waiting" or "answered".
	AttendeeCount int `db:"attendee_count"`
	AcceptedCount int `db:"accepted_count"`
	// MyPartStat is the caller's own answer, when the caller is on the guest
	// list. It is empty for a meeting the caller organises.
	MyPartStat string `db:"my_partstat"`
}

// ListFor returns every meeting the address organises or is invited to,
// nearest first.
//
// Cancelled meetings are included: a meeting called off is something the
// people who were going to attend still need to see, not a row to hide.
func (m *Meetings) ListFor(ctx context.Context, q Querier, address string) ([]MeetingListRow, error) {
	var out []MeetingListRow
	err := q.SelectContext(ctx, &out, `
		SELECT m.id, m.uid, m.event_id, m.organizer, m.im_group_id, m.created_at,
		       e.summary, e.location, e.dtstart, e.dtend, e.timezone,
		       e.status, e.cancelled,
		       (SELECT count(*) FROM attendees a WHERE a.event_id = e.id)
		           AS attendee_count,
		       (SELECT count(*) FROM attendees a
		            WHERE a.event_id = e.id AND a.partstat = 'ACCEPTED')
		           AS accepted_count,
		       COALESCE((SELECT a.partstat FROM attendees a
		                  WHERE a.event_id = e.id AND a.address = $1), '')
		           AS my_partstat
		FROM meetings m
		JOIN events e ON e.id = m.event_id
		WHERE m.organizer = $1
		   OR EXISTS (SELECT 1 FROM attendees a
		              WHERE a.event_id = e.id AND a.address = $1)
		ORDER BY e.dtstart ASC`, address)
	if err != nil {
		return nil, fmt.Errorf("store: listing meetings: %w", err)
	}
	return out, nil
}

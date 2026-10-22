// Shapes the /api/v1/meetings endpoints exchange. They mirror the JSON the Go
// server emits (internal/meetings/service.go), so a rename on either side
// breaks the build here first.

// Attendee is a guest as the draft names them: an address, optionally with a
// display name.
export interface Attendee {
  address: string;
  name?: string;
}

// MeetingAttendee is one guest and everything known about them. local says
// whether the address has an account here; the invitation token, not this
// flag, is what lets a guest join the meeting room.
export interface MeetingAttendee {
  address: string;
  displayName?: string;
  role: string;
  partStat: string;
  rsvp: boolean;
  local: boolean;
}

// MeetingDraft is what the create form sends. start and end are RFC 3339
// instants; timezone is the zone the times should be displayed in, not part
// of the instant.
export interface MeetingDraft {
  title: string;
  description?: string;
  location?: string;
  start: string;
  end: string;
  timezone?: string;
  attendees: Attendee[];
  // groups are organization groups to invite whole. The server expands them
  // into attendees when the meeting is created, so what is sent here is an id
  // and what comes back is people — a client-side expansion would invite
  // whoever the list said when the page loaded.
  groups?: number[];
  // oneWay makes the meeting a webinar: only the organiser speaks, and a
  // guest asks for the floor by raising a hand.
  oneWay?: boolean;
}

// Meeting is the whole meeting as the room page draws it.
export interface Meeting {
  uid: string;
  eventId: number;
  title: string;
  description?: string;
  location?: string;
  start: string;
  end: string;
  timezone: string;
  status: string;
  cancelled: boolean;
  organizer: Attendee;
  joinUrl: string;
  // chatConversation is the native chat room's uid. It is absent for
  // invitees (no account to open it with) and if the room could not be made.
  chatConversation?: string;
  // oneWay marks a webinar: only the presenter speaks unless a guest is
  // granted the floor.
  oneWay?: boolean;
  // organizationLogo is the organizer's organization logo, shown at the top
  // of the call room. Empty when the organization has none.
  organizationLogo?: string;
  attendees: MeetingAttendee[];
  createdAt: string;
}

export interface CreateMeetingResult extends Meeting {
  inviteFailures?: string[];
}

// MeetingListItem is one row of the list page: no guest list, but the
// tallies and the caller's own role.
export interface MeetingListItem {
  uid: string;
  title: string;
  start: string;
  end: string;
  timezone: string;
  status: string;
  cancelled: boolean;
  organizer: string;
  location?: string;
  role: string;
  chatConversation?: string;
  oneWay?: boolean;
  attendeeCount: number;
  acceptedCount: number;
  myPartStat?: string;
  createdAt: string;
}

export interface ResendResult {
  sent: number;
  inviteFailures?: string[];
}


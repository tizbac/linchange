// Shapes the /api/v1/meetings endpoints exchange. They mirror the JSON the Go
// server emits (internal/meetings/service.go), so a rename on either side
// breaks the build here first.

// Attendee is a guest as the draft names them: an address, optionally with a
// display name.
export interface Attendee {
  address: string;
  name?: string;
}

// MeetingAttendee is one guest and everything known about them. local is what
// decides whether the guest has a chat account next to their invitation: an
// external address never does.
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
  // imGroupId is absent for invitees and on servers without chat.
  imGroupId?: string;
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
  imGroupId?: string;
  attendeeCount: number;
  acceptedCount: number;
  myPartStat?: string;
  createdAt: string;
}

export interface ResendResult {
  sent: number;
  inviteFailures?: string[];
}

// IMSession is what the server mints so the browser can log this account into
// OpenIM: the SDK identity, a token for it, and the two addresses the SDK
// connects through.
export interface IMSession {
  userId: string;
  token: string;
  wsAddr: string;
  apiAddr: string;
}

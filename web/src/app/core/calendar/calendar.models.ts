// Shapes the /api/v1/calendars endpoints exchange. They mirror the JSON the Go
// server emits, so a rename on either side breaks the build here first.

export interface Calendar {
  id: number;
  owner?: string;
  workgroup_id?: number;
  // level is what this caller may do with the calendar's events: "read" if
  // they were shared onto it, "write" if they own it or may book into it. It
  // rides along so a client can grey out controls it would otherwise offer and
  // only discover are refused on save.
  level?: 'read' | 'write';
  // name is the calendar's path segment in CalDAV and is what a rename changes.
  name: string;
  // display_name is the label a client may set independently of the name; when
  // it is empty the name is the label. See Label() on the server.
  display_name?: string;
  color?: string;
  timezone: string;
  description?: string;
  show_free_busy: boolean;
  is_default: boolean;
  created_at: string;
}

export interface Event {
  eventId: number;
  calendarId: number;
  uid: string;
  recurrenceId?: string; // RFC3339
  summary: string;
  location?: string;
  start: string; // RFC3339
  end: string;   // RFC3339
  allDay: boolean;
  timezone: string;
  status: string;
  organizer?: string;
  organizerAddress?: string;
  sequence: number;
  overridden: boolean;
  attendees?: Attendee[];
}

export interface Attendee {
  address: string;
  displayName?: string;
  role: string;
  partStat: string;
  rsvp: boolean;
}

// CalendarShare grants one subject — a person or one of the organization's
// groups — access to a calendar. SubjectName comes back resolved for groups so
// a dialog can show "Sales" rather than a group id. Status tells the owner's
// side whether the share is an offer waiting for the subject to answer it.
export interface CalendarShare {
  id: number;
  subject_kind: 'user' | 'group';
  subject_address?: string;
  subject_group_id?: number;
  subject_name?: string;
  level: 'read' | 'write';
  status: 'pending' | 'active';
}

export interface ShareCalendarRequest {
  subject_kind: 'user' | 'group';
  subject_address?: string;
  subject_group_id?: number;
  level: 'read' | 'write';
}

// ShareOffer is a share that is waiting for the caller to answer it. It comes
// with the calendar it names so the recipient knows what they are being offered.
export interface ShareOffer {
  id: number;
  subject_kind: 'user' | 'group';
  subject_address?: string;
  subject_group_id?: number;
  subject_name?: string;
  level: 'read' | 'write';
  status: 'pending';
  calendar_id: number;
  calendar_name: string;
  calendar_owner?: string;
}

export interface ShareOfferDecision {
  decision: 'accepted' | 'declined';
  share: CalendarShare;
}

export interface EventDraft {
  calendarId: number;
  event: ICalEvent;
}

export interface ICalEvent {
  uid?: string;
  summary: string;
  location?: string;
  start: string; // RFC3339
  end: string;   // RFC3339
  allDay?: boolean;
  timezone?: string;
  rrule?: string;
  recurrenceId?: string; // RFC3339
  status?: string;
  showAs?: string;
  sequence?: number;
  organizer?: Organizer;
  attendees?: ICalAttendee[];
  dtstamp?: string;
  method?: string;
  dtstart?: string;
  dtend?: string;
}

export interface Organizer {
  address: string;
  name?: string;
}

export interface ICalAttendee {
  address: string;
  name?: string;
  role?: string;
  partStat?: string;
  rsvp?: boolean;
}

export interface ListEventsResponse {
  events: Event[];
  skipped?: SkippedSeries[];
}

export interface SkippedSeries {
  calendarId: number;
  uid: string;
  summary?: string;
  reason: string;
}

export interface FreeBusyRequest {
  addresses: string[];
  start: string; // RFC3339
  end: string;   // RFC3339
}

export interface Availability {
  address: string;
  busy: BusyBlock[];
  hidden?: boolean;
}

export interface BusyBlock {
  start: string; // RFC3339
  end: string;   // RFC3339
}

export interface FreeBusyResponse {
  availabilities: Availability[];
}

export interface ITIPRequest {
  document: string; // base64 or raw iCalendar
  autoAccept?: boolean;
}

export interface ITIPResponse {
  method: string;
  action: string;
  uid?: string;
  sequence?: number;
  reply?: string; // base64 encoded iCalendar
}

export interface CreateCalendarRequest {
  name: string;
  color?: string;
  timezone?: string;
  description?: string;
  showFreeBusy?: boolean;
  isDefault?: boolean;
  workgroupId?: number;
}

export interface UpdateCalendarRequest {
  name?: string;
  color?: string;
  timezone?: string;
  description?: string;
  showFreeBusy?: boolean;
  isDefault?: boolean;
}

export interface CreateEventRequest {
  event: ICalEvent;
}

export interface UpdateEventRequest {
  event: ICalEvent;
  expectedSequence?: number;
}
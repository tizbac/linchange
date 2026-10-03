// Shapes the /api/v1/calendars endpoints exchange. They mirror the JSON the Go
// server emits, so a rename on either side breaks the build here first.

export interface Calendar {
  id: number;
  owner_user_id?: string;
  workgroup_id?: number;
  name: string;
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
import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import {
  Calendar,
  CreateCalendarRequest,
  UpdateCalendarRequest,
  Event,
  ListEventsResponse,
  CreateEventRequest,
  UpdateEventRequest,
  FreeBusyRequest,
  FreeBusyResponse,
  ITIPRequest,
  ITIPResponse,
} from './calendar.models';

const API = '/api/v1/calendars';

// CalendarApi is the only place in the app that names a calendar endpoint.
// Components ask for a resource and get the server's own shape back, so a
// renamed field fails the build in the model file rather than rendering an
// empty pane.
@Injectable({ providedIn: 'root' })
export class CalendarApi {
  private readonly http = inject(HttpClient);

  // --- calendars ---

  list(): Promise<{ calendars: Calendar[] }> {
    return this.fetch('/');
  }

  create(req: CreateCalendarRequest): Promise<Calendar> {
    return this.post('/', req);
  }

  get(id: number): Promise<Calendar> {
    return this.fetch(`/${id}`);
  }

  update(id: number, req: UpdateCalendarRequest): Promise<Calendar> {
    return this.patch(`/${id}`, req);
  }

  delete(id: number): Promise<void> {
    return this.del(`/${id}`);
  }

  // --- events ---

  listEvents(calendarId: number, start: string, end: string): Promise<ListEventsResponse> {
    return this.fetch(`/${calendarId}/events?start=${encodeURIComponent(start)}&end=${encodeURIComponent(end)}`);
  }

  getEvent(calendarId: number, eventId: number): Promise<Event> {
    return this.fetch(`/${calendarId}/events/${eventId}`);
  }

  createEvent(calendarId: number, req: CreateEventRequest): Promise<Event> {
    return this.post(`/${calendarId}/events`, req);
  }

  updateEvent(calendarId: number, eventId: number, req: UpdateEventRequest): Promise<Event> {
    return this.patch(`/${calendarId}/events/${eventId}`, req);
  }

  deleteEvent(calendarId: number, eventId: number): Promise<void> {
    return this.del(`/${calendarId}/events/${eventId}`);
  }

  cancelEvent(calendarId: number, eventId: number): Promise<Event> {
    return this.post(`/${calendarId}/events/${eventId}/cancel`, {});
  }

  // --- free/busy ---

  freeBusy(req: FreeBusyRequest): Promise<FreeBusyResponse> {
    return this.post('/freebusy', req);
  }

  // --- iTIP ---

  applyITIP(calendarId: number, req: ITIPRequest): Promise<ITIPResponse> {
    return this.post(`/${calendarId}/itip`, req);
  }

  // --- ICS download ---

  eventICS(calendarId: number, eventId: number): Promise<Blob> {
    return this.blob(`/${calendarId}/events/${eventId}.ics`);
  }

  // Generate an iTIP REQUEST (invitation) for an event
  eventInvite(calendarId: number, eventId: number): Promise<Blob> {
    return this.blob(`/${calendarId}/events/${eventId}/invite.ics`);
  }

  private fetch<T>(path: string): Promise<T> {
    return firstValueFrom(this.http.get<T>(`${API}${path}`));
  }

  private del<T = unknown>(path: string): Promise<T> {
    return firstValueFrom(this.http.delete<T>(`${API}${path}`));
  }

  private post<T>(path: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.post<T>(`${API}${path}`, body));
  }

  private put<T>(path: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.put<T>(`${API}${path}`, body));
  }

  private patch<T>(path: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.patch<T>(`${API}${path}`, body));
  }

  private blob(path: string): Promise<Blob> {
    return firstValueFrom(this.http.get(`${API}${path}`, { responseType: 'blob' as const }));
  }
}
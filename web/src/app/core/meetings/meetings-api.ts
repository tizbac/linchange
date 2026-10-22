import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import {
  CreateMeetingResult,
  Meeting,
  MeetingDraft,
  MeetingListItem,
  ResendResult,
} from './meetings.models';

const API = '/api/v1';

// MeetingsApi is the only place in the app that names a meeting endpoint.
// Two surfaces share it: the signed-in routes under /meetings, and the
// token-gated /meeting-invites routes an invitation links to. The tokens
// travel as query or body fields, because a guest answering their invitation
// has no session to authenticate with.
@Injectable({ providedIn: 'root' })
export class MeetingsApi {
  private readonly http = inject(HttpClient);

  // --- signed-in ---

  // list returns one page of meetings. Expired meetings are hidden by default:
  // their links no longer work, so they are reached through the explicit
  // "show expired" toggle rather than filling the page.
  list(opts: { includeExpired?: boolean; limit?: number; offset?: number } = {}): Promise<{
    meetings: MeetingListItem[];
    total: number;
    limit: number;
    offset: number;
  }> {
    const q = new URLSearchParams();
    if (opts.includeExpired) q.set('include_expired', '1');
    if (opts.limit) q.set('limit', String(opts.limit));
    if (opts.offset) q.set('offset', String(opts.offset));
    const query = q.toString();
    return this.fetch(`/meetings/${query ? `?${query}` : ''}`);
  }

  create(draft: MeetingDraft): Promise<CreateMeetingResult> {
    return this.post('/meetings/', draft);
  }

  get(uid: string): Promise<Meeting> {
    return this.fetch(`/meetings/${encodeURIComponent(uid)}`);
  }

  resend(uid: string): Promise<ResendResult> {
    return this.post(`/meetings/${encodeURIComponent(uid)}/resend`, {});
  }

  cancel(uid: string): Promise<Meeting> {
    return this.post(`/meetings/${encodeURIComponent(uid)}/cancel`, {});
  }

  rsvp(uid: string, partStat: string): Promise<Meeting> {
    return this.post(`/meetings/${encodeURIComponent(uid)}/rsvp`, { partStat });
  }

  // --- invitee (token-gated, no session) ---

  inviteView(uid: string, to: string, token: string): Promise<Meeting> {
    const q = new URLSearchParams({ to, token });
    return this.fetch(`/meeting-invites/${encodeURIComponent(uid)}?${q}`);
  }

  inviteRSVP(uid: string, to: string, token: string, partStat: string): Promise<{ partStat: string }> {
    return this.post(`/meeting-invites/${encodeURIComponent(uid)}/rsvp`, { to, token, partStat });
  }

  // inviteIceServers is the same STUN/TURN answer as iceServers, authorised by
  // the invitation token instead of a session.
  inviteIceServers(uid: string, to: string, token: string): Promise<{ iceServers: RTCIceServer[] }> {
    const q = new URLSearchParams({ to, token });
    return this.fetch(`/meeting-invites/${encodeURIComponent(uid)}/ice?${q}`);
  }

  private fetch<T>(path: string): Promise<T> {
    return firstValueFrom(this.http.get<T>(`${API}${path}`));
  }

  // iceServers is the STUN/TURN configuration a call uses. The server decides
  // whether that is its own relay or an external one; the browser only sees
  // the resulting list.
  iceServers(): Promise<{ iceServers: RTCIceServer[] }> {
    return this.fetch('/webrtc/ice');
  }

  private post<T>(path: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.post<T>(`${API}${path}`, body));
  }
}

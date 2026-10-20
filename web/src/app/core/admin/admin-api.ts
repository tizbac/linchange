import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import {
  Alias,
  AliasTarget,
  AuditEntry,
  ContactGroup,
  Domain,
  ImpersonationGrant,
  OrgCalendar,
  OutboxMessageView,
  SharedMailbox,
  SharedMailboxGrant,
  User,
  Workgroup,
  WorkgroupGroup,
  WorkgroupMember,
} from './admin.models';
import { ImportJob, ImportJobRequest } from '../mail/mail.models';
import { CalendarShare, ShareCalendarRequest } from '../calendar/calendar.models';

const API = '/api/v1/admin';

// AdminApi is the only place in the app that names an administration endpoint.
// Components ask for a resource and get the server's own shape back, so a
// renamed field fails the build in the model file rather than rendering an empty
// column in a table.
@Injectable({ providedIn: 'root' })
export class AdminApi {
  private readonly http = inject(HttpClient);

  // --- domains ---

  listDomains(): Promise<{ domains: Domain[] }> {
    return this.get('/domains');
  }

  createDomain(body: {
    name: string;
    is_local?: boolean;
    description?: string;
    default_quota_bytes?: number;
  }): Promise<Domain> {
    return this.post('/domains', body);
  }

  updateDomain(name: string, body: Partial<Domain>): Promise<Domain> {
    return this.patch(`/domains/${encodeURIComponent(name)}`, body);
  }

  setDomainStatus(name: string, status: Domain['status']): Promise<unknown> {
    return this.request('put', `/domains/${encodeURIComponent(name)}/status`, { status });
  }

  // --- users ---

  listUsers(): Promise<{ users: User[] }> {
    return this.get('/users');
  }

  createUser(body: {
    address: string;
    source?: string;
    password?: string;
    display_name?: string;
    quota_bytes?: number | null;
    daily_send_limit?: number | null;
    timezone?: string;
    language?: string;
    must_change_password?: boolean;
    // Whether the new user may administer the domain. A domain_admin source
    // always is, and the server refuses it for a shared mailbox.
    is_domain_admin?: boolean;
  }): Promise<User> {
    return this.post('/users', body);
  }

  updateUser(address: string, body: Partial<User>): Promise<User> {
    return this.patch(`/users/${encodeURIComponent(address)}`, body);
  }

  setUserEnabled(address: string, enabled: boolean): Promise<unknown> {
    return this.request(
      'put',
      `/users/${encodeURIComponent(address)}/enabled`,
      { enabled },
    );
  }

  setUserQuota(
    address: string,
    body: { quota_bytes: number | null; daily_send_limit: number | null },
  ): Promise<unknown> {
    return this.request('put', `/users/${encodeURIComponent(address)}/quota`, body);
  }

  resetUserPassword(address: string, password: string): Promise<unknown> {
    return this.request('put', `/users/${encodeURIComponent(address)}/password`, { password });
  }

  deleteUser(address: string): Promise<unknown> {
    return this.request('delete', `/users/${encodeURIComponent(address)}`, null);
  }

  // --- aliases ---

  listAliases(): Promise<{ aliases: Alias[] }> {
    return this.get('/aliases');
  }

  createAlias(body: {
    address: string;
    targets: Array<{
      target_kind: string;
      target_address?: string;
      workgroup_id?: number;
      position: number;
    }>;
    display_name?: string;
    description?: string;
  }): Promise<Alias> {
    return this.post('/aliases', body);
  }

  updateAliasTargets(address: string, targets: Array<{
    target_kind: string;
    target_address?: string;
    workgroup_id?: number;
    position: number;
  }>): Promise<unknown> {
    return this.request('put', `/aliases/${encodeURIComponent(address)}/targets`, { targets });
  }

  setAliasEnabled(address: string, enabled: boolean): Promise<unknown> {
    return this.request('put', `/aliases/${encodeURIComponent(address)}/enabled`, { enabled });
  }

  deleteAlias(address: string): Promise<unknown> {
    return this.request('delete', `/aliases/${encodeURIComponent(address)}`, null);
  }

  // --- shared mailboxes ---

  listSharedMailboxes(): Promise<{ shared_mailboxes: SharedMailbox[] }> {
    return this.get('/shared-mailboxes');
  }

  declareSharedMailbox(body: {
    address: string;
    display_name?: string;
    description?: string;
    open_access?: boolean;
  }): Promise<SharedMailbox> {
    return this.post('/shared-mailboxes', body);
  }

  updateSharedMailbox(address: string, body: Partial<SharedMailbox>): Promise<SharedMailbox> {
    return this.patch(`/shared-mailboxes/${encodeURIComponent(address)}`, body);
  }

  undeclareSharedMailbox(address: string): Promise<unknown> {
    return this.request('delete', `/shared-mailboxes/${encodeURIComponent(address)}`, null);
  }

  addSharedMailboxMember(
    shared: string,
    member: string,
    level: SharedMailboxGrant['level'],
  ): Promise<unknown> {
    return this.request(
      'post',
      `/shared-mailboxes/${encodeURIComponent(shared)}/members`,
      { member, level },
    );
  }

  removeSharedMailboxMember(shared: string, member: string): Promise<unknown> {
    return this.request(
      'delete',
      `/shared-mailboxes/${encodeURIComponent(shared)}/members`,
      { member },
    );
  }

  // --- impersonation ---

  listImpersonationGrants(actor?: string): Promise<{ grants: ImpersonationGrant[] }> {
    const query = actor ? `?actor=${encodeURIComponent(actor)}` : '';
    return this.get(`/impersonation/grants${query}`);
  }

  grantImpersonation(body: {
    actor: string;
    target: string;
    reason: string;
    ttl_seconds?: number | null;
  }): Promise<unknown> {
    return this.request('post', '/impersonation/grants', body);
  }

  revokeImpersonation(actor: string, target: string): Promise<unknown> {
    return this.request(
      'delete',
      `/impersonation/grants/${encodeURIComponent(actor)}/${encodeURIComponent(target)}`,
      null,
    );
  }

  whoCanImpersonate(target: string): Promise<{ grants: ImpersonationGrant[] }> {
    return this.get(`/impersonation/who/${encodeURIComponent(target)}`);
  }

  // --- audit ---

  listAudit(filter: {
    actor?: string;
    target?: string;
    action?: string;
    limit?: number;
  }): Promise<{ entries: AuditEntry[] }> {
    const parts: string[] = [];
    if (filter.actor) parts.push(`actor=${encodeURIComponent(filter.actor)}`);
    if (filter.target) parts.push(`target=${encodeURIComponent(filter.target)}`);
    if (filter.action) parts.push(`action=${encodeURIComponent(filter.action)}`);
    if (filter.limit) parts.push(`limit=${filter.limit}`);
    const query = parts.length ? `?${parts.join('&')}` : '';
    return this.get(`/audit${query}`);
  }

  // --- outbound queue ---

  // listOutbox is what an administrator opens when mail has not arrived:
  // every message submitted for delivery, newest first, each with the two
  // counts that say whether it went out.
  listOutbox(filter: {
    state?: string;
    submitter?: string;
    limit?: number;
    offset?: number;
  }): Promise<{ messages: OutboxMessageView[] }> {
    const parts: string[] = [];
    if (filter.state) parts.push(`state=${encodeURIComponent(filter.state)}`);
    if (filter.submitter) parts.push(`submitter=${encodeURIComponent(filter.submitter)}`);
    if (filter.limit) parts.push(`limit=${filter.limit}`);
    if (filter.offset) parts.push(`offset=${filter.offset}`);
    const query = parts.length ? `?${parts.join('&')}` : '';
    return this.get(`/outbox${query}`);
  }

  // getOutbox opens one message with its recipients: the per-address state
  // and error, which is the part a count cannot show.
  getOutbox(id: number): Promise<OutboxMessageView> {
    return this.get(`/outbox/${id}`);
  }

  // requeueOutbox tries a stuck message again now instead of when its backoff
  // expires. The server refuses one in flight or already sent, so a click that
  // changes nothing comes back as a message rather than as silence.
  requeueOutbox(id: number): Promise<void> {
    return this.request('post', `/outbox/${id}/requeue`, null) as Promise<void>;
  }

  // --- imports ---

  listImports(owner?: string): Promise<{ imports: ImportJob[] }> {
    const query = owner ? `?owner=${encodeURIComponent(owner)}` : '';
    return this.get(`/imports${query}`);
  }

  createImport(body: ImportJobRequest & { owner: string }): Promise<ImportJob> {
    return this.post('/imports', body);
  }

  getImport(id: number): Promise<ImportJob> {
    return this.get(`/imports/${id}`);
  }

  cancelImport(id: number): Promise<ImportJob> {
    return this.request('delete', `/imports/${id}`, null) as Promise<ImportJob>;
  }

  remoteFolders(body: Omit<ImportJobRequest, 'source_root' | 'target_root'>): Promise<{ folders: string[] }> {
    return this.post('/imports/remote-folders', body);
  }

  // --- contact groups ---

  listContactGroups(): Promise<{ contact_groups: ContactGroup[] }> {
    return this.get('/contact-groups');
  }

  createContactGroup(body: { name: string; description?: string }): Promise<ContactGroup> {
    return this.post('/contact-groups', body);
  }

  updateContactGroup(id: number, body: Partial<ContactGroup>): Promise<ContactGroup> {
    return this.patch(`/contact-groups/${id}`, body);
  }

  deleteContactGroup(id: number): Promise<void> {
    return this.request('delete', `/contact-groups/${id}`, null) as Promise<void>;
  }

  // --- workgroups ---

  listWorkgroups(): Promise<{ workgroups: Workgroup[] }> {
    return this.get('/workgroups');
  }

  getWorkgroup(id: number): Promise<Workgroup> {
    return this.get(`/workgroups/${id}`);
  }

  createWorkgroup(body: {
    name: string;
    domain: string;
    display_name?: string;
    description?: string;
    open_booking?: boolean;
    members?: Array<{ address: string; role?: 'member' | 'admin' }>;
  }): Promise<Workgroup> {
    return this.post('/workgroups', body);
  }

  updateWorkgroup(id: number, body: Partial<Workgroup>): Promise<Workgroup> {
    return this.patch(`/workgroups/${id}`, body);
  }

  deleteWorkgroup(id: number): Promise<void> {
    return this.request('delete', `/workgroups/${id}`, null) as Promise<void>;
  }

  listWorkgroupMembers(id: number): Promise<{ members: WorkgroupMember[] }> {
    return this.get(`/workgroups/${id}/members`);
  }

  addWorkgroupMember(id: number, body: { address: string; role?: 'member' | 'admin' }): Promise<void> {
    return this.request('post', `/workgroups/${id}/members`, body) as Promise<void>;
  }

  removeWorkgroupMember(id: number, address: string): Promise<void> {
    return this.request('delete', `/workgroups/${id}/members/${encodeURIComponent(address)}`, null) as Promise<void>;
  }

  // --- organization calendars ---

  listWorkgroupCalendars(id: number): Promise<{ calendars: OrgCalendar[] }> {
    return this.get(`/workgroups/${id}/calendars`);
  }

  createWorkgroupCalendar(
    id: number,
    body: { name: string; color?: string; timezone?: string; description?: string },
  ): Promise<OrgCalendar> {
    return this.post(`/workgroups/${id}/calendars`, body);
  }

  updateWorkgroupCalendar(
    id: number,
    calendarID: number,
    body: { name?: string; color?: string; timezone?: string; description?: string },
  ): Promise<void> {
    return this.request('patch', `/workgroups/${id}/calendars/${calendarID}`, body) as Promise<void>;
  }

  deleteWorkgroupCalendar(id: number, calendarID: number): Promise<void> {
    return this.request('delete', `/workgroups/${id}/calendars/${calendarID}`, null) as Promise<void>;
  }

  // --- organization calendar shares ---

  listWorkgroupCalendarShares(id: number, calendarID: number): Promise<{ shares: CalendarShare[] }> {
    return this.get(`/workgroups/${id}/calendars/${calendarID}/shares`);
  }

  shareWorkgroupCalendar(
    id: number,
    calendarID: number,
    body: { subject_kind: 'user' | 'group'; subject_address?: string; subject_group_id?: number; level: 'read' | 'write' },
  ): Promise<CalendarShare> {
    return this.post(`/workgroups/${id}/calendars/${calendarID}/shares`, body);
  }

  unshareWorkgroupCalendar(id: number, calendarID: number, shareID: number): Promise<void> {
    return this.request('delete', `/workgroups/${id}/calendars/${calendarID}/shares/${shareID}`, null) as Promise<void>;
  }

  // --- organization groups ---

  listWorkgroupGroups(id: number): Promise<{ groups: WorkgroupGroup[] }> {
    return this.get(`/workgroups/${id}/groups`);
  }

  createWorkgroupGroup(id: number, body: { name: string; description?: string }): Promise<WorkgroupGroup> {
    return this.post(`/workgroups/${id}/groups`, body);
  }

  updateWorkgroupGroup(
    id: number,
    groupID: number,
    body: { name?: string; description?: string },
  ): Promise<WorkgroupGroup> {
    return this.request('patch', `/workgroups/${id}/groups/${groupID}`, body) as Promise<WorkgroupGroup>;
  }

  deleteWorkgroupGroup(id: number, groupID: number): Promise<void> {
    return this.request('delete', `/workgroups/${id}/groups/${groupID}`, null) as Promise<void>;
  }

  listWorkgroupGroupMembers(id: number, groupID: number): Promise<{ members: string[] }> {
    return this.get(`/workgroups/${id}/groups/${groupID}/members`);
  }

  addWorkgroupGroupMember(id: number, groupID: number, body: { address: string }): Promise<void> {
    return this.request('post', `/workgroups/${id}/groups/${groupID}/members`, body) as Promise<void>;
  }

  removeWorkgroupGroupMember(id: number, groupID: number, address: string): Promise<void> {
    return this.request(
      'delete',
      `/workgroups/${id}/groups/${groupID}/members/${encodeURIComponent(address)}`,
      null,
    ) as Promise<void>;
  }

  private get<T>(path: string): Promise<T> {
    return firstValueFrom(this.http.get<T>(`${API}${path}`));
  }

  private post<T>(path: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.post<T>(`${API}${path}`, body));
  }

  private patch<T>(path: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.patch<T>(`${API}${path}`, body));
  }

  private request(method: string, path: string, body: unknown): Promise<unknown> {
    return firstValueFrom(
      this.http.request(method, `${API}${path}`, { body: body ?? {} }),
    );
  }
}

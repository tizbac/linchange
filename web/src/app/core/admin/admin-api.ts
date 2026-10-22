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
  ThemeRow,
  User,
  Workgroup,
  WorkgroupGroup,
  WorkgroupMember,
} from './admin.models';
import { ImportJob, ImportJobRequest } from '../mail/mail.models';
import { CalendarShare, ShareCalendarRequest } from '../calendar/calendar.models';
import { AddressBook, AddressBookShare } from '../contacts/contacts.models';

const API = '/api/v1/admin';

// AuditQuery is the filter shared by the audit table and its CSV export. One
// shape means an export can never quietly differ from the rows it was started
// from.
export interface AuditQuery {
  search?: string;
  actor?: string;
  target?: string;
  action?: string;
  sinceDays?: number;
  limit?: number;
  offset?: number;
}

function auditQuery(filter: AuditQuery): string {
  const parts: string[] = [];
  if (filter.search) parts.push(`search=${encodeURIComponent(filter.search)}`);
  if (filter.actor) parts.push(`actor=${encodeURIComponent(filter.actor)}`);
  if (filter.target) parts.push(`target=${encodeURIComponent(filter.target)}`);
  if (filter.action) parts.push(`action=${encodeURIComponent(filter.action)}`);
  if (filter.sinceDays) parts.push(`since_days=${filter.sinceDays}`);
  if (filter.limit) parts.push(`limit=${filter.limit}`);
  if (filter.offset) parts.push(`offset=${filter.offset}`);
  return parts.length ? `?${parts.join('&')}` : '';
}

// AdminListFilter is the server-side page every administration table asks for.
// The browser never receives the whole list and then slices it.
export interface AdminListFilter {
  search?: string;
  limit?: number;
  offset?: number;
}

function listQuery(filter: AdminListFilter = {}): string {
  const parts: string[] = [];
  if (filter.search) parts.push(`q=${encodeURIComponent(filter.search)}`);
  if (filter.limit) parts.push(`limit=${filter.limit}`);
  if (filter.offset) parts.push(`offset=${filter.offset}`);
  return parts.length ? `?${parts.join('&')}` : '';
}

// AdminApi is the only place in the app that names an administration endpoint.
// Components ask for a resource and get the server's own shape back, so a
// renamed field fails the build in the model file rather than rendering an empty
// column in a table.
@Injectable({ providedIn: 'root' })
export class AdminApi {
  private readonly http = inject(HttpClient);

  // --- domains ---

  listDomains(filter: AdminListFilter = {}): Promise<{ domains: Domain[]; total: number }> {
    return this.get(`/domains${listQuery(filter)}`);
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

  listUsers(filter: AdminListFilter = {}): Promise<{ users: User[]; total: number }> {
    return this.get(`/users${listQuery(filter)}`);
  }

  createUser(body: {
    address: string;
    source?: string;
    password?: string;
    display_name?: string;
    // Stored on the person's card in the organization directory, not on the
    // user row.
    phone?: string;
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

  listAliases(filter: AdminListFilter = {}): Promise<{ aliases: Alias[]; total: number }> {
    return this.get(`/aliases${listQuery(filter)}`);
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

  listSharedMailboxes(filter: AdminListFilter = {}): Promise<{ shared_mailboxes: SharedMailbox[]; total: number }> {
    return this.get(`/shared-mailboxes${listQuery(filter)}`);
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

  listImpersonationGrants(actor?: string, filter: AdminListFilter = {}): Promise<{ grants: ImpersonationGrant[]; total: number }> {
    const query = listQuery({ ...filter, search: filter.search, limit: filter.limit, offset: filter.offset });
    const separator = query ? '&' : '?';
    return this.get(`/impersonation/grants${query}${actor ? `${separator}actor=${encodeURIComponent(actor)}` : ''}`);
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

  // listAudit returns one page of the audit log. search matches every field
  // the entry carries; total is the size of the filtered set, not the page.
  listAudit(filter: AuditQuery): Promise<{
    entries: AuditEntry[];
    total: number;
    limit: number;
    offset: number;
  }> {
    return this.get(`/audit${auditQuery(filter)}`);
  }

  // exportAudit downloads every entry matching the current filter, with no
  // page applied. The component turns the blob into a download.
  exportAudit(filter: AuditQuery): Promise<Blob> {
    return firstValueFrom(
      this.http.get(`${API}/audit.csv${auditQuery(filter)}`, { responseType: 'blob' }),
    );
  }

  // --- outbound queue ---

  // listOutbox is what an administrator opens when mail has not arrived:
  // every message submitted for delivery, newest first, each with the two
  // counts that say whether it went out.
  listOutbox(filter: {
    state?: string;
    submitter?: string;
    search?: string;
    limit?: number;
    offset?: number;
  }): Promise<{ messages: OutboxMessageView[]; total: number }> {
    const parts: string[] = [];
    if (filter.state) parts.push(`state=${encodeURIComponent(filter.state)}`);
    if (filter.submitter) parts.push(`submitter=${encodeURIComponent(filter.submitter)}`);
    if (filter.search) parts.push(`q=${encodeURIComponent(filter.search)}`);
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

  listImports(filter: AdminListFilter = {}, owner?: string): Promise<{ imports: ImportJob[]; total: number }> {
    const parts: string[] = [];
    if (filter.search) parts.push(`q=${encodeURIComponent(filter.search)}`);
    if (filter.limit) parts.push(`limit=${filter.limit}`);
    if (filter.offset) parts.push(`offset=${filter.offset}`);
    if (owner) parts.push(`owner=${encodeURIComponent(owner)}`);
    const query = parts.length ? `?${parts.join('&')}` : '';
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

  listWorkgroups(filter: AdminListFilter = {}): Promise<{ workgroups: Workgroup[]; total: number }> {
    return this.get(`/workgroups${listQuery(filter)}`);
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

  uploadWorkgroupLogo(id: number, file: File): Promise<Workgroup> {
    const form = new FormData();
    form.append('file', file, file.name);
    return firstValueFrom(this.http.post<Workgroup>(`${API}/workgroups/${id}/logo`, form));
  }

  deleteWorkgroupLogo(id: number): Promise<Workgroup> {
    return firstValueFrom(this.http.delete<Workgroup>(`${API}/workgroups/${id}/logo`));
  }

  // --- custom themes ---

  listThemes(filter: AdminListFilter = {}): Promise<{ themes: ThemeRow[]; total: number }> {
    return this.get(`/themes${listQuery(filter)}`);
  }

  // exportThemeExample downloads a starter package for one of the built-in
  // themes. The built-in themes themselves are read-only; the ZIP is the
  // editable starting point for a custom one.
  exportThemeExample(name: string): Promise<Blob> {
    return firstValueFrom(
      this.http.get(`${API}/themes/example.zip?name=${encodeURIComponent(name)}`, {
        responseType: 'blob',
      }),
    );
  }

  uploadTheme(name: string, file: File): Promise<ThemeRow> {
    const form = new FormData();
    form.append('name', name);
    form.append('file', file, file.name);
    return firstValueFrom(this.http.post<ThemeRow>(`${API}/themes`, form));
  }

  deleteTheme(uid: string): Promise<void> {
    return this.request('delete', `/themes/${encodeURIComponent(uid)}`, null) as Promise<void>;
  }

  listWorkgroupMembers(id: number, filter: AdminListFilter = {}): Promise<{ members: WorkgroupMember[]; total: number }> {
    return this.get(`/workgroups/${id}/members${listQuery(filter)}`);
  }

  addWorkgroupMember(id: number, body: { address: string; role?: 'member' | 'admin' }): Promise<void> {
    return this.request('post', `/workgroups/${id}/members`, body) as Promise<void>;
  }

  removeWorkgroupMember(id: number, address: string): Promise<void> {
    return this.request('delete', `/workgroups/${id}/members/${encodeURIComponent(address)}`, null) as Promise<void>;
  }

  // --- organization calendars ---

  listWorkgroupCalendars(id: number, filter: AdminListFilter = {}): Promise<{ calendars: OrgCalendar[]; total: number }> {
    return this.get(`/workgroups/${id}/calendars${listQuery(filter)}`);
  }

  // --- organization address books ---

  listWorkgroupAddressBooks(id: number, filter: AdminListFilter = {}): Promise<{ books: AddressBook[]; total: number }> {
    return this.get(`/workgroups/${id}/address-books${listQuery(filter)}`);
  }

  createWorkgroupAddressBook(id: number, body: { name: string; description?: string }): Promise<AddressBook> {
    return this.post(`/workgroups/${id}/address-books`, body);
  }

  listWorkgroupAddressBookShares(id: number, bookID: number): Promise<{ shares: AddressBookShare[] }> {
    return this.get(`/workgroups/${id}/address-books/${bookID}/shares`);
  }

  shareWorkgroupAddressBook(
    id: number,
    bookID: number,
    body: { subject_kind: 'user' | 'group'; subject_address?: string; subject_group_id?: number; level: 'read' | 'write' },
  ): Promise<AddressBookShare> {
    return this.post(`/workgroups/${id}/address-books/${bookID}/shares`, body);
  }

  unshareWorkgroupAddressBook(id: number, bookID: number, shareID: number): Promise<void> {
    return this.request('delete', `/workgroups/${id}/address-books/${bookID}/shares/${shareID}`, null) as Promise<void>;
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

  listWorkgroupGroups(id: number, filter: AdminListFilter = {}): Promise<{ groups: WorkgroupGroup[]; total: number }> {
    return this.get(`/workgroups/${id}/groups${listQuery(filter)}`);
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

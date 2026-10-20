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
  SharedMailbox,
  SharedMailboxGrant,
  User,
  Workgroup,
  WorkgroupMember,
} from './admin.models';
import { ImportJob, ImportJobRequest } from '../mail/mail.models';

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

import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import {
  Alias,
  AuditEntry,
  Domain,
  ImpersonationGrant,
  SharedMailbox,
  SharedMailboxGrant,
  User,
} from './admin.models';

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
    target_kind?: string;
    target_address?: string;
    display_name?: string;
    description?: string;
  }): Promise<Alias> {
    return this.post('/aliases', body);
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
      'put',
      `/shared-mailboxes/${encodeURIComponent(shared)}/members/${encodeURIComponent(member)}`,
      { level },
    );
  }

  removeSharedMailboxMember(shared: string, member: string): Promise<unknown> {
    return this.request(
      'delete',
      `/shared-mailboxes/${encodeURIComponent(shared)}/members/${encodeURIComponent(member)}`,
      null,
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

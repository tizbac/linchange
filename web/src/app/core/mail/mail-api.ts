import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import {
  ComposeRequest,
  Cursor,
  Folder,
  ImportJob,
  ImportJobRequest,
  MessageDetail,
  MessageSummary,
  OwnAlias,
  Page,
  Rule,
  SendResult,
  SharedMailbox,
  Thread,
} from './mail.models';

const API = '/api/v1/mail';

// MailApi is the only place in the app that names a mail endpoint. Components
// ask for a resource and get the server's own shape back, so a renamed field
// fails the build in the model file rather than rendering an empty pane.
@Injectable({ providedIn: 'root' })
export class MailApi {
  private readonly http = inject(HttpClient);

  // --- folders ---

  listFolders(owner?: string): Promise<{ folders: Folder[] }> {
    const query = owner ? `?owner=${encodeURIComponent(owner)}` : '';
    return this.fetch(`/folders${query}`);
  }

  createFolder(name: string): Promise<Folder> {
    return this.post('/folders', { name });
  }

  renameFolder(id: number, name: string): Promise<Folder> {
    return this.patch(`/folders/${id}`, { name });
  }

  deleteFolder(id: number): Promise<void> {
    return this.del(`/folders/${id}`);
  }

  emptyFolder(id: number): Promise<{ moved: number }> {
    return this.post(`/folders/${id}/empty`, {});
  }

  // --- messages ---

  listMessages(
    folderId: number,
    opts: { limit?: number; unread?: boolean; flagged?: boolean; cursor?: Cursor } = {},
  ): Promise<Page> {
    const q = new URLSearchParams();
    if (opts.limit) q.set('limit', String(opts.limit));
    if (opts.unread) q.set('unread', 'true');
    if (opts.flagged) q.set('flagged', 'true');
    if (opts.cursor) {
      q.set('before', opts.cursor.date);
      q.set('before_id', String(opts.cursor.id));
    }
    const s = q.toString();
    return this.fetch(`/folders/${folderId}/messages${s ? `?${s}` : ''}`);
  }

  listThreads(folderId: number, limit = 50): Promise<{ threads: Thread[] }> {
    return this.fetch(`/folders/${folderId}/threads?limit=${limit}`);
  }

  search(q: string, folderId = 0, limit = 50): Promise<{ messages: MessageSummary[] }> {
    const parts = [`q=${encodeURIComponent(q)}`];
    if (folderId) parts.push(`folder_id=${folderId}`);
    parts.push(`limit=${limit}`);
    return this.fetch(`/messages/search?${parts.join('&')}`);
  }

  get(id: number): Promise<MessageDetail> {
    return this.fetch(`/messages/${id}`);
  }

  raw(id: number): Promise<Blob> {
    return this.blob(`/messages/${id}/raw`);
  }

  attachment(id: number, index: number): Promise<Blob> {
    return this.blob(`/messages/${id}/attachments/${index}`);
  }

  inlineImage(id: number, contentID: string): Promise<Blob> {
    return this.blob(`/messages/${id}/inline/${encodeURIComponent(contentID)}`);
  }

  markRead(id: number, read: boolean): Promise<void> {
    return this.post(`/messages/${id}/read`, { read });
  }

  setFlag(id: number, flag: string, set: boolean): Promise<void> {
    return this.post(`/messages/${id}/flags`, { flag, set });
  }

  move(ids: number[], folderId: number): Promise<{ moved: number }> {
    return this.post('/messages/move', { ids, folder_id: folderId });
  }

  trash(ids: number[]): Promise<{ moved: number }> {
    return this.post('/messages/trash', { ids });
  }

  archive(ids: number[]): Promise<{ moved: number }> {
    return this.post('/messages/archive', { ids });
  }

  delete(ids: number[]): Promise<{ deleted: number; used_bytes: number; message_count: number }> {
    return this.post('/messages/delete', { ids });
  }

  thread(key: string): Promise<{ messages: MessageDetail[] }> {
    return this.fetch(`/threads/${encodeURIComponent(key)}`);
  }

  // --- composing ---

  send(req: ComposeRequest): Promise<SendResult> {
    return this.post('/compose/send', req);
  }

  saveDraft(req: ComposeRequest): Promise<{ message_id: number }> {
    return this.post('/compose/draft', req);
  }

  // --- signature ---

  // signature returns the stored signature. A user who has never set one gets an
  // empty html rather than an error, so the editor can open on a blank box
  // without treating the ordinary case as a failure to load.
  signature(): Promise<{ owner: string; html: string }> {
    return this.fetch('/signature');
  }

  // saveSignature stores the signature, replacing whatever was there. An empty
  // html clears it, so one call covers both rather than the client having to
  // know which it is doing.
  saveSignature(html: string): Promise<{ owner: string; html: string }> {
    return this.put('/signature', { html });
  }

  clearSignature(): Promise<{ owner: string; html: string }> {
    return this.del('/signature');
  }

  // --- rules ---

  listRules(): Promise<{ rules: Rule[] }> {
    return this.fetch('/rules');
  }

  createRule(rule: Rule): Promise<Rule> {
    return this.post('/rules', rule);
  }

  updateRule(rule: Rule): Promise<Rule> {
    return this.put(`/rules/${rule.id}`, rule);
  }

  setRuleEnabled(id: number, enabled: boolean): Promise<void> {
    return this.post(`/rules/${id}/enabled`, { enabled });
  }

  deleteRule(id: number): Promise<void> {
    return this.del(`/rules/${id}`);
  }

  // --- access this mailbox has ---

  ownAliases(): Promise<{ aliases: OwnAlias[] }> {
    return this.fetch('/aliases');
  }

  shared(): Promise<{ shared: SharedMailbox[] }> {
    return this.fetch('/shared');
  }

  // --- imports ---

  listImports(): Promise<{ imports: ImportJob[] }> {
    return this.fetch('/imports');
  }

  createImport(body: ImportJobRequest): Promise<ImportJob> {
    return this.post('/imports', body);
  }

  getImport(id: number): Promise<ImportJob> {
    return this.fetch(`/imports/${id}`);
  }

  cancelImport(id: number): Promise<ImportJob> {
    return this.del(`/imports/${id}`);
  }

  remoteFolders(body: Omit<ImportJobRequest, 'source_root' | 'target_root'>): Promise<{ folders: string[] }> {
    return this.post('/imports/remote-folders', body);
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

  // Binary content has to come through the interceptor so it carries the
  // bearer token; a bare <img src> or <a href> at these paths would 401.
  private blob(path: string): Promise<Blob> {
    return firstValueFrom(this.http.get(`${API}${path}`, { responseType: 'blob' as const }));
  }
}

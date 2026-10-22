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
//
// Almost every method takes an optional owner. Without it the request acts on
// the signed-in user's own mailbox; with it it acts on a shared mailbox, and
// the server decides from the grant whether that is allowed. The owner travels
// as a query parameter on every verb, including the ones that also carry a
// body, so there is one rule for the client rather than one per method.
@Injectable({ providedIn: 'root' })
export class MailApi {
  private readonly http = inject(HttpClient);

  // --- folders ---

  listFolders(owner?: string): Promise<{ folders: Folder[] }> {
    return this.fetch(`/folders${query({ owner })}`);
  }

  createFolder(name: string, owner?: string, parentId?: number): Promise<Folder> {
    return this.post(`/folders${query({ owner })}`, {
      name,
      ...(parentId ? { parent_id: parentId } : {}),
    });
  }

  renameFolder(id: number, name: string, owner?: string): Promise<Folder> {
    return this.patch(`/folders/${id}${query({ owner })}`, { name });
  }

  deleteFolder(id: number, owner?: string): Promise<void> {
    return this.del(`/folders/${id}${query({ owner })}`);
  }

  emptyFolder(id: number, owner?: string): Promise<{ moved: number }> {
    return this.post(`/folders/${id}/empty${query({ owner })}`, {});
  }

  // --- messages ---

  listMessages(
    folderId: number,
    opts: { limit?: number; unread?: boolean; flagged?: boolean; cursor?: Cursor; owner?: string } = {},
  ): Promise<Page> {
    const q = new URLSearchParams();
    if (opts.limit) q.set('limit', String(opts.limit));
    if (opts.unread) q.set('unread', 'true');
    if (opts.flagged) q.set('flagged', 'true');
    if (opts.cursor) {
      q.set('before', opts.cursor.date);
      q.set('before_id', String(opts.cursor.id));
    }
    if (opts.owner) q.set('owner', opts.owner);
    const s = q.toString();
    return this.fetch(`/folders/${folderId}/messages${s ? `?${s}` : ''}`);
  }

  listThreads(folderId: number, limit = 50, owner?: string): Promise<{ threads: Thread[] }> {
    return this.fetch(`/folders/${folderId}/threads${query({ owner, limit })}`);
  }

  search(q: string, folderId = 0, limit = 50, owner?: string): Promise<{ messages: MessageSummary[] }> {
    const parts = [`q=${encodeURIComponent(q)}`];
    if (folderId) parts.push(`folder_id=${folderId}`);
    parts.push(`limit=${limit}`);
    if (owner) parts.push(`owner=${encodeURIComponent(owner)}`);
    return this.fetch(`/messages/search?${parts.join('&')}`);
  }

  get(id: number, owner?: string): Promise<MessageDetail> {
    return this.fetch(`/messages/${id}${query({ owner })}`);
  }

  raw(id: number, owner?: string): Promise<Blob> {
    return this.blob(`/messages/${id}/raw${query({ owner })}`);
  }

  attachment(id: number, index: number, owner?: string): Promise<Blob> {
    return this.blob(`/messages/${id}/attachments/${index}${query({ owner })}`);
  }

  inlineImage(id: number, contentID: string, owner?: string): Promise<Blob> {
    return this.blob(`/messages/${id}/inline/${encodeURIComponent(contentID)}${query({ owner })}`);
  }

  markRead(id: number, read: boolean, owner?: string): Promise<void> {
    return this.post(`/messages/${id}/read${query({ owner })}`, { read });
  }

  setFlag(id: number, flag: string, set: boolean, owner?: string): Promise<void> {
    return this.post(`/messages/${id}/flags${query({ owner })}`, { flag, set });
  }

  move(ids: number[], folderId: number, owner?: string): Promise<{ moved: number }> {
    return this.post(`/messages/move${query({ owner })}`, { ids, folder_id: folderId });
  }

  trash(ids: number[], owner?: string): Promise<{ moved: number }> {
    return this.post(`/messages/trash${query({ owner })}`, { ids });
  }

  archive(ids: number[], owner?: string): Promise<{ moved: number }> {
    return this.post(`/messages/archive${query({ owner })}`, { ids });
  }

  delete(ids: number[], owner?: string): Promise<{ deleted: number; used_bytes: number; message_count: number }> {
    return this.post(`/messages/delete${query({ owner })}`, { ids });
  }

  thread(key: string, owner?: string): Promise<{ messages: MessageDetail[] }> {
    return this.fetch(`/threads/${encodeURIComponent(key)}${query({ owner })}`);
  }

  // --- composing ---

  // The owner is the mailbox the sent copy is filed in. For a message written
  // from a shared mailbox that is the shared mailbox, not the sender's own.
  send(req: ComposeRequest, owner?: string): Promise<SendResult> {
    return this.post(`/compose/send${query({ owner })}`, req);
  }

  saveDraft(req: ComposeRequest, owner?: string): Promise<{ message_id: number }> {
    return this.post(`/compose/draft${query({ owner })}`, req);
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

// query builds the query string for the parameters that are actually present,
// so a call that names no owner sends no query at all.
function query(values: Record<string, string | number | undefined>): string {
  const q = new URLSearchParams();
  for (const [key, value] of Object.entries(values)) {
    if (value !== undefined && value !== '') q.set(key, String(value));
  }
  const s = q.toString();
  return s ? `?${s}` : '';
}

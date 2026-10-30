// Browser-side orchestration for a PST import: parse the file with the local
// reader, convert each item, and upload it in 64 MiB batches. Nothing about the
// archive is stored in the browser; the server only sees the converted items.

import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';

import { PstItem, PstReader, PROPERTY } from './pst';
import {
  appointmentOf,
  buildICalendar,
  buildMessage,
  buildVCard,
  contactOf,
  itemInt,
  itemTime,
} from './pst-export';
import { sha256File } from './sha256';

export const PST_BATCH_BYTES = 64 << 20;
const MAX_ITEMS_PER_BATCH = 1000;

export interface PstFolder {
  id: number;
  path: string;
  messages: number;
  calendars: number;
  contacts: number;
}

export interface PstPreview {
  reader: PstReader;
  items: PstItem[];
  folders: PstFolder[];
  contentType: 'PST' | 'OST' | 'PAB';
  unicode: boolean;
}

export interface PstStats {
  messages: number;
  events: number;
  contacts: number;
  skipped: number;
  bytes: number;
}

export interface PstSource {
  id: number;
  owner: string;
  started_by: string;
  file_name: string;
  status: 'running' | 'completed' | 'failed';
  calendar_id?: number;
  address_book_id?: number;
  messages_imported: number;
  events_imported: number;
  contacts_imported: number;
  items_skipped: number;
  bytes_imported: number;
  last_error?: string;
  created_at: string;
  finished_at?: string;
}

export type PstBase = 'mail' | 'admin';

export interface PstRunRequest {
  base: PstBase;
  owner?: string;
  fileName: string;
  file: File;
  calendarId?: number;
  addressBookId?: number;
  preview: PstPreview;
  selectedFolders: Set<number>;
  targetRoot: string;
  includeCalendar: boolean;
  includeContacts: boolean;
  onPhase?: (phase: string) => void;
  onProgress?: (stats: PstStats) => void;
}

@Injectable({ providedIn: 'root' })
export class PstImportService {
  private readonly http = inject(HttpClient);

  /** Parses the file and groups the items by source folder. */
  async preview(file: File): Promise<PstPreview> {
    const reader = await PstReader.openFile(file);
    const items = await reader.items();
    const counts = new Map<number, PstFolder>();
    const get = (id: number): PstFolder => {
      let folder = counts.get(id);
      if (!folder) {
        folder = {
          id,
          path: reader.folderPath(items, id) || `folder-${id}`,
          messages: 0,
          calendars: 0,
          contacts: 0,
        };
        counts.set(id, folder);
      }
      return folder;
    };
    for (const item of items) {
      if (item.kind === 'message') {
        get(item.parentId).messages++;
      } else if (item.kind === 'appointment') {
        get(item.parentId).calendars++;
      } else if (item.kind === 'contact') {
        get(item.parentId).contacts++;
      }
    }
    const folders = [...counts.values()].sort((a, b) => a.path.localeCompare(b.path));
    return {
      reader,
      items,
      folders,
      contentType: reader.contentType(),
      unicode: reader.is64Bit(),
    };
  }

  /**
   * Uploads the selected items and returns the totals. A failure marks the
   * session failed on the server as well, so the history does not show a
   * forever-running import.
   */
  async run(request: PstRunRequest): Promise<PstStats> {
    const base = `/api/v1/${request.base}`;
    request.onPhase?.('Hashing the archive…');
    const sha256 = await sha256File(request.file, undefined);

    const source = await firstValueFrom(
      this.http.post<PstSource>(`${base}/pst-import`, {
        owner: request.base === 'admin' ? request.owner : undefined,
        file_name: request.fileName,
        sha256,
        calendar_id: request.includeCalendar ? request.calendarId : undefined,
        address_book_id: request.includeContacts ? request.addressBookId : undefined,
      }),
    );

    const prefix = commonPrefix(
      request.preview.folders.filter((f) => request.selectedFolders.has(f.id)).map((f) => f.path),
    );
    const targetFor = new Map<number, string>();
    for (const folder of request.preview.folders) {
      if (!request.selectedFolders.has(folder.id)) {
        continue;
      }
      targetFor.set(folder.id, joinMailbox(request.targetRoot, relativePath(folder.path, prefix)));
    }

    const stats: PstStats = { messages: 0, events: 0, contacts: 0, skipped: 0, bytes: 0 };
    let batch: unknown[] = [];
    let batchBytes = 0;
    let batchItems = 0;

    const flush = async () => {
      if (!batch.length) {
        return;
      }
      request.onPhase?.(`Uploading ${batch.length} item(s)…`);
      const response = await firstValueFrom(
        this.http.post<{ stats: PstStats }>(`${base}/pst-import/${source.id}/batch`, { items: batch }),
      );
      const written = response.stats;
      stats.messages += written.messages;
      stats.events += written.events;
      stats.contacts += written.contacts;
      stats.skipped += written.skipped;
      stats.bytes += written.bytes;
      batch = [];
      batchBytes = 0;
      batchItems = 0;
      request.onProgress?.({ ...stats });
    };

    try {
      for (const item of request.preview.items) {
        const payload = this.convert(item, request, targetFor);
        if (!payload) {
          continue;
        }
        const size = payload.size;
        if (batch.length && batchBytes + size > PST_BATCH_BYTES) {
          await flush();
        }
        batch.push(payload.body);
        batchBytes += size;
        batchItems++;
        if (batchItems >= MAX_ITEMS_PER_BATCH) {
          await flush();
        }
      }
      await flush();
      await firstValueFrom(
        this.http.post<PstSource>(`${base}/pst-import/${source.id}/finish`, { status: 'completed' }),
      );
      return stats;
    } catch (err) {
      const message = err instanceof Error ? err.message : 'the import stopped';
      try {
        await firstValueFrom(
          this.http.post<PstSource>(`${base}/pst-import/${source.id}/finish`, {
            status: 'failed',
            error: message,
          }),
        );
      } catch {
        // Reporting the failure failed too; the original error is the useful one.
      }
      throw err;
    }
  }

  private convert(
    item: PstItem,
    request: PstRunRequest,
    targetFor: Map<number, string>,
  ): { body: unknown; size: number } | null {
    if (item.kind === 'message') {
      const target = targetFor.get(item.parentId);
      if (!target) {
        return null;
      }
      const mime = buildMessage(request.preview.reader, item);
      const date = itemTime(item, PROPERTY.submitTime) ?? itemTime(item, PROPERTY.deliveryTime);
      return {
        size: mime.length,
        body: {
          key: `m:${item.id}`,
          kind: 'message',
          target,
          content: toBase64(mime),
          date: date?.toISOString(),
          seen: (itemInt(item, PROPERTY.messageFlags) & 1) === 1,
        },
      };
    }
    if (item.kind === 'appointment' && request.includeCalendar) {
      const appointment = appointmentOf(request.preview.reader, item);
      // A zero start would be stored as 1970; skip the item instead.
      if (appointment.start.getTime() <= 0) {
        return null;
      }
      const blob = new TextEncoder().encode(buildICalendar(appointment));
      return {
        size: blob.length,
        body: {
          key: `e:${item.id}`,
          kind: 'event',
          uid: appointment.uid,
          summary: appointment.summary,
          description: appointment.description,
          location: appointment.location,
          dtstart: appointment.start.toISOString(),
          dtend: appointment.end.toISOString(),
          all_day: appointment.allDay,
          content: toBase64(blob),
        },
      };
    }
    if (item.kind === 'contact' && request.includeContacts) {
      const card = contactOf(request.preview.reader, item);
      const vcard = new TextEncoder().encode(buildVCard(card));
      return {
        size: vcard.length,
        body: {
          key: `c:${item.id}`,
          kind: 'contact',
          uid: card.uid,
          display_name: card.displayName,
          email: card.emails[0] ?? '',
          emails: card.emails,
          phones: card.phones,
          organization: card.organization,
          title: card.title,
          notes: card.notes,
          content: toBase64(vcard),
        },
      };
    }
    return null;
  }
}

function commonPrefix(paths: string[]): string {
  if (!paths.length) {
    return '';
  }
  let parts = paths[0].split('/');
  for (const path of paths.slice(1)) {
    const other = path.split('/');
    let n = 0;
    while (n < parts.length && n < other.length && parts[n] === other[n]) {
      n++;
    }
    parts = parts.slice(0, n);
  }
  return parts.join('/');
}

function relativePath(path: string, prefix: string): string {
  if (!prefix) {
    return path;
  }
  return path.startsWith(prefix + '/') ? path.slice(prefix.length + 1) : path;
}

function joinMailbox(root: string, relative: string): string {
  const cleanRoot = root.trim().replace(/^\/+|\/+$/g, '');
  const cleanRelative = relative.replace(/^\/+|\/+$/g, '');
  if (!cleanRoot) {
    return cleanRelative;
  }
  if (!cleanRelative) {
    return cleanRoot;
  }
  return `${cleanRoot}/${cleanRelative}`;
}

function toBase64(bytes: Uint8Array): string {
  let binary = '';
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}

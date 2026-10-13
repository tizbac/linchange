import { ChangeDetectionStrategy, Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { MailApi } from '../core/mail/mail-api';
import { ComposeTarget } from '../core/mail/compose-target';
import { Cursor, Folder, MessageSummary, SharedMailbox, Thread } from '../core/mail/mail.models';
import { errorMessage } from '../core/http-error';
import { FLAG_FLAGGED, FLAG_SEEN, MailIcon, formatDate, folderIcon, hasFlag } from '../core/mail/ui';
import { MailReaderComponent, MailSelection } from './mail.reader.component';

// The three-pane mail view: mailboxes on the left, a list (messages or
// conversations, with search and batch actions) in the middle and the reading
// pane on the right. It owns the current folder and selection; the reader loads
// what it is told to show and reports changes back so the panes keep up.
interface ListData {
  messages: MessageSummary[];
  total: number;
  hasMore: boolean;
  cursor?: Cursor;
}

@Component({
  selector: 'app-mail',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, RouterLink, MailIcon, MailReaderComponent],
  template: `
    <div class="flex h-[calc(100vh-53px)] min-h-0 bg-slate-50">
      <aside class="flex w-64 shrink-0 flex-col border-r border-slate-200 bg-white">
        <div class="border-b border-slate-200 p-3">
          <button
            class="flex w-full items-center justify-center gap-2 rounded-lg bg-brand-600 px-3 py-2 text-sm font-medium text-white transition hover:bg-brand-700"
            (click)="compose()"
          >
            <app-mail-icon name="send" /> New message
          </button>
        </div>
        <nav class="min-h-0 flex-1 overflow-y-auto p-2">
          <div class="px-2 py-1.5 text-xs font-semibold uppercase tracking-wide text-slate-400">
            Mailboxes
          </div>
          @for (f of sortedFolders(); track f.id) {
            <div>
              <button
                class="flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm transition"
                [class.bg-brand-50]="selectedFolderId() === f.id"
                [class.font-semibold]="selectedFolderId() === f.id"
                [class.text-brand-800]="selectedFolderId() === f.id"
                [class.text-slate-700]="selectedFolderId() !== f.id"
                [style.padding-left.px]="f.parent_id ? 28 : 8"
                (click)="selectFolder(f)"
              >
                <app-mail-icon [name]="folderIcon(f.special_use)" class="text-slate-400" />
                <span class="min-w-0 flex-1 truncate">{{ f.name }}</span>
                @if (f.unread > 0) {
                  <span class="rounded-full bg-brand-600 px-1.5 py-0.5 text-[11px] font-semibold text-white">
                    {{ f.unread > 999 ? '999+' : f.unread }}
                  </span>
                }
              </button>
              @if (selectedFolderId() === f.id) {
                <div class="mb-1 flex flex-wrap gap-x-2 pb-1 pt-0.5" [style.padding-left.px]="f.parent_id ? 28 : 8">
                  <button class="text-[11px] font-medium text-blue-600 hover:underline" (click)="renameFolder(f)">Rename</button>
                  @if (f.total > 0) {
                    <button class="text-[11px] font-medium text-blue-600 hover:underline" (click)="emptyFolder(f)">Empty</button>
                  }
                  @if (f.deletable) {
                    <button class="text-[11px] font-medium text-rose-600 hover:underline" (click)="deleteFolder(f)">Delete</button>
                  }
                </div>
              }
            </div>
          }
          <button
            class="mt-1 flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm text-slate-600 transition hover:bg-slate-100"
            (click)="newFolder()"
          >
            <app-mail-icon name="plus" /> New folder
          </button>
          @if (shared().length > 0) {
            <div class="px-2 pb-1.5 pt-4 text-xs font-semibold uppercase tracking-wide text-slate-400">
              Shared with you
            </div>
            @for (s of shared(); track s.address) {
              <div class="flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm text-slate-600 cursor-pointer hover:bg-slate-50"
                   (click)="selectShared(s)">
                <app-mail-icon name="users" class="text-slate-400" />
                <span class="min-w-0 flex-1 truncate" [title]="s.address">
                  {{ s.display_name || s.address }}
                </span>
                <span class="text-[11px] uppercase text-slate-400">{{ s.level }}</span>
                <app-mail-icon name="chevron-right" class="text-slate-400" />
              </div>
              @if (selectedShared() === s.address) {
                @for (f of sharedFolders().get(s.address) || []; track f.id) {
                  <button
                    class="w-full flex items-center gap-2 ml-6 rounded-lg px-2 py-1.5 text-sm text-slate-600 hover:bg-slate-50 transition"
                    (click)="selectFolder(f)"
                    [class.bg-brand-50]="selectedFolderId() === f.id"
                    [class.text-brand-700]="selectedFolderId() === f.id"
                  >
                    <app-mail-icon [name]="folderIcon(f.special_use)" class="text-slate-400" />
                    <span class="min-w-0 flex-1 truncate">{{ f.name }}</span>
                    <span class="text-xs text-slate-400">{{ f.total }}</span>
                  </button>
                }
              }
            }
          }
        </nav>
        <div class="border-t border-slate-200 p-2">
          <a
            routerLink="/mail/rules"
            class="flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm text-slate-600 transition hover:bg-slate-100"
          >
            <app-mail-icon name="sliders" /> Rules
          </a>
          <a
            routerLink="/mail/import"
            class="flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm text-slate-600 transition hover:bg-slate-100"
          >
            <app-mail-icon name="download" /> Import mail
          </a>
        </div>
      </aside>

      <section class="flex w-96 min-w-72 shrink-0 flex-col border-r border-slate-200 bg-white">
        <div class="flex items-center gap-2 border-b border-slate-200 px-3 py-2">
          <h2 class="min-w-0 flex-1 truncate text-sm font-semibold text-slate-900">{{ listTitle() }}</h2>
          <button class="mail-btn" title="Refresh" (click)="refreshAll()">
            <app-mail-icon name="refresh" />
          </button>
          <button
            class="mail-btn"
            [class.border-brand-600]="viewMode() === 'threads'"
            [class.bg-brand-50]="viewMode() === 'threads'"
            [class.text-brand-700]="viewMode() === 'threads'"
            title="Toggle conversation view"
            (click)="switchView(viewMode() === 'threads' ? 'messages' : 'threads')"
          >
            <app-mail-icon name="conversation" />
          </button>
        </div>

        <form class="border-b border-slate-200 p-2" (ngSubmit)="doSearch()">
          <div class="relative">
            <input
              class="w-full rounded-lg border border-slate-300 py-1.5 pl-3 pr-8 text-sm focus:border-brand-500 focus:outline-none"
              placeholder="Search mail"
              name="search"
              [ngModel]="searchInput"
              (ngModelChange)="searchInput = $event"
            />
            <button
              type="submit"
              class="absolute right-2 top-1/2 -translate-y-1/2 text-slate-400"
              title="Search"
            >
              <app-mail-icon name="search" />
            </button>
          </div>
          @if (!searchQuery()) {
            <div class="mt-2 flex flex-wrap gap-1.5">
              <button
                type="button"
                class="rounded-full border px-2.5 py-0.5 text-[11px] font-medium transition"
                [class.border-brand-600]="unreadOnly()"
                [class.bg-brand-600]="unreadOnly()"
                [class.text-white]="unreadOnly()"
                [class.border-slate-300]="!unreadOnly()"
                [class.bg-white]="!unreadOnly()"
                [class.text-slate-600]="!unreadOnly()"
                [class.hover:bg-slate-100]="!unreadOnly()"
                (click)="toggleFilter('unread')"
              >
                Unread
              </button>
              <button
                type="button"
                class="rounded-full border px-2.5 py-0.5 text-[11px] font-medium transition"
                [class.border-brand-600]="flaggedOnly()"
                [class.bg-brand-600]="flaggedOnly()"
                [class.text-white]="flaggedOnly()"
                [class.border-slate-300]="!flaggedOnly()"
                [class.bg-white]="!flaggedOnly()"
                [class.text-slate-600]="!flaggedOnly()"
                [class.hover:bg-slate-100]="!flaggedOnly()"
                (click)="toggleFilter('flagged')"
              >
                Flagged
              </button>
            </div>
          } @else {
            <div class="mt-2 flex items-center gap-2 text-xs text-slate-500">
              <span>Results for “{{ searchQuery() }}”</span>
              <button type="button" class="ml-auto font-medium text-blue-600 hover:underline" (click)="clearSearch()">
                Clear
              </button>
            </div>
          }
        </form>

        @if (selectedIds().length > 0) {
          <div class="flex flex-wrap items-center gap-1 border-b border-slate-200 bg-slate-50 px-2 py-1.5">
            <span class="px-1 text-xs font-medium text-slate-600">{{ selectedIds().length }} selected</span>
            <select class="mail-select" aria-label="Move selected to folder" (change)="onBatchMove($event)">
              <option value="">Move to…</option>
              @for (f of moveTargets(); track f.id) {
                <option [value]="f.id">{{ f.name }}</option>
              }
            </select>
            <button class="mail-btn" (click)="batch('archive')" [disabled]="batchBusy()" title="Archive">
              <app-mail-icon name="archive" />
            </button>
            <button class="mail-btn" (click)="batch('trash')" [disabled]="batchBusy()" title="Trash">
              <app-mail-icon name="trash" />
            </button>
            <button class="mail-btn mail-btn-danger" (click)="batch('delete')" [disabled]="batchBusy()" title="Delete forever">
              <app-mail-icon name="x" />
            </button>
            <button class="mail-btn" (click)="batchRead(true)" [disabled]="batchBusy()" title="Mark read">
              <app-mail-icon name="check" />
            </button>
            <button class="mail-btn" (click)="batchRead(false)" [disabled]="batchBusy()" title="Mark unread">
              <app-mail-icon name="inbox" />
            </button>
            <button class="mail-btn" (click)="batchFlag(true)" [disabled]="batchBusy()" title="Flag">
              <app-mail-icon name="flag" />
            </button>
            <button class="mail-btn ml-auto" (click)="selectedIds.set([])" title="Clear selection">
              <app-mail-icon name="x" />
            </button>
          </div>
        }

        <div class="min-h-0 flex-1 overflow-y-auto">
          @if (error()) {
            <p class="border-b border-rose-200 bg-rose-50 px-3 py-2 text-sm text-rose-800">{{ error() }}</p>
          }
          @if (listLoading()) {
            <p class="p-4 text-sm text-slate-400">Loading…</p>
          } @else if (viewMode() === 'threads' && !searchQuery()) {
            @if (threads().length === 0) {
              <p class="p-4 text-sm text-slate-400">No conversations in this folder.</p>
            } @else {
              @for (t of threads(); track t.thread_key) {
                <div
                  class="flex cursor-pointer items-center gap-2 border-b border-slate-100 px-3 py-2 transition hover:bg-slate-50"
                  [class.bg-slate-100]="isThreadOpen(t.thread_key)"
                  (click)="openThread(t)"
                >
                  <span class="min-w-0 flex-1">
                    <span
                      class="block truncate text-sm"
                      [class.font-semibold]="t.unread_count > 0"
                      [class.text-slate-900]="t.unread_count > 0"
                      [class.text-slate-600]="t.unread_count === 0"
                      >{{ t.subject }}</span
                    >
                    <span class="block text-xs text-slate-400">
                      {{ t.message_count }} message(s) · {{ formatDate(t.latest.date) }}
                    </span>
                  </span>
                  @if (t.unread_count > 0) {
                    <span class="rounded-full bg-brand-600 px-1.5 py-0.5 text-[11px] font-semibold text-white">
                      {{ t.unread_count }}
                    </span>
                  }
                </div>
              }
            }
          } @else {
            @if (listData().messages.length === 0) {
              <p class="p-4 text-sm text-slate-400">
                {{ searchQuery() ? 'No messages match.' : 'No messages here.' }}
              </p>
            } @else {
              @for (m of listData().messages; track m.id) {
                <div
                  class="flex cursor-pointer items-start gap-2 border-b border-slate-100 px-3 py-2 transition hover:bg-slate-50"
                  [class.bg-slate-100]="isOpen(m.id)"
                  (click)="openMessage(m)"
                >
                  <input
                    type="checkbox"
                    class="mt-1"
                    [checked]="isSelected(m.id)"
                    (click)="$event.stopPropagation()"
                    (change)="toggleSelect(m, $event)"
                  />
                  <div class="min-w-0 flex-1">
                    <div class="flex items-baseline justify-between gap-2">
                      <span
                        class="truncate text-sm"
                        [class.font-semibold]="!hasSeen(m)"
                        [class.text-slate-900]="!hasSeen(m)"
                        [class.font-normal]="hasSeen(m)"
                        [class.text-slate-500]="hasSeen(m)"
                        >{{ m.from_name || m.from }}</span
                      >
                      <span class="shrink-0 text-xs text-slate-400">{{ formatDate(m.date) }}</span>
                    </div>
                    <div class="flex items-baseline gap-2">
                      <span
                        class="truncate text-sm"
                        [class.font-medium]="!hasSeen(m)"
                        [class.text-slate-800]="!hasSeen(m)"
                        [class.text-slate-500]="hasSeen(m)"
                        >{{ m.subject }}</span
                      >
                      <span class="min-w-0 truncate text-xs text-slate-400">{{ m.snippet }}</span>
                      @if (m.has_attachments) {
                        <app-mail-icon name="clip" class="shrink-0 text-slate-400" />
                      }
                      @if (hasFlagged(m)) {
                        <app-mail-icon name="flag" class="shrink-0 text-amber-500" />
                      }
                    </div>
                  </div>
                  <button
                    class="mt-0.5 shrink-0 text-slate-300 transition hover:text-amber-500"
                    [class.text-amber-500]="hasFlagged(m)"
                    title="Flag"
                    (click)="quickFlag(m, $event)"
                  >
                    <app-mail-icon name="flag" />
                  </button>
                </div>
              }
              @if (listData().hasMore) {
                <button
                  class="w-full py-2 text-center text-xs font-medium text-brand-600 transition hover:bg-slate-50"
                  [disabled]="moreLoading()"
                  (click)="loadOlder()"
                >
                  {{ moreLoading() ? 'Loading…' : 'Load older' }}
                </button>
              }
            }
          }
        </div>
      </section>

      <app-mail-reader
        [selection]="selection()"
        [folders]="folders()"
        (changed)="refreshAll()"
        (closed)="onReaderClosed()"
      />
    </div>
  `,
  styles: [
    `.mail-btn {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      border-radius: 0.5rem;
      border: 1px solid #cbd5e1;
      padding: 6px 10px;
      font-size: 12px;
      font-weight: 500;
      color: #334155;
      transition: background 0.15s;
    }
    .mail-btn:hover { background: #f1f5f9; }
    .mail-btn:disabled { cursor: not-allowed; opacity: 0.5; }
    .mail-btn:disabled:hover { background: transparent; }
    .mail-btn-danger { border-color: #fda4af; color: #e11d48; }
    .mail-btn-danger:hover { background: #fff1f2; }
    .mail-select {
      border-radius: 0.5rem;
      border: 1px solid #cbd5e1;
      background: #fff;
      padding: 6px 8px;
      font-size: 12px;
      font-weight: 500;
      color: #334155;
    }`,
  ],
})
export class MailComponent implements OnInit {
  private readonly api = inject(MailApi);
  private readonly router = inject(Router);
  private readonly composeTarget = inject(ComposeTarget);

  readonly folders = signal<Folder[]>([]);
  readonly shared = signal<SharedMailbox[]>([]);
  readonly sharedFolders = signal<Map<string, Folder[]>>(new Map());
  readonly selectedShared = signal<string | null>(null);
  readonly selectedFolderId = signal<number | null>(null);
  readonly selection = signal<MailSelection>({ kind: 'none' });
  readonly searchQuery = signal<string | null>(null);
  readonly unreadOnly = signal(false);
  readonly flaggedOnly = signal(false);
  readonly viewMode = signal<'messages' | 'threads'>('messages');
  readonly listData = signal<ListData>({ messages: [], total: 0, hasMore: false });
  readonly threads = signal<Thread[]>([]);
  readonly listLoading = signal(false);
  readonly moreLoading = signal(false);
  readonly batchBusy = signal(false);
  readonly selectedIds = signal<number[]>([]);
  readonly error = signal('');
  searchInput = '';

  protected readonly formatDate = formatDate;
  protected readonly folderIcon = folderIcon;

  protected readonly sortedFolders = computed(() => {
    const specials: Record<string, number> = {
      '\\sent': 1,
      '\\drafts': 2,
      '\\junk': 3,
      '\\trash': 4,
      '\\archive': 5,
    };
    const rank = (f: Folder): number => {
      if (f.name.toUpperCase() === 'INBOX') return 0;
      const r = specials[f.special_use ?? ''];
      return r === undefined ? 100 : r;
    };
    return [...this.folders()].sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name));
  });

  protected readonly moveTargets = computed(() =>
    this.folders().filter((f) => f.id !== this.selectedFolderId()),
  );

  private listSeq = 0;

  ngOnInit(): void {
    void this.bootstrap();
  }

  private async bootstrap(): Promise<void> {
    await this.refreshFolders();
    try {
      const res = await this.api.shared();
      this.shared.set(res.shared ?? []);
    } catch {
      // Shared mailboxes are a convenience, not a requirement for opening mail.
    }
    const inbox =
      this.folders().find((f) => f.name.toUpperCase() === 'INBOX') ?? this.folders()[0];
    if (inbox) this.selectFolder(inbox);
  }

  // --- folders ---------------------------------------------------------------

  protected listTitle(): string {
    const q = this.searchQuery();
    if (q) return `Search: “${q}”`;
    const f = this.folders().find((x) => x.id === this.selectedFolderId());
    return f ? f.name : 'Mail';
  }

  protected async refreshFolders(): Promise<void> {
    try {
      const res = await this.api.listFolders();
      this.folders.set(res.folders ?? []);
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected selectFolder(f: Folder): void {
    this.selectedFolderId.set(f.id);
    this.selectedShared.set(null);
    this.searchQuery.set(null);
    this.searchInput = '';
    this.unreadOnly.set(false);
    this.flaggedOnly.set(false);
    this.viewMode.set('messages');
    this.selectedIds.set([]);
    this.selection.set({ kind: 'none' });
    void this.loadList(true);
  }

  protected async selectShared(s: SharedMailbox): Promise<void> {
    this.selectedShared.set(s.address);
    this.selectedFolderId.set(null);
    this.searchQuery.set(null);
    this.searchInput = '';
    this.unreadOnly.set(false);
    this.flaggedOnly.set(false);
    this.viewMode.set('messages');
    this.selectedIds.set([]);
    this.selection.set({ kind: 'none' });

    const cached = this.sharedFolders().get(s.address);
    if (cached) {
      // Folders already loaded
    } else {
      try {
        const res = await this.api.listFolders(s.address);
        const folders = res.folders ?? [];
        this.sharedFolders.update(m => new Map(m).set(s.address, folders));
      } catch (err) {
        this.error.set(errorMessage(err));
      }
    }
  }

  protected switchView(mode: 'messages' | 'threads'): void {
    this.viewMode.set(mode);
    void this.loadList(true);
  }

  protected toggleFilter(which: 'unread' | 'flagged'): void {
    if (which === 'unread') this.unreadOnly.update((v) => !v);
    else this.flaggedOnly.update((v) => !v);
    void this.loadList(true);
  }

  protected doSearch(): void {
    this.searchQuery.set(this.searchInput.trim() || null);
    this.selectedIds.set([]);
    void this.loadList(true);
  }

  protected clearSearch(): void {
    this.searchInput = '';
    this.searchQuery.set(null);
    this.selectedIds.set([]);
    void this.loadList(true);
  }

  protected openMessage(m: MessageSummary): void {
    this.selection.set({ kind: 'message', id: m.id });
  }

  protected openThread(t: Thread): void {
    this.selection.set({ kind: 'thread', key: t.thread_key });
  }

  protected onReaderClosed(): void {
    this.selection.set({ kind: 'none' });
  }

  // --- folder operations -------------------------------------------------------

  protected async newFolder(): Promise<void> {
    const name = prompt('New folder name');
    if (!name || !name.trim()) return;
    try {
      await this.api.createFolder(name.trim());
      await this.refreshFolders();
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected async renameFolder(f: Folder): Promise<void> {
    const name = prompt('Rename folder', f.name);
    if (!name || !name.trim() || name === f.name) return;
    try {
      await this.api.renameFolder(f.id, name.trim());
      await this.refreshFolders();
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected async emptyFolder(f: Folder): Promise<void> {
    if (!confirm(`Empty “${f.name}”? Its ${f.total} message(s) move to Trash.`)) return;
    try {
      await this.api.emptyFolder(f.id);
      await this.refreshFolders();
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected async deleteFolder(f: Folder): Promise<void> {
    if (!confirm(`Delete folder “${f.name}” and its ${f.total} message(s)?`)) return;
    try {
      await this.api.deleteFolder(f.id);
      await this.refreshFolders();
      if (this.selectedFolderId() === f.id) {
        const next = this.folders()[0];
        if (next) {
          this.selectFolder(next);
        } else {
          this.selectedFolderId.set(null);
          this.selection.set({ kind: 'none' });
          this.listData.set({ messages: [], total: 0, hasMore: false });
        }
      }
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected compose(): void {
    this.composeTarget.set({});
    void this.router.navigate(['/mail/compose']);
  }
  // --- list loading ------------------------------------------------------------

  protected async refreshAll(): Promise<void> {
    await Promise.all([this.refreshFolders(), this.loadList(true)]);
  }

  protected loadOlder(): void {
    void this.loadList(false);
  }

  private async loadList(reset: boolean): Promise<void> {
    const seq = ++this.listSeq;
    const q = this.searchQuery();
    const folderId = this.selectedFolderId();
    const done = (fn: () => void): void => {
      if (seq === this.listSeq) fn();
    };

    if (q) {
      this.listLoading.set(true);
      this.error.set('');
      try {
        const res = await this.api.search(q, folderId ?? 0);
        const messages = res.messages ?? [];
        done(() => this.listData.set({ messages, total: messages.length, hasMore: false }));
      } catch (err) {
        done(() => this.error.set(errorMessage(err)));
      } finally {
        done(() => this.listLoading.set(false));
      }
      return;
    }

    if (!folderId) {
      this.listData.set({ messages: [], total: 0, hasMore: false });
      this.threads.set([]);
      return;
    }

    if (this.viewMode() === 'threads') {
      this.listLoading.set(true);
      this.error.set('');
      try {
        const res = await this.api.listThreads(folderId);
        done(() => this.threads.set(res.threads ?? []));
      } catch (err) {
        done(() => this.error.set(errorMessage(err)));
      } finally {
        done(() => this.listLoading.set(false));
      }
      return;
    }

    const opts = {
      unread: this.unreadOnly() || undefined,
      flagged: this.flaggedOnly() || undefined,
    };

    if (!reset) {
      const cur = this.listData();
      if (!cur.hasMore || !cur.cursor) return;
      this.moreLoading.set(true);
      try {
        const res = await this.api.listMessages(folderId, { ...opts, cursor: cur.cursor });
        done(() =>
          this.listData.set({
            messages: [...cur.messages, ...(res.messages ?? [])],
            total: res.total,
            hasMore: res.has_more,
            cursor: res.next_cursor,
          }),
        );
      } catch (err) {
        done(() => this.error.set(errorMessage(err)));
      } finally {
        done(() => this.moreLoading.set(false));
      }
      return;
    }

    this.listLoading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listMessages(folderId, opts);
      done(() =>
        this.listData.set({
          messages: res.messages ?? [],
          total: res.total,
          hasMore: res.has_more,
          cursor: res.next_cursor,
        }),
      );
    } catch (err) {
      done(() => this.error.set(errorMessage(err)));
    } finally {
      done(() => this.listLoading.set(false));
    }
  }

  // --- rows and batch actions ---------------------------------------------------

  protected hasSeen(m: MessageSummary): boolean {
    return hasFlag(m.flags, FLAG_SEEN);
  }

  protected hasFlagged(m: MessageSummary): boolean {
    return hasFlag(m.flags, FLAG_FLAGGED);
  }

  protected isOpen(id: number): boolean {
    const s = this.selection();
    return s.kind === 'message' && s.id === id;
  }

  protected isThreadOpen(key: string): boolean {
    const s = this.selection();
    return s.kind === 'thread' && s.key === key;
  }

  protected isSelected(id: number): boolean {
    return this.selectedIds().includes(id);
  }

  protected toggleSelect(m: MessageSummary, event: Event): void {
    const checked = (event.target as HTMLInputElement).checked;
    const cur = this.selectedIds();
    this.selectedIds.set(checked ? [...cur, m.id] : cur.filter((x) => x !== m.id));
  }

  protected async quickFlag(m: MessageSummary, event: Event): Promise<void> {
    event.stopPropagation();
    const set = !hasFlag(m.flags, FLAG_FLAGGED);
    try {
      await this.api.setFlag(m.id, FLAG_FLAGGED, set);
      this.listData.update((cur) => ({
        ...cur,
        messages: cur.messages.map((x) =>
          x.id === m.id
            ? {
                ...x,
                flags: set ? [...x.flags, FLAG_FLAGGED] : x.flags.filter((f) => f !== FLAG_FLAGGED),
              }
            : x,
        ),
      }));
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected onBatchMove(event: Event): void {
    const value = (event.target as HTMLSelectElement).value;
    void this.moveSelected(value ? Number(value) : 0);
  }

  private async moveSelected(folderId: number): Promise<void> {
    const ids = this.selectedIds();
    if (!ids.length || !folderId) return;
    this.batchBusy.set(true);
    this.error.set('');
    try {
      await this.api.move(ids, folderId);
      this.selectedIds.set([]);
      await this.refreshAll();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.batchBusy.set(false);
    }
  }

  protected async batch(kind: 'archive' | 'trash' | 'delete'): Promise<void> {
    const ids = this.selectedIds();
    if (!ids.length) return;
    if (
      kind === 'delete' &&
      !confirm(`Permanently delete ${ids.length} message(s)? This cannot be undone.`)
    ) {
      return;
    }
    this.batchBusy.set(true);
    this.error.set('');
    try {
      if (kind === 'archive') await this.api.archive(ids);
      else if (kind === 'trash') await this.api.trash(ids);
      else await this.api.delete(ids);
      this.selectedIds.set([]);
      await this.refreshAll();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.batchBusy.set(false);
    }
  }

  protected async batchRead(read: boolean): Promise<void> {
    const ids = this.selectedIds();
    this.batchBusy.set(true);
    this.error.set('');
    try {
      await Promise.all(ids.map((id) => this.api.markRead(id, read)));
      await this.refreshAll();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.batchBusy.set(false);
    }
  }

  protected async batchFlag(set: boolean): Promise<void> {
    const ids = this.selectedIds();
    this.batchBusy.set(true);
    this.error.set('');
    try {
      await Promise.all(ids.map((id) => this.api.setFlag(id, FLAG_FLAGGED, set)));
      await this.refreshAll();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.batchBusy.set(false);
    }
  }
}

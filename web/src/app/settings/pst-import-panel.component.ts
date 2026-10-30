import { DecimalPipe } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  ElementRef,
  OnDestroy,
  OnInit,
  inject,
  input,
  output,
  signal,
  viewChild,
} from '@angular/core';
import { FormsModule } from '@angular/forms';

import { Calendar } from '../core/calendar/calendar.models';
import { CalendarApi } from '../core/calendar/calendar-api';
import { AddressBook } from '../core/contacts/contacts.models';
import { ContactsApi } from '../core/contacts/contacts-api';
import { errorMessage } from '../core/http-error';
import { Folder } from '../core/mail/mail.models';
import { MailApi } from '../core/mail/mail-api';
import { PstPreview, PstStats, PstBase, PstImportService } from '../core/pst/pst-import.service';

// The form shared by the user page and the admin page: pick a PST, choose the
// folders and destinations, upload the converted items in batches.
//
// The two pages differ in only two ways: the admin imports into somebody else's
// mailbox, and the server uses that user's default calendar and address book
// because the admin panel has no calendar picker for arbitrary users. Keeping
// one component means the safeguards (the 64 MiB batch, the close warning, the
// dedupe the server applies) cannot drift between them.
@Component({
  selector: 'app-pst-import-panel',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [DecimalPipe, FormsModule],
  template: `
    @if (error()) {
      <p class="mt-4 rounded-lg border border-rose-200 bg-rose-50 px-3 py-2 text-sm text-rose-800">
        {{ error() }}
      </p>
    }
    @if (ok()) {
      <p class="mt-4 rounded-lg border border-emerald-200 bg-emerald-50 px-3 py-2 text-sm text-emerald-800">
        {{ ok() }}
      </p>
    }

    <!-- File selection -->
    @if (!preview() && !parsing()) {
      <div class="mt-5 rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
        <h2 class="text-sm font-semibold text-slate-900">Choose an Outlook data file</h2>
        <p class="mt-1 text-sm text-slate-600">
          A <span class="font-medium">.pst</span> (Outlook 97-2002 or 2007+) or an uncompressed
          <span class="font-medium">.ost</span>. The file is read in this tab and only the converted
          messages, appointments and contacts are uploaded.
        </p>
        <button
          type="button"
          class="mt-4 rounded-lg bg-brand-600 px-4 py-2 text-sm font-medium text-white transition hover:bg-brand-700"
          (click)="pickFile()"
        >
          Select file
        </button>
        <input
          #fileInput
          type="file"
          accept=".pst,.ost"
          class="hidden"
          (change)="onFile($event)"
        />
        <p class="mt-3 text-xs text-slate-500">
          Compressed 4k-page OST files are not supported; export them to a .pst from Outlook first.
        </p>
      </div>
    }

    @if (parsing()) {
      <div class="mt-5 rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
        <p class="text-sm text-slate-700">Reading the archive…</p>
        <p class="mt-1 text-xs text-slate-500">
          Large files take a while. The page stays responsive; do not close it.
        </p>
      </div>
    }

    @if (preview(); as p) {
      <div class="mt-5 space-y-5">
        <div class="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div>
              <h2 class="text-sm font-semibold text-slate-900">{{ fileName() }}</h2>
              <p class="mt-1 text-xs text-slate-500">
                {{ p.contentType }} · {{ p.unicode ? '64-bit Unicode' : '32-bit ANSI' }} ·
                {{ p.folders.length }} folder(s) · {{ messageCount() }} message(s),
                {{ appointmentCount() }} appointment(s), {{ contactCount() }} contact(s)
              </p>
            </div>
            <button
              type="button"
              class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 transition hover:bg-slate-100 disabled:opacity-50"
              [disabled]="importing()"
              (click)="reset()"
            >
              Choose another file
            </button>
          </div>
        </div>

        @if (base() === 'admin') {
          <div class="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
            <label class="block text-xs font-medium text-slate-600">Target mailbox</label>
            <input
              class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm"
              name="pst-owner"
              list="pst-owner-list"
              [ngModel]="ownerValue()"
              (ngModelChange)="ownerValue.set($event)"
              placeholder="user@example.com"
              [disabled]="importing()"
            />
            <datalist id="pst-owner-list">
              @for (address of users(); track address) {
                <option [value]="address"></option>
              }
            </datalist>
            <p class="mt-1 text-xs text-slate-500">
              Appointments and contacts go into this mailbox's default calendar and personal address
              book.
            </p>
          </div>
        }

        <!-- Folders -->
        <div class="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
          <div class="flex items-center justify-between gap-3">
            <h2 class="text-sm font-semibold text-slate-900">Mail folders</h2>
            <div class="flex gap-2 text-xs">
              <button type="button" class="text-brand-700 hover:underline" (click)="selectAll(true)">All</button>
              <button type="button" class="text-brand-700 hover:underline" (click)="selectAll(false)">None</button>
            </div>
          </div>
          <p class="mt-1 text-xs text-slate-500">
            Each selected folder keeps its name under the destination folder.
          </p>
          <div class="mt-3 max-h-72 overflow-y-auto rounded-lg border border-slate-200">
            <table class="w-full text-sm">
              <thead class="sticky top-0 border-b border-slate-200 bg-slate-50 text-left text-xs font-medium text-slate-500">
                <tr>
                  <th class="w-8 px-3 py-2"></th>
                  <th class="px-3 py-2">Source folder</th>
                  <th class="px-3 py-2 text-right">Mail</th>
                  <th class="px-3 py-2 text-right">Appts</th>
                  <th class="px-3 py-2 text-right">Contacts</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-slate-100">
                @for (folder of p.folders; track folder.id) {
                  <tr class="hover:bg-slate-50">
                    <td class="px-3 py-1.5">
                      <input
                        type="checkbox"
                        class="rounded"
                        [checked]="selected().has(folder.id)"
                        [disabled]="folder.messages === 0 || importing()"
                        (change)="toggleFolder(folder.id)"
                        [attr.aria-label]="'Import ' + folder.path"
                      />
                    </td>
                    <td class="px-3 py-1.5 text-slate-800">{{ folder.path }}</td>
                    <td class="px-3 py-1.5 text-right text-slate-600">{{ folder.messages }}</td>
                    <td class="px-3 py-1.5 text-right text-slate-500">{{ folder.calendars }}</td>
                    <td class="px-3 py-1.5 text-right text-slate-500">{{ folder.contacts }}</td>
                  </tr>
                }
              </tbody>
            </table>
          </div>

          <div class="mt-4 grid gap-4 sm:grid-cols-2">
            <div>
              <label class="block text-xs font-medium text-slate-600">Destination folder</label>
              <input
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm"
                name="pst-target"
                list="pst-folder-list"
                [ngModel]="targetRoot()"
                (ngModelChange)="targetRoot.set($event)"
                [disabled]="importing()"
              />
              <datalist id="pst-folder-list">
                @for (folder of mailFolders(); track folder.id) {
                  <option [value]="folder.name"></option>
                }
              </datalist>
              <p class="mt-1 text-xs text-slate-500">A new folder is created if it does not exist.</p>
            </div>
          </div>
        </div>

        <!-- Calendar and contacts -->
        <div class="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
          <h2 class="text-sm font-semibold text-slate-900">Appointments and contacts</h2>
          <div class="mt-3 grid gap-4 sm:grid-cols-2">
            <div>
              <label class="flex items-center gap-2 text-sm text-slate-700">
                <input
                  type="checkbox"
                  class="rounded"
                  [ngModel]="includeCalendar()"
                  (ngModelChange)="includeCalendar.set($event)"
                  [disabled]="importing()"
                />
                Import {{ appointmentCount() }} appointment(s)
              </label>
              @if (base() === 'mail' && includeCalendar()) {
                <select
                  class="mt-2 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm"
                  [ngModel]="calendarId()"
                  (ngModelChange)="calendarId.set(+$event)"
                  [disabled]="importing()"
                >
                  @for (calendar of calendars(); track calendar.id) {
                    <option [value]="calendar.id">
                      {{ calendar.display_name || calendar.name }}
                    </option>
                  }
                </select>
              }
            </div>
            <div>
              <label class="flex items-center gap-2 text-sm text-slate-700">
                <input
                  type="checkbox"
                  class="rounded"
                  [ngModel]="includeContacts()"
                  (ngModelChange)="includeContacts.set($event)"
                  [disabled]="importing()"
                />
                Import {{ contactCount() }} contact(s)
              </label>
              @if (base() === 'mail' && includeContacts()) {
                <select
                  class="mt-2 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm"
                  [ngModel]="addressBookId()"
                  (ngModelChange)="addressBookId.set(+$event)"
                  [disabled]="importing()"
                >
                  @for (book of books(); track book.id) {
                    <option [value]="book.id">{{ book.name }}</option>
                  }
                </select>
              }
            </div>
          </div>
        </div>

        <!-- Progress -->
        @if (importing()) {
          <div class="rounded-xl border border-amber-300 bg-amber-50 p-5">
            <p class="text-sm font-semibold text-amber-900">
              Do not close this page while the import runs.
            </p>
            <p class="mt-1 text-xs text-amber-800">
              Batches already committed are kept. If the page is closed, open the file again and run
              the import: items already imported are skipped.
            </p>
            <p class="mt-3 text-sm text-amber-900">{{ phase() }}</p>
            @if (stats(); as s) {
              <p class="mt-1 text-xs text-amber-800">
                {{ s.messages }} mail · {{ s.events }} appointment(s) · {{ s.contacts }} contact(s) ·
                {{ s.skipped }} skipped · {{ s.bytes | number }} bytes
              </p>
            }
          </div>
        }

        <div class="flex flex-wrap items-center gap-2">
          <button
            type="button"
            class="rounded-lg bg-brand-600 px-4 py-2 text-sm font-medium text-white transition hover:bg-brand-700 disabled:opacity-50"
            [disabled]="!canImport() || importing()"
            (click)="start()"
          >
            {{ importing() ? 'Importing…' : 'Start import' }}
          </button>
          @if (base() === 'admin' && ownerValue() === '') {
            <span class="text-xs text-rose-600">Choose the target mailbox first.</span>
          }
        </div>
      </div>
    }
  `,
})
export class PstImportPanelComponent implements OnInit, OnDestroy {
  private readonly service = inject(PstImportService);
  private readonly mailApi = inject(MailApi);
  private readonly calendarApi = inject(CalendarApi);
  private readonly contactsApi = inject(ContactsApi);
  private readonly fileInput = viewChild<ElementRef<HTMLInputElement>>('fileInput');

  readonly base = input.required<PstBase>();
  readonly owner = input<string>('');
  readonly users = input<string[]>([]);
  readonly done = output<PstStats>();

  protected readonly file = signal<File | null>(null);
  protected readonly fileName = signal('');
  protected readonly preview = signal<PstPreview | null>(null);
  protected readonly parsing = signal(false);
  protected readonly importing = signal(false);
  protected readonly phase = signal('');
  protected readonly stats = signal<PstStats | null>(null);
  protected readonly error = signal('');
  protected readonly ok = signal('');

  protected readonly selected = signal<Set<number>>(new Set());
  protected readonly targetRoot = signal('PST Import');
  protected readonly includeCalendar = signal(true);
  protected readonly includeContacts = signal(true);
  protected readonly calendarId = signal(0);
  protected readonly addressBookId = signal(0);

  protected readonly mailFolders = signal<Folder[]>([]);
  protected readonly calendars = signal<Calendar[]>([]);
  protected readonly books = signal<AddressBook[]>([]);
  protected readonly ownerValue = signal('');

  private unloadGuard = (event: BeforeUnloadEvent) => {
    event.preventDefault();
    event.returnValue = '';
  };

  async ngOnInit(): Promise<void> {
    this.ownerValue.set(this.owner());
    if (this.base() !== 'mail') {
      return;
    }
    try {
      const [folders, calendars, books] = await Promise.all([
        this.mailApi.listFolders(),
        this.calendarApi.list(),
        this.contactsApi.listBooks(),
      ]);
      this.mailFolders.set(folders.folders ?? []);
      this.calendars.set(calendars.calendars ?? []);
      this.books.set(books.books ?? []);
      const preferred = this.calendars().find((calendar) => calendar.is_default) ?? this.calendars()[0];
      if (preferred) {
        this.calendarId.set(preferred.id);
      }
      const personal = this.books().find((book) => book.scope === 'personal') ?? this.books()[0];
      if (personal) {
        this.addressBookId.set(personal.id);
      }
    } catch (err) {
      // The form still works with the defaults the server applies; only the
      // pickers are unavailable.
      this.error.set(errorMessage(err));
    }
  }

  ngOnDestroy(): void {
    this.removeUnloadGuard();
  }

  protected pickFile(): void {
    this.fileInput()?.nativeElement.click();
  }

  protected async onFile(event: Event): Promise<void> {
    const input = event.target as HTMLInputElement;
    const file = input.files?.[0];
    input.value = '';
    if (!file) {
      return;
    }
    this.file.set(file);
    this.fileName.set(file.name);
    this.preview.set(null);
    this.stats.set(null);
    this.ok.set('');
    this.error.set('');
    this.parsing.set(true);
    try {
      const preview = await this.service.preview(file);
      this.preview.set(preview);
      this.selected.set(new Set(preview.folders.filter((folder) => folder.messages > 0).map((folder) => folder.id)));
    } catch (err) {
      this.error.set(errorMessage(err));
      this.file.set(null);
    } finally {
      this.parsing.set(false);
    }
  }

  protected reset(): void {
    if (this.importing()) {
      return;
    }
    this.file.set(null);
    this.preview.set(null);
    this.stats.set(null);
    this.error.set('');
    this.ok.set('');
  }

  protected toggleFolder(id: number): void {
    const next = new Set(this.selected());
    if (next.has(id)) {
      next.delete(id);
    } else {
      next.add(id);
    }
    this.selected.set(next);
  }

  protected selectAll(on: boolean): void {
    const preview = this.preview();
    if (!preview) {
      return;
    }
    this.selected.set(
      new Set(on ? preview.folders.filter((folder) => folder.messages > 0).map((folder) => folder.id) : []),
    );
  }

  protected messageCount(): number {
    return this.preview()?.items.filter((item) => item.kind === 'message').length ?? 0;
  }

  protected appointmentCount(): number {
    return this.preview()?.items.filter((item) => item.kind === 'appointment').length ?? 0;
  }

  protected contactCount(): number {
    return this.preview()?.items.filter((item) => item.kind === 'contact').length ?? 0;
  }

  protected canImport(): boolean {
    const preview = this.preview();
    if (!preview) {
      return false;
    }
    if (this.base() === 'admin' && this.ownerValue().trim() === '') {
      return false;
    }
    return this.selected().size > 0 || this.includeCalendar() || this.includeContacts();
  }

  protected async start(): Promise<void> {
    const preview = this.preview();
    const file = this.file();
    if (!preview || !file) {
      return;
    }
    this.importing.set(true);
    this.error.set('');
    this.ok.set('');
    this.stats.set(null);
    this.phase.set('Preparing…');
    window.addEventListener('beforeunload', this.unloadGuard);
    try {
      const stats = await this.service.run({
        base: this.base(),
        owner: this.base() === 'admin' ? this.ownerValue().trim() : undefined,
        fileName: file.name,
        file,
        preview,
        selectedFolders: this.selected(),
        targetRoot: this.targetRoot(),
        includeCalendar: this.includeCalendar(),
        includeContacts: this.includeContacts(),
        calendarId: this.calendarId() || undefined,
        addressBookId: this.addressBookId() || undefined,
        onPhase: (phase) => this.phase.set(phase),
        onProgress: (progress) => this.stats.set(progress),
      });
      this.stats.set(stats);
      this.ok.set(
        `Import finished: ${stats.messages} message(s), ${stats.events} appointment(s), ` +
          `${stats.contacts} contact(s), ${stats.skipped} skipped.`,
      );
      this.done.emit(stats);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.importing.set(false);
      this.removeUnloadGuard();
    }
  }

  private removeUnloadGuard(): void {
    window.removeEventListener('beforeunload', this.unloadGuard);
  }
}

import { ChangeDetectionStrategy, Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';
import { AuthService } from '../core/auth/auth.service';
import { MailApi } from '../core/mail/mail-api';
import { ComposeTarget } from '../core/mail/compose-target';
import { Address, ComposeRequest, SharedMailbox } from '../core/mail/mail.models';
import { errorMessage } from '../core/http-error';
import { MailIcon, addressLabel, formatBytes, parseAddresses } from '../core/mail/ui';
import {
  RichTextEditorComponent,
  htmlHasContent,
  htmlToText,
} from '../core/mail/rich-text';
import { CalendarApi } from '../core/calendar/calendar-api';
import { Calendar as CalendarModel } from '../core/calendar/calendar.models';
import { formatDateTime } from '../core/calendar/ui';
import { Contact, OrganizationApi } from '../core/organization/organization-api';

// escapeHtml turns the characters that are markup into text, for wrapping a
// plain-text body as HTML.
//
// The rich text editor interprets whatever it is given, so a body typed in the
// plain-text tab and then moved across has to arrive as text rather than as
// markup. Without this, a message containing "<b>" would come back with the tags
// interpreted and a line that was indented by spaces would come back indented by
// a tab.
function escapeHtml(value: string): string {
  return value
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

// One file the composer will attach, read as a data URL the server accepts.
interface AttachmentItem {
  name: string;
  size: number;
  mime: string;
  content: string;
}

// InviteCandidate is one event offered in the invitation picker: enough to
// fetch its invitation and to say who is already going.
interface InviteCandidate {
  calendarId: number;
  eventId: number;
  summary: string;
  start: string;
  attendees: string[];
}

// blobToDataUrl renders a fetched file the way a chosen file arrives, so an
// invitation attachment travels through the same path as every other one.
function blobToDataUrl(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const fr = new FileReader();
    fr.onload = () => resolve(String(fr.result));
    fr.onerror = () => reject(fr.error ?? new Error('could not read the invitation'));
    fr.readAsDataURL(blob);
  });
}

// inviteFileName keeps a title that may contain punctuation or non-Latin
// letters out of the filename, without losing the part that identifies it.
function inviteFileName(summary: string): string {
  const base = summary
    .replace(/[\\/:*?"<>|]+/g, ' ')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, 60);
  return `${base || 'invitation'}.ics`;
}

// The composer: From (the user plus their send-as aliases), To/Cc/Bcc with a
// plain-text preview so a mistyped recipient is visible before it is sent, a
// body, and files. Replies and forwards arrive pre-filled through the
// ComposeTarget service and are consumed once.
@Component({
  selector: 'app-mail-compose',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, MailIcon, RichTextEditorComponent],
  template: `
    <main class="mx-auto max-w-4xl px-6 py-8">
      <div class="flex items-center justify-between gap-3">
        <h1 class="text-lg font-semibold text-slate-900">{{ replyLabel() || 'New message' }}</h1>
        <button
          class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 transition hover:bg-slate-100"
          (click)="cancel()"
        >
          Close
        </button>
      </div>

      <form class="mt-4 rounded-xl border border-slate-200 bg-white p-5" (ngSubmit)="send()">
        <div class="space-y-4">
          <div class="flex flex-wrap items-center gap-3 text-sm">
            <label class="w-20 shrink-0 text-right font-medium text-slate-600" for="mc-from">From</label>
            <select
              id="mc-from"
              name="from"
              class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm focus:border-brand-500 focus:outline-none"
              [(ngModel)]="from"
            >
              @for (a of sendableAddresses(); track a) {
                <option [value]="a">{{ a }}</option>
              }
            </select>
          </div>

          <div class="flex flex-wrap items-start gap-3 text-sm">
            <label class="w-20 shrink-0 pt-1.5 text-right font-medium text-slate-600" for="mc-to">To</label>
            <div class="min-w-0 flex-1">
              <input
                id="mc-to"
                name="to"
                class="w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm focus:border-brand-500 focus:outline-none"
                placeholder="name@example.com"
                [(ngModel)]="toRaw"
              />
              @if (hasInvalid(toRaw)) {
                <p class="mt-1 text-xs text-rose-600">
                  Every entry needs an @ — some entries above are not addresses.
                </p>
              }
            </div>
          </div>

          <div class="flex flex-wrap items-start gap-3 text-sm">
            <label class="w-20 shrink-0 pt-1.5 text-right font-medium text-slate-600" for="mc-cc">Cc</label>
            <div class="min-w-0 flex-1">
              <input
                id="mc-cc"
                name="cc"
                class="w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm focus:border-brand-500 focus:outline-none"
                placeholder="name@example.com"
                [(ngModel)]="ccRaw"
              />
              @if (hasInvalid(ccRaw)) {
                <p class="mt-1 text-xs text-rose-600">
                  Every entry needs an @ — some entries above are not addresses.
                </p>
              }
            </div>
          </div>

          <div class="flex flex-wrap items-start gap-3 text-sm">
            <label class="w-20 shrink-0 pt-1.5 text-right font-medium text-slate-600" for="mc-bcc">Bcc</label>
            <div class="min-w-0 flex-1">
              <input
                id="mc-bcc"
                name="bcc"
                class="w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm focus:border-brand-500 focus:outline-none"
                placeholder="name@example.com"
                [(ngModel)]="bccRaw"
              />
              @if (hasInvalid(bccRaw)) {
                <p class="mt-1 text-xs text-rose-600">
                  Every entry needs an @ — some entries above are not addresses.
                </p>
              }
            </div>
          </div>

          <!-- The address book: everyone with an address in the organization's
               domains. A list rather than a typeahead, because the point of an
               organization is that you should not have to remember who is in
               it. -->
          <div class="flex flex-wrap items-start gap-3 text-sm">
            <span class="w-20 shrink-0 pt-1.5 text-right font-medium text-slate-600">Contacts</span>
            <div class="min-w-0 flex-1">
              <button
                type="button"
                class="text-sm font-medium text-blue-600 hover:underline"
                (click)="openContacts()"
              >
                {{ showContacts() ? 'Hide contacts' : 'People in your organization' }}
              </button>
              @if (showContacts()) {
                <div class="mt-2 rounded-lg border border-slate-200 bg-slate-50 p-3">
                  <input
                    type="search"
                    name="contactFilter"
                    class="w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm focus:border-brand-500 focus:outline-none"
                    placeholder="Filter by name or address"
                    [ngModel]="contactFilter()"
                    (ngModelChange)="contactFilter.set($event)"
                  />
                  @if (contactsLoading()) {
                    <p class="mt-2 text-sm text-slate-500">Loading contacts…</p>
                  } @else if (matchingContacts().length === 0) {
                    <p class="mt-2 text-sm text-slate-500">Nobody matches that.</p>
                  } @else {
                    <ul class="mt-2 max-h-56 space-y-1 overflow-auto">
                      @for (c of matchingContacts(); track c.address) {
                        <li>
                          <button
                            type="button"
                            class="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-sm text-slate-700 transition hover:bg-white"
                            (click)="addRecipient(c.address)"
                          >
                            <span class="min-w-0 flex-1 truncate">{{ c.display_name || c.address }}</span>
                            <span class="shrink-0 truncate text-xs text-slate-400">{{ c.address }}</span>
                          </button>
                        </li>
                      }
                    </ul>
                  }
                </div>
              }
            </div>
          </div>

          <div class="flex flex-wrap items-center gap-3 text-sm">
            <label class="w-20 shrink-0 text-right font-medium text-slate-600" for="mc-subject">Subject</label>
            <input
              id="mc-subject"
              name="subject"
              class="min-w-0 flex-1 rounded-lg border border-slate-300 px-3 py-1.5 text-sm focus:border-brand-500 focus:outline-none"
              [(ngModel)]="subject"
            />
          </div>

          <div class="flex flex-wrap items-center justify-between gap-3">
            <span class="text-sm font-medium text-slate-600">Message</span>
            <div class="flex items-center gap-3">
              @if (format() === 'html') {
                <button
                  type="button"
                  class="text-xs text-slate-500 hover:text-slate-800"
                  (click)="showPlainText.set(!showPlainText())"
                >
                  {{ showPlainText() ? 'Hide the plain text version' : 'Preview the plain text version' }}
                </button>
              }
              <div class="flex rounded-lg border border-slate-300 p-0.5 text-xs font-medium" role="group" aria-label="Message format">
                <button
                  type="button"
                  class="rounded-md px-2.5 py-1 transition"
                  [class.bg-slate-100]="format() === 'html'"
                  [class.text-slate-900]="format() === 'html'"
                  [class.text-slate-500]="format() !== 'html'"
                  [attr.aria-pressed]="format() === 'html'"
                  (click)="setFormat('html')"
                >
                  Rich text
                </button>
                <button
                  type="button"
                  class="rounded-md px-2.5 py-1 transition"
                  [class.bg-slate-100]="format() === 'text'"
                  [class.text-slate-900]="format() === 'text'"
                  [class.text-slate-500]="format() !== 'text'"
                  [attr.aria-pressed]="format() === 'text'"
                  (click)="setFormat('text')"
                >
                  Plain text
                </button>
              </div>
            </div>
          </div>

          @if (format() === 'html') {
            <app-rich-text-editor
              [html]="bodyHtml()"
              [placeholder]="'Write your message'"
              (htmlChange)="bodyHtml.set($event)"
            />
            @if (signatureLoading()) {
              <p class="text-xs text-slate-500 mt-2">Loading signature…</p>
            } @else if (signatureHtml()) {
              <div class="mt-3 rounded-lg border border-slate-200 bg-slate-50">
                <p class="px-3 py-2 text-xs font-medium text-slate-500 border-b border-slate-200">Your signature (appended on send)</p>
                <div class="px-3 py-2 text-sm text-slate-700" [innerHTML]="signatureHtml()"></div>
              </div>
            }
            @if (showPlainText()) {
              <!-- What a recipient who cannot render HTML will read. Shown
                   because the two parts are sent together and the user cannot
                   otherwise tell what the second one says. -->
              <div class="rounded-lg border border-dashed border-slate-300 bg-slate-50 px-3 py-2">
                <p class="mb-1 text-xs font-medium text-slate-500">Plain text version, as they will see it</p>
                <pre class="whitespace-pre-wrap text-xs text-slate-700">{{ plainTextAlternative() }}</pre>
              </div>
            }
          } @else {
            <textarea
              name="body"
              rows="12"
              class="w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none"
              placeholder="Write your message"
              [(ngModel)]="body"
            ></textarea>
          }

          <div>
            <div class="flex flex-wrap items-center gap-4">
              <label class="cursor-pointer text-sm font-medium text-blue-600 hover:underline">
                Attach file(s)
                <input type="file" class="hidden" multiple (change)="addFiles($event)" />
              </label>
              <!-- An invitation is a file whose content the composer cannot
                   write itself: the server builds it from the stored event so
                   that the organizer, the sequence and the UID match what every
                   calendar client already holds. -->
              <button
                type="button"
                class="text-sm font-medium text-blue-600 hover:underline"
                (click)="openInvitePicker()"
              >
                {{ showInvite() ? 'Close invites' : 'Insert invite' }}
              </button>
            </div>
            @if (showInvite()) {
              <div class="mt-2 rounded-lg border border-slate-200 bg-slate-50 p-3">
                @if (inviteLoading()) {
                  <p class="text-sm text-slate-500">
                    {{ inviteEvents().length ? 'Preparing the invitation…' : 'Loading your upcoming events…' }}
                  </p>
                } @else if (inviteEvents().length === 0) {
                  <p class="text-sm text-slate-500">
                    Nothing is coming up in the next two months. Create the event in your calendar
                    first, then insert the invitation here.
                  </p>
                } @else {
                  <p class="mb-2 text-xs font-medium text-slate-500">
                    Attach an event as an invitation. Recipients whose mail client understands it
                    will be able to accept.
                  </p>
                  <ul class="max-h-64 space-y-1 overflow-auto">
                    @for (ev of inviteEvents(); track ev.eventId) {
                      <li>
                        <button
                          type="button"
                          class="flex w-full items-center gap-3 rounded px-2 py-1.5 text-left text-sm text-slate-700 transition hover:bg-white"
                          (click)="insertInvite(ev)"
                        >
                          <span class="w-40 shrink-0 text-xs text-slate-500">{{ formatWhen(ev.start) }}</span>
                          <span class="min-w-0 flex-1 truncate">{{ ev.summary }}</span>
                          <span class="shrink-0 text-xs text-slate-400">{{ ev.attendees.length }} invited</span>
                        </button>
                      </li>
                    }
                  </ul>
                }
              </div>
            }
            @if (attachments().length > 0) {
              <div class="mt-2 flex flex-wrap gap-2">
                @for (a of attachments(); track a.name) {
                  <span
                    class="flex items-center gap-2 rounded-lg border border-slate-200 bg-slate-50 px-3 py-1.5 text-sm"
                  >
                    <app-mail-icon name="clip" class="text-slate-400" />
                    <span class="max-w-56 truncate">{{ a.name }}</span>
                    <span class="text-xs text-slate-400">{{ formatBytes(a.size) }}</span>
                    <button
                      type="button"
                      class="text-slate-400 transition hover:text-rose-600"
                      title="Remove"
                      (click)="removeAttachment(a)"
                    >
                      <app-mail-icon name="x" [size]="12" />
                    </button>
                  </span>
                }
              </div>
            }
          </div>

          @if (error()) {
            <p class="rounded-lg border border-rose-200 bg-rose-50 px-3 py-2 text-sm text-rose-800">{{ error() }}</p>
          }

          <div class="flex flex-wrap items-center gap-2 border-t border-slate-200 pt-4">
            <button
              type="submit"
              [disabled]="busy()"
              class="flex items-center gap-2 rounded-lg bg-brand-600 px-4 py-2 text-sm font-medium text-white transition hover:bg-brand-700 disabled:cursor-not-allowed disabled:opacity-50"
            >
              <app-mail-icon name="send" /> {{ busy() ? 'Sending…' : 'Send' }}
            </button>
            <button
              type="button"
              (click)="saveDraft()"
              [disabled]="saving()"
              class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 transition hover:bg-slate-100 disabled:opacity-50"
            >
              {{ saving() ? 'Saving…' : 'Save draft' }}
            </button>
            <button type="button" (click)="cancel()" class="px-2 py-2 text-sm font-medium text-slate-500 hover:text-slate-700">
              Discard
            </button>
            <button
              type="button"
              (click)="editSignature()"
              class="ml-auto px-2 py-2 text-sm font-medium text-slate-500 hover:text-slate-700"
              title="Edit the text and images appended to everything you send"
            >
              Edit signature
            </button>
          </div>
        </div>
      </form>
    </main>
  `,
})
export class MailComposeComponent implements OnInit {
  private readonly api = inject(MailApi);
  private readonly router = inject(Router);
  private readonly auth = inject(AuthService);
  private readonly composeTarget = inject(ComposeTarget);
  private readonly calendarApi = inject(CalendarApi);
  private readonly organizationApi = inject(OrganizationApi);

  from = '';
  toRaw = '';
  ccRaw = '';
  bccRaw = '';
  subject = '';
  // body is the plain-text body, and bodyHtml the rich-text one. Which of them
  // the message is composed in is format(), which starts as HTML: a user writing
  // a message is nearly always writing prose, and prose is what the rich-text
  // editor is for. The other one is kept in step so that switching format mid
  // message does not throw away what was written.
  body = '';
  bodyHtml = signal('');

  readonly format = signal<'html' | 'text'>('html');
  readonly showPlainText = signal(false);

  readonly aliases = signal<string[]>([]);
  readonly sharedMailboxes = signal<SharedMailbox[]>([]);
  // mailbox is the shared mailbox this message is being written from. It is the
  // mailbox the sent copy is filed in, which is not the signed-in user's own.
  mailbox = '';
  readonly attachments = signal<AttachmentItem[]>([]);
  readonly busy = signal(false);
  readonly saving = signal(false);
  readonly error = signal('');
  readonly replyLabel = signal('');

  // Signature loaded from the server and shown in the editor.
  readonly signatureHtml = signal('');
  readonly signatureLoading = signal(true);

  // Threading headers a reply carries; the server only needs them to group
  // the sent copy with its conversation, so they are never user-visible.
  private inReplyTo = '';
  private references: string[] = [];
  // The server caps one attachment at 25 MiB and the whole request body at
  // 32 MiB, so 24 MiB per file is the largest size that can always fit.
  private static readonly maxFileBytes = 24 << 20;

  protected readonly formatBytes = formatBytes;
  protected readonly addressLabel = addressLabel;

  ngOnInit(): void {
    this.from = this.auth.user() ?? '';
    const pre = this.composeTarget.target();
    if (pre) {
      if (pre.subject) this.subject = pre.subject;
      if (pre.to) this.toRaw = this.formatList(pre.to);
      if (pre.cc) this.ccRaw = this.formatList(pre.cc);
      this.inReplyTo = pre.in_reply_to ?? '';
      this.references = pre.references ?? [];
      this.replyLabel.set(pre.reply_label ?? '');
      // Seed the citation of the original message, so a reply opens with the
      // text it answers already in the body rather than an empty editor.
      if (pre.quoted_html) this.bodyHtml.set(pre.quoted_html);
      if (pre.quoted_text) this.body = pre.quoted_text;
      if (pre.mailbox) {
        this.mailbox = pre.mailbox;
        this.from = pre.mailbox;
      }
      this.composeTarget.set(null);
    }
    void this.loadAliases();
    void this.loadSharedMailboxes();
    void this.loadSignature();
  }

  private async loadSignature(): Promise<void> {
    try {
      const res = await this.api.signature();
      this.signatureHtml.set(res.html ?? '');
    } catch {
      // Signature is optional; proceed without it.
    } finally {
      this.signatureLoading.set(false);
    }
  }

  // --- fields -----------------------------------------------------------------

  private async loadSharedMailboxes(): Promise<void> {
    try {
      const res = await this.api.shared();
      this.sharedMailboxes.set(res.shared ?? []);
    } catch {
      // Shared mailboxes are optional; the own address and aliases still work.
    }
  }

  // Addresses this user may write from: their own address, the aliases pointed
  // at it, and every shared mailbox they hold a grant on that allows writing.
  protected sendableAddresses(): string[] {
    const seen = new Set<string>();
    const out: string[] = [];
    const add = (address: string): void => {
      const key = address.toLowerCase();
      if (!key || seen.has(key)) return;
      seen.add(key);
      out.push(address);
    };
    add(this.from);
    for (const a of this.aliases()) add(a);
    for (const s of this.sharedMailboxes()) {
      if (s.writable) add(s.address);
    }
    return out;
  }

  // --- format -------------------------------------------------------------------

  // setFormat switches between the rich-text editor and a plain textarea.
  //
  // The body is carried across rather than discarded, because a user who starts
  // in one and moves to the other halfway through a message has not said they
  // wanted to start again. Going to plain text seeds the textarea with the text
  // the rich-text body reads as, so what was written is still there to edit.
  protected setFormat(format: 'html' | 'text'): void {
    if (format === this.format()) return;
    if (format === 'text') {
      this.body = this.body || this.plainTextAlternative();
    } else {
      this.bodyHtml.set(this.bodyHtml() || this.htmlFromPlainText());
    }
    this.format.set(format);
  }

  // plainTextAlternative is the text version of the rich-text body, which is what
  // the message is sent with alongside the HTML.
  protected plainTextAlternative(): string {
    return htmlToText(this.bodyHtml());
  }

  // htmlFromPlainText wraps a plain-text body so that switching into the editor
  // shows the same message rather than an empty one.
  private htmlFromPlainText(): string {
    if (!this.body) return '';
    const paragraphs = this.body
      .split(/\n{2,}/)
      .map((p) => `<p>${escapeHtml(p).replace(/\n/g, '<br>')}</p>`)
      .join('');
    return paragraphs;
  }

  private async loadAliases(): Promise<void> {
    try {
      const res = await this.api.ownAliases();
      const list = (res.aliases ?? []).filter((a) => a.enabled).map((a) => a.address);
      this.aliases.set([this.from, ...list.filter((a) => a.toLowerCase() !== this.from.toLowerCase())]);
    } catch {
      // Aliases are a convenience; the own address is always sendable.
    }
  }

  private formatList(addrs: Address[]): string {
    return addrs.map((a) => (a.name ? `${a.name} <${a.address}>` : a.address)).join(', ');
  }

  protected hasInvalid(raw: string): boolean {
    if (!raw.trim()) return false;
    return raw
      .split(/[,;]/)
      .map((p) => p.trim())
      .filter(Boolean)
      .some((p) => !p.includes('@'));
  }

  // --- organization contacts ---------------------------------------------------

  protected readonly showContacts = signal(false);
  protected readonly contactsLoading = signal(false);
  protected readonly people = signal<Contact[]>([]);
  protected readonly contactFilter = signal('');

  // matchingContacts is the filtered list as it is typed, capped so that an
  // organization with hundreds of people does not render hundreds of rows into
  // a composer that is being read for what it says, not for who it can reach.
  protected readonly matchingContacts = computed(() => {
    const q = this.contactFilter().trim().toLowerCase();
    const all = this.people();
    const hits = q
      ? all.filter(
          (c) =>
            c.address.toLowerCase().includes(q) ||
            (c.display_name ?? '').toLowerCase().includes(q),
        )
      : all;
    return hits.slice(0, 50);
  });

  // openContacts fetches the list the first time it is opened rather than with
  // the page: most messages do not need an address book, and those that do are
  // a click away from having one.
  protected async openContacts(): Promise<void> {
    if (this.showContacts()) {
      this.showContacts.set(false);
      return;
    }
    this.showContacts.set(true);
    if (this.people().length > 0 || this.contactsLoading()) return;
    this.contactsLoading.set(true);
    try {
      const res = await this.organizationApi.people();
      this.people.set(res.people ?? []);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.contactsLoading.set(false);
    }
  }

  // addRecipient appends an address to To without disturbing what is already
  // there: a picker that replaced the field would throw away a recipient the
  // user picked for a reason.
  protected addRecipient(address: string): void {
    const parts = this.toRaw
      .split(/[,;]/)
      .map((p) => p.trim())
      .filter(Boolean);
    if (parts.some((p) => p.toLowerCase() === address.toLowerCase())) return;
    this.toRaw = [...parts, address].join(', ');
  }

  // --- attachments -------------------------------------------------------------

  protected addFiles(event: Event): void {
    const input = event.target as HTMLInputElement;
    const files = Array.from(input.files ?? []);
    input.value = '';
    for (const file of files) {
      if (file.size > MailComposeComponent.maxFileBytes) {
        this.error.set(`“${file.name}” is ${formatBytes(file.size)}, over the 24 MiB per-file limit.`);
        continue;
      }
      void this.readOne(file).catch((err) => this.error.set(errorMessage(err)));
    }
  }

  private readOne(file: File): Promise<void> {
    return new Promise((resolve, reject) => {
      const fr = new FileReader();
      fr.onload = () => {
        this.attachments.update((cur) => [
          ...cur,
          {
            name: file.name,
            size: file.size,
            mime: file.type || 'application/octet-stream',
            content: String(fr.result),
          },
        ]);
        resolve();
      };
      fr.onerror = () => reject(fr.error ?? new Error('could not read the file'));
      fr.readAsDataURL(file);
    });
  }

  protected removeAttachment(a: AttachmentItem): void {
    this.attachments.update((cur) => cur.filter((x) => x !== a));
  }

  // --- invitations -------------------------------------------------------------

  protected readonly showInvite = signal(false);
  protected readonly inviteLoading = signal(false);
  protected readonly inviteEvents = signal<InviteCandidate[]>([]);

  protected readonly formatWhen = formatDateTime;

  // openInvitePicker lists what is coming up in the next two months across every
  // calendar the user has, because an invitation belongs to one event and the
  // composer has no idea which calendar that event was written in.
  protected async openInvitePicker(): Promise<void> {
    if (this.showInvite()) {
      this.showInvite.set(false);
      return;
    }
    this.showInvite.set(true);
    this.inviteLoading.set(true);
    this.error.set('');
    try {
      const { calendars } = await this.calendarApi.list();
      const start = new Date().toISOString();
      const end = new Date(Date.now() + 60 * 24 * 3600 * 1000).toISOString();
      const perCalendar = await Promise.all(
        (calendars ?? []).map(async (cal: CalendarModel) => {
          const res = await this.calendarApi.listEvents(cal.id, start, end);
          return (res.events ?? []).map((ev) => ({
            calendarId: ev.calendarId,
            eventId: ev.eventId,
            summary: ev.summary,
            start: ev.start,
            attendees: (ev.attendees ?? []).map((a) => a.address),
          }));
        }),
      );
      this.inviteEvents.set(
        perCalendar.flat().sort((a, b) => a.start.localeCompare(b.start)).slice(0, 100),
      );
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.inviteLoading.set(false);
    }
  }

  // insertInvite attaches the event's iTIP REQUEST.
  //
  // The bytes come from the server rather than from the client because the
  // invitation has to agree with the entry the organiser's calendar already
  // holds — same UID, same sequence, same organiser — and only the server is
  // looking at that entry.
  protected async insertInvite(ev: InviteCandidate): Promise<void> {
    this.inviteLoading.set(true);
    this.error.set('');
    try {
      const blob = await this.calendarApi.eventInvite(ev.calendarId, ev.eventId);
      const content = await blobToDataUrl(blob);
      const name = inviteFileName(ev.summary);
      this.attachments.update((cur) => [
        ...cur.filter((a) => a.name !== name),
        { name, size: blob.size, mime: 'text/calendar; charset=utf-8; method=REQUEST', content },
      ]);
      // An invitation with nobody addressed to it goes nowhere, so the
      // attendees are filled in while the fields are still untouched. Once the
      // user has typed anything the list is theirs to edit, not ours.
      if (!this.toRaw && !this.ccRaw && !this.bccRaw) {
        this.toRaw = ev.attendees.join(', ');
      }
      if (!this.subject) this.subject = `Invitation: ${ev.summary}`;
      this.showInvite.set(false);
      this.inviteEvents.set([]);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.inviteLoading.set(false);
    }
  }

  // --- submitting ----------------------------------------------------------------

  private buildRequest(requireRecipient: boolean): ComposeRequest | null {
    const to = parseAddresses(this.toRaw);
    const cc = parseAddresses(this.ccRaw);
    const bcc = parseAddresses(this.bccRaw);
    const typed = this.toRaw || this.ccRaw || this.bccRaw;
    if (typed && !to.length && !cc.length && !bcc.length) {
      this.error.set('The recipient fields have no valid address.');
      return null;
    }
    if (requireRecipient && !to.length) {
      this.error.set('A message needs at least one recipient in To.');
      return null;
    }
    const attachments = this.attachments().map((a) => ({
      filename: a.name,
      mime_type: a.mime,
      content: a.content,
    }));

    // A message composed as rich text is sent as both parts, because a recipient
    // who cannot render HTML still has to be able to read it. The text is
    // derived rather than typed, so it is whatever the HTML says.
    const html = this.format() === 'html' ? this.bodyHtml() : '';
    const text = this.format() === 'text' ? this.body : this.plainTextAlternative();

    if (requireRecipient && !htmlHasContent(html) && !text.trim() && !attachments.length) {
      this.error.set('The message has no body.');
      return null;
    }
    return {
      from: this.from || undefined,
      to,
      cc,
      bcc,
      subject: this.subject,
      text_body: text || undefined,
      html_body: html || undefined,
      in_reply_to: this.inReplyTo || undefined,
      references: this.references.length ? this.references : undefined,
      attachments: attachments.length ? attachments : undefined,
    };
  }

  // mailboxForSend is the mailbox the sent copy is filed in. Sending as an alias
  // keeps the copy in the sender's own mailbox; sending as a shared mailbox puts
  // it in that mailbox, which is where the rest of the team will look for it.
  private mailboxForSend(req: ComposeRequest): string | undefined {
    const from = (req.from ?? '').toLowerCase();
    const me = (this.auth.user() ?? '').toLowerCase();
    if (!from || from === me) return undefined;
    return this.sharedMailboxes().some((s) => s.address.toLowerCase() === from) ? from : undefined;
  }

  protected async send(): Promise<void> {
    const req = this.buildRequest(true);
    if (!req) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.send(req, this.mailboxForSend(req));
      await this.router.navigate(['/mail']);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async saveDraft(): Promise<void> {
    if (
      !this.toRaw &&
      !this.ccRaw &&
      !this.bccRaw &&
      !this.subject.trim() &&
      !this.body.trim() &&
      !htmlHasContent(this.bodyHtml()) &&
      !this.attachments().length
    ) {
      this.error.set('There is nothing to save yet.');
      return;
    }
    const req = this.buildRequest(false);
    if (!req) return;
    this.saving.set(true);
    this.error.set('');
    try {
      await this.api.saveDraft(req, this.mailboxForSend(req));
      await this.router.navigate(['/mail']);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.saving.set(false);
    }
  }

  protected cancel(): void {
    void this.router.navigate(['/mail']);
  }

  // editSignature leaves the composer for the signature editor.
  //
  // The composer is not carried over, because a signature is not part of this
  // message: coming back means starting the message again, and the draft the user
  // was working on is still in Drafts. Saving the signature is on that page
  // rather than here, so that navigating here cannot change it by accident.
  protected editSignature(): void {
    void this.router.navigate(['/mail/signature']);
  }
}

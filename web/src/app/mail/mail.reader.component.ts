import { CommonModule } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  input,
  output,
  signal,
} from '@angular/core';
import { Router } from '@angular/router';
import { AuthService } from '../core/auth/auth.service';
import { MailApi } from '../core/mail/mail-api';
import { ComposeTarget } from '../core/mail/compose-target';
import { Address, Folder, MessageDetail } from '../core/mail/mail.models';
import { errorMessage } from '../core/http-error';
import {
  FLAG_FLAGGED,
  FLAG_SEEN,
  MailIcon,
  addressLabel,
  buildReplyQuote,
  formatBytes,
  formatDate,
  hasFlag,
} from '../core/mail/ui';

// MailSelection names what the reader shows. The mail view owns this signal;
// the reader renders it and reports changes that need a list refresh back.
export type MailSelection =
  | { kind: 'message'; id: number }
  | { kind: 'thread'; key: string }
  | { kind: 'none' };

// The reading pane: one message with its body, attachments and actions, or a
// whole conversation. It loads what it is told to show and degrades to the
// plain text body when there is no HTML to render.
@Component({
  selector: 'app-mail-reader',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [CommonModule, MailIcon],
  template: `
    <section class="flex min-h-0 min-w-0 flex-1 flex-col bg-white">
      @if (error()) {
        <p class="border-b border-rose-200 bg-rose-50 px-4 py-2 text-sm text-rose-800">{{ error() }}</p>
      }
      @switch (mode()) {
        @case ('none') {
          <div class="flex flex-1 items-center justify-center p-8">
            <div class="text-center text-sm text-slate-400">
              <app-mail-icon name="inbox" [size]="40" />
              <p class="mt-3">Select a message to read it.</p>
            </div>
          </div>
        }

        @case ('thread') {
          <div class="border-b border-slate-200 px-4 py-3">
            <h2 class="text-base font-semibold text-slate-900">Conversation</h2>
            <p class="mt-0.5 text-xs text-slate-500">
              {{ thread().length }} message(s) · click one to read it
            </p>
          </div>
          <div class="min-h-0 flex-1 overflow-y-auto">
            @for (m of thread(); track m.id) {
              <button
                class="block w-full border-b border-slate-100 px-4 py-3 text-left transition hover:bg-slate-50"
                (click)="showFromThread(m)"
              >
                <div class="flex items-baseline justify-between gap-2 text-sm">
                  <span class="font-medium text-slate-900">
                    {{ (m.from ?? []).map(addressLabel).join(', ') || m.from_name }}
                  </span>
                  <span class="shrink-0 text-xs text-slate-400">{{ formatDate(m.date) }}</span>
                </div>
                <p class="mt-0.5 truncate text-sm text-slate-700">{{ m.subject }}</p>
                @if (m.text_body) {
                  <p class="mt-0.5 truncate text-xs text-slate-400">
                    {{ m.text_body | slice:0:140 }}
                  </p>
                }
              </button>
            }
          </div>
        }

        @case ('message') {
          @if (loading()) {
            <div class="flex flex-1 items-center justify-center text-sm text-slate-400">
              Loading…
            </div>
          } @else if (detail()) {
            <div class="flex flex-wrap items-center gap-x-1 gap-y-1 border-b border-slate-200 px-3 py-1.5">
              <button class="mail-btn" (click)="reply(false)">
                <app-mail-icon name="reply" /> Reply
              </button>
              <button class="mail-btn" (click)="reply(true)">
                <app-mail-icon name="users" /> Reply all
              </button>
              <button class="mail-btn" (click)="forward()">
                <app-mail-icon name="arrow" /> Forward
              </button>
              <span class="mx-1 h-5 w-px bg-slate-200"></span>
              <select
                class="mail-select"
                aria-label="Move to folder"
                [value]="moveTarget()"
                (change)="onMoveSelect($event)"
              >
                <option value="" disabled>Move to…</option>
                @for (f of folders(); track f.id) {
                  <option [value]="f.id">{{ f.name }}</option>
                }
              </select>
              <button class="mail-btn" (click)="trash()" [disabled]="busy()">
                <app-mail-icon name="trash" /> Trash
              </button>
              <button class="mail-btn mail-btn-danger" (click)="deleteForever()" [disabled]="busy()">
                <app-mail-icon name="x" /> Delete
              </button>
              <span class="mx-1 h-5 w-px bg-slate-200"></span>
              <button class="mail-btn" (click)="toggleRead()" [disabled]="busy()">
                <app-mail-icon name="inbox" /> {{ hasSeen() ? 'Unread' : 'Read' }}
              </button>
              <button class="mail-btn" (click)="toggleFlag()" [disabled]="busy()">
                <app-mail-icon name="flag" /> {{ hasFlagged() ? 'Unflag' : 'Flag' }}
              </button>
              <button class="mail-btn" (click)="downloadEml()" [disabled]="busy()">
                <app-mail-icon name="download" /> .eml
              </button>
              @if (detail()?.thread_key) {
                <button class="mail-btn ml-auto" (click)="viewConversation()">
                  <app-mail-icon name="conversation" /> Conversation
                </button>
              }
            </div>

            <div class="border-b border-slate-200 px-4 py-3">
              <h2 class="text-base font-semibold text-slate-900">{{ detail()?.subject }}</h2>
              <div class="mt-1 flex flex-wrap items-center gap-x-2 text-sm text-slate-600">
                <span class="font-medium text-slate-900">
                  {{ (detail()?.from ?? []).map(addressLabel).join(', ') || detail()?.from_name }}
                </span>
                <span class="text-slate-300">·</span>
                <span>{{ formatDate(detail()?.date ?? '') }}</span>
              </div>
              @if ((detail()?.to?.length ?? 0) > 0) {
                <p class="mt-0.5 text-xs text-slate-500">
                  To: {{ (detail()?.to ?? []).map(addressLabel).join(', ') }}
                  @if ((detail()?.cc?.length ?? 0) > 0) {
                    &nbsp;·&nbsp; Cc: {{ (detail()?.cc ?? []).map(addressLabel).join(', ') }}
                  }
                  @if ((detail()?.bcc?.length ?? 0) > 0) {
                    &nbsp;·&nbsp; Bcc: {{ (detail()?.bcc ?? []).map(addressLabel).join(', ') }}
                  }
                </p>
              }
            </div>

            <div class="flex min-h-0 flex-1 flex-col">
              @if (detail()?.html_body) {
                <iframe
                  sandbox
                  title="message body"
                  class="min-h-0 w-full flex-1 bg-white"
                  [srcdoc]="srcdoc() ?? 'Loading…'"
                ></iframe>
              } @else if (detail()?.text_body) {
                <pre
                  class="min-h-0 w-full flex-1 overflow-y-auto whitespace-pre-wrap p-4 font-sans text-sm leading-relaxed text-slate-800"
                  >{{ detail()?.text_body }}</pre
                >
              } @else {
                <p class="p-4 text-sm text-slate-500">This message has no readable body.</p>
              }
            </div>

            @if ((detail()?.attachments ?? []).length > 0) {
              <div class="border-t border-slate-200 px-4 py-3">
                <h3 class="text-xs font-semibold uppercase tracking-wide text-slate-400">
                  Attachments
                </h3>
                <div class="mt-2 flex flex-wrap gap-2">
                  @for (a of detail()?.attachments ?? []; track a.index) {
                    <button
                      class="flex items-center gap-2 rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 text-left transition hover:bg-slate-100"
                      (click)="downloadAttachment(a)"
                      [title]="'Download ' + a.filename"
                    >
                      <app-mail-icon name="clip" class="text-slate-400" />
                      <span>
                        <span class="block max-w-56 truncate text-sm font-medium text-slate-800">
                          {{ a.filename }}
                        </span>
                        <span class="block text-xs text-slate-400">
                          {{ formatBytes(a.size) }} · {{ a.mime_type }}
                        </span>
                      </span>
                    </button>
                  }
                </div>
              </div>
            }

            <details class="border-t border-slate-200 px-4 py-3">
              <summary class="cursor-pointer text-xs font-medium text-slate-500">Details</summary>
              <dl class="mt-2 max-h-48 overflow-y-auto text-xs">
                @for (h of detail()?.headers ?? []; track h.name) {
                  <div class="flex gap-2 border-b border-slate-100 py-1">
                    <dt class="w-36 shrink-0 break-all font-mono text-slate-500">{{ h.name }}</dt>
                    <dd class="break-all text-slate-700">{{ h.value }}</dd>
                  </div>
                }
              </dl>
            </details>
          }
        }
      }
    </section>
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
    }
    .mail-btn app-mail-icon { margin-top: 0; }
  `,
  ],
})
export class MailReaderComponent {
  private readonly api = inject(MailApi);
  private readonly router = inject(Router);
  private readonly composeTarget = inject(ComposeTarget);
  private readonly auth = inject(AuthService);

  readonly selection = input.required<MailSelection>();
  readonly folders = input.required<Folder[]>();
  readonly changed = output<void>();
  readonly closed = output<void>();

  protected readonly mode = signal<'none' | 'message' | 'thread'>('none');
  protected readonly detail = signal<MessageDetail | null>(null);
  protected readonly thread = signal<MessageDetail[]>([]);
  protected readonly srcdoc = signal<string | null>(null);
  protected readonly loading = signal(false);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly moveTarget = signal('');

  protected readonly addressLabel = addressLabel;
  protected readonly formatDate = formatDate;
  protected readonly formatBytes = formatBytes;

  private loadSeq = 0;

  constructor() {
    effect(() => {
      const sel = this.selection();
      this.error.set('');
      this.moveTarget.set('');
      if (sel.kind === 'none') {
        this.loadSeq++;
        this.mode.set('none');
        this.detail.set(null);
        this.thread.set([]);
        this.srcdoc.set(null);
        return;
      }
      void this.load(sel);
    });
  }

  protected hasSeen(): boolean {
    return hasFlag(this.detail()?.flags ?? [], FLAG_SEEN);
  }

  protected hasFlagged(): boolean {
    return hasFlag(this.detail()?.flags ?? [], FLAG_FLAGGED);
  }

  // --- selection loading -----------------------------------------------------

  private async load(sel: MailSelection): Promise<void> {
    const seq = ++this.loadSeq;
    this.loading.set(true);
    try {
      if (sel.kind === 'message') {
        const d = await this.api.get(sel.id);
        if (seq !== this.loadSeq) return;
        this.detail.set(d);
        this.thread.set([]);
        this.mode.set('message');
        this.loading.set(false);
        // Opening is what marks it read; the server does not, and the list
        // learns about it through changed.
        if (!hasFlag(d.flags, FLAG_SEEN)) {
          await this.api.markRead(sel.id, true).catch(() => undefined);
          this.changed.emit();
        }
        void this.renderBody(d);
      } else if (sel.kind === 'thread') {
        const res = await this.api.thread(sel.key);
        if (seq !== this.loadSeq) return;
        this.thread.set(res.messages);
        this.detail.set(null);
        this.srcdoc.set(null);
        this.mode.set('thread');
        this.loading.set(false);
      }
    } catch (err) {
      if (seq !== this.loadSeq) return;
      this.error.set(errorMessage(err));
      this.loading.set(false);
    }
  }

  // The body renders in a sandboxed frame: no scripts, no same-origin access,
  // no top-level form submission, so hostile markup can be looked at but do
  // nothing to the app. cid: references are swapped for the inline parts'
  // data URLs first, because the frame could never fetch them itself.
  private async renderBody(d: MessageDetail): Promise<void> {
    const seq = this.loadSeq;
    this.srcdoc.set(null);
    const html = d.html_body ?? '';
    if (!html) return;
    const images = d.inline_images ?? [];
    const swapped: Array<[string, string]> = [];
    await Promise.all(
      images.map(async (img) => {
        try {
          const blob = await this.api.inlineImage(d.id, img.content_id);
          swapped.push([img.content_id, await this.toDataUrl(blob)]);
        } catch {
          // A part that will not load degrades to a broken reference inside
          // the frame; the rest of the body still renders.
        }
      }),
    );
    if (seq !== this.loadSeq) return;
    let out = `<base target="_blank">\n${html}`;
    for (const [cid, dataUrl] of swapped) {
      out = out.split(`cid:${cid}`).join(dataUrl).split(`cid<${cid}>`).join(dataUrl);
    }
    this.srcdoc.set(out);
  }

  private toDataUrl(blob: Blob): Promise<string> {
    return new Promise((resolve, reject) => {
      const fr = new FileReader();
      fr.onload = () => resolve(String(fr.result));
      fr.onerror = () => reject(fr.error ?? new Error('read failed'));
      fr.readAsDataURL(blob);
    });
  }

  // --- thread view -----------------------------------------------------------

  protected showFromThread(m: MessageDetail): void {
    this.detail.set(m);
    this.srcdoc.set(null);
    this.mode.set('message');
    if (m.html_body) {
      void this.renderBody(m);
    }
  }

  protected viewConversation(): void {
    const d = this.detail();
    if (d?.thread_key) {
      void this.load({ kind: 'thread', key: d.thread_key });
    }
  }

  // --- composing from the reader ----------------------------------------------

  private header(d: MessageDetail, name: string): string {
    const n = name.toLowerCase();
    return (d.headers ?? []).find((h) => h.name.toLowerCase() === n)?.value ?? '';
  }

  protected reply(all: boolean): void {
    const d = this.detail();
    if (!d) return;
    const me = this.auth.user();
    const sender = d.reply_to?.length ? d.reply_to : d.from ?? [];
    const to = all ? this.collectRecipients(d, me) : [...sender];
    const msgId = this.header(d, 'message-id');
    const references = this.header(d, 'references')
      ? this.header(d, 'references')
          .split(/\s+/)
          .filter(Boolean)
      : [];
    if (msgId) references.push(msgId);
    const subject = /^re:/i.test(d.subject) ? d.subject : `Re: ${d.subject}`;
    const quote = buildReplyQuote(
      `On ${formatDate(d.date)}, ${(d.from ?? []).map(addressLabel).join(', ')} wrote:`,
      d.html_body ?? '',
      d.text_body ?? '',
    );
    this.composeTarget.set({
      to,
      subject,
      in_reply_to: msgId,
      references,
      reply_label: `in reply to ${(d.from ?? []).map(addressLabel).join(', ')}`,
      quoted_html: quote.quoted_html,
      quoted_text: quote.quoted_text,
    });
    void this.router.navigate(['/mail/compose']);
  }

  private collectRecipients(d: MessageDetail, me: string): Address[] {
    const out: Address[] = [];
    const seen = new Set<string>();
    const add = (a: Address): void => {
      const k = a.address.toLowerCase();
      if (!seen.has(k) && k !== me.toLowerCase()) {
        seen.add(k);
        out.push(a);
      }
    };
    (d.reply_to?.length ? d.reply_to : d.from ?? []).forEach(add);
    d.to?.forEach(add);
    d.cc?.forEach(add);
    return out;
  }

  protected forward(): void {
    const d = this.detail();
    if (!d) return;
    const quote = buildReplyQuote(
      `---------- Forwarded message ----------\nFrom: ${(d.from ?? []).map(addressLabel).join(', ')}\nDate: ${formatDate(d.date)}\nSubject: ${d.subject}`,
      d.html_body ?? '',
      d.text_body ?? '',
    );
    this.composeTarget.set({
      to: [],
      subject: /^fwd:/i.test(d.subject) ? d.subject : `Fwd: ${d.subject}`,
      reply_label: 'forwarding',
      quoted_html: quote.quoted_html,
      quoted_text: quote.quoted_text,
    });
    void this.router.navigate(['/mail/compose']);
  }

  // --- message actions ---------------------------------------------------------

  protected onMoveSelect(event: Event): void {
    const value = (event.target as HTMLSelectElement).value;
    void this.moveTo(value);
  }

  protected async moveTo(folderId: string): Promise<void> {
    const d = this.detail();
    const id = Number(folderId);
    if (!d || !id) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.move([d.id], id);
      this.moveTarget.set('');
      this.changed.emit();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async trash(): Promise<void> {
    const d = this.detail();
    if (!d) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.trash([d.id]);
      this.changed.emit();
      this.closed.emit();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async deleteForever(): Promise<void> {
    const d = this.detail();
    if (!d) return;
    if (!confirm('Delete this message permanently? This cannot be undone.')) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.delete([d.id]);
      this.changed.emit();
      this.closed.emit();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async toggleRead(): Promise<void> {
    const d = this.detail();
    if (!d) return;
    const read = !hasFlag(d.flags, FLAG_SEEN);
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.markRead(d.id, read);
      this.changed.emit();
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async toggleFlag(): Promise<void> {
    const d = this.detail();
    if (!d) return;
    const set = !hasFlag(d.flags, FLAG_FLAGGED);
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.setFlag(d.id, FLAG_FLAGGED, set);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  private async reload(): Promise<void> {
    const d = this.detail();
    if (!d) return;
    const fresh = await this.api.get(d.id);
    this.detail.set(fresh);
  }

  protected async downloadEml(): Promise<void> {
    const d = this.detail();
    if (!d) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.download(await this.api.raw(d.id), `${d.subject || String(d.id)}.eml`);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async downloadAttachment(a: { index: number; filename: string }): Promise<void> {
    const d = this.detail();
    if (!d) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.download(await this.api.attachment(d.id, a.index), a.filename);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  private download(blob: Blob, filename: string): Promise<void> {
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10_000);
    return Promise.resolve();
  }
}

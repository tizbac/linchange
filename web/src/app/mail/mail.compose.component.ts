import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';
import { AuthService } from '../core/auth/auth.service';
import { MailApi } from '../core/mail/mail-api';
import { ComposeTarget } from '../core/mail/compose-target';
import { Address, ComposeRequest } from '../core/mail/mail.models';
import { errorMessage } from '../core/http-error';
import { MailIcon, addressLabel, formatBytes, parseAddresses } from '../core/mail/ui';

// One file the composer will attach, read as a data URL the server accepts.
interface AttachmentItem {
  name: string;
  size: number;
  mime: string;
  content: string;
}

// The composer: From (the user plus their send-as aliases), To/Cc/Bcc with a
// plain-text preview so a mistyped recipient is visible before it is sent, a
// body, and files. Replies and forwards arrive pre-filled through the
// ComposeTarget service and are consumed once.
@Component({
  selector: 'app-mail-compose',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, MailIcon],
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
              @for (a of identities(); track a) {
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

          <div class="flex flex-wrap items-center gap-3 text-sm">
            <label class="w-20 shrink-0 text-right font-medium text-slate-600" for="mc-subject">Subject</label>
            <input
              id="mc-subject"
              name="subject"
              class="min-w-0 flex-1 rounded-lg border border-slate-300 px-3 py-1.5 text-sm focus:border-brand-500 focus:outline-none"
              [(ngModel)]="subject"
            />
          </div>

          <textarea
            name="body"
            rows="12"
            class="w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none"
            placeholder="Write your message"
            [(ngModel)]="body"
          ></textarea>

          <div>
            <label class="cursor-pointer text-sm font-medium text-blue-600 hover:underline">
              Attach file(s)
              <input type="file" class="hidden" multiple (change)="addFiles($event)" />
            </label>
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

  from = '';
  toRaw = '';
  ccRaw = '';
  bccRaw = '';
  subject = '';
  body = '';

  readonly aliases = signal<string[]>([]);
  readonly attachments = signal<AttachmentItem[]>([]);
  readonly busy = signal(false);
  readonly saving = signal(false);
  readonly error = signal('');
  readonly replyLabel = signal('');

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
      this.composeTarget.set(null);
    }
    void this.loadAliases();
  }

  // --- fields -----------------------------------------------------------------

  protected identities(): string[] {
    return this.aliases();
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
    if (requireRecipient && !this.body.trim() && !attachments.length) {
      this.error.set('The message has no body.');
      return null;
    }
    return {
      from: this.from || undefined,
      to,
      cc,
      bcc,
      subject: this.subject,
      text_body: this.body || undefined,
      in_reply_to: this.inReplyTo || undefined,
      references: this.references.length ? this.references : undefined,
      attachments: attachments.length ? attachments : undefined,
    };
  }

  protected async send(): Promise<void> {
    const req = this.buildRequest(true);
    if (!req) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.send(req);
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
      await this.api.saveDraft(req);
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
}

import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { Router } from '@angular/router';
import { MailApi } from '../core/mail/mail-api';
import { errorMessage } from '../core/http-error';
import { MailIcon } from '../core/mail/ui';
import {
  RichTextEditorComponent,
  htmlHasContent,
  htmlToText,
} from '../core/mail/rich-text';

// The signature editor.
//
// A signature is kept once and appended to everything the user sends, so this is
// a page about something that happens on their behalf rather than about a single
// message. Three things it has to make clear, because each of them is a
// reasonable thing for a user to wonder:
//
// That the signature is theirs and goes out on their mail. It is stored against
// their mailbox and added when a message is sent; a draft does not carry it, so
// saving a draft a dozen times does not leave a dozen signatures behind.
//
// That images are sent as message parts rather than as inline text. A browser
// produces a data: URI for a pasted image, and that form is not rendered by
// Outlook and is stripped by several webmail clients, so it is decoded into a
// MIME part with a Content-ID the body refers to. Nothing to configure, but the
// size of what is pasted is the size of the message, which is worth saying.
//
// That what the plain-text reader sees is derived, not written twice. It is
// shown here so it can be checked, and it cannot be edited separately because
// two copies of a signature is one more thing to keep in step.
@Component({
  selector: 'app-signature',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RichTextEditorComponent, MailIcon],
  template: `
    <main class="mx-auto max-w-3xl px-6 py-8">
      <div class="flex items-center justify-between gap-3">
        <h1 class="text-lg font-semibold text-slate-900">Signature</h1>
        <button
          class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 transition hover:bg-slate-100"
          (click)="back()"
        >
          Back to mail
        </button>
      </div>

      <p class="mt-2 text-sm text-slate-600">
        Appended to the bottom of every message you send. It is not added to drafts, so a draft you
        save repeatedly does not collect copies of it.
      </p>

      @if (error()) {
        <p class="mt-4 rounded-lg border border-rose-200 bg-rose-50 px-3 py-2 text-sm text-rose-800">
          {{ error() }}
        </p>
      }
      @if (saved()) {
        <p class="mt-4 rounded-lg border border-emerald-200 bg-emerald-50 px-3 py-2 text-sm text-emerald-800">
          Signature saved.
        </p>
      }

      <div class="mt-5 space-y-4">
        @if (loading()) {
          <p class="text-sm text-slate-500">Loading your signature…</p>
        } @else {
          <app-rich-text-editor
            [html]="html()"
            [placeholder]="'Your name, role and contact details'"
            (htmlChange)="onHtml($event)"
          />

          @if (htmlHasContent(html())) {
            <div class="rounded-lg border border-slate-200 bg-slate-50 px-3 py-2">
              <p class="mb-1 text-xs font-medium text-slate-500">As a plain-text reader will see it</p>
              <pre class="whitespace-pre-wrap text-xs text-slate-700">{{ plainText() }}</pre>
            </div>
          }

          <p class="text-xs text-slate-500">
            An image pasted here is sent as a proper part of the message rather than as inline text,
            so it displays in Outlook and other clients. Inline images make the message larger by
            about a third, so a logo is worth it and a screenshot often is not.
          </p>

          <div class="flex flex-wrap items-center gap-2 border-t border-slate-200 pt-4">
            <button
              type="button"
              [disabled]="busy()"
              class="rounded-lg bg-brand-600 px-4 py-2 text-sm font-medium text-white transition hover:bg-brand-700 disabled:opacity-50"
              (click)="save()"
            >
              {{ busy() ? 'Saving…' : 'Save signature' }}
            </button>
            @if (htmlHasContent(html())) {
              <button
                type="button"
                [disabled]="busy()"
                class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 transition hover:bg-slate-100 disabled:opacity-50"
                (click)="clear()"
              >
                Remove
              </button>
            }
          </div>
        }
      </div>
    </main>
  `,
})
export class SignatureComponent implements OnInit {
  private readonly api = inject(MailApi);
  private readonly router = inject(Router);

  protected readonly htmlHasContent = htmlHasContent;

  // html is the signature being edited. It is not written to the server on every
  // keystroke: the editor is the user's work in progress and a save is a
  // deliberate act, so there is a button.
  readonly html = signal('');
  readonly loading = signal(true);
  readonly busy = signal(false);
  readonly error = signal('');
  readonly saved = signal(false);

  ngOnInit(): void {
    void this.load();
  }

  private async load(): Promise<void> {
    try {
      const res = await this.api.signature();
      this.html.set(res.html ?? '');
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected onHtml(value: string): void {
    this.html.set(value);
    // Any edit means the last save no longer describes what is on screen, so the
    // confirmation is withdrawn rather than left sitting there next to a body
    // that has since changed.
    this.saved.set(false);
  }

  protected plainText(): string {
    return htmlToText(this.html());
  }

  protected async save(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    this.saved.set(false);
    try {
      const res = await this.api.saveSignature(this.html());
      this.html.set(res.html ?? '');
      this.saved.set(true);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async clear(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    this.saved.set(false);
    try {
      await this.api.clearSignature();
      this.html.set('');
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected back(): void {
    void this.router.navigate(['/mail']);
  }
}

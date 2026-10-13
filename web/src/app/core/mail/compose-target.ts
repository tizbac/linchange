import { Injectable, computed, signal } from '@angular/core';
import { Address } from './mail.models';

// ComposePrefill is what opens the composer on somebody's behalf: a reply, a
// reply-all or a forward. The mail view writes it, the composer reads it once
// and clears it, so a later plain visit to /mail/compose starts empty.
export interface ComposePrefill {
  to?: Address[];
  cc?: Address[];
  subject?: string;
  in_reply_to?: string;
  references?: string[];
  reply_label?: string;
  // quoted_html and quoted_text are the original message, cited in the body so
  // the recipient can read what is being answered. quoted_html goes in the
  // rich-text editor and quoted_text in the plain-text one; both carry the
  // attribution line ("On <date>, <sender> wrote:") ahead of the citation.
  quoted_html?: string;
  quoted_text?: string;
}

@Injectable({ providedIn: 'root' })
export class ComposeTarget {
  private readonly _target = signal<ComposePrefill | null>(null);

  readonly target = this._target.asReadonly();
  readonly active = computed(() => this._target() !== null);

  set(target: ComposePrefill | null): void {
    this._target.set(target);
  }
}

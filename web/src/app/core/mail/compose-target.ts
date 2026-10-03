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

import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { OutboxMessageState, OutboxMessageView } from '../core/admin/admin.models';
import { errorMessage } from '../core/http-error';
import {
  AdminButton,
  Busy,
  Notice,
  PageEmpty,
  PageHeader,
  Panel,
  inputClass,
  tableCellClass,
  tableHeadClass,
} from '../core/admin/ui';
import { AdminNavComponent } from '../core/admin/admin-nav.component';
import { AdminPageComponent } from '../core/admin/admin-page.component';
import { AdminListFooter, AdminSearch } from '../core/admin/admin-list';

// The queue is where undelivered mail can be looked at: what was submitted,
// who it was going to, what each attempt said, and a way to try a stuck
// message again. Every state the delivery worker uses is offered as a filter,
// because the question is usually "what failed?" rather than "what is here?".
const STATES: OutboxMessageState[] = ['queued', 'sending', 'sent', 'deferred', 'failed', 'bounced'];

@Component({
  selector: 'app-admin-queue',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    FormsModule,
    AdminButton,
    Busy,
    Notice,
    PageEmpty,
    PageHeader,
    Panel,
    AdminNavComponent,
    AdminPageComponent,
    AdminSearch,
    AdminListFooter,
  ],
  templateUrl: './admin-queue.component.html',
})
export class AdminQueueComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly input = inputClass;
  protected readonly states = STATES;

  protected readonly messages = signal<OutboxMessageView[]>([]);
  protected readonly total = signal(0);
  protected query = '';
  protected page = 1;
  protected readonly pageSize = 20;
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');
  // The opened message, with its recipients. One at a time: the detail is
  // read against the row it came from, and two open messages would put their
  // recipients side by side for no reason.
  protected readonly detail = signal<OutboxMessageView | null>(null);

  // Empty means "every visible state". Sent is separate: it is hidden by
  // default because the queue answers "what has not gone out", and asking for
  // it is a deliberate act.
  protected readonly selectedStates = signal<OutboxMessageState[]>([]);
  protected showSent = false;
  protected submitterFilter = '';

  ngOnInit(): void {
    void this.reload();
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listOutbox({
        states: this.selectedStates(),
        include_sent: this.showSent,
        submitter: this.submitterFilter.trim() || undefined,
        search: this.query.trim() || undefined,
        limit: this.pageSize,
        offset: (this.page - 1) * this.pageSize,
      });
      this.messages.set(res.messages ?? []);
      this.total.set(res.total);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected search(value: string): void {
    this.query = value;
    this.page = 1;
    void this.reload();
  }

  protected goPage(page: number): void {
    this.page = page;
    void this.reload();
  }

  protected isStateSelected(state: OutboxMessageState): boolean {
    return this.selectedStates().includes(state);
  }

  protected toggleState(state: OutboxMessageState): void {
    const current = this.selectedStates();
    this.selectedStates.set(
      current.includes(state) ? current.filter((s) => s !== state) : [...current, state],
    );
    this.page = 1;
    void this.reload();
  }

  protected setShowSent(show: boolean): void {
    this.showSent = show;
    this.page = 1;
    void this.reload();
  }

  protected clearFilters(): void {
    this.selectedStates.set([]);
    this.showSent = false;
    this.submitterFilter = '';
    this.query = '';
    this.page = 1;
    void this.reload();
  }

  // deleteAll removes the whole set the current filters describe, not only the
  // page on screen. The confirmation names the count so the warning is about
  // something concrete.
  protected async deleteAll(): Promise<void> {
    const filters = this.selectedStates().length
      ? `states: ${this.selectedStates().join(', ')}`
      : `all visible states${this.showSent ? ' including sent' : ' excluding sent'}`;
    const search = this.query.trim() || this.submitterFilter.trim();
    const scope = search ? `${filters}; matching "${search}"` : filters;
    if (
      !confirm(
        `Delete every message in the queue matching ${scope}?\n\n` +
          `This removes ${this.total()} message${this.total() === 1 ? '' : 's'} from the ` +
          'queue and cannot be undone. The messages themselves are not recalled.',
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const res = await this.api.deleteOutbox({
        states: this.selectedStates(),
        include_sent: this.showSent,
        submitter: this.submitterFilter.trim() || undefined,
        search: this.query.trim() || undefined,
      });
      this.ok.set(
        `${res.deleted} queue message${res.deleted === 1 ? ' was' : 's were'} deleted.`,
      );
      this.page = 1;
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // open fetches the message with its recipients. The list deliberately does
  // not carry them: a listing with one query per row is how this page becomes
  // the slowest one exactly when the server is busiest.
  protected async open(msg: OutboxMessageView): Promise<void> {
    this.error.set('');
    try {
      this.detail.set(await this.api.getOutbox(msg.id));
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected closeDetail(): void {
    this.detail.set(null);
  }

  protected async requeue(msg: OutboxMessageView): Promise<void> {
    if (
      !confirm(
        `Try message ${msg.id} again now?\n\nIts refused recipients are put back in the retry set; ` +
          'the ones that already received it are left alone.',
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      await this.api.requeueOutbox(msg.id);
      this.ok.set(`Message ${msg.id} is queued for another attempt.`);
      await this.reload();
      if (this.detail()?.id === msg.id) {
        await this.open(msg);
      }
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // canRequeue is the button's whole rule: a message waiting (queued or
  // deferred) or one given up on (failed or bounced) can be tried again. One
  // in flight is not touched, and one already sent has nothing left to retry —
  // the server refuses both, so the control is not offered rather than
  // failing on the click.
  protected canRequeue(msg: OutboxMessageView): boolean {
    return msg.state === 'queued' || msg.state === 'deferred' || msg.state === 'failed' || msg.state === 'bounced';
  }

  protected stateTone(state: OutboxMessageState): string {
    switch (state) {
      case 'sent':
        return 'bg-emerald-50 text-emerald-700 ring-emerald-200';
      case 'failed':
      case 'bounced':
        return 'bg-rose-50 text-rose-700 ring-rose-200';
      case 'deferred':
        return 'bg-amber-50 text-amber-700 ring-amber-200';
      case 'sending':
        return 'bg-sky-50 text-sky-700 ring-sky-200';
      default:
        return 'bg-slate-100 text-slate-600 ring-slate-200';
    }
  }

  protected recipientTone(state: string): string {
    switch (state) {
      case 'delivered':
        return 'bg-emerald-50 text-emerald-700 ring-emerald-200';
      case 'failed':
        return 'bg-rose-50 text-rose-700 ring-rose-200';
      case 'deferred':
        return 'bg-amber-50 text-amber-700 ring-amber-200';
      default:
        return 'bg-slate-100 text-slate-600 ring-slate-200';
    }
  }
}

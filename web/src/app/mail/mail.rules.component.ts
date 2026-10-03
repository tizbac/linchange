import { ChangeDetectionStrategy, Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { MailApi } from '../core/mail/mail-api';
import { Folder, Rule } from '../core/mail/mail.models';
import { errorMessage } from '../core/http-error';
import { MailIcon } from '../core/mail/ui';

// The rules editor. The vocabulary is closed on the server, so the form offers
// only what the engine implements; save re-checks the same things the server
// will, so a round-trip does not come back as a 400 the form should have caught.
@Component({
  selector: 'app-mail-rules',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, MailIcon],
  template: `
    <main class="mx-auto max-w-4xl px-6 py-8">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 class="text-lg font-semibold text-slate-900">Rules</h1>
          <p class="mt-1 max-w-xl text-sm text-slate-500">
            Rules run in priority order on every arriving message. When all of a rule's conditions
            match, its actions run.
          </p>
        </div>
        <button
          class="flex items-center gap-2 rounded-lg bg-brand-600 px-4 py-2 text-sm font-medium text-white transition hover:bg-brand-700"
          (click)="newRule()"
        >
          <app-mail-icon name="plus" /> New rule
        </button>
      </div>

      @if (error()) {
        <p class="mt-4 rounded-lg border border-rose-200 bg-rose-50 px-3 py-2 text-sm text-rose-800">{{ error() }}</p>
      }

      <div class="mt-4 space-y-2">
        @if (loading()) {
          <p class="p-2 text-sm text-slate-400">Loading…</p>
        } @else if (rules().length === 0) {
          <p class="rounded-xl border border-dashed border-slate-300 p-6 text-center text-sm text-slate-400">
            No rules yet. New mail is filed in your Inbox.
          </p>
        } @else {
          @for (r of rules(); track r.id) {
            <div class="flex flex-wrap items-center gap-3 rounded-xl border border-slate-200 bg-white px-4 py-3">
              <input
                type="checkbox"
                [checked]="r.enabled"
                (change)="toggleEnabled(r, $event)"
                title="Enabled"
              />
              <div class="min-w-0 flex-1">
                <span
                  class="text-sm font-medium"
                  [class.text-slate-900]="r.enabled"
                  [class.text-slate-400]="!r.enabled"
                  [class.line-through]="!r.enabled"
                  >{{ r.name }}</span
                >
                <span class="ml-2 text-xs text-slate-400">priority {{ r.priority }}</span>
                <p class="mt-0.5 break-words text-xs text-slate-500">{{ summarize(r) }}</p>
              </div>
              <button class="rule-link" (click)="editRule(r)">Edit</button>
              <button class="rule-link rule-link-danger" (click)="deleteRule(r)">Delete</button>
            </div>
          }
        }
      </div>

      @if (editing(); as d) {
        <form class="mt-6 rounded-xl border border-slate-300 bg-white p-5" (ngSubmit)="save()">
          <h2 class="text-base font-semibold text-slate-900">{{ d.id ? 'Edit rule' : 'New rule' }}</h2>

          <div class="mt-4 flex flex-wrap items-end gap-4">
            <label class="flex flex-col gap-1 text-xs font-medium text-slate-600">
              Name
              <input
                class="rrule rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-normal focus:border-brand-500 focus:outline-none"
                placeholder="e.g. Invoices to the archive"
                [ngModel]="d.name"
                (ngModelChange)="d.name = $event"
              />
            </label>
            <label class="flex flex-col gap-1 text-xs font-medium text-slate-600">
              Priority
              <input
                type="number"
                min="0"
                class="rrule w-24 rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-normal focus:border-brand-500 focus:outline-none"
                [ngModel]="d.priority"
                (ngModelChange)="setPriority(d, $event)"
              />
            </label>
            <label class="mb-1.5 flex items-center gap-2 text-xs font-medium text-slate-600">
              <input type="checkbox" [ngModel]="d.stop" (ngModelChange)="d.stop = $event" />
              stop processing later rules
            </label>
          </div>

          <h3 class="mt-6 text-sm font-semibold text-slate-900">Conditions — all must match</h3>
          <div class="mt-2 space-y-2">
            @for (c of d.conditions; track $index) {
              <div class="flex flex-wrap items-center gap-2 rounded-lg bg-slate-50 p-2">
                <label class="flex items-center gap-1.5 text-xs text-slate-600" title="Negate this condition">
                  <input type="checkbox" [ngModel]="c.negate" (ngModelChange)="c.negate = $event" /> NOT
                </label>
                <select class="rrule" [ngModel]="c.field" (ngModelChange)="c.field = $event">
                  @for (f of FIELDS; track f) {
                    <option [value]="f">{{ f }}</option>
                  }
                </select>
                <select class="rrule" [ngModel]="c.operator" (ngModelChange)="c.operator = $event">
                  @for (o of OPERATORS; track o) {
                    <option [value]="o">{{ o }}</option>
                  }
                </select>
                @if (c.field === 'header') {
                  <input
                    class="rrule w-40"
                    placeholder="Header name"
                    [ngModel]="c.header"
                    (ngModelChange)="c.header = $event"
                  />
                }
                @if (needsValue(c.operator)) {
                  <input
                    class="rrule min-w-44 flex-1"
                    placeholder="{{ c.operator === 'matches' ? 'Regular expression' : 'Value' }}"
                    [ngModel]="c.value"
                    (ngModelChange)="c.value = $event"
                  />
                }
                <label class="flex items-center gap-1.5 text-xs text-slate-600">
                  <input type="checkbox" [ngModel]="c.case_sensitive" (ngModelChange)="c.case_sensitive = $event" />
                  case-sensitive
                </label>
                <button type="button" class="rule-link rule-link-danger" (click)="removeCondition(d, $index)">
                  Remove
                </button>
              </div>
            }
            @if (d.conditions.length === 0) {
              <p class="text-xs text-slate-400">No conditions — the rule will not save.</p>
            }
          </div>
          <button type="button" class="rule-link mt-2" (click)="addCondition(d)">+ Add condition</button>

          <h3 class="mt-6 text-sm font-semibold text-slate-900">Actions</h3>
          <div class="mt-2 space-y-2">
            @for (a of d.actions; track $index) {
              <div class="flex flex-wrap items-center gap-2 rounded-lg bg-slate-50 p-2">
                <select class="rrule" [ngModel]="a.type" (ngModelChange)="a.type = $event">
                  @for (t of ACTION_TYPES; track t) {
                    <option [value]="t">{{ t }}</option>
                  }
                </select>
                @if (a.type === 'move') {
                  <select class="rrule" [ngModel]="a.target" (ngModelChange)="a.target = $event">
                    @for (n of folderNames(); track n) {
                      <option [value]="n">{{ n }}</option>
                    }
                  </select>
                }
                @if (a.type === 'forward') {
                  <input
                    class="rrule min-w-52"
                    placeholder="forward@example.com"
                    [ngModel]="a.target"
                    (ngModelChange)="a.target = $event"
                  />
                }
                <label class="flex items-center gap-1.5 text-xs text-slate-600">
                  <input type="checkbox" [ngModel]="a.stop" (ngModelChange)="a.stop = $event" />
                  stop after
                </label>
                <button type="button" class="rule-link rule-link-danger" (click)="removeAction(d, $index)">
                  Remove
                </button>
              </div>
            }
            @if (d.actions.length === 0) {
              <p class="text-xs text-slate-400">No actions — the rule will not save.</p>
            }
          </div>
          <button type="button" class="rule-link mt-2" (click)="addAction(d)">+ Add action</button>

          <div class="mt-6 flex items-center gap-2 border-t border-slate-200 pt-4">
            <button
              type="submit"
              [disabled]="saving()"
              class="rounded-lg bg-brand-600 px-4 py-2 text-sm font-medium text-white transition hover:bg-brand-700 disabled:opacity-50"
            >
              {{ saving() ? 'Saving…' : 'Save rule' }}
            </button>
            <button
              type="button"
              (click)="editing.set(null)"
              class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 transition hover:bg-slate-100"
            >
              Cancel
            </button>
          </div>
        </form>
      }
    </main>
  `,
  styles: [
    `.rrule {
      border-radius: 0.5rem;
      border: 1px solid #cbd5e1;
      background: #fff;
      padding: 6px 8px;
      font-size: 13px;
      color: #1e293b;
    }
    .rule-link {
      font-size: 12px;
      font-weight: 500;
      color: #2563eb;
    }
    .rule-link:hover { text-decoration: underline; }
    .rule-link-danger { color: #e11d48; }
  `,
  ],
})
export class MailRulesComponent implements OnInit {
  private readonly api = inject(MailApi);

  // The closed vocabularies the engine implements; the server rejects anything
  // outside them, so the form offers exactly these and nothing else.
  protected readonly FIELDS = [
    'from', 'to', 'cc', 'subject', 'body', 'attachment', 'header', 'size', 'sender_domain',
  ];
  protected readonly OPERATORS = [
    'contains', 'is', 'starts_with', 'ends_with', 'matches', 'exists', 'greater_than', 'less_than', 'is_true',
  ];
  protected readonly ACTION_TYPES = ['move', 'mark_read', 'mark_flagged', 'mark_unflagged', 'forward', 'discard'];

  readonly rules = signal<Rule[]>([]);
  readonly folders = signal<Folder[]>([]);
  readonly loading = signal(true);
  readonly saving = signal(false);
  readonly error = signal('');
  readonly editing = signal<Rule | null>(null);

  readonly folderNames = computed(() => this.folders().map((f) => f.name));

  ngOnInit(): void {
    void this.refresh();
  }

  private async refresh(): Promise<void> {
    try {
      const [r, f] = await Promise.all([this.api.listRules(), this.api.listFolders()]);
      this.rules.set(r.rules ?? []);
      this.folders.set(f.folders ?? []);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected needsValue(op: string): boolean {
    return op !== 'exists' && op !== 'is_true';
  }

  protected setPriority(d: Rule, v: string | number): void {
    const n = Number(v);
    d.priority = Number.isFinite(n) ? n : 0;
  }

  protected summarize(r: Rule): string {
    const conds = (r.conditions ?? []).map((c) => {
      const target = c.field === 'header' ? `header ${c.header ?? '?'}` : c.field;
      const v = c.value ? ` ${c.operator} “${c.value}”` : ` ${c.operator}`;
      return `${c.negate ? 'NOT ' : ''}${target}${v}`;
    }).join(' AND ');
    const acts = (r.actions ?? []).map((a) => {
      if (a.type === 'move') return `move to ${a.target}`;
      if (a.type === 'forward') return `forward to ${a.target}`;
      return a.type.replace(/_/g, ' ');
    }).join(', ');
    return `${conds} → ${acts}${r.stop ? ' (then stop)' : ''}`;
  }

  // --- list operations ----------------------------------------------------------

  protected async toggleEnabled(r: Rule, event: Event): Promise<void> {
    const enabled = (event.target as HTMLInputElement).checked;
    try {
      await this.api.setRuleEnabled(r.id, enabled);
      this.rules.update((cur) => cur.map((x) => (x.id === r.id ? { ...x, enabled } : x)));
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected async deleteRule(r: Rule): Promise<void> {
    if (!window.confirm(`Delete the rule “${r.name}”?`)) return;
    try {
      await this.api.deleteRule(r.id);
      this.rules.update((cur) => cur.filter((x) => x.id !== r.id));
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  // --- editor ---------------------------------------------------------------------

  protected newRule(): void {
    const priority = this.rules().reduce((m, r) => Math.max(m, r.priority), 0) + 1;
    this.editing.set({
      id: 0,
      name: '',
      enabled: true,
      priority,
      stop: false,
      conditions: [{ field: 'from', operator: 'contains', value: '' }],
      actions: [{ type: 'move', target: this.folderNames()[0] ?? '' }],
    });
    this.error.set('');
  }

  protected editRule(r: Rule): void {
    this.editing.set({
      id: r.id,
      name: r.name,
      enabled: r.enabled,
      priority: r.priority,
      stop: r.stop,
      conditions: (r.conditions ?? []).map((c) => ({ ...c })),
      actions: (r.actions ?? []).map((a) => ({ ...a })),
    });
    this.error.set('');
  }

  protected addCondition(d: Rule): void {
    d.conditions.push({ field: 'from', operator: 'contains', value: '' });
  }

  protected removeCondition(d: Rule, index: number): void {
    d.conditions.splice(index, 1);
  }

  protected addAction(d: Rule): void {
    d.actions.push({ type: 'mark_read' });
  }

  protected removeAction(d: Rule, index: number): void {
    d.actions.splice(index, 1);
  }

  // The same checks the server's ValidateRule makes, so bad rules fail in the
  // form instead of as a 400 after a save the user expected to work.
  private validate(d: Rule): string | null {
    if (!d.name.trim()) return 'A rule needs a name.';
    if (d.priority < 0) return 'Priority cannot be negative.';
    if (d.conditions.length === 0) return 'A rule needs at least one condition.';
    if (d.actions.length === 0) return 'A rule needs at least one action.';

    for (let i = 0; i < d.conditions.length; i++) {
      const c = d.conditions[i];
      if (c.field === 'header' && !(c.header ?? '').trim()) {
        return `Condition ${i + 1} tests a header without naming one.`;
      }
      if (this.needsValue(c.operator) && !(c.value ?? '').trim()) {
        return `Condition ${i + 1} needs a value to compare against.`;
      }
      if (c.operator === 'matches') {
        try {
          new RegExp(c.value ?? '');
        } catch {
          return `Condition ${i + 1} is not a valid regular expression.`;
        }
      }
    }

    for (let i = 0; i < d.actions.length; i++) {
      const a = d.actions[i];
      if (a.type === 'move' && !(a.target ?? '').trim()) {
        return `Action ${i + 1} moves the message nowhere.`;
      }
      if (a.type === 'forward' && !/^[^@\s]+@[^@\s]+$/.test(a.target ?? '')) {
        return `Action ${i + 1} forwards to “${a.target}”, which is not an address.`;
      }
    }
    return null;
  }

  protected async save(): Promise<void> {
    const d = this.editing();
    if (!d) return;
    const problem = this.validate(d);
    if (problem) {
      this.error.set(problem);
      return;
    }
    this.saving.set(true);
    this.error.set('');
    try {
      if (d.id) await this.api.updateRule(d);
      else await this.api.createRule(d);
      this.editing.set(null);
      await this.refresh();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.saving.set(false);
    }
  }
}

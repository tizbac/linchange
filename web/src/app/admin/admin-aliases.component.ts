import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { Alias, AliasTarget } from '../core/admin/admin.models';
import { errorMessage } from '../core/http-error';
import {
  AdminButton,
  Busy,
  Field,
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

interface TargetDraft {
  targetKind: 'user' | 'shared' | 'workgroup';
  targetAddress: string;
  workgroupId: number | null;
  position: number;
}

@Component({
  selector: 'app-admin-aliases',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    FormsModule,
    AdminButton,
    Busy,
    Field,
    Notice,
    PageEmpty,
    PageHeader,
    Panel,
    AdminNavComponent,
    AdminPageComponent,
  ],
  template: `
    <admin-page>
      <admin-nav />
      <div class="space-y-6">
        <admin-page-header
          title="Aliases"
          hint="An alias delivers to one or more mailboxes. Nothing is stored in the
            alias address, so removing one cannot lose mail. Multiple targets are
            tried in order until delivery succeeds."
        >
          <admin-button variant="primary" (pressed)="creating.set(!creating())">
            {{ creating() ? 'Cancel' : 'Add an alias' }}
          </admin-button>
          <admin-button (pressed)="reload()">Refresh</admin-button>
        </admin-page-header>

        <admin-notice [message]="error()" tone="error" />
        <admin-notice [message]="ok()" tone="ok" />

        @if (creating()) {
          <admin-panel title="New alias">
            <form class="grid gap-4 sm:grid-cols-2" (ngSubmit)="create()">
              <admin-field label="Alias address" hint="The address people write to.">
                <input
                  class="{{ inputClass }}"
                  name="address"
                  [(ngModel)]="draft.address"
                  required
                  placeholder="sales@example.com"
                />
              </admin-field>

              <div class="sm:col-span-2">
                <div class="flex items-center justify-between mb-2">
                  <h4 class="text-sm font-medium text-slate-700">Targets (delivered in order)</h4>
                  <admin-button variant="secondary" size="sm" (pressed)="addTarget()" [disabled]="targets().length >= 5">
                    Add target
                  </admin-button>
                </div>
                @for (t of targets(); track t.position; let i = $index) {
                  <div class="grid gap-3 sm:grid-cols-4 border rounded-lg p-3 bg-slate-50 mb-2" [attr.data-index]="i">
                    <admin-field label="Kind" class="sm:col-span-1">
                      <select class="{{ inputClass }}" [(ngModel)]="t.targetKind" (ngModelChange)="onKindChange(t)">
                        <option value="user">User mailbox</option>
                        <option value="shared">Shared mailbox</option>
                        <option value="workgroup">Workgroup</option>
                      </select>
                    </admin-field>
                    @if (t.targetKind !== 'workgroup') {
                      <admin-field label="Delivers to" class="sm:col-span-3" hint="An existing mailbox or shared mailbox.">
                        <input
                          class="{{ inputClass }}"
                          [(ngModel)]="t.targetAddress"
                          required
                          placeholder="alice@example.com"
                        />
                      </admin-field>
                    } @else {
                      <admin-field label="Workgroup" class="sm:col-span-3" hint="A workgroup to expand to its members.">
                        <input
                          class="{{ inputClass }}"
                          type="number"
                          min="1"
                          [(ngModel)]="t.workgroupId"
                          required
                          placeholder="1"
                        />
                      </admin-field>
                    }
                    <admin-field label="Position" class="sm:col-span-1">
                      <input
                        class="{{ inputClass }}"
                        type="number"
                        min="0"
                        [(ngModel)]="t.position"
                      />
                    </admin-field>
                    <div class="sm:col-span-1 flex items-end">
                      <admin-button variant="danger" size="sm" (pressed)="removeTarget(i)" [disabled]="targets().length <= 1">
                        Remove
                      </admin-button>
                    </div>
                  </div>
                }
              </div>

              <admin-field label="Display name">
                <input class="{{ inputClass }}" name="displayName" [(ngModel)]="draft.displayName" />
              </admin-field>
              <admin-field label="Description">
                <input class="{{ inputClass }}" name="description" [(ngModel)]="draft.description" />
              </admin-field>

              <div class="sm:col-span-2 flex items-center gap-3">
                <admin-button variant="primary" type="submit" [disabled]="busy()">
                  Create the alias
                </admin-button>
                <admin-busy [busy]="busy()" label="Creating…" />
              </div>
            </form>
          </admin-panel>
        }

        <admin-panel [bodyClass]="'p-0'">
          @if (loading()) {
            <div class="px-5 py-6"><admin-busy [busy]="true" label="Loading aliases…" /></div>
          } @else if (aliases().length === 0) {
            <admin-empty text="No aliases yet." />
          } @else {
            <table class="w-full">
              <thead class="border-b border-slate-200 bg-slate-50">
                <tr>
                  <th class="{{ head }}">Alias</th>
                  <th class="{{ head }}">Targets</th>
                  <th class="{{ head }}">State</th>
                  <th class="{{ head }} text-right">Actions</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-slate-100">
                @for (a of aliases(); track a.address) {
                  <tr>
                    <td class="{{ cell }}">
                      <span class="font-medium text-slate-900">{{ a.address }}</span>
                      @if (a.description) {
                        <span class="block text-xs text-slate-500">{{ a.description }}</span>
                      }
                    </td>
                    <td class="{{ cell }}">
                      @for (t of a.targets; track t.position) {
                        <div class="flex items-center gap-1.5 text-sm mb-1 last:mb-0">
                          <span
                            class="inline-flex rounded-full px-1.5 py-0.5 text-xs font-medium ring-1"
                            [class]="targetKindClass(t.target_kind)"
                          >
                            {{ t.target_kind }}
                          </span>
                          @if (t.target_address) {
                            <span class="text-slate-700">{{ t.target_address }}</span>
                          } @else {
                            <span class="text-slate-700">workgroup {{ t.workgroup_id }}</span>
                          }
                          <span class="text-xs text-slate-400">(pos {{ t.position }})</span>
                        </div>
                      }
                    </td>
                    <td class="{{ cell }}">
                      <span
                        class="inline-flex rounded-full px-2 py-0.5 text-xs font-medium ring-1"
                        [class]="
                          a.enabled
                            ? 'bg-emerald-50 text-emerald-700 ring-emerald-200'
                            : 'bg-slate-100 text-slate-600 ring-slate-200'
                        "
                      >
                        {{ a.enabled ? 'enabled' : 'disabled' }}
                      </span>
                    </td>
                    <td class="{{ cell }}">
                      <div class="flex items-center justify-end gap-2">
                        <admin-button [disabled]="busy()" (pressed)="toggleEnabled(a)">
                          {{ a.enabled ? 'Disable' : 'Enable' }}
                        </admin-button>
                        <admin-button variant="secondary" size="sm" (pressed)="editTargets(a)">
                          Edit targets
                        </admin-button>
                        <admin-button variant="danger" [disabled]="busy()" (pressed)="remove(a)">
                          Delete
                        </admin-button>
                      </div>
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          }
        </admin-panel>

        @if (editingTargets()) {
          <admin-panel title="Edit targets for {{ editingTargets()?.address }}">
            <form class="grid gap-4 sm:grid-cols-2" (ngSubmit)="saveTargets()">
              <div class="sm:col-span-2">
                <h4 class="text-sm font-medium text-slate-700 mb-2">Targets (delivered in order)</h4>
                @for (t of editTargetsList(); track t.position; let i = $index) {
                  <div class="grid gap-3 sm:grid-cols-4 border rounded-lg p-3 bg-slate-50 mb-2" [attr.data-index]="i">
                    <admin-field label="Kind" class="sm:col-span-1">
                      <select class="{{ inputClass }}" [(ngModel)]="t.targetKind" (ngModelChange)="onKindChange(t)">
                        <option value="user">User mailbox</option>
                        <option value="shared">Shared mailbox</option>
                        <option value="workgroup">Workgroup</option>
                      </select>
                    </admin-field>
                    @if (t.targetKind !== 'workgroup') {
                      <admin-field label="Delivers to" class="sm:col-span-3" hint="An existing mailbox or shared mailbox.">
                        <input
                          class="{{ inputClass }}"
                          [(ngModel)]="t.targetAddress"
                          required
                          placeholder="alice@example.com"
                        />
                      </admin-field>
                    } @else {
                      <admin-field label="Workgroup" class="sm:col-span-3" hint="A workgroup to expand to its members.">
                        <input
                          class="{{ inputClass }}"
                          type="number"
                          min="1"
                          [(ngModel)]="t.workgroupId"
                          required
                          placeholder="1"
                        />
                      </admin-field>
                    }
                    <admin-field label="Position" class="sm:col-span-1">
                      <input
                        class="{{ inputClass }}"
                        type="number"
                        min="0"
                        [(ngModel)]="t.position"
                      />
                    </admin-field>
                    <div class="sm:col-span-1 flex items-end">
                      <admin-button variant="danger" size="sm" (pressed)="removeEditTarget(i)" [disabled]="editTargetsList().length <= 1">
                        Remove
                      </admin-button>
                    </div>
                  </div>
                }
                <admin-button variant="secondary" size="sm" (pressed)="addEditTarget()" [disabled]="editTargetsList().length >= 5">
                  Add target
                </admin-button>
              </div>
              <div class="sm:col-span-2 flex items-center gap-3">
                <admin-button variant="primary" type="submit" [disabled]="busy()">
                  Save targets
                </admin-button>
                <admin-busy [busy]="busy()" label="Saving…" />
                <admin-button (pressed)="cancelEditTargets()">Cancel</admin-button>
              </div>
            </form>
          </admin-panel>
        }
      </div>
    </admin-page>
  `,
})
export class AdminAliasesComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;

  protected readonly aliases = signal<Alias[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');

  // For creating new alias
  protected draft = {
    address: '',
    displayName: '',
    description: '',
  };
  protected targets = signal<TargetDraft[]>([{ targetKind: 'user', targetAddress: '', workgroupId: null, position: 0 }]);

  // For editing targets
  protected editingTargets = signal<Alias | null>(null);
  protected editTargetsList = signal<TargetDraft[]>([]);

  ngOnInit(): void {
    this.reload();
  }

  protected targetKindClass(kind: string): string {
    switch (kind) {
      case 'user': return 'bg-blue-50 text-blue-700 ring-blue-200';
      case 'shared': return 'bg-violet-50 text-violet-700 ring-violet-200';
      case 'workgroup': return 'bg-amber-50 text-amber-700 ring-amber-200';
      default: return 'bg-slate-50 text-slate-700 ring-slate-200';
    }
  }

  protected onKindChange(t: TargetDraft): void {
    if (t.targetKind === 'workgroup') {
      t.targetAddress = '';
    } else {
      t.workgroupId = null;
    }
  }

  protected addTarget(): void {
    const nextPos = this.targets().length > 0
      ? Math.max(...this.targets().map(t => t.position)) + 1
      : 0;
    this.targets.update(arr => [...arr, { targetKind: 'user', targetAddress: '', workgroupId: null, position: nextPos }]);
  }

  protected removeTarget(i: number): void {
    this.targets.update(arr => arr.filter((_, idx) => idx !== i));
  }

  protected addEditTarget(): void {
    const nextPos = this.editTargetsList().length > 0
      ? Math.max(...this.editTargetsList().map(t => t.position)) + 1
      : 0;
    this.editTargetsList.update(arr => [...arr, { targetKind: 'user', targetAddress: '', workgroupId: null, position: nextPos }]);
  }

  protected removeEditTarget(i: number): void {
    this.editTargetsList.update(arr => arr.filter((_, idx) => idx !== i));
  }

  protected onEditKindChange(t: TargetDraft): void {
    if (t.targetKind === 'workgroup') {
      t.targetAddress = '';
    } else {
      t.workgroupId = null;
    }
  }

  protected editTargets(a: Alias): void {
    this.editingTargets.set(a);
    this.editTargetsList.set(a.targets.map(t => ({
      targetKind: t.target_kind as 'user' | 'shared' | 'workgroup',
      targetAddress: t.target_address ?? '',
      workgroupId: t.workgroup_id,
      position: t.position,
    })));
  }

  protected cancelEditTargets(): void {
    this.editingTargets.set(null);
    this.editTargetsList.set([]);
  }

  protected async saveTargets(): Promise<void> {
    const a = this.editingTargets();
    if (!a) return;

    this.busy.set(true);
    this.error.set('');
    try {
      const targets = this.editTargetsList().map(t => ({
        target_kind: t.targetKind,
        target_address: t.targetKind === 'workgroup' ? undefined : t.targetAddress,
        workgroup_id: t.targetKind === 'workgroup' ? (t.workgroupId ?? undefined) : undefined,
        position: t.position,
      }));
      await this.api.updateAliasTargets(a.address, targets);
      this.ok.set(`Targets for ${a.address} updated.`);
      this.cancelEditTargets();
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listAliases();
      this.aliases.set(res.aliases ?? []);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      const targets = this.targets().map(t => ({
        target_kind: t.targetKind,
        target_address: t.targetKind === 'workgroup' ? undefined : t.targetAddress,
        workgroup_id: t.targetKind === 'workgroup' ? (t.workgroupId ?? undefined) : undefined,
        position: t.position,
      }));
      await this.api.createAlias({
        address: this.draft.address.trim(),
        targets,
        display_name: this.draft.displayName.trim(),
        description: this.draft.description.trim(),
      });
      this.ok.set(`${this.draft.address.trim()} created with ${targets.length} target(s).`);
      this.draft = { address: '', displayName: '', description: '' };
      this.targets.set([{ targetKind: 'user', targetAddress: '', workgroupId: null, position: 0 }]);
      this.creating.set(false);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async toggleEnabled(a: Alias): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.setAliasEnabled(a.address, !a.enabled);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(a: Alias): Promise<void> {
    if (!confirm(`Delete the alias ${a.address}? Mail sent to it will bounce.`)) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.deleteAlias(a.address);
      this.ok.set(`${a.address} was deleted.`);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}
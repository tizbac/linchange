import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { Alias } from '../core/admin/admin.models';
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
        hint="An alias delivers to somebody else's mailbox. Nothing is stored in the
          alias address, so removing one cannot lose mail."
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
            <admin-field label="Delivers to" hint="An existing mailbox or shared mailbox.">
              <input
                class="{{ inputClass }}"
                name="target"
                [(ngModel)]="draft.targetAddress"
                required
                placeholder="alice@example.com"
              />
            </admin-field>
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
                <th class="{{ head }}">Delivers to</th>
                <th class="{{ head }}">Kind</th>
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
                  <td class="{{ cell }}">{{ a.target_address ?? 'workgroup ' + a.workgroup_id }}</td>
                  <td class="{{ cell }}">{{ a.target_kind }}</td>
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

  protected draft = { address: '', targetAddress: '', displayName: '', description: '' };

  ngOnInit(): void {
    this.reload();
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
      await this.api.createAlias({
        address: this.draft.address.trim(),
        target_address: this.draft.targetAddress.trim(),
        display_name: this.draft.displayName.trim(),
        description: this.draft.description.trim(),
      });
      this.ok.set(`${this.draft.address.trim()} now delivers to ${this.draft.targetAddress.trim()}.`);
      this.draft = { address: '', targetAddress: '', displayName: '', description: '' };
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
    // The confirmation names the alias, because "delete" next to a list of
    // addresses is a button that gets pressed by muscle memory.
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

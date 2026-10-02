import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { Domain, formatBytes } from '../core/admin/admin.models';
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

@Component({
  selector: 'app-admin-domains',
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
  ],
  template: `
    <div class="space-y-6">
      <admin-page-header
        title="Domains"
        hint="A domain is the namespace users are created in. Suspending one keeps its
          users and mail intact but stops them signing in and receiving new mail."
      >
        <admin-button variant="primary" (pressed)="toggleCreate()">
          {{ creating() ? 'Cancel' : 'Add a domain' }}
        </admin-button>
        <admin-button (pressed)="reload()">Refresh</admin-button>
      </admin-page-header>

      <admin-notice [message]="error()" tone="error" />
      <admin-notice [message]="ok()" tone="ok" />

      @if (creating()) {
        <admin-panel title="New domain">
          <form class="grid gap-4 sm:grid-cols-2" (ngSubmit)="create()">
            <admin-field label="Domain name" hint="For example example.com. It cannot be changed later.">
              <input
                class="{{ inputClass }}"
                name="name"
                [(ngModel)]="draft.name"
                required
                placeholder="example.com"
              />
            </admin-field>
            <admin-field label="Description">
              <input class="{{ inputClass }}" name="description" [(ngModel)]="draft.description" />
            </admin-field>
            <admin-field
              label="Default quota (MiB)"
              hint="Applied to new mailboxes in this domain. Leave empty for the schema default."
            >
              <input
                class="{{ inputClass }}"
                name="quota"
                type="number"
                min="0"
                [(ngModel)]="draft.quotaMiB"
                placeholder="unlimited"
              />
            </admin-field>
            <label class="flex items-center gap-2 self-end pb-1.5 text-sm text-slate-700">
              <input type="checkbox" name="local" [(ngModel)]="draft.isLocal" />
              Delivered locally rather than by a relay
            </label>
            <div class="sm:col-span-2 flex items-center gap-3">
              <admin-button variant="primary" type="submit" [disabled]="busy()">
                Create the domain
              </admin-button>
              <admin-busy [busy]="busy()" label="Creating…" />
            </div>
          </form>
        </admin-panel>
      }

      <admin-panel [bodyClass]="'p-0'">
        @if (loading()) {
          <div class="px-5 py-6"><admin-busy [busy]="true" label="Loading domains…" /></div>
        } @else if (domains().length === 0) {
          <admin-empty text="No domains yet." />
        } @else {
          <table class="w-full">
            <thead class="border-b border-slate-200 bg-slate-50">
              <tr>
                <th class="{{ head }}">Domain</th>
                <th class="{{ head }}">Status</th>
                <th class="{{ head }}">Delivery</th>
                <th class="{{ head }}">Default quota</th>
                <th class="{{ head }}">Send limit</th>
                <th class="{{ head }} text-right">Actions</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-100">
              @for (d of domains(); track d.name) {
                <tr>
                  <td class="{{ cell }}">
                    <span class="font-medium text-slate-900">{{ d.name }}</span>
                    @if (d.description) {
                      <span class="block text-xs text-slate-500">{{ d.description }}</span>
                    }
                  </td>
                  <td class="{{ cell }}">
                    <span
                      class="inline-flex rounded-full px-2 py-0.5 text-xs font-medium ring-1"
                      [class]="statusClass(d.status)"
                    >
                      {{ d.status }}
                    </span>
                  </td>
                  <td class="{{ cell }}">{{ d.is_local ? 'local' : 'relay' }}</td>
                  <td class="{{ cell }}">{{ bytes(d.default_quota_bytes) }}</td>
                  <td class="{{ cell }}">
                    {{ d.daily_send_limit === null || d.daily_send_limit === undefined
                      ? 'unlimited'
                      : d.daily_send_limit + ' / day' }}
                  </td>
                  <td class="{{ cell }} text-right">
                    <admin-button (pressed)="toggleStatus(d)">
                      {{ nextStatusLabel(d) }}
                    </admin-button>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        }
      </admin-panel>
    </div>
  `,
})
export class AdminDomainsComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;
  protected readonly bytes = formatBytes;

  protected readonly domains = signal<Domain[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');

  protected draft = { name: '', description: '', quotaMiB: null, isLocal: true };

  ngOnInit(): void {
    this.reload();
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listDomains();
      this.domains.set(res.domains ?? []);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected toggleCreate(): void {
    this.creating.update((v) => !v);
    this.ok.set('');
  }

  protected async create(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      // The quota is entered in MiB because that is what an operator reasons
      // about, and the server stores bytes. Sending a raw byte count from a
      // number input is a reliable way to set a mailbox to 16 EiB.
      const quota =
        this.draft.quotaMiB === null || this.draft.quotaMiB === undefined
          ? undefined
          : Math.round(this.draft.quotaMiB * 1024 * 1024);
      await this.api.createDomain({
        name: this.draft.name.trim(),
        is_local: this.draft.isLocal,
        description: this.draft.description.trim(),
        ...(quota === undefined ? {} : { default_quota_bytes: quota }),
      });
      this.ok.set(`${this.draft.name.trim()} was created.`);
      this.draft = { name: '', description: '', quotaMiB: null, isLocal: true };
      this.creating.set(false);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // toggleStatus moves a domain one step rather than offering a free-text
  // status, because the only three values are the three legal ones and a
  // dropdown that can hold an invalid value is a way to get a 400.
  protected async toggleStatus(d: Domain): Promise<void> {
    const next: Domain['status'] = d.status === 'active' ? 'suspended' : 'active';
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.setDomainStatus(d.name, next);
      this.ok.set(`${d.name} is now ${next}.`);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected nextStatusLabel(d: Domain): string {
    return d.status === 'active' ? 'Suspend' : 'Activate';
  }

  protected statusClass(status: Domain['status']): string {
    switch (status) {
      case 'active':
        return 'bg-emerald-50 text-emerald-700 ring-emerald-200';
      case 'suspended':
        return 'bg-amber-50 text-amber-800 ring-amber-200';
      default:
        return 'bg-slate-100 text-slate-600 ring-slate-200';
    }
  }
}

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
import { AdminNavComponent } from '../core/admin/admin-nav.component';
import { AdminPageComponent } from '../core/admin/admin-page.component';

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
    AdminNavComponent,
    AdminPageComponent,
  ],
  templateUrl: './admin-domains.component.html',
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

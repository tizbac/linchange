import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { AuditEntry } from '../core/admin/admin.models';
import {
  AdminButton,
  Busy,
  Notice,
  PageEmpty,
  PageHeader,
  Panel,
  tableCellClass,
  tableHeadClass,
} from '../core/admin/ui';
import { AdminNavComponent } from '../core/admin/admin-nav.component';
import { AdminPageComponent } from '../core/admin/admin-page.component';

@Component({
  selector: 'app-admin-audit',
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
  ],
  templateUrl: './admin-audit.component.html',
})
export class AdminAuditComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;

  protected readonly entries = signal<AuditEntry[]>([]);
  protected readonly loading = signal(true);
  protected readonly error = signal('');

  ngOnInit(): void {
    this.reload();
  }

  protected reason(entry: AuditEntry): string {
    try {
      const detail = entry.detail as { reason?: string } | null;
      return detail?.reason ?? '';
    } catch {
      return '';
    }
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listAudit({});
      this.entries.set(res.entries ?? []);
    } catch (err) {
      this.error.set(String(err));
    } finally {
      this.loading.set(false);
    }
  }
}

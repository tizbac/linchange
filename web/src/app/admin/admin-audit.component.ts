import { ChangeDetectionStrategy, Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { AuditEntry } from '../core/admin/admin.models';
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

// The audit log is the record of who did what to whose data. It is read two
// ways: one page at a time with a search across every column, and as a CSV of
// the whole filtered set when an auditor needs the records outside this page.
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
  protected readonly input = inputClass;

  protected readonly entries = signal<AuditEntry[]>([]);
  protected readonly loading = signal(true);
  protected readonly exporting = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');
  protected readonly total = signal(0);
  protected readonly page = signal(0);
  protected readonly pageSize = 50;
  protected readonly pageCount = computed(() =>
    Math.max(1, Math.ceil(this.total() / this.pageSize)),
  );

  // search is one field on purpose: an administrator reading the log does not
  // know whether the value is an actor, a target, an action, an IP or inside
  // the detail JSON. The server matches all of them.
  protected search = '';

  ngOnInit(): void {
    void this.reload();
  }

  protected async reload(resetPage = false): Promise<void> {
    if (resetPage) this.page.set(0);
    this.loading.set(true);
    this.error.set('');
    try {
      const offset = this.page() * this.pageSize;
      const res = await this.api.listAudit({
        search: this.search.trim() || undefined,
        limit: this.pageSize,
        offset,
      });
      this.entries.set(res.entries ?? []);
      this.total.set(res.total);
      // A row deleted by retention can leave the last page empty. Step back
      // to the last real page rather than showing "nothing" beside a total.
      if ((res.entries ?? []).length === 0 && res.total > 0 && offset > 0) {
        this.page.set(Math.max(0, Math.ceil(res.total / this.pageSize) - 1));
        await this.reload();
      }
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected clearSearch(): void {
    this.search = '';
    void this.reload(true);
  }

  protected previousPage(): void {
    if (this.page() === 0) return;
    this.page.update((p) => p - 1);
    void this.reload();
  }

  protected nextPage(): void {
    if (this.page() + 1 >= this.pageCount()) return;
    this.page.update((p) => p + 1);
    void this.reload();
  }

  // exportCsv downloads the whole filtered set, not the current page. The
  // total is the same filter the table is showing, so the confirmation can
  // say how many records left.
  protected async exportCsv(): Promise<void> {
    this.exporting.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const blob = await this.api.exportAudit({
        search: this.search.trim() || undefined,
      });
      const url = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = url;
      link.download = `audit-log-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.csv`;
      document.body.appendChild(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(url);
      const n = this.total();
      this.ok.set(`Exported ${n} ${n === 1 ? 'entry' : 'entries'} to CSV.`);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.exporting.set(false);
    }
  }

  protected when(entry: AuditEntry): string {
    const date = new Date(entry.created_at);
    return Number.isNaN(date.getTime()) ? entry.created_at : date.toLocaleString();
  }

  protected detailText(entry: AuditEntry): string {
    if (entry.detail == null) return '';
    if (typeof entry.detail === 'string') return entry.detail;
    try {
      return JSON.stringify(entry.detail);
    } catch {
      return String(entry.detail);
    }
  }
}

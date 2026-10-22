import { ChangeDetectionStrategy, Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { DatePipe } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { User } from '../core/admin/admin.models';
import { ImportJob } from '../core/mail/mail.models';
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
import { AdminListFooter, AdminSearch } from '../core/admin/admin-list';
import { errorMessage } from '../core/http-error';

// The admin import page. An operator launches an import into any mailbox —
// including shared mailboxes, which a user cannot do for themselves — and can
// watch or cancel any job.
@Component({
  selector: 'app-admin-imports',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    DatePipe,
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
    AdminSearch,
    AdminListFooter,
  ],
  templateUrl: './admin-imports.component.html',
})
export class AdminImportsComponent implements OnInit, OnDestroy {
  private readonly api = inject(AdminApi);

  protected readonly inputClass = inputClass;
  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;

  protected readonly jobs = signal<ImportJob[]>([]);
  protected readonly total = signal(0);
  protected query = '';
  protected page = 1;
  protected readonly pageSize = 20;
  protected readonly users = signal<User[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');
  protected readonly remoteFolders = signal<string[]>([]);
  protected readonly creating = signal(false);

  protected readonly draft = {
    owner: '',
    host: '',
    port: 0,
    tls: 'tls',
    username: '',
    password: '',
    acceptSelfSigned: false,
    sourceRoot: '',
    targetRoot: '',
  };

  private pollTimer: ReturnType<typeof setTimeout> | null = null;

  ngOnInit(): void {
    void this.reload();
  }

  ngOnDestroy(): void {
    this.stopPolling();
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

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const [jobsRes, usersRes] = await Promise.all([
        this.api.listImports({
          search: this.query || undefined,
          limit: this.pageSize,
          offset: (this.page - 1) * this.pageSize,
        }),
        this.api.listUsers(),
      ]);
      this.jobs.set(jobsRes.imports ?? []);
      this.total.set(jobsRes.total);
      this.users.set(usersRes.users ?? []);
      this.schedulePoll();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  private schedulePoll(): void {
    this.stopPolling();
    const active = this.jobs().some((j) => j.status === 'queued' || j.status === 'running');
    if (active) {
      this.pollTimer = setTimeout(() => void this.reload(), 3000);
    }
  }

  private stopPolling(): void {
    if (this.pollTimer) {
      clearTimeout(this.pollTimer);
      this.pollTimer = null;
    }
  }

  protected async startImport(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const job = await this.api.createImport({
        owner: this.draft.owner,
        remote_host: this.draft.host,
        remote_port: this.draft.port || undefined,
        remote_tls: this.draft.tls,
        remote_username: this.draft.username,
        remote_password: this.draft.password,
        accept_self_signed: this.draft.acceptSelfSigned,
        source_root: this.draft.sourceRoot,
        target_root: this.draft.targetRoot,
      });
      this.ok.set(`Import queued for ${job.owner} (job #${job.id}).`);
      this.draft.password = '';
      this.creating.set(false);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async listRemoteFolders(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      const res = await this.api.remoteFolders({
        remote_host: this.draft.host,
        remote_port: this.draft.port || undefined,
        remote_tls: this.draft.tls,
        remote_username: this.draft.username,
        remote_password: this.draft.password,
        accept_self_signed: this.draft.acceptSelfSigned,
      });
      this.remoteFolders.set(res.folders ?? []);
    } catch (err) {
      this.error.set(errorMessage(err));
      this.remoteFolders.set([]);
    } finally {
      this.busy.set(false);
    }
  }

  protected async cancel(job: ImportJob): Promise<void> {
    if (!window.confirm(`Cancel import #${job.id} for ${job.owner}?`)) return;
    try {
      await this.api.cancelImport(job.id);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected progressPercent(job: ImportJob): number {
    if (!job.folders_total) return 0;
    return Math.min(100, Math.round((job.folders_done / job.folders_total) * 100));
  }

  protected statusClass(status: string): string {
    switch (status) {
      case 'completed':
        return 'bg-emerald-100 text-emerald-800';
      case 'running':
        return 'bg-blue-100 text-blue-800';
      case 'queued':
        return 'bg-slate-100 text-slate-700';
      case 'cancelled':
        return 'bg-amber-100 text-amber-800';
      case 'failed':
        return 'bg-rose-100 text-rose-800';
      default:
        return 'bg-slate-100 text-slate-700';
    }
  }
}

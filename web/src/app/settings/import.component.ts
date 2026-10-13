import { ChangeDetectionStrategy, Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { DatePipe } from '@angular/common';
import { Router } from '@angular/router';
import { FormsModule } from '@angular/forms';
import { MailApi } from '../core/mail/mail-api';
import { errorMessage } from '../core/http-error';
import { MailIcon } from '../core/mail/ui';
import { ImportJob } from '../core/mail/mail.models';

// The remote-IMAP import page.
//
// A user pastes the connection details of their old server, optionally picks a
// source folder and a destination folder, and the server pulls the mail in the
// background. The page polls while a job is queued or running so progress is
// visible without a websocket; nothing else in the app has one.
@Component({
  selector: 'app-import',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [DatePipe, FormsModule, MailIcon],
  template: `
    <main class="mx-auto max-w-3xl px-6 py-8">
      <div class="flex items-center justify-between gap-3">
        <h1 class="text-lg font-semibold text-slate-900">Import mail</h1>
        <button
          class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 transition hover:bg-slate-100"
          (click)="back()"
        >
          Back to mail
        </button>
      </div>

      <p class="mt-2 text-sm text-slate-600">
        Pull mail from another IMAP server into this mailbox. Messages already here (matched by
        content) are skipped, so you can run the import again safely.
      </p>

      @if (error()) {
        <p class="mt-4 rounded-lg border border-rose-200 bg-rose-50 px-3 py-2 text-sm text-rose-800">
          {{ error() }}
        </p>
      }
      @if (ok()) {
        <p class="mt-4 rounded-lg border border-emerald-200 bg-emerald-50 px-3 py-2 text-sm text-emerald-800">
          {{ ok() }}
        </p>
      }

      <!-- Create form -->
      <div class="mt-5 rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
        <h2 class="text-sm font-semibold text-slate-900">New import</h2>
        <div class="mt-4 grid grid-cols-2 gap-4">
          <div>
            <label class="block text-xs font-medium text-slate-600">Server host</label>
            <input
              [(ngModel)]="form.host"
              class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm"
              placeholder="imap.example.com"
            />
          </div>
          <div>
            <label class="block text-xs font-medium text-slate-600">Port</label>
            <input
              type="number"
              [(ngModel)]="form.port"
              class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm"
              placeholder="993"
            />
          </div>
          <div>
            <label class="block text-xs font-medium text-slate-600">Encryption</label>
            <select [(ngModel)]="form.tls" class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm">
              <option value="tls">TLS (993)</option>
              <option value="starttls">STARTTLS (143)</option>
              <option value="plain">Plain (143)</option>
            </select>
          </div>
          <div>
            <label class="block text-xs font-medium text-slate-600">Username</label>
            <input
              [(ngModel)]="form.username"
              class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm"
              placeholder="user&#64;old.example.com"
            />
          </div>
          <div>
            <label class="block text-xs font-medium text-slate-600">Password</label>
            <input
              type="password"
              [(ngModel)]="form.password"
              class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm"
            />
          </div>
          <div class="flex items-end pb-1">
            <label class="flex items-center gap-2 text-sm text-slate-700">
              <input type="checkbox" [(ngModel)]="form.acceptSelfSigned" class="rounded" />
              Accept self-signed certificate
            </label>
          </div>
          <div>
            <label class="block text-xs font-medium text-slate-600">
              Source folder (blank = all)
            </label>
            <input
              [(ngModel)]="form.sourceRoot"
              class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm"
              placeholder="INBOX"
            />
          </div>
          <div>
            <label class="block text-xs font-medium text-slate-600">
              Target folder prefix (blank = root)
            </label>
            <input
              [(ngModel)]="form.targetRoot"
              class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm"
              placeholder="Imported"
            />
          </div>
        </div>

        <div class="mt-4 flex flex-wrap items-center gap-2">
          <button
            type="button"
            [disabled]="busy()"
            class="rounded-lg bg-brand-600 px-4 py-2 text-sm font-medium text-white transition hover:bg-brand-700 disabled:opacity-50"
            (click)="startImport()"
          >
            {{ busy() ? 'Starting…' : 'Start import' }}
          </button>
          <button
            type="button"
            [disabled]="busy()"
            class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 transition hover:bg-slate-100 disabled:opacity-50"
            (click)="listRemoteFolders()"
          >
            List folders
          </button>
        </div>

        @if (remoteFolders().length) {
          <div class="mt-3 rounded-lg border border-slate-200 bg-slate-50 px-3 py-2">
            <p class="mb-1 text-xs font-medium text-slate-500">Remote folders</p>
            <ul class="max-h-40 overflow-y-auto text-xs text-slate-700">
              @for (f of remoteFolders(); track f) {
                <li>
                  <button class="text-left hover:text-brand-700" (click)="form.sourceRoot = f">{{ f }}</button>
                </li>
              }
            </ul>
          </div>
        }
      </div>

      <!-- Job list -->
      <div class="mt-6">
        <h2 class="text-sm font-semibold text-slate-900">Import history</h2>
        @if (loading()) {
          <p class="mt-2 text-sm text-slate-500">Loading…</p>
        } @else if (!jobs().length) {
          <p class="mt-2 text-sm text-slate-500">No imports yet.</p>
        } @else {
          <div class="mt-2 overflow-x-auto rounded-xl border border-slate-200 bg-white shadow-sm">
            <table class="w-full text-sm">
              <thead class="border-b border-slate-200 bg-slate-50 text-left text-xs font-medium text-slate-500">
                <tr>
                  <th class="px-4 py-2">Server</th>
                  <th class="px-4 py-2">Status</th>
                  <th class="px-4 py-2">Progress</th>
                  <th class="px-4 py-2">Imported</th>
                  <th class="px-4 py-2">Skipped</th>
                  <th class="px-4 py-2">Created</th>
                  <th class="px-4 py-2"></th>
                </tr>
              </thead>
              <tbody class="divide-y divide-slate-100">
                @for (job of jobs(); track job.id) {
                  <tr>
                    <td class="px-4 py-2 text-slate-900">{{ job.remote_host }}</td>
                    <td class="px-4 py-2">
                      <span
                        class="inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium"
                        [class]="statusClass(job.status)"
                      >
                        {{ job.status }}
                      </span>
                    </td>
                    <td class="px-4 py-2 text-slate-600">
                      @if (job.status === 'running' || job.status === 'queued') {
                        {{ job.folders_done }} / {{ job.folders_total }} folders, {{ job.messages_seen }} seen
                        <div class="mt-1 h-1.5 w-24 overflow-hidden rounded-full bg-slate-200">
                          <div
                            class="h-full rounded-full bg-brand-600 transition-all"
                            [style.width.%]="progressPercent(job)"
                          ></div>
                        </div>
                      } @else {
                        {{ job.folders_done }} / {{ job.folders_total }} folders
                      }
                    </td>
                    <td class="px-4 py-2 text-slate-600">{{ job.messages_imported }}</td>
                    <td class="px-4 py-2 text-slate-600">{{ job.messages_skipped }}</td>
                    <td class="px-4 py-2 text-slate-500">{{ job.created_at | date: 'short' }}</td>
                    <td class="px-4 py-2">
                      @if (job.status === 'queued' || job.status === 'running') {
                        <button
                          class="text-xs font-medium text-rose-600 hover:text-rose-800"
                          (click)="cancel(job)"
                        >
                          Cancel
                        </button>
                      }
                      @if (job.last_error) {
                        <p class="mt-1 max-w-48 truncate text-xs text-rose-600" [title]="job.last_error">
                          {{ job.last_error }}
                        </p>
                      }
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          </div>
        }
      </div>
    </main>
  `,
})
export class ImportComponent implements OnInit, OnDestroy {
  private readonly api = inject(MailApi);
  private readonly router = inject(Router);

  protected readonly jobs = signal<ImportJob[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');
  protected readonly remoteFolders = signal<string[]>([]);

  protected readonly form = {
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

  private async reload(): Promise<void> {
    try {
      const res = await this.api.listImports();
      this.jobs.set(res.imports ?? []);
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
        remote_host: this.form.host,
        remote_port: this.form.port || undefined,
        remote_tls: this.form.tls,
        remote_username: this.form.username,
        remote_password: this.form.password,
        accept_self_signed: this.form.acceptSelfSigned,
        source_root: this.form.sourceRoot,
        target_root: this.form.targetRoot,
      });
      this.ok.set(`Import queued (job #${job.id}).`);
      this.form.password = '';
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
        remote_host: this.form.host,
        remote_port: this.form.port || undefined,
        remote_tls: this.form.tls,
        remote_username: this.form.username,
        remote_password: this.form.password,
        accept_self_signed: this.form.acceptSelfSigned,
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
    if (!window.confirm(`Cancel import #${job.id}?`)) return;
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

  protected back(): void {
    void this.router.navigate(['/mail']);
  }
}

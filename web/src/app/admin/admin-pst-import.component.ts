import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';

import { AdminApi } from '../core/admin/admin-api';
import { AdminNavComponent } from '../core/admin/admin-nav.component';
import { AdminPageComponent } from '../core/admin/admin-page.component';
import { PageHeader } from '../core/admin/ui';
import { errorMessage } from '../core/http-error';
import { PstImportPanelComponent } from '../settings/pst-import-panel.component';

// The admin PST import page. It reuses the user panel so the batching, the
// close guard and the server-side dedupe are identical; the only difference is
// that the operator names the mailbox and the server imports appointments and
// contacts into that mailbox's own defaults.
@Component({
  selector: 'app-admin-pst-import',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink, AdminNavComponent, AdminPageComponent, PageHeader, PstImportPanelComponent],
  template: `
    <admin-page>
      <admin-nav />
      <div class="space-y-6">
        <admin-page-header
          title="PST/OST import"
          hint="Import an Outlook archive into any mailbox. The archive is read in this browser
            and uploaded in 64 MiB batches; items already imported are skipped."
        >
          <a
            routerLink="/admin/imports"
            class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 transition hover:bg-slate-100"
          >
            IMAP imports
          </a>
        </admin-page-header>

        @if (error()) {
          <p class="rounded-lg border border-rose-200 bg-rose-50 px-3 py-2 text-sm text-rose-800">
            {{ error() }}
          </p>
        }

        <app-pst-import-panel base="admin" [users]="addresses()" />
      </div>
    </admin-page>
  `,
})
export class AdminPstImportComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly addresses = signal<string[]>([]);
  protected readonly error = signal('');

  async ngOnInit(): Promise<void> {
    try {
      const response = await this.api.listUsers({ limit: 200 });
      this.addresses.set((response.users ?? []).map((user) => user.address));
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }
}

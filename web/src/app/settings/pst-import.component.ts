import { ChangeDetectionStrategy, Component } from '@angular/core';
import { RouterLink } from '@angular/router';

import { PstImportPanelComponent } from './pst-import-panel.component';

// The PST/OST import page. The heavy lifting happens in the browser, so this
// page only frames the shared panel and offers a way back to the IMAP import.
@Component({
  selector: 'app-pst-import',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [PstImportPanelComponent, RouterLink],
  template: `
    <main class="mx-auto max-w-4xl px-6 py-8">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <h1 class="text-lg font-semibold text-slate-900">Import from a PST/OST file</h1>
        <div class="flex gap-2">
          <a
            routerLink="/mail/import/imap"
            class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 transition hover:bg-slate-100"
          >
            Remote IMAP
          </a>
          <a
            routerLink="/mail"
            class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 transition hover:bg-slate-100"
          >
            Back to mail
          </a>
        </div>
      </div>

      <p class="mt-2 text-sm text-slate-600">
        Import the mail, appointments and contacts of an Outlook archive. The file is read in this
        browser; only the converted items are uploaded in 64 MiB batches. Re-running the same file
        is safe — items already imported are skipped.
      </p>

      <app-pst-import-panel base="mail" />
    </main>
  `,
})
export class PstImportComponent {}

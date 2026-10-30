import { ChangeDetectionStrategy, Component } from '@angular/core';
import { RouterLink } from '@angular/router';

// The import entry point: mail arrives from one of two places, and the next
// screen asks which kind of import the user wants. Keeping the choice here
// means the old IMAP page and the new PST page do not have to know about each
// other.
@Component({
  selector: 'app-import-chooser',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink],
  template: `
    <main class="mx-auto max-w-3xl px-6 py-8">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <h1 class="text-lg font-semibold text-slate-900">Import mail</h1>
        <a
          routerLink="/mail"
          class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 transition hover:bg-slate-100"
        >
          Back to mail
        </a>
      </div>

      <p class="mt-2 text-sm text-slate-600">
        Bring mail into this mailbox from another server, or from an Outlook data file.
      </p>

      <div class="mt-6 grid gap-4 sm:grid-cols-2">
        <a
          routerLink="/mail/import/imap"
          class="group rounded-xl border border-slate-200 bg-white p-5 shadow-sm transition hover:border-brand-300 hover:shadow"
        >
          <h2 class="text-sm font-semibold text-slate-900 group-hover:text-brand-700">
            Remote IMAP
          </h2>
          <p class="mt-2 text-sm text-slate-600">
            Pull mail from another IMAP server. The server connects in the background and copies
            folders, so you can close this page and come back later.
          </p>
        </a>

        <a
          routerLink="/mail/import/pst"
          class="group rounded-xl border border-slate-200 bg-white p-5 shadow-sm transition hover:border-brand-300 hover:shadow"
        >
          <h2 class="text-sm font-semibold text-slate-900 group-hover:text-brand-700">
            PST/OST file
          </h2>
          <p class="mt-2 text-sm text-slate-600">
            Read an Outlook archive in this browser and import its mail, appointments and contacts.
            Choose the folders and their destinations before anything is written.
          </p>
        </a>
      </div>
    </main>
  `,
})
export class ImportChooserComponent {}

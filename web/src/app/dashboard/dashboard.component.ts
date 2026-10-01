import { Component } from '@angular/core';

@Component({
  selector: 'app-dashboard',
  template: `
    <main class="mx-auto max-w-4xl px-6 py-10">
      <header>
        <h1 class="text-3xl font-semibold tracking-tight text-slate-900">Mail</h1>
      </header>

      <section class="mt-8 rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
        <h2 class="text-lg font-semibold text-slate-900">Inbox</h2>
        <p class="mt-2 text-sm text-slate-500">
          You are signed in. The mailbox, calendar and admin screens build on this
          shell in the following phases.
        </p>
      </section>
    </main>
  `,
})
export class DashboardComponent {}

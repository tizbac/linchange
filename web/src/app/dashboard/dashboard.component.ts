import { Component } from '@angular/core';
import { RouterLink } from '@angular/router';

@Component({
  selector: 'app-dashboard',
  standalone: true,
  imports: [RouterLink],
  template: `
    <main class="mx-auto max-w-4xl px-6 py-10">
      <header>
        <h1 class="text-3xl font-semibold tracking-tight text-slate-900">Linchange</h1>
      </header>

      <div class="mt-8 grid gap-4 sm:grid-cols-2">
        <a routerLink="/mail" class="rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200 hover:ring-blue-400 transition-colors">
          <h2 class="text-lg font-semibold text-slate-900" i18n="@@dashboard.mail">Mail</h2>
          <p class="mt-2 text-sm text-slate-500" i18n="@@dashboard.mailHint">Read, compose, and organize your email.</p>
        </a>
        <a routerLink="/calendar" class="rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200 hover:ring-blue-400 transition-colors">
          <h2 class="text-lg font-semibold text-slate-900" i18n="@@dashboard.calendar">Calendar</h2>
          <p class="mt-2 text-sm text-slate-500" i18n="@@dashboard.calendarHint">Manage events, free/busy, and scheduling.</p>
        </a>
        <a routerLink="/meetings" class="rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200 hover:ring-blue-400 transition-colors">
          <h2 class="text-lg font-semibold text-slate-900" i18n="@@dashboard.meetings">Meetings</h2>
          <p class="mt-2 text-sm text-slate-500" i18n="@@dashboard.meetingsHint">Invite people by email and chat in the meeting room.</p>
        </a>
      </div>
    </main>
  `,
})
export class DashboardComponent {}

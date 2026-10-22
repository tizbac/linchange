import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { RouterLink, RouterLinkActive } from '@angular/router';

interface Tab {
  label: string;
  path: string;
}

const TABS: Tab[] = [
  { label: $localize`:@@adminNav.domains:Domains`, path: '/admin/domains' },
  { label: $localize`:@@adminNav.organizations:Organizations`, path: '/admin/workgroups' },
  { label: $localize`:@@adminNav.users:Users`, path: '/admin/users' },
  { label: $localize`:@@adminNav.aliases:Aliases`, path: '/admin/aliases' },
  { label: $localize`:@@adminNav.sharedMailboxes:Shared mailboxes`, path: '/admin/shared-mailboxes' },
  { label: $localize`:@@adminNav.imports:Imports`, path: '/admin/imports' },
  { label: $localize`:@@adminNav.impersonation:Impersonation`, path: '/admin/impersonation' },
  { label: $localize`:@@adminNav.queue:Outbound queue`, path: '/admin/queue' },
  { label: $localize`:@@adminNav.audit:Audit log`, path: '/admin/audit' },
];

@Component({
  selector: 'admin-nav',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink, RouterLinkActive],
  template: `
    <nav class="flex flex-wrap gap-1 border-b border-slate-200">
      @for (tab of tabs(); track tab.path) {
        <a
          [routerLink]="tab.path"
          routerLinkActive="border-brand-600 text-brand-700"
          [routerLinkActiveOptions]="{ exact: true }"
          class="shrink-0 border-b-2 border-transparent px-4 py-2 text-sm font-medium text-slate-600 hover:text-brand-700 hover:border-brand-300 transition"
        >
          {{ tab.label }}
        </a>
      }
    </nav>
  `,
})
export class AdminNavComponent {
  readonly tabs = input<Tab[]>(TABS);
}
import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { RouterLink, RouterLinkActive } from '@angular/router';

interface Tab {
  label: string;
  path: string;
}

const TABS: Tab[] = [
  { label: 'Domains', path: '/admin/domains' },
  { label: 'Users', path: '/admin/users' },
  { label: 'Aliases', path: '/admin/aliases' },
  { label: 'Shared mailboxes', path: '/admin/shared-mailboxes' },
  { label: 'Impersonation', path: '/admin/impersonation' },
  { label: 'Audit log', path: '/admin/audit' },
];

@Component({
  selector: 'admin-nav',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink, RouterLinkActive],
  template: `
    <nav class="flex gap-1 border-b border-slate-200 overflow-x-auto">
      @for (tab of tabs(); track tab.path) {
        <a
          [routerLink]="tab.path"
          routerLinkActive="border-brand-600 text-brand-700"
          [routerLinkActiveOptions]="{ exact: true }"
          class="flex-shrink-0 border-b-2 border-transparent px-4 py-2 text-sm font-medium text-slate-600 hover:text-brand-700 hover:border-brand-300 transition"
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
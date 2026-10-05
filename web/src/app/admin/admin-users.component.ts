import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { User, formatBytes } from '../core/admin/admin.models';
import { errorMessage } from '../core/http-error';
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

@Component({
  selector: 'app-admin-users',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    FormsModule,
    AdminButton,
    Busy,
    Field,
    Notice,
    PageEmpty,
    PageHeader,
    Panel,
  ],
  template: `
    <div class="space-y-6">
      <admin-page-header
        title="Mailboxes"
        hint="Every account in the system. Disabling one signs it out everywhere and stops
          it receiving mail, but keeps its mail, so it can be turned back on."
      >
        <admin-button variant="primary" (pressed)="creating.set(!creating())">
          {{ creating() ? 'Cancel' : 'Add a mailbox' }}
        </admin-button>
        <admin-button (pressed)="reload()">Refresh</admin-button>
      </admin-page-header>

      <admin-notice [message]="error()" tone="error" />
      <admin-notice [message]="ok()" tone="ok" />

      @if (creating()) {
        <admin-panel title="New mailbox">
          <form class="grid gap-4 sm:grid-cols-2" (ngSubmit)="create()">
            <admin-field
              label="Address"
              hint="A domain admin has no mailbox, so its address must use the local source."
            >
              <input
                class="{{ inputClass }}"
                name="address"
                [(ngModel)]="draft.address"
                required
                placeholder="alice@example.com"
              />
            </admin-field>
            <admin-field label="Display name">
              <input class="{{ inputClass }}" name="displayName" [(ngModel)]="draft.displayName" />
            </admin-field>
            <admin-field label="Source">
              <select class="{{ inputClass }}" name="source" [(ngModel)]="draft.source">
                <option value="local">local — a password is stored here</option>
                <option value="ldap">ldap — authenticated by a directory</option>
                <option value="domain_admin">domain_admin — administers mail, holds none</option>
              </select>
            </admin-field>
            <admin-field
              label="Password"
              hint="Left empty for a domain admin or an LDAP user, who has no stored password."
            >
              <input
                class="{{ inputClass }}"
                name="password"
                type="password"
                autocomplete="new-password"
                [(ngModel)]="draft.password"
              />
            </admin-field>
            <admin-field label="Quota (MiB)" hint="Leave empty for unlimited.">
              <input
                class="{{ inputClass }}"
                name="quota"
                type="number"
                min="0"
                [(ngModel)]="draft.quotaMiB"
                placeholder="unlimited"
              />
            </admin-field>
            <admin-field label="Send limit (per day)" hint="Leave empty for unlimited.">
              <input
                class="{{ inputClass }}"
                name="sendLimit"
                type="number"
                min="0"
                [(ngModel)]="draft.sendLimit"
                placeholder="unlimited"
              />
            </admin-field>
            <label class="sm:col-span-2 flex items-start gap-2 text-sm text-slate-700">
              <input
                type="checkbox"
                name="isDomainAdmin"
                class="mt-0.5 h-4 w-4 rounded border-slate-300"
                [(ngModel)]="draft.isDomainAdmin"
              />
              <span>
                May administer this domain
                <span class="block text-xs text-slate-500">
                  Gives access to these administration pages. It is a permission rather than a kind
                  of account, so a user can hold a mailbox and administer at the same time. A
                  domain_admin source always has it and needs nothing set here.
                </span>
              </span>
            </label>
            <div class="sm:col-span-2 flex items-center gap-3">
              <admin-button variant="primary" type="submit" [disabled]="busy()">
                Create the mailbox
              </admin-button>
              <admin-busy [busy]="busy()" label="Creating…" />
            </div>
          </form>
        </admin-panel>
      }

      <admin-panel [bodyClass]="'p-0'">
        @if (loading()) {
          <div class="px-5 py-6"><admin-busy [busy]="true" label="Loading mailboxes…" /></div>
        } @else if (filtered().length === 0) {
          <admin-empty [text]="query() ? 'No mailbox matches that search.' : 'No mailboxes yet.'" />
        } @else {
          <table class="w-full">
            <thead class="border-b border-slate-200 bg-slate-50">
              <tr>
                <th class="{{ head }}">Address</th>
                <th class="{{ head }}">Source</th>
                <th class="{{ head }}">State</th>
                <th class="{{ head }}">Quota</th>
                <th class="{{ head }}">Used</th>
                <th class="{{ head }} text-right">Actions</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-100">
              @for (u of filtered(); track u.address) {
                <tr>
                  <td class="{{ cell }}">
                    <span class="font-medium text-slate-900">{{ u.address }}</span>
                    @if (u.display_name) {
                      <span class="block text-xs text-slate-500">{{ u.display_name }}</span>
                    }
                  </td>
                  <td class="{{ cell }}">
                    {{ u.source }}
                    @if (u.is_domain_admin) {
                      <span class="mt-1 block text-xs font-medium text-brand-700">administrator</span>
                    }
                  </td>
                  <td class="{{ cell }}">
                    <span
                      class="inline-flex rounded-full px-2 py-0.5 text-xs font-medium ring-1"
                      [class]="
                        u.enabled
                          ? 'bg-emerald-50 text-emerald-700 ring-emerald-200'
                          : 'bg-slate-100 text-slate-600 ring-slate-200'
                      "
                    >
                      {{ u.enabled ? 'enabled' : 'disabled' }}
                    </span>
                    @if (u.must_change_password) {
                      <span class="mt-1 block text-xs text-amber-700">must change password</span>
                    }
                  </td>
                  <td class="{{ cell }}">{{ bytes(u.quota_bytes) }}</td>
                  <td class="{{ cell }}">
                    {{ bytes(u.quota_used_bytes) }}
                    <span class="block text-xs text-slate-400">
                      {{ u.quota_message_count }} messages
                    </span>
                  </td>
                  <td class="{{ cell }}">
                    <div class="flex items-center justify-end gap-2">
                      <admin-button (pressed)="openPassword(u)">Password</admin-button>
                      <admin-button (pressed)="openQuota(u)">Quota</admin-button>
                      @if (canGrantAdmin(u)) {
                        <admin-button
                          [variant]="u.is_domain_admin ? 'secondary' : 'primary'"
                          [disabled]="busy()"
                          (pressed)="toggleAdmin(u)"
                        >
                          {{ u.is_domain_admin ? 'Remove admin' : 'Make admin' }}
                        </admin-button>
                      }
                      <admin-button
                        [variant]="u.enabled ? 'secondary' : 'primary'"
                        [disabled]="busy()"
                        (pressed)="toggleEnabled(u)"
                      >
                        {{ u.enabled ? 'Disable' : 'Enable' }}
                      </admin-button>
                    </div>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        }
      </admin-panel>

      @if (passwordFor(); as target) {
        <admin-panel title="Set a new password for {{ target }}">
          <form class="flex flex-wrap items-end gap-3" (ngSubmit)="setPassword()">
            <admin-field
              label="New password"
              class="min-w-64 flex-1"
              hint="The mailbox is signed out everywhere and must change it at the next sign-in."
            >
              <input
                class="{{ inputClass }}"
                name="password"
                type="password"
                autocomplete="new-password"
                [(ngModel)]="newPassword"
                required
              />
            </admin-field>
            <admin-button variant="primary" type="submit" [disabled]="busy()">Set password</admin-button>
            <admin-button (pressed)="passwordFor.set(null)">Cancel</admin-button>
          </form>
        </admin-panel>
      }

      @if (quotaFor(); as target) {
        <admin-panel title="Quota for {{ target }}">
          <form class="grid gap-4 sm:grid-cols-2" (ngSubmit)="setQuota()">
            <admin-field label="Quota (MiB)" hint="Empty means unlimited.">
              <input
                class="{{ inputClass }}"
                name="quota"
                type="number"
                min="0"
                [(ngModel)]="quotaMiB"
                [placeholder]="target.quota_bytes === null ? 'unlimited' : ''"
              />
            </admin-field>
            <admin-field label="Send limit (per day)" hint="Empty means unlimited.">
              <input
                class="{{ inputClass }}"
                name="sendLimit"
                type="number"
                min="0"
                [(ngModel)]="sendLimit"
                [placeholder]="target.daily_send_limit === null ? 'unlimited' : ''"
              />
            </admin-field>
            <div class="sm:col-span-2 flex items-center gap-3">
              <admin-button variant="primary" type="submit" [disabled]="busy()">Save quota</admin-button>
              <admin-button (pressed)="quotaFor.set(null)">Cancel</admin-button>
            </div>
          </form>
        </admin-panel>
      }
    </div>
  `,
})
export class AdminUsersComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;
  protected readonly bytes = formatBytes;

  protected readonly users = signal<User[]>([]);
  protected readonly query = signal('');
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');

  protected readonly passwordFor = signal<User | null>(null);
  protected readonly quotaFor = signal<User | null>(null);
  protected newPassword = '';
  protected quotaMiB: number | null = null;
  protected sendLimit: number | null = null;

  protected draft: {
    address: string;
    displayName: string;
    source: string;
    password: string;
    quotaMiB: number | null;
    sendLimit: number | null;
    isDomainAdmin: boolean;
  } = {
    address: '',
    displayName: '',
    source: 'local',
    password: '',
    quotaMiB: null,
    sendLimit: null,
    isDomainAdmin: false,
  };

  // filtered is computed rather than a search over the DOM: the list is the
  // only copy of the data, so searching it cannot fall out of step with what is
  // on screen.
  protected readonly filtered = () => {
    const q = this.query().trim().toLowerCase();
    const all = this.users();
    if (!q) return all;
    return all.filter(
      (u) =>
        u.address.toLowerCase().includes(q) ||
        (u.display_name ?? '').toLowerCase().includes(q) ||
        u.source.includes(q),
    );
  };

  ngOnInit(): void {
    this.reload();
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listUsers();
      this.users.set(res.users ?? []);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      const quota = this.draft.quotaMiB;
      const limit = this.draft.sendLimit;
      const source = this.draft.source;
      await this.api.createUser({
        address: this.draft.address.trim(),
        source,
        display_name: this.draft.displayName.trim(),
        // A password is only sent when one was typed. A domain admin or an
        // LDAP user has no stored credential, and sending an empty string
        // would either fail the minimum length or store a password nobody
        // chose.
        ...(this.draft.password ? { password: this.draft.password } : {}),
        ...(quota === null || quota === undefined
          ? {}
          : { quota_bytes: Math.round(quota * 1024 * 1024) }),
        ...(limit === null || limit === undefined ? {} : { daily_send_limit: limit }),
        // Only sent when it is wanted. A false would be a request to take the
        // grant away from a domain_admin, which the server refuses, and the
        // checkbox being unticked means "not asked for" rather than "removed".
        ...(this.draft.isDomainAdmin ? { is_domain_admin: true } : {}),
      });
      this.ok.set(`${this.draft.address.trim()} was created.`);
      this.draft = {
        address: '',
        displayName: '',
        source: 'local',
        password: '',
        quotaMiB: null,
        sendLimit: null,
        isDomainAdmin: false,
      };
      this.creating.set(false);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async toggleEnabled(u: User): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.setUserEnabled(u.address, !u.enabled);
      this.ok.set(`${u.address} is now ${u.enabled ? 'disabled' : 'enabled'}.`);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // canGrantAdmin reports whether the grant is worth offering for this user.
  //
  // A domain_admin always holds it, so a button to remove it would only ever
  // produce the server's refusal, and a shared mailbox can never hold it, so a
  // button to grant it would only ever produce that. The page offers the action
  // where it can succeed rather than showing a control whose failure teaches
  // nothing.
  protected canGrantAdmin(u: User): boolean {
    return u.source !== 'shared' && u.source !== 'domain_admin';
  }

  // toggleAdmin grants or takes back the right to administer the domain.
  protected async toggleAdmin(u: User): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    const granting = !u.is_domain_admin;
    try {
      await this.api.updateUser(u.address, { is_domain_admin: granting });
      this.ok.set(
        granting
          ? `${u.address} may now administer this domain.`
          : `${u.address} may no longer administer this domain.`,
      );
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected openPassword(u: User): void {
    this.newPassword = '';
    this.error.set('');
    this.passwordFor.set(u);
  }

  protected async setPassword(): Promise<void> {
    const target = this.passwordFor();
    if (!target) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.resetUserPassword(target.address, this.newPassword);
      this.ok.set(`The password for ${target.address} was changed.`);
      this.passwordFor.set(null);
      this.newPassword = '';
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected openQuota(u: User): void {
    this.error.set('');
    this.quotaMiB = u.quota_bytes === null ? null : Math.round(u.quota_bytes / (1024 * 1024));
    this.sendLimit = u.daily_send_limit;
    this.quotaFor.set(u);
  }

  protected async setQuota(): Promise<void> {
    const target = this.quotaFor();
    if (!target) return;
    this.busy.set(true);
    this.error.set('');
    try {
      // null is sent rather than omitted for "unlimited": the server treats a
      // missing limit as "leave it alone", so an empty box has to be a
      // deliberate null to clear a limit that is already set.
      await this.api.setUserQuota(target.address, {
        quota_bytes:
          this.quotaMiB === null || this.quotaMiB === undefined
            ? null
            : Math.round(this.quotaMiB * 1024 * 1024),
        daily_send_limit: this.sendLimit === undefined ? null : this.sendLimit,
      });
      this.ok.set(`The quota for ${target.address} was saved.`);
      this.quotaFor.set(null);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}

import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { AdminApi } from '../core/admin/admin-api';
import { User, formatBytes } from '../core/admin/admin.models';
import { errorMessage } from '../core/http-error';
import {
  AdminButton,
  Busy,
  Field,
  Notice,
  PageHeader,
  Panel,
  inputClass,
} from '../core/admin/ui';
import { AdminNavComponent } from '../core/admin/admin-nav.component';
import { AdminPageComponent } from '../core/admin/admin-page.component';

// The mailbox edit screen. The list used to open one small panel per field
// through an Actions menu; editing a person meant opening four of them in turn
// and remembering what the others held. This page puts the fields together and
// saves them as one account.
@Component({
  selector: 'app-admin-user-edit',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    FormsModule,
    RouterLink,
    AdminButton,
    Busy,
    Field,
    Notice,
    PageHeader,
    Panel,
    AdminNavComponent,
    AdminPageComponent,
  ],
  template: `
    <admin-page>
      <admin-nav />
      <div class="space-y-6">
        <admin-page-header
          [title]="user()?.address || 'Edit mailbox'"
          hint="Name, personal email, quota, locale and administration rights in one place."
        >
          <a
            routerLink="/admin/users"
            class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-50"
          >
            Back to mailboxes
          </a>
        </admin-page-header>

        <admin-notice [message]="error()" tone="error" />
        <admin-notice [message]="ok()" tone="ok" />

        @if (loading()) {
          <admin-panel><admin-busy [busy]="true" label="Loading the mailbox…" /></admin-panel>
        } @else if (user(); as u) {
          <admin-panel title="Account">
            <form class="grid gap-4 sm:grid-cols-2" (ngSubmit)="save(u)">
              <admin-field label="Address" hint="The sign-in name and the mailbox address. It cannot be changed here.">
                <input class="{{ inputClass }} bg-slate-50" name="address" [value]="u.address" disabled />
              </admin-field>
              <admin-field label="Source" hint="Where authentication and mail for this account live.">
                <input class="{{ inputClass }} bg-slate-50" name="source" [value]="u.source" disabled />
              </admin-field>
              <admin-field label="Display name">
                <input class="{{ inputClass }}" name="displayName" [(ngModel)]="displayName" />
              </admin-field>
              <admin-field
                label="Personal email"
                hint="Used for the invitation link and password resets. Empty clears it."
              >
                <input class="{{ inputClass }}" name="recoveryEmail" type="email" [(ngModel)]="recoveryEmail" />
              </admin-field>
              <admin-field label="Quota (MiB)" hint="Empty means inherit the domain default.">
                <input class="{{ inputClass }}" name="quota" type="number" min="0" [(ngModel)]="quotaMiB" />
              </admin-field>
              <admin-field label="Send limit (per day)" hint="Empty means unlimited.">
                <input class="{{ inputClass }}" name="sendLimit" type="number" min="0" [(ngModel)]="sendLimit" />
              </admin-field>
              <admin-field label="Timezone" hint="IANA name, for example Europe/Rome.">
                <input class="{{ inputClass }}" name="timezone" [(ngModel)]="timezone" placeholder="UTC" />
              </admin-field>
              <admin-field label="Language" hint="The UI language for this account: en or it.">
                <input class="{{ inputClass }}" name="language" [(ngModel)]="language" placeholder="en" />
              </admin-field>
              @if (u.source !== 'shared' && u.source !== 'domain_admin') {
                <label class="sm:col-span-2 flex items-start gap-2 text-sm text-slate-700">
                  <input
                    type="checkbox"
                    name="isDomainAdmin"
                    class="mt-0.5 h-4 w-4 rounded border-slate-300"
                    [(ngModel)]="isDomainAdmin"
                  />
                  <span>
                    May administer this domain
                    <span class="block text-xs text-slate-500">
                      A domain_admin source always holds this; a shared mailbox never can.
                    </span>
                  </span>
                </label>
              }
              <div class="sm:col-span-2 flex flex-wrap items-center gap-3 border-t border-slate-100 pt-4">
                <admin-button variant="primary" type="submit" [disabled]="busy()">Save changes</admin-button>
                <admin-button type="button" [disabled]="busy()" (pressed)="toggleEnabled(u)">
                  {{ u.enabled ? 'Disable account' : 'Enable account' }}
                </admin-button>
                <admin-busy [busy]="busy()" label="Saving…" />
              </div>
            </form>
          </admin-panel>

          <admin-panel title="Password and invitation">
            <div class="space-y-5">
              <p class="text-sm text-slate-500">
                The account currently holds {{ formatBytes(u.quota_used_bytes) }} in
                {{ u.quota_message_count }} message(s).
              </p>
              <form class="flex flex-wrap items-end gap-3" (ngSubmit)="setPassword(u)">
                <admin-field
                  label="Set a new password"
                  class="min-w-64 flex-1"
                  hint="The account is signed out everywhere and must change it at the next sign-in."
                >
                  <input
                    class="{{ inputClass }}"
                    name="newPassword"
                    type="password"
                    autocomplete="new-password"
                    [(ngModel)]="newPassword"
                  />
                </admin-field>
                <admin-button type="submit" [disabled]="busy() || !newPassword">Set password</admin-button>
              </form>
              <div class="flex flex-wrap items-center gap-3 border-t border-slate-100 pt-4">
                <span class="text-sm text-slate-600">
                  Send a single-use set-password link to the personal email.
                </span>
                <admin-button
                  [disabled]="busy() || !u.recovery_email"
                  (pressed)="sendInvite(u)"
                >
                  Send invitation
                </admin-button>
                @if (!u.recovery_email) {
                  <span class="text-xs text-slate-400">Set a personal email first.</span>
                }
              </div>
            </div>
          </admin-panel>
        }
      </div>
    </admin-page>
  `,
})
export class AdminUserEditComponent implements OnInit {
  private readonly api = inject(AdminApi);
  private readonly route = inject(ActivatedRoute);

  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');
  protected readonly user = signal<User | null>(null);

  protected displayName = '';
  protected recoveryEmail = '';
  protected quotaMiB: number | null = null;
  protected sendLimit: number | null = null;
  protected timezone = 'UTC';
  protected language = 'en';
  protected isDomainAdmin = false;
  protected newPassword = '';

  protected readonly formatBytes = formatBytes;
  protected readonly inputClass = inputClass;

  private address = '';

  ngOnInit(): void {
    this.address = this.route.snapshot.paramMap.get('address') ?? '';
    void this.load();
  }

  private async load(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const u = await this.api.getUser(this.address);
      this.user.set(u);
      this.displayName = u.display_name;
      this.recoveryEmail = u.recovery_email || '';
      this.quotaMiB = u.quota_bytes === null ? null : Math.round(u.quota_bytes / (1024 * 1024));
      this.sendLimit = u.daily_send_limit;
      this.timezone = u.timezone || 'UTC';
      this.language = u.language || 'en';
      this.isDomainAdmin = u.is_domain_admin;
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async save(u: User): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      await this.api.updateUser(u.address, {
        display_name: this.displayName.trim(),
        recovery_email: this.recoveryEmail.trim(),
        timezone: this.timezone.trim() || 'UTC',
        language: this.language.trim() || 'en',
        is_domain_admin: this.isDomainAdmin,
      });
      // Quota is its own endpoint because an empty box means "inherit", which
      // is a different request from "leave the current value alone".
      await this.api.setUserQuota(u.address, {
        quota_bytes:
          this.quotaMiB === null || this.quotaMiB === undefined
            ? null
            : Math.round(this.quotaMiB * 1024 * 1024),
        daily_send_limit: this.sendLimit === undefined ? null : this.sendLimit,
      });
      this.ok.set(`${u.address} was saved.`);
      await this.load();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async toggleEnabled(u: User): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      await this.api.setUserEnabled(u.address, !u.enabled);
      this.ok.set(`${u.address} is now ${u.enabled ? 'disabled' : 'enabled'}.`);
      await this.load();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async setPassword(u: User): Promise<void> {
    if (!this.newPassword) return;
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      await this.api.resetUserPassword(u.address, this.newPassword);
      this.newPassword = '';
      this.ok.set(`The password for ${u.address} was changed.`);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async sendInvite(u: User): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const res = await this.api.inviteUser(u.address);
      this.ok.set(`An invitation was sent to ${res.sent_to}.`);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}

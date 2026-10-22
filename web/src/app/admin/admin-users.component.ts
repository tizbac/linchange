import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { User, formatBytes } from '../core/admin/admin.models';
import { errorMessage } from '../core/http-error';
import {
  AdminButton,
  Busy,
  Dropdown,
  DropdownItem,
  Field,
  Notice,
  PageEmpty,
  PageHeader,
  Panel,
  inputClass,
  tableCellClass,
  tableHeadClass,
} from '../core/admin/ui';
import { AdminNavComponent } from '../core/admin/admin-nav.component';
import { AdminPageComponent } from '../core/admin/admin-page.component';
import { AdminListFooter, AdminSearch } from '../core/admin/admin-list';

@Component({
  selector: 'app-admin-users',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    FormsModule,
    AdminButton,
    Busy,
    Dropdown,
    DropdownItem,
    Field,
    Notice,
    PageEmpty,
    PageHeader,
    Panel,
    AdminNavComponent,
    AdminPageComponent,
    AdminSearch,
    AdminListFooter,
  ],
  templateUrl: './admin-users.component.html',
})
export class AdminUsersComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;
  protected readonly bytes = formatBytes;

  protected readonly users = signal<User[]>([]);
  protected readonly total = signal(0);
  protected query = '';
  protected page = 1;
  protected readonly pageSize = 20;
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
    // Phone lives in the organization directory card, not on the user row.
    phone: string;
    source: string;
    password: string;
    quotaMiB: number | null;
    sendLimit: number | null;
    isDomainAdmin: boolean;
  } = {
    address: '',
    displayName: '',
    phone: '',
    source: 'local',
    password: '',
    quotaMiB: null,
    sendLimit: null,
    isDomainAdmin: false,
  };

  ngOnInit(): void {
    this.reload();
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listUsers({
        search: this.query || undefined,
        limit: this.pageSize,
        offset: (this.page - 1) * this.pageSize,
      });
      this.users.set(res.users ?? []);
      this.total.set(res.total);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected search(value: string): void {
    this.query = value;
    this.page = 1;
    void this.reload();
  }

  protected goPage(page: number): void {
    this.page = page;
    void this.reload();
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
        ...(this.draft.phone.trim() ? { phone: this.draft.phone.trim() } : {}),
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
        phone: '',
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

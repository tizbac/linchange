import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { ImpersonationGrant } from '../core/admin/admin.models';
import { AuthService } from '../core/auth/auth.service';
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
import { AdminNavComponent } from '../core/admin/admin-nav.component';
import { AdminPageComponent } from '../core/admin/admin-page.component';
import { AdminListFooter, AdminSearch } from '../core/admin/admin-list';

@Component({
  selector: 'app-admin-impersonation',
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
    AdminNavComponent,
    AdminPageComponent,
    AdminSearch,
    AdminListFooter,
  ],
  templateUrl: './admin-impersonation.component.html',
})
export class AdminImpersonationComponent implements OnInit {
  private readonly api = inject(AdminApi);
  private readonly auth = inject(AuthService);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;

  protected readonly grants = signal<ImpersonationGrant[]>([]);
  protected readonly total = signal(0);
  protected query = '';
  protected page = 1;
  protected readonly pageSize = 20;
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');

  protected draft = { actor: '', target: '', reason: '', ttlMinutes: null };

  // grantsRequired is the server's own answer, read from /me. The panel has to
  // say which mode it is in, because the same list means two different things:
  // an allowlist when a grant is required, and only a record of intentions when
  // it is not.
  protected get grantsRequired(): boolean {
    return this.auth.impersonationRequiresGrant();
  }

  ngOnInit(): void {
    this.reload();
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      // The default listing is the caller's own grants, so a supervisor does not
      // have to name themselves to see what they handed out.
      const res = await this.api.listImpersonationGrants(undefined, {
        search: this.query || undefined,
        limit: this.pageSize,
        offset: (this.page - 1) * this.pageSize,
      });
      this.grants.set(res.grants ?? []);
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
      const ttl = this.draft.ttlMinutes;
      await this.api.grantImpersonation({
        actor: this.draft.actor.trim(),
        target: this.draft.target.trim(),
        reason: this.draft.reason.trim(),
        ...(ttl === null || ttl === undefined
          ? {}
          : { ttl_seconds: Math.round(ttl * 60) }),
      });
      this.ok.set(
        `${this.draft.actor.trim()} may act as ${this.draft.target.trim()}${
          ttl ? ` for ${ttl} minutes` : ''
        }.`,
      );
      this.draft = { actor: '', target: '', reason: '', ttlMinutes: null };
      this.creating.set(false);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async revoke(g: ImpersonationGrant): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.revokeImpersonation(g.actor_address, g.target_address);
      this.ok.set(`The grant for ${g.target_address} was revoked.`);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}

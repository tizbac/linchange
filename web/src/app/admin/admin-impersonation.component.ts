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
  ],
  template: `
    <div class="space-y-6">
      <admin-page-header
        title="Impersonation"
        hint="A grant lets one domain admin sign in as one named user. It is recorded every
          time it is used, so the audit log is the answer to who read whose mail."
      >
        <admin-button variant="primary" (pressed)="creating.set(!creating())">
          {{ creating() ? 'Cancel' : 'Grant access' }}
        </admin-button>
        <admin-button (pressed)="reload()">Refresh</admin-button>
      </admin-page-header>

      <admin-notice [message]="error()" tone="error" />
      <admin-notice [message]="ok()" tone="ok" />

      @if (grantsRequired) {
        <admin-panel>
          <p class="text-sm text-slate-600">
            This server requires a grant for every impersonation, so the list below is
            the complete set of people who can act as somebody else. A grant can be
            given an expiry, and revoking one takes effect on the next sign-in rather
            than when the outstanding session ends.
          </p>
        </admin-panel>
      } @else {
        <admin-panel>
          <p class="text-sm text-amber-800">
            This server does not require a grant: any domain admin may impersonate any
            user. The grants below record an intention but do not restrict anything.
          </p>
        </admin-panel>
      }

      @if (creating()) {
        <admin-panel title="New grant">
          <form class="grid gap-4 sm:grid-cols-2" (ngSubmit)="create()">
            <admin-field label="Admin" hint="The domain admin who will be allowed to act as the user.">
              <input
                class="{{ inputClass }}"
                name="actor"
                [(ngModel)]="draft.actor"
                required
                placeholder="root@example.com"
              />
            </admin-field>
            <admin-field label="User" hint="Whose mailbox the admin may act as.">
              <input
                class="{{ inputClass }}"
                name="target"
                [(ngModel)]="draft.target"
                required
                placeholder="bob@example.com"
              />
            </admin-field>
            <admin-field
              label="Reason"
              hint="Required. It is the record of why the power was handed over, and it is
                written to the audit log."
            >
              <input
                class="{{ inputClass }}"
                name="reason"
                [(ngModel)]="draft.reason"
                required
                placeholder="a support incident"
              />
            </admin-field>
            <admin-field label="Expires in (minutes)" hint="Leave empty for a grant that does not expire.">
              <input
                class="{{ inputClass }}"
                name="ttl"
                type="number"
                min="1"
                [(ngModel)]="draft.ttlMinutes"
                placeholder="never"
              />
            </admin-field>
            <div class="sm:col-span-2 flex items-center gap-3">
              <admin-button variant="primary" type="submit" [disabled]="busy()">Grant</admin-button>
              <admin-busy [busy]="busy()" label="Granting…" />
            </div>
          </form>
        </admin-panel>
      }

      <admin-panel [bodyClass]="'p-0'">
        @if (loading()) {
          <div class="px-5 py-6"><admin-busy [busy]="true" label="Loading grants…" /></div>
        } @else if (grants().length === 0) {
          <admin-empty text="No impersonation grants." />
        } @else {
          <table class="w-full">
            <thead class="border-b border-slate-200 bg-slate-50">
              <tr>
                <th class="{{ head }}">Admin</th>
                <th class="{{ head }}">Acting as</th>
                <th class="{{ head }}">Reason</th>
                <th class="{{ head }}">State</th>
                <th class="{{ head }} text-right">Actions</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-100">
              @for (g of grants(); track g.actor_address + g.target_address) {
                <tr>
                  <td class="{{ cell }} font-medium text-slate-900">{{ g.actor_address }}</td>
                  <td class="{{ cell }}">{{ g.target_address }}</td>
                  <td class="{{ cell }}">{{ g.reason }}</td>
                  <td class="{{ cell }}">
                    <span
                      class="inline-flex rounded-full px-2 py-0.5 text-xs font-medium ring-1"
                      [class]="
                        g.active
                          ? 'bg-emerald-50 text-emerald-700 ring-emerald-200'
                          : 'bg-slate-100 text-slate-600 ring-slate-200'
                      "
                    >
                      {{ g.active ? 'active' : 'not active' }}
                    </span>
                    @if (g.expires_at) {
                      <span class="mt-1 block text-xs text-slate-400">
                        until {{ g.expires_at }}
                      </span>
                    }
                  </td>
                  <td class="{{ cell }} text-right">
                    <admin-button variant="danger" [disabled]="busy()" (pressed)="revoke(g)">
                      Revoke
                    </admin-button>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        }
      </admin-panel>
    </div>
  `,
})
export class AdminImpersonationComponent implements OnInit {
  private readonly api = inject(AdminApi);
  private readonly auth = inject(AuthService);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;

  protected readonly grants = signal<ImpersonationGrant[]>([]);
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
      const res = await this.api.listImpersonationGrants();
      this.grants.set(res.grants ?? []);
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

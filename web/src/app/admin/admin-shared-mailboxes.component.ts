import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { SharedMailbox, SharedMailboxGrant } from '../core/admin/admin.models';
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

@Component({
  selector: 'app-admin-shared-mailboxes',
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
  ],
  template: `
    <admin-page>
      <admin-nav />
    <div class="space-y-6">
      <admin-page-header
        title="Shared mailboxes"
        hint="A team address. Declaring one is what makes it shared: members are granted
          access to it, and it is not a person's account."
      >
        <admin-button variant="primary" (pressed)="creating.set(!creating())">
          {{ creating() ? 'Cancel' : 'Declare a shared mailbox' }}
        </admin-button>
        <admin-button (pressed)="reload()">Refresh</admin-button>
      </admin-page-header>

      <admin-notice [message]="error()" tone="error" />
      <admin-notice [message]="ok()" tone="ok" />

      @if (creating()) {
        <admin-panel title="New shared mailbox">
          <form class="grid gap-4 sm:grid-cols-2" (ngSubmit)="create()">
            <admin-field label="Address" hint="The team address, for example support@example.com.">
              <input
                class="{{ inputClass }}"
                name="address"
                [(ngModel)]="draft.address"
                required
                placeholder="support@example.com"
              />
            </admin-field>
            <admin-field label="Display name">
              <input class="{{ inputClass }}" name="displayName" [(ngModel)]="draft.displayName" />
            </admin-field>
            <admin-field label="Description">
              <input class="{{ inputClass }}" name="description" [(ngModel)]="draft.description" />
            </admin-field>
            <label class="flex items-center gap-2 self-end pb-1.5 text-sm text-slate-700">
              <input type="checkbox" name="openAccess" [(ngModel)]="draft.openAccess" />
              Anyone may read it without being a member
            </label>
            <div class="sm:col-span-2 flex items-center gap-3">
              <admin-button variant="primary" type="submit" [disabled]="busy()">
                Declare it
              </admin-button>
              <admin-busy [busy]="busy()" label="Declaring…" />
            </div>
          </form>
        </admin-panel>
      }

      @if (loading()) {
        <admin-panel><admin-busy [busy]="true" label="Loading shared mailboxes…" /></admin-panel>
      } @else if (boxes().length === 0) {
        <admin-panel [bodyClass]="'p-0'"><admin-empty text="No shared mailboxes yet." /></admin-panel>
      } @else {
        @for (box of boxes(); track box.address) {
          <admin-panel [title]="box.address" [hint]="box.description || box.display_name">
            <div class="space-y-4">
              <div class="flex flex-wrap items-center gap-3">
                @if (box.open_access) {
                  <span
                    class="inline-flex rounded-full bg-amber-50 px-2 py-0.5 text-xs font-medium text-amber-800 ring-1 ring-amber-200"
                  >
                    open to everybody
                  </span>
                }
                <span class="text-xs text-slate-500">
                  {{ box.members.length }} member{{ box.members.length === 1 ? '' : 's' }}
                </span>
                <span class="flex-1"></span>
                <admin-button (pressed)="openMembers(box)">Add a member</admin-button>
                <admin-button variant="danger" [disabled]="busy()" (pressed)="undeclare(box)">
                  Undeclare
                </admin-button>
              </div>

              @if (box.members.length === 0) {
                <admin-empty text="No members yet, so only an open mailbox is reachable." />
              } @else {
                <table class="w-full">
                  <thead class="border-b border-slate-200 bg-slate-50">
                    <tr>
                      <th class="{{ head }}">Member</th>
                      <th class="{{ head }}">Access</th>
                      <th class="{{ head }} text-right">Actions</th>
                    </tr>
                  </thead>
                  <tbody class="divide-y divide-slate-100">
                    @for (m of box.members; track m.member_address) {
                      <tr>
                        <td class="{{ cell }} font-medium text-slate-900">{{ m.member_address }}</td>
                        <td class="{{ cell }}">{{ m.level }}</td>
                        <td class="{{ cell }} text-right">
                          <admin-button
                            variant="danger"
                            [disabled]="busy()"
                            (pressed)="removeMember(box, m)"
                          >
                            Remove
                          </admin-button>
                        </td>
                      </tr>
                    }
                  </tbody>
                </table>
              }

              @if (memberFor() === box.address) {
                <form class="flex flex-wrap items-end gap-3" (ngSubmit)="addMember(box)">
                  <admin-field
                    label="Member address"
                    hint="An existing user, not another shared mailbox."
                    class="min-w-64 flex-1"
                  >
                    <input
                      class="{{ inputClass }}"
                      name="member"
                      [(ngModel)]="memberAddress"
                      required
                      placeholder="bob@example.com"
                    />
                  </admin-field>
                  <admin-field label="Access">
                    <select class="{{ inputClass }}" name="level" [(ngModel)]="memberLevel">
                      <option value="read">read</option>
                      <option value="read">read</option>
                      <option value="contribute">contribute</option>
                      <option value="editor">editor</option>
                      <option value="owner">owner</option>
                    </select>
                  </admin-field>
                  <admin-button variant="primary" type="submit" [disabled]="busy()">Grant</admin-button>
                  <admin-button (pressed)="memberFor.set('')">Cancel</admin-button>
                </form>
              }
            </div>
          </admin-panel>
        }
      }
    </div>
  `,
})
export class AdminSharedMailboxesComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;

  protected readonly boxes = signal<SharedMailbox[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');
  protected readonly memberFor = signal('');

  protected memberAddress = '';
  protected memberLevel: SharedMailboxGrant['level'] = 'editor';
  protected draft = {
    address: '',
    displayName: '',
    description: '',
    openAccess: false,
  };

  ngOnInit(): void {
    this.reload();
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listSharedMailboxes();
      this.boxes.set(res.shared_mailboxes ?? []);
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
      await this.api.declareSharedMailbox({
        address: this.draft.address.trim(),
        display_name: this.draft.displayName.trim(),
        description: this.draft.description.trim(),
        open_access: this.draft.openAccess,
      });
      this.ok.set(`${this.draft.address.trim()} was declared as a shared mailbox.`);
      this.draft = { address: '', displayName: '', description: '', openAccess: false };
      this.creating.set(false);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected openMembers(box: SharedMailbox): void {
    this.error.set('');
    this.memberAddress = '';
    this.memberFor.set(this.memberFor() === box.address ? '' : box.address);
  }

  protected async addMember(box: SharedMailbox): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.addSharedMailboxMember(box.address, this.memberAddress.trim(), this.memberLevel);
      this.ok.set(`${this.memberAddress.trim()} can now reach ${box.address}.`);
      this.memberFor.set('');
      this.memberAddress = '';
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeMember(box: SharedMailbox, m: SharedMailboxGrant): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.removeSharedMailboxMember(box.address, m.member_address);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async undeclare(box: SharedMailbox): Promise<void> {
    if (
      !confirm(
        `Undeclare ${box.address}? It stops being a shared mailbox. Its mail is kept, but the ` +
          `address will no longer accept mail until it is declared again.`,
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.undeclareSharedMailbox(box.address);
      this.ok.set(`${box.address} is no longer a shared mailbox.`);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}

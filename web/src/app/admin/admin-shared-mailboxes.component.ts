import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { SharedMailbox, SharedMailboxGrant } from '../core/admin/admin.models';
import { errorMessage } from '../core/http-error';
import {
  AdminButton,
  Busy,
  Field,
  Multiselect,
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
    Multiselect,
    Notice,
    PageEmpty,
    PageHeader,
    Panel,
    AdminNavComponent,
    AdminPageComponent,
  ],
  templateUrl: './admin-shared-mailboxes.component.html',
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
  protected readonly availableUsers = signal<string[]>([]);

  protected memberAddresses: string[] = [];
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
      const [boxesRes, usersRes] = await Promise.all([
        this.api.listSharedMailboxes(),
        this.api.listUsers(),
      ]);
      this.boxes.set(boxesRes.shared_mailboxes ?? []);
      this.availableUsers.set((usersRes.users ?? []).map(u => u.address));
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
    this.memberAddresses = [];
    this.memberFor.set(this.memberFor() === box.address ? '' : box.address);
  }

  protected async addMember(box: SharedMailbox): Promise<void> {
    if (this.memberAddresses.length === 0) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await Promise.all(this.memberAddresses.map(addr =>
        this.api.addSharedMailboxMember(box.address, addr, this.memberLevel)
      ));
      this.ok.set(`${this.memberAddresses.join(', ')} can now reach ${box.address}.`);
      this.memberFor.set('');
      this.memberAddresses = [];
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // Changing a member's level re-grants, which the server treats as an upsert:
  // the grant row is updated in place, so the member is never removed and
  // re-added and keeps everything tied to that grant.
  protected async setMemberLevel(
    box: SharedMailbox,
    m: SharedMailboxGrant,
    level: SharedMailboxGrant['level'],
  ): Promise<void> {
    if (level === m.level) return;
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      await this.api.addSharedMailboxMember(box.address, m.member_address, level);
      this.ok.set(`${m.member_address} now has ${level} access to ${box.address}.`);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      // Reload either way so the selector shows what the server actually holds
      // rather than the value the browser just tried to apply.
      await this.reload();
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

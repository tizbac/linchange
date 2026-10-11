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

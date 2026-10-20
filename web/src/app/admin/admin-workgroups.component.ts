import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { Domain, Workgroup, WorkgroupMember } from '../core/admin/admin.models';
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

// Organizations are workgroups: a named group of people defined by the domains
// it owns. Anyone with an address in one of those domains belongs to it, so the
// page is really about domains — the members list is a convenience for naming
// who administers the group, not the boundary of who is in it.
@Component({
  selector: 'app-admin-workgroups',
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
  templateUrl: './admin-workgroups.component.html',
})
export class AdminWorkgroupsComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;

  protected readonly workgroups = signal<Workgroup[]>([]);
  protected readonly domains = signal<Domain[]>([]);
  protected readonly users = signal<string[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);
  protected readonly editing = signal<number | null>(null);
  protected readonly memberFor = signal<number | null>(null);
  protected readonly error = signal('');
  protected readonly ok = signal('');

  protected memberAddresses: string[] = [];
  protected memberRole: WorkgroupMember['role'] = 'member';

  protected draft = {
    name: '',
    domain: '',
    displayName: '',
    description: '',
    openBooking: true,
  };

  ngOnInit(): void {
    this.reload();
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const [groups, domainRes, userRes] = await Promise.all([
        this.api.listWorkgroups(),
        this.api.listDomains(),
        this.api.listUsers(),
      ]);
      this.workgroups.set(groups.workgroups ?? []);
      this.domains.set(domainRes.domains ?? []);
      this.users.set((userRes.users ?? []).map((u) => u.address));
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
      const wg = await this.api.createWorkgroup({
        name: this.draft.name.trim(),
        domain: this.draft.domain.trim(),
        display_name: this.draft.displayName.trim(),
        description: this.draft.description.trim(),
        open_booking: this.draft.openBooking,
      });
      this.ok.set(`${wg.display_name || wg.name} was created.`);
      this.draft = { name: '', domain: '', displayName: '', description: '', openBooking: true };
      this.creating.set(false);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // ownedDomains is the set of domains the group covers: the domains whose row
  // names it, plus its home domain, which is not necessarily a domain row.
  protected ownedDomains(wg: Workgroup): Domain[] {
    return this.domains().filter((d) => d.workgroup_id === wg.id);
  }

  protected openEdit(wg: Workgroup): void {
    this.error.set('');
    this.editing.set(this.editing() === wg.id ? null : wg.id);
  }

  protected async save(wg: Workgroup): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      const updated = await this.api.updateWorkgroup(wg.id, {
        name: wg.name,
        domain: wg.domain,
        display_name: wg.display_name,
        description: wg.description,
        open_booking: wg.open_booking,
      });
      this.ok.set(`${updated.display_name || updated.name} was updated.`);
      this.editing.set(null);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(wg: Workgroup): Promise<void> {
    if (
      !confirm(
        `Delete ${wg.display_name || wg.name}? Its calendars and membership are removed. ` +
          'The domains it owns are left in place and simply belong to no organization.',
      )
    ) {
      return;
    }
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.deleteWorkgroup(wg.id);
      this.ok.set(`${wg.display_name || wg.name} was deleted.`);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected openMembers(wg: Workgroup): void {
    this.error.set('');
    this.memberAddresses = [];
    this.memberFor.set(this.memberFor() === wg.id ? null : wg.id);
  }

  protected async addMembers(wg: Workgroup): Promise<void> {
    if (this.memberAddresses.length === 0) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await Promise.all(
        this.memberAddresses.map((address) =>
          this.api.addWorkgroupMember(wg.id, { address, role: this.memberRole }),
        ),
      );
      this.ok.set(`${this.memberAddresses.join(', ')} are now members of ${wg.name}.`);
      this.memberFor.set(null);
      this.memberAddresses = [];
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeMember(wg: Workgroup, m: WorkgroupMember): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.removeWorkgroupMember(wg.id, m.address);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // onAttachDomain reads the picked domain off the select and resets it, so the
  // control shows the prompt again rather than claiming a domain is selected.
  protected onAttachDomain(wg: Workgroup, event: Event): void {
    const select = event.target as HTMLSelectElement;
    const name = select.value;
    select.value = '';
    void this.attachDomain(wg, name);
  }

  protected async attachDomain(wg: Workgroup, name: string): Promise<void> {
    if (!name) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.updateDomain(name, { workgroup_id: wg.id });
      this.ok.set(`${name} now belongs to ${wg.display_name || wg.name}.`);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async detachDomain(wg: Workgroup, name: string): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.updateDomain(name, { workgroup_id: null });
      this.ok.set(`${name} no longer belongs to ${wg.display_name || wg.name}.`);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // unownedDomains are the domains an administrator can still hand to this group.
  protected unownedDomains(wg: Workgroup): Domain[] {
    return this.domains().filter((d) => d.workgroup_id !== wg.id && d.name !== wg.domain);
  }
}

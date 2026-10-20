import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { Domain, OrgCalendar, Workgroup, WorkgroupGroup, WorkgroupMember } from '../core/admin/admin.models';
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
  protected readonly calendarFor = signal<number | null>(null);
  protected readonly calendars = signal<Map<number, OrgCalendar[]>>(new Map());
  protected readonly groupFor = signal<number | null>(null);
  protected readonly groups = signal<Map<number, WorkgroupGroup[]>>(new Map());
  protected readonly membersFor = signal<number | null>(null);
  protected readonly groupMembers = signal<Map<number, string[]>>(new Map());
  protected readonly error = signal('');
  protected readonly ok = signal('');

  protected memberAddresses: string[] = [];
  protected memberRole: WorkgroupMember['role'] = 'member';
  protected calendarDraft = { name: '', color: '#3b82f6', description: '' };
  protected groupDraft = { name: '', description: '' };
  protected groupMemberAddresses: string[] = [];

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

  // memberCandidates offers the addresses the picker does not already contain:
  // somebody in one of the organization's domains is a member whether or not a
  // row says so, and offering them would ask the administrator to add a person
  // the server will refuse to add.
  protected memberCandidates(wg: Workgroup): string[] {
    const taken = new Set((wg.members ?? []).map((m) => m.address.toLowerCase()));
    return this.users().filter((address) => !taken.has(address.toLowerCase()));
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

  // --- organization calendars ---

  // openCalendars loads an organization's calendars the first time its panel is
  // opened. They are fetched on demand because most visits to this page are
  // about something else, and a request per organization to look at nobody's
  // calendars is a request too many.
  protected openCalendars(wg: Workgroup): void {
    this.error.set('');
    const next = this.calendarFor() === wg.id ? null : wg.id;
    this.calendarFor.set(next);
    if (next !== null && !this.calendars().has(wg.id)) {
      void this.loadCalendars(wg);
    }
  }

  private async loadCalendars(wg: Workgroup): Promise<void> {
    try {
      const res = await this.api.listWorkgroupCalendars(wg.id);
      const next = new Map(this.calendars());
      next.set(wg.id, res.calendars ?? []);
      this.calendars.set(next);
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected async createCalendar(wg: Workgroup): Promise<void> {
    const name = this.calendarDraft.name.trim();
    if (!name) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.createWorkgroupCalendar(wg.id, {
        name,
        color: this.calendarDraft.color,
        description: this.calendarDraft.description.trim(),
      });
      this.ok.set(`${name} belongs to ${wg.display_name || wg.name}.`);
      this.calendarDraft = { name: '', color: '#3b82f6', description: '' };
      await this.loadCalendars(wg);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async renameCalendar(wg: Workgroup, cal: OrgCalendar): Promise<void> {
    const name = prompt('Calendar name', cal.name);
    if (name === null || !name.trim() || name.trim() === cal.name) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.updateWorkgroupCalendar(wg.id, cal.id, { name: name.trim() });
      await this.loadCalendars(wg);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeCalendar(wg: Workgroup, cal: OrgCalendar): Promise<void> {
    if (!confirm(`Delete ${cal.name}? Every event in it goes with it.`)) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.deleteWorkgroupCalendar(wg.id, cal.id);
      this.ok.set(`${cal.name} was deleted.`);
      await this.loadCalendars(wg);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // --- organization groups ---

  protected openGroups(wg: Workgroup): void {
    this.error.set('');
    const next = this.groupFor() === wg.id ? null : wg.id;
    this.groupFor.set(next);
    if (next !== null && !this.groups().has(wg.id)) {
      void this.loadGroups(wg);
    }
  }

  private async loadGroups(wg: Workgroup): Promise<void> {
    try {
      const res = await this.api.listWorkgroupGroups(wg.id);
      const next = new Map(this.groups());
      next.set(wg.id, res.groups ?? []);
      this.groups.set(next);
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected async createGroup(wg: Workgroup): Promise<void> {
    const name = this.groupDraft.name.trim();
    if (!name) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.createWorkgroupGroup(wg.id, {
        name,
        description: this.groupDraft.description.trim(),
      });
      this.ok.set(`${name} was created in ${wg.display_name || wg.name}.`);
      this.groupDraft = { name: '', description: '' };
      await this.loadGroups(wg);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async renameGroup(wg: Workgroup, group: WorkgroupGroup): Promise<void> {
    const name = group.everyone ? group.name : prompt('Group name', group.name);
    if (name === null || !name.trim() || name.trim() === group.name) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.updateWorkgroupGroup(wg.id, group.id, { name: name.trim() });
      await this.loadGroups(wg);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeGroup(wg: Workgroup, group: WorkgroupGroup): Promise<void> {
    if (!confirm(`Delete ${group.name}? Calendars shared with it lose the share too.`)) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.deleteWorkgroupGroup(wg.id, group.id);
      this.ok.set(`${group.name} was deleted.`);
      await this.loadGroups(wg);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // openGroupMembers shows who is in a group, loading the list the first time
  // it is asked for.
  protected openGroupMembers(group: WorkgroupGroup): void {
    this.error.set('');
    this.groupMemberAddresses = [];
    const next = this.membersFor() === group.id ? null : group.id;
    this.membersFor.set(next);
    if (next !== null && !this.groupMembers().has(group.id)) {
      void this.loadGroupMembers(group);
    }
  }

  private async loadGroupMembers(group: WorkgroupGroup): Promise<void> {
    try {
      const res = await this.api.listWorkgroupGroupMembers(group.workgroup_id, group.id);
      const next = new Map(this.groupMembers());
      next.set(group.id, res.members ?? []);
      this.groupMembers.set(next);
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  // groupCandidates offers only addresses the group does not already hold:
  // adding somebody twice is a request the server answers with "already there".
  protected groupCandidates(group: WorkgroupGroup): string[] {
    const taken = new Set((this.groupMembers().get(group.id) ?? []).map((a) => a.toLowerCase()));
    return this.users().filter((address) => !taken.has(address.toLowerCase()));
  }

  protected async addGroupMembers(group: WorkgroupGroup): Promise<void> {
    if (this.groupMemberAddresses.length === 0) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await Promise.all(
        this.groupMemberAddresses.map((address) =>
          this.api.addWorkgroupGroupMember(group.workgroup_id, group.id, { address }),
        ),
      );
      this.ok.set(`${this.groupMemberAddresses.join(', ')} are in ${group.name}.`);
      this.groupMemberAddresses = [];
      await this.loadGroupMembers(group);
      await this.loadGroupsById(group.workgroup_id);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeGroupMember(group: WorkgroupGroup, address: string): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.removeWorkgroupGroupMember(group.workgroup_id, group.id, address);
      await this.loadGroupMembers(group);
      await this.loadGroupsById(group.workgroup_id);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // loadGroupsById refreshes an organization's group list without opening its
  // panel, so a count that changed as a side effect of editing members is not
  // left showing the old number.
  private async loadGroupsById(workgroupID: number): Promise<void> {
    try {
      const res = await this.api.listWorkgroupGroups(workgroupID);
      const next = new Map(this.groups());
      next.set(workgroupID, res.groups ?? []);
      this.groups.set(next);
    } catch (err) {
      this.error.set(errorMessage(err));
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

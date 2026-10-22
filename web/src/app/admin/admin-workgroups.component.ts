import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { AdminApi } from '../core/admin/admin-api';
import { Domain, Workgroup } from '../core/admin/admin.models';
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
} from '../core/admin/ui';
import { AdminNavComponent } from '../core/admin/admin-nav.component';
import { AdminPageComponent } from '../core/admin/admin-page.component';
import { AdminListFooter, AdminSearch } from '../core/admin/admin-list';

// Organizations are workgroups: a named group of people defined by the domains
// it owns. Anyone with an address in one of those domains belongs to it, so the
// page is really about domains. Members, calendars and groups are administered
// on the organization's own Manage page, which keeps this list a list.
@Component({
  selector: 'app-admin-workgroups',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    FormsModule,
    RouterLink,
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
  templateUrl: './admin-workgroups.component.html',
})
export class AdminWorkgroupsComponent implements OnInit {
  private readonly api = inject(AdminApi);
  // The organization's theme selector offers the same themes as the top bar.
  protected readonly auth = inject(AuthService);

  protected readonly inputClass = inputClass;

  protected readonly workgroups = signal<Workgroup[]>([]);
  protected readonly total = signal(0);
  protected query = '';
  protected page = 1;
  protected readonly pageSize = 20;
  protected readonly domains = signal<Domain[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);
  protected readonly editing = signal<number | null>(null);
  protected readonly error = signal('');
  protected readonly ok = signal('');

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
      const [groups, domainRes] = await Promise.all([
        this.api.listWorkgroups({
          search: this.query || undefined,
          limit: this.pageSize,
          offset: (this.page - 1) * this.pageSize,
        }),
        this.api.listDomains(),
      ]);
      this.workgroups.set(groups.workgroups ?? []);
      this.total.set(groups.total);
      this.domains.set(domainRes.domains ?? []);
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

  protected async uploadLogo(wg: Workgroup, event: Event): Promise<void> {
    const input = event.target as HTMLInputElement;
    const file = input.files?.[0];
    if (!file) return;
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const updated = await this.api.uploadWorkgroupLogo(wg.id, file);
      this.replaceWorkgroup(updated);
      this.ok.set('Logo updated.');
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
      input.value = '';
    }
  }

  protected async removeLogo(wg: Workgroup): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const updated = await this.api.deleteWorkgroupLogo(wg.id);
      this.replaceWorkgroup(updated);
      this.ok.set('Logo removed.');
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  private replaceWorkgroup(updated: Workgroup): void {
    this.workgroups.update((list) =>
      list.map((w) =>
        w.id === updated.id ? { ...w, ...updated, logo_url: updated.logo_url ?? '' } : w,
      ),
    );
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
        theme: wg.theme ?? '',
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

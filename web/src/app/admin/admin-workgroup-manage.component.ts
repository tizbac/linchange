import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { ActivatedRoute } from '@angular/router';
import { AdminApi } from '../core/admin/admin-api';
import { Domain, OrgCalendar, Workgroup, WorkgroupGroup, WorkgroupMember } from '../core/admin/admin.models';
import { CalendarShare } from '../core/calendar/calendar.models';
import { AddressBook, AddressBookShare } from '../core/contacts/contacts.models';
import { AuthService } from '../core/auth/auth.service';
import { errorMessage } from '../core/http-error';
import { AdminListFooter, AdminSearch } from '../core/admin/admin-list';
import {
  AdminButton,
  Busy,
  Field,
  Notice,
  Panel,
  inputClass,
  tableCellClass,
  tableHeadClass,
} from '../core/admin/ui';
import { AdminNavComponent } from '../core/admin/admin-nav.component';
import { AdminPageComponent } from '../core/admin/admin-page.component';

type Tab = 'general' | 'members' | 'calendars' | 'groups' | 'addressbooks';

// AdminWorkgroupManageComponent is the organization's own screen. The list page
// answers "which organizations are there"; this one answers everything about
// one of them, one tab at a time.
@Component({
  selector: 'app-admin-workgroup-manage',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    FormsModule,
    RouterLink,
    AdminButton,
    Busy,
    Field,
    Notice,
    Panel,
    AdminNavComponent,
    AdminPageComponent,
    AdminSearch,
    AdminListFooter,
  ],
  template: `
    <admin-page>
      <admin-nav />
      <div class="space-y-6">
        <div class="flex flex-wrap items-center gap-3">
          <a routerLink="/admin/workgroups" class="text-sm font-medium text-brand-700 hover:underline">
            Organizations
          </a>
          <span class="text-slate-300">/</span>
          <h1 class="text-2xl font-semibold tracking-tight text-slate-900">
            {{ wg()?.display_name || wg()?.name || 'Organization' }}
          </h1>
        </div>

        <admin-notice [message]="error()" tone="error" />
        <admin-notice [message]="ok()" tone="ok" />

        @if (loading()) {
          <admin-panel><admin-busy [busy]="true" label="Loading organization…" /></admin-panel>
        } @else if (wg(); as org) {
          <div class="flex flex-wrap gap-1 border-b border-slate-200">
            @for (item of tabs; track item.key) {
              <button
                type="button"
                (click)="tab.set(item.key)"
                class="border-b-2 px-4 py-2 text-sm font-medium"
                [class]="tab() === item.key
                  ? 'border-brand-600 text-brand-700'
                  : 'border-transparent text-slate-500 hover:text-slate-800'"
              >
                {{ item.label }}
              </button>
            }
          </div>

          @if (tab() === 'general') {
            <admin-panel title="General settings">
              <form class="grid gap-4 sm:grid-cols-2" (ngSubmit)="saveGeneral()">
                <admin-field label="Name">
                  <input class="{{ inputClass }}" name="name" [(ngModel)]="general.name" />
                </admin-field>
                <admin-field label="Display name">
                  <input class="{{ inputClass }}" name="displayName" [(ngModel)]="general.displayName" />
                </admin-field>
                <admin-field label="Description" class="sm:col-span-2">
                  <input class="{{ inputClass }}" name="description" [(ngModel)]="general.description" />
                </admin-field>
                <admin-field label="Theme">
                  <select class="{{ inputClass }}" name="theme" [(ngModel)]="general.theme">
                    <option value="">No organization default</option>
                    @for (t of auth.themeOptions(); track t.key) {
                      <option [value]="t.key">{{ t.name }}</option>
                    }
                  </select>
                </admin-field>
                <label class="flex items-start gap-2 pt-6 text-sm text-slate-700">
                  <input type="checkbox" name="openBooking" [(ngModel)]="general.openBooking" class="mt-0.5 h-4 w-4" />
                  <span>Anyone may book the organization's calendars</span>
                </label>
                <div class="sm:col-span-2 flex items-center gap-3">
                  <admin-button variant="primary" type="submit" [disabled]="busy()">Save</admin-button>
                  <admin-busy [busy]="busy()" label="Saving…" />
                </div>
              </form>

              <div class="mt-6 flex flex-wrap items-center gap-3 rounded-lg border border-slate-200 p-3">
                @if (org.logo_url) {
                  <img [src]="org.logo_url" [alt]="org.display_name || org.name" class="h-10 max-w-[180px] object-contain" />
                } @else {
                  <span class="text-xs text-slate-500">No logo. The organization name is shown instead.</span>
                }
                <span class="flex-1"></span>
                <label class="cursor-pointer rounded-lg border border-slate-300 px-3 py-1.5 text-xs font-medium text-slate-700 hover:bg-slate-100">
                  {{ org.logo_url ? 'Replace logo' : 'Upload logo' }}
                  <input type="file" accept="image/png,image/jpeg,image/gif,image/webp" class="hidden" (change)="uploadLogo($event)" />
                </label>
                @if (org.logo_url) {
                  <admin-button variant="danger" [disabled]="busy()" (pressed)="removeLogo()">Remove logo</admin-button>
                }
              </div>

              <div class="mt-6">
                <h3 class="text-sm font-semibold text-slate-800">Domains</h3>
                <p class="mt-1 text-xs text-slate-500">
                  Every enabled mailbox in these domains belongs to the organization.
                </p>
                <ul class="mt-3 divide-y divide-slate-100">
                  @for (d of ownedDomains(); track d.name) {
                    <li class="flex items-center justify-between py-2 text-sm">
                      <span>{{ d.name }}</span>
                      <admin-button [disabled]="busy()" (pressed)="detachDomain(d.name)">Detach</admin-button>
                    </li>
                  } @empty {
                    <li class="py-2 text-sm text-slate-500">No extra domains.</li>
                  }
                </ul>
                @if (unownedDomains().length > 0) {
                  <div class="mt-3 flex items-center gap-2">
                    <select class="{{ inputClass }} max-w-xs" [(ngModel)]="attachDomainName" name="attachDomain">
                      <option value="">Attach a domain…</option>
                      @for (d of unownedDomains(); track d.name) {
                        <option [value]="d.name">{{ d.name }}</option>
                      }
                    </select>
                    <admin-button [disabled]="busy() || !attachDomainName" (pressed)="attachDomain(attachDomainName)">
                      Attach
                    </admin-button>
                  </div>
                }
              </div>
            </admin-panel>
          }

          @if (tab() === 'members') {
            <admin-panel title="Members" [bodyClass]="'p-0'">
              <div class="flex flex-wrap items-end gap-3 px-4 pt-4">
                <admin-search [query]="memberQuery" (search)="searchMembers($event)" placeholder="Search members…" />
                <span class="flex-1"></span>
                <label class="block">
                  <span class="mb-1 block text-xs font-medium text-slate-600">Add member</span>
                  <select class="{{ inputClass }} min-w-64" [(ngModel)]="newMember" name="newMember">
                    <option value="">Choose an address</option>
                    @for (address of userCandidates(); track address) {
                      <option [value]="address">{{ address }}</option>
                    }
                  </select>
                </label>
                <admin-button [disabled]="busy() || !newMember" (pressed)="addMember()">Add</admin-button>
              </div>
              <table class="mt-4 w-full">
                <thead class="border-b border-slate-200 bg-slate-50">
                  <tr>
                    <th class="{{ head }}">Address</th>
                    <th class="{{ head }}">Role</th>
                    <th class="{{ head }}"></th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-slate-100">
                  @for (m of members(); track m.address) {
                    <tr>
                      <td class="{{ cell }}">{{ m.address }}</td>
                      <td class="{{ cell }}">{{ m.role }}</td>
                      <td class="{{ cell }} text-right">
                        <admin-button variant="danger" [disabled]="busy()" (pressed)="removeMember(m)">Remove</admin-button>
                      </td>
                    </tr>
                  } @empty {
                    <tr><td class="{{ cell }} text-slate-500" colspan="3">No members match.</td></tr>
                  }
                </tbody>
              </table>
              <admin-list-footer [page]="memberPage" [total]="memberTotal()" [pageSize]="pageSize" (pageChange)="goMemberPage($event)" />
            </admin-panel>
          }

          @if (tab() === 'calendars') {
            <admin-panel title="Calendars" [bodyClass]="'p-0'">
              <div class="flex flex-wrap items-end gap-3 px-4 pt-4">
                <admin-search [query]="calendarQuery" (search)="searchCalendars($event)" placeholder="Search calendars…" />
                <span class="flex-1"></span>
                <label class="block">
                  <span class="mb-1 block text-xs font-medium text-slate-600">New calendar</span>
                  <input class="{{ inputClass }}" name="newCalendar" [(ngModel)]="newCalendar" placeholder="Name" />
                </label>
                <admin-button [disabled]="busy() || !newCalendar.trim()" (pressed)="createCalendar()">Create</admin-button>
              </div>
              <table class="mt-4 w-full">
                <thead class="border-b border-slate-200 bg-slate-50">
                  <tr>
                    <th class="{{ head }}">Name</th>
                    <th class="{{ head }}">Description</th>
                    <th class="{{ head }}"></th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-slate-100">
                  @for (cal of calendars(); track cal.id) {
                    <tr>
                      <td class="{{ cell }} font-medium">{{ cal.name }}</td>
                      <td class="{{ cell }} text-slate-500">{{ cal.description || '—' }}</td>
                      <td class="{{ cell }} whitespace-nowrap text-right">
                        <admin-button (pressed)="toggleCalendarShares(cal)">
                          {{ shareCalendarFor === cal.id ? 'Hide shares' : 'Share' }}
                        </admin-button>
                        <admin-button variant="danger" [disabled]="busy()" (pressed)="removeCalendar(cal)">Delete</admin-button>
                      </td>
                    </tr>
                    @if (shareCalendarFor === cal.id) {
                      <tr class="bg-slate-50/60">
                        <td colspan="3" class="px-4 py-3">
                          <div class="flex flex-wrap items-end gap-2">
                            <label class="block">
                              <span class="mb-1 block text-xs font-medium text-slate-600">Share with</span>
                              <select class="{{ inputClass }} min-w-64" [(ngModel)]="calendarShareSubject" name="calendarShareSubject">
                                <option value="">Choose a person or group</option>
                                @for (candidate of bookShareCandidates(); track candidate.value) {
                                  <option [value]="candidate.value">{{ candidate.label }}</option>
                                }
                              </select>
                            </label>
                            <label class="block">
                              <span class="mb-1 block text-xs font-medium text-slate-600">Access</span>
                              <select class="{{ inputClass }}" [(ngModel)]="calendarShareLevel" name="calendarShareLevel">
                                <option value="read">Read only</option>
                                <option value="write">Read and write</option>
                              </select>
                            </label>
                            <admin-button [disabled]="busy() || !calendarShareSubject" (pressed)="addCalendarShare(cal)">Share</admin-button>
                          </div>
                          <ul class="mt-3 divide-y divide-slate-100">
                            @for (share of calendarSharesFor(cal); track share.id) {
                              <li class="flex items-center justify-between py-2 text-sm">
                                <span>{{ calendarShareLabel(share) }} · {{ share.level }}</span>
                                <admin-button variant="danger" [disabled]="busy()" (pressed)="removeCalendarShare(cal, share)">Remove</admin-button>
                              </li>
                            } @empty {
                              <li class="py-2 text-sm text-slate-500">Not shared yet.</li>
                            }
                          </ul>
                        </td>
                      </tr>
                    }
                  } @empty {
                    <tr><td class="{{ cell }} text-slate-500" colspan="3">No calendars match.</td></tr>
                  }
                </tbody>
              </table>
              <admin-list-footer [page]="calendarPage" [total]="calendarTotal()" [pageSize]="pageSize" (pageChange)="goCalendarPage($event)" />
            </admin-panel>
          }

          @if (tab() === 'groups') {
            <admin-panel title="Groups" [bodyClass]="'p-0'">
              <div class="flex flex-wrap items-end gap-3 px-4 pt-4">
                <admin-search [query]="groupQuery" (search)="searchGroups($event)" placeholder="Search groups…" />
                <span class="flex-1"></span>
                <label class="block">
                  <span class="mb-1 block text-xs font-medium text-slate-600">New group</span>
                  <input class="{{ inputClass }}" name="newGroup" [(ngModel)]="newGroup" placeholder="Name" />
                </label>
                <admin-button [disabled]="busy() || !newGroup.trim()" (pressed)="createGroup()">Create</admin-button>
              </div>
              <table class="mt-4 w-full">
                <thead class="border-b border-slate-200 bg-slate-50">
                  <tr>
                    <th class="{{ head }}">Name</th>
                    <th class="{{ head }}">Members</th>
                    <th class="{{ head }}"></th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-slate-100">
                  @for (group of groups(); track group.id) {
                    <tr>
                      <td class="{{ cell }} font-medium">
                        {{ group.name }}
                        @if (group.everyone) { <span class="ml-2 text-xs text-slate-400">everyone</span> }
                      </td>
                      <td class="{{ cell }}">{{ group.member_count }}</td>
                      <td class="{{ cell }} text-right">
                        @if (!group.everyone) {
                          <admin-button variant="danger" [disabled]="busy()" (pressed)="removeGroup(group)">Delete</admin-button>
                        }
                      </td>
                    </tr>
                  } @empty {
                    <tr><td class="{{ cell }} text-slate-500" colspan="3">No groups match.</td></tr>
                  }
                </tbody>
              </table>
              <admin-list-footer [page]="groupPage" [total]="groupTotal()" [pageSize]="pageSize" (pageChange)="goGroupPage($event)" />
            </admin-panel>
          }

          @if (tab() === 'addressbooks') {
            <admin-panel title="Address books" [bodyClass]="'p-0'">
              <div class="flex flex-wrap items-end gap-3 px-4 pt-4">
                <admin-search [query]="bookQuery" (search)="searchBooks($event)" placeholder="Search address books…" />
                <span class="flex-1"></span>
                <label class="block">
                  <span class="mb-1 block text-xs font-medium text-slate-600">New shared book</span>
                  <input class="{{ inputClass }}" name="newBook" [(ngModel)]="newBook" placeholder="Suppliers" />
                </label>
                <admin-button [disabled]="busy() || !newBook.trim()" (pressed)="createBook()">Create</admin-button>
              </div>
              <table class="mt-4 w-full">
                <thead class="border-b border-slate-200 bg-slate-50">
                  <tr>
                    <th class="{{ head }}">Name</th>
                    <th class="{{ head }}">Description</th>
                    <th class="{{ head }}"></th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-slate-100">
                  @for (book of books(); track book.id) {
                    <tr>
                      <td class="{{ cell }} font-medium">{{ book.name }}</td>
                      <td class="{{ cell }} text-slate-500">{{ book.description || '—' }}</td>
                      <td class="{{ cell }} text-right">
                        <admin-button (pressed)="toggleBookShares(book)">
                          {{ shareBookFor === book.id ? 'Hide shares' : 'Share' }}
                        </admin-button>
                      </td>
                    </tr>
                    @if (shareBookFor === book.id) {
                      <tr class="bg-slate-50/60">
                        <td colspan="3" class="px-4 py-3">
                          <div class="flex flex-wrap items-end gap-2">
                            <label class="block">
                              <span class="mb-1 block text-xs font-medium text-slate-600">Share with</span>
                              <select class="{{ inputClass }} min-w-64" [(ngModel)]="bookShareSubject" name="bookShareSubject">
                                <option value="">Choose a person or group</option>
                                @for (c of bookShareCandidates(); track c.value) {
                                  <option [value]="c.value">{{ c.label }}</option>
                                }
                              </select>
                            </label>
                            <label class="block">
                              <span class="mb-1 block text-xs font-medium text-slate-600">Access</span>
                              <select class="{{ inputClass }}" [(ngModel)]="bookShareLevel" name="bookShareLevel">
                                <option value="read">Read only</option>
                                <option value="write">Read and write</option>
                              </select>
                            </label>
                            <admin-button [disabled]="busy() || !bookShareSubject" (pressed)="addBookShare(book)">Share</admin-button>
                          </div>
                          <ul class="mt-3 divide-y divide-slate-100">
                            @for (share of bookSharesFor(book); track share.id) {
                              <li class="flex items-center justify-between py-2 text-sm">
                                <span>{{ bookShareLabel(share) }} · {{ share.level }}</span>
                                <admin-button variant="danger" [disabled]="busy()" (pressed)="removeBookShare(book, share)">Remove</admin-button>
                              </li>
                            } @empty {
                              <li class="py-2 text-sm text-slate-500">Not shared yet.</li>
                            }
                          </ul>
                        </td>
                      </tr>
                    }
                  } @empty {
                    <tr><td class="{{ cell }} text-slate-500" colspan="3">No address books match.</td></tr>
                  }
                </tbody>
              </table>
              <admin-list-footer [page]="bookPage" [total]="bookTotal()" [pageSize]="pageSize" (pageChange)="goBookPage($event)" />
            </admin-panel>
          }
        }
      </div>
    </admin-page>
  `,
})
export class AdminWorkgroupManageComponent implements OnInit {
  private readonly api = inject(AdminApi);
  private readonly route = inject(ActivatedRoute);
  protected readonly auth = inject(AuthService);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;
  protected readonly pageSize = 20;
  protected readonly tabs: { key: Tab; label: string }[] = [
    { key: 'general', label: 'General' },
    { key: 'members', label: 'Members' },
    { key: 'calendars', label: 'Calendars' },
    { key: 'groups', label: 'Groups' },
    { key: 'addressbooks', label: 'Address books' },
  ];

  protected readonly wg = signal<Workgroup | null>(null);
  protected readonly tab = signal<Tab>('general');
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');
  protected readonly domains = signal<Domain[]>([]);
  protected readonly users = signal<string[]>([]);

  protected general = { name: '', displayName: '', description: '', openBooking: true, theme: '' };
  protected attachDomainName = '';

  protected readonly members = signal<WorkgroupMember[]>([]);
  protected readonly memberTotal = signal(0);
  protected memberQuery = '';
  protected memberPage = 1;
  protected newMember = '';

  protected readonly calendars = signal<OrgCalendar[]>([]);
  protected readonly calendarTotal = signal(0);
  protected calendarQuery = '';
  protected calendarPage = 1;
  protected newCalendar = '';
  protected readonly calendarShares = signal<Map<number, CalendarShare[]>>(new Map());
  protected shareCalendarFor = 0;
  protected calendarShareSubject = '';
  protected calendarShareLevel: 'read' | 'write' = 'read';

  protected readonly groups = signal<WorkgroupGroup[]>([]);
  protected readonly groupTotal = signal(0);
  protected groupQuery = '';
  protected groupPage = 1;
  protected newGroup = '';
  // allGroups is the complete picker list, not the current page of the groups
  // tab: sharing with a group must not depend on which page is visible.
  protected readonly allGroups = signal<WorkgroupGroup[]>([]);

  protected readonly books = signal<AddressBook[]>([]);
  protected readonly bookTotal = signal(0);
  protected bookQuery = '';
  protected bookPage = 1;
  protected newBook = '';
  protected readonly bookShares = signal<Map<number, AddressBookShare[]>>(new Map());
  protected shareBookFor = 0;
  protected bookShareSubject = '';
  protected bookShareLevel: 'read' | 'write' = 'read';

  private id = 0;

  async ngOnInit(): Promise<void> {
    this.id = Number(this.route.snapshot.paramMap.get('id'));
    await this.load();
  }

  private async load(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const [wg, domains, users] = await Promise.all([
        this.api.getWorkgroup(this.id),
        this.api.listDomains(),
        this.api.listUsers(),
      ]);
      this.wg.set(wg);
      this.domains.set(domains.domains ?? []);
      this.users.set((users.users ?? []).map((u) => u.address));
      this.general = {
        name: wg.name,
        displayName: wg.display_name,
        description: wg.description,
        openBooking: wg.open_booking,
        theme: wg.theme ?? '',
      };
      await Promise.all([this.loadMembers(), this.loadCalendars(), this.loadGroups(), this.loadBooks(), this.loadAllGroups()]);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  private async refreshWorkgroup(): Promise<void> {
    this.wg.set(await this.api.getWorkgroup(this.id));
  }

  protected ownedDomains(): Domain[] {
    return this.domains().filter((d) => d.workgroup_id === this.id);
  }

  protected unownedDomains(): Domain[] {
    return this.domains().filter((d) => d.workgroup_id !== this.id && d.name !== this.wg()?.domain);
  }

  protected async saveGeneral(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const updated = await this.api.updateWorkgroup(this.id, {
        name: this.general.name,
        display_name: this.general.displayName,
        description: this.general.description,
        open_booking: this.general.openBooking,
        theme: this.general.theme,
      });
      this.wg.set(updated);
      this.ok.set('Organization settings saved.');
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async uploadLogo(event: Event): Promise<void> {
    const file = (event.target as HTMLInputElement).files?.[0];
    if (!file) return;
    this.busy.set(true);
    try {
      this.wg.set(await this.api.uploadWorkgroupLogo(this.id, file));
      this.ok.set('Logo updated.');
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeLogo(): Promise<void> {
    this.busy.set(true);
    try {
      this.wg.set(await this.api.deleteWorkgroupLogo(this.id));
      this.ok.set('Logo removed.');
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async attachDomain(name: string): Promise<void> {
    if (!name) return;
    this.busy.set(true);
    try {
      await this.api.updateDomain(name, { workgroup_id: this.id });
      this.domains.set((await this.api.listDomains()).domains ?? []);
      this.attachDomainName = '';
      this.ok.set(`${name} now belongs to the organization.`);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async detachDomain(name: string): Promise<void> {
    this.busy.set(true);
    try {
      await this.api.updateDomain(name, { workgroup_id: null });
      this.domains.set((await this.api.listDomains()).domains ?? []);
      this.ok.set(`${name} detached.`);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected userCandidates(): string[] {
    const taken = new Set(this.members().map((m) => m.address.toLowerCase()));
    return this.users().filter((address) => !taken.has(address.toLowerCase()));
  }

  private async loadMembers(): Promise<void> {
    const res = await this.api.listWorkgroupMembers(this.id, {
      search: this.memberQuery || undefined,
      limit: this.pageSize,
      offset: (this.memberPage - 1) * this.pageSize,
    });
    this.members.set(res.members ?? []);
    this.memberTotal.set(res.total);
  }

  protected searchMembers(value: string): void {
    this.memberQuery = value;
    this.memberPage = 1;
    void this.loadMembers();
  }

  protected goMemberPage(page: number): void {
    this.memberPage = page;
    void this.loadMembers();
  }

  protected async addMember(): Promise<void> {
    if (!this.newMember) return;
    this.busy.set(true);
    try {
      await this.api.addWorkgroupMember(this.id, { address: this.newMember, role: 'member' });
      this.newMember = '';
      await this.loadMembers();
      await this.refreshWorkgroup();
      this.ok.set('Member added.');
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeMember(member: WorkgroupMember): Promise<void> {
    this.busy.set(true);
    try {
      await this.api.removeWorkgroupMember(this.id, member.address);
      await this.loadMembers();
      await this.refreshWorkgroup();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  private async loadCalendars(): Promise<void> {
    const res = await this.api.listWorkgroupCalendars(this.id, {
      search: this.calendarQuery || undefined,
      limit: this.pageSize,
      offset: (this.calendarPage - 1) * this.pageSize,
    });
    this.calendars.set(res.calendars ?? []);
    this.calendarTotal.set(res.total);
  }

  protected searchCalendars(value: string): void {
    this.calendarQuery = value;
    this.calendarPage = 1;
    void this.loadCalendars();
  }

  protected goCalendarPage(page: number): void {
    this.calendarPage = page;
    void this.loadCalendars();
  }

  protected async createCalendar(): Promise<void> {
    const name = this.newCalendar.trim();
    if (!name) return;
    this.busy.set(true);
    try {
      await this.api.createWorkgroupCalendar(this.id, { name });
      this.newCalendar = '';
      await this.loadCalendars();
      this.ok.set(`${name} created.`);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeCalendar(cal: OrgCalendar): Promise<void> {
    if (!confirm(`Delete ${cal.name}? Every event in it goes with it.`)) return;
    this.busy.set(true);
    try {
      await this.api.deleteWorkgroupCalendar(this.id, cal.id);
      await this.loadCalendars();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async toggleCalendarShares(cal: OrgCalendar): Promise<void> {
    this.error.set('');
    if (this.shareCalendarFor === cal.id) {
      this.shareCalendarFor = 0;
      return;
    }
    this.shareCalendarFor = cal.id;
    this.calendarShareSubject = '';
    try {
      const res = await this.api.listWorkgroupCalendarShares(this.id, cal.id);
      this.calendarShares.update((m) => new Map(m).set(cal.id, res.shares ?? []));
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected calendarSharesFor(cal: OrgCalendar): CalendarShare[] {
    return this.calendarShares().get(cal.id) ?? [];
  }

  protected async addCalendarShare(cal: OrgCalendar): Promise<void> {
    const separator = this.calendarShareSubject.indexOf(':');
    const kind = separator < 0 ? '' : this.calendarShareSubject.slice(0, separator);
    const value = separator < 0 ? '' : this.calendarShareSubject.slice(separator + 1);
    if (!value) {
      this.error.set('Choose a person or a group to share with.');
      return;
    }
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.shareWorkgroupCalendar(this.id, cal.id, {
        subject_kind: kind === 'group' ? 'group' : 'user',
        ...(kind === 'group'
          ? { subject_group_id: Number(value) }
          : { subject_address: value }),
        level: this.calendarShareLevel,
      });
      this.calendarShareSubject = '';
      const res = await this.api.listWorkgroupCalendarShares(this.id, cal.id);
      this.calendarShares.update((m) => new Map(m).set(cal.id, res.shares ?? []));
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeCalendarShare(cal: OrgCalendar, share: CalendarShare): Promise<void> {
    this.busy.set(true);
    try {
      await this.api.unshareWorkgroupCalendar(this.id, cal.id, share.id);
      const res = await this.api.listWorkgroupCalendarShares(this.id, cal.id);
      this.calendarShares.update((m) => new Map(m).set(cal.id, res.shares ?? []));
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected calendarShareLabel(share: CalendarShare): string {
    if (share.subject_name) return share.subject_name;
    return share.subject_kind === 'group'
      ? `group ${share.subject_group_id ?? ''}`
      : share.subject_address || '';
  }

  private async loadGroups(): Promise<void> {
    const res = await this.api.listWorkgroupGroups(this.id, {
      search: this.groupQuery || undefined,
      limit: this.pageSize,
      offset: (this.groupPage - 1) * this.pageSize,
    });
    this.groups.set(res.groups ?? []);
    this.groupTotal.set(res.total);
  }

  protected searchGroups(value: string): void {
    this.groupQuery = value;
    this.groupPage = 1;
    void this.loadGroups();
  }

  protected goGroupPage(page: number): void {
    this.groupPage = page;
    void this.loadGroups();
  }

  protected async createGroup(): Promise<void> {
    const name = this.newGroup.trim();
    if (!name) return;
    this.busy.set(true);
    try {
      await this.api.createWorkgroupGroup(this.id, { name });
      this.newGroup = '';
      await this.loadGroups();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeGroup(group: WorkgroupGroup): Promise<void> {
    if (!confirm(`Delete ${group.name}? Calendars shared with it lose the share too.`)) return;
    this.busy.set(true);
    try {
      await this.api.deleteWorkgroupGroup(this.id, group.id);
      await this.loadGroups();
      await this.loadAllGroups();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // --- organization address books ---

  private async loadAllGroups(): Promise<void> {
    const res = await this.api.listWorkgroupGroups(this.id, { limit: 100 });
    this.allGroups.set(res.groups ?? []);
  }

  private async loadBooks(): Promise<void> {
    const res = await this.api.listWorkgroupAddressBooks(this.id, {
      search: this.bookQuery || undefined,
      limit: this.pageSize,
      offset: (this.bookPage - 1) * this.pageSize,
    });
    this.books.set(res.books ?? []);
    this.bookTotal.set(res.total);
  }

  protected searchBooks(value: string): void {
    this.bookQuery = value;
    this.bookPage = 1;
    void this.loadBooks();
  }

  protected goBookPage(page: number): void {
    this.bookPage = page;
    void this.loadBooks();
  }

  protected async createBook(): Promise<void> {
    const name = this.newBook.trim();
    if (!name) return;
    this.busy.set(true);
    try {
      await this.api.createWorkgroupAddressBook(this.id, { name });
      this.newBook = '';
      await this.loadBooks();
      this.ok.set(`${name} created.`);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async toggleBookShares(book: AddressBook): Promise<void> {
    this.error.set('');
    if (this.shareBookFor === book.id) {
      this.shareBookFor = 0;
      return;
    }
    this.shareBookFor = book.id;
    this.bookShareSubject = '';
    try {
      const res = await this.api.listWorkgroupAddressBookShares(this.id, book.id);
      this.bookShares.update((m) => new Map(m).set(book.id, res.shares ?? []));
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected bookSharesFor(book: AddressBook): AddressBookShare[] {
    return this.bookShares().get(book.id) ?? [];
  }

  // bookShareCandidates labels a person or group with the value the request
  // needs, exactly as the calendar share picker does.
  protected bookShareCandidates(): { value: string; label: string }[] {
    const out: { value: string; label: string }[] = [];
    for (const address of this.users()) {
      out.push({ value: `user:${address}`, label: address });
    }
    for (const group of this.allGroups()) {
      out.push({
        value: `group:${group.id}`,
        label: group.everyone ? group.name : `${group.name} · ${group.member_count}`,
      });
    }
    return out;
  }

  protected async addBookShare(book: AddressBook): Promise<void> {
    const separator = this.bookShareSubject.indexOf(':');
    const kind = separator < 0 ? '' : this.bookShareSubject.slice(0, separator);
    const value = separator < 0 ? '' : this.bookShareSubject.slice(separator + 1);
    if (!value) {
      this.error.set('Choose a person or a group to share with.');
      return;
    }
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.shareWorkgroupAddressBook(this.id, book.id, {
        subject_kind: kind === 'group' ? 'group' : 'user',
        ...(kind === 'group'
          ? { subject_group_id: Number(value) }
          : { subject_address: value }),
        level: this.bookShareLevel,
      });
      this.bookShareSubject = '';
      const res = await this.api.listWorkgroupAddressBookShares(this.id, book.id);
      this.bookShares.update((m) => new Map(m).set(book.id, res.shares ?? []));
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async removeBookShare(book: AddressBook, share: AddressBookShare): Promise<void> {
    this.busy.set(true);
    try {
      await this.api.unshareWorkgroupAddressBook(this.id, book.id, share.id);
      const res = await this.api.listWorkgroupAddressBookShares(this.id, book.id);
      this.bookShares.update((m) => new Map(m).set(book.id, res.shares ?? []));
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected bookShareLabel(share: AddressBookShare): string {
    return share.subjectKind === 'group'
      ? `group ${share.subjectGroupId ?? ''}`
      : share.subjectAddress || '';
  }
}

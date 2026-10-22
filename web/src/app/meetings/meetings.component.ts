import { NgClass } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { ContactsApi } from '../core/contacts/contacts-api';
import { Suggestion } from '../core/contacts/contacts.models';
import { errorMessage } from '../core/http-error';
import { MeetingsApi } from '../core/meetings/meetings-api';
import { MeetingDraft, MeetingListItem } from '../core/meetings/meetings.models';
import { formatWhen, partStatLabel } from '../core/meetings/meetings.ui';
import { OrganizationApi, OrganizationGroup } from '../core/organization/organization-api';

// browserZone is the zone this browser displays times in; it becomes the
// meeting's display timezone unless the organiser changes it.
function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch {
    return 'UTC';
  }
}

// Default form values start the meeting tomorrow at ten, in the browser's own
// zone - the shape of a booking somebody is about to type over anyway, and
// one that is always in the future rather than today at a time already past.
function defaults() {
  const d = new Date(Date.now() + 24 * 3600 * 1000);
  const date = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
  return { date, start: '10:00', end: '11:00' };
}

@Component({
  selector: 'app-meetings',
  standalone: true,
  imports: [NgClass, FormsModule, RouterLink],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <main class="mx-auto max-w-4xl px-6 py-10">
      <header class="flex items-center justify-between">
        <div>
          <h1 class="text-3xl font-semibold tracking-tight text-slate-900" i18n="@@meetings.title">Meetings</h1>
          <p class="mt-1 text-sm text-slate-500" i18n="@@meetings.intro">
            A meeting is a calendar event with an invitation: every guest is mailed a link that
            works whether or not they have an account here.
          </p>
        </div>
        <button
          type="button"
          (click)="creating.update((v) => !v)"
          class="rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800 transition-colors"
        >
          @if (creating()) {
            <span i18n="@@meetings.close">Close</span>
          } @else {
            <span i18n="@@meetings.new">New meeting</span>
          }
        </button>
      </header>

      @if (creating()) {
        <section class="mt-6 rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
          <h2 class="text-lg font-semibold text-slate-900" i18n="@@meetings.newTitle">New meeting</h2>

          <div class="mt-4 grid gap-4 sm:grid-cols-2">
            <label class="block sm:col-span-2">
              <span class="text-sm font-medium text-slate-700" i18n="@@meetings.titleLabel">Title</span>
              <input
                type="text"
                [(ngModel)]="title"
                name="title"
                placeholder="Design review"
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              />
            </label>

            <label class="block">
              <span class="text-sm font-medium text-slate-700" i18n="@@meetings.date">Date</span>
              <input
                type="date"
                [(ngModel)]="date"
                name="date"
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              />
            </label>
            <div class="grid grid-cols-2 gap-3">
              <label class="block">
                <span class="text-sm font-medium text-slate-700" i18n="@@meetings.from">From</span>
                <input
                  type="time"
                  [(ngModel)]="startTime"
                  name="startTime"
                  class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                />
              </label>
              <label class="block">
                <span class="text-sm font-medium text-slate-700" i18n="@@meetings.to">To</span>
                <input
                  type="time"
                  [(ngModel)]="endTime"
                  name="endTime"
                  class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                />
              </label>
            </div>

            <label class="block sm:col-span-2">
              <span class="text-sm font-medium text-slate-700">
                Guests <span class="font-normal text-slate-400">— one per line, "Bob &lt;bob&#64;example.com&gt;" or just the address</span>
              </span>
              <textarea
                [(ngModel)]="attendeesText"
                name="attendees"
                rows="3"
                placeholder="bob@example.com&#10;carol@outside.example"
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm font-mono focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              ></textarea>
            </label>

            <!-- The same search the composer uses, with the book each
                 candidate came from, so an invitee from a personal address
                 book is visibly different from one in the company directory. -->
            <div class="relative sm:col-span-2">
              <span class="text-sm font-medium text-slate-700">
                Add from contacts
                <span class="font-normal text-slate-400">— search your books and the organization directory</span>
              </span>
              <input
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                name="contactQuery"
                autocomplete="off"
                [(ngModel)]="contactQuery"
                (ngModelChange)="searchContacts()"
                placeholder="Name, address, phone or organization"
              />
              @if (suggestions().length > 0) {
                <ul
                  class="absolute z-10 mt-1 max-h-64 w-full overflow-auto rounded-lg border border-slate-200 bg-white shadow-lg"
                >
                  @for (s of suggestions(); track s.address) {
                    <li>
                      <button
                        type="button"
                        class="flex w-full items-center gap-2 px-3 py-2 text-left text-sm hover:bg-slate-50"
                        (click)="addSuggestion(s)"
                      >
                        <span class="min-w-0 flex-1">
                          <span class="block truncate font-medium text-slate-800">
                            {{ s.displayName || s.address }}
                          </span>
                          <span class="block truncate text-xs text-slate-500">{{ s.address }}</span>
                        </span>
                        <span
                          class="shrink-0 rounded bg-brand-50 px-1.5 py-0.5 text-[10px] font-medium text-brand-700"
                        >
                          {{ s.source }}
                        </span>
                      </button>
                    </li>
                  }
                </ul>
              }
            </div>

            <!-- Groups invite people who have not been written down: the
                 server expands each one into guests when the meeting is
                 created, so a colleague who joins on Tuesday is in Wednesday's
                 meeting without this list being rewritten. -->
            <div class="sm:col-span-2">
              <span class="text-sm font-medium text-slate-700">
                Groups <span class="font-normal text-slate-400">— invited whole, at whatever they hold when the invitation goes out</span>
              </span>
              @if (orgGroups().length > 0) {
                <div class="mt-1 flex flex-wrap gap-2">
                  @for (g of orgGroups(); track g.id) {
                    <label
                      class="inline-flex cursor-pointer items-center gap-2 rounded-lg border px-3 py-1.5 text-sm transition-colors"
                      [ngClass]="
                        invitedGroups().includes(g.id)
                          ? 'border-brand-500 bg-brand-50 text-brand-800'
                          : 'border-slate-300 text-slate-700 hover:bg-slate-50'
                      "
                    >
                      <input
                        type="checkbox"
                        class="h-4 w-4 rounded border-slate-300 text-brand-600 focus:ring-brand-500"
                        [checked]="invitedGroups().includes(g.id)"
                        (change)="toggleGroup(g)"
                      />
                      {{ g.name }}
                      <span class="text-xs" [ngClass]="invitedGroups().includes(g.id) ? 'text-brand-600' : 'text-slate-400'">
                        {{ g.everyone ? 'whole organization' : g.member_count + (g.member_count === 1 ? ' person' : ' people') }}
                      </span>
                    </label>
                  }
                </div>
              } @else {
                <p class="mt-1 text-sm text-slate-400">
                  Your organization has no groups — invite people by address above.
                </p>
              }
            </div>

            <!-- A webinar is chosen here, with the invitation, so every guest
                 knows before they join that only the organiser will speak. -->
            <label class="flex items-start gap-3 rounded-lg border border-slate-200 p-3 sm:col-span-2">
              <input
                type="checkbox"
                [(ngModel)]="oneWay"
                name="oneWay"
                class="mt-0.5 h-4 w-4 rounded border-slate-300 text-brand-600 focus:ring-brand-500"
              />
              <span>
                <span class="text-sm font-medium text-slate-700">One-way conference (webinar)</span>
                <span class="block text-xs text-slate-400">
                  Only you speak. Guests stay muted and can raise a hand to ask for the floor.
                </span>
              </span>
            </label>

            <label class="block">
              <span class="text-sm font-medium text-slate-700">Location <span class="font-normal text-slate-400">(optional; the join link fills in)</span></span>
              <input
                type="text"
                [(ngModel)]="location"
                name="location"
                placeholder="Room 3 or a link"
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              />
            </label>
            <label class="block">
              <span class="text-sm font-medium text-slate-700">Shown in</span>
              <input
                type="text"
                [(ngModel)]="timezone"
                name="timezone"
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              />
            </label>

            <label class="block sm:col-span-2">
              <span class="text-sm font-medium text-slate-700">Description <span class="font-normal text-slate-400">(optional)</span></span>
              <textarea
                [(ngModel)]="description"
                name="description"
                rows="2"
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              ></textarea>
            </label>
          </div>

          @if (formError) {
            <p class="mt-3 text-sm text-rose-600">{{ formError }}</p>
          }

          <div class="mt-4 flex items-center gap-3">
            <button
              type="button"
              [disabled]="saving()"
              (click)="create()"
              class="rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800 disabled:opacity-50 transition-colors"
            >
              @if (saving()) {
                <span i18n="@@meetings.inviting">Inviting…</span>
              } @else {
                <span i18n="@@meetings.create">Create and invite</span>
              }
            </button>
            <span class="text-sm text-slate-500" i18n="@@meetings.inviteHint">Invitations go out immediately, one per guest.</span>
          </div>
        </section>
      }

      @if (loadError) {
        <p class="mt-6 text-sm text-rose-600">{{ loadError }}</p>
      }

      <section class="mt-8">
        @if (loading()) {
          <p class="text-sm text-slate-500">Loading…</p>
        } @else if (items().length === 0) {
          <div class="rounded-2xl bg-white p-8 text-center shadow-sm ring-1 ring-slate-200">
            <p class="text-sm text-slate-500">No meetings yet. Create one and the guests are mailed their links.</p>
          </div>
        } @else {
          <ul class="space-y-3">
            @for (m of items(); track m.uid) {
              <li>
                <a
                  [routerLink]="['/meetings', m.uid]"
                  class="block rounded-2xl bg-white p-5 shadow-sm ring-1 ring-slate-200 hover:ring-brand-400 transition-colors"
                >
                  <div class="flex items-start justify-between gap-4">
                    <div class="min-w-0">
                      <div class="flex items-center gap-2">
                        <span class="truncate font-medium text-slate-900">{{ m.title }}</span>
                        @if (m.cancelled) {
                          <span class="rounded bg-rose-100 px-2 py-0.5 text-xs font-medium text-rose-700">Cancelled</span>
                        }
                        <span
                          class="rounded px-2 py-0.5 text-xs font-medium"
                          [ngClass]="m.role === 'organizer' ? 'bg-brand-100 text-brand-800' : 'bg-slate-100 text-slate-600'"
                        >
                          {{ m.role === 'organizer' ? 'Organiser' : 'Guest' }}
                        </span>
                      </div>
                      <p class="mt-1 text-sm text-slate-600">{{ when(m) }}</p>
                      @if (m.location) {
                        <p class="mt-0.5 text-sm text-slate-400">{{ m.location }}</p>
                      }
                    </div>
                    <div class="shrink-0 text-right text-sm text-slate-500">
                      <div>{{ m.acceptedCount }} of {{ m.attendeeCount }} accepted</div>
                      @if (m.myPartStat && m.myPartStat !== 'NEEDS-ACTION') {
                        <div class="mt-0.5 text-slate-400">You: {{ answer(m.myPartStat) }}</div>
                      }
                    </div>
                  </div>
                </a>
              </li>
            }
          </ul>
        )}
      </section>
    </main>
  `,
})
export class MeetingsComponent {
  private readonly api = inject(MeetingsApi);
  private readonly router = inject(Router);
  private readonly orgApi = inject(OrganizationApi);
  private readonly contactsApi = inject(ContactsApi);

  readonly items = signal<MeetingListItem[]>([]);
  readonly loading = signal(true);
  readonly saving = signal(false);
  readonly creating = signal(false);

  // The organization's groups: the other way to name guests, fetched with the
  // page because the list is short and the form is the only place it is used.
  // Somebody whose domain belongs to no organization simply sees none.
  readonly orgGroups = signal<OrganizationGroup[]>([]);
  readonly invitedGroups = signal<number[]>([]);

  loadError = '';
  formError = '';

  // The create form. Model fields are plain properties: they are typed into,
  // not rendered as signals - the template's ngModel pair already keeps them
  // current, and a signal per keystroke would only add machinery.
  title = '';
  description = '';
  location = '';
  attendeesText = '';
  contactQuery = '';
  readonly suggestions = signal<Suggestion[]>([]);
  private searchTimer?: ReturnType<typeof setTimeout>;
  oneWay = false;
  timezone = browserZone();
  date = defaults().date;
  startTime = defaults().start;
  endTime = defaults().end;

  constructor() {
    this.load();
    void this.orgApi
      .groups()
      .then((res) => this.orgGroups.set(res.groups ?? []))
      // A failure here only costs the group picker: the address list above it
      // still invites everybody, so there is nothing to report.
      .catch(() => this.orgGroups.set([]));
  }

  async load(): Promise<void> {
    this.loading.set(true);
    this.loadError = '';
    try {
      const res = await this.api.list();
      this.items.set(res.meetings);
    } catch (err) {
      this.loadError = errorMessage(err);
    } finally {
      this.loading.set(false);
    }
  }

  when(m: MeetingListItem): string {
    return formatWhen(m.start, m.end, m.timezone);
  }

  answer(partStat: string): string {
    return partStatLabel(partStat);
  }

  async create(): Promise<void> {
    this.formError = '';
    const title = this.title.trim();
    if (!title) {
      this.formError = 'A meeting needs a title.';
      return;
    }
    if (!this.date || !this.startTime || !this.endTime) {
      this.formError = 'A meeting needs a date and a time.';
      return;
    }
    const attendees = this.parseAttendees();
    const groups = this.invitedGroups();
    if (attendees.length === 0 && groups.length === 0) {
      this.formError = 'Add at least one guest, or invite a group.';
      return;
    }

    const start = new Date(`${this.date}T${this.startTime}:00`);
    const end = new Date(`${this.date}T${this.endTime}:00`);
    if (Number.isNaN(start.getTime()) || Number.isNaN(end.getTime())) {
      this.formError = 'That date or time could not be read.';
      return;
    }
    // An end before the start means the meeting runs past midnight: the next
    // day is what was meant, and rejecting it would only make the organiser
    // retype a meeting they had already described correctly.
    const endAdjusted = end.getTime() <= start.getTime() ? new Date(end.getTime() + 24 * 3600 * 1000) : end;

    const draft: MeetingDraft = {
      title,
      description: this.description.trim() || undefined,
      location: this.location.trim() || undefined,
      start: start.toISOString(),
      end: endAdjusted.toISOString(),
      timezone: this.timezone.trim() || undefined,
      attendees,
      // The groups go as ids and become people on the server, so what the
      // invitation carries is the group's membership when it was sent rather
      // than when the form was filled in.
      groups: groups.length > 0 ? groups : undefined,
      oneWay: this.oneWay || undefined,
    };

    this.saving.set(true);
    try {
      const result = await this.api.create(draft);
      this.creating.set(false);
      if (result.inviteFailures?.length) {
        // The meeting exists and the other guests were mailed: this is a
        // result the organiser must see, not an error that hides the success.
        this.loadError = `Meeting created, but invitations failed for: ${result.inviteFailures.join(', ')}`;
        await this.load();
        return;
      }
      await this.router.navigate(['/meetings', result.uid]);
    } catch (err) {
      this.formError = errorMessage(err);
    } finally {
      this.saving.set(false);
    }
  }

  // toggleGroup adds or drops a whole group from the invitation. It sets a new
  // array rather than mutating the old one because the checkbox reads the
  // signal on every change detection pass.
  toggleGroup(group: OrganizationGroup): void {
    const chosen = this.invitedGroups();
    this.invitedGroups.set(
      chosen.includes(group.id) ? chosen.filter((id) => id !== group.id) : [...chosen, group.id],
    );
  }

  protected searchContacts(): void {
    const query = this.contactQuery.trim();
    if (this.searchTimer) {
      clearTimeout(this.searchTimer);
    }
    if (!query) {
      this.suggestions.set([]);
      return;
    }
    this.searchTimer = setTimeout(() => {
      void this.contactsApi
        .search(query, 8)
        .then((res) => this.suggestions.set(res.suggestions ?? []))
        .catch(() => this.suggestions.set([]));
    }, 180);
  }

  protected addSuggestion(suggestion: Suggestion): void {
    const label = suggestion.displayName
      ? `${suggestion.displayName} <${suggestion.address}>`
      : suggestion.address;
    const lines = this.attendeesText
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean);
    if (!lines.some((line) => line.toLowerCase().includes(suggestion.address.toLowerCase()))) {
      this.attendeesText = [...lines, label].join('\n');
    }
    this.contactQuery = '';
    this.suggestions.set([]);
  }

  // parseAttendees reads the textarea: one guest per line, with the
  // "Display name <address>" spelling mail clients use accepted as-is.
  private parseAttendees(): { address: string; name?: string }[] {
    const out: { address: string; name?: string }[] = [];
    const seen = new Set<string>();
    for (const line of this.attendeesText.split('\n')) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      const angled = /^(.*?)\s*<([^>]+)>$/.exec(trimmed);
      const address = (angled ? angled[2] : trimmed).trim().toLowerCase();
      const name = angled ? angled[1].trim() : '';
      if (!address || seen.has(address)) continue;
      seen.add(address);
      out.push(name ? { address, name } : { address });
    }
    return out;
  }
}

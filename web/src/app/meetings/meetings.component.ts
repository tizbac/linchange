import { NgClass } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { errorMessage } from '../core/http-error';
import { MeetingsApi } from '../core/meetings/meetings-api';
import { MeetingDraft, MeetingListItem } from '../core/meetings/meetings.models';
import { formatWhen, partStatLabel } from '../core/meetings/meetings.ui';

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
          <h1 class="text-3xl font-semibold tracking-tight text-slate-900">Meetings</h1>
          <p class="mt-1 text-sm text-slate-500">
            A meeting is a calendar event with an invitation: every guest is mailed a link that
            works whether or not they have an account here.
          </p>
        </div>
        <button
          type="button"
          (click)="creating.update((v) => !v)"
          class="rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800 transition-colors"
        >
          {{ creating() ? 'Close' : 'New meeting' }}
        </button>
      </header>

      @if (creating()) {
        <section class="mt-6 rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
          <h2 class="text-lg font-semibold text-slate-900">New meeting</h2>

          <div class="mt-4 grid gap-4 sm:grid-cols-2">
            <label class="block sm:col-span-2">
              <span class="text-sm font-medium text-slate-700">Title</span>
              <input
                type="text"
                [(ngModel)]="title"
                name="title"
                placeholder="Design review"
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              />
            </label>

            <label class="block">
              <span class="text-sm font-medium text-slate-700">Date</span>
              <input
                type="date"
                [(ngModel)]="date"
                name="date"
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              />
            </label>
            <div class="grid grid-cols-2 gap-3">
              <label class="block">
                <span class="text-sm font-medium text-slate-700">From</span>
                <input
                  type="time"
                  [(ngModel)]="startTime"
                  name="startTime"
                  class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                />
              </label>
              <label class="block">
                <span class="text-sm font-medium text-slate-700">To</span>
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
              {{ saving() ? 'Inviting…' : 'Create and invite' }}
            </button>
            <span class="text-sm text-slate-500">Invitations go out immediately, one per guest.</span>
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

  readonly items = signal<MeetingListItem[]>([]);
  readonly loading = signal(true);
  readonly saving = signal(false);
  readonly creating = signal(false);

  loadError = '';
  formError = '';

  // The create form. Model fields are plain properties: they are typed into,
  // not rendered as signals - the template's ngModel pair already keeps them
  // current, and a signal per keystroke would only add machinery.
  title = '';
  description = '';
  location = '';
  attendeesText = '';
  timezone = browserZone();
  date = defaults().date;
  startTime = defaults().start;
  endTime = defaults().end;

  constructor() {
    this.load();
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
    if (attendees.length === 0) {
      this.formError = 'Add at least one guest.';
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

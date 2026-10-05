import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  signal,
  viewChild,
  ViewEncapsulation,
  AfterViewInit,
  OnDestroy,
  ElementRef,
} from '@angular/core';
import { FullCalendarModule, FullCalendarComponent } from '@fullcalendar/angular';
import {
  CalendarOptions,
  EventClickArg,
  EventDropArg,
  DateSelectArg,
  EventApi,
  Calendar,
  DatesSetArg,
} from '@fullcalendar/core';
import { EventResizeDoneArg } from '@fullcalendar/interaction';
import dayGridPlugin from '@fullcalendar/daygrid';
import timeGridPlugin from '@fullcalendar/timegrid';
import listPlugin from '@fullcalendar/list';
import interactionPlugin from '@fullcalendar/interaction';
import rrulePlugin from '@fullcalendar/rrule';
import { CalendarApi } from '../core/calendar/calendar-api';
import {
  Calendar as CalendarModel,
  Event as EventModel,
  ListEventsResponse,
  CreateEventRequest,
  UpdateEventRequest,
  FreeBusyRequest,
  FreeBusyResponse,
  Availability,
  ICalEvent,
} from '../core/calendar/calendar.models';
import { formatDateTime, formatDate, startOfDay, endOfDay, toRFC3339, parseRFC3339 } from '../core/calendar/ui';
import { NgIf, NgFor, NgClass } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { CalIcon } from '../core/calendar/ui';

@Component({
  selector: 'app-calendar',
  standalone: true,
  imports: [FullCalendarModule, NgIf, NgFor, NgClass, FormsModule, CalIcon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  encapsulation: ViewEncapsulation.None,
  template: `
    <div class="h-full flex flex-col">
      <!-- Toolbar -->
      <div class="flex items-center justify-between p-4 border-b bg-white dark:bg-gray-900">
        <div class="flex items-center gap-4">
          <button
            type="button"
            class="p-2 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 transition-colors"
            (click)="prev()"
            aria-label="Previous"
          >
            <app-cal-icon name="chevron_left" [size]="20" />
          </button>
          <button
            type="button"
            class="p-2 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 transition-colors"
            (click)="next()"
            aria-label="Next"
          >
            <app-cal-icon name="chevron_right" [size]="20" />
          </button>
          <button
            type="button"
            class="p-2 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 transition-colors"
            (click)="today()"
            aria-label="Today"
          >
            <app-cal-icon name="today" [size]="20" />
          </button>
          <h1 class="text-lg font-semibold">{{ title() }}</h1>
        </div>
        <div class="flex items-center gap-2">
          <select
            [ngModel]="view()"
            (ngModelChange)="changeView($event)"
            class="px-3 py-1.5 text-sm border rounded-lg bg-white dark:bg-gray-800 focus:ring-2 focus:ring-blue-500"
            aria-label="Calendar view"
          >
            <option value="dayGridMonth">Month</option>
            <option value="timeGridWeek">Week</option>
            <option value="timeGridDay">Day</option>
            <option value="listWeek">List</option>
          </select>
          <button
            type="button"
            class="px-4 py-1.5 text-sm font-medium text-white bg-blue-600 rounded-lg hover:bg-blue-700 transition-colors"
            (click)="openCreateEvent()"
          >
            <app-cal-icon name="plus" [size]="16" class="mr-1 inline" />
            New Event
          </button>
        </div>
      </div>

      <!-- Calendar -->
      <div class="flex-1 overflow-hidden relative">
        <full-calendar
          #fullCalendar
          [options]="calendarOptions"
          class="h-full"
        />
      </div>

      <!-- Event Detail Modal -->
      @if (selectedEvent()) {
        <div class="fixed inset-0 z-50 flex items-center justify-center p-4 bg-black/50" (click)="closeEventDetail()">
          <div
            class="bg-white dark:bg-gray-900 rounded-xl shadow-xl max-w-md w-full max-h-[90vh] overflow-y-auto"
            (click)="$event.stopPropagation()"
          >
            <div class="p-4 border-b flex items-start justify-between">
              <h2 class="text-lg font-semibold">{{ selectedEvent()?.extendedProps?.['summary'] || 'Event' }}</h2>
              <button
                type="button"
                class="p-1 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 transition-colors"
                (click)="closeEventDetail()"
                aria-label="Close"
              >
                <app-cal-icon name="x" [size]="20" />
              </button>
            </div>
            <div class="p-4 space-y-3">
              <div>
                <label class="text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Time</label>
                <p class="text-sm">
                  @if (selectedEvent()?.allDay) {
                    All day
                    @if (selectedEvent()?.end) {
                      <span class="text-gray-500"> · </span>
                      {{ fmtDate(selectedEvent()!.end) }}
                    }
                  } @else {
                    {{ fmtTime(selectedEvent()!.start) }} — {{ fmtTime(selectedEvent()!.end) }}
                  }
                </p>
              </div>
              @if (selectedEvent()?.extendedProps?.['location']) {
                <div>
                  <label class="text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Location</label>
                  <p class="text-sm">{{ selectedEvent()!.extendedProps!['location'] }}</p>
                </div>
              }
              @if (selectedEvent()?.extendedProps?.['attendees']?.length) {
                <div>
                  <label class="text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Attendees</label>
                  <ul class="text-sm space-y-1">
                    @for (a of selectedEvent()!.extendedProps!['attendees']; track a.address) {
                      <li class="flex items-center gap-2">
                        <span
                          class="w-2 h-2 rounded-full"
                          [ngClass]="{
                            'bg-green-500': a.partStat === 'ACCEPTED',
                            'bg-yellow-500': a.partStat === 'TENTATIVE',
                            'bg-red-500': a.partStat === 'DECLINED',
                            'bg-gray-400': a.partStat === 'NEEDS-ACTION'
                          }"
                        ></span>
                        {{ a.name || a.address }}
                        @if (a.partStat !== 'ACCEPTED') {
                          <span class="text-xs text-gray-500 capitalize">{{ a.partStat?.toLowerCase().replace('-', ' ') }}</span>
                        }
                      </li>
                    }
                  </ul>
                </div>
              }
              <div class="flex gap-2 pt-2 border-t">
                <button
                  type="button"
                  class="flex-1 px-3 py-2 text-sm font-medium text-gray-700 dark:text-gray-300 bg-gray-100 dark:bg-gray-800 rounded-lg hover:bg-gray-200 dark:hover:bg-gray-700 transition-colors"
                  (click)="editEvent(selectedEvent()!)"
                >
                  Edit
                </button>
                <button
                  type="button"
                  class="flex-1 px-3 py-2 text-sm font-medium text-red-600 bg-red-50 dark:bg-red-900/20 rounded-lg hover:bg-red-100 dark:hover:bg-red-900/30 transition-colors"
                  (click)="deleteEvent(selectedEvent()!)"
                >
                  Delete
                </button>
              </div>
            </div>
          </div>
        </div>
      }

      <!-- Create/Edit Event Modal -->
      @if (editingEvent()) {
        <div class="fixed inset-0 z-50 flex items-center justify-center p-4 bg-black/50" (click)="closeEditModal()">
          <div
            class="bg-white dark:bg-gray-900 rounded-xl shadow-xl max-w-md w-full max-h-[90vh] overflow-y-auto"
            (click)="$event.stopPropagation()"
          >
            <div class="p-4 border-b flex items-center justify-between">
              <h2 class="text-lg font-semibold">{{ editingEvent()?.eventId ? 'Edit Event' : 'New Event' }}</h2>
              <button
                type="button"
                class="p-1 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 transition-colors"
                (click)="closeEditModal()"
                aria-label="Close"
              >
                <app-cal-icon name="x" [size]="20" />
              </button>
            </div>
            <form (ngSubmit)="saveEvent()" class="p-4 space-y-4">
              <div>
                <label class="block text-sm font-medium mb-1">Title</label>
                <input
                  type="text"
                  [(ngModel)]="editForm.title"
                  name="title"
                  required
                  class="w-full px-3 py-2 border rounded-lg bg-white dark:bg-gray-800 focus:ring-2 focus:ring-blue-500"
                  placeholder="Event title"
                />
              </div>
              <div>
                <label class="block text-sm font-medium mb-1">Calendar</label>
                <select
                  [(ngModel)]="editForm.calendarId"
                  name="calendarId"
                  class="w-full px-3 py-2 border rounded-lg bg-white dark:bg-gray-800 focus:ring-2 focus:ring-blue-500"
                >
                  @for (cal of calendars(); track cal.id) {
                    <option [value]="cal.id">{{ cal.display_name || cal.name }}</option>
                  }
                </select>
              </div>
              <div class="grid grid-cols-2 gap-3">
                <div>
                  <label class="block text-sm font-medium mb-1">Start</label>
                  <input
                    type="datetime-local"
                    [(ngModel)]="editForm.start"
                    name="start"
                    required
                    class="w-full px-3 py-2 border rounded-lg bg-white dark:bg-gray-800 focus:ring-2 focus:ring-blue-500"
                  />
                </div>
                <div>
                  <label class="block text-sm font-medium mb-1">End</label>
                  <input
                    type="datetime-local"
                    [(ngModel)]="editForm.end"
                    name="end"
                    required
                    class="w-full px-3 py-2 border rounded-lg bg-white dark:bg-gray-800 focus:ring-2 focus:ring-blue-500"
                  />
                </div>
              </div>
              <div class="flex items-center gap-2">
                <input
                  type="checkbox"
                  [(ngModel)]="editForm.allDay"
                  name="allDay"
                  id="allDay"
                  class="w-4 h-4 rounded border-gray-300 text-blue-600 focus:ring-blue-500"
                />
                <label for="allDay" class="text-sm">All day</label>
              </div>
              <div>
                <label class="block text-sm font-medium mb-1">Location</label>
                <input
                  type="text"
                  [(ngModel)]="editForm.location"
                  name="location"
                  class="w-full px-3 py-2 border rounded-lg bg-white dark:bg-gray-800 focus:ring-2 focus:ring-blue-500"
                  placeholder="Location (optional)"
                />
              </div>
              <div>
                <label class="block text-sm font-medium mb-1">Timezone</label>
                <input
                  type="text"
                  [(ngModel)]="editForm.timezone"
                  name="timezone"
                  class="w-full px-3 py-2 border rounded-lg bg-white dark:bg-gray-800 focus:ring-2 focus:ring-blue-500"
                  placeholder="Europe/London"
                />
              </div>
              <div class="flex gap-2 pt-2 border-t">
                <button
                  type="submit"
                  class="flex-1 px-3 py-2 text-sm font-medium text-white bg-blue-600 rounded-lg hover:bg-blue-700 transition-colors"
                  [disabled]="saving()"
                >
                  {{ saving() ? 'Saving...' : (editingEvent()?.eventId ? 'Save' : 'Create') }}
                </button>
                <button
                  type="button"
                  class="flex-1 px-3 py-2 text-sm font-medium text-gray-700 dark:text-gray-300 bg-gray-100 dark:bg-gray-800 rounded-lg hover:bg-gray-200 dark:hover:bg-gray-700 transition-colors"
                  (click)="closeEditModal()"
                >
                  Cancel
                </button>
              </div>
            </form>
          </div>
        </div>
      }
    </div>
  `,
  styles: [`
    :host {
      display: block;
      height: 100%;
    }
    .fc {
      --fc-border-color: #e5e7eb;
      --fc-button-bg-color: #2563eb;
      --fc-button-border-color: #2563eb;
      --fc-button-hover-bg-color: #1d4ed8;
      --fc-button-hover-border-color: #1d4ed8;
      --fc-button-active-bg-color: #1e40af;
      --fc-button-active-border-color: #1e40af;
      --fc-today-bg-color: #dbeafe;
      --fc-event-bg-color: #2563eb;
      --fc-event-border-color: #2563eb;
      --fc-popover-bg: white;
      --fc-popover-border-color: #e5e7eb;
    }
    .dark .fc {
      --fc-border-color: #374151;
      --fc-button-bg-color: #3b82f6;
      --fc-button-border-color: #3b82f6;
      --fc-button-hover-bg-color: #2563eb;
      --fc-button-hover-border-color: #2563eb;
      --fc-button-active-bg-color: #1d4ed8;
      --fc-button-active-border-color: #1d4ed8;
      --fc-today-bg-color: #1e3a5f;
      --fc-event-bg-color: #3b82f6;
      --fc-event-border-color: #3b82f6;
      --fc-popover-bg: #1f2937;
      --fc-popover-border-color: #374151;
    }
  `]
})
export class CalendarComponent implements AfterViewInit, OnDestroy {
  private readonly api = inject(CalendarApi);
  // The FullCalendar component instance, not the DOM element. The wrapper is
  // what holds the live CalendarApi, so typing this as ElementRef<HTMLElement>
  // would hand back a div with no view on it.
  private readonly calendarRef = viewChild.required<FullCalendarComponent>('fullCalendar');

  // State
  readonly calendars = signal<CalendarModel[]>([]);
  readonly events = signal<EventModel[]>([]);
  readonly freeBusy = signal<Availability[]>([]);
  readonly selectedEvent = signal<EventApi | null>(null);
  readonly editingEvent = signal<EventModel | null>(null);
  readonly view = signal<'dayGridMonth' | 'timeGridWeek' | 'timeGridDay' | 'listWeek'>('dayGridMonth');
  readonly title = signal('');
  readonly saving = signal(false);
  readonly loading = signal(false);

  // Edit form - use a plain object for ngModel binding
  editForm = {
    id: 0,
    calendarId: 0,
    title: '',
    start: '',
    end: '',
    allDay: false,
    location: '',
    timezone: 'Europe/London',
    sequence: 0,
  };

  // Helper to update editForm and trigger change detection
  private updateEditForm(partial: Partial<typeof this.editForm>): void {
    this.editForm = { ...this.editForm, ...partial };
  }

  // FullCalendar options
  //
  // calendarApi is the live handle on the calendar FullCalendar built for us.
  // Nothing else can hand it over: the wrapper component creates it internally,
  // and the only way to reach it is through the component reference below. That
  // is why ngAfterViewInit has to ask for it explicitly, and why every use is
  // guarded rather than asserted with a non-null assertion.
  private calendarApi: Calendar | null = null;

  readonly calendarOptions: CalendarOptions = {
    plugins: [dayGridPlugin, timeGridPlugin, listPlugin, interactionPlugin, rrulePlugin],
    initialView: 'dayGridMonth',
    headerToolbar: false, // We use our own toolbar
    height: '100%',
    events: [],
    eventTimeFormat: {
      hour: '2-digit',
      minute: '2-digit',
      meridiem: 'short',
    },
    slotMinTime: '06:00:00',
    slotMaxTime: '22:00:00',
    allDaySlot: true,
    nowIndicator: true,
    selectable: true,
    selectMirror: true,
    dayMaxEvents: true,
    weekNumbers: true,
    eventDisplay: 'block',
    eventClick: this.handleEventClick.bind(this),
    eventDrop: this.handleEventDrop.bind(this),
    eventResize: this.handleEventResize.bind(this),
    select: this.handleDateSelect.bind(this),
    datesSet: this.handleDatesSet.bind(this),
    eventContent: this.renderEventContent.bind(this),
  };

  async ngAfterViewInit(): Promise<void> {
    // The CalendarApi exists only once the wrapper has rendered, so this is the
    // first moment it can be read. Until it is, every handler below would find
    // a null api and dereference it.
    this.calendarApi = this.calendarRef().getApi();

    // The calendars have to be known before the events are fetched: the fetch
    // asks each calendar in turn, so starting it first would only ever load the
    // first range of calendars to arrive.
    await this.loadCalendars();
    await this.loadEvents();
  }

  ngOnDestroy(): void {
    // Cleanup if needed
  }

  private async loadCalendars(): Promise<void> {
    try {
      const res = await this.api.list();
      this.calendars.set(res.calendars);
    } catch (err) {
      console.error('Failed to load calendars:', err);
    }
  }

  private async loadEvents(): Promise<void> {
    const cal = this.calendarApi;
    if (!cal) return;
    const view = cal.view;
    const start = toRFC3339(view.activeStart);
    const end = toRFC3339(view.activeEnd);

    this.loading.set(true);
    try {
      // The visible range replaces what was loaded, it does not add to it.
      // Appending meant every navigation, and every refetch after a save,
      // doubled the events on screen.
      const found: EventModel[] = [];
      const calendarIds = this.calendars().map((c) => c.id);
      for (const id of calendarIds) {
        const res = await this.api.listEvents(id, start, end);
        found.push(...res.events);
      }
      this.events.set(found);
      this.updateFullCalendarEvents();
    } catch (err) {
      console.error('Failed to load events:', err);
    } finally {
      this.loading.set(false);
    }
  }

  private updateFullCalendarEvents(): void {
    const cal = this.calendarApi;
    if (!cal) return;
    cal.removeAllEvents();
    for (const ev of this.events()) {
      cal.addEvent(this.toFullCalendarEvent(ev));
    }
  }

  private toFullCalendarEvent(ev: EventModel): any {
    return {
      id: ev.eventId.toString(),
      title: ev.summary,
      start: ev.start,
      end: ev.end,
      allDay: ev.allDay,
      backgroundColor: this.calendarColor(ev.calendarId),
      borderColor: this.calendarColor(ev.calendarId),
      extendedProps: {
        // eventId and uid are carried here as well as id, because the handlers
        // work from extendedProps while editing and from id while dragging, and
        // a save built from one of them has to be able to find the other.
        eventId: ev.eventId,
        uid: ev.uid,
        summary: ev.summary,
        location: ev.location,
        timezone: ev.timezone,
        status: ev.status,
        organizer: ev.organizer,
        organizerAddress: ev.organizerAddress,
        sequence: ev.sequence,
        overridden: ev.overridden,
        attendees: ev.attendees || [],
        recurrenceId: ev.recurrenceId,
        calendarId: ev.calendarId,
      },
    };
  }

  private calendarColor(calendarId: number): string {
    const cal = this.calendars().find(c => c.id === calendarId);
    return cal?.color || '#3b82f6';
  }

  // View navigation
  prev(): void {
    this.calendarApi?.prev();
  }

  next(): void {
    this.calendarApi?.next();
  }

  today(): void {
    this.calendarApi?.today();
  }

  changeView(viewName: string): void {
    this.view.set(viewName as any);
    this.calendarApi?.changeView(viewName);
  }

  handleDatesSet(arg: DatesSetArg): void {
    // The range is read from the argument FullCalendar passes rather than from
    // the stored api: the argument is the one describing the range that was just
    // set, which is not necessarily the range the api already reflects while
    // this is being called.
    this.title.set(arg.view.title);
    void this.loadEvents();
  }

  // Event interactions
  handleEventClick(arg: EventClickArg): void {
    this.selectedEvent.set(arg.event);
  }

  handleDateSelect(arg: DateSelectArg): void {
    const start = toRFC3339(arg.start);
    const end = toRFC3339(arg.end);
    const defaultCal = this.calendars().find(c => c.is_default) || this.calendars()[0];
    if (!defaultCal) return;

    this.editForm = {
      id: 0,
      calendarId: defaultCal.id,
      title: '',
      start: start.replace('Z', '').replace('T', 'T'),
      end: end.replace('Z', '').replace('T', 'T'),
      allDay: arg.allDay,
      location: '',
      timezone: defaultCal.timezone || 'Europe/London',
      sequence: 0,
    };
    this.editingEvent.set(null);
  }

  handleEventDrop(arg: EventDropArg): void {
    this.saveMovedEvent(arg.event);
  }

  handleEventResize(arg: EventResizeDoneArg): void {
    this.saveMovedEvent(arg.event);
  }

  private async saveMovedEvent(event: EventApi): Promise<void> {
    const ext = event.extendedProps as any;
    const calendarId = ext.calendarId;
    const eventId = parseInt(event.id, 10);
    const startDate = new Date(event.start!);
    const endDate = event.end ? new Date(event.end) : new Date(startDate.getTime() + 3600000);

    try {
      await this.api.updateEvent(calendarId, eventId, {
        event: {
          uid: ext.uid,
          summary: ext.summary,
          location: ext.location,
          start: toRFC3339(startDate),
          end: toRFC3339(endDate),
          allDay: event.allDay,
          timezone: ext.timezone,
          status: ext.status,
          showAs: 'BUSY',
          sequence: ext.sequence + 1,
        },
        expectedSequence: ext.sequence,
      });
      // Reload events to sync
      void this.loadEvents();
    } catch (err) {
      console.error('Failed to move event:', err);
      void this.loadEvents(); // Refetch to revert UI
    }
  }

  openCreateEvent(): void {
    const defaultCal = this.calendars().find(c => c.is_default) || this.calendars()[0];
    if (!defaultCal) return;

    const now = new Date();
    const start = toRFC3339(now).replace('Z', '').replace('T', 'T');
    const end = toRFC3339(new Date(now.getTime() + 3600000)).replace('Z', '').replace('T', 'T');

    this.editForm = {
      id: 0,
      calendarId: defaultCal.id,
      title: '',
      start,
      end,
      allDay: false,
      location: '',
      timezone: defaultCal.timezone || 'Europe/London',
      sequence: 0,
    };
    this.editingEvent.set(null);
  }

  editEvent(event: EventApi): void {
    const ext = event.extendedProps as any;
    this.closeEventDetail();
    const startDate = new Date(event.start!);
    const endDate = event.end ? new Date(event.end) : new Date(startDate.getTime() + 3600000);
    this.editForm = {
      id: parseInt(event.id, 10),
      calendarId: ext.calendarId,
      title: ext.summary,
      start: toRFC3339(startDate).replace('Z', '').replace('T', 'T'),
      end: toRFC3339(endDate).replace('Z', '').replace('T', 'T'),
      allDay: event.allDay,
      location: ext.location || '',
      timezone: ext.timezone,
      sequence: ext.sequence,
    };
    // editingEvent is what the modal reads to decide whether it is editing or
    // creating, so it has to be the model as the API returned it. Handing it
    // extendedProps instead meant the modal read a record with no eventId and
    // titled an edit of an existing event "New Event".
    this.editingEvent.set(this.eventModelFor(event));
  }

  // eventModelFor rebuilds the model an EventApi was made from. The mapping runs
  // the other way too, in toFullCalendarEvent, so this is its inverse.
  private eventModelFor(event: EventApi): EventModel {
    const ext = event.extendedProps as any;
    const startDate = event.start ? new Date(event.start) : new Date();
    return {
      eventId: parseInt(event.id, 10),
      calendarId: ext.calendarId,
      uid: ext.uid,
      recurrenceId: ext.recurrenceId,
      summary: ext.summary,
      location: ext.location,
      start: startDate.toISOString(),
      end: (event.end ? new Date(event.end) : new Date(startDate.getTime() + 3600000)).toISOString(),
      allDay: event.allDay,
      timezone: ext.timezone,
      status: ext.status,
      organizer: ext.organizer,
      organizerAddress: ext.organizerAddress,
      sequence: ext.sequence,
      overridden: ext.overridden,
      attendees: ext.attendees || [],
    };
  }

  async saveEvent(): Promise<void> {
    const form = this.editForm;
    if (!form.title.trim()) return;

    this.saving.set(true);
    try {
      const eventData: ICalEvent = {
        // The uid names the series, so it comes from the event being edited
        // rather than from a new one. A save without it leaves the server to
        // invent a uid and quietly create a second event instead of updating
        // the one the user is looking at.
        uid: form.id ? this.events().find((e) => e.eventId === form.id)?.uid : undefined,
        summary: form.title,
        location: form.location,
        start: form.start,
        end: form.end,
        allDay: form.allDay,
        timezone: form.timezone,
        status: 'CONFIRMED',
        showAs: 'BUSY',
        sequence: form.id ? form.sequence + 1 : 1,
      };

      if (form.id) {
        await this.api.updateEvent(form.calendarId, form.id, {
          event: eventData,
          expectedSequence: form.sequence,
        });
      } else {
        await this.api.createEvent(form.calendarId, { event: eventData });
      }
      this.closeEditModal();
      void this.loadEvents();
    } catch (err) {
      console.error('Failed to save event:', err);
      alert('Failed to save event: ' + (err as Error).message);
    } finally {
      this.saving.set(false);
    }
  }

  async deleteEvent(event: EventApi): Promise<void> {
    if (!confirm('Delete this event?')) return;

    const ext = event.extendedProps as any;
    const calendarId = ext.calendarId;
    const eventId = parseInt(event.id, 10);

    try {
      await this.api.deleteEvent(calendarId, eventId);
      this.closeEventDetail();
      void this.loadEvents();
    } catch (err) {
      console.error('Failed to delete event:', err);
      alert('Failed to delete event: ' + (err as Error).message);
    }
  }

  closeEventDetail(): void {
    this.selectedEvent.set(null);
  }

  closeEditModal(): void {
    this.editingEvent.set(null);
    this.editForm = {
      id: 0,
      calendarId: 0,
      title: '',
      start: '',
      end: '',
      allDay: false,
      location: '',
      timezone: 'Europe/London',
      sequence: 0,
    };
  }

  protected formatDateTime = formatDateTime;
  protected formatDate = formatDate;

  // Format helpers for template (FullCalendar EventApi uses Date objects)
  protected fmtTime(d: Date | null | undefined): string {
    return d ? formatDateTime(d.toISOString()) : '';
  }
  protected fmtDate(d: Date | null | undefined): string {
    return d ? formatDate(d.toISOString()) : '';
  }
  protected fmtDateTime(d: Date | null | undefined): string {
    return d ? formatDateTime(d.toISOString()) : '';
  }

  private renderEventContent(eventInfo: any) {
    // Custom event rendering if needed
    return {
      html: `<div class="fc-event-title">${eventInfo.event.title}</div>`,
    };
  }
}
import { ChangeDetectionStrategy, Component, input } from '@angular/core';

// Feather-derived 24x24 stroke icons. The set is closed: a missing name is a
// bug that renders an empty box rather than an unbounded icon library.
const ICONS: Record<string, string[]> = {
  calendar: [
    'M8 2v4',
    'M16 2v4',
    'M21 13V6a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-5',
    'M3 10h18',
    'M8 14h.01',
    'M12 14h.01',
    'M16 14h.01',
  ],
  'calendar-plus': [
    'M8 2v4',
    'M16 2v4',
    'M21 13V6a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-5',
    'M12 8v6',
    'M9 11h6',
  ],
  'calendar-x': [
    'M8 2v4',
    'M16 2v4',
    'M21 13V6a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-5',
    'M9 11l6 6',
    'M15 11l-6 6',
  ],
  'calendar-clock': [
    'M8 2v4',
    'M16 2v4',
    'M21 13V6a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-5',
    'M12 12v4',
    'M12 12l3 3',
  ],
  today: [
    'M4 7V4a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v3',
    'M9 20h6',
    'M12 4v16',
  ],
  prev: ['M15 18l-6-6 6-6'],
  next: ['M9 18l6-6-6-6'],
  plus: ['M12 5v14', 'M5 12h14'],
  x: ['M18 6 6 18', 'M6 6l12 12'],
  edit: ['M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7', 'M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z'],
  trash: [
    'M3 6h18',
    'M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6',
    'M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2',
    'M10 11v6',
    'M14 11v6',
  ],
  eye: [
    'M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7z',
    'M12 18a6 6 0 0 1 0-12',
    'M12 14a2 2 0 0 1 0-4',
  ],
  'eye-off': [
    'M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24',
    'M1 1l22 22',
  ],
  users: [
    'M12 11a4 4 0 1 1 0-8 4 4 0 0 1 0 8z',
    'M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2',
  ],
  user: ['M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2', 'M12 11a4 4 0 1 1 0-8 4 4 0 0 1 0 8z'],
  search: ['M11 3a8 8 0 1 0 0 16 8 8 0 0 0 0-16z', 'M21 21l-4.35-4.35'],
  filter: ['M3 3h18v18H3z', 'M7 9h10', 'M7 15h6'],
  settings: [
    'M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.15a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z',
    'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z',
  ],
  arrow_left: ['M15 18l-6-6 6-6'],
  // share-2: three nodes and the two links between them, which is what a share
  // is — one calendar reaching several people.
  share: [
    'M18 8a3 3 0 1 0 0-6 3 3 0 0 0 0 6z',
    'M6 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z',
    'M18 22a3 3 0 1 0 0-6 3 3 0 0 0 0 6z',
    'M8.59 13.51l6.83 3.98',
    'M15.41 6.51l-6.82 3.98',
  ],
  arrow_right: ['M9 18l6-6-6-6'],
  chevron_left: ['M15 18l-6-6 6-6'],
  chevron_right: ['M9 18l6-6-6-6'],
  download: ['M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4', 'M7 10l5 5 5-5', 'M12 15V3'],
  upload: ['M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4', 'M17 21l-5-5-5 5', 'M12 15v-12'],
  more_vertical: ['M12 12h.01', 'M12 5h.01', 'M12 19h.01'],
  check: ['M20 6 9 17l-5-5'],
  alert: [
    'M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z',
    'M12 9v4',
    'M12 17h.01',
  ],
  info: [
    'M12 22c5.523 0 10-4.477 10-10S17.523 2 12 2 2 6.477 2 12s4.477 10 10 10z',
    'M12 16v-4',
    'M12 8h.01',
  ],
  loader: ['M12 2v4', 'M12 18v4', 'M4.93 4.93l2.83 2.83', 'M16.24 16.24l2.83 2.83', 'M2 12h4', 'M18 12h4', 'M4.93 19.07l2.83-2.83', 'M16.24 7.76l2.83-2.83'],
  repeat: [
    'M17 1l4 4-4 4',
    'M3 11V9a4 4 0 0 1 4-4h14',
    'M7 23l-4-4 4-4',
    'M21 13v2a4 4 0 0 1-4 4H3',
  ],
  copy: [
    'M16 16h2a2 2 0 0 1 2 2v2a2 2 0 0 1-2 2h-2',
    'M8 8H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2',
    'M8 2v2a2 2 0 0 0 2 2h2',
    'M16 22v-2a2 2 0 0 0-2-2h-2',
    'M22 8v2a2 2 0 0 1-2 2h-2',
    'M22 16v2a2 2 0 0 1-2 2h-2',
  ],
};

// CalIcon renders one icon from the closed set above.
@Component({
  selector: 'app-cal-icon',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
      [style.width.px]="size()"
      [style.height.px]="size()"
      class="shrink-0"
    >
      @for (d of paths(); track d) {
        <path [attr.d]="d" />
      }
    </svg>
  `,
})
export class CalIcon {
  readonly name = input.required<string>();
  readonly size = input(16);

  protected paths(): string[] {
    return ICONS[this.name()] ?? ICONS['calendar'] ?? [];
  }
}

// The status strings the server stores and returns.
export const STATUS_CONFIRMED = 'CONFIRMED';
export const STATUS_TENTATIVE = 'TENTATIVE';
export const STATUS_CANCELLED = 'CANCELLED';
export const SHOW_AS_BUSY = 'BUSY';
export const SHOW_AS_FREE = 'FREE';
export const SHOW_AS_TENTATIVE = 'TENTATIVE';
export const SHOW_AS_OOF = 'OOF';

export function isBusy(status: string, showAs: string): boolean {
  if (status === STATUS_CANCELLED) return false;
  if (showAs === SHOW_AS_FREE) return false;
  return true;
}

// formatDateTime renders an RFC 3339 timestamp for a calendar event.
export function formatDateTime(iso: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
}

// formatDate renders an RFC 3339 date for an all-day event.
export function formatDate(iso: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

// formatMonthYear renders the calendar header.
export function formatMonthYear(d: Date): string {
  return d.toLocaleDateString(undefined, { month: 'long', year: 'numeric' });
}

// formatDayHeader renders a day-of-week header.
export function formatDayHeader(d: Date): string {
  return d.toLocaleDateString(undefined, { weekday: 'short' });
}

// eventColorClass returns a Tailwind color class for an event based on the
// calendar's color or the event's status.
export function eventColorClass(color?: string): string {
  if (!color) return 'bg-blue-500';
  // The server stores hex colors; map known ones to Tailwind classes.
  // Fallback: use the hex directly via style.
  return '';
}

// parseRFC3339 parses an RFC 3339 string to a Date, or returns null on failure.
export function parseRFC3339(s: string): Date | null {
  if (!s) return null;
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? null : d;
}

// toRFC3339 formats a Date as RFC 3339 (UTC, no milliseconds).
export function toRFC3339(d: Date): string {
  return d.toISOString().replace(/\.\d{3}Z$/, 'Z');
}

// toLocalInputValue formats a Date for a datetime-local input. Those inputs
// read and write wall-clock time in the browser's local zone, so building the
// string from local components (rather than stripping the Z off a UTC string)
// keeps the prefilled value in the same zone the input will parse it back in.
export function toLocalInputValue(d: Date): string {
  const pad = (n: number): string => String(n).padStart(2, '0');
  return (
    `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}` +
    `T${pad(d.getHours())}:${pad(d.getMinutes())}`
  );
}

// addDays returns a new Date with days added.
export function addDays(d: Date, days: number): Date {
  const out = new Date(d);
  out.setDate(out.getDate() + days);
  return out;
}

// startOfDay returns a new Date at midnight in the local timezone.
export function startOfDay(d: Date): Date {
  const out = new Date(d);
  out.setHours(0, 0, 0, 0);
  return out;
}

// endOfDay returns a new Date at 23:59:59.999 in the local timezone.
export function endOfDay(d: Date): Date {
  const out = new Date(d);
  out.setHours(23, 59, 59, 999);
  return out;
}

// startOfWeek returns the Monday of the week containing d.
export function startOfWeek(d: Date): Date {
  const out = new Date(d);
  const day = out.getDay(); // 0 = Sunday
  const diff = day === 0 ? -6 : 1 - day; // Monday = 1
  out.setDate(out.getDate() + diff);
  out.setHours(0, 0, 0, 0);
  return out;
}

// startOfMonth returns the first day of the month at midnight.
export function startOfMonth(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), 1);
}

// endOfMonth returns the last day of the month at 23:59:59.999.
export function endOfMonth(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth() + 1, 0, 23, 59, 59, 999);
}

// daysInMonth returns the number of days in the month of d.
export function daysInMonth(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth() + 1, 0).getDate();
}

// eachDayOfInterval yields every day between start (inclusive) and end (inclusive).
export function* eachDayOfInterval(start: Date, end: Date): Generator<Date> {
  const cur = new Date(start);
  while (cur <= end) {
    yield new Date(cur);
    cur.setDate(cur.getDate() + 1);
  }
}
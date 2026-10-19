// Presentation helpers for the meeting pages.

// PARTSTAT_LABELS are the answers as a guest list shows them. The wire values
// are iTIP's; nobody reads one of those on screen.
export const PARTSTAT_LABELS: Record<string, string> = {
  'NEEDS-ACTION': 'No answer',
  ACCEPTED: 'Accepted',
  DECLINED: 'Declined',
  TENTATIVE: 'Maybe',
};

export function partStatLabel(partStat: string): string {
  return PARTSTAT_LABELS[partStat] ?? partStat;
}

// partStatClass colours an answer. Accepted is the one the organiser scans
// for, declined the one they must not miss.
export function partStatClass(partStat: string): string {
  switch (partStat) {
    case 'ACCEPTED':
      return 'bg-emerald-100 text-emerald-800';
    case 'DECLINED':
      return 'bg-rose-100 text-rose-800';
    case 'TENTATIVE':
      return 'bg-amber-100 text-amber-800';
    default:
      return 'bg-slate-100 text-slate-600';
  }
}

// formatWhen renders the meeting's span in the meeting's own timezone, with
// the zone named, the way the invitation email renders it - a time with no
// zone attached is a promise no reader can keep.
//
// A meeting that crosses midnight shows both dates: one date would send
// somebody to the wrong morning.
export function formatWhen(startIso: string, endIso: string, timezone: string): string {
  const start = new Date(startIso);
  const end = new Date(endIso);
  if (Number.isNaN(start.getTime()) || Number.isNaN(end.getTime())) {
    return `${startIso} - ${endIso}`;
  }
  const zone = safeZone(timezone);
  const dayAndClock = (withZone: boolean): Intl.DateTimeFormatOptions => ({
    timeZone: zone,
    weekday: 'short',
    day: 'numeric',
    month: 'short',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    ...(withZone ? { timeZoneName: 'short' as const } : {}),
  });
  const clockOnly: Intl.DateTimeFormatOptions = { timeZone: zone, hour: '2-digit', minute: '2-digit' };

  if (sameLocalDay(start, end, zone)) {
    return `${start.toLocaleString(undefined, dayAndClock(true))} - ${end.toLocaleString(undefined, clockOnly)}`;
  }
  return `${start.toLocaleString(undefined, dayAndClock(true))} - ${end.toLocaleString(undefined, dayAndClock(false))}`;
}

// sameLocalDay compares calendar days in the zone the times display in: the
// UTC date and the zone's date are not the same question.
function sameLocalDay(a: Date, b: Date, timeZone: string | undefined): boolean {
  const fmt = new Intl.DateTimeFormat('en-CA', {
    timeZone,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  });
  return fmt.format(a) === fmt.format(b);
}

// safeZone falls back to the browser's zone for a timezone the browser does
// not know, so a meeting can still be read rather than throwing at render.
function safeZone(timezone: string): string | undefined {
  if (!timezone) return undefined;
  try {
    new Intl.DateTimeFormat('en', { timeZone: timezone }).format();
    return timezone;
  } catch {
    return undefined;
  }
}

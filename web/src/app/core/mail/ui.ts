import { ChangeDetectionStrategy, Component, input } from '@angular/core';
import { Address } from './mail.models';
import { htmlToText } from './rich-text';

// The presentational pieces and helpers the mail pages share.

// Feather-derived 24x24 stroke icons. The set is closed: a missing name is a
// bug that renders an empty box rather than an unbounded icon library.
const ICONS: Record<string, string[]> = {
  inbox: [
    'M22 12h-6l-2 3h-4l-2-3H2',
    'M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z',
  ],
  send: ['M22 2 11 13', 'M22 2 15 22l-4-9-9-4Z'],
  file: [
    'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z',
    'M14 2v6h6',
    'M16 13H8',
    'M16 17H8',
    'M10 9H8',
  ],
  alert: [
    'M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z',
    'M12 9v4',
    'M12 17h.01',
  ],
  trash: [
    'M3 6h18',
    'M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6',
    'M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2',
    'M10 11v6',
    'M14 11v6',
  ],
  archive: ['M21 8v13H3V8', 'M1 3h14l6 5H1z', 'M10 12h4'],
  folder: ['M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z'],
  clip: [
    'M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48',
  ],
  flag: ['M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1z', 'M4 22v-7'],
  search: ['M11 3a8 8 0 1 0 0 16 8 8 0 0 0 0-16z', 'M21 21l-4.35-4.35'],
  plus: ['M12 5v14', 'M5 12h14'],
  x: ['M18 6 6 18', 'M6 6l12 12'],
  reply: ['M9 14 4 9l5-5', 'M20 20v-7a4 4 0 0 0-4-4H4'],
  conversation: [
    'M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z',
  ],
  sliders: [
    'M4 21v-7',
    'M4 10V3',
    'M12 21v-9',
    'M12 8V3',
    'M20 21v-5',
    'M20 12V3',
    'M1 14h6',
    'M9 8h6',
    'M17 16h6',
  ],
  download: ['M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4', 'M7 10l5 5 5-5', 'M12 15V3'],
  refresh: [
    'M23 4v6h-6',
    'M1 20v-6h6',
    'M3.51 9a9 9 0 0 1 14.85-3.36L23 10',
    'M20.49 15a9 9 0 0 1-14.85 3.36L1 14',
  ],
  check: ['M20 6 9 17l-5-5'],
  arrow: ['M5 12h14', 'M12 5l7 7-7 7'],
  users: [
    'M12 11a4 4 0 1 1 0-8 4 4 0 0 1 0 8z',
    'M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2',
  ],
  // The disclosure triangle beside a collapsible row. Two spellings because
  // an icon name that is not in this map renders the folder icon instead of
  // nothing, which is how a chevron used to show up as a second folder.
  chevron_right: ['M9 18l6-6-6-6'],
  chevron_down: ['M6 9l6 6 6-6'],
};

// MailIcon renders one icon from the closed set above.
@Component({
  selector: 'app-mail-icon',
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
export class MailIcon {
  readonly name = input.required<string>();
  readonly size = input(16);

  protected paths(): string[] {
    return ICONS[this.name()] ?? ICONS['folder'] ?? [];
  }
}

// The IMAP flag strings the server stores and returns.
export const FLAG_SEEN = '\\Seen';
export const FLAG_FLAGGED = '\\Flagged';
export const FLAG_DELETED = '\\Deleted';

export function hasFlag(flags: string[], flag: string): boolean {
  return flags.includes(flag);
}

// formatBytes renders a byte count the way the reader and composer show it.
export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '';
  if (n < 1024) return `${n} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

// formatDate renders an RFC 3339 timestamp for a list row: "Mar 2, 14:05",
// with the year added only for older mail.
export function formatDate(iso: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const now = new Date();
  const date = d.toLocaleDateString(undefined, {
    month: 'short',
    day: 'numeric',
    ...(d.getFullYear() === now.getFullYear() ? {} : { year: 'numeric' }),
  });
  const time = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  return `${date}, ${time}`;
}

// addressLabel renders one header address the way the reader shows it.
export function addressLabel(a: Address): string {
  return a.name ? `${a.name} <${a.address}>` : a.address;
}

// folderIcon maps the RFC 6154 special uses onto the sidebar glyphs.
export function folderIcon(use?: string): string {
  switch (use) {
    case '\\inbox':
      return 'inbox';
    case '\\sent':
      return 'send';
    case '\\drafts':
      return 'file';
    case '\\junk':
      return 'alert';
    case '\\trash':
      return 'trash';
    case '\\archive':
      return 'archive';
    default:
      return 'folder';
  }
}

// parseAddresses turns a comma or semicolon separated recipient field into
// address objects, keeping a display name when one was typed. Entries that are
// not addresses are dropped, which is what makes a red preview line possible
// in the composer instead of a silent mis-send.
export function parseAddresses(raw: string): Address[] {
  const out: Address[] = [];
  for (const part of raw.split(/[,;]/)) {
    const entry = part.trim();
    if (!entry) continue;
    const m = entry.match(/^([^<>]*?)\s*<([^>]+)>$/);
    if (m) {
      const name = m[1].trim().replace(/^"|"$/g, '').trim();
      const address = m[2].trim();
      if (address) out.push(name ? { address, name } : { address });
    } else if (entry.includes('@')) {
      out.push({ address: entry });
    }
  }
  return out;
}

// escapeHtml turns markup characters into text, for quoting a message body
// that arrived as plain text into an HTML citation.
export function escapeHtml(value: string): string {
  return value
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

// quoteTextPrefix prefixes every line of a plain-text body with the citation
// marker, which is how a reply cites the message it answers.
export function quoteTextPrefix(body: string): string {
  if (!body) return '';
  return body
    .replace(/\r\n/g, '\n')
    .split('\n')
    .map((line) => `> ${line}`)
    .join('\n');
}

// buildReplyQuote renders the original message as a citation for the composer,
// in both the rich-text and plain-text forms.
//
// The HTML form puts the original body inside a <blockquote> so it is visually
// set apart and cannot be mistaken for the reply's own words. A body that
// arrived as HTML is passed through as the citation (it is already markup); one
// that arrived as text is escaped and line-prefixed, so nothing in the message
// can inject markup into the composer. An empty paragraph leads the quote so
// the caret starts above it, where the reply is written.
//
// attribution is the "On <date>, <sender> wrote:" line, already rendered.
export function buildReplyQuote(attribution: string, htmlBody: string, textBody: string): {
  quoted_html: string;
  quoted_text: string;
} {
  const attributionText = attribution.replace(/\s+/g, ' ').trim();

  const quoted_html = htmlBody
    ? `<p></p><p>${escapeHtml(attributionText)}</p><blockquote>${htmlBody}</blockquote>`
    : textBody
      ? `<p></p><p>${escapeHtml(attributionText)}</p><blockquote>${escapeHtml(textBody).replace(/\n/g, '<br>')}</blockquote>`
      : '';

  const body = textBody || (htmlBody ? htmlToText(htmlBody) : '');
  const quoted_text = body
    ? `${attributionText}\n${quoteTextPrefix(body)}`
    : '';

  return { quoted_html, quoted_text };
}

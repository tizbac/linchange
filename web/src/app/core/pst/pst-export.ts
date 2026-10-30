// Converts items read by the PST reader into the formats the rest of linchange
// stores: RFC 5322 messages, vCards and iCalendar documents.
//
// The browser import sends these bytes to the server, which stores them the
// same way it stores mail received over SMTP, so a message imported from a PST
// threads, searches and displays exactly like one that arrived normally.

import {
  GUID_ADDRESS,
  GUID_APPOINTMENT,
  GUID_MEETING,
  LID,
  PROPERTY,
  PstItem,
  PstReader,
} from "./pst";

/** A postal address, for a vCard. */
export interface PostalAddress {
  street: string;
  city: string;
  region: string;
  postalCode: string;
  country: string;
}

/** The subset of a MAPI contact a vCard can carry. */
export interface ContactCard {
  uid: string;
  displayName: string;
  givenName: string;
  familyName: string;
  middleName: string;
  organization: string;
  title: string;
  notes: string;
  emails: string[];
  phones: string[];
  workAddress: PostalAddress;
  homeAddress: PostalAddress;
}

/** The subset of a MAPI appointment an iCalendar VEVENT can carry. */
export interface Appointment {
  uid: string;
  summary: string;
  description: string;
  location: string;
  start: Date;
  end: Date;
  allDay: boolean;
}

export function itemText(item: PstItem, identifier: number): string {
  return item.properties.get(identifier)?.text ?? "";
}

export function itemTime(item: PstItem, identifier: number): Date | undefined {
  return item.properties.get(identifier)?.time;
}

export function itemInt(item: PstItem, identifier: number): number {
  return item.properties.get(identifier)?.int ?? 0;
}

function named(reader: PstReader, item: PstItem, guid: string, lid: number) {
  for (const [identifier, property] of item.properties) {
    const np = reader.namedProperty(identifier);
    if (!np) {
      continue;
    }
    if (np.lid === lid && np.name === "" && np.guid === guid) {
      return property;
    }
  }
  return undefined;
}

function namedText(reader: PstReader, item: PstItem, guid: string, lid: number): string {
  return named(reader, item, guid, lid)?.text ?? "";
}

function namedTime(reader: PstReader, item: PstItem, guid: string, lid: number): Date | undefined {
  return named(reader, item, guid, lid)?.time;
}

function namedInt(reader: PstReader, item: PstItem, guid: string, lid: number): number {
  return named(reader, item, guid, lid)?.int ?? 0;
}

/** Strips the control characters Outlook puts in front of a subject. */
export function subjectOf(subject: string): string {
  return subject.replace(/^[\x01-\x04]+/, "");
}

// --- message ---------------------------------------------------------------

/** Renders a message item as an RFC 5322 message. */
export function buildMessage(reader: PstReader, item: PstItem): Uint8Array {
  const subject = subjectOf(itemText(item, PROPERTY.subject));
  const from = senderOf(item);
  const transportHeaders = itemText(item, PROPERTY.transportHeaders);
  const date = itemTime(item, PROPERTY.submitTime) ?? itemTime(item, PROPERTY.deliveryTime) ?? new Date();
  let messageId = itemText(item, PROPERTY.internetMessageId).trim().replace(/[<>]/g, "");
  if (!messageId) {
    messageId = `pst-${item.id}@linchange.import`;
  }

  const body = itemText(item, PROPERTY.body);
  const html = itemText(item, PROPERTY.bodyHtml);

  let payload: Uint8Array;
  let contentType: string;
  if (html && body) {
    const boundary = `=_pst_${item.id}_${Math.random().toString(36).slice(2)}`;
    payload = concat([
      text(`--${boundary}\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n`),
      quotedPrintable(body),
      text(`\r\n--${boundary}\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n`),
      quotedPrintable(html),
      text(`\r\n--${boundary}--\r\n`),
    ]);
    contentType = `multipart/alternative; boundary="${boundary}"`;
  } else if (html) {
    payload = quotedPrintable(html);
    contentType = "text/html; charset=utf-8";
  } else {
    payload = quotedPrintable(body);
    contentType = "text/plain; charset=utf-8";
  }

  let transferEncoding = contentType.startsWith("text/") ? "quoted-printable" : "";
  if (item.attachments.length) {
    const boundary = `=_pst_mix_${item.id}_${Math.random().toString(36).slice(2)}`;
    const parts: Uint8Array[] = [];
    parts.push(
      text(
        `--${boundary}\r\nContent-Type: ${contentType}\r\nContent-Transfer-Encoding: ${
          contentType.startsWith("text/") ? "quoted-printable" : "8bit"
        }\r\n\r\n`,
      ),
    );
    parts.push(payload);
    for (const attachment of item.attachments) {
      const name = attachment.name || "attachment";
      parts.push(
        text(
          `\r\n--${boundary}\r\nContent-Type: application/octet-stream; name="${escapeHeader(name)}"\r\nContent-Disposition: attachment; filename="${escapeHeader(
            name,
          )}"\r\nContent-Transfer-Encoding: base64\r\n\r\n`,
        ),
      );
      parts.push(text(wrapBase64(base64(attachment.data))));
    }
    parts.push(text(`\r\n--${boundary}--\r\n`));
    payload = concat(parts);
    contentType = `multipart/mixed; boundary="${boundary}"`;
    transferEncoding = "";
  }

  const headers: string[] = [];
  headers.push("MIME-Version: 1.0");
  headers.push(`Date: ${date.toUTCString()}`);
  if (from.address) {
    headers.push(`From: ${formatAddress(from.name, from.address)}`);
  }
  let to = itemText(item, PROPERTY.displayTo);
  if (to && !to.includes("@")) {
    to = headerValue(transportHeaders, "To") || to;
  }
  to = cleanAddressList(to);
  if (to) {
    headers.push(`To: ${sanitizeHeader(to)}`);
  }
  let cc = itemText(item, PROPERTY.displayCc);
  if (cc && !cc.includes("@")) {
    cc = headerValue(transportHeaders, "Cc") || cc;
  }
  cc = cleanAddressList(cc);
  if (cc) {
    headers.push(`Cc: ${sanitizeHeader(cc)}`);
  }
  headers.push(`Subject: ${encodeHeader(subject)}`);
  headers.push(`Message-ID: <${messageId}>`);
  const references = itemText(item, PROPERTY.internetReferences).trim();
  if (references) {
    headers.push(`References: ${sanitizeHeader(references)}`);
  }
  headers.push(`Content-Type: ${contentType}`);
  if (transferEncoding) {
    headers.push(`Content-Transfer-Encoding: ${transferEncoding}`);
  }
  return concat([text(headers.join("\r\n") + "\r\n\r\n"), payload]);
}

function formatAddress(name: string, address: string): string {
  if (!name) {
    return address;
  }
  return `${encodeHeader(name)} <${address}>`;
}

// senderOf prefers a routable SMTP address. Outlook puts an Exchange DN in
// PR_SENDER_EMAIL_ADDRESS for internal mail; the SMTP-valued properties and
// the transport headers carry the address another mail system can reply to.
function senderOf(item: PstItem): { name: string; address: string } {
  const name =
    itemText(item, PROPERTY.senderName) || itemText(item, PROPERTY.sentRepresentingName);
  const candidates = [
    itemText(item, PROPERTY.senderAddress),
    itemText(item, 0x5d01), // PidTagSenderSmtpAddress
    itemText(item, 0x5d02), // PidTagSentRepresentingSmtpAddress
    itemText(item, PROPERTY.sentRepresentingAddress),
  ];
  for (const candidate of candidates) {
    if (candidate.includes("@")) {
      return { name, address: candidate.trim() };
    }
  }
  const from = headerValue(itemText(item, PROPERTY.transportHeaders), "From");
  if (from) {
    const match = from.match(/^(.*)<([^>]+)>/);
    if (match) {
      return { name: name || match[1].trim().replace(/^"|"$/g, ""), address: match[2].trim() };
    }
    return { name, address: from.trim() };
  }
  return { name, address: candidates[0] ?? "" };
}

function headerValue(headers: string, name: string): string {
  for (const line of headers.replace(/\r\n/g, "\n").split("\n")) {
    const colon = line.indexOf(":");
    if (colon < 0) {
      continue;
    }
    if (line.slice(0, colon).trim().toLowerCase() === name.toLowerCase()) {
      return line.slice(colon + 1).trim();
    }
  }
  return "";
}

/** Drops the single quotes Outlook sometimes wraps a display address list in. */
function cleanAddressList(value: string): string {
  value = value.trim();
  if (value.length >= 2 && value.startsWith("'") && value.endsWith("'")) {
    value = value.slice(1, -1);
  }
  return value;
}

function escapeHeader(value: string): string {
  return sanitizeHeader(value).replace(/["\\]/g, "");
}

function sanitizeHeader(value: string): string {
  return value.replace(/[\r\n]+/g, " ");
}

function encodeHeader(value: string): string {
  if (/^[\x00-\x7f]*$/.test(value)) {
    return value;
  }
  return `=?UTF-8?B?${base64(new TextEncoder().encode(value))}?=`;
}

function quotedPrintable(value: string): Uint8Array {
  const bytes = new TextEncoder().encode(value.replace(/\r\n/g, "\n").replace(/\n/g, "\r\n"));
  const out: string[] = [];
  let line = "";
  const flush = () => {
    out.push(line);
    line = "";
  };
  for (const byte of bytes) {
    if (byte === 0x0d) {
      continue;
    }
    if (byte === 0x0a) {
      flush();
      continue;
    }
    let piece: string;
    const printable =
      (byte >= 33 && byte <= 126 && byte !== 61) || byte === 32 || byte === 9;
    if (printable) {
      piece = String.fromCharCode(byte);
    } else {
      piece = `=${byte.toString(16).toUpperCase().padStart(2, "0")}`;
    }
    if (line.length + piece.length > 75) {
      out.push(line + "=");
      line = "";
    }
    line += piece;
  }
  if (line) {
    out.push(line);
  }
  return text(out.join("\r\n") + "\r\n");
}

// --- calendar --------------------------------------------------------------

/** Reads an appointment item into the values a VEVENT needs. */
export function appointmentOf(reader: PstReader, item: PstItem): Appointment {
  let start = namedTime(reader, item, GUID_APPOINTMENT, LID.appointmentStartWhole) ?? itemTime(item, PROPERTY.startDate);
  let end = namedTime(reader, item, GUID_APPOINTMENT, LID.appointmentEndWhole) ?? itemTime(item, PROPERTY.endDate);
  const allDay = namedInt(reader, item, GUID_APPOINTMENT, LID.appointmentSubType) !== 0;
  if (!start) {
    start = new Date(0);
  }
  if (!end) {
    end = new Date(start.getTime() + 60 * 60 * 1000);
  }
  let uid = `pst-${item.id}@linchange.import`;
  const globalId = named(reader, item, GUID_MEETING, LID.globalObjectId);
  if (globalId?.data?.length) {
    uid = `pst-${item.id}-${hex(globalId.data)}`;
  }
  return {
    uid,
    summary: subjectOf(itemText(item, PROPERTY.subject)),
    description: itemText(item, PROPERTY.body),
    location: namedText(reader, item, GUID_APPOINTMENT, LID.location),
    start,
    end,
    allDay,
  };
}

/** Renders an appointment as a plain VCALENDAR. */
export function buildICalendar(appointment: Appointment): string {
  const lines: string[] = [
    "BEGIN:VCALENDAR",
    "VERSION:2.0",
    "PRODID:-//linchange//calendars//EN",
    "BEGIN:VEVENT",
    `UID:${escapeICal(appointment.uid)}`,
    `DTSTAMP:${formatICalTime(new Date(), false)}`,
    `DTSTART${appointment.allDay ? ";VALUE=DATE" : ""}:${formatICalTime(appointment.start, appointment.allDay)}`,
  ];
  if (!appointment.allDay) {
    lines.push(`DTEND:${formatICalTime(appointment.end, false)}`);
  }
  if (appointment.summary) {
    lines.push(`SUMMARY:${escapeICal(appointment.summary)}`);
  }
  if (appointment.description) {
    lines.push(`DESCRIPTION:${escapeICal(appointment.description)}`);
  }
  if (appointment.location) {
    lines.push(`LOCATION:${escapeICal(appointment.location)}`);
  }
  lines.push("STATUS:CONFIRMED", "TRANSP:OPAQUE", "END:VEVENT", "END:VCALENDAR");
  return lines.map(foldLine).join("\r\n") + "\r\n";
}

function formatICalTime(date: Date, dateOnly: boolean): string {
  const iso = date.toISOString().replace(/[-:]/g, "");
  return dateOnly ? iso.slice(0, 8) : iso.slice(0, 15) + "Z";
}

function escapeICal(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/;/g, "\\;").replace(/,/g, "\\,").replace(/\r?\n/g, "\\n");
}

// --- contacts --------------------------------------------------------------

/** Reads a contact item into the values a vCard needs. */
export function contactOf(reader: PstReader, item: PstItem): ContactCard {
  const card: ContactCard = {
    uid: `pst-${item.id}@linchange.import`,
    displayName: itemText(item, PROPERTY.displayName),
    givenName: itemText(item, PROPERTY.givenName),
    familyName: itemText(item, PROPERTY.surname),
    middleName: itemText(item, PROPERTY.middleName),
    organization: itemText(item, PROPERTY.organization),
    title: itemText(item, PROPERTY.jobTitle),
    notes: itemText(item, PROPERTY.notes) || itemText(item, PROPERTY.body),
    emails: [],
    phones: [],
    workAddress: {
      street: namedText(reader, item, GUID_ADDRESS, LID.workAddressStreet),
      city: namedText(reader, item, GUID_ADDRESS, LID.workAddressCity),
      region: namedText(reader, item, GUID_ADDRESS, LID.workAddressState),
      postalCode: namedText(reader, item, GUID_ADDRESS, LID.workAddressPostalCode),
      country: namedText(reader, item, GUID_ADDRESS, LID.workAddressCountry),
    },
    homeAddress: { street: "", city: "", region: "", postalCode: "", country: "" },
  };
  if (!card.displayName) {
    card.displayName = `${card.givenName} ${card.familyName}`.trim();
  }

  const addEmail = (address: string) => {
    address = address.trim();
    if (!address.includes("@")) {
      return;
    }
    if (!card.emails.some((existing) => existing.toLowerCase() === address.toLowerCase())) {
      card.emails.push(address);
    }
  };
  addEmail(itemText(item, PROPERTY.emailAddress));
  addEmail(namedText(reader, item, GUID_ADDRESS, LID.email1Address));
  addEmail(namedText(reader, item, GUID_ADDRESS, LID.email2Address));
  addEmail(namedText(reader, item, GUID_ADDRESS, LID.email3Address));
  if (!card.emails.length) {
    for (const [identifier, property] of item.properties) {
      if (identifier >= 0x8000 && property.text.includes("@")) {
        addEmail(property.text);
      }
    }
  }

  const addPhone = (number: string) => {
    number = number.trim();
    if ((number.match(/\d/g) ?? []).length >= 3) {
      card.phones.push(number);
    }
  };
  addPhone(itemText(item, PROPERTY.businessPhone));
  addPhone(itemText(item, PROPERTY.homePhone));
  addPhone(itemText(item, PROPERTY.mobilePhone));
  return card;
}

/** Renders a contact as a version 3.0 vCard. */
export function buildVCard(card: ContactCard): string {
  const lines: string[] = [
    "BEGIN:VCARD",
    "VERSION:3.0",
    `UID:${escapeVCard(card.uid)}`,
    `FN:${escapeVCard(card.displayName)}`,
    `N:${escapeVCard(card.familyName)};${escapeVCard(card.givenName)};${escapeVCard(card.middleName)};;`,
  ];
  if (card.organization) {
    lines.push(`ORG:${escapeVCard(card.organization)}`);
  }
  if (card.title) {
    lines.push(`TITLE:${escapeVCard(card.title)}`);
  }
  for (const email of card.emails) {
    lines.push(`EMAIL;TYPE=INTERNET:${escapeVCard(email)}`);
  }
  for (const phone of card.phones) {
    lines.push(`TEL;TYPE=VOICE:${escapeVCard(phone)}`);
  }
  if (hasAddress(card.workAddress)) {
    lines.push(`ADR;TYPE=WORK:${vcardAddress(card.workAddress)}`);
  }
  if (hasAddress(card.homeAddress)) {
    lines.push(`ADR;TYPE=HOME:${vcardAddress(card.homeAddress)}`);
  }
  if (card.notes) {
    lines.push(`NOTE:${escapeVCard(card.notes)}`);
  }
  lines.push("END:VCARD");
  return lines.map(foldLine).join("\r\n") + "\r\n";
}

function hasAddress(address: PostalAddress): boolean {
  return Boolean(address.street || address.city || address.region || address.postalCode || address.country);
}

function vcardAddress(address: PostalAddress): string {
  return ["", "", address.street, address.city, address.region, address.postalCode, address.country]
    .map(escapeVCard)
    .join(";");
}

function escapeVCard(value: string): string {
  return value
    .replace(/\\/g, "\\\\")
    .replace(/;/g, "\\;")
    .replace(/,/g, "\\,")
    .replace(/\r\n/g, "\\n")
    .replace(/\n/g, "\\n");
}

// --- shared helpers --------------------------------------------------------

function foldLine(line: string): string {
  if (line.length <= 75) {
    return line;
  }
  const parts: string[] = [];
  let rest = line;
  while (rest.length > 75) {
    parts.push(rest.slice(0, 75));
    rest = rest.slice(75);
  }
  parts.push(rest);
  return parts.join("\r\n ");
}

function text(value: string): Uint8Array {
  return new TextEncoder().encode(value);
}

function concat(parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((sum, part) => sum + part.length, 0);
  const out = new Uint8Array(total);
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.length;
  }
  return out;
}

function base64(bytes: Uint8Array): string {
  let binary = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}

function wrapBase64(value: string): string {
  const lines: string[] = [];
  for (let i = 0; i < value.length; i += 76) {
    lines.push(value.slice(i, i + 76));
  }
  return lines.join("\r\n");
}

function hex(bytes: Uint8Array): string {
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

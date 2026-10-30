// A from-scratch reader for Microsoft Personal Folder Files (PST) and Offline
// Storage Tables (OST), shaped like the Go reader in internal/pst.
//
// The browser never sends the archive anywhere: it reads 512 byte pages and
// data blocks through File.slice, so a multi-gigabyte PST can be browsed and
// imported without holding it in memory. The layout is the one described by
// [MS-PST]: a 512 byte header, an offsets B-tree (data identifier to file
// position), a descriptors B-tree (item identifier to data and parent), and
// tables of MAPI record entries.
//
// The 4k-page compressed OST variant is rejected with a clear message: it uses
// DEFLATE and a different index, and a half-parsed mailbox would be worse than
// an honest refusal.

const HEADER_SIZE = 512;
const NODE_SIZE = 512;
const SIGNATURE = "!BDN";

const NODE_TYPE_FOLDER = 0x02;
const NODE_TYPE_SEARCH_FOLDER = 0x03;
const NODE_TYPE_MESSAGE = 0x04;
const NODE_TYPE_ATTACHMENT = 0x05;
const NODE_TYPE_ASSOCIATED_CONTENT = 0x08;

const ATTACHMENTS_TABLE_IDENTIFIER = 0x0671;

const FLAG_INTERNAL = 1 << 1;

/** Random access over a PST: a File in the browser, or a buffer in a test. */
export interface RandomAccess {
  size(): number;
  read(offset: number, length: number): Promise<Uint8Array>;
}

/** Reads a browser File through slice(), one page at a time. */
export class FileAccess implements RandomAccess {
  constructor(private readonly file: File) {}

  size(): number {
    return this.file.size;
  }

  async read(offset: number, length: number): Promise<Uint8Array> {
    const end = Math.min(offset + length, this.file.size);
    if (offset < 0 || end <= offset) {
      return new Uint8Array(0);
    }
    const buffer = await this.file.slice(offset, end).arrayBuffer();
    return new Uint8Array(buffer);
  }
}

/** Reads an in-memory buffer, which is what tests use. */
export class BufferAccess implements RandomAccess {
  constructor(private readonly data: Uint8Array) {}

  size(): number {
    return this.data.length;
  }

  async read(offset: number, length: number): Promise<Uint8Array> {
    return this.data.slice(offset, Math.min(offset + length, this.data.length));
  }
}

export class PstUnsupportedError extends Error {
  constructor(reason: string) {
    super(`pst: ${reason}`);
    this.name = "PstUnsupportedError";
  }
}

export interface OffsetEntry {
  identifier: number;
  fileOffset: number;
  dataSize: number;
  refCount: number;
}

export interface DescriptorEntry {
  identifier: number;
  dataIdentifier: number;
  localDescriptors: number;
  parentIdentifier: number;
}

export interface IndexValue {
  start: number;
  end: number;
}

export interface TableBlock {
  data: Uint8Array;
  index: IndexValue[];
}

export interface PstProperty {
  identifier: number;
  valueType: number;
  text: string;
  data?: Uint8Array;
  int: number;
  time?: Date;
  hasValue: boolean;
}

export interface PstAttachment {
  name: string;
  data: Uint8Array;
}

export interface NamedProperty {
  guid: string;
  lid: number;
  name: string;
}

export type ItemKind =
  | "folder"
  | "search folder"
  | "message"
  | "appointment"
  | "contact"
  | "task"
  | "journal"
  | "item"
  | "attachment";

export interface PstItem {
  id: number;
  parentId: number;
  nodeType: number;
  kind: ItemKind;
  name: string;
  properties: Map<number, PstProperty>;
  attachments: PstAttachment[];
}

interface RecordEntry {
  identifier: number;
  valueType: number;
  raw: number;
}

// Property identifiers this reader interprets.
const PROP_ENTRY_ID = 0x0fff;
const PROP_MESSAGE_CLASS = 0x001a;
const PROP_SUBJECT = 0x0037;
const PROP_BODY = 0x1000;
const PROP_BODY_HTML = 0x1013;
const PROP_TRANSPORT_HEADERS = 0x007d;
const PROP_INTERNET_MESSAGE_ID = 0x1035;
const PROP_INTERNET_REFERENCES = 0x1036;
const PROP_SENDER_NAME = 0x0c1a;
const PROP_SENDER_ADDRESS = 0x0c1f;
const PROP_SENT_REPRESENTING_NAME = 0x0042;
const PROP_SENT_REPRESENTING_ADDRESS = 0x0065;
const PROP_DISPLAY_TO = 0x0e04;
const PROP_DISPLAY_CC = 0x0e03;
const PROP_MESSAGE_FLAGS = 0x0e07;
const PROP_SUBMIT_TIME = 0x0039;
const PROP_DELIVERY_TIME = 0x0019;
const PROP_START_DATE = 0x0060;
const PROP_END_DATE = 0x0061;
const PROP_DISPLAY_NAME = 0x3001;
const PROP_EMAIL_ADDRESS = 0x3003;
const PROP_ATTACH_FILENAME = 0x3704;
const PROP_ATTACH_LONG_FILENAME = 0x3707;
const PROP_ATTACH_DATA = 0x3701;
const PROP_GIVEN_NAME = 0x3a06;
const PROP_SURNAME = 0x3a11;
const PROP_MIDDLE_NAME = 0x3a44;
const PROP_ORGANIZATION = 0x3a16;
const PROP_JOB_TITLE = 0x3a17;
const PROP_BUSINESS_PHONE = 0x3a08;
const PROP_HOME_PHONE = 0x3a09;
const PROP_MOBILE_PHONE = 0x3a1c;
const PROP_NOTES = 0x3004;

export const PROPERTY = {
  entryId: PROP_ENTRY_ID,
  messageClass: PROP_MESSAGE_CLASS,
  subject: PROP_SUBJECT,
  body: PROP_BODY,
  bodyHtml: PROP_BODY_HTML,
  transportHeaders: PROP_TRANSPORT_HEADERS,
  internetMessageId: PROP_INTERNET_MESSAGE_ID,
  internetReferences: PROP_INTERNET_REFERENCES,
  senderName: PROP_SENDER_NAME,
  senderAddress: PROP_SENDER_ADDRESS,
  sentRepresentingName: PROP_SENT_REPRESENTING_NAME,
  sentRepresentingAddress: PROP_SENT_REPRESENTING_ADDRESS,
  displayTo: PROP_DISPLAY_TO,
  displayCc: PROP_DISPLAY_CC,
  messageFlags: PROP_MESSAGE_FLAGS,
  submitTime: PROP_SUBMIT_TIME,
  deliveryTime: PROP_DELIVERY_TIME,
  startDate: PROP_START_DATE,
  endDate: PROP_END_DATE,
  displayName: PROP_DISPLAY_NAME,
  emailAddress: PROP_EMAIL_ADDRESS,
  attachFilename: PROP_ATTACH_FILENAME,
  attachLongFilename: PROP_ATTACH_LONG_FILENAME,
  attachData: PROP_ATTACH_DATA,
  givenName: PROP_GIVEN_NAME,
  surname: PROP_SURNAME,
  middleName: PROP_MIDDLE_NAME,
  organization: PROP_ORGANIZATION,
  jobTitle: PROP_JOB_TITLE,
  businessPhone: PROP_BUSINESS_PHONE,
  homePhone: PROP_HOME_PHONE,
  mobilePhone: PROP_MOBILE_PHONE,
  notes: PROP_NOTES,
} as const;

// Named property constants, matching the Go reader.
export const GUID_APPOINTMENT = "00062002-0000-0000-c000-000000000046";
export const GUID_ADDRESS = "00062004-0000-0000-c000-000000000046";
export const GUID_MEETING = "6ed8da90-450b-101b-98da-00aa003f1305";

export const LID = {
  appointmentStartWhole: 0x820d,
  appointmentEndWhole: 0x820e,
  appointmentSubType: 0x8215,
  location: 0x8208,
  email1Address: 0x8083,
  email2Address: 0x8093,
  email3Address: 0x80a3,
  workAddressStreet: 0x8045,
  workAddressCity: 0x8046,
  workAddressState: 0x8047,
  workAddressPostalCode: 0x8048,
  workAddressCountry: 0x8049,
  globalObjectId: 0x0003,
} as const;

export class PstReader {
  private is64 = false;
  private encryption = 0;
  private offsets = new Map<number, OffsetEntry>();
  private descriptors = new Map<number, DescriptorEntry>();
  private blockCache = new Map<number, TableBlock[]>();
  private nameCache: Map<number, NamedProperty> | null = null;

  constructor(private readonly access: RandomAccess) {}

  static async open(access: RandomAccess): Promise<PstReader> {
    const reader = new PstReader(access);
    await reader.readHeader();
    return reader;
  }

  static async openFile(file: File): Promise<PstReader> {
    return PstReader.open(new FileAccess(file));
  }

  is64Bit(): boolean {
    return this.is64;
  }

  contentType(): "PST" | "OST" | "PAB" {
    return this.contentTypeValue;
  }

  private contentTypeValue: "PST" | "OST" | "PAB" = "PST";

  private async readHeader(): Promise<void> {
    const readSize = Math.min(564, this.access.size());
    const data = await this.access.read(0, readSize);
    if (data.length < HEADER_SIZE || text(data, 0, 4) !== SIGNATURE) {
      throw new PstUnsupportedError("not a Personal Folder File");
    }
    const version = u16(data, 10);
    if (version >= 0x0024) {
      throw new PstUnsupportedError(
        "compressed 4k page OST files are not supported; open the file in Outlook and export it to a .pst first",
      );
    }
    this.is64 = version >= 0x0015;
    if (version <= 0x000f) {
      this.is64 = false;
    }

    const contentType = text(data, 8, 2);
    this.contentTypeValue = contentType === "SO" || contentType === "OS" ? "OST" : contentType === "AB" || contentType === "PA" ? "PAB" : "PST";

    let descriptorsRoot = 0;
    let offsetsRoot = 0;
    if (this.is64) {
      this.encryption = data[513];
      descriptorsRoot = Number(u64(data, 224));
      offsetsRoot = Number(u64(data, 240));
    } else {
      this.encryption = data[461];
      descriptorsRoot = u32(data, 188);
      offsetsRoot = u32(data, 196);
    }
    if (!offsetsRoot || !descriptorsRoot) {
      throw new PstUnsupportedError("the file header has no index root");
    }
    await this.walkIndex(offsetsRoot, true, 0);
    await this.walkIndex(descriptorsRoot, false, 0);
    if (this.offsets.size === 0) {
      throw new PstUnsupportedError("the offsets index is empty");
    }
    if (this.descriptors.size === 0) {
      throw new PstUnsupportedError("the descriptors index is empty");
    }
    // The name-to-id map is small and every appointment and contact needs it,
    // so it is parsed once while opening rather than on first use.
    await this.namedProperties();
  }

  private async walkIndex(offset: number, offsetIndex: boolean, depth: number): Promise<void> {
    if (depth > 12) {
      throw new PstUnsupportedError("index tree deeper than 12 levels");
    }
    const data = await this.access.read(offset, NODE_SIZE);
    if (data.length < NODE_SIZE) {
      throw new PstUnsupportedError(`index node at ${offset} is truncated`);
    }
    const footer = this.is64 ? 488 : 496;
    const count = data[footer];
    const entrySize = data[footer + 2];
    const level = data[footer + 3];
    if (!entrySize) {
      return;
    }
    for (let i = 0; i < count; i++) {
      const at = i * entrySize;
      if (at + entrySize > footer) {
        break;
      }
      if (level === 0) {
        if (offsetIndex) {
          this.offsets.set(Number(u64(data, at)), {
            identifier: Number(u64(data, at)),
            fileOffset: Number(u64(data, at + 8)),
            dataSize: u16(data, at + 16),
            refCount: u16(data, at + 18),
          });
        } else {
          const id = Number(u64(data, at));
          this.descriptors.set(id, {
            identifier: id,
            dataIdentifier: Number(u64(data, at + 8)),
            localDescriptors: Number(u64(data, at + 16)),
            parentIdentifier: u32(data, at + 24),
          });
        }
        continue;
      }
      const child = Number(u64(data, at + 16));
      if (child) {
        await this.walkIndex(child, offsetIndex, depth + 1);
      }
    }
  }

  private async dataBlocks(id: number): Promise<TableBlock[]> {
    const cached = this.blockCache.get(id);
    if (cached) {
      return cached;
    }
    const entry = this.offsets.get(id);
    if (!entry) {
      throw new PstUnsupportedError(`no data block for identifier ${id}`);
    }
    const footerSize = this.is64 ? 16 : 12;
    let blockSize = Math.ceil(entry.dataSize / 64) * 64;
    if (blockSize - entry.dataSize < footerSize) {
      blockSize += 64;
    }
    blockSize = Math.min(blockSize, this.access.size());
    let raw = await this.access.read(entry.fileOffset, blockSize);
    const internal = (entry.identifier & FLAG_INTERNAL) !== 0;
    if (!internal) {
      raw = decryptBlock(raw, this.encryption, entry.identifier);
    }
    if (raw.length < footerSize) {
      throw new PstUnsupportedError(`data block ${id} is smaller than its footer`);
    }
    let payloadSize = u16(raw, raw.length - footerSize);
    if (payloadSize > raw.length - footerSize) {
      payloadSize = raw.length - footerSize;
    }
    const payload = raw.slice(0, payloadSize);

    if (internal && payload.length >= 2 && payload[0] === 0x01 && (payload[1] === 1 || payload[1] === 2)) {
      const count = u16(payload, 2);
      const step = this.is64 ? 8 : 4;
      const entries = payload.subarray(8);
      const out: TableBlock[] = [];
      for (let i = 0; i < count; i++) {
        const at = i * step;
        if (at + step > entries.length) {
          break;
        }
        const sub = this.is64 ? Number(u64(entries, at)) : u32(entries, at);
        if (!sub) {
          continue;
        }
        out.push(...(await this.dataBlocks(sub)));
      }
      this.blockCache.set(id, out);
      return out;
    }

    const out: TableBlock[] = [{ data: payload, index: [] }];
    this.blockCache.set(id, out);
    return out;
  }

  private async tableFor(descriptor: number): Promise<TableBlock[]> {
    const entry = this.descriptors.get(descriptor);
    if (!entry || !entry.dataIdentifier) {
      return [];
    }
    const blocks = await this.dataBlocks(entry.dataIdentifier);
    for (const block of blocks) {
      block.index = parseBlockIndex(block.data);
    }
    return blocks;
  }

  private async properties(descriptor: number): Promise<RecordEntry[]> {
    const blocks = await this.tableFor(descriptor);
    if (!blocks.length || blocks[0].data.length < 4 || blocks[0].data[2] !== 0xec) {
      return [];
    }
    const tableType = blocks[0].data[3];
    const valueRef = u32(blocks[0].data, 4);
    let b5Ref = valueRef;
    if (tableType !== 0x8c && tableType !== 0xa5 && tableType !== 0xbc) {
      const header = tableValue(blocks, valueRef);
      if (!header || header.length < 12) {
        return [];
      }
      switch (tableType) {
        case 0x6c:
        case 0x9c:
          b5Ref = u32(header, 0);
          break;
        case 0x7c:
        case 0xac:
          b5Ref = u32(header, 10);
          break;
        default:
          return [];
      }
    }
    const b5 = tableValue(blocks, b5Ref);
    if (!b5 || b5.length < 8 || b5[0] !== 0xb5) {
      return [];
    }
    return readRecordEntries(blocks, u32(b5, 4), b5[3], b5[1], b5[2], 0);
  }

  private async localDescriptorBlocks(descriptor: number, identifier: number): Promise<TableBlock[]> {
    const entry = this.descriptors.get(descriptor);
    if (!entry || !entry.localDescriptors) {
      return [];
    }
    const blocks = await this.dataBlocks(entry.localDescriptors);
    for (const block of blocks) {
      const found = await this.findLocalDescriptor(block.data, identifier);
      if (found) {
        return this.dataBlocks(found);
      }
    }
    return [];
  }

  private async findLocalDescriptor(data: Uint8Array, want: number): Promise<number> {
    if (data.length < 4 || data[0] !== 0x02) {
      return 0;
    }
    const level = data[1];
    const count = u16(data, 2);
    const nodeHeader = this.is64 ? 8 : 4;
    let entrySize = this.is64 ? 16 : 8;
    if (level === 0) {
      entrySize = this.is64 ? 24 : 12;
    }
    const readId = (at: number) => (this.is64 ? Number(u64(data, at)) : u32(data, at));
    const readSub = (at: number) => (this.is64 ? Number(u64(data, at + nodeHeader)) : u32(data, at + nodeHeader));

    for (let i = 0; i < count; i++) {
      const at = nodeHeader + i * entrySize;
      if (at + entrySize > data.length) {
        break;
      }
      const id = readId(at);
      if (level === 0) {
        if (id === want) {
          return readSub(at);
        }
        if (id > want) {
          return 0;
        }
        continue;
      }

      // A branch entry covers identifiers from its own up to the next one's.
      let next = 0xffffffff;
      const nextAt = nodeHeader + (i + 1) * entrySize;
      if (nextAt + entrySize <= data.length) {
        next = readId(nextAt);
      }
      if (want < id || want >= next) {
        continue;
      }
      const blocks = await this.dataBlocks(readSub(at));
      for (const block of blocks) {
        const found = await this.findLocalDescriptor(block.data, want);
        if (found) {
          return found;
        }
      }
      return 0;
    }
    return 0;
  }

  private async localDescriptorData(descriptor: number, identifier: number): Promise<Uint8Array | null> {
    const blocks = await this.localDescriptorBlocks(descriptor, identifier);
    if (!blocks.length) {
      return null;
    }
    return concat(blocks.map((block) => block.data));
  }

  private async propertyValue(descriptor: number, entry: RecordEntry): Promise<PstProperty> {
    const prop: PstProperty = {
      identifier: entry.identifier,
      valueType: entry.valueType,
      text: "",
      int: 0,
      hasValue: false,
    };
    switch (entry.valueType) {
      case 0x0002:
      case 0x0003:
      case 0x0004:
      case 0x000a:
        prop.int = (entry.raw << 16) >> 16;
        prop.hasValue = true;
        return prop;
      case 0x000b:
        prop.int = entry.raw & 0xff;
        prop.hasValue = true;
        return prop;
    }
    if (!entry.raw) {
      return prop;
    }
    let data: Uint8Array | null;
    if ((entry.raw & 0x1f) !== 0) {
      data = await this.localDescriptorData(descriptor, entry.raw);
    } else {
      const blocks = await this.tableFor(descriptor);
      data = tableValue(blocks, entry.raw);
    }
    if (!data || !data.length) {
      return prop;
    }
    switch (entry.valueType) {
      case 0x001e:
        prop.text = trimAscii(data);
        break;
      case 0x001f:
        prop.text = trimUtf16(data);
        break;
      case 0x0040:
      case 0x0007:
        if (data.length >= 8) {
          prop.time = filetimeToDate(u64(data, 0));
        }
        break;
      default:
        prop.data = data;
        break;
    }
    prop.hasValue = true;
    return prop;
  }

  async items(): Promise<PstItem[]> {
    const items: PstItem[] = [];
    for (const [id, entry] of this.descriptors) {
      const nodeType = id & 0x1f;
      if (
        nodeType !== NODE_TYPE_FOLDER &&
        nodeType !== NODE_TYPE_SEARCH_FOLDER &&
        nodeType !== NODE_TYPE_MESSAGE &&
        nodeType !== NODE_TYPE_ASSOCIATED_CONTENT &&
        nodeType !== NODE_TYPE_ATTACHMENT
      ) {
        continue;
      }
      const entries = await this.properties(id);
      const properties = new Map<number, PstProperty>();
      for (const record of entries) {
        const value = await this.propertyValue(id, record);
        if (value.hasValue) {
          properties.set(record.identifier, value);
        }
      }
      const item: PstItem = {
        id,
        parentId: entry.parentIdentifier,
        nodeType,
        kind: itemKind(nodeType, properties.get(PROP_MESSAGE_CLASS)?.text ?? ""),
        name: "",
        properties,
        attachments: [],
      };
      item.name = itemName(item);
      if (item.kind === "message") {
        item.attachments = await this.attachments(id);
      }
      items.push(item);
    }
    return items;
  }

  private async attachments(message: number): Promise<PstAttachment[]> {
    const blocks = await this.localDescriptorBlocks(message, ATTACHMENTS_TABLE_IDENTIFIER);
    if (!blocks.length || blocks[0].data.length < 4 || blocks[0].data[2] !== 0xec || blocks[0].data[3] !== 0x8c) {
      return [];
    }
    const b5 = tableValue(blocks, u32(blocks[0].data, 4));
    if (!b5 || b5.length < 8 || b5[0] !== 0xb5) {
      return [];
    }
    const entries = readRecordEntries(blocks, u32(b5, 4), b5[3], b5[1], b5[2], 0);
    const out: PstAttachment[] = [];
    for (const entry of entries) {
      if (!entry.raw) {
        continue;
      }
      const data = await this.localDescriptorData(message, entry.raw);
      if (!data || data.length < 4 || data[2] !== 0xec) {
        continue;
      }
      const attachmentBlocks: TableBlock[] = [{ data, index: parseBlockIndex(data) }];
      const attachmentEntries = await this.propertiesFromBlocks(attachmentBlocks);
      let name = "";
      let body: Uint8Array | null = null;
      for (const prop of attachmentEntries) {
        const value = await this.propertyValue(message, prop);
        if (!value.hasValue) {
          continue;
        }
        if (prop.identifier === PROP_ATTACH_FILENAME || prop.identifier === PROP_ATTACH_LONG_FILENAME) {
          name = value.text;
        } else if (prop.identifier === PROP_ATTACH_DATA) {
          body = value.data ?? null;
        }
      }
      if (body && body.length) {
        out.push({ name, data: body });
      }
    }
    return out;
  }

  private async propertiesFromBlocks(blocks: TableBlock[]): Promise<RecordEntry[]> {
    if (!blocks.length || blocks[0].data.length < 4 || blocks[0].data[2] !== 0xec) {
      return [];
    }
    const tableType = blocks[0].data[3];
    let b5Ref = u32(blocks[0].data, 4);
    if (tableType !== 0x8c && tableType !== 0xa5 && tableType !== 0xbc) {
      const header = tableValue(blocks, b5Ref);
      if (!header || header.length < 12) {
        return [];
      }
      b5Ref = tableType === 0x7c || tableType === 0xac ? u32(header, 10) : u32(header, 0);
    }
    const b5 = tableValue(blocks, b5Ref);
    if (!b5 || b5.length < 8 || b5[0] !== 0xb5) {
      return [];
    }
    return readRecordEntries(blocks, u32(b5, 4), b5[3], b5[1], b5[2], 0);
  }

  async namedProperties(): Promise<Map<number, NamedProperty>> {
    if (this.nameCache) {
      return this.nameCache;
    }
    const out = new Map<number, NamedProperty>();
    const entries = await this.properties(0x61);
    let classes: Uint8Array = new Uint8Array(0);
    let table: Uint8Array = new Uint8Array(0);
    let strings: Uint8Array = new Uint8Array(0);
    for (const entry of entries) {
      const value = await this.propertyValue(0x61, entry);
      if (entry.identifier === 0x0002 && value.data) {
        classes = value.data;
      } else if (entry.identifier === 0x0003 && value.data) {
        table = value.data;
      } else if (entry.identifier === 0x0004 && value.data) {
        strings = value.data;
      }
    }
    const guids: string[] = [];
    for (let at = 0; at + 16 <= classes.length; at += 16) {
      guids.push(formatGuid(classes.subarray(at, at + 16)));
    }
    for (let at = 0; at + 8 <= table.length; at += 8) {
      const value = u32(table, at);
      const entryType = u16(table, at + 4);
      const number = u16(table, at + 6);
      let guid = "";
      if (entryType >= 6) {
        guid = guids[Math.floor(entryType / 2) - 3] ?? "";
      }
      if ((entryType & 1) === 1) {
        out.set(0x8000 + number, { guid, lid: 0, name: readMapiString(strings, value) });
      } else {
        out.set(0x8000 + number, { guid, lid: value, name: "" });
      }
    }
    this.nameCache = out;
    return out;
  }

  namedProperty(id: number): NamedProperty | undefined {
    return this.nameCache?.get(id);
  }

  folderPath(items: PstItem[], id: number): string {
    const byId = new Map<number, PstItem>(items.map((item) => [item.id, item]));
    const parts: string[] = [];
    const seen = new Set<number>();
    let current = id;
    while (current && !seen.has(current)) {
      seen.add(current);
      const item = byId.get(current);
      if (!item) {
        break;
      }
      if (item.name) {
        parts.push(item.name);
      }
      current = item.parentId;
    }
    return parts.reverse().join("/");
  }
}

function itemKind(nodeType: number, messageClass: string): ItemKind {
  if (nodeType === NODE_TYPE_FOLDER) {
    return "folder";
  }
  if (nodeType === NODE_TYPE_SEARCH_FOLDER) {
    return "search folder";
  }
  if (nodeType === NODE_TYPE_ATTACHMENT) {
    return "attachment";
  }
  if (messageClass.startsWith("IPM.Contact")) {
    return "contact";
  }
  if (messageClass.startsWith("IPM.Appointment") || messageClass.startsWith("IPM.OnlineMeeting")) {
    return "appointment";
  }
  if (messageClass.startsWith("IPM.Task")) {
    return "task";
  }
  if (messageClass.startsWith("IPM.Journal")) {
    return "journal";
  }
  if (messageClass.startsWith("IPM.Note")) {
    return "message";
  }
  if (
    messageClass.startsWith("IPM.Configuration") ||
    messageClass.startsWith("IPM.Microsoft") ||
    messageClass.startsWith("IPM.OLE") ||
    messageClass.startsWith("IPM.Outlook") ||
    messageClass.startsWith("IPM.Search") ||
    messageClass.startsWith("IPM.Activity") ||
    messageClass.startsWith("IPM.Abch")
  ) {
    // Outlook's own bookkeeping records, not mail.
    return "item";
  }
  return "message";
}

function itemName(item: PstItem): string {
  return (
    item.properties.get(PROP_DISPLAY_NAME)?.text ||
    item.properties.get(PROP_SUBJECT)?.text ||
    item.properties.get(PROP_ATTACH_FILENAME)?.text ||
    ""
  );
}

function parseBlockIndex(data: Uint8Array): IndexValue[] {
  if (data.length < 4) {
    return [];
  }
  const indexOffset = u16(data, 0);
  if (!indexOffset || indexOffset + 4 > data.length) {
    return [];
  }
  const count = u16(data, indexOffset);
  if (!count || indexOffset + 4 + (count + 1) * 2 > data.length) {
    return [];
  }
  const boundaries: number[] = [];
  for (let i = 0; i <= count; i++) {
    boundaries.push(u16(data, indexOffset + 4 + i * 2));
  }
  const values: IndexValue[] = [];
  for (let i = 0; i < count; i++) {
    if (boundaries[i] > boundaries[i + 1] || boundaries[i + 1] > data.length) {
      continue;
    }
    values.push({ start: boundaries[i], end: boundaries[i + 1] });
  }
  return values;
}

function tableValue(blocks: TableBlock[], ref: number): Uint8Array | null {
  if (ref & 0x1f) {
    return null;
  }
  const arrayEntry = ref >>> 16;
  const valueIndex = ((ref & 0x0000ffe0) >>> 5) - 1;
  const block = blocks[arrayEntry];
  if (!block) {
    return null;
  }
  const value = block.index[valueIndex];
  if (!value || value.end > block.data.length) {
    return null;
  }
  return block.data.subarray(value.start, value.end);
}

function readRecordEntries(
  blocks: TableBlock[],
  ref: number,
  level: number,
  idSize: number,
  valueSize: number,
  depth: number,
): RecordEntry[] {
  if (!ref || depth > 8) {
    return [];
  }
  const data = tableValue(blocks, ref);
  if (!data) {
    return [];
  }
  if (level === 0) {
    const entrySize = idSize + valueSize;
    if (entrySize <= 0 || data.length < entrySize) {
      return [];
    }
    const out: RecordEntry[] = [];
    for (let at = 0; at + entrySize <= data.length; at += entrySize) {
      let identifier = 0;
      if (idSize === 2) {
        identifier = u16(data, at);
      } else if (idSize === 4) {
        identifier = u32(data, at);
      } else if (idSize === 8) {
        identifier = Number(u64(data, at) & 0xffffffffn);
      } else if (idSize === 16) {
        identifier = u32(data, at);
      } else {
        return out;
      }
      const raw = u32(data, at + entrySize - 4);
      const valueType = idSize === 2 ? u16(data, at + 2) : 0;
      out.push({ identifier, valueType, raw });
    }
    return out;
  }
  const entrySize = idSize + 4;
  const out: RecordEntry[] = [];
  for (let at = 0; at + entrySize <= data.length; at += entrySize) {
    const subRef = u32(data, at + idSize);
    out.push(...readRecordEntries(blocks, subRef, level - 1, idSize, valueSize, depth + 1));
  }
  return out;
}

function readMapiString(data: Uint8Array, offset: number): string {
  if (offset < 0 || offset + 4 > data.length) {
    return "";
  }
  const length = u32(data, offset);
  const start = offset + 4;
  if (!length || start + length > data.length) {
    return "";
  }
  const raw = data.subarray(start, start + length);
  let utf16 = false;
  for (let i = 1; i < raw.length; i += 2) {
    if (raw[i] === 0) {
      utf16 = true;
      break;
    }
  }
  if (utf16) {
    const units: number[] = [];
    for (let i = 0; i + 1 < raw.length; i += 2) {
      units.push(u16(raw, i));
    }
    return String.fromCharCode(...trimNuls(units));
  }
  return trimAscii(raw);
}

function trimNuls(units: number[]): number[] {
  while (units.length && units[units.length - 1] === 0) {
    units.pop();
  }
  return units;
}

// --- format helpers --------------------------------------------------------

function u16(data: Uint8Array, offset: number): number {
  return data[offset] | (data[offset + 1] << 8);
}

function u32(data: Uint8Array, offset: number): number {
  return (data[offset] | (data[offset + 1] << 8) | (data[offset + 2] << 16) | (data[offset + 3] << 24)) >>> 0;
}

function u64(data: Uint8Array, offset: number): bigint {
  let value = 0n;
  for (let i = 7; i >= 0; i--) {
    value = (value << 8n) | BigInt(data[offset + i]);
  }
  return value;
}

function text(data: Uint8Array, offset: number, length: number): string {
  return String.fromCharCode(...data.subarray(offset, offset + length));
}

function trimAscii(data: Uint8Array): string {
  let end = data.length;
  while (end > 0 && data[end - 1] === 0) {
    end--;
  }
  return new TextDecoder("utf-8").decode(data.subarray(0, end));
}

function trimUtf16(data: Uint8Array): string {
  const units: number[] = [];
  for (let i = 0; i + 1 < data.length; i += 2) {
    const unit = u16(data, i);
    if (unit === 0) {
      break;
    }
    units.push(unit);
  }
  return String.fromCharCode(...units);
}

function filetimeToDate(ticks: bigint): Date | undefined {
  if (ticks === 0n) {
    return undefined;
  }
  const seconds = Number(ticks / 10000000n) - 11644473600;
  return new Date(seconds * 1000);
}

function formatGuid(guid: Uint8Array): string {
  if (guid.length < 16) {
    return "";
  }
  const hex = (n: number, width: number) => n.toString(16).padStart(width, "0");
  return `${hex(u32(guid, 0), 8)}-${hex(u16(guid, 4), 4)}-${hex(u16(guid, 6), 4)}-${hex(guid[8], 2)}${hex(
    guid[9],
    2,
  )}-${hex(guid[10], 2)}${hex(guid[11], 2)}${hex(guid[12], 2)}${hex(guid[13], 2)}${hex(guid[14], 2)}${hex(
    guid[15],
    2,
  )}`;
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

// The obfuscation tables from [MS-PST].
const COMPRESSIBLE = new Uint8Array([
  0x47, 0xf1, 0xb4, 0xe6, 0x0b, 0x6a, 0x72, 0x48, 0x85, 0x4e, 0x9e, 0xeb, 0xe2, 0xf8, 0x94, 0x53,
  0xe0, 0xbb, 0xa0, 0x02, 0xe8, 0x5a, 0x09, 0xab, 0xdb, 0xe3, 0xba, 0xc6, 0x7c, 0xc3, 0x10, 0xdd,
  0x39, 0x05, 0x96, 0x30, 0xf5, 0x37, 0x60, 0x82, 0x8c, 0xc9, 0x13, 0x4a, 0x6b, 0x1d, 0xf3, 0xfb,
  0x8f, 0x26, 0x97, 0xca, 0x91, 0x17, 0x01, 0xc4, 0x32, 0x2d, 0x6e, 0x31, 0x95, 0xff, 0xd9, 0x23,
  0xd1, 0x00, 0x5e, 0x79, 0xdc, 0x44, 0x3b, 0x1a, 0x28, 0xc5, 0x61, 0x57, 0x20, 0x90, 0x3d, 0x83,
  0xb9, 0x43, 0xbe, 0x67, 0xd2, 0x46, 0x42, 0x76, 0xc0, 0x6d, 0x5b, 0x7e, 0xb2, 0x0f, 0x16, 0x29,
  0x3c, 0xa9, 0x03, 0x54, 0x0d, 0xda, 0x5d, 0xdf, 0xf6, 0xb7, 0xc7, 0x62, 0xcd, 0x8d, 0x06, 0xd3,
  0x69, 0x5c, 0x86, 0xd6, 0x14, 0xf7, 0xa5, 0x66, 0x75, 0xac, 0xb1, 0xe9, 0x45, 0x21, 0x70, 0x0c,
  0x87, 0x9f, 0x74, 0xa4, 0x22, 0x4c, 0x6f, 0xbf, 0x1f, 0x56, 0xaa, 0x2e, 0xb3, 0x78, 0x33, 0x50,
  0xb0, 0xa3, 0x92, 0xbc, 0xcf, 0x19, 0x1c, 0xa7, 0x63, 0xcb, 0x1e, 0x4d, 0x3e, 0x4b, 0x1b, 0x9b,
  0x4f, 0xe7, 0xf0, 0xee, 0xad, 0x3a, 0xb5, 0x59, 0x04, 0xea, 0x40, 0x55, 0x25, 0x51, 0xe5, 0x7a,
  0x89, 0x38, 0x68, 0x52, 0x7b, 0xfc, 0x27, 0xae, 0xd7, 0xbd, 0xfa, 0x07, 0xf4, 0xcc, 0x8e, 0x5f,
  0xef, 0x35, 0x9c, 0x84, 0x2b, 0x15, 0xd5, 0x77, 0x34, 0x49, 0xb6, 0x12, 0x0a, 0x7f, 0x71, 0x88,
  0xfd, 0x9d, 0x18, 0x41, 0x7d, 0x93, 0xd8, 0x58, 0x2c, 0xce, 0xfe, 0x24, 0xaf, 0xde, 0xb8, 0x36,
  0xc8, 0xa1, 0x80, 0xa6, 0x99, 0x98, 0xa8, 0x2f, 0x0e, 0x81, 0x65, 0x73, 0xe4, 0xc2, 0xa2, 0x8a,
  0xd4, 0xe1, 0x11, 0xd0, 0x08, 0x8b, 0x2a, 0xf2, 0xed, 0x9a, 0x64, 0x3f, 0xc1, 0x6c, 0xf9, 0xec,
]);

const HIGH1 = new Uint8Array([
  0x41, 0x36, 0x13, 0x62, 0xa8, 0x21, 0x6e, 0xbb, 0xf4, 0x16, 0xcc, 0x04, 0x7f, 0x64, 0xe8, 0x5d,
  0x1e, 0xf2, 0xcb, 0x2a, 0x74, 0xc5, 0x5e, 0x35, 0xd2, 0x95, 0x47, 0x9e, 0x96, 0x2d, 0x9a, 0x88,
  0x4c, 0x7d, 0x84, 0x3f, 0xdb, 0xac, 0x31, 0xb6, 0x48, 0x5f, 0xf6, 0xc4, 0xd8, 0x39, 0x8b, 0xe7,
  0x23, 0x3b, 0x38, 0x8e, 0xc8, 0xc1, 0xdf, 0x25, 0xb1, 0x20, 0xa5, 0x46, 0x60, 0x4e, 0x9c, 0xfb,
  0xaa, 0xd3, 0x56, 0x51, 0x45, 0x7c, 0x55, 0x00, 0x07, 0xc9, 0x2b, 0x9d, 0x85, 0x9b, 0x09, 0xa0,
  0x8f, 0xad, 0xb3, 0x0f, 0x63, 0xab, 0x89, 0x4b, 0xd7, 0xa7, 0x15, 0x5a, 0x71, 0x66, 0x42, 0xbf,
  0x26, 0x4a, 0x6b, 0x98, 0xfa, 0xea, 0x77, 0x53, 0xb2, 0x70, 0x05, 0x2c, 0xfd, 0x59, 0x3a, 0x86,
  0x7e, 0xce, 0x06, 0xeb, 0x82, 0x78, 0x57, 0xc7, 0x8d, 0x43, 0xaf, 0xb4, 0x1c, 0xd4, 0x5b, 0xcd,
  0xe2, 0xe9, 0x27, 0x4f, 0xc3, 0x08, 0x72, 0x80, 0xcf, 0xb0, 0xef, 0xf5, 0x28, 0x6d, 0xbe, 0x30,
  0x4d, 0x34, 0x92, 0xd5, 0x0e, 0x3c, 0x22, 0x32, 0xe5, 0xe4, 0xf9, 0x9f, 0xc2, 0xd1, 0x0a, 0x81,
  0x12, 0xe1, 0xee, 0x91, 0x83, 0x76, 0xe3, 0x97, 0xe6, 0x61, 0x8a, 0x17, 0x79, 0xa4, 0xb7, 0xdc,
  0x90, 0x7a, 0x5c, 0x8c, 0x02, 0xa6, 0xca, 0x69, 0xde, 0x50, 0x1a, 0x11, 0x93, 0xb9, 0x52, 0x87,
  0x58, 0xfc, 0xed, 0x1d, 0x37, 0x49, 0x1b, 0x6a, 0xe0, 0x29, 0x33, 0x99, 0xbd, 0x6c, 0xd9, 0x94,
  0xf3, 0x40, 0x54, 0x6f, 0xf0, 0xc6, 0x73, 0xb8, 0xd6, 0x3e, 0x65, 0x18, 0x44, 0x1f, 0xdd, 0x67,
  0x10, 0xf1, 0x0c, 0x19, 0xec, 0xae, 0x03, 0xa1, 0x14, 0x7b, 0xa9, 0x0b, 0xff, 0xf8, 0xa3, 0xc0,
  0xa2, 0x01, 0xf7, 0x2e, 0xbc, 0x24, 0x68, 0x75, 0x0d, 0xfe, 0xba, 0x2f, 0xb5, 0xd0, 0xda, 0x3d,
]);

const HIGH2 = new Uint8Array([
  0x14, 0x53, 0x0f, 0x56, 0xb3, 0xc8, 0x7a, 0x9c, 0xeb, 0x65, 0x48, 0x17, 0x16, 0x15, 0x9f, 0x02,
  0xcc, 0x54, 0x7c, 0x83, 0x00, 0x0d, 0x0c, 0x0b, 0xa2, 0x62, 0xa8, 0x76, 0xdb, 0xd9, 0xed, 0xc7,
  0xc5, 0xa4, 0xdc, 0xac, 0x85, 0x74, 0xd6, 0xd0, 0xa7, 0x9b, 0xae, 0x9a, 0x96, 0x71, 0x66, 0xc3,
  0x63, 0x99, 0xb8, 0xdd, 0x73, 0x92, 0x8e, 0x84, 0x7d, 0xa5, 0x5e, 0xd1, 0x5d, 0x93, 0xb1, 0x57,
  0x51, 0x50, 0x80, 0x89, 0x52, 0x94, 0x4f, 0x4e, 0x0a, 0x6b, 0xbc, 0x8d, 0x7f, 0x6e, 0x47, 0x46,
  0x41, 0x40, 0x44, 0x01, 0x11, 0xcb, 0x03, 0x3f, 0xf7, 0xf4, 0xe1, 0xa9, 0x8f, 0x3c, 0x3a, 0xf9,
  0xfb, 0xf0, 0x19, 0x30, 0x82, 0x09, 0x2e, 0xc9, 0x9d, 0xa0, 0x86, 0x49, 0xee, 0x6f, 0x4d, 0x6d,
  0xc4, 0x2d, 0x81, 0x34, 0x25, 0x87, 0x1b, 0x88, 0xaa, 0xfc, 0x06, 0xa1, 0x12, 0x38, 0xfd, 0x4c,
  0x42, 0x72, 0x64, 0x13, 0x37, 0x24, 0x6a, 0x75, 0x77, 0x43, 0xff, 0xe6, 0xb4, 0x4b, 0x36, 0x5c,
  0xe4, 0xd8, 0x35, 0x3d, 0x45, 0xb9, 0x2c, 0xec, 0xb7, 0x31, 0x2b, 0x29, 0x07, 0x68, 0xa3, 0x0e,
  0x69, 0x7b, 0x18, 0x9e, 0x21, 0x39, 0xbe, 0x28, 0x1a, 0x5b, 0x78, 0xf5, 0x23, 0xca, 0x2a, 0xb0,
  0xaf, 0x3e, 0xfe, 0x04, 0x8c, 0xe7, 0xe5, 0x98, 0x32, 0x95, 0xd3, 0xf6, 0x4a, 0xe8, 0xa6, 0xea,
  0xe9, 0xf3, 0xd5, 0x2f, 0x70, 0x20, 0xf2, 0x1f, 0x05, 0x67, 0xad, 0x55, 0x10, 0xce, 0xcd, 0xe3,
  0x27, 0x3b, 0xda, 0xba, 0xd7, 0xc2, 0x26, 0xd4, 0x91, 0x1d, 0xd2, 0x1c, 0x22, 0x33, 0xf8, 0xfa,
  0xf1, 0x5a, 0xef, 0xcf, 0x90, 0xb6, 0x8b, 0xb5, 0xbd, 0xc0, 0xbf, 0x08, 0x97, 0x1e, 0x6c, 0xe2,
  0x61, 0xe0, 0xc6, 0xc1, 0x59, 0xab, 0xbb, 0x58, 0xde, 0x5f, 0xdf, 0x60, 0x79, 0x7e, 0xb2, 0x8a,
]);

function decryptBlock(data: Uint8Array, encryption: number, key: number): Uint8Array {
  if (encryption === 0) {
    return data;
  }
  const out = new Uint8Array(data.length);
  for (let i = 0; i < data.length; i++) {
    out[i] = COMPRESSIBLE[data[i]];
  }
  if (encryption !== 2) {
    return out;
  }
  let salt = (((key >>> 16) ^ (key & 0xffff)) & 0xffff) >>> 0;
  for (let i = 0; i < out.length; i++) {
    let value = out[i];
    value = (value + (salt & 0xff)) & 0xff;
    value = HIGH1[value];
    value = (value + ((salt >>> 8) & 0xff)) & 0xff;
    value = HIGH2[value];
    value = (value - ((salt >>> 8) & 0xff)) & 0xff;
    value = COMPRESSIBLE[value];
    value = (value - (salt & 0xff)) & 0xff;
    out[i] = value;
    salt = (salt + 1) & 0xffff;
  }
  return out;
}

// Shapes the /api/v1/mail endpoints exchange. They mirror the JSON the Go
// server emits, so a rename on either side breaks the build here first.

export interface Address {
  address: string;
  name?: string;
}

export interface Folder {
  id: number;
  name: string;
  parent_id?: number;
  special_use?: string;
  subscribed: boolean;
  total: number;
  unread: number;
  flagged: number;
  deletable: boolean;
  // owner is the mailbox address this folder belongs to. It is empty for the
  // signed-in user's own mailbox, and set when the folder is in a shared
  // mailbox.
  owner?: string;
}

export interface Attachment {
  index: number;
  filename: string;
  mime_type: string;
  size: number;
  inline: boolean;
  content_id?: string;
}

export interface InlineImage {
  content_id: string;
  filename?: string;
  mime_type: string;
  size: number;
}

export interface Header {
  name: string;
  value: string;
}

// MessageSummary is one row of a folder listing. Its "from" is the
// denormalised sender string, which is all a list row has to render.
export interface MessageSummary {
  id: number;
  folder_id: number;
  uid: number;
  subject: string;
  from: string;
  from_name: string;
  snippet: string;
  date: string;
  size: number;
  flags: string[];
  has_attachments: boolean;
  thread_key: string;
}

// MessageDetail is the full message. Its "from" is the parsed address list,
// which shadows the summary field of the same name.
export interface MessageDetail extends Omit<MessageSummary, 'from'> {
  from: Address[];
  to: Address[];
  cc: Address[];
  bcc?: Address[];
  reply_to?: Address[];
  text_body?: string;
  html_body?: string;
  attachments: Attachment[];
  inline_images?: InlineImage[];
  headers?: Header[];
}

export interface Cursor {
  date: string;
  id: number;
}

export interface Page {
  messages: MessageSummary[];
  total: number;
  has_more: boolean;
  next_cursor?: Cursor;
}

export interface Thread {
  thread_key: string;
  subject: string;
  message_count: number;
  unread_count: number;
  latest: MessageSummary;
}

export interface ComposeAttachmentInput {
  filename: string;
  mime_type: string;
  content: string;
  content_id?: string;
}

export interface ComposeRequest {
  from?: string;
  to: Address[];
  cc: Address[];
  bcc: Address[];
  subject: string;
  text_body?: string;
  html_body?: string;
  reply_to?: Address[];
  in_reply_to?: string;
  references?: string[];
  draft_id?: number;
  attachments?: ComposeAttachmentInput[];
}

export interface SendResult {
  outbox_id: number;
  message_id: string;
  sent_message_id?: number;
}

export interface RuleCondition {
  field: string;
  operator: string;
  value?: string;
  header?: string;
  case_sensitive?: boolean;
  negate?: boolean;
}

export interface RuleAction {
  type: string;
  target?: string;
  stop?: boolean;
}

export interface Rule {
  id: number;
  name: string;
  enabled: boolean;
  priority: number;
  conditions: RuleCondition[];
  actions: RuleAction[];
  stop: boolean;
}

export interface OwnAlias {
  address: string;
  target: string;
  enabled: boolean;
}

export interface SharedMailbox {
  address: string;
  display_name: string;
  level: string;
  writable: boolean;
}

export interface ImportJob {
  id: number;
  owner: string;
  started_by: string;
  remote_host: string;
  remote_port: number;
  remote_tls: string;
  remote_username: string;
  accept_self_signed: boolean;
  source_root: string;
  target_root: string;
  status: 'queued' | 'running' | 'completed' | 'cancelled' | 'failed';
  cancel_requested: boolean;
  last_error?: string;
  folders_total: number;
  folders_done: number;
  messages_seen: number;
  messages_imported: number;
  messages_skipped: number;
  bytes_imported: number;
  created_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface ImportJobRequest {
  remote_host: string;
  remote_port?: number;
  remote_tls?: string;
  remote_username: string;
  remote_password: string;
  accept_self_signed?: boolean;
  source_root?: string;
  target_root?: string;
}

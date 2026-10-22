// The administration API's wire shapes. They mirror the view types the Go
// server writes out in internal/httpapi/admin.go, so a rename on either side
// breaks this file first rather than showing up as an empty table.
//
// The server writes these types out by hand instead of serialising its row
// structs, so these names are the contract and are expected to be stable.
export interface Domain {
  name: string;
  status: 'active' | 'suspended' | 'disabled';
  is_local: boolean;
  description: string;
  default_quota_bytes: number;
  daily_send_limit?: number | null;
  dkim_selector?: string | null;
  contact_group_id?: number | null;
  workgroup_id?: number | null;
}

export type UserSource = 'local' | 'ldap' | 'shared' | 'domain_admin';

export interface User {
  address: string;
  address_local: string;
  source: UserSource;
  display_name: string;
  enabled: boolean;
  mailbox_enabled: boolean;
  // Whether this user may use the administration pages. It is a grant and not a
  // kind of account: a local or LDAP user who receives mail can also be an
  // administrator, which is the ordinary shape in a small deployment. Every
  // domain_admin row has it and no shared mailbox ever can, both enforced by
  // constraints on the server.
  is_domain_admin: boolean;
  quota_bytes: number | null;
  daily_send_limit: number | null;
  timezone: string;
  language: string;
  must_change_password: boolean;
  created_at: string;
  quota_used_bytes: number;
  quota_message_count: number;
}

export type AliasTargetKind = 'user' | 'shared' | 'workgroup';

export interface AliasTarget {
  target_kind: AliasTargetKind;
  target_address: string | null;
  workgroup_id: number | null;
  position: number;
}

export interface Alias {
  address: string;
  targets: AliasTarget[];
  display_name: string;
  description: string;
  enabled: boolean;
}

export interface SharedMailboxGrant {
  shared_address: string;
  member_address: string;
  // The levels the server accepts. Anything else is refused by
  // policy.GrantLevel.Valid(), so the UI must not invent its own names.
  level: 'read' | 'contribute' | 'editor' | 'owner';
}

export interface SharedMailbox {
  address: string;
  display_name: string;
  description: string;
  colour: string | null;
  open_access: boolean;
  auto_reply_subject: string | null;
  auto_reply_body: string | null;
  auto_reply_expires_at: string | null;
  members: SharedMailboxGrant[];
}

export interface ImpersonationGrant {
  actor_address: string;
  target_address: string;
  reason: string;
  active: boolean;
  expires_at: string | null;
  created_at: string;
}

export interface AuditEntry {
  id: number;
  actor_address: string | null;
  target_address: string | null;
  action: string;
  target_type: string;
  target_id: string;
  ip: string | null;
  detail: unknown;
  created_at: string;
}

// OutboxMessageState mirrors the server's outbox_state enum: what the delivery
// worker is doing with a message overall.
export type OutboxMessageState = 'queued' | 'sending' | 'sent' | 'deferred' | 'failed' | 'bounced';

// OutboxRecipientState mirrors recipient_state: each address settles on its
// own, which is how one message can be half delivered and half refused.
export type OutboxRecipientState = 'pending' | 'delivered' | 'deferred' | 'failed';

// OutboxMessageView is one message in the outbound queue. The two counts are
// what a list is read for; recipients arrive only from the detail endpoint.
export interface OutboxMessageView {
  id: number;
  submitted_by: string;
  envelope_from: string;
  size: number;
  state: OutboxMessageState;
  attempts: number;
  next_attempt_at: string;
  last_error?: string;
  recipient_count: number;
  delivered_count: number;
  created_at: string;
  sent_at?: string;
  failed_at?: string;
  dsn_at?: string;
  recipients?: OutboxRecipientView[];
}

// OutboxRecipientView is one envelope recipient and its own fate. A message
// may have been delivered to two addresses and refused by the third, and the
// bounce that explains it names exactly those — never the ones that worked.
export interface OutboxRecipientView {
  position: number;
  address: string;
  kind: string;
  state: OutboxRecipientState;
  attempts: number;
  last_error?: string;
  delivered_at?: string;
}

// Quota is rendered from bytes everywhere in the server, and the admin panel is
// where an operator checks it, so the conversion lives in one place rather than
// being repeated in every template.
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined || bytes < 0) return 'unlimited';
  if (bytes === 0) return 'none';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const rounded = unit === 0 ? String(value) : value.toFixed(value < 10 ? 1 : 0);
  return `${rounded} ${units[unit]}`;
}

export interface ContactGroup {
  id: number;
  name: string;
  description: string;
  created_at: string;
}

export interface Workgroup {
  id: number;
  name: string;
  domain: string;
  display_name: string;
  description: string;
  open_booking: boolean;
  // logo_url is the public image path; empty when the organization has no logo.
  logo_url?: string;
  // theme is the organization's default theme key; empty means the server
  // default applies.
  theme?: string;
  created_at: string;
  updated_at: string;
  // Named members come back with the group. Everyone in the organization's
  // domains belongs to it regardless; these are the people who administer it.
  members?: WorkgroupMember[];
}

export interface WorkgroupMember {
  workgroup_id: number;
  address: string;
  // 'member' is a row somebody added; 'domain' is an address inside one of the
  // organization's own domains, which is a membership nobody maintains and
  // nobody can remove. A domain member has no role, since nobody granted one.
  role?: 'member' | 'admin';
  source?: 'member' | 'domain';
  created_at: string;
}

// OrgCalendar is a calendar the organization owns rather than a person: it has
// no owner, so it outlasts whoever created it, and it appears in the calendar
// picker of everybody in the organization.
export interface OrgCalendar {
  id: number;
  workgroup_id?: number;
  owner?: string;
  name: string;
  display_name?: string;
  color: string;
  timezone: string;
  description: string;
  show_free_busy: boolean;
  is_default?: boolean;
  created_at: string;
  updated_at: string;
}

// WorkgroupGroup is a named set of people inside an organization, addressed
// when inviting to a meeting or sharing a calendar.
export interface WorkgroupGroup {
  id: number;
  workgroup_id: number;
  name: string;
  description: string;
  // everyone marks the organization's own group: every address in its domains.
  // It takes no members and cannot be deleted, because there is nothing in it
  // to add or to take away — its membership is the directory.
  everyone?: boolean;
  member_count: number;
  created_at: string;
  updated_at: string;
}

// ThemeRow is a custom theme uploaded as a ZIP. `key` is the value stored in a
// user or organization preference ("custom:<uid>").
export interface ThemeRow {
  uid: string;
  name: string;
  key: string;
  css: string;
  created_by: string;
  created_at: string;
}

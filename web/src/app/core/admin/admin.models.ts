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

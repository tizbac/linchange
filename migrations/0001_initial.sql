-- linchange 0001: initial schema.
--
-- Conventions:
--   * Addresses are citext so that Alice@Example.com and alice@example.com are
--     the same principal. Every address column is citext, never text.
--   * users.address is the primary key, because a username in this system *is*
--     a full email address and must be unique as such.
--   * Message bodies are not stored here. messages.blob_sha256 references the
--     on-disk content-addressed store; only metadata lives in PostgreSQL.
--   * Timestamps are timestamptz and always stored in UTC.

CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- Enumerated types
-- ---------------------------------------------------------------------------

-- local     : owns an argon2id credential in this server
-- ldap      : authenticates against a directory; has mail only if the address
--              domain is hosted here
-- domain_admin: can sign in and manage domains but holds no mail
-- shared    : a shared mailbox reached through shared_mailbox_grants
CREATE TYPE user_source AS ENUM ('local', 'ldap', 'domain_admin', 'shared');

CREATE TYPE domain_status AS ENUM ('active', 'suspended', 'disabled');

CREATE TYPE alias_target_kind AS ENUM ('user', 'shared', 'workgroup');

CREATE TYPE grant_level AS ENUM ('read', 'contribute', 'editor', 'owner');

CREATE TYPE outbox_state AS ENUM ('queued', 'sending', 'sent', 'deferred', 'failed', 'bounced');

CREATE TYPE outbox_recipient_kind AS ENUM ('to', 'cc', 'bcc');

-- ---------------------------------------------------------------------------
-- Domains
-- ---------------------------------------------------------------------------

CREATE TABLE domains (
    name                citext PRIMARY KEY,
    status              domain_status   NOT NULL DEFAULT 'active',
    -- A local domain is one this server accepts mail for and delivers to.
    is_local            boolean         NOT NULL DEFAULT true,
    description         text,
    -- Inherited by new mailboxes in this domain when the user has no explicit
    -- quota of their own.
    default_quota_bytes bigint          NOT NULL DEFAULT 10737418240,
    -- Per-user override of mail.submission.daily_send_limit. NULL means fall
    -- back to the global value.
    daily_send_limit    integer,
    -- Rows in dkim_keys are keyed by (domain, selector).
    dkim_selector       citext,
    created_at          timestamptz     NOT NULL DEFAULT now(),
    updated_at          timestamptz     NOT NULL DEFAULT now(),
    -- A domain must not accept mail while it is suspended.
    CONSTRAINT domains_quota_non_negative CHECK (default_quota_bytes >= 0),
    CONSTRAINT domains_send_limit_non_negative CHECK (daily_send_limit IS NULL OR daily_send_limit >= 0)
);

COMMENT ON TABLE domains IS
    'Domains this server hosts. A domain is local when it accepts inbound mail.';

-- ---------------------------------------------------------------------------
-- Users: local users, LDAP users, domain admins and shared mailboxes
-- ---------------------------------------------------------------------------

CREATE TABLE users (
    address             citext PRIMARY KEY,
    source              user_source   NOT NULL,
    display_name        text          NOT NULL DEFAULT '',
    -- A disabled user cannot authenticate and receives no mail.
    enabled             boolean       NOT NULL DEFAULT true,
    -- Whether this principal may hold mail. False for domain_admin rows.
    -- Checked by every mail protocol and by the REST API, so that a domain
    -- admin can sign in to the web UI but never open an IMAP session.
    mailbox_enabled     boolean       NOT NULL DEFAULT true,
    -- Quota for this user's mailboxes. 0 means unlimited, NULL means inherit
    -- from the domain and then from mail.default_quota_bytes.
    quota_bytes         bigint,
    -- Per-user override of the daily send cap. NULL inherits.
    daily_send_limit    integer,
    -- Seconds the user's timezone is offset from UTC, used to render times in
    -- the web UI and calendar.
    timezone            text          NOT NULL DEFAULT 'UTC',
    -- Locale hint for the web UI.
    language            text          NOT NULL DEFAULT 'en',
    -- Set when a local or domain_admin row must change its password at next
    -- login.
    must_change_password boolean      NOT NULL DEFAULT false,
    -- Denormalised copy of the address, lowercased, for case-sensitive
    -- comparisons and for the shared-mailbox '#' suffix split.
    address_local       text          NOT NULL,
    last_login_at       timestamptz,
    last_login_ip       inet,
    created_at          timestamptz    NOT NULL DEFAULT now(),
    updated_at          timestamptz    NOT NULL DEFAULT now(),
    deleted_at          timestamptz,
    CONSTRAINT users_quota_non_negative CHECK (quota_bytes IS NULL OR quota_bytes >= 0),
    CONSTRAINT users_send_limit_non_negative CHECK (daily_send_limit IS NULL OR daily_send_limit >= 0)
);

CREATE INDEX users_source_idx       ON users (source) WHERE deleted_at IS NULL;
CREATE INDEX users_mailbox_idx      ON users (mailbox_enabled) WHERE deleted_at IS NULL;
CREATE INDEX users_address_local_idx ON users (address_local);

COMMENT ON COLUMN users.address_local IS
    'Lowercased copy of address, used for the username#sharedmailbox split.';
COMMENT ON COLUMN users.quota_bytes IS
    'NULL inherits from the domain, 0 means unlimited.';

-- Credentials for local users and domain admins. LDAP rows have none.
CREATE TABLE credentials (
    address         citext PRIMARY KEY REFERENCES users (address) ON DELETE CASCADE,
    -- Encoded argon2id PHC string, so parameters travel with the hash and can
    -- be raised later without invalidating existing passwords.
    password_hash   text        NOT NULL,
    -- Set when a local or ldap user must change their password.
    must_change     boolean     NOT NULL DEFAULT false,
    changed_at      timestamptz,
    failed_attempts integer     NOT NULL DEFAULT 0,
    locked_until    timestamptz,
    CONSTRAINT credentials_failed_non_negative CHECK (failed_attempts >= 0)
);

CREATE INDEX credentials_locked_idx ON credentials (locked_until) WHERE locked_until IS NOT NULL;

-- Directory bindings.
CREATE TABLE ldap_links (
    address       citext PRIMARY KEY REFERENCES users (address) ON DELETE CASCADE,
    provider      text        NOT NULL,
    dn            text        NOT NULL,
    -- Last time the directory reported this entry. Used by ldap.sync to decide
    -- whether a missing user should be disabled now or after a grace period.
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    -- Directory attributes we care about, for display without a live lookup.
    display_name  text        NOT NULL DEFAULT '',
    member_of     text[]      NOT NULL DEFAULT '{}',
    synced_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ldap_links_provider_idx  ON ldap_links (provider);
CREATE INDEX ldap_links_last_seen_idx ON ldap_links (last_seen_at);

-- Recovery address for self-service password reset. Local users only: an LDAP
-- user's password is changed in the directory.
CREATE TABLE user_secondary_emails (
    address      citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    email        citext      NOT NULL,
    verified_at  timestamptz,
    is_primary   boolean     NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (address, email)
);

CREATE UNIQUE INDEX user_secondary_one_primary_idx
    ON user_secondary_emails (address) WHERE is_primary;

-- TOTP enrolments. Held locally for every user, including LDAP users, because
-- the directory does not provide a second factor.
CREATE TABLE user_totp (
    address          citext PRIMARY KEY REFERENCES users (address) ON DELETE CASCADE,
    -- AES-256-GCM ciphertext of the base32 secret; the key comes from config.
    secret_encrypted bytea        NOT NULL,
    enabled          boolean      NOT NULL DEFAULT false,
    recovery_codes   text[]       NOT NULL DEFAULT '{}',
    last_used_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE user_totp IS
    'TOTP secrets are encrypted at rest with mail.dkim_private_key_encryption_key.';

-- ---------------------------------------------------------------------------
-- Workgroups and their calendars
-- ---------------------------------------------------------------------------

CREATE TABLE workgroups (
    id           bigserial PRIMARY KEY,
    name         citext      NOT NULL,
    domain       citext      NOT NULL,
    display_name text        NOT NULL DEFAULT '',
    description  text        NOT NULL DEFAULT '',
    -- Any authenticated user may book time; the members list controls
    -- administration instead.
    open_booking boolean     NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (domain, name)
);

CREATE TABLE workgroup_members (
    workgroup_id bigint  NOT NULL REFERENCES workgroups (id) ON DELETE CASCADE,
    address      citext  NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    role         text    NOT NULL DEFAULT 'member',
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workgroup_id, address),
    CONSTRAINT workgroup_members_role CHECK (role IN ('member', 'admin'))
);

-- ---------------------------------------------------------------------------
-- Aliases
-- ---------------------------------------------------------------------------

CREATE TABLE aliases (
    address      citext           PRIMARY KEY,
    target_kind  alias_target_kind NOT NULL,
    target_id    bigint           NOT NULL,
    -- Optional display name and destination comment shown in clients.
    display_name text             NOT NULL DEFAULT '',
    description  text             NOT NULL DEFAULT '',
    enabled      boolean          NOT NULL DEFAULT true,
    created_at   timestamptz      NOT NULL DEFAULT now(),
    updated_at   timestamptz      NOT NULL DEFAULT now(),
    updated_by   citext
);

CREATE INDEX aliases_target_idx ON aliases (target_kind, target_id);

COMMENT ON TABLE aliases IS
    'target_id references users.address as text for kind=user or shared, and workgroups.id for kind=workgroup.';

-- Access grants that let a member user open a shared mailbox. This is the
-- model Exchange uses; the 'user#sharedmailbox' login suffix is resolved from
-- it too.
CREATE TABLE shared_mailbox_grants (
    shared_address citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    member_address citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    level          grant_level NOT NULL DEFAULT 'contribute',
    granted_by     citext,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (shared_address, member_address)
);

CREATE INDEX shared_grants_member_idx ON shared_mailbox_grants (member_address);

-- ---------------------------------------------------------------------------
-- Mailboxes and messages
-- ---------------------------------------------------------------------------

CREATE TABLE mailboxes (
    id           bigserial PRIMARY KEY,
    owner        citext   NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    -- Path relative to the owner, e.g. 'INBOX', 'INBOX/Projects', or
    -- '#shared/support' for a shared mailbox namespace. The IMAP name is
    -- derived from this.
    name         text     NOT NULL,
    parent_id    bigint   REFERENCES mailboxes (id) ON DELETE CASCADE,
    -- IMAP requires uidvalidity to change whenever a mailbox is emptied so
    -- that clients discard their cached sequence numbers.
    uidvalidity  bigint   NOT NULL,
    uidnext      bigint   NOT NULL DEFAULT 1,
    -- RFC 6154 special-use attribute, e.g. \Sent.
    special_use  text,
    subscribed   boolean  NOT NULL DEFAULT true,
    -- NULL for a system folder, set for a user-created one.
    created_by   citext,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    -- An owner's folder names are unique; '\Sent' can appear once per owner.
    UNIQUE (owner, name),
    -- A folder cannot be its own ancestor.
    CONSTRAINT mailboxes_not_self_parent CHECK (parent_id IS DISTINCT FROM id)
);

CREATE INDEX mailboxes_owner_idx  ON mailboxes (owner);
CREATE INDEX mailboxes_parent_idx ON mailboxes (parent_id);

CREATE TABLE messages (
    id            bigserial PRIMARY KEY,
    mailbox_id    bigint      NOT NULL REFERENCES mailboxes (id) ON DELETE CASCADE,
    -- IMAP UID. Monotonic per mailbox, never reused within a uidvalidity.
    uid           bigint      NOT NULL,
    -- Key into the on-disk content-addressed store. The bytes are the exact
    -- RFC 5322 message.
    blob_sha256   bytea       NOT NULL,
    size          bigint      NOT NULL,
    internal_date timestamptz NOT NULL,
    flags         text[]      NOT NULL DEFAULT '{}',
    -- Denormalised headers. Keeping these in the database is what lets the
    -- webmail list view, IMAP SEARCH and threading work without reading and
    -- re-parsing every message body.
    subject       text        NOT NULL DEFAULT '',
    from_address  text        NOT NULL DEFAULT '',
    from_name     text        NOT NULL DEFAULT '',
    snippet       text        NOT NULL DEFAULT '',
    has_attachments boolean   NOT NULL DEFAULT false,
    message_id    text,
    -- Stable key linking a conversation: the root Message-ID when the
    -- References chain can be resolved, otherwise the first-seen Message-ID.
    thread_key    text        NOT NULL,
    -- Subject normalised for thread grouping, so "Re: Re: Fwd: Re: x" and "x"
    -- land in one thread.
    thread_subject text       NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (mailbox_id, uid),
    CONSTRAINT messages_size_non_negative CHECK (size >= 0)
);

-- The list-view and search index.
CREATE INDEX messages_mailbox_recent_idx ON messages (mailbox_id, internal_date DESC, id DESC);
-- UID lookups for IMAP FETCH/UID ranges.
CREATE INDEX messages_mailbox_uid_idx     ON messages (mailbox_id, uid);
CREATE INDEX messages_blob_idx           ON messages (blob_sha256);
CREATE INDEX messages_thread_idx         ON messages (thread_key);
CREATE INDEX messages_from_idx           ON messages (from_address);
CREATE INDEX messages_message_id_idx     ON messages (message_id);
CREATE INDEX messages_unread_idx         ON messages (mailbox_id) WHERE NOT ('seen' = ANY (flags));
CREATE INDEX messages_attachments_idx    ON messages (mailbox_id) WHERE has_attachments;

COMMENT ON COLUMN messages.flags IS
    'IMAP system flags: \Seen, \Answered, \Flagged, \Deleted, \Draft, \Recent.';

-- Header addresses, indexed. This is what powers IMAP SEARCH HEADER/FROM/TO,
-- recipient resolution for autocomplete, and conversation grouping without
-- re-parsing bodies.
CREATE TABLE message_addresses (
    message_id   bigint NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    header_name  text   NOT NULL,
    address      citext NOT NULL DEFAULT '',
    display_name text   NOT NULL DEFAULT '',
    position     integer NOT NULL DEFAULT 0,
    PRIMARY KEY (message_id, header_name, position)
);

CREATE INDEX message_addresses_address_idx ON message_addresses (address);
CREATE INDEX message_addresses_header_idx  ON message_addresses (header_name, address);

-- Reference/In-Reply-To links for conversation assembly.
CREATE TABLE message_references (
    message_id   bigint NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    reference    text   NOT NULL,
    position     integer NOT NULL DEFAULT 0,
    PRIMARY KEY (message_id, position)
);

CREATE INDEX message_references_ref_idx ON message_references (reference);

-- Aggregated quota usage per user, maintained transactionally on delivery,
-- APPEND, move and delete so that a quota check never has to sum the whole
-- mailbox table.
CREATE TABLE quota_usage (
    owner        citext PRIMARY KEY REFERENCES users (address) ON DELETE CASCADE,
    -- Sum of message sizes across all of this owner's mailboxes.
    used_bytes   bigint NOT NULL DEFAULT 0,
    message_count bigint NOT NULL DEFAULT 0,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT quota_usage_non_negative CHECK (used_bytes >= 0 AND message_count >= 0)
);

-- Per-day send counters backing mail.submission.daily_send_limit. One row per
-- user per UTC day, incremented in the same transaction that queues the
-- message so the cap cannot be raced.
CREATE TABLE send_counters (
    owner        citext     NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    day          date       NOT NULL,
    count        integer    NOT NULL DEFAULT 0,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner, day),
    CONSTRAINT send_counters_non_negative CHECK (count >= 0)
);

CREATE INDEX send_counters_day_idx ON send_counters (day);

-- ---------------------------------------------------------------------------
-- Outbound queue
-- ---------------------------------------------------------------------------

CREATE TABLE outbox (
    id            bigserial PRIMARY KEY,
    -- The authenticated principal that submitted this message, for audit and
    -- for the daily send cap.
    submitted_by  citext      NOT NULL,
    envelope_from text        NOT NULL,
    -- One row per recipient keeps partial-failure DSNs accurate.
    recipients    outbox_recipient_kind[] NOT NULL DEFAULT '{}',
    envelope_to   text[]      NOT NULL,
    blob_sha256   bytea       NOT NULL,
    size          bigint      NOT NULL,
    state         outbox_state NOT NULL DEFAULT 'queued',
    attempts      integer     NOT NULL DEFAULT 0,
    -- Retry scheduling. Indexed so the worker can claim due work cheaply.
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error    text        NOT NULL DEFAULT '',
    -- Set when the final attempt failed, so a DSN can be generated after
    -- mail.outbound.dsn.delay.
    failed_at     timestamptz,
    -- Bounce deferred to this time, then the DSN is generated.
    dsn_at        timestamptz,
    sent_at       timestamptz,
    created_at    timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT outbox_size_non_negative CHECK (size >= 0)
);

-- The worker's claim query: queued rows whose next_attempt_at has passed.
CREATE INDEX outbox_claim_idx ON outbox (next_attempt_at)
    WHERE state IN ('queued', 'deferred');
CREATE INDEX outbox_state_idx ON outbox (state);
CREATE INDEX outbox_submitter_idx ON outbox (submitted_by, created_at DESC);
CREATE INDEX outbox_due_dsn_idx ON outbox (dsn_at) WHERE dsn_at IS NOT NULL;

-- Greylist for inbound mail. First contact from an unknown sender is deferred
-- this long; legitimate senders retry, most spam does not.
CREATE TABLE greylist (
    sender_ip     inet        NOT NULL,
    envelope_from text        NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    accepted_at   timestamptz,
    PRIMARY KEY (sender_ip, envelope_from)
);

CREATE INDEX greylist_expiry_idx ON greylist (expires_at);

-- Per-connection mail volume counters, used by the inbound spam scorer.
CREATE TABLE rate_buckets (
    scope      text        NOT NULL,
    key        text        NOT NULL,
    -- Start of the counting window. Named bucket_start because WINDOW is a
    -- reserved word in SQL.
    bucket_start timestamptz NOT NULL,
    count      integer     NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, key, bucket_start)
);

CREATE INDEX rate_buckets_expiry_idx ON rate_buckets (bucket_start);

-- ---------------------------------------------------------------------------
-- Calendars
-- ---------------------------------------------------------------------------

CREATE TABLE calendars (
    id             bigserial PRIMARY KEY,
    -- Exactly one of owner or workgroup is set: a personal calendar, or a
    -- workgroup calendar shared by a team.
    owner          citext   REFERENCES users (address) ON DELETE CASCADE,
    workgroup_id   bigint   REFERENCES workgroups (id) ON DELETE CASCADE,
    name           text     NOT NULL DEFAULT 'Calendar',
    color          text     NOT NULL DEFAULT '#3b82f6',
    timezone       text     NOT NULL DEFAULT 'UTC',
    description    text     NOT NULL DEFAULT '',
    -- Free/busy visibility for other users, mirroring Exchange's settings.
    show_free_busy boolean NOT NULL DEFAULT true,
    is_default     boolean  NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT calendars_single_owner CHECK ((owner IS NOT NULL)::int + (workgroup_id IS NOT NULL)::int = 1)
);

-- At most one default calendar per personal owner.
CREATE UNIQUE INDEX calendars_one_default_idx
    ON calendars (owner) WHERE is_default AND owner IS NOT NULL;
CREATE INDEX calendars_owner_idx    ON calendars (owner);
CREATE INDEX calendars_workgroup_idx ON calendars (workgroup_id);

CREATE TABLE events (
    id           bigserial PRIMARY KEY,
    calendar_id  bigint      NOT NULL REFERENCES calendars (id) ON DELETE CASCADE,
    -- iCalendar UID, unique within a calendar.
    uid          text        NOT NULL,
    -- The full VEVENT, so that properties this schema does not model still
    -- round-trip to other clients unchanged.
    ical_blob    bytea       NOT NULL,
    -- Denormalised for range queries; a full scan of events parsing iCalendar
    -- on every calendar view would not scale.
    dtstart      timestamptz NOT NULL,
    dtend        timestamptz NOT NULL,
    -- NULL when the event does not recur.
    rrule        text,
    summary      text        NOT NULL DEFAULT '',
    location     text        NOT NULL DEFAULT '',
    -- iTIP METHOD this event was created from, so a REPLY can be routed back.
    method       text        NOT NULL DEFAULT '',
    organizer    text        NOT NULL DEFAULT '',
    organizer_address citext  NOT NULL DEFAULT '',
    status       text        NOT NULL DEFAULT 'CONFIRMED',
    -- Exchange's own flag, used to distinguish busy from free.
    show_as      text        NOT NULL DEFAULT 'BUSY',
    -- Bumped on every change; clients use it for conflict detection.
    sequence     integer     NOT NULL DEFAULT 0,
    dtstamp      timestamptz NOT NULL DEFAULT now(),
    -- Set when the event has been cancelled. The row is kept so that attendees
    -- receive a CANCEL and so history is not lost.
    cancelled    boolean     NOT NULL DEFAULT false,
    created_by   citext,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (calendar_id, uid),
    CONSTRAINT events_range_ordered CHECK (dtend >= dtstart),
    CONSTRAINT events_show_as CHECK (show_as IN ('FREE', 'TENTATIVE', 'BUSY', 'OOF'))
);

-- The dominant query: every event overlapping a time range, per calendar.
CREATE INDEX events_calendar_range_idx ON events (calendar_id, dtstart, dtend);
CREATE INDEX events_range_idx           ON events (dtstart, dtend);
CREATE INDEX events_organizer_idx       ON events (organizer_address);
CREATE INDEX events_uid_idx             ON events (uid);

CREATE TABLE attendees (
    event_id   bigint      NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    address    citext      NOT NULL,
    display_name text      NOT NULL DEFAULT '',
    -- ROLE in iCalendar: REQ-PARTICIPANT, OPT-PARTICIPANT, CHAIR, NON-PARTICIPANT.
    role       text        NOT NULL DEFAULT 'REQ-PARTICIPANT',
    -- PARTSTAT: NEEDS-ACTION, ACCEPTED, DECLINED, TENTATIVE, DELEGATED.
    partstat   text        NOT NULL DEFAULT 'NEEDS-ACTION',
    rsvp       boolean     NOT NULL DEFAULT true,
    PRIMARY KEY (event_id, address)
);

CREATE INDEX attendees_address_idx ON attendees (address);

-- ---------------------------------------------------------------------------
-- Sessions, tokens, audit
-- ---------------------------------------------------------------------------

CREATE TABLE sessions (
    id                text        PRIMARY KEY,
    address           citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    -- Hash, not the token itself, so a database leak does not yield sessions.
    refresh_token_hash bytea      NOT NULL,
    -- Set while a domain admin is impersonating another user; the original
    -- session owner. The audit log records both.
    impersonating     citext,
    ip                inet,
    user_agent        text        NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    last_seen_at      timestamptz NOT NULL DEFAULT now(),
    absolute_expires_at timestamptz NOT NULL,
    revoked_at        timestamptz
);

CREATE INDEX sessions_address_idx   ON sessions (address);
CREATE INDEX sessions_live_idx      ON sessions (absolute_expires_at) WHERE revoked_at IS NULL;

CREATE TABLE reset_tokens (
    id          bigserial PRIMARY KEY,
    address     citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    token_hash  bytea       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    -- Where the reset link was sent, for audit.
    sent_to     citext      NOT NULL
);

CREATE INDEX reset_tokens_lookup_idx ON reset_tokens (token_hash);
CREATE INDEX reset_tokens_live_idx   ON reset_tokens (expires_at) WHERE used_at IS NULL;

-- Privilege grants allowing a domain admin to act as a specific user, as
-- opposed to the blanket ability implied by being a domain admin.
CREATE TABLE impersonation_grants (
    actor_address  citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    target_address citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    reason         text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz,
    revoked_at     timestamptz,
    PRIMARY KEY (actor_address, target_address)
);

CREATE TABLE audit_log (
    id            bigserial PRIMARY KEY,
    actor_address citext,
    -- The user whose data was acted upon. Differs from the actor under
    -- impersonation.
    target_address citext,
    action        text        NOT NULL,
    target_type   text        NOT NULL DEFAULT '',
    target_id     text        NOT NULL DEFAULT '',
    ip            inet,
    detail        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_actor_idx  ON audit_log (actor_address, created_at DESC);
CREATE INDEX audit_log_target_idx ON audit_log (target_address, created_at DESC);
CREATE INDEX audit_log_action_idx ON audit_log (action, created_at DESC);
CREATE INDEX audit_log_time_idx   ON audit_log (created_at);

-- ---------------------------------------------------------------------------
-- DKIM keys
-- ---------------------------------------------------------------------------

CREATE TABLE dkim_keys (
    domain            citext  NOT NULL REFERENCES domains (name) ON DELETE CASCADE,
    selector          citext  NOT NULL,
    -- AES-256-GCM ciphertext. Private keys must never be readable at rest.
    private_key_encrypted bytea NOT NULL,
    public_key        text    NOT NULL,
    key_bits          integer NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (domain, selector)
);

-- ---------------------------------------------------------------------------
-- LDAP sync bookkeeping
-- ---------------------------------------------------------------------------

CREATE TABLE ldap_sync_state (
    provider      text        PRIMARY KEY,
    last_run_at   timestamptz,
    last_success_at timestamptz,
    last_error    text        NOT NULL DEFAULT '',
    entries_seen  bigint      NOT NULL DEFAULT 0,
    entries_added bigint      NOT NULL DEFAULT 0,
    entries_updated bigint    NOT NULL DEFAULT 0,
    entries_disabled bigint   NOT NULL DEFAULT 0
);

-- ---------------------------------------------------------------------------
-- Notification channel for IMAP IDLE and for the web UI.
--
-- A NOTIFY payload is limited to 8000 bytes, so this carries identifiers only;
-- clients re-read from the tables.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION notify_mailbox_change() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('mailbox_change',
        json_build_object('mailbox_id', NEW.id, 'owner', NEW.owner)::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER mailboxes_notify
    AFTER INSERT OR UPDATE OR DELETE ON mailboxes
    FOR EACH ROW EXECUTE FUNCTION notify_mailbox_change();

CREATE OR REPLACE FUNCTION notify_message_change() RETURNS trigger AS $$
DECLARE
    target bigint;
    who    citext;
BEGIN
    target := COALESCE(NEW.mailbox_id, OLD.mailbox_id);
    SELECT owner INTO who FROM mailboxes WHERE id = target;
    PERFORM pg_notify('message_change',
        json_build_object('mailbox_id', target, 'owner', who, 'message_id', COALESCE(NEW.id, OLD.id))::text);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER messages_notify
    AFTER INSERT OR UPDATE OR DELETE ON messages
    FOR EACH ROW EXECUTE FUNCTION notify_message_change();

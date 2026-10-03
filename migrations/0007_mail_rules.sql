-- Server-side mail filtering rules.
--
-- A rule belongs to one mailbox owner and runs when a message is delivered to
-- them. Conditions and actions are JSON rather than columns because they are
-- read as a unit by the delivery path and never queried field by field; what is
-- queried is the owner, the enabled flag and the order, and those are columns.
CREATE TABLE mail_rules (
    id            bigserial   PRIMARY KEY,
    -- The mailbox owner the rule applies to. A rule on a shared mailbox is
    -- owned by the shared address, so delivery resolves it the same way it
    -- resolves the mailbox.
    owner         citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    name          text        NOT NULL,
    enabled       boolean     NOT NULL DEFAULT true,
    -- Lower numbers run first. Ties break on id, so the order is total and two
    -- rules cannot shuffle between runs.
    priority      integer     NOT NULL DEFAULT 100,
    -- Conditions and actions are validated by the application. The schema
    -- insists only on their shape, because a wrong shape here would fail deep
    -- inside the delivery path instead of at the write. Conditions are the
    -- rules' list of tests (an array); actions the list of things to do.
    conditions    jsonb       NOT NULL DEFAULT '[]'::jsonb,
    actions       jsonb       NOT NULL DEFAULT '[]'::jsonb,
    -- Whether later rules still run after this one matched. A rule that moves
    -- mail to Junk normally stops the chain, so one rule cannot undo another.
    stop_processing boolean  NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT mail_rules_conditions_array CHECK (jsonb_typeof(conditions) = 'array'),
    CONSTRAINT mail_rules_actions_array   CHECK (jsonb_typeof(actions) = 'array'),
    CONSTRAINT mail_rules_priority_positive CHECK (priority >= 0)
);

-- The delivery path reads a user's enabled rules in order on every message.
CREATE INDEX mail_rules_owner_enabled_idx ON mail_rules (owner, enabled, priority, id);

-- Folder defaults for a new mailbox.
--
-- A user created today gets INBOX and nothing else, so the folder list a
-- webmail client shows has one entry and nowhere to file a reply or a draft.
-- These are the standard set, declared once, and provisioned together.
CREATE TABLE system_folders (
    name          text        PRIMARY KEY,
    -- Display name for clients that show one.
    display_name  text        NOT NULL,
    -- RFC 6154 special-use attribute, which is how a client knows this folder
    -- is the Sent folder rather than one the user named Sent.
    special_use   text,
    -- Order in the folder list, smallest first.
    display_order integer     NOT NULL DEFAULT 100,
    CONSTRAINT system_folders_special_use_valid CHECK (
        special_use IS NULL OR special_use IN (
            '\sent', '\drafts', '\trash', '\junk', '\archive', '\all', '\flagged')
    )
);

INSERT INTO system_folders (name, display_name, special_use, display_order) VALUES
    ('Sent',    'Sent',    '\sent',    10),
    ('Drafts',  'Drafts',  '\drafts',  20),
    ('Junk',    'Junk',    '\junk',    30),
    ('Trash',   'Trash',   '\trash',   40),
    ('Archive', 'Archive', '\archive', 50);

COMMENT ON TABLE mail_rules IS
    'Per-user filtering rules, applied in the inbound delivery path before the message is filed.';
COMMENT ON TABLE system_folders IS
    'Folders provisioned for every mailbox owner, keyed by name so provisioning is idempotent.';
-- Threading looks up the conversation a reference-less reply belongs to by
-- subject and sender within one folder. Without this index every message in
-- the folder is scanned on every delivery.
CREATE INDEX messages_thread_lookup_idx
    ON messages (mailbox_id, thread_subject, internal_date DESC);

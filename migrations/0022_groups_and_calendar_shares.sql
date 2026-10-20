-- linchange 0022: organization groups, and calendars shared with people or
-- groups at a chosen level.
--
-- Two things this adds, which belong together because a share almost always
-- names the second:
--
--   - A group is a named set of people inside one organization, so a calendar
--     or an invitation can be addressed to "the sales team" instead of to six
--     addresses that go stale the week after they are written down.
--   - A calendar share grants one subject -- a person or a group -- either the
--     right to read a calendar or the right to write into it. Before this, a
--     calendar was either somebody's private one or a team one that everybody
--     could book; there was no way to let a colleague see your diary without
--     letting them edit it.

BEGIN;

-- ---------------------------------------------------------------------------
-- Groups
-- ---------------------------------------------------------------------------

CREATE TABLE workgroup_groups (
    id           bigserial PRIMARY KEY,
    workgroup_id bigint   NOT NULL REFERENCES workgroups (id) ON DELETE CASCADE,
    name         text     NOT NULL,
    description  text     NOT NULL DEFAULT '',
    -- everyone marks the organization's own group: it is every address in the
    -- organization's domains, derived from the directory rather than listed, so
    -- there is no membership to keep in step as people join and leave. Its
    -- member rows are refused by the application for the same reason -- an
    -- address in the group's domains is in this group already, and a row
    -- saying so could later be removed while the person stayed a member.
    everyone     boolean  NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workgroup_id, name)
);

CREATE INDEX workgroup_groups_workgroup_idx ON workgroup_groups (workgroup_id);

-- At most one Everyone group per organization, which is what keeps "share with
-- the organization" a single answer rather than a set somebody could add to.
CREATE UNIQUE INDEX workgroup_groups_everyone_idx
    ON workgroup_groups (workgroup_id) WHERE everyone;

CREATE TABLE workgroup_group_members (
    group_id   bigint   NOT NULL REFERENCES workgroup_groups (id) ON DELETE CASCADE,
    address    citext   NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, address)
);

CREATE INDEX workgroup_group_members_address_idx ON workgroup_group_members (address);

-- Every organization that exists when this migration runs gets its Everyone
-- group now, and the trigger below gives the same group to every organization
-- created afterwards, so the invariant lives in the schema rather than in a
-- code path somebody could bypass.
INSERT INTO workgroup_groups (workgroup_id, name, description, everyone)
SELECT id, 'Everyone', 'Every address in the organization''s domains.', true
FROM workgroups
ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION create_workgroup_everyone_group() RETURNS trigger AS $$
BEGIN
    INSERT INTO workgroup_groups (workgroup_id, name, description, everyone)
    VALUES (NEW.id, 'Everyone', 'Every address in the organization''s domains.', true)
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workgroup_everyone_group_trigger
AFTER INSERT ON workgroups
FOR EACH ROW EXECUTE FUNCTION create_workgroup_everyone_group();

-- ---------------------------------------------------------------------------
-- Calendar shares
-- ---------------------------------------------------------------------------

-- workgroup_group_has_address is the one place that answers "is this address
-- in this group". The share lookup and the list of calendars shared with an
-- address both ask it, so they cannot come to different answers about who a
-- group reaches -- which would show up as a calendar in somebody's sidebar
-- that they are then refused when they open it.
--
-- An Everyone group reaches an address through the directory: the domains the
-- organization owns, which is where Workgroups.Domains looks too. A listed
-- group reaches it through its member rows.
CREATE FUNCTION workgroup_group_has_address(p_group_id bigint, p_addr citext)
RETURNS boolean
LANGUAGE sql
STABLE
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM workgroup_groups wg
        JOIN workgroups w ON w.id = wg.workgroup_id
        WHERE wg.id = p_group_id
          AND (
            (wg.everyone AND EXISTS (
                SELECT 1 FROM users u
                WHERE u.address = p_addr
                  AND u.enabled
                  AND (lower(split_part(u.address, '@', 2)) = lower(w.domain)
                       OR lower(split_part(u.address, '@', 2)) IN (
                           SELECT lower(d.name) FROM domains d WHERE d.workgroup_id = w.id))))
            OR
            (NOT wg.everyone AND EXISTS (
                SELECT 1 FROM workgroup_group_members m
                WHERE m.group_id = wg.id AND m.address = p_addr))
          )
    )
$$;

CREATE TABLE calendar_shares (
    id              bigserial PRIMARY KEY,
    calendar_id     bigint   NOT NULL REFERENCES calendars (id) ON DELETE CASCADE,
    subject_kind    text     NOT NULL CHECK (subject_kind IN ('user', 'group')),
    -- Exactly one subject column is set, and it is the one the kind names.
    -- Without the pair of checks an incoherent row is possible, and the access
    -- lookup would then answer for nobody while looking like a share.
    subject_address citext   REFERENCES users (address) ON DELETE CASCADE,
    subject_group_id bigint  REFERENCES workgroup_groups (id) ON DELETE CASCADE,
    level           text     NOT NULL CHECK (level IN ('read', 'write')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT calendar_shares_subject_user
        CHECK ((subject_kind = 'user') = (subject_address IS NOT NULL)),
    CONSTRAINT calendar_shares_subject_group
        CHECK ((subject_kind = 'group') = (subject_group_id IS NOT NULL))
);

-- Sharing twice with the same subject is an edit, not a second share, so the
-- pair is unique and the write path upserts.
CREATE UNIQUE INDEX calendar_shares_user_idx
    ON calendar_shares (calendar_id, subject_address)
    WHERE subject_address IS NOT NULL;
CREATE UNIQUE INDEX calendar_shares_group_idx
    ON calendar_shares (calendar_id, subject_group_id)
    WHERE subject_group_id IS NOT NULL;

-- "Which of my calendars is shared with this address" is the question the
-- calendar list asks when it marks a calendar as shared.
CREATE INDEX calendar_shares_address_idx
    ON calendar_shares (subject_address) WHERE subject_address IS NOT NULL;

COMMIT;

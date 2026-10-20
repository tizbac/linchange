-- linchange 0020: a workgroup owns its domains.
--
-- An organization (workgroup) is defined by the domains it owns. Every user
-- whose address lives in one of those domains is a member of the organization,
-- so contacts, calendars and the organization name in the top bar can all be
-- derived from the address the user signed in with.
--
-- workgroups.domain is kept: it is the group's home domain, the one its
-- address and alias live in. A group may own additional domains through
-- domains.workgroup_id.

BEGIN;

ALTER TABLE domains
    ADD COLUMN workgroup_id bigint REFERENCES workgroups (id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS domains_workgroup_idx ON domains (workgroup_id);

-- Backfill: a workgroup created before this migration already names its
-- domain, so that domain now belongs to it.
UPDATE domains
SET workgroup_id = w.id
FROM workgroups w
WHERE domains.name = w.domain;

COMMIT;

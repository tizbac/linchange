-- Administration as a grant, rather than as an account type.
--
-- users.source already has a 'domain_admin' member, but that source is not an
-- "administrator" flag and was never meant to be one. It exists for the
-- principal that administers mail and never receives it, which is the shape an
-- AD user with an address outside the hosted domains has to be given. Because
-- the source also forbids holding mail, there has been no way to express the
-- other half of an administrator: a local or LDAP user who receives mail and
-- also administers the domain. Such a user is the common case in a small
-- deployment, and the demo is one.
--
-- That is not merely a missing convenience. Both the web interface and the API
-- derive "may administer" from the source, so a mailbox-holding administrator
-- was refused by every admin route and offered no admin link anywhere: the
-- account existed in the database and was unable to use the product it was an
-- account of.
--
-- This column is that grant. It is orthogonal to source, and nothing about
-- receiving mail follows from holding it. The domain_admin source keeps the
-- meaning it always had, and the CHECK below keeps the two agreeing at the
-- storage layer, so a domain_admin row can never have its grant taken away
-- underneath it and stop being an administrator.

ALTER TABLE users ADD COLUMN is_domain_admin boolean NOT NULL DEFAULT false;

-- Every existing domain admin is, and always was, an administrator. Without
-- this the new CHECK would fail against any installation that already has one.
UPDATE users SET is_domain_admin = true WHERE source = 'domain_admin';

ALTER TABLE users ADD CONSTRAINT users_domain_admin_is_admin
    CHECK (source <> 'domain_admin' OR is_domain_admin);

-- A shared mailbox is not a login identity: it is reached through grants and
-- has no password, so it can never authenticate and therefore could never act
-- on the grant even if it held one. Refusing the combination here keeps the
-- stored fact true rather than merely unusable.
ALTER TABLE users ADD CONSTRAINT users_shared_mailbox_is_not_admin
    CHECK (source <> 'shared' OR NOT is_domain_admin);

-- Listing the administrators.
--
-- Partial, because the administration page asks for exactly this set and every
-- other reader of users does not: the mailbox, quota and login paths all
-- filter on their own columns, and an index they never mention would only cost
-- writes.
CREATE INDEX users_domain_admin_idx
    ON users (is_domain_admin) WHERE deleted_at IS NULL AND is_domain_admin;

COMMENT ON COLUMN users.is_domain_admin IS
    'May use the administration API and pages. Independent of source: a mailbox-holding local or LDAP user can be an administrator. True for every domain_admin row and false for every shared mailbox row, both enforced by CHECKs.';
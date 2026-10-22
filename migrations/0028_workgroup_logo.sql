-- linchange 0028: an organization can carry its own logo.
--
-- The logo is a small image stored in the blob store. logo_uid is the public,
-- unguessable handle the browser uses in an <img> URL, so viewing a logo needs
-- no bearer token; the rest of the columns describe the blob. Null means the
-- organization has no logo and the product name is shown instead.

BEGIN;

ALTER TABLE workgroups
    ADD COLUMN logo_uid    text UNIQUE,
    ADD COLUMN logo_sha256 bytea,
    ADD COLUMN logo_mime   text;

COMMIT;

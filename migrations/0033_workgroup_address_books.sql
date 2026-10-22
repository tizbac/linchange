-- linchange 0033: an organization may own shared address books.
--
-- A workgroup already owns exactly one organization directory. Administrators
-- also need shared books (for example "Suppliers") that belong to the
-- organization rather than to the administrator's account, so they survive
-- that person leaving. A workgroup-owned shared book still uses the ordinary
-- share rows to reach its members and groups.

BEGIN;

ALTER TABLE address_books DROP CONSTRAINT IF EXISTS address_books_check;

ALTER TABLE address_books ADD CONSTRAINT address_books_workgroup_scope CHECK (
    (scope = 'organization') = (workgroup_id IS NOT NULL)
    OR (scope = 'shared' AND workgroup_id IS NOT NULL)
);

CREATE UNIQUE INDEX address_books_workgroup_slug_idx
    ON address_books (workgroup_id, slug)
    WHERE workgroup_id IS NOT NULL;

COMMIT;

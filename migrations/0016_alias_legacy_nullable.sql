-- 0016: make legacy alias columns nullable
--
-- The alias multi-target migration (0015) introduced the alias_targets table
-- but left the legacy columns in aliases as NOT NULL. Since all new code uses
-- alias_targets, the legacy columns must be nullable so that new aliases can be
-- created without populating the deprecated fields.

ALTER TABLE aliases
    ALTER COLUMN target_kind DROP NOT NULL,
    ALTER COLUMN target_address DROP NOT NULL,
    ALTER COLUMN workgroup_id DROP NOT NULL;

-- The check constraint aliases_target_matches_kind references target_kind,
-- target_address, and workgroup_id. Since those columns are now nullable,
-- the constraint must be updated to handle NULL values. However, the constraint
-- is only enforced when target_kind IS NOT NULL. We can drop and recreate it
-- with a NULL-safe version, or simply drop it since the new alias_targets table
-- enforces the same logic.
ALTER TABLE aliases DROP CONSTRAINT aliases_target_matches_kind;

-- The NOT SELF constraint references target_address which is now nullable.
-- Drop and recreate it with NULL-safe logic.
ALTER TABLE aliases DROP CONSTRAINT aliases_target_not_self;
ALTER TABLE aliases ADD CONSTRAINT aliases_target_not_self CHECK (
    target_address IS NULL OR target_address <> address
);
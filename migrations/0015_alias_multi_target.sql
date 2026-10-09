-- 0015: alias multi-target support
--
-- Aliases can now have multiple targets. The original aliases table is kept
-- for backward compatibility during migration, but a new alias_targets table
-- holds the 1:N relationship.

-- Create the new alias_targets table
CREATE TABLE alias_targets (
    id            bigserial PRIMARY KEY,
    alias_address citext NOT NULL REFERENCES aliases(address) ON DELETE CASCADE,
    target_kind   alias_target_kind NOT NULL,
    -- Exactly one of target_address or workgroup_id is set
    target_address citext REFERENCES users(address) ON DELETE CASCADE,
    workgroup_id   bigint REFERENCES workgroups(id) ON DELETE CASCADE,
    -- Order matters for delivery attempt sequence
    position       int NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT alias_targets_one_target CHECK (
        (target_kind = 'workgroup' AND workgroup_id IS NOT NULL AND target_address IS NULL)
        OR
        (target_kind IN ('user', 'shared') AND target_address IS NOT NULL AND workgroup_id IS NULL)
    ),
    CONSTRAINT alias_targets_position_nonneg CHECK (position >= 0)
);

CREATE UNIQUE INDEX alias_targets_unique_target ON alias_targets (alias_address, target_kind, target_address, workgroup_id)
    WHERE target_address IS NOT NULL OR workgroup_id IS NOT NULL;

CREATE INDEX alias_targets_alias_idx ON alias_targets (alias_address);
CREATE INDEX alias_targets_target_idx ON alias_targets (target_address) WHERE target_address IS NOT NULL;
CREATE INDEX alias_targets_workgroup_idx ON alias_targets (workgroup_id) WHERE workgroup_id IS NOT NULL;

COMMENT ON TABLE alias_targets IS
    'Multiple targets for an alias. An alias with N targets delivers to all N destinations.';

-- Migrate existing single-target aliases to the new table
INSERT INTO alias_targets (alias_address, target_kind, target_address, workgroup_id, position, created_at)
SELECT address, target_kind, target_address, workgroup_id, 0, created_at
FROM aliases
WHERE target_address IS NOT NULL OR workgroup_id IS NOT NULL;

-- The original columns in aliases are kept for backward compatibility during transition
-- but are now deprecated. New code should use alias_targets.
-- A view can be created for compatibility:
-- CREATE VIEW aliases_compat AS ... (not needed if we update all readers)
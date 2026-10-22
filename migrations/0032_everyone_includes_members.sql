-- linchange 0032: Everyone includes explicit organization members.
--
-- An organization is every enabled mailbox in its domains, plus anybody an
-- administrator added to workgroup_members. The first set is derived from the
-- directory; the second is how a contractor or an account on another domain
-- joins without moving the mailbox. The share lookup and the calendar list
-- both use this function, so extending it here keeps them in agreement.

BEGIN;

CREATE OR REPLACE FUNCTION workgroup_group_has_address(p_group_id bigint, p_addr citext)
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
                  AND (
                      lower(split_part(u.address, '@', 2)) = lower(w.domain)
                      OR lower(split_part(u.address, '@', 2)) IN (
                          SELECT lower(d.name) FROM domains d WHERE d.workgroup_id = w.id)
                      OR EXISTS (
                          SELECT 1 FROM workgroup_members m
                          WHERE m.workgroup_id = w.id AND m.address = p_addr)
                  )))
            OR
            (NOT wg.everyone AND EXISTS (
                SELECT 1 FROM workgroup_group_members m
                WHERE m.group_id = wg.id AND m.address = p_addr))
          )
    )
$$;

COMMIT;

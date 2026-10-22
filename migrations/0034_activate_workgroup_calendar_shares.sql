-- linchange 0034: organization calendar shares are active immediately.
--
-- A team calendar belongs to the organization, not to the administrator who
-- opened the sharing dialog. Shares of it made through the organization page
-- already had status 'active'; shares of the same calendar made through the
-- calendar page were written 'pending', which asked the group to accept the
-- organization's own resource and left it out of their list until they did.
--
-- Bring the older rows in line with the rule rather than leaving two meanings
-- for the same share depending on which button created it.
BEGIN;

UPDATE calendar_shares cs
SET status = 'active', updated_at = now()
FROM calendars c
WHERE cs.calendar_id = c.id
  AND c.workgroup_id IS NOT NULL
  AND cs.status = 'pending';

COMMIT;

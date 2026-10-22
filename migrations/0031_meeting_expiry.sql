-- linchange 0031: meeting links and rooms expire.
--
-- A meeting's link is a capability. It should not stay usable forever, and the
-- organiser chooses how long it remains valid when creating the meeting. The
-- value is anchored to the meeting's end, so a meeting booked for next month
-- is not expired before it happens: expires_at is the end plus the chosen
-- lifetime (default 12 hours, maximum 24).
--
-- Existing rows get the default measured from their end, so the migration
-- does not silently expire meetings that are still ahead.

BEGIN;

ALTER TABLE meetings ADD COLUMN expires_at timestamptz;

UPDATE meetings m
SET expires_at = e.dtend + interval '12 hours'
FROM events e
WHERE e.id = m.event_id;

UPDATE meetings
SET expires_at = now() + interval '12 hours'
WHERE expires_at IS NULL;

ALTER TABLE meetings ALTER COLUMN expires_at SET NOT NULL;

CREATE INDEX meetings_expires_at_idx ON meetings (expires_at);

COMMIT;

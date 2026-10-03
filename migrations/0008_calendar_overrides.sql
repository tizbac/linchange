-- Calendar storage gaps that only appear once recurring events are used.
--
-- The events table was written with a single row per (calendar, uid), which is
-- right for a simple event and wrong for a recurring one: moving a single
-- occurrence of a weekly meeting is a *second* row carrying the same UID, and
-- storing it that way is impossible while the constraint holds. The same goes
-- for the display timezone, which a range query has to have in order to
-- expand a recurrence without re-parsing the iCalendar of every row it
-- touched.

-- Which occurrence of a series this row replaces.
--
-- NULL on the master event. Set on a detached instance, whose DTSTART may be
-- anywhere: the value names the slot in the *original* series that this row
-- has taken the place of. Storing the new time here instead would make the
-- override impossible to find, and the moved meeting would then appear at both
-- the old time and the new one.
ALTER TABLE events ADD COLUMN recurrence_id timestamptz;

-- The TZID the event was written in.
--
-- Expansion happens in the event's own zone so that a 09:00 meeting stays at
-- 09:00 when the clocks change. That requires the zone as a column: reading it
-- out of ical_blob would mean parsing iCalendar for every row a range query
-- touches, which is the scan the denormalised dtstart/dtend columns exist to
-- avoid.
ALTER TABLE events ADD COLUMN timezone text NOT NULL DEFAULT 'UTC';

-- Whether DTSTART was a DATE rather than a DATE-TIME.
--
-- An all-day event has no instant, only dates, and its DTEND is exclusive. Both
-- facts change how the event is expanded and drawn, and neither can be read
-- without the blob.
ALTER TABLE events ADD COLUMN all_day boolean NOT NULL DEFAULT false;

-- Replace the single uniqueness rule with the two that actually hold.
--
-- The old constraint made a detached instance unstorable, so it goes. Two
-- partial indexes say the rule in a form that can be read: at most one master
-- per UID per calendar, and at most one override per slot.
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_calendar_id_uid_key;

CREATE UNIQUE INDEX events_uid_master_idx
    ON events (calendar_id, uid) WHERE recurrence_id IS NULL;

CREATE UNIQUE INDEX events_uid_override_idx
    ON events (calendar_id, uid, recurrence_id) WHERE recurrence_id IS NOT NULL;

-- Finding the events a person has been invited to.
--
-- The existing index leads with event_id, which serves the write that adds an
-- attendee. The calendar view needs the other direction: every event this
-- address attends, ordered so the events can be fetched in one pass.
CREATE INDEX attendees_address_event_idx ON attendees (address, event_id);

-- Booking onto a workgroup calendar means finding its events in a range, and
-- the calendar_id lead is already there for that. What is missing is the
-- recurrence slot lookup used when one occurrence of a workgroup booking is
-- moved.
CREATE INDEX events_calendar_recurrence_idx
    ON events (calendar_id, recurrence_id) WHERE recurrence_id IS NOT NULL;

COMMENT ON COLUMN events.recurrence_id IS
    'The occurrence of the series this row replaces; NULL on the master event.';
COMMENT ON COLUMN events.timezone IS
    'TZID the event was written in, denormalised so a range query can expand a recurrence without parsing ical_blob.';
COMMENT ON COLUMN events.all_day IS
    'True when DTSTART is a DATE, in which case DTEND is an exclusive date rather than an instant.';

-- A record of every change to a calendar, which is what a CalDAV sync reports.
--
-- RFC 6578 defines sync-collection as "what changed in this collection since the
-- token you last gave me", and a client applies exactly what it is told: a member
-- that is not in the answer is a member the client keeps. So a server that answers
-- only with the rows that still exist cannot report a deletion at all, and an
-- event deleted on the web interface stays on every phone that subscribed to it
-- until the user deletes it by hand, one device at a time.
--
-- The events table cannot answer the question either. Its sequence column says
-- which revision a row is at, which is not the same thing: it is per row, so a row
-- created after a token was issued has a lower sequence than the token and would
-- never be reported, and it is bumped by an attendance reply, which changes what a
-- client sees of the document. What sync needs is a per-collection order, and a
-- deletion has to leave a trace, because "this row is gone" is the message.
--
-- The event is identified the way the rest of the schema identifies one -- by UID
-- and recurrence slot -- rather than by row id, so a change can be recorded and
-- then still be reported after the row it named has been deleted. It is the same
-- key the DAV resource name is derived from, so a client can be told which URL to
-- forget without this layer knowing how URLs are spelled.
--
-- One row per change, in the order the changes happened, because that order is
-- the report: a client that is told about the addition and the removal of the same
-- resource applies them in the order given. Consecutive changes to one resource
-- are all recorded rather than folded together, so the sequence a client walks is
-- the sequence that happened.
--
-- The rows are pruned after a month. A client that has not synchronised for longer
-- than that is answered 403 with DAV:valid-sync-token, which RFC 6578 §3.6
-- requires for a token the server cannot honour and which every client handles by
-- starting over. That is a full listing once a month for a client that was not
-- checking anyway, and it is the alternative to a table that grows without bound
-- and a token that is valid but describes changes the server no longer has.
CREATE TABLE calendar_changes (
    id            bigserial PRIMARY KEY,
    calendar_id   bigint      NOT NULL REFERENCES calendars (id) ON DELETE CASCADE,
    uid           text        NOT NULL,
    -- Which occurrence of a series this row is about; NULL on the master.
    recurrence_id timestamptz,
    -- Whether the change is that this resource went away. A client that sees this
    -- forgets the href rather than downloading it.
    removed       boolean     NOT NULL DEFAULT false,
    changed_at    timestamptz NOT NULL DEFAULT now()
);

-- The only query that matters: everything after a position, in the order it
-- happened.
CREATE INDEX calendar_changes_position_idx ON calendar_changes (calendar_id, id);

-- Pruning walks changed_at, which this supplies; without it a monthly DELETE
-- seq-scans the table to find rows older than a date.
CREATE INDEX calendar_changes_changed_at_idx ON calendar_changes (changed_at);
-- linchange 0035: out-of-office replies.
--
-- The schedule and the message belong to the account. A reply is sent at most
-- once per correspondent per day, and the table that remembers that is kept
-- separate so the account row stays about the setting rather than about every
-- sender who happened to write while it was active.
BEGIN;

ALTER TABLE users
    ADD COLUMN out_of_office_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN out_of_office_start timestamptz,
    ADD COLUMN out_of_office_end timestamptz,
    ADD COLUMN out_of_office_subject text NOT NULL DEFAULT '',
    ADD COLUMN out_of_office_message text NOT NULL DEFAULT '';

CREATE TABLE out_of_office_replies (
    owner        citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    recipient    citext      NOT NULL,
    last_sent_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner, recipient)
);

COMMIT;

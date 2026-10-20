-- A meeting is a calendar event that also has a chat room and an invitation
-- trail. The event itself - times, organiser, guest list with every answer -
-- stays in events and attendees where every calendar client already reads it;
-- this schema adds only what a plain event does not carry: the identity of the
-- OpenIM group the conversation happens in, and the per-guest token that lets
-- somebody without an account here answer the invitation from the web.

CREATE TABLE meetings (
    id           bigserial PRIMARY KEY,
    -- iCalendar UID of the backing event. Unique, and also the public
    -- identifier the join and RSVP links are built from: the UID is 128 bits
    -- of randomness, so the link is a capability rather than a guessable path.
    uid          text        NOT NULL UNIQUE,
    event_id     bigint      NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    -- The organiser, for listing meetings without joining through the event.
    organizer    citext      NOT NULL,
    -- OpenIM group id holding the meeting's chat. Empty when chat is off or
    -- the group could not be created: a meeting is still a meeting without it.
    im_group_id  text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX meetings_organizer_idx ON meetings (organizer);

CREATE TABLE meeting_invites (
    meeting_id   bigint      NOT NULL REFERENCES meetings (id) ON DELETE CASCADE,
    address      citext      NOT NULL,
    -- OpenIM user id this guest was provisioned as. Empty for guests who are
    -- not users of this server: an external address gets the invitation and
    -- the RSVP link, but no chat account, because OpenIM users are linchange
    -- users and nothing else.
    im_user_id   text        NOT NULL DEFAULT '',
    -- Per-guest secret embedded in the RSVP link. It is what lets an
    -- unauthenticated visitor answer for one address without being able to
    -- answer for any other.
    rsvp_token   text        NOT NULL,
    PRIMARY KEY (meeting_id, address)
);

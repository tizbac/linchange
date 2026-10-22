-- linchange 0024: chat lives here, not on an external chat server.
--
-- A conversation is the unit a message belongs to. A direct message is a
-- conversation with two members, a group is one with more, and a meeting's room
-- is a conversation tied to the meeting. One table serves all three so the
-- membership, receipt and history rules are written once.
--
-- The meeting's chat room therefore becomes a conversation in this database:
-- the column that held an external group id now holds the conversation uid,
-- and a guest's "is this a local user" fact is recorded at invitation time
-- rather than inferred from an external account id.

BEGIN;

CREATE TABLE chat_conversations (
    id          bigserial   PRIMARY KEY,
    -- Public identifier used by clients and the socket protocol.
    uid         text        NOT NULL UNIQUE,
    kind        text        NOT NULL CHECK (kind IN ('direct', 'group', 'meeting')),
    title       text        NOT NULL DEFAULT '',
    -- For a direct conversation: the two member addresses sorted and joined.
    -- Unique, so opening the same DM twice returns the same room rather than a
    -- pair of one-to-one conversations that never see each other.
    dm_key      text        UNIQUE,
    -- For a meeting room: the meeting's UID. One room per meeting.
    meeting_uid text        REFERENCES meetings (uid) ON DELETE CASCADE,
    -- For an organization group's room: the group. One room per group, and its
    -- members are the group's members, read from the group rather than copied
    -- into chat_members, so the room cannot drift from the group it is for.
    group_id    bigint      REFERENCES workgroup_groups (id) ON DELETE CASCADE,
    -- A one-way (webinar) meeting lets only the presenter speak; everybody
    -- else listens until the presenter grants them the floor. presenter is
    -- that one voice, and is empty for every other kind of room.
    one_way     boolean     NOT NULL DEFAULT false,
    presenter   citext      REFERENCES users (address) ON DELETE SET NULL,
    created_by  citext      NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- Each kind is tied to exactly its own target, so a half-built row cannot
    -- be written at all.
    CHECK ((kind = 'meeting') = (meeting_uid IS NOT NULL)),
    CHECK ((kind = 'direct') = (dm_key IS NOT NULL)),
    CHECK ((kind = 'group') = (group_id IS NOT NULL))
);

CREATE UNIQUE INDEX chat_conversations_meeting_idx
    ON chat_conversations (meeting_uid) WHERE meeting_uid IS NOT NULL;

CREATE UNIQUE INDEX chat_conversations_group_idx
    ON chat_conversations (group_id) WHERE group_id IS NOT NULL;

CREATE TABLE chat_members (
    conversation_id bigint      NOT NULL REFERENCES chat_conversations (id) ON DELETE CASCADE,
    member          citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    -- Highest message id this member has read; zero means nothing yet. It is a
    -- watermark rather than one row per message, which is all an unread badge
    -- and a read receipt need.
    last_read_id    bigint      NOT NULL DEFAULT 0,
    joined_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, member)
);

CREATE INDEX chat_members_member_idx ON chat_members (member);

CREATE TABLE chat_messages (
    id              bigserial   PRIMARY KEY,
    conversation_id bigint      NOT NULL REFERENCES chat_conversations (id) ON DELETE CASCADE,
    sender          citext      NOT NULL,
    body            text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- History is read newest-first within a conversation, which is exactly this
-- ordering.
CREATE INDEX chat_messages_conversation_idx ON chat_messages (conversation_id, id);

-- The meeting's chat is now a conversation in this server, not a group on an
-- external chat server. The column keeps its place but changes meaning.
ALTER TABLE meetings RENAME COLUMN im_group_id TO chat_conversation;

-- A one-way (webinar) meeting is chosen when it is created and travels with
-- the invitation: only the presenter speaks unless somebody raises a hand.
ALTER TABLE meetings ADD COLUMN one_way boolean NOT NULL DEFAULT false;

-- Whether a guest has a chat account here is decided when the invitation is
-- made: a local user can open the room, an external address cannot, and the
-- answer is recorded rather than derived from a chat-server identifier.
ALTER TABLE meeting_invites ADD COLUMN local boolean NOT NULL DEFAULT false;
UPDATE meeting_invites SET local = (im_user_id <> '');
ALTER TABLE meeting_invites DROP COLUMN im_user_id;

COMMIT;

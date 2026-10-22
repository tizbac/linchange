-- linchange 0027: files in a chat room.
--
-- A message may carry one attachment. The bytes live in the content-addressed
-- blob store, exactly like a message body, and this table holds the fact that
-- belongs to the room: who uploaded it, what it is called, and which
-- conversation may read it. Keeping the metadata here is what lets a download
-- be authorised by room membership rather than by knowing a digest.

BEGIN;

CREATE TABLE chat_attachments (
    id              bigserial   PRIMARY KEY,
    -- Public, unguessable identifier used in download URLs. The numeric id is
    -- an implementation detail and never leaves the server.
    uid             text        NOT NULL UNIQUE,
    conversation_id bigint      NOT NULL REFERENCES chat_conversations (id) ON DELETE CASCADE,
    uploader        citext      NOT NULL,
    -- SHA-256 of the file bytes, the blob store's address.
    blob_sha256     bytea       NOT NULL,
    name            text        NOT NULL,
    mime            text        NOT NULL,
    size            bigint      NOT NULL CHECK (size >= 0),
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX chat_attachments_conversation_idx ON chat_attachments (conversation_id);

-- One attachment per message. Deleting the attachment leaves the message
-- readable instead of cascading a conversation's history away.
ALTER TABLE chat_messages
    ADD COLUMN attachment_id bigint REFERENCES chat_attachments (id) ON DELETE SET NULL;

COMMIT;

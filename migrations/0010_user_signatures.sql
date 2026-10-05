-- The signature a user appends to the mail they send.
--
-- It is a table of its own rather than a column on users because it is not a
-- property of the identity but a piece of content the user wrote, with a
-- lifecycle of its own: it can be edited, cleared, and is large when it carries
-- an image. Keeping it out of users keeps that row about who somebody is and
-- how they authenticate, and keeps every reader of users from dragging a
-- possibly-large blob of HTML along with a login lookup.
--
-- One row per mailbox owner, which is the only principal that sends mail.
-- A shared mailbox has members but no single author, so it has no signature of
-- its own; a member composing from inside it sends without one rather than with
-- a signature chosen by whoever happens to be logged in. That is a real
-- limitation and it is the honest one: a shared signature would have to be
-- attributed to nobody in particular.

CREATE TABLE user_signatures (
    -- The mailbox that owns the signature. Deleting the user removes it with
    -- them: a signature is personal data, and leaving it behind for a reused
    -- address would put one person's name under another person's mail.
    owner   citext PRIMARY KEY REFERENCES users (address) ON DELETE CASCADE,
    -- The signature as HTML, which is what the user edits and what is appended
    -- to the HTML part of a message. Empty is a legitimate value and means the
    -- user has no signature; it is not the same as having no row, which means
    -- the default and not yet chosen.
    html    text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- A signature cannot be an unbounded blob. A megabyte of base64 in a table
    -- row that every send reads is a way to make sending slow, and a limit here
    -- is the only place that can be enforced for every writer at once. 256 KiB
    -- of HTML is far beyond a signature with a logo in it and below what a
    -- large pasted screenshot would reach.
    CONSTRAINT user_signatures_html_size CHECK (octet_length(html) <= 262144)
);

COMMENT ON TABLE user_signatures IS
    'The HTML signature appended to mail this owner sends. One row per mailbox owner; empty html means no signature.';

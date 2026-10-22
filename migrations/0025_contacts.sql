-- linchange 0025: address books, contacts, and sharing them.
--
-- Three kinds of address book, because they answer three different questions:
--
--   - personal: one per user, created on first use. "Who do I know?"
--   - shared:   a named book a person owns and offers to people or groups.
--               "Who do we know?" -- and it grants nothing until the person
--               it names accepts, because being handed a colleague's address
--               book unasked is not the same as being given it.
--   - organization: created by an administrator for a workgroup. Every member
--               of the organization can read it; it is the global directory.
--
-- A contact is stored as its vCard, which is what a CardDAV client reads and
-- writes, together with a searchable projection of the fields the web UI and
-- the composer need. The vCard is the source of truth; the columns are an
-- index over it, so the two cannot disagree about what a contact says.

BEGIN;

CREATE TABLE address_books (
    id           bigserial   PRIMARY KEY,
    owner        citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    -- Set only for an organization book; the book is visible to every member
    -- of that workgroup.
    workgroup_id bigint      REFERENCES workgroups (id) ON DELETE CASCADE,
    scope        text        NOT NULL CHECK (scope IN ('personal', 'shared', 'organization')),
    name         text        NOT NULL,
    -- The path segment a CardDAV client sees. It is unique per owner so two
    -- books of one person cannot address the same collection.
    slug         text        NOT NULL,
    description  text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (btrim(name) <> ''),
    CHECK (btrim(slug) <> ''),
    CHECK ((scope = 'organization') = (workgroup_id IS NOT NULL))
);

CREATE UNIQUE INDEX address_books_owner_slug_idx ON address_books (owner, slug);
-- Exactly one personal book per user, which is what "by default" means.
CREATE UNIQUE INDEX address_books_personal_idx
    ON address_books (owner) WHERE scope = 'personal';
-- Exactly one directory per organization, so "the organization address book"
-- is a single answer rather than a set an administrator could add to.
CREATE UNIQUE INDEX address_books_organization_idx
    ON address_books (workgroup_id) WHERE scope = 'organization';
CREATE INDEX address_books_workgroup_idx
    ON address_books (workgroup_id) WHERE workgroup_id IS NOT NULL;

CREATE TABLE contacts (
    id           bigserial   PRIMARY KEY,
    book_id      bigint      NOT NULL REFERENCES address_books (id) ON DELETE CASCADE,
    -- The vCard UID. Stable across edits, which is what a CardDAV client uses
    -- to recognise a contact it already has.
    uid          text        NOT NULL,
    vcard        text        NOT NULL,
    display_name text        NOT NULL DEFAULT '',
    -- The primary address, used to recognise somebody who writes to us and to
    -- attach a recipient list to the right card.
    email        citext      NOT NULL DEFAULT '',
    -- Lowercased name, addresses, phones and organization, concatenated for
    -- the composer's search. It is rebuilt whenever the card changes.
    search_text  text        NOT NULL DEFAULT '',
    organization text        NOT NULL DEFAULT '',
    title        text        NOT NULL DEFAULT '',
    notes        text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (book_id, uid)
);

CREATE INDEX contacts_book_idx ON contacts (book_id);
CREATE INDEX contacts_email_idx ON contacts (book_id, email);
CREATE INDEX contacts_search_idx ON contacts (book_id, lower(search_text));

CREATE TABLE address_book_shares (
    id               bigserial   PRIMARY KEY,
    book_id          bigint      NOT NULL REFERENCES address_books (id) ON DELETE CASCADE,
    subject_kind     text        NOT NULL CHECK (subject_kind IN ('user', 'group')),
    subject_address  citext      REFERENCES users (address) ON DELETE CASCADE,
    subject_group_id bigint      REFERENCES workgroup_groups (id) ON DELETE CASCADE,
    level            text        NOT NULL CHECK (level IN ('read', 'write')),
    -- A person's offer starts pending and grants nothing until the subject
    -- accepts. An organization book is readable without an offer at all.
    status           text        NOT NULL DEFAULT 'pending'
                                 CHECK (status IN ('pending', 'active', 'declined')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CHECK ((subject_kind = 'user') = (subject_address IS NOT NULL)),
    CHECK ((subject_kind = 'group') = (subject_group_id IS NOT NULL))
);

CREATE UNIQUE INDEX address_book_shares_user_idx
    ON address_book_shares (book_id, subject_address) WHERE subject_kind = 'user';
CREATE UNIQUE INDEX address_book_shares_group_idx
    ON address_book_shares (book_id, subject_group_id) WHERE subject_kind = 'group';
CREATE INDEX address_book_shares_book_idx ON address_book_shares (book_id);

CREATE TABLE address_book_share_decisions (
    share_id   bigint      NOT NULL REFERENCES address_book_shares (id) ON DELETE CASCADE,
    address    citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    decision   text        NOT NULL CHECK (decision IN ('accepted', 'declined')),
    decided_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (share_id, address)
);

CREATE INDEX address_book_share_decisions_address_idx
    ON address_book_share_decisions (address);

COMMIT;

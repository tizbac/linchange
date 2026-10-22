-- linchange 0029: presentation themes.
--
-- A theme is either one of the built-ins (light, dark, system) or a custom
-- theme uploaded by an administrator as a ZIP whose entry point is theme.css.
-- The ZIP lives in the blob store; this table holds the name and the prefix
-- (a ZIP may wrap its files in one top-level directory).
--
-- users.theme is a personal choice, workgroups.theme the organization default;
-- both are nullable so that "not chosen" falls through to the next level.

BEGIN;

CREATE TABLE themes (
    id          bigserial   PRIMARY KEY,
    uid         text        NOT NULL UNIQUE,
    name        text        NOT NULL,
    -- prefix is the directory inside the ZIP that holds theme.css, with a
    -- trailing slash, or empty when theme.css is at the root.
    prefix      text        NOT NULL DEFAULT '',
    blob_sha256 bytea       NOT NULL,
    created_by  citext      NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE users ADD COLUMN theme text;
ALTER TABLE workgroups ADD COLUMN theme text;

COMMIT;

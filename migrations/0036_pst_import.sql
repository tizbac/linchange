-- PST and OST imports. A PST describes its own items with stable identifiers,
-- so an import is keyed by (source, item) and re-running it over the same file
-- skips what already landed. The source is the file's SHA-256 for the owner:
-- two people importing the same archive each get their own copy, while one
-- person importing it twice gets it once.

CREATE TABLE pst_import_sources (
    id            bigserial PRIMARY KEY,
    owner         citext   NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    source_sha256 bytea    NOT NULL,
    file_name     text     NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner, source_sha256)
);

CREATE TABLE pst_import_items (
    id         bigserial PRIMARY KEY,
    source_id  bigint    NOT NULL REFERENCES pst_import_sources (id) ON DELETE CASCADE,
    item_key   text      NOT NULL,
    kind       text      NOT NULL,
    target     text      NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_id, item_key)
);

CREATE INDEX pst_import_items_source_idx ON pst_import_items (source_id, kind);

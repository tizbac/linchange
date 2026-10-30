-- Import sessions get the same progress fields the remote-IMAP jobs have, so a
-- browser import can be listed and watched after the tab that started it is
-- gone. The item ledger already makes a re-run safe; these columns only report.

ALTER TABLE pst_import_sources
    ADD COLUMN started_by        citext NOT NULL DEFAULT '',
    ADD COLUMN status            text   NOT NULL DEFAULT 'running'
                                 CHECK (status IN ('running', 'completed', 'failed')),
    ADD COLUMN calendar_id       bigint REFERENCES calendars (id) ON DELETE SET NULL,
    ADD COLUMN address_book_id   bigint REFERENCES address_books (id) ON DELETE SET NULL,
    ADD COLUMN messages_imported bigint NOT NULL DEFAULT 0,
    ADD COLUMN events_imported   bigint NOT NULL DEFAULT 0,
    ADD COLUMN contacts_imported bigint NOT NULL DEFAULT 0,
    ADD COLUMN items_skipped     bigint NOT NULL DEFAULT 0,
    ADD COLUMN bytes_imported    bigint NOT NULL DEFAULT 0,
    ADD COLUMN last_error        text,
    ADD COLUMN finished_at       timestamptz;

CREATE INDEX pst_import_sources_owner_idx ON pst_import_sources (owner, created_at DESC);

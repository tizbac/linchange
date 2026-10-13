-- Remote IMAP import jobs, in the shape the outbox uses: a state machine the
-- runner claims one row at a time, plus per-item progress so a job can be
-- cancelled and re-run without re-importing what already landed.

CREATE TABLE imap_import_jobs (
    id                bigserial PRIMARY KEY,
    owner             citext   NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    started_by        citext   NOT NULL,
    remote_host       text     NOT NULL,
    remote_port       integer  NOT NULL DEFAULT 993,
    remote_tls        text     NOT NULL DEFAULT 'tls'
                            CHECK (remote_tls IN ('tls', 'starttls', 'plain')),
    remote_username   text     NOT NULL,
    remote_password   bytea    NOT NULL, -- AES-GCM, key derived from auth encryption key
    accept_self_signed boolean NOT NULL DEFAULT false,
    source_root       text     NOT NULL DEFAULT '',
    target_root       text     NOT NULL DEFAULT '',
    status            text     NOT NULL DEFAULT 'queued'
                            CHECK (status IN ('queued', 'running', 'completed', 'cancelled', 'failed')),
    cancel_requested  boolean  NOT NULL DEFAULT false,
    last_error        text,
    folders_total     integer  NOT NULL DEFAULT 0,
    folders_done      integer  NOT NULL DEFAULT 0,
    messages_seen     bigint   NOT NULL DEFAULT 0,
    messages_imported bigint   NOT NULL DEFAULT 0,
    messages_skipped  bigint   NOT NULL DEFAULT 0,
    bytes_imported    bigint   NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    started_at        timestamptz,
    finished_at       timestamptz
);

CREATE INDEX imap_import_jobs_claim_idx ON imap_import_jobs (id) WHERE status = 'queued';
CREATE INDEX imap_import_jobs_owner_idx ON imap_import_jobs (owner, created_at DESC);

-- Per-message progress. A re-run of the same job skips a (folder, uid) that
-- already landed; the sha256 also lets a re-run of a *different* job skip a
-- message whose content is already in the target mailbox, which is checked
-- against messages.blob_sha256 at insert time.
CREATE TABLE imap_import_items (
    job_id            bigint   NOT NULL REFERENCES imap_import_jobs (id) ON DELETE CASCADE,
    remote_folder     text     NOT NULL,
    remote_uid        bigint   NOT NULL,
    remote_uidvalidity bigint  NOT NULL,
    target_mailbox_id bigint   NOT NULL,
    message_sha256    bytea    NOT NULL,
    imported_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, remote_folder, remote_uid)
);

-- linchange 0038: application passwords for mail, calendar and contact clients.
--
-- An application password is a credential of its own, named by the person who
-- made it (MailCraft on their computer, a phone, a tablet). It reaches the same
-- mailbox as the account, so it can be revoked without changing the account's
-- password, and a computer that is lost or compromised does not expose that
-- password. Only a SHA-256 digest is stored: the password is shown once.
--
-- The account password no longer logs in to the mail protocols by default
-- (auth.allow_main_password); application passwords are how a client signs in.
-- An organization may allow the account password for its own people instead;
-- that is a per-organization switch the administrator manages.
BEGIN;

ALTER TABLE workgroups
    ADD COLUMN legacy_mail_auth boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN workgroups.legacy_mail_auth IS
    'Whether the account password may sign in to IMAP, SMTP, CalDAV, CardDAV and the other mail protocols for addresses in this organization. Off by default; application passwords work either way.';

CREATE TABLE application_tokens (
    id           bigserial PRIMARY KEY,
    address      citext      NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    -- Public lookup part of the credential. It is not a secret: it names the
    -- row so verification does not have to scan every token of every account.
    prefix       text        NOT NULL UNIQUE,
    -- SHA-256 of the secret part. A 256-bit secret is not guessable offline, so
    -- a slow password hash would only make every login slower.
    token_hash   text        NOT NULL,
    description  text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz,
    CONSTRAINT application_tokens_prefix_not_empty CHECK (prefix <> ''),
    CONSTRAINT application_tokens_hash_not_empty CHECK (token_hash <> '')
);

CREATE INDEX application_tokens_address_idx
    ON application_tokens (address) WHERE revoked_at IS NULL;

COMMENT ON TABLE application_tokens IS
    'Application passwords for mail, calendar and contact clients. The account password is not stored or changed by these rows.';

-- A pending web approval for a client that is signing in. The browser signs in
-- with the account password and second factor; the client polls with the
-- device code and receives only the new application password.
CREATE TABLE application_token_requests (
    id               bigserial PRIMARY KEY,
    -- SHA-256 of the device code the client holds. The code itself is never stored.
    device_hash      text        NOT NULL UNIQUE,
    -- The short code the person types or follows in the browser.
    user_code        text        NOT NULL UNIQUE,
    description      text        NOT NULL DEFAULT '',
    -- Set when an authenticated person approves. NULL while pending or denied.
    address          citext      REFERENCES users (address) ON DELETE CASCADE,
    status           text        NOT NULL DEFAULT 'pending',
    interval_seconds integer     NOT NULL DEFAULT 5,
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    approved_at      timestamptz,
    consumed_at      timestamptz,
    CONSTRAINT application_token_requests_status CHECK (
        status IN ('pending', 'approved', 'denied', 'consumed')
    ),
    CONSTRAINT application_token_requests_interval_positive CHECK (interval_seconds > 0)
);

CREATE INDEX application_token_requests_expiry_idx
    ON application_token_requests (expires_at) WHERE status = 'pending';

COMMENT ON TABLE application_token_requests IS
    'A short-lived web approval through which a mail client obtains an application password without seeing the account password.';

COMMIT;

-- Authentication lockout state, per account and per source address.
--
-- 0001 put failed_attempts and locked_until on credentials. That cannot work on
-- its own: credentials rows exist only for local users and domain admins, so an
-- LDAP user had nowhere to record a failed attempt and no limit to slow a guess
-- against their account. Shared mailboxes do not authenticate at all, so they
-- need no row here either.
--
-- The state therefore moves to a table keyed by address, which covers every
-- principal that can actually authenticate, whatever its source.
--
-- The address table is deliberately keyed to a real user. Recording a row for an
-- address that does not exist would let anyone grow this table without bound by
-- submitting invented addresses, so unknown addresses are covered by the
-- per-address table below instead, which is pruned.

CREATE TABLE auth_attempts (
    address           citext     PRIMARY KEY REFERENCES users (address) ON DELETE CASCADE,
    -- Failures counted in the current run. Reset when the run ages out or when
    -- a lock is served.
    failed_attempts   integer    NOT NULL DEFAULT 0,
    -- When the current run started. A failure arriving after auth.lockout.window
    -- has elapsed starts a new run rather than joining this one.
    window_started_at timestamptz NOT NULL DEFAULT now(),
    last_failed_at    timestamptz NOT NULL DEFAULT now(),
    -- Non-null while locked. Cleared by serving the lock, not by expiry alone,
    -- so that a lock shorter than the window cannot re-lock on the next attempt.
    locked_until      timestamptz,
    CONSTRAINT auth_attempts_failed_non_negative CHECK (failed_attempts >= 0)
);

CREATE INDEX auth_attempts_locked_idx ON auth_attempts (locked_until) WHERE locked_until IS NOT NULL;

-- Per source address. Unlike the table above this one is not foreign keyed,
-- because an address that has never been seen is still worth counting against.
-- That means it can grow, so it needs pruning; see auth_ip_attempts_prune.
CREATE TABLE auth_ip_attempts (
    ip                inet       PRIMARY KEY,
    failed_attempts   integer    NOT NULL DEFAULT 0,
    window_started_at timestamptz NOT NULL DEFAULT now(),
    last_failed_at    timestamptz NOT NULL DEFAULT now(),
    locked_until      timestamptz,
    CONSTRAINT auth_ip_attempts_failed_non_negative CHECK (failed_attempts >= 0)
);

CREATE INDEX auth_ip_attempts_stale_idx ON auth_ip_attempts (last_failed_at);
CREATE INDEX auth_ip_attempts_locked_idx ON auth_ip_attempts (locked_until) WHERE locked_until IS NOT NULL;

-- Remove the per-account state that has moved to auth_attempts. The index went
-- with the column.
ALTER TABLE credentials DROP COLUMN failed_attempts;
ALTER TABLE credentials DROP COLUMN locked_until;

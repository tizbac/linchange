-- Removes a duplicate password-change flag and documents the recovery code
-- encoding.
--
-- 0001 carried must_change on credentials as well as must_change_password on
-- users. Two flags for one fact is two things that can disagree, and the ways
-- they disagree are both bad: a user who has just chosen a password gets forced
-- to choose it again, or a user who should be forced is not. Nothing was
-- reading the credentials copy, because LoadPrincipal and the session store both
-- read the users copy, so it was a column that could only ever be wrong.
--
-- users is the survivor because it is per principal rather than per credential.
-- A domain can require a change for a principal whose password is not held here
-- at all, and a session or an access token carries the users copy, so it is the
-- one that is already on the paths that enforce it.

ALTER TABLE credentials DROP COLUMN must_change;

-- The TOTP package produces raw SHA-256 digests, which have no text
-- representation of their own, and the column was left as text[] with the
-- encoding unstated. Base64 is what this repository writes and what it expects
-- to read, so a row written here is a row only this repository can interpret.
COMMENT ON COLUMN user_totp.recovery_codes IS
    'SHA-256 digests of the issued recovery codes, base64 encoded, one per '
    'unused code. The codes themselves are shown once and never stored.';

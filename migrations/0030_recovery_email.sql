-- linchange 0030: a recovery address on the account.
--
-- The value is what the profile page shows and what a future reset delivery can
-- use; it is deliberately separate from the mailbox being recovered, so losing
-- access to the mailbox does not also lose the way back in.

BEGIN;

ALTER TABLE users ADD COLUMN recovery_email citext;

COMMIT;

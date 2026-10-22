-- linchange 0026: allow an invitation guest into the meeting's chat/call room.
--
-- A meeting guest may have no account here. The room must still record them as
-- a member: membership is what authorises a chat message and a WebRTC signal,
-- and the invitation token is what proves the guest's address. The address is
-- therefore not a foreign key to users any more. A nullable user_address keeps
-- the old cascade for real accounts: deleting a user removes the memberships
-- that belonged to that account, while an external guest's row remains.
--
-- The member column stays citext, so existing comparisons and the socket's
-- membership checks are unchanged.

BEGIN;

ALTER TABLE chat_members DROP CONSTRAINT IF EXISTS chat_members_member_fkey;
ALTER TABLE chat_members ADD COLUMN IF NOT EXISTS user_address citext;

UPDATE chat_members m
SET user_address = u.address
FROM users u
WHERE u.address = m.member
  AND m.user_address IS NULL;

ALTER TABLE chat_members
    ADD CONSTRAINT chat_members_user_address_fkey
    FOREIGN KEY (user_address) REFERENCES users (address) ON DELETE CASCADE;

COMMENT ON COLUMN chat_members.member IS
    'Address that participates in the room. It may be an account holder or an external meeting guest.';
COMMENT ON COLUMN chat_members.user_address IS
    'Set when member is an account on this server; NULL for an external meeting guest.';

COMMIT;

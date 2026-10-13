-- The guard trigger was firing on both INSERT and UPDATE, which caused
-- ON CONFLICT DO UPDATE to fail when the member couldn't hold mail.
-- The Go validation in GrantSharedMailboxAccess already enforces this rule,
-- so the trigger only needs to guard INSERT.
ALTER TABLE shared_mailbox_grants DISABLE TRIGGER shared_mailbox_grants_member_guard;

DROP TRIGGER shared_mailbox_grants_member_guard ON shared_mailbox_grants;

CREATE TRIGGER shared_mailbox_grants_member_guard
    BEFORE INSERT ON shared_mailbox_grants
    FOR EACH ROW EXECUTE FUNCTION guard_shared_mailbox_grant();

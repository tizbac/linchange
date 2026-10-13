-- Contact groups: group domains together for contact isolation.
-- A user sees only mailboxes from domains in their contact group.

CREATE TABLE contact_groups (
    id              bigserial PRIMARY KEY,
    name            citext   NOT NULL UNIQUE,
    description     text     NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- Domains belong to a contact group.
ALTER TABLE domains ADD COLUMN contact_group_id bigint REFERENCES contact_groups (id) ON DELETE SET NULL;

-- Users inherit their domain's contact group.
ALTER TABLE users ADD COLUMN contact_group_id bigint REFERENCES contact_groups (id) ON DELETE SET NULL;

-- Index for querying users by contact group.
CREATE INDEX users_contact_group_idx ON users (contact_group_id) WHERE contact_group_id IS NOT NULL;

-- Trigger to set user's contact group from domain on insert/update.
CREATE OR REPLACE FUNCTION set_user_contact_group() RETURNS trigger AS $$
BEGIN
    IF NEW.contact_group_id IS NULL THEN
        SELECT contact_group_id INTO NEW.contact_group_id
        FROM domains WHERE name = split_part(NEW.address, '@', 2);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER user_contact_group_trigger
BEFORE INSERT OR UPDATE ON users
FOR EACH ROW EXECUTE FUNCTION set_user_contact_group();
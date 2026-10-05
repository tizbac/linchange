-- A calendar's name is a URL path segment in CalDAV, so it has to identify one
-- calendar, and it has to survive being put in a path at all.
--
-- Until now nothing enforced either. Two calendars for the same owner could
-- share a name, and a name could contain a slash, in which case a CalDAV request
-- for /caldav/alice@example.com/Work/Team/ would name no calendar at all -- it
-- would be a request for a resource in a collection that cannot exist.
--
-- The index is on the folded name because CalDAV lookups are case-insensitive:
-- "Work" and "work" are the same collection as far as a client is concerned, so
-- they must not be two. Without the fold the uniqueness would only hold for
-- clients that spelled every calendar the same way twice.
--
-- A backslash is refused alongside the slash. Clients that do not escape their
-- path segments treat one as a separator, so a name holding it addresses a
-- different resource depending on the client.
--
-- The existing names in a deployment that has been running are not touched here.
-- Where a duplicate exists the first by id wins the index and the others keep
-- working through the web interface, which addresses calendars by id; they are
-- simply not addressable by name until an operator renames one.
CREATE UNIQUE INDEX calendars_owner_name_idx
    ON calendars (owner, lower(name)) WHERE owner IS NOT NULL;
CREATE UNIQUE INDEX calendars_workgroup_name_idx
    ON calendars (workgroup_id, lower(name)) WHERE workgroup_id IS NOT NULL;

ALTER TABLE calendars
    ADD CONSTRAINT calendars_name_is_a_path_segment
    CHECK (name <> '' AND name !~ '[/\\]');

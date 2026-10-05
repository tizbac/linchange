-- A calendar's display name, which is not the same thing as its name.
--
-- The name is a URL path segment in CalDAV: it identifies the collection, and a
-- client that has been told one URL keeps using it for ever. The display name is a
-- label, and RFC 4918 §9.2 requires a client to be able to change it without
-- changing anything else about the resource. Collapsing the two means the first
-- rename a person makes in a calendar client moves the collection's URL, so every
-- other device subscribed to the old URL stops receiving it -- which surfaces as an
-- event disappearing from a phone rather than as a rename.
--
-- Empty means "the name is the label", which is the case for every calendar that
-- has not been renamed by a client. Nothing sets it except a PROPPATCH of
-- DAV:displayname, so an existing deployment keeps its calendars where they are.
ALTER TABLE calendars
    ADD COLUMN display_name text NOT NULL DEFAULT '';

-- A display name is shown to a person, so a blank one would be shown as a blank
-- row. Empty is allowed because empty is how the column says "there is no separate
-- label", and a check that forbade it would refuse the column's own default.
ALTER TABLE calendars
    ADD CONSTRAINT calendars_display_name_is_not_blank
    CHECK (display_name = '' OR btrim(display_name) <> '');
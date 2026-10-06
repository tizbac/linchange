-- The MS-OXPROPS item class of a message.
--
-- An Exchange client filters a folder by item class, and it draws a message
-- differently depending on the class it has: a meeting request gets a Respond
-- button, a distribution list gets an expandable row. The class cannot be derived
-- from the flags, because the two are not the same fact -- a message can be a
-- draft and a meeting request, and a client filtering on IPM.Schedule wants the
-- second of those whichever folder it happens to be filed in.

ALTER TABLE messages ADD COLUMN IF NOT EXISTS item_class text NOT NULL DEFAULT 'IPM.Note';

-- The item class is filtered on with a prefix, because a client filters on
-- IPM.Schedule expecting both requests and responses to match, so the index has
-- to answer a prefix rather than an equality.
CREATE INDEX IF NOT EXISTS messages_item_class_idx ON messages (mailbox_id, item_class);

COMMENT ON COLUMN messages.item_class IS
    'MS-OXPROPS item class: IPM.Note, IPM.Schedule.Meeting.Request, IPM.Contact.';
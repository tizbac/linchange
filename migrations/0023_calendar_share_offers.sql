-- linchange 0023: a calendar share made by a person is an offer until the
-- person it names says yes.
--
-- An administrator creating a team calendar and sharing it is an act of the
-- organization: the calendar exists to be shared, and there is nobody in
-- particular to ask. A colleague offering you their diary is a different
-- thing: it is addressed to you personally, and being handed access to a
-- calendar you never asked about is not the same as being given it. So:
--
--   - Every share carries a status. Shares made by an administrator start
--     active, as they always were. Shares made through the calendar owner's
--     own API start pending, and grant nothing at all until the subject
--     accepts.
--   - The answer is recorded per address, because a share may name a group.
--     One person's yes must not answer for the rest of the group: the offer
--     reaches each of them separately, and each of them decides.
--
-- Deciding again is possible, and so is re-offering. A declined offer can be
-- accepted later; a share whose owner offers it anew starts over as pending,
-- because what somebody accepted was the thing offered to them then.

BEGIN;

ALTER TABLE calendar_shares
    ADD COLUMN status text NOT NULL DEFAULT 'active';

ALTER TABLE calendar_shares
    ADD CONSTRAINT calendar_shares_status
        CHECK (status IN ('pending', 'active'));

CREATE TABLE calendar_share_decisions (
    share_id   bigint   NOT NULL REFERENCES calendar_shares (id) ON DELETE CASCADE,
    -- The person who answered, rather than the share's subject. A group
    -- offer is one row per member who answered, and their answer belongs to
    -- them alone: leaving the group takes them out of the share, not out of
    -- having decided.
    address    citext   NOT NULL REFERENCES users (address) ON DELETE CASCADE,
    decision   text     NOT NULL CHECK (decision IN ('accepted', 'declined')),
    decided_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (share_id, address)
);

-- The offers waiting for somebody are looked up by address, so that is the
-- index the lookup wants.
CREATE INDEX calendar_share_decisions_address_idx
    ON calendar_share_decisions (address);

COMMIT;

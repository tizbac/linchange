-- Inbound trust for a domain whose mail is filtered before it arrives.
--
-- An external anti-spam service needs two things, and both are per domain:
--
-- 1. The addresses it delivers from. When a domain's mail is filtered by a
--    third party, the same addresses must not be deliverable from anywhere
--    else, or the filter is only a suggestion.
-- 2. The verdict the filter writes into the message. That verdict is a header,
--    and every service spells it differently - "X-Spam: YES", "X-Spamstatus:
--    HAM", "Authentication-Results: ... spam=high" - so the header, the value
--    and the comparison are named by the administrator rather than hardcoded.
--
-- An empty antispam_networks array means "any source may deliver", which is
-- what every domain does today, so existing rows are unaffected.
ALTER TABLE domains ADD COLUMN antispam_networks text[] NOT NULL DEFAULT ARRAY[]::text[];

COMMENT ON COLUMN domains.antispam_networks IS
    'IP addresses or CIDR networks allowed to deliver inbound mail to this domain. An empty array allows any source.';

-- One header test with one outcome. A rule is deliberately smaller than a
-- mail_rules row: the delivery path needs "this header says junk", not a
-- condition tree, and a rule that could not be read by an outside filter's
-- documentation would be useless here.
CREATE TABLE domain_header_rules (
    id             bigserial   PRIMARY KEY,
    domain         citext      NOT NULL REFERENCES domains (name) ON DELETE CASCADE,
    -- Header names are compared case-insensitively, because a header's own
    -- name is case-insensitive in the protocol.
    header         text        NOT NULL,
    -- The text looked for in the header. Empty is only meaningful for the
    -- "exists" match, which asks whether the header is present at all.
    value          text        NOT NULL DEFAULT '',
    -- How the value is compared with the header's content.
    match          text        NOT NULL DEFAULT 'contains',
    -- What to do when the comparison matches.
    action         text        NOT NULL DEFAULT 'junk',
    -- Anti-spam verdicts are written in mixed case ("Yes", "YES", "no"), so
    -- the comparison is insensitive by default and the administrator can turn
    -- sensitivity on when a service really distinguishes the two.
    case_sensitive boolean     NOT NULL DEFAULT false,
    enabled        boolean     NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT domain_header_rules_header_present CHECK (trim(header) <> ''),
    CONSTRAINT domain_header_rules_match_valid CHECK (
        match IN ('contains', 'is', 'starts_with', 'ends_with', 'exists')
    ),
    CONSTRAINT domain_header_rules_action_valid CHECK (
        action IN ('junk', 'reject', 'discard')
    ),
    -- A rule with no value and a comparison that needs one would match every
    -- message carrying the header, which is never what "contains" was written
    -- to mean.
    CONSTRAINT domain_header_rules_value_present CHECK (
        match = 'exists' OR trim(value) <> ''
    )
);

-- The delivery path reads a domain's enabled rules on every inbound message.
CREATE INDEX domain_header_rules_domain_enabled_idx
    ON domain_header_rules (domain, enabled, id);

COMMENT ON TABLE domain_header_rules IS
    'Per-domain header tests applied to inbound mail, usually the verdict of an external anti-spam service.';

-- Link history (ADR 11). Every statement is idempotent; old rows keep NULLs.

-- The proposal an asserted link promoted.
ALTER TABLE identity_link ADD COLUMN IF NOT EXISTS promoted_from VARCHAR;
-- The link_decision.id that opened or closed the row, for a person's decision.
ALTER TABLE identity_link ADD COLUMN IF NOT EXISTS open_decision VARCHAR;
ALTER TABLE identity_link ADD COLUMN IF NOT EXISTS close_decision VARCHAR;
-- What opening or closing an asserted row did to the clusters: joined, split,
-- none, or refused (an opening same_as the clustering did not apply). NULL for
-- proposals, and for close_effect while the row is open.
ALTER TABLE identity_link ADD COLUMN IF NOT EXISTS open_effect VARCHAR;
ALTER TABLE identity_link ADD COLUMN IF NOT EXISTS close_effect VARCHAR;

-- Identity links between records (ADR 7). Every statement is idempotent.

-- Files made before ADR 7 get cluster_id (each record its own cluster) and
-- lose merged_into, which nothing reads any more.
ALTER TABLE entity ADD COLUMN IF NOT EXISTS cluster_id VARCHAR;
UPDATE entity SET cluster_id = id WHERE cluster_id IS NULL;
ALTER TABLE entity DROP COLUMN IF EXISTS merged_into;

-- A same_as or distinct_from link between records a < b. At most one open
-- link (valid_to NULL) per pair and kind; only asserted same_as joins
-- clusters.
CREATE TABLE IF NOT EXISTS identity_link (
    id         VARCHAR PRIMARY KEY,
    a          VARCHAR NOT NULL,
    b          VARCHAR NOT NULL,
    kind       VARCHAR NOT NULL,      -- same_as or distinct_from
    status     VARCHAR NOT NULL,      -- asserted or proposed
    rule       VARCHAR NOT NULL,      -- linked_ids, shared_key, a settings rule, a matcher
    valid_from TIMESTAMPTZ NOT NULL,
    valid_to   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS identity_link_a ON identity_link (a);
CREATE INDEX IF NOT EXISTS identity_link_b ON identity_link (b);

-- The evidence for (or against) each link over time.
CREATE TABLE IF NOT EXISTS link_deposit (
    link_id        VARCHAR NOT NULL,
    deposited_at   TIMESTAMPTZ NOT NULL,
    weight         DOUBLE NOT NULL,
    against        BOOLEAN NOT NULL,
    observation_id VARCHAR,
    features       JSON               -- a matcher's features and contributions
);
CREATE INDEX IF NOT EXISTS link_deposit_link ON link_deposit (link_id);

-- Who decided, for a person's decision; NULL for observations and matchers.
-- Files made before this column was added get it here.
ALTER TABLE link_deposit ADD COLUMN IF NOT EXISTS actor VARCHAR;

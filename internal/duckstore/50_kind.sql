-- The vocabulary the server last booted with (ADR 10), so reads resolve
-- parents and shapes after the config drops a kind. Written by
-- SyncVocabulary; every statement is idempotent.

CREATE TABLE IF NOT EXISTS kind (
    name       VARCHAR PRIMARY KEY,
    parent     VARCHAR,               -- NULL: no parent
    origin     VARCHAR NOT NULL,      -- 'core' or 'configured'
    added_at   TIMESTAMPTZ NOT NULL,
    retired_at TIMESTAMPTZ            -- NULL: active
);

-- Attribute and relation shapes. domain and range are NULL for any kind.
CREATE TABLE IF NOT EXISTS kind_predicate (
    name        VARCHAR PRIMARY KEY,
    relation    BOOLEAN NOT NULL,
    type        VARCHAR,              -- attribute value type; relations have none
    cardinality VARCHAR NOT NULL,
    conflict    VARCHAR NOT NULL,
    domain      VARCHAR[],
    range       VARCHAR[]
);

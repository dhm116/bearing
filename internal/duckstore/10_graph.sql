-- The store's tables (ADR 6). Applied on Open; every statement is
-- idempotent so a reopened file keeps its data.

-- Every observation as received. Append-only: a repeated id is skipped.
CREATE TABLE IF NOT EXISTS observation (
    id          VARCHAR PRIMARY KEY,
    source      VARCHAR NOT NULL,     -- the configured source, e.g. github-acme
    kind        VARCHAR NOT NULL,
    key         VARCHAR NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL,
    url         VARCHAR,              -- evidence link, when the adapter gave one
    payload     JSON NOT NULL         -- the observation's data as ProtoJSON
);

-- One row per thing. merged_into is set when two entities turn out to be
-- the same; the survivor holds every key and fact from then on.
CREATE TABLE IF NOT EXISTS entity (
    id          VARCHAR PRIMARY KEY,
    kind        VARCHAR NOT NULL,
    name        VARCHAR NOT NULL,
    merged_into VARCHAR,
    created_at  TIMESTAMPTZ NOT NULL
);

-- Every key that names an entity.
CREATE TABLE IF NOT EXISTS alias (
    key        VARCHAR PRIMARY KEY,
    entity_id  VARCHAR NOT NULL,
    source     VARCHAR NOT NULL,
    first_seen TIMESTAMPTZ NOT NULL,
    last_seen  TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS alias_entity ON alias (entity_id);

-- A statement about an entity over [valid_from, valid_to). Exactly one of
-- object_id (a relation) and value (an attribute) is set. valid_to is NULL
-- while the fact holds.
CREATE TABLE IF NOT EXISTS fact (
    id         VARCHAR PRIMARY KEY,
    subject_id VARCHAR NOT NULL,
    predicate  VARCHAR NOT NULL,
    object_id  VARCHAR,
    value      JSON,
    status     VARCHAR NOT NULL,      -- asserted or proposed
    confidence DOUBLE NOT NULL,
    valid_from TIMESTAMPTZ NOT NULL,
    valid_to   TIMESTAMPTZ,
    source     VARCHAR NOT NULL       -- the source whose snapshot can close it
);
CREATE INDEX IF NOT EXISTS fact_subject ON fact (subject_id, predicate);
CREATE INDEX IF NOT EXISTS fact_object ON fact (object_id, predicate);

-- The observations behind each fact: its provenance.
CREATE TABLE IF NOT EXISTS fact_source (
    fact_id        VARCHAR NOT NULL,
    observation_id VARCHAR NOT NULL,
    PRIMARY KEY (fact_id, observation_id)
);

-- The vocabulary as data (ADR 13, spike). Beside kind and kind_predicate,
-- which the ADR would replace. Every statement is idempotent.

-- Who owns each namespace, and where its module came from.
CREATE TABLE IF NOT EXISTS prefix (
    namespace   VARCHAR PRIMARY KEY,  -- full, such as io.k8s
    owner       VARCHAR NOT NULL,     -- core, adapter or operator
    source      VARCHAR NOT NULL,     -- the configured source that published it; '' for core and operator
    version     VARCHAR NOT NULL,     -- the module version last loaded
    repo        VARCHAR,
    ref         VARCHAR,              -- the commit
    sha256      VARCHAR,
    verified_by VARCHAR               -- build, commit or sha256
);

-- One row per term. A term only grows: its shape never changes and it is
-- never dropped, only deprecated.
CREATE TABLE IF NOT EXISTS term (
    name        VARCHAR PRIMARY KEY,  -- full: io.k8s:Deployment
    namespace   VARCHAR NOT NULL,
    is_a        VARCHAR NOT NULL,     -- kind, relation or attribute
    shape       VARCHAR NOT NULL,     -- what grow-only compares
    version     VARCHAR NOT NULL,     -- the module version that added it
    deprecated  JSON,
    added_at    TIMESTAMPTZ NOT NULL
);

-- Mappings and rules: the shape of identity_link. One row per version and
-- status; valid_to closes a row when it is decided or superseded.
CREATE TABLE IF NOT EXISTS mapping (
    id            VARCHAR NOT NULL,
    version       INTEGER NOT NULL,
    subject       VARCHAR NOT NULL,
    predicate     VARCHAR NOT NULL,   -- broader, exact_match, close_match, derives
    object        VARCHAR NOT NULL,
    target        JSON,               -- a derives rule's object kind and key template
    guess         BOOLEAN NOT NULL DEFAULT false,
    status        VARCHAR NOT NULL,   -- proposed, asserted or rejected
    author        VARCHAR NOT NULL,
    justification VARCHAR NOT NULL,
    valid_from    TIMESTAMPTZ NOT NULL,
    valid_to      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS mapping_id ON mapping (id, version);

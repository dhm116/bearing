-- The vocabulary the server last booted with (ADR 13), so reads resolve
-- parents and shapes after the config drops a kind, and a term only grows.
-- Written by SyncVocabulary; every statement is idempotent.

-- One row per loaded module: who publishes the namespace, and from where.
CREATE TABLE IF NOT EXISTS prefix (
    namespace   VARCHAR PRIMARY KEY,  -- such as com.dougmccall.bearing.core
    owner       VARCHAR NOT NULL,     -- core, adapter or operator
    version     VARCHAR NOT NULL,     -- the module version last loaded
    repo        VARCHAR,
    path        VARCHAR,
    commit_sha  VARCHAR,              -- the commit it was read or built at
    sha256      VARCHAR,
    verified_by VARCHAR NOT NULL      -- the check that passed: build for the core
);

-- One row per kind, relation and attribute. name is the stored name: bare
-- for a core term (owned_by), namespace:Term otherwise (io.k8s:Service).
-- Predicates adapters and settings declare are bare and read as core terms
-- until adapters publish modules. domain and range are NULL for any kind.
CREATE TABLE IF NOT EXISTS term (
    name        VARCHAR PRIMARY KEY,
    namespace   VARCHAR NOT NULL,
    type        VARCHAR NOT NULL,     -- a TermType name
    origin      VARCHAR NOT NULL,     -- 'core' (the core module) or 'configured'
    parent      VARCHAR,              -- kinds only; NULL: no parent
    value_type  VARCHAR,              -- attributes only
    cardinality VARCHAR,              -- predicates only
    conflict    VARCHAR,              -- predicates only
    domain      VARCHAR[],
    range       VARCHAR[],
    added_at    TIMESTAMPTZ NOT NULL,
    retired_at  TIMESTAMPTZ           -- configured kinds only; NULL: active
);

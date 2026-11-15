-- Proposed closes of facts (ADR 13: a derived fact closes on evidence).
-- Another adapter's observation can only support closing a fact, so the
-- close is a proposal, shaped like identity_link, that a person decides.
-- The fact itself is untouched until then. Every statement is idempotent.
CREATE TABLE IF NOT EXISTS fact_close (
    id         VARCHAR PRIMARY KEY,
    fact_id    VARCHAR NOT NULL,      -- the fact it would close
    cause      VARCHAR NOT NULL,      -- the record whose change supports it (a pull request)
    cause_fact VARCHAR NOT NULL,      -- the proposed fact saying cause removed the subject
    rule       VARCHAR NOT NULL,      -- the proposer, id@version
    close_at   TIMESTAMPTZ NOT NULL,  -- when the change took effect; confirming closes the fact here
    status     VARCHAR NOT NULL,      -- proposed, asserted, rejected, contradicted or superseded
    valid_from TIMESTAMPTZ NOT NULL,
    valid_to   TIMESTAMPTZ,           -- set when decided, contradicted or superseded
    against    VARCHAR,               -- the observation that contradicted it
    decision   VARCHAR,               -- the person's decision (its event ID), actor and reason
    actor      VARCHAR,
    reason     VARCHAR
);
-- One proposal per fact and cause, whatever became of it: the same evidence
-- never proposes again.
CREATE UNIQUE INDEX IF NOT EXISTS fact_close_cause ON fact_close (fact_id, cause);

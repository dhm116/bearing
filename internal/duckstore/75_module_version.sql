-- The adapter module version an observation was made under (ADR 13), such
-- as io.k8s@0.4.0. Outside the payload and the id, so deduplication is
-- unchanged; NULL for adapters that publish no module.
ALTER TABLE observation ADD COLUMN IF NOT EXISTS module_version VARCHAR;

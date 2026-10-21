-- A relation's qualifiers (protocol, port, ...): details of the fact that do
-- not make a new one. NULL when it has none. Adds the column to files made
-- before this one.
ALTER TABLE fact ADD COLUMN IF NOT EXISTS qualifiers JSON;

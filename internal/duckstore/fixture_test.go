package duckstore

import "testing"

// load runs plain SQL against the store, so read tests don't depend on Ingest.
func load(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

// loadAcme loads a small world: team payments (t-pay), people Alice and Bob,
// services api, ledger and db. api depends on ledger, ledger on db and db on
// ledger (a cycle). db was owned by the team until March; a proposed fact
// says Alice owns it. api's version changed on 2026-06-01. s-old is a second
// record in s-api's cluster.
func loadAcme(t *testing.T, s *Store) {
	t.Helper()
	load(t, s, `
INSERT INTO entity VALUES
 ('t-pay', 'Team', 'payments', TIMESTAMPTZ '2026-01-01 00:00:00+00', 't-pay'),
 ('p-alice', 'Person', 'Alice', TIMESTAMPTZ '2026-01-01 00:00:00+00', 'p-alice'),
 ('p-bob', 'Person', 'Bob', TIMESTAMPTZ '2026-01-01 00:00:00+00', 'p-bob'),
 ('s-api', 'Service', 'api', TIMESTAMPTZ '2026-01-01 00:00:00+00', 's-api'),
 ('s-ledger', 'Service', 'ledger', TIMESTAMPTZ '2026-01-01 00:00:00+00', 's-ledger'),
 ('s-db', 'Service', 'db', TIMESTAMPTZ '2026-01-01 00:00:00+00', 's-db'),
 ('s-old', 'Service', 'api-legacy', TIMESTAMPTZ '2026-01-02 00:00:00+00', 's-api');
INSERT INTO identity_link VALUES
 ('l1', 's-api', 's-old', 'same_as', 'asserted', 'shared_key', TIMESTAMPTZ '2026-01-02 00:00:00+00', NULL);
INSERT INTO alias VALUES
 ('team:payments', 't-pay', 'dir', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 00:00:00+00'),
 ('person:alice', 'p-alice', 'dir', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 00:00:00+00'),
 ('person:bob', 'p-bob', 'dir', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 00:00:00+00'),
 ('svc:api', 's-api', 'gh', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 00:00:00+00'),
 ('svc:api-old', 's-api', 'gh', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 00:00:00+00'),
 ('svc:legacy-api', 's-old', 'gh', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 00:00:00+00'),
 ('svc:ledger', 's-ledger', 'gh', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 00:00:00+00'),
 ('svc:db', 's-db', 'gh', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 00:00:00+00');
INSERT INTO observation VALUES
 ('o1', 'gh', 'Service', 'svc:api', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 00:00:01+00', 'https://example.test/o1', '{}'),
 ('o2', 'dir', 'Person', 'person:bob', TIMESTAMPTZ '2026-02-01 00:00:00+00', TIMESTAMPTZ '2026-02-01 00:00:01+00', NULL, '{}'),
 ('o3', 'gh', 'Service', 'svc:api', TIMESTAMPTZ '2026-06-01 00:00:00+00', TIMESTAMPTZ '2026-06-01 00:00:01+00', NULL, '{}');
INSERT INTO fact (id, subject_id, predicate, object_id, value, status, confidence, valid_from, valid_to, source) VALUES
 ('f01', 's-api', 'depends_on', 's-ledger', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'gh'),
 ('f02', 's-ledger', 'depends_on', 's-db', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'gh'),
 ('f03', 's-db', 'depends_on', 's-ledger', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'gh'),
 ('f04', 's-api', 'owned_by', 't-pay', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'gh'),
 ('f05', 's-ledger', 'owned_by', 't-pay', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'gh'),
 ('f06', 's-db', 'owned_by', 't-pay', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-03-01 00:00:00+00', 'gh'),
 ('f07', 's-db', 'owned_by', 'p-alice', NULL, 'proposed', 0.5, TIMESTAMPTZ '2026-04-01 00:00:00+00', NULL, 'gh'),
 ('f08', 's-api', 'language', NULL, '"go"', 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'gh'),
 ('f09', 's-api', 'version', NULL, '"1.21"', 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-06-01 00:00:00+00', 'gh'),
 ('f10', 's-api', 'version', NULL, '"1.22"', 'asserted', 1, TIMESTAMPTZ '2026-06-01 00:00:00+00', NULL, 'gh'),
 ('f11', 's-ledger', 'language', NULL, '"java"', 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'gh'),
 ('f12', 'p-alice', 'member_of', 't-pay', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'dir'),
 ('f13', 'p-bob', 'member_of', 't-pay', NULL, 'asserted', 1, TIMESTAMPTZ '2026-02-01 00:00:00+00', NULL, 'dir');
INSERT INTO fact_source VALUES
 ('f01', 'o3'), ('f01', 'o1'), ('f02', 'o1'), ('f03', 'o1'), ('f04', 'o1'), ('f05', 'o1'),
 ('f06', 'o1'), ('f07', 'o1'), ('f08', 'o1'), ('f09', 'o1'), ('f10', 'o3'), ('f11', 'o1'),
 ('f12', 'o1'), ('f13', 'o2');`)
}

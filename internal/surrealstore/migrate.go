package surrealstore

import (
	"context"
	"fmt"
	"strings"
)

// A migration is one step of the database schema. Steps run in order, each
// in its own transaction that also records it in schema_version, so a
// database always says which steps it has. Never edit a step that has
// shipped; add the next one. Every statement in a step is idempotent
// (IF NOT EXISTS, INSERT IGNORE), so two stores opening one database at
// once cannot clash: the loser finds its step already recorded.
type migration struct {
	version int
	name    string
	sql     string
}

var migrations = []migration{
	{1, "vector points", `
DEFINE TABLE IF NOT EXISTS vector SCHEMALESS;
DEFINE INDEX IF NOT EXISTS vector_subject ON vector FIELDS subject;`},

	// The graph is stored as rows the Go side reads and writes in whole
	// transactions; the tables below are all it needs.
	//
	//   meta:graph        the head (µs since the epoch, 0 when empty), the last
	//                     minted subject ID, the merge and journal counts and the
	//                     token of the latest large apply to stage rows
	//   subject           one per minted subject; sid is its ID
	//   merge             one per merge record, keyed by its sequence number
	//   series            one per bitemporal series (bindings, supports, facts,
	//                     state): tbl and key
	//   series_subject    which subjects a series names, to find series by subject
	//   version           the rows of a series: tbl, key and n
	//   journal           one per apply, keyed by its sequence number
	//   processed_event   one per applied event ID: the mark that makes Apply
	//                     idempotent
	//
	// subject, series, series_subject and version rows carry rec (µs), the
	// record time of the apply that wrote them, and stg, a token naming that
	// attempt. A row with rec after the head belongs to an apply that has not
	// committed, and readers skip it; see commit in apply.go.
	{2, "graph store", `
DEFINE TABLE IF NOT EXISTS meta SCHEMALESS;
INSERT IGNORE INTO meta { id: 'graph', head: 0, last_id: '', merges: 0, journal: 0, staged: '' };
DEFINE TABLE IF NOT EXISTS subject SCHEMALESS;
DEFINE INDEX IF NOT EXISTS subject_sid ON subject FIELDS sid;
DEFINE INDEX IF NOT EXISTS subject_rec ON subject FIELDS rec;
DEFINE TABLE IF NOT EXISTS merge SCHEMALESS;
DEFINE INDEX IF NOT EXISTS merge_seq ON merge FIELDS seq;
DEFINE TABLE IF NOT EXISTS series SCHEMALESS;
DEFINE INDEX IF NOT EXISTS series_key ON series FIELDS tbl, key;
DEFINE INDEX IF NOT EXISTS series_predicate ON series FIELDS tbl, predicate;
DEFINE INDEX IF NOT EXISTS series_rec ON series FIELDS rec;
DEFINE TABLE IF NOT EXISTS series_subject SCHEMALESS;
DEFINE INDEX IF NOT EXISTS series_subject_lookup ON series_subject FIELDS tbl, subject;
DEFINE INDEX IF NOT EXISTS series_subject_rec ON series_subject FIELDS rec;
DEFINE TABLE IF NOT EXISTS version SCHEMALESS;
DEFINE INDEX IF NOT EXISTS version_key ON version FIELDS tbl, key;
DEFINE INDEX IF NOT EXISTS version_rec ON version FIELDS rec;
DEFINE TABLE IF NOT EXISTS journal SCHEMALESS;
DEFINE INDEX IF NOT EXISTS journal_seq ON journal FIELDS seq;
DEFINE TABLE IF NOT EXISTS processed_event SCHEMALESS;`},

	// Un-merge records, one per un-merge, keyed by sequence number like
	// merge records. A database written before this step has none for the
	// un-merges it already recorded.
	{3, "unmerge records", `
DEFINE TABLE IF NOT EXISTS unmerge SCHEMALESS;
DEFINE INDEX IF NOT EXISTS unmerge_seq ON unmerge FIELDS seq;
UPDATE meta:graph SET unmerges = 0 WHERE unmerges = NONE;`},
}

// schemaVersion is the newest step this build knows.
func schemaVersion() int { return migrations[len(migrations)-1].version }

// migrate brings the database to schemaVersion. It refuses a database a
// newer build has migrated, which this build would misread.
func migrate(ctx context.Context, q Querier) error {
	if _, err := q.Query(ctx, `DEFINE TABLE IF NOT EXISTS schema_version SCHEMALESS`, nil); err != nil {
		return fmt.Errorf("define schema_version: %w", err)
	}
	res, err := q.Query(ctx, `SELECT VALUE version FROM schema_version`, nil)
	if err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	var applied []int
	if err := decode(res[0], &applied); err != nil {
		return fmt.Errorf("decode schema_version: %w", err)
	}
	have := 0
	for _, v := range applied {
		have = max(have, v)
	}
	if have > schemaVersion() {
		return fmt.Errorf("database schema is version %d; this build reads up to version %d", have, schemaVersion())
	}
	for _, m := range migrations {
		if m.version <= have {
			continue
		}
		// A second store may have recorded this step since we read; the
		// CREATE then fails, and so does the transaction, which is fine as
		// long as the step is there.
		sql := "BEGIN TRANSACTION;\n" + m.sql + "\n" + `CREATE type::record('schema_version', $v) CONTENT { version: $v, name: $name };` + "\nCOMMIT TRANSACTION;"
		if _, err := q.Query(ctx, sql, map[string]any{"v": m.version, "name": m.name}); err != nil {
			if recorded(ctx, q, m.version) {
				continue
			}
			return fmt.Errorf("migration %d (%s): %w", m.version, strings.TrimSpace(m.name), err)
		}
	}
	return nil
}

func recorded(ctx context.Context, q Querier, version int) bool {
	res, err := q.Query(ctx, `SELECT VALUE version FROM schema_version WHERE version = $v`, map[string]any{"v": version})
	if err != nil {
		return false
	}
	var got []int
	return decode(res[0], &got) == nil && len(got) == 1
}

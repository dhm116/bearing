package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// A migration is one step of the database schema. Steps run in order, in one
// transaction that also records them in schema_version, so a database
// always says which steps it has. Never edit a step that has shipped; add
// the next one. A lock held for the transaction makes two stores opening one
// database at once take turns: the second finds the steps recorded.
type migration struct {
	version int
	name    string
	sql     string
}

var migrations = []migration{
	// The graph is stored as rows that the Go side reads and writes in whole
	// transactions; the tables below are all it needs. Keys of series are
	// bytea, not text, because a key can hold a NUL byte (a support's key is
	// its source and its fact key, separated by one) and can be long, and
	// each series is found by kid, the SHA-256 of its key, so no index entry
	// is large.
	//
	//   meta             one row: the head (µs since the epoch, 0 when empty),
	//                    the last minted subject ID, the merge, un-merge and
	//                    journal counts, and restoring, set while a Restore
	//                    runs. Apply takes this row's lock.
	//   subject          one per minted subject
	//   merge_record     one per merge record, by its sequence number
	//   unmerge_record   one per un-merge record, by its sequence number
	//   component        one per subject that any merge record ever joined to
	//                    another: comp is the lowest subject ID of its
	//                    component. Components only grow (an un-merge ends a
	//                    merge record, not the link), so rows are relabelled
	//                    when two components join and never split.
	//   series           one per bitemporal series (bindings, supports, facts,
	//                    conflicts, issues, state): tbl and kid
	//   series_subject   which subjects a series names, to find series by
	//                    subject
	//   version          the rows of a series: tbl, kid and n
	//   journal          one per apply, by its sequence number; event is
	//                    unique, which is what makes Apply idempotent
	{1, "graph store", `
CREATE TABLE meta (
	id        boolean PRIMARY KEY DEFAULT true CHECK (id),
	head      bigint  NOT NULL,
	last_id   text    NOT NULL,
	merges    bigint  NOT NULL,
	unmerges  bigint  NOT NULL,
	journal   bigint  NOT NULL,
	restoring boolean NOT NULL DEFAULT false
);
INSERT INTO meta (head, last_id, merges, unmerges, journal) VALUES (0, '', 0, 0, 0);

CREATE TABLE subject (
	sid  text  PRIMARY KEY,
	data bytea NOT NULL
);

CREATE TABLE merge_record (
	seq      bigint PRIMARY KEY,
	survivor text   NOT NULL,
	merged   text   NOT NULL,
	data     bytea  NOT NULL
);
CREATE INDEX merge_record_survivor ON merge_record (survivor);
CREATE INDEX merge_record_merged ON merge_record (merged);

CREATE TABLE unmerge_record (
	seq     bigint PRIMARY KEY,
	subject text   NOT NULL,
	target  text   NOT NULL,
	data    bytea  NOT NULL
);
CREATE INDEX unmerge_record_subject ON unmerge_record (subject);
CREATE INDEX unmerge_record_target ON unmerge_record (target);

CREATE TABLE component (
	subject text PRIMARY KEY,
	comp    text NOT NULL
);
CREATE INDEX component_comp ON component (comp);

CREATE TABLE series (
	tbl       smallint NOT NULL,
	kid       bytea    NOT NULL,
	key       bytea    NOT NULL,
	predicate bytea    NOT NULL,
	head      bytea    NOT NULL,
	PRIMARY KEY (tbl, kid)
);
CREATE INDEX series_predicate ON series (tbl, predicate);

CREATE TABLE series_subject (
	tbl     smallint NOT NULL,
	subject text     NOT NULL,
	kid     bytea    NOT NULL,
	PRIMARY KEY (tbl, subject, kid)
);

CREATE TABLE version (
	tbl  smallint NOT NULL,
	kid  bytea    NOT NULL,
	n    integer  NOT NULL,
	rec  bigint   NOT NULL,
	ret  bigint   NOT NULL,
	data bytea    NOT NULL,
	PRIMARY KEY (tbl, kid, n)
);

CREATE TABLE journal (
	seq   bigint PRIMARY KEY,
	event text   NOT NULL UNIQUE,
	data  bytea  NOT NULL
);`},
}

// schemaVersion is the newest step this build knows.
func schemaVersion() int { return migrations[len(migrations)-1].version }

// migrate brings schema to schemaVersion, creating it if it is missing. It
// refuses a database a newer build has migrated, which this build would
// misread.
func migrate(ctx context.Context, db pool, schema string) error {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", cleanError(err))
	}
	defer rollback(tx)
	// The lock is per schema and ends with the transaction.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "bearing:migrate:"+schema); err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&exists); err != nil {
		return fmt.Errorf("look for schema: %w", err)
	}
	if !exists {
		// CREATE SCHEMA needs a privilege on the database even when the
		// schema is there, so it is created only when it is missing. The
		// name passed schemaPattern, and format quotes it.
		var create string
		if err := tx.QueryRow(ctx, `SELECT format('CREATE SCHEMA %I', $1::text)`, schema).Scan(&create); err != nil {
			return fmt.Errorf("quote schema: %w", err)
		}
		if _, err := tx.Exec(ctx, create); err != nil {
			return fmt.Errorf("create schema %s (a role without the right to, as Provision makes, needs it created for it): %w", schema, err)
		}
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version integer PRIMARY KEY, name text NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	have := 0
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_version`).Scan(&have); err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	if have > schemaVersion() {
		return fmt.Errorf("database schema is version %d; this build reads up to version %d", have, schemaVersion())
	}
	for _, m := range migrations {
		if m.version <= have {
			continue
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_version (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

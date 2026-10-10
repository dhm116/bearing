package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
)

// statement is a query the database ran during a measurement, with what it
// cost in total.
type statement struct {
	Calls      int64   `json:"calls"`
	TotalMS    float64 `json:"total_ms"`
	MeanMS     float64 `json:"mean_ms"`
	Rows       int64   `json:"rows"`
	SharedRead int64   `json:"shared_blks_read"`
	SharedHit  int64   `json:"shared_blks_hit"`
	Query      string  `json:"query"`
	// Plan is the generic plan of the query (EXPLAIN (GENERIC_PLAN)).
	Plan []string `json:"generic_plan,omitempty"`
}

// resetStatements clears pg_stat_statements, so the next topStatements covers
// what ran after it. It reports whether the extension answered.
func (p *pg) resetStatements(ctx context.Context) bool {
	if p == nil {
		return false
	}
	_, err := p.conn.Exec(ctx, `SELECT pg_stat_statements_reset()`)
	return err == nil
}

// topStatements returns the n statements that took the most time since the
// last reset, with their generic plans.
func (p *pg) topStatements(ctx context.Context, n int) ([]statement, error) {
	if p == nil {
		return nil, nil
	}
	rows, err := p.conn.Query(ctx, `
SELECT calls, total_exec_time, mean_exec_time, rows, shared_blks_read, shared_blks_hit, query
FROM pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND query !~* '^(begin|commit|rollback|show|set|savepoint|release|select pg_|vacuum|analyze|explain)'
  AND query NOT LIKE '%pg_stat_%' AND query NOT LIKE '%pg_class%'
  AND query !~* '(password|secret|create role|alter role)'
ORDER BY total_exec_time DESC LIMIT $1`, n)
	if err != nil {
		return nil, nil //nolint:nilerr // pg_stat_statements is optional
	}
	var out []statement
	for rows.Next() {
		var s statement
		if err := rows.Scan(&s.Calls, &s.TotalMS, &s.MeanMS, &s.Rows, &s.SharedRead, &s.SharedHit, &s.Query); err != nil {
			rows.Close()
			return nil, err
		}
		s.Query = strings.Join(strings.Fields(s.Query), " ")
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Plan = p.genericPlan(ctx, out[i].Query)
	}
	return out, nil
}

// genericPlan explains a statement with parameters left unbound. Statements
// that cannot be explained that way return nothing.
func (p *pg) genericPlan(ctx context.Context, q string) []string {
	rows, err := p.conn.Query(ctx, "EXPLAIN (GENERIC_PLAN) "+q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if rows.Scan(&l) != nil {
			return nil
		}
		out = append(out, l)
	}
	return out
}

// plan is the executed plan of one of the store's load queries.
type plan struct {
	Name string   `json:"name"`
	SQL  string   `json:"sql"`
	Plan []string `json:"plan"`
	// ExecutionMS is the time the plan reports.
	ExecutionMS float64 `json:"execution_ms"`
}

// loadQueries are the statements internal/pgstore's loaders send for the
// reads measured here (load.go), with the arguments of a sampled subject.
// They are copied, so the plans can be run with real arguments; the report
// checks that each still appears among the database's statements.
func (w *world) loadQueries(rs *readSet) []struct {
	name, sql string
	args      []any
} {
	kid := func(key string) []byte { h := sha256.Sum256([]byte(key)); return h[:] }
	repo, person := string(rs.repoID[0]), string(rs.personID[0])
	return []struct {
		name, sql string
		args      []any
	}{
		{"ResolveKey: the alias's series", `SELECT s.kid, s.key, s.head FROM series s WHERE s.tbl = $1 AND (s.kid = ANY($2))`, []any{int16(tblBindings), [][]byte{kid(rs.repoAlias[0])}}},
		{"ResolveKey: its versions", `SELECT v.kid, v.n, v.rec, v.ret, v.data FROM version v WHERE v.tbl = $1 AND v.kid = ANY($2) ORDER BY v.kid, v.n`, []any{int16(tblBindings), [][]byte{kid(rs.repoAlias[0])}}},
		{"AsOf a repository: facts naming it", `SELECT s.kid, s.key, s.head FROM series s WHERE s.tbl = $1 AND (s.kid IN (SELECT kid FROM series_subject WHERE tbl = $1 AND subject = ANY($2)))`, []any{int16(tblFacts), []string{repo}}},
		{"AsOf a repository: their versions", `SELECT v.kid, v.n, v.rec, v.ret, v.data FROM version v WHERE v.tbl = $1 AND v.kid IN (SELECT s.kid FROM series s WHERE s.tbl = $1 AND (s.kid IN (SELECT kid FROM series_subject WHERE tbl = $1 AND subject = ANY($2)))) ORDER BY v.kid, v.n`, []any{int16(tblFacts), []string{repo}}},
		{"AsOf a person: facts naming them", `SELECT s.kid, s.key, s.head FROM series s WHERE s.tbl = $1 AND (s.kid IN (SELECT kid FROM series_subject WHERE tbl = $1 AND subject = ANY($2)))`, []any{int16(tblFacts), []string{person}}},
		{"AsOf a person: their versions", `SELECT v.kid, v.n, v.rec, v.ret, v.data FROM version v WHERE v.tbl = $1 AND v.kid IN (SELECT s.kid FROM series s WHERE s.tbl = $1 AND (s.kid IN (SELECT kid FROM series_subject WHERE tbl = $1 AND subject = ANY($2)))) ORDER BY v.kid, v.n`, []any{int16(tblFacts), []string{person}}},
		{"AsOf by predicate: facts with one predicate", `SELECT s.kid, s.key, s.head FROM series s WHERE s.tbl = $1 AND sha256(s.predicate) = $2`, []any{int16(tblFacts), kid("member_of")}},
		{"AsOf by predicate: their versions", `SELECT v.kid, v.n, v.rec, v.ret, v.data FROM version v WHERE v.tbl = $1 AND v.kid IN (SELECT s.kid FROM series s WHERE s.tbl = $1 AND sha256(s.predicate) = $2) ORDER BY v.kid, v.n`, []any{int16(tblFacts), kid("member_of")}},
		{"unfiltered read: every fact series", `SELECT s.kid, s.key, s.head FROM series s WHERE s.tbl = $1`, []any{int16(tblFacts)}},
		{"unfiltered read: every fact version", `SELECT v.kid, v.n, v.rec, v.ret, v.data FROM version v WHERE v.tbl = $1 AND v.kid IN (SELECT s.kid FROM series s WHERE s.tbl = $1) ORDER BY v.kid, v.n`, []any{int16(tblFacts)}},
		{"merge components of a subject", `SELECT subject FROM component WHERE comp IN (SELECT comp FROM component WHERE subject = ANY($1))`, []any{[]string{person}}},
		{"merge records of a subject", `SELECT seq, data FROM merge_record WHERE survivor = ANY($1) OR merged = ANY($1) ORDER BY seq`, []any{[]string{person}}},
	}
}

// explainAnalyze runs EXPLAIN (ANALYZE, BUFFERS) on each load query. The
// two that would load every fact are only planned, not run.
func (w *world) explainAnalyze(rs *readSet) ([]plan, error) {
	var out []plan
	for _, q := range w.loadQueries(rs) {
		analyze := !strings.HasPrefix(q.name, "unfiltered")
		opts := "(ANALYZE, BUFFERS, SETTINGS)"
		if !analyze {
			opts = "(SETTINGS)"
		}
		rows, err := w.pg.conn.Query(w.ctx, "EXPLAIN "+opts+" "+q.sql, q.args...)
		if err != nil {
			return nil, fmt.Errorf("explain %s: %w", q.name, err)
		}
		p := plan{Name: q.name, SQL: strings.Join(strings.Fields(q.sql), " ")}
		for rows.Next() {
			var l string
			if err := rows.Scan(&l); err != nil {
				rows.Close()
				return nil, err
			}
			p.Plan = append(p.Plan, l)
			if v, ok := strings.CutPrefix(l, "Execution Time: "); ok {
				_, _ = fmt.Sscanf(v, "%f", &p.ExecutionMS)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

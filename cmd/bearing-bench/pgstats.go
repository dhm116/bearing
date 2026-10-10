package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
)

// pg is the benchmark's own connection to the database behind a postgres://
// store URL, for what the GraphStore contract does not offer: the number of
// rows, table sizes, query statistics and plans. It reads and runs
// ANALYZE/VACUUM on Bearing's tables, so it needs no more rights than the
// store's role (plus pg_monitor for pg_stat_statements, optional).
type pg struct {
	conn   *pgx.Conn
	schema string
}

// The values of the tbl column of the series, version and series_subject
// tables (internal/memstore.Table; a test keeps them in step).
const (
	tblBindings = iota
	tblSupports
	tblFacts
	tblConflicts
	tblIssues
	tblState
)

var tblNames = []string{"bindings", "supports", "facts", "conflicts", "issues", "state"}

// openPG connects with the store URL's user, host, database and schema, and
// the password from BEARING_STORE_PASSWORD. It returns nil, nil for a URL
// that is not a PostgreSQL one (mem://), where there is nothing to ask.
func openPG(ctx context.Context, storeURL string) (*pg, error) {
	u, err := url.Parse(storeURL)
	if err != nil {
		return nil, errors.New("store URL does not parse")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, nil //nolint:nilnil // not PostgreSQL: no connection, no error
	}
	q := u.Query()
	schema := q.Get("schema")
	if schema == "" {
		schema = "bearing"
	}
	sslmode := q.Get("sslmode")
	if sslmode == "" {
		sslmode = "disable"
	}
	dsn := url.URL{Scheme: "postgres", User: u.User, Host: u.Host, Path: u.Path}
	cfg, err := pgx.ParseConfig(dsn.String() + "?sslmode=" + sslmode)
	if err != nil {
		return nil, fmt.Errorf("parse store URL: %w", err)
	}
	cfg.Password = os.Getenv("BEARING_STORE_PASSWORD")
	cfg.RuntimeParams["search_path"] = quoteIdent(schema) + ", public" // public: where extensions such as pg_stat_statements live
	cfg.RuntimeParams["application_name"] = "bearing-bench"
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &pg{conn: c, schema: schema}, nil
}

func (p *pg) close(ctx context.Context) {
	if p != nil {
		_ = p.conn.Close(ctx)
	}
}

// journal returns the number of applies the store has recorded.
func (p *pg) journal(ctx context.Context) (int64, error) {
	var n int64
	err := p.conn.QueryRow(ctx, `SELECT journal FROM meta`).Scan(&n)
	return n, err
}

// factRows counts the fact rows ever recorded, retracted ones included: a
// fact's rows are its spans, kept for good.
func (p *pg) factRows(ctx context.Context) (int64, error) {
	var n int64
	err := p.conn.QueryRow(ctx, `SELECT count(*) FROM version WHERE tbl = $1`, tblFacts).Scan(&n)
	return n, err
}

// rowCounts counts rows by table: the series, their versions (all, and still
// current), and the other tables.
type rowCounts struct {
	Series   map[string]int64 `json:"series"`
	Versions map[string]int64 `json:"versions"`
	Current  map[string]int64 `json:"current_versions"`
	Other    map[string]int64 `json:"other"`
}

func (p *pg) rowCounts(ctx context.Context) (*rowCounts, error) {
	rc := &rowCounts{Series: map[string]int64{}, Versions: map[string]int64{}, Current: map[string]int64{}, Other: map[string]int64{}}
	for _, q := range []struct {
		sql string
		dst map[string]int64
	}{
		{`SELECT tbl, count(*) FROM series GROUP BY tbl`, rc.Series},
		{`SELECT tbl, count(*) FROM version GROUP BY tbl`, rc.Versions},
		{`SELECT tbl, count(*) FROM version WHERE ret = 0 GROUP BY tbl`, rc.Current},
	} {
		rows, err := p.conn.Query(ctx, q.sql)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t int16
			var n int64
			if err := rows.Scan(&t, &n); err != nil {
				return nil, err
			}
			if int(t) < len(tblNames) {
				q.dst[tblNames[t]] = n
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	for _, t := range []string{"subject", "merge_record", "unmerge_record", "component", "series_subject", "journal"} {
		var n int64
		if err := p.conn.QueryRow(ctx, "SELECT count(*) FROM "+t).Scan(&n); err != nil {
			return nil, err
		}
		rc.Other[t] = n
	}
	return rc, nil
}

// tableSize is the space one table or index takes.
type tableSize struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"` // table or index
	Bytes int64  `json:"bytes"`
	// Table is the table an index belongs to.
	Table string `json:"table,omitempty"`
	Live  int64  `json:"live_tuples,omitempty"`
	Dead  int64  `json:"dead_tuples,omitempty"`
}

// sizes lists every table and index of the schema with its size, largest
// first. The table size is the heap and TOAST; indexes are listed apart.
func (p *pg) sizes(ctx context.Context) ([]tableSize, error) {
	rows, err := p.conn.Query(ctx, `
SELECT c.relname, CASE c.relkind WHEN 'r' THEN 'table' ELSE 'index' END,
       pg_table_size(c.oid), coalesce(t.relname, ''), coalesce(s.n_live_tup, 0), coalesce(s.n_dead_tup, 0)
FROM pg_class c
LEFT JOIN pg_index i ON i.indexrelid = c.oid
LEFT JOIN pg_class t ON t.oid = i.indrelid
LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
WHERE c.relnamespace = to_regnamespace($1)::oid AND c.relkind IN ('r', 'i')
ORDER BY 3 DESC`, quoteIdent(p.schema))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tableSize
	for rows.Next() {
		var s tableSize
		if err := rows.Scan(&s.Name, &s.Kind, &s.Bytes, &s.Table, &s.Live, &s.Dead); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// analyze refreshes the planner's statistics and vacuums the schema's tables,
// as autovacuum would have by the time an operator looks.
func (p *pg) analyze(ctx context.Context) error {
	for _, t := range []string{"series", "series_subject", "version", "subject", "merge_record", "component", "journal"} {
		if _, err := p.conn.Exec(ctx, "VACUUM (ANALYZE) "+quoteIdent(p.schema)+"."+t); err != nil {
			return fmt.Errorf("vacuum %s: %w", t, err)
		}
	}
	return nil
}

// version returns the server's version string.
func (p *pg) version(ctx context.Context) (string, error) {
	var v string
	err := p.conn.QueryRow(ctx, `SHOW server_version`).Scan(&v)
	return v, err
}

// settings returns the server settings that matter for the numbers.
func (p *pg) settings(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range []string{"shared_buffers", "effective_cache_size", "work_mem", "max_wal_size", "synchronous_commit", "fsync", "wal_compression", "max_connections", "random_page_cost", "checkpoint_timeout", "autovacuum"} {
		var v string
		if err := p.conn.QueryRow(ctx, `SELECT current_setting($1)`, n).Scan(&v); err != nil {
			return nil, err
		}
		out[n] = v
	}
	return out, nil
}

// extra returns how many events measurements applied that are not part of the
// generated stream. They are kept in bench_meta, a table beside Bearing's, so
// a load that stops can go on from the right event.
func (p *pg) extra(ctx context.Context) (int64, error) {
	if p == nil {
		return 0, nil
	}
	if _, err := p.conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS bench_meta (id boolean PRIMARY KEY DEFAULT true CHECK (id), extra bigint NOT NULL)`); err != nil {
		return 0, fmt.Errorf("bench_meta: %w", err)
	}
	if _, err := p.conn.Exec(ctx, `INSERT INTO bench_meta (extra) VALUES (0) ON CONFLICT DO NOTHING`); err != nil {
		return 0, fmt.Errorf("bench_meta: %w", err)
	}
	var n int64
	err := p.conn.QueryRow(ctx, `SELECT extra FROM bench_meta`).Scan(&n)
	return n, err
}

func (p *pg) addExtra(ctx context.Context, n int) error {
	if p == nil || n == 0 {
		return nil
	}
	if _, err := p.extra(ctx); err != nil {
		return err
	}
	_, err := p.conn.Exec(ctx, `UPDATE bench_meta SET extra = extra + $1`, n)
	return err
}

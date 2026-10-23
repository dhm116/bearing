package duckstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// factCols are the fact columns every read selects, in scanFact's order, with
// subject and object as cluster IDs. factFrom joins what they need.
const (
	factCols = `f.id, se.cluster_id, f.predicate, oe.cluster_id, CAST(f.value AS VARCHAR),
	f.status, f.confidence, f.valid_from, f.valid_to, CAST(f.qualifiers AS VARCHAR)`
	factFrom = `fact f JOIN entity se ON se.id = f.subject_id LEFT JOIN entity oe ON oe.id = f.object_id`
	// notSelfLoop hides a relation whose ends are in one cluster: an artifact
	// of two records joining, not something a source said.
	notSelfLoop = `(oe.cluster_id IS NULL OR oe.cluster_id <> se.cluster_id)`
)

// Entity implements graph.Graph.
func (s *Store) Entity(ctx context.Context, ref string) (graph.Entity, error) {
	return read(ctx, s, func(db querier) (graph.Entity, error) {
		cluster, ok, err := clusterID(ctx, db, ref)
		if err != nil {
			return graph.Entity{}, err
		}
		if ok {
			ents, err := entities(ctx, db, []string{cluster})
			if err != nil {
				return graph.Entity{}, err
			}
			if e, found := ents[cluster]; found {
				return e, nil
			}
		}
		return graph.Entity{}, fmt.Errorf("entity %s: %w", ref, graph.ErrNotFound)
	})
}

// clusterID resolves a cluster ID, record ID or key to its cluster's ID. ok
// is false when nothing matches.
func clusterID(ctx context.Context, db querier, ref string) (id string, ok bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT cluster_id FROM entity WHERE id = ?`, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = db.QueryRowContext(ctx, `SELECT e.cluster_id FROM alias a JOIN entity e ON e.id = a.entity_id
			WHERE a.key = ?`, ref).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve %s: %w", ref, err)
	}
	return id, true, nil
}

// resolveID maps a record ID to its cluster's; an ID nothing names stays as
// it is, so a filter on it simply matches no facts.
func resolveID(ctx context.Context, db querier, id graph.EntityID) (graph.EntityID, error) {
	if id == "" {
		return "", nil
	}
	got, ok, err := clusterID(ctx, db, string(id))
	if err != nil || !ok {
		return id, err
	}
	return graph.EntityID(got), nil
}

// entities loads the given clusters: each one's ID, the kind and name of its
// oldest record that has them, every key, sorted, and its records.
func entities(ctx context.Context, db querier, clusters []string) (map[string]graph.Entity, error) {
	out := map[string]graph.Entity{}
	if len(clusters) == 0 {
		return out, nil
	}
	recs, err := records(ctx, db, `e.cluster_id IN (`+placeholders(len(clusters))+`)`, anySlice(clusters)...)
	if err != nil {
		return nil, err
	}
	for _, r := range recs { // oldest first
		e := out[string(r.Cluster)]
		e.ID = r.Cluster
		if e.Kind == "" {
			e.Kind = r.Kind
		}
		if e.Name == "" {
			e.Name = r.Name
		}
		e.Keys = append(e.Keys, r.Keys...)
		e.Records = append(e.Records, r)
		out[string(r.Cluster)] = e
	}
	for id, e := range out {
		slices.Sort(e.Keys)
		out[id] = e
	}
	return out, nil
}

// records loads the records (entity rows aliased e) matching where, oldest
// first, with their sorted keys and the sources that observed them.
func records(ctx context.Context, db querier, where string, args ...any) ([]graph.Record, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.id, e.cluster_id, e.kind, e.name,
		(SELECT CASE WHEN arg_max(coalesce(o.payload->>'$.entity.deleted', 'false') = 'true', row(o.observed_at, coalesce(o.payload->>'$.entity.deleted', 'false') = 'true')) THEN NULL ELSE max(o.observed_at) END
		 FROM alias a JOIN observation o ON o.key = a.key WHERE a.entity_id = e.id)
		FROM entity e WHERE `+where+` ORDER BY e.created_at, e.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("records: %w", err)
	}
	recs, err := collect(rows, func(sc scanner) (graph.Record, error) {
		var r graph.Record
		var kind string
		var seen sql.NullTime
		err := sc.Scan(&r.ID, &r.Cluster, &kind, &r.Name, &seen)
		r.Kind = model.Kind(kind)
		if seen.Valid {
			r.LastSeen = seen.Time.UTC()
		}
		return r, err
	})
	if err != nil || len(recs) == 0 {
		return nil, wrapErr("records", err)
	}
	idx := make(map[string]int, len(recs))
	for i, r := range recs {
		idx[string(r.ID)] = i
	}
	add := func(query string, put func(r *graph.Record, v string)) error {
		rows, err := db.QueryContext(ctx, query+` WHERE `+where+` ORDER BY 1, 2`, args...)
		if err != nil {
			return err
		}
		_, err = collect(rows, func(sc scanner) (struct{}, error) {
			var id, v string
			err := sc.Scan(&id, &v)
			if i, ok := idx[id]; ok && err == nil {
				put(&recs[i], v)
			}
			return struct{}{}, err
		})
		return err
	}
	if err := add(`SELECT a.entity_id, a.key FROM alias a JOIN entity e ON e.id = a.entity_id`,
		func(r *graph.Record, v string) { r.Keys = append(r.Keys, model.Key(v)) }); err != nil {
		return nil, fmt.Errorf("records: keys: %w", err)
	}
	if err := add(`SELECT DISTINCT a.entity_id, o.source FROM alias a JOIN entity e ON e.id = a.entity_id
		JOIN observation o ON o.key = a.key`,
		func(r *graph.Record, v string) { r.Sources = append(r.Sources, v) }); err != nil {
		return nil, fmt.Errorf("records: sources: %w", err)
	}
	return recs, nil
}

// Find implements graph.Graph. Exact matches rank first, then substrings,
// then names and keys at least 0.8 similar (Jaro-Winkler); a cluster ranks
// by its best record.
func (s *Store) Find(ctx context.Context, query string, kind model.Kind, limit int) ([]graph.Match, error) {
	return read(ctx, s, func(db querier) ([]graph.Match, error) {
		if limit <= 0 {
			limit = 20
		}
		// Every record yields one row for its name and one per key; the best
		// row (lowest tier, highest score) stands for its cluster.
		rows, err := db.QueryContext(ctx, `
WITH cand AS (
  SELECT cluster_id AS id, name AS txt FROM entity WHERE (? = '' OR kind = ?)
  UNION ALL
  SELECT e.cluster_id, a.key FROM entity e JOIN alias a ON a.entity_id = e.id
  WHERE (? = '' OR e.kind = ?)
), scored AS (
  SELECT id,
    CASE WHEN lower(txt) = lower(?) THEN 0 WHEN contains(lower(txt), lower(?)) THEN 1
         WHEN jaro_winkler_similarity(lower(txt), lower(?)) > 0.8 THEN 2 ELSE 3 END AS tier,
    CASE WHEN lower(txt) = lower(?) THEN 1.0
         WHEN contains(lower(txt), lower(?)) THEN greatest(0.9, jaro_winkler_similarity(lower(txt), lower(?)))
         ELSE jaro_winkler_similarity(lower(txt), lower(?)) END AS score
  FROM cand
)
SELECT id, max(score) FROM scored WHERE tier < 3 GROUP BY id
ORDER BY min(tier), max(score) DESC, id LIMIT ?`,
			string(kind), string(kind), string(kind), string(kind),
			query, query, query, query, query, query, query, limit)
		if err != nil {
			return nil, fmt.Errorf("find %q: %w", query, err)
		}
		var ids []string
		var scores []float64
		for rows.Next() {
			var id string
			var score float64
			if err := rows.Scan(&id, &score); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("find %q: %w", query, err)
			}
			ids = append(ids, id)
			scores = append(scores, score)
		}
		if err := closeRows(rows); err != nil {
			return nil, fmt.Errorf("find %q: %w", query, err)
		}
		if len(ids) == 0 {
			return nil, nil
		}
		byID, err := entities(ctx, db, ids)
		if err != nil {
			return nil, fmt.Errorf("find %q: %w", query, err)
		}
		out := make([]graph.Match, len(ids))
		for i, id := range ids {
			out[i] = graph.Match{Entity: byID[id], Score: min(scores[i], 1)}
		}
		return out, nil
	})
}

// Facts implements graph.Graph.
func (s *Store) Facts(ctx context.Context, f graph.FactFilter) ([]graph.Fact, error) {
	return read(ctx, s, func(db querier) ([]graph.Fact, error) {
		var err error
		if f.Subject, err = resolveID(ctx, db, f.Subject); err != nil {
			return nil, err
		}
		if f.Object, err = resolveID(ctx, db, f.Object); err != nil {
			return nil, err
		}
		where, args := holdsAt(f.At, !f.WithProposed)
		if f.Subject != "" {
			where += ` AND se.cluster_id = ?`
			args = append(args, string(f.Subject))
		}
		if f.Predicate != "" {
			where += ` AND f.predicate = ?`
			args = append(args, f.Predicate)
		}
		if f.Object != "" {
			where += ` AND oe.cluster_id = ?`
			args = append(args, string(f.Object))
		}
		rows, err := db.QueryContext(ctx,
			`SELECT `+factCols+` FROM `+factFrom+` WHERE `+where+` ORDER BY se.cluster_id, f.predicate, f.valid_from, f.id`, args...)
		if err != nil {
			return nil, fmt.Errorf("facts: %w", err)
		}
		facts, err := collect(rows, func(sc scanner) (graph.Fact, error) { return scanFact(sc) })
		if err != nil {
			return nil, fmt.Errorf("facts: %w", err)
		}
		ptrs := make([]*graph.Fact, len(facts))
		for i := range facts {
			ptrs[i] = &facts[i]
		}
		if err := attachSources(ctx, db, ptrs); err != nil {
			return nil, err
		}
		return facts, nil
	})
}

// Walk implements graph.Graph over clusters: a fact leads from its subject's
// cluster to its object's. A path-tracking recursive CTE ends cycles; a fact
// reached by several paths is returned once, at its shallowest depth.
// shortcut: the CTE enumerates paths, so a dense DAG can blow up; switch to
// a level-by-level walk if graphs get that shape.
func (s *Store) Walk(ctx context.Context, r graph.WalkRequest) ([]graph.Step, error) {
	return read(ctx, s, func(db querier) ([]graph.Step, error) {
		from, to := "a", "b"
		if r.Direction == graph.In {
			from, to = to, from
		}
		if r.MaxDepth <= 0 {
			r.MaxDepth = 10
		}
		start, err := resolveID(ctx, db, r.From)
		if err != nil {
			return nil, err
		}
		where, args := holdsAt(r.At, true)
		where += ` AND f.object_id IS NOT NULL`
		if len(r.Predicates) > 0 {
			where += ` AND f.predicate IN (` + placeholders(len(r.Predicates)) + `)`
			args = append(args, anySlice(r.Predicates)...)
		}
		args = append(args, string(start), r.MaxDepth)
		rows, err := db.QueryContext(ctx, `
WITH RECURSIVE cf AS (
  SELECT f.id, se.cluster_id AS a, oe.cluster_id AS b FROM `+factFrom+` WHERE `+where+`
), w(fact_id, node, depth, path) AS (
  SELECT id, `+to+`, 1, [`+from+`, `+to+`] FROM cf WHERE `+from+` = ?
  UNION ALL
  SELECT cf.id, cf.`+to+`, w.depth + 1, list_append(w.path, cf.`+to+`)
  FROM w JOIN cf ON cf.`+from+` = w.node
  WHERE NOT list_contains(w.path, cf.`+to+`) AND w.depth < ?
), shallow AS (SELECT fact_id, min(depth) AS depth FROM w GROUP BY fact_id)
SELECT d.depth, `+factCols+` FROM shallow d JOIN `+factFrom+` ON f.id = d.fact_id
ORDER BY d.depth, f.id`, args...)
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", r.From, err)
		}
		steps, err := collect(rows, func(sc scanner) (graph.Step, error) {
			var st graph.Step
			f, err := scanFact(sc, &st.Depth)
			st.Fact = f
			return st, err
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", r.From, err)
		}
		ptrs := make([]*graph.Fact, len(steps))
		for i := range steps {
			ptrs[i] = &steps[i].Fact
		}
		return steps, attachSources(ctx, db, ptrs)
	})
}

// Changes implements graph.Graph. A zero Since or Until leaves that end of
// the window open. Only asserted facts are reported.
func (s *Store) Changes(ctx context.Context, r graph.ChangesRequest) ([]graph.Change, error) {
	return read(ctx, s, func(db querier) ([]graph.Change, error) {
		if r.Limit <= 0 {
			r.Limit = 1000
		}
		subject, err := resolveID(ctx, db, r.Subject)
		if err != nil {
			return nil, err
		}
		// window and about are applied to each half of the union.
		window := func(col string) (string, []any) {
			w, a := `f.`+col+` IS NOT NULL`, []any(nil)
			if !r.Since.IsZero() {
				w += ` AND f.` + col + ` >= ?`
				a = append(a, r.Since)
			}
			if !r.Until.IsZero() {
				w += ` AND f.` + col + ` < ?`
				a = append(a, r.Until)
			}
			if subject != "" {
				w += ` AND (se.cluster_id = ? OR oe.cluster_id = ?)`
				a = append(a, string(subject), string(subject))
			}
			return w + ` AND f.status = 'asserted'`, a
		}
		ow, oa := window("valid_from")
		cw, ca := window("valid_to")
		args := append(append(oa, ca...), r.Limit)
		rows, err := db.QueryContext(ctx, `
SELECT c.change_kind, c.changed_at, `+factCols+` FROM (
  SELECT 'opened' AS change_kind, f.valid_from AS changed_at, f.id FROM `+factFrom+` WHERE `+ow+`
  UNION ALL
  SELECT 'closed', f.valid_to, f.id FROM `+factFrom+` WHERE `+cw+`
) c JOIN `+factFrom+` ON f.id = c.id ORDER BY c.changed_at DESC, f.id, c.change_kind LIMIT ?`, args...)
		if err != nil {
			return nil, fmt.Errorf("changes: %w", err)
		}
		changes, err := collect(rows, func(sc scanner) (graph.Change, error) {
			var kind string
			var at time.Time
			f, err := scanFact(sc, &kind, &at)
			return graph.Change{Kind: graph.ChangeKind(kind), At: at.UTC(), Fact: f}, err
		})
		if err != nil {
			return nil, fmt.Errorf("changes: %w", err)
		}
		ptrs := make([]*graph.Fact, len(changes))
		for i := range changes {
			ptrs[i] = &changes[i].Fact
		}
		return changes, attachSources(ctx, db, ptrs)
	})
}

// holdsAt builds the condition for facts (alias f) that held at at (zero
// means now), asserted only when assertedOnly. A current read leaves out
// self-loops by today's clusters; a read at a past time does not, since the
// clusters then were not today's.
func holdsAt(at time.Time, assertedOnly bool) (string, []any) {
	var w string
	var args []any
	if at.IsZero() {
		w = `f.valid_to IS NULL AND ` + notSelfLoop
	} else {
		w = `f.valid_from <= ? AND (f.valid_to IS NULL OR f.valid_to > ?)`
		args = []any{at, at}
	}
	if assertedOnly {
		w += ` AND f.status = 'asserted'`
	}
	return w, args
}

// attachSources fills Sources for all facts with one query.
func attachSources(ctx context.Context, db querier, facts []*graph.Fact) error {
	if len(facts) == 0 {
		return nil
	}
	idx := map[string][]int{} // the same fact can appear more than once (Changes)
	var ids []string
	for i, f := range facts {
		if _, seen := idx[f.ID]; !seen {
			ids = append(ids, f.ID)
		}
		idx[f.ID] = append(idx[f.ID], i)
	}
	// Every sync adds a fact_source row; only each source's first and latest
	// observation are returned, with the total count.
	rows, err := db.QueryContext(ctx, `
SELECT fact_id, id, source, observed_at, url, n FROM (
  SELECT fs.fact_id, o.id, o.source, o.observed_at, o.url,
    row_number() OVER (PARTITION BY fs.fact_id, o.source ORDER BY o.observed_at, o.id) AS first,
    row_number() OVER (PARTITION BY fs.fact_id, o.source ORDER BY o.observed_at DESC, o.id DESC) AS last,
    count(*) OVER (PARTITION BY fs.fact_id) AS n
  FROM fact_source fs JOIN observation o ON o.id = fs.observation_id
  WHERE fs.fact_id IN (`+placeholders(len(ids))+`)
) WHERE first = 1 OR last = 1 ORDER BY observed_at, id`, anySlice(ids)...)
	if err != nil {
		return fmt.Errorf("load sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var factID string
		var src graph.Source
		var url sql.NullString
		var n int
		if err := rows.Scan(&factID, &src.ObservationID, &src.Source, &src.ObservedAt, &url, &n); err != nil {
			return fmt.Errorf("load sources: %w", err)
		}
		src.ObservedAt = src.ObservedAt.UTC()
		src.URL = url.String
		for _, i := range idx[factID] {
			facts[i].Sources = append(facts[i].Sources, src)
			facts[i].SourceCount = n
		}
	}
	return rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

// collect scans every row with scan and closes rows.
func collect[T any](rows *sql.Rows, scan func(scanner) (T, error)) ([]T, error) {
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, v)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	return out, nil
}

func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}

func scanFact(sc scanner, lead ...any) (graph.Fact, error) {
	var f graph.Fact
	var object, value, quals sql.NullString
	var status string
	var from time.Time
	var to sql.NullTime
	dest := append(lead, &f.ID, &f.Subject, &f.Predicate, &object, &value, &status, &f.Confidence, &from, &to, &quals)
	if err := sc.Scan(dest...); err != nil {
		return graph.Fact{}, err
	}
	f.Object = graph.EntityID(object.String)
	f.Status = graph.Status(status)
	f.ValidFrom = from.UTC()
	if to.Valid {
		f.ValidTo = to.Time.UTC()
	}
	if value.Valid {
		if err := json.Unmarshal([]byte(value.String), &f.Value); err != nil {
			return graph.Fact{}, fmt.Errorf("fact %s: decode value: %w", f.ID, err)
		}
	}
	if quals.Valid {
		if err := json.Unmarshal([]byte(quals.String), &f.Qualifiers); err != nil {
			return graph.Fact{}, fmt.Errorf("fact %s: decode qualifiers: %w", f.ID, err)
		}
	}
	return f, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anySlice[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// querier is what reads run on: the transaction read opens.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// read runs f in one transaction, so a recluster cannot land between its
// queries, and rolls it back: the driver has no read-only transactions.
// Nothing inside f may use s.db, which has one connection.
func read[T any](ctx context.Context, s *Store, f func(db querier) (T, error)) (T, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return f(tx)
}

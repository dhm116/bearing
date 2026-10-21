package duckstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// factCols are the fact columns every read selects, in scanFact's order.
const factCols = `f.id, f.subject_id, f.predicate, f.object_id, CAST(f.value AS VARCHAR),
	f.status, f.confidence, f.valid_from, f.valid_to, CAST(f.qualifiers AS VARCHAR)`

// maxMergeHops bounds the merged_into chain so a corrupt loop cannot hang a read.
const maxMergeHops = 100

// Entity implements graph.Graph.
func (s *Store) Entity(ctx context.Context, ref string) (graph.Entity, error) {
	id, ok, err := s.survivor(ctx, ref)
	if err != nil {
		return graph.Entity{}, err
	}
	if !ok {
		return graph.Entity{}, fmt.Errorf("entity %s: %w", ref, graph.ErrNotFound)
	}
	var e graph.Entity
	var kind string
	err = s.db.QueryRowContext(ctx, `SELECT id, kind, name FROM entity WHERE id = ?`, id).Scan(&e.ID, &kind, &e.Name)
	if err != nil {
		return graph.Entity{}, fmt.Errorf("entity %s: %w", ref, err)
	}
	e.Kind = model.Kind(kind)
	keys, err := s.keys(ctx, []string{id})
	if err != nil {
		return graph.Entity{}, err
	}
	e.Keys = keys[id]
	return e, nil
}

// survivor resolves an entity ID or key to the entity it ends in after
// following merged_into. ok is false when nothing matches.
func (s *Store) survivor(ctx context.Context, ref string) (id string, ok bool, err error) {
	id = ref
	err = s.db.QueryRowContext(ctx, `SELECT id FROM entity WHERE id = ?`, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.QueryRowContext(ctx, `SELECT entity_id FROM alias WHERE key = ?`, ref).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve %s: %w", ref, err)
	}
	for range maxMergeHops {
		var next sql.NullString
		err = s.db.QueryRowContext(ctx, `SELECT merged_into FROM entity WHERE id = ?`, id).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !next.Valid) {
			return id, true, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("resolve %s: %w", ref, err)
		}
		id = next.String
	}
	return "", false, fmt.Errorf("resolve %s: merge chain too long", ref)
}

// resolveID maps a merged entity ID to its survivor; an ID nothing names stays
// as it is, so a filter on it simply matches no facts.
func (s *Store) resolveID(ctx context.Context, id graph.EntityID) (graph.EntityID, error) {
	if id == "" {
		return "", nil
	}
	got, ok, err := s.survivor(ctx, string(id))
	if err != nil || !ok {
		return id, err
	}
	return graph.EntityID(got), nil
}

// keys returns the sorted alias keys of each entity ID, in one query.
func (s *Store) keys(ctx context.Context, ids []string) (map[string][]model.Key, error) {
	out := map[string][]model.Key{}
	if len(ids) == 0 {
		return out, nil
	}
	//nolint:gosec // only fixed fragments and "?" lists are joined; values are bound
	rows, err := s.db.QueryContext(ctx,
		`SELECT entity_id, key FROM alias WHERE entity_id IN (`+placeholders(len(ids))+`) ORDER BY entity_id, key`, anySlice(ids)...)
	if err != nil {
		return nil, fmt.Errorf("load keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, fmt.Errorf("load keys: %w", err)
		}
		out[id] = append(out[id], model.Key(key))
	}
	return out, rows.Err()
}

// Find implements graph.Graph. Exact matches rank first, then substrings,
// then names and keys at least 0.8 similar (Jaro-Winkler).
func (s *Store) Find(ctx context.Context, query string, kind model.Kind, limit int) ([]graph.Match, error) {
	if limit <= 0 {
		limit = 20
	}
	// Every entity yields one row for its name and one per key; the best
	// row (lowest tier, highest score) stands for the entity.
	rows, err := s.db.QueryContext(ctx, `
WITH cand AS (
  SELECT id, name AS txt FROM entity WHERE merged_into IS NULL AND (? = '' OR kind = ?)
  UNION ALL
  SELECT e.id, a.key FROM entity e JOIN alias a ON a.entity_id = e.id
  WHERE e.merged_into IS NULL AND (? = '' OR e.kind = ?)
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
	keys, err := s.keys(ctx, ids)
	if err != nil {
		return nil, err
	}
	//nolint:gosec // only fixed fragments and "?" lists are joined; values are bound
	ents, err := s.db.QueryContext(ctx,
		`SELECT id, kind, name FROM entity WHERE id IN (`+placeholders(len(ids))+`)`, anySlice(ids)...)
	if err != nil {
		return nil, fmt.Errorf("find %q: %w", query, err)
	}
	byID := map[string]graph.Entity{}
	for ents.Next() {
		var e graph.Entity
		var k string
		if err := ents.Scan(&e.ID, &k, &e.Name); err != nil {
			_ = ents.Close()
			return nil, fmt.Errorf("find %q: %w", query, err)
		}
		e.Kind = model.Kind(k)
		e.Keys = keys[string(e.ID)]
		byID[string(e.ID)] = e
	}
	if err := closeRows(ents); err != nil {
		return nil, fmt.Errorf("find %q: %w", query, err)
	}
	out := make([]graph.Match, len(ids))
	for i, id := range ids {
		out[i] = graph.Match{Entity: byID[id], Score: min(scores[i], 1)}
	}
	return out, nil
}

// Facts implements graph.Graph.
func (s *Store) Facts(ctx context.Context, f graph.FactFilter) ([]graph.Fact, error) {
	var err error
	if f.Subject, err = s.resolveID(ctx, f.Subject); err != nil {
		return nil, err
	}
	if f.Object, err = s.resolveID(ctx, f.Object); err != nil {
		return nil, err
	}
	where, args := holdsAt("f", f.At, !f.WithProposed)
	if f.Subject != "" {
		where += ` AND f.subject_id = ?`
		args = append(args, string(f.Subject))
	}
	if f.Predicate != "" {
		where += ` AND f.predicate = ?`
		args = append(args, f.Predicate)
	}
	if f.Object != "" {
		where += ` AND f.object_id = ?`
		args = append(args, string(f.Object))
	}
	//nolint:gosec // only fixed fragments and "?" lists are joined; values are bound
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+factCols+` FROM fact f WHERE `+where+` ORDER BY f.subject_id, f.predicate, f.valid_from, f.id`, args...)
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
	if err := s.attachSources(ctx, ptrs); err != nil {
		return nil, err
	}
	return facts, nil
}

// Walk implements graph.Graph. A path-tracking recursive CTE ends cycles; a
// fact reached by several paths is returned once, at its shallowest depth.
// shortcut: the CTE enumerates paths, so a dense DAG can blow up; switch to
// a level-by-level walk if graphs get that shape.
func (s *Store) Walk(ctx context.Context, r graph.WalkRequest) ([]graph.Step, error) {
	from, to := "subject_id", "object_id"
	if r.Direction == graph.In {
		from, to = to, from
	}
	if r.MaxDepth <= 0 {
		r.MaxDepth = 10
	}
	start, err := s.resolveID(ctx, r.From)
	if err != nil {
		return nil, err
	}
	where, args := holdsAt("f", r.At, true)
	where += ` AND f.object_id IS NOT NULL`
	if len(r.Predicates) > 0 {
		where += ` AND f.predicate IN (` + placeholders(len(r.Predicates)) + `)`
		args = append(args, anySlice(r.Predicates)...)
	}
	all := append([]any{string(start)}, args...)
	all = append(all, args...)
	all = append(all, r.MaxDepth)
	//nolint:gosec // only fixed fragments and "?" lists are joined; values are bound
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE w(fact_id, node, depth, path) AS (
  SELECT f.id, f.`+to+`, 1, [f.`+from+`, f.`+to+`] FROM fact f
  WHERE f.`+from+` = ? AND `+where+`
  UNION ALL
  SELECT f.id, f.`+to+`, w.depth + 1, list_append(w.path, f.`+to+`)
  FROM w JOIN fact f ON f.`+from+` = w.node
  WHERE `+where+` AND NOT list_contains(w.path, f.`+to+`) AND w.depth < ?
), shallow AS (SELECT fact_id, min(depth) AS depth FROM w GROUP BY fact_id)
SELECT d.depth, `+factCols+` FROM shallow d JOIN fact f ON f.id = d.fact_id
ORDER BY d.depth, f.id`, all...)
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
	return steps, s.attachSources(ctx, ptrs)
}

// Changes implements graph.Graph. A zero Since or Until leaves that end of
// the window open. Only asserted facts are reported.
func (s *Store) Changes(ctx context.Context, r graph.ChangesRequest) ([]graph.Change, error) {
	if r.Limit <= 0 {
		r.Limit = 1000
	}
	subject, err := s.resolveID(ctx, r.Subject)
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
			w += ` AND (f.subject_id = ? OR f.object_id = ?)`
			a = append(a, string(subject), string(subject))
		}
		return w + ` AND f.status = 'asserted'`, a
	}
	ow, oa := window("valid_from")
	cw, ca := window("valid_to")
	args := append(append(oa, ca...), r.Limit)
	//nolint:gosec // only fixed fragments and "?" lists are joined; values are bound
	rows, err := s.db.QueryContext(ctx, `
SELECT change_kind, changed_at, `+factCols+` FROM (
  SELECT 'opened' AS change_kind, f.valid_from AS changed_at, f.* FROM fact f WHERE `+ow+`
  UNION ALL
  SELECT 'closed', f.valid_to, f.* FROM fact f WHERE `+cw+`
) f ORDER BY changed_at DESC, f.id, change_kind LIMIT ?`, args...)
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
	return changes, s.attachSources(ctx, ptrs)
}

// holdsAt builds the condition for facts about alias a that held at at (zero
// means now), asserted only when assertedOnly.
func holdsAt(a string, at time.Time, assertedOnly bool) (string, []any) {
	var w string
	var args []any
	if at.IsZero() {
		w = a + `.valid_to IS NULL`
	} else {
		w = a + `.valid_from <= ? AND (` + a + `.valid_to IS NULL OR ` + a + `.valid_to > ?)`
		args = []any{at, at}
	}
	if assertedOnly {
		w += ` AND ` + a + `.status = 'asserted'`
	}
	return w, args
}

// attachSources fills Sources for all facts with one query.
func (s *Store) attachSources(ctx context.Context, facts []*graph.Fact) error {
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
	//nolint:gosec // only fixed fragments and "?" lists are joined; values are bound
	rows, err := s.db.QueryContext(ctx, `
SELECT fs.fact_id, o.id, o.source, o.observed_at, o.url
FROM fact_source fs JOIN observation o ON o.id = fs.observation_id
WHERE fs.fact_id IN (`+placeholders(len(ids))+`) ORDER BY o.observed_at, o.id`, anySlice(ids)...)
	if err != nil {
		return fmt.Errorf("load sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var factID string
		var src graph.Source
		var url sql.NullString
		if err := rows.Scan(&factID, &src.ObservationID, &src.Source, &src.ObservedAt, &url); err != nil {
			return fmt.Errorf("load sources: %w", err)
		}
		src.ObservedAt = src.ObservedAt.UTC()
		src.URL = url.String
		for _, i := range idx[factID] {
			facts[i].Sources = append(facts[i].Sources, src)
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

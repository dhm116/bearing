package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"bearing.example/pkg/contracts"
)

var _ contracts.VectorIndex = (*Store)(nil)

// ErrNoVectorIndex is returned by the VectorIndex methods of a store opened
// without Options.VectorDimensions.
var ErrNoVectorIndex = errors.New("pgstore: the vector index is not configured; set the vector dimensions")

// MaxVectorDimensions is the most dimensions the HNSW index takes for the
// vector type (pgvector). A larger model needs halfvec, which this store does
// not use yet.
const MaxVectorDimensions = 2000

// minPgvector is the first pgvector release with HNSW indexes.
var minPgvector = [3]int{0, 5, 0}

// maxEfSearch is pgvector's limit on hnsw.ef_search; a search for more hits
// than that scans exactly.
const maxEfSearch = 1000

// pointChunk is the most points one Upsert statement carries.
const pointChunk = 1000

// vectorConfig is what the store learned when it set up the vector table.
type vectorConfig struct {
	dims int
	// ext is the extension's schema, quoted, for the type and the operators:
	// the connection's search_path is Bearing's schema alone.
	ext string
	// iterative is true for pgvector 0.8 and later, whose HNSW scans keep
	// going until a filtered search has its rows; before that a search with a
	// kind filter scans exactly, which is correct and slower.
	iterative bool
}

// setupVectors checks that pgvector is installed and creates the vector table
// for dims dimensions, or checks that an existing table has them. It never
// creates the extension: an administrator installs it. Changing the
// dimensions means re-indexing from the graph, so a different number than the
// table was made for is an error.
func setupVectors(ctx context.Context, db pool, schema string, dims int) (*vectorConfig, error) {
	if dims < 1 || dims > MaxVectorDimensions {
		return nil, fmt.Errorf("vector dimensions are from 1 to %d", MaxVectorDimensions)
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", cleanError(err))
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "bearing:vectors:"+schema); err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	var ext, version string
	err = tx.QueryRow(ctx, `SELECT format('%I', n.nspname), e.extversion FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'vector'`).Scan(&ext, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("the pgvector extension is not installed in this database; an administrator installs it with CREATE EXTENSION vector (pgvector 0.5 or later)")
	}
	if err != nil {
		return nil, fmt.Errorf("look for pgvector: %w", err)
	}
	ver, ok := parseVersion(version)
	if !ok || compareVersions(ver, minPgvector) < 0 {
		return nil, fmt.Errorf("pgvector %s is installed; the vector index needs 0.5 or later", version)
	}
	cfg := &vectorConfig{dims: dims, ext: ext, iterative: compareVersions(ver, [3]int{0, 8, 0}) >= 0}

	var made bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('vector_meta') IS NOT NULL`).Scan(&made); err != nil {
		return nil, fmt.Errorf("look for vector_meta: %w", err)
	}
	if made {
		var have int
		if err := tx.QueryRow(ctx, `SELECT dims FROM vector_meta`).Scan(&have); err != nil {
			return nil, fmt.Errorf("read vector_meta: %w", err)
		}
		if have != dims {
			return nil, fmt.Errorf("the vector index was made for %d dimensions and the store is set to %d; changing it means dropping the vector tables and indexing again from the graph", have, dims)
		}
		return cfg, tx.Commit(ctx)
	}
	// Caller-chosen values are keyed by hash (id_kid) or in a hash index
	// (subject_id), because a btree entry over 2.7 KB is refused.
	ddl := fmt.Sprintf(`
CREATE TABLE vector_meta (
	id   boolean PRIMARY KEY DEFAULT true CHECK (id),
	dims integer NOT NULL
);
CREATE TABLE vector_point (
	id_kid     bytea PRIMARY KEY,
	id         text  NOT NULL,
	subject_id text  NOT NULL,
	kind       text  NOT NULL DEFAULT '',
	text       text  NOT NULL DEFAULT '',
	payload    jsonb,
	vec        %[1]s.vector(%[2]d) NOT NULL
);
CREATE INDEX vector_point_subject ON vector_point USING hash (subject_id);
CREATE INDEX vector_point_vec ON vector_point USING hnsw (vec %[1]s.vector_cosine_ops);`, ext, dims)
	if _, err := tx.Exec(ctx, ddl); err != nil {
		return nil, fmt.Errorf("create the vector tables: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO vector_meta (dims) VALUES ($1)`, dims); err != nil {
		return nil, fmt.Errorf("record the vector dimensions: %w", err)
	}
	return cfg, tx.Commit(ctx)
}

// parseVersion reads "0.8.1" as {0, 8, 1}; a missing part is 0.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		n, err := strconv.Atoi(strings.TrimFunc(p, func(r rune) bool { return r < '0' || r > '9' }))
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func compareVersions(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// vectorText is a vector as pgvector reads it: "[1,0.5]". The elements must
// be finite and have a norm pgvector can compute, which it does in float32:
// all zeros, or numbers so small or large that their squares underflow or
// overflow, have no cosine distance there.
func (c *vectorConfig) vectorText(v []float32) (string, error) {
	if len(v) != c.dims {
		return "", fmt.Errorf("vector has %d dimensions, want %d", len(v), c.dims)
	}
	var b strings.Builder
	b.WriteByte('[')
	var norm float32
	for i, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return "", errors.New("vector has an element that is not a finite number")
		}
		norm += x * x
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	if norm == 0 || math.IsInf(float64(norm), 0) {
		return "", errors.New("vector is all zeros, or its elements are too small or too large to give it a length")
	}
	return b.String(), nil
}

// parseVectorText reads pgvector's text form back.
func parseVectorText(s string) ([]float32, error) {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(p, 32)
		if err != nil {
			return nil, fmt.Errorf("read a vector: %w", err)
		}
		out[i] = float32(f)
	}
	return out, nil
}

// Upsert implements contracts.VectorIndex. A point's kind is its "kind"
// payload field. The batch is written in one transaction, so a point that is
// refused leaves none of the others stored.
func (s *Store) Upsert(ctx context.Context, points []contracts.VectorPoint) error {
	if s.vec == nil {
		return ErrNoVectorIndex
	}
	// The last of two points with one ID wins, as in the reference store.
	last := make(map[string]int, len(points))
	vecs := make([]string, len(points))
	for i, p := range points {
		if p.ID == "" || p.SubjectID == "" {
			return errors.New("vector point needs an id and a subject id")
		}
		if strings.IndexByte(p.ID, 0) >= 0 || strings.IndexByte(string(p.SubjectID), 0) >= 0 || strings.IndexByte(p.Text, 0) >= 0 {
			return errors.New("vector point has a NUL byte in a text field")
		}
		vec, err := s.vec.vectorText(p.Vector)
		if err != nil {
			return fmt.Errorf("point %s: %w", clip(p.ID), err)
		}
		vecs[i] = vec
		last[p.ID] = i
	}
	type row struct {
		kid                     []byte
		id, subject, kind, text string
		payload, vec            any
	}
	rows := make([]row, 0, len(last))
	for i, p := range points {
		if last[p.ID] != i {
			continue
		}
		r := row{kid: kidOf(p.ID), id: p.ID, subject: string(p.SubjectID), text: p.Text, vec: vecs[i]}
		r.kind, _ = p.Payload["kind"].(string)
		if p.Payload != nil {
			b, err := json.Marshal(p.Payload)
			if err != nil {
				return fmt.Errorf("point %s: payload: %w", clip(p.ID), err)
			}
			r.payload = string(b)
		}
		rows = append(rows, r)
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", cleanError(err))
	}
	defer rollback(tx)
	stmt := fmt.Sprintf(`
INSERT INTO vector_point (id_kid, id, subject_id, kind, text, payload, vec)
SELECT a, b, c, d, e, f::jsonb, g::%[1]s.vector
FROM unnest($1::bytea[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[]) AS t(a, b, c, d, e, f, g)
ON CONFLICT (id_kid) DO UPDATE SET id = EXCLUDED.id, subject_id = EXCLUDED.subject_id, kind = EXCLUDED.kind,
	text = EXCLUDED.text, payload = EXCLUDED.payload, vec = EXCLUDED.vec
WHERE (vector_point.id, vector_point.subject_id, vector_point.kind, vector_point.text, vector_point.payload)
	IS DISTINCT FROM (EXCLUDED.id, EXCLUDED.subject_id, EXCLUDED.kind, EXCLUDED.text, EXCLUDED.payload)
	OR NOT (vector_point.vec OPERATOR(%[1]s.=) EXCLUDED.vec)`, s.vec.ext)
	for i := 0; i < len(rows); i += pointChunk {
		chunk := rows[i:min(i+pointChunk, len(rows))]
		var kids [][]byte
		var ids, subjects, kinds, texts []string
		var payloads, vecs []any
		for _, r := range chunk {
			kids, ids, subjects, kinds, texts = append(kids, r.kid), append(ids, r.id), append(subjects, r.subject), append(kinds, r.kind), append(texts, r.text)
			payloads, vecs = append(payloads, r.payload), append(vecs, r.vec)
		}
		if _, err := tx.Exec(ctx, stmt, kids, ids, subjects, kinds, texts, payloads, vecs); err != nil {
			return fmt.Errorf("write points: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// clip shortens a caller's value for an error message.
func clip(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

// Search implements contracts.VectorIndex. Scores are cosine similarity, so
// higher is closer, and equal scores come back in ID order (which of several
// equal scores reach the limit is up to the index). The HNSW index answers a
// search for up to 1,000 hits. An unlimited search, a larger one, and a
// search the index answers with fewer hits than asked for scan every point
// instead, which is exact. That last case is the index's own blind spot: it
// holds on to the entries of rows that were replaced or deleted until
// vacuum, and they use up the candidates a search is allowed to look at, and
// pgvector stops an iterative scan after hnsw.max_scan_tuples (20,000) rows.
// A search for a rare kind in a very large index may therefore scan exactly.
func (s *Store) Search(ctx context.Context, q contracts.VectorQuery) ([]contracts.VectorHit, error) {
	if s.vec == nil {
		return nil, ErrNoVectorIndex
	}
	qv, err := s.vec.vectorText(q.Vector)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	var kinds any
	if len(q.Kinds) > 0 {
		names := make([]string, len(q.Kinds))
		for i, k := range q.Kinds {
			names[i] = string(k)
		}
		kinds = names
	}
	var limit any
	if q.Limit > 0 {
		limit = q.Limit
	}
	if q.Limit > 0 && q.Limit <= maxEfSearch {
		hits, err := s.search(ctx, qv, kinds, limit, q.Limit)
		if err != nil || len(hits) == q.Limit {
			return hits, err
		}
	}
	return s.search(ctx, qv, kinds, limit, 0)
}

// search runs one search: through the HNSW index with ef_search sized for
// the limit when approx is its limit, or exactly when approx is 0.
func (s *Store) search(ctx context.Context, qv string, kinds, limit any, approx int) ([]contracts.VectorHit, error) {
	order := "vec OPERATOR(%[1]s.<=>) $1::%[1]s.vector"
	if approx == 0 {
		order += ", id"
	}
	// The outer query puts equal scores in ID order whichever way the inner
	// one found them.
	stmt := fmt.Sprintf(`SELECT id, subject_id, text, payload, vec::text, 1 - dist FROM (
SELECT id, subject_id, text, payload, vec, vec OPERATOR(%[1]s.<=>) $1::%[1]s.vector AS dist
FROM vector_point WHERE ($2::text[] IS NULL OR kind = ANY($2)) ORDER BY `+order+` LIMIT $3) found ORDER BY dist, id`, s.vec.ext)
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", cleanError(err))
	}
	defer rollback(tx)
	if approx == 0 {
		_, err = tx.Exec(ctx, `SELECT set_config('enable_indexscan', 'off', true)`)
	} else {
		// The index returns at most hnsw.ef_search candidates, 40 by default.
		_, err = tx.Exec(ctx, `SELECT set_config('hnsw.ef_search', $1, true)`, strconv.Itoa(min(max(2*approx, 40), maxEfSearch)))
		if err == nil && s.vec.iterative {
			_, err = tx.Exec(ctx, `SELECT set_config('hnsw.iterative_scan', 'strict_order', true)`)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("set up the search: %w", err)
	}
	rows, err := tx.Query(ctx, stmt, qv, kinds, limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	var hits []contracts.VectorHit
	for rows.Next() {
		var p contracts.VectorPoint
		var subject, vec string
		var payload []byte
		var score float64
		if err := rows.Scan(&p.ID, &subject, &p.Text, &payload, &vec, &score); err != nil {
			return nil, fmt.Errorf("read a hit: %w", err)
		}
		p.SubjectID = contracts.SubjectID(subject)
		if p.Vector, err = parseVectorText(vec); err != nil {
			return nil, err
		}
		if payload != nil {
			if err := json.Unmarshal(payload, &p.Payload); err != nil {
				return nil, fmt.Errorf("point %s: payload: %w", clip(p.ID), err)
			}
		}
		hits = append(hits, contracts.VectorHit{Point: p, Score: float32(score)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return hits, nil
}

// DeleteBySubject implements contracts.VectorIndex.
func (s *Store) DeleteBySubject(ctx context.Context, id contracts.SubjectID) error {
	if s.vec == nil {
		return ErrNoVectorIndex
	}
	if _, err := s.exec(ctx, `DELETE FROM vector_point WHERE subject_id = $1`, string(id)); err != nil {
		return fmt.Errorf("delete the points of %s: %w", clip(string(id)), err)
	}
	return nil
}

// Repoint implements contracts.VectorIndex.
func (s *Store) Repoint(ctx context.Context, from, to contracts.SubjectID) error {
	if s.vec == nil {
		return ErrNoVectorIndex
	}
	if from == to {
		return nil
	}
	if to == "" || strings.IndexByte(string(to), 0) >= 0 {
		return errors.New("repoint needs a subject to move the points to")
	}
	if _, err := s.exec(ctx, `UPDATE vector_point SET subject_id = $2 WHERE subject_id = $1`, string(from), string(to)); err != nil {
		return fmt.Errorf("repoint the points of %s: %w", clip(string(from)), err)
	}
	return nil
}

// exec runs one statement in its own transaction.
func (s *Store) exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin: %w", cleanError(err))
	}
	defer rollback(tx)
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return tag.RowsAffected(), nil
}

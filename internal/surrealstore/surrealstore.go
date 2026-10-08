// Package surrealstore backs GraphStore and VectorIndex with one SurrealDB
// database. Vectors live in an HNSW index. The graph is stored as rows (see
// migrate.go) that every operation reads and writes through whole
// transactions: Apply is one SurrealQL transaction that checks the head and
// the processed-event mark and then writes, so a failed apply writes nothing
// and concurrent applies cannot both land on one head.
//
// The rules of the data model (checks, canonicalization through merges,
// ordering) are not written a second time here. Each operation loads the
// rows it needs into a scratch memstore.Store, the reference
// implementation, runs the operation there and writes back what changed, so
// SurrealDB can differ from the reference only in how it stores rows. The
// conformance suite checks that it does not.
//
// The store talks to SurrealDB through a Querier, so the same code runs
// against a SurrealDB server (pure Go, see Dial) or an embedded engine
// (CGO, built with the surrealembed tag).
package surrealstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"bearing.example/internal/memstore"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// Querier runs SurrealQL. It returns one value per statement and fails if
// any statement failed.
type Querier interface {
	Query(ctx context.Context, sql string, vars map[string]any) ([]any, error)
	Close(ctx context.Context) error
}

// Store implements contracts.GraphStore and contracts.VectorIndex.
type Store struct {
	// Now is the store's clock and IDs its subject ID source. Set them
	// before first use; New fills real defaults.
	Now func() time.Time
	IDs memstore.IDSource
	// NewID names an apply attempt; New fills a random one.
	NewID func() string

	q Querier

	// afterStage, set by tests, runs between a large apply's staging and its
	// commit.
	afterStage func()

	mu  sync.Mutex
	dim int // vector dimension, fixed by the first Upsert
}

var (
	_ contracts.VectorIndex = (*Store)(nil)
	_ contracts.GraphStore  = (*Store)(nil)
)

// New prepares the schema and returns a store that uses q. The store owns q
// and closes it on Close.
func New(ctx context.Context, q Querier) (*Store, error) {
	if err := migrate(ctx, q); err != nil {
		return nil, fmt.Errorf("surrealstore: migrate: %w", err)
	}
	s := &Store{q: q, Now: time.Now, IDs: model.NewUUIDv7Source(time.Now, rand.Reader), NewID: randomToken}
	// Reuse the vector dimension from an earlier run, if there is one.
	res, err := q.Query(ctx, `SELECT VALUE array::len(vector) FROM vector LIMIT 1`, nil)
	if err != nil {
		return nil, fmt.Errorf("surrealstore: read vector dimension: %w", err)
	}
	var dims []int
	if err := decode(res[0], &dims); err == nil && len(dims) == 1 {
		s.dim = dims[0]
	}
	return s, nil
}

// Close closes the underlying connection or engine.
func (s *Store) Close(ctx context.Context) error { return s.q.Close(ctx) }

// decode converts a query result into dst. Results cross the wire as CBOR
// and come back as generic values; JSON is the common shape for both
// transports.
func decode(v any, dst any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// Vectors

// ensureIndex defines the HNSW index the first time vectors are stored. The
// dimension is fixed from then on, as it is in every vector database.
func (s *Store) ensureIndex(ctx context.Context, dim int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dim != 0 && s.dim != dim {
		return fmt.Errorf("vector has %d dimensions, index has %d", dim, s.dim)
	}
	// DIMENSION can't be a parameter; dim is an int, so formatting it is safe.
	sql := fmt.Sprintf(`DEFINE INDEX IF NOT EXISTS vector_hnsw ON vector FIELDS vector HNSW DIMENSION %d DIST COSINE TYPE F32`, dim)
	if _, err := s.q.Query(ctx, sql, nil); err != nil {
		return fmt.Errorf("define vector index: %w", err)
	}
	s.dim = dim
	return nil
}

// Upsert implements contracts.VectorIndex.
func (s *Store) Upsert(ctx context.Context, points []contracts.VectorPoint) error {
	if len(points) == 0 {
		return nil
	}
	rows := make([]map[string]any, len(points))
	for i, p := range points {
		if p.ID == "" || p.SubjectID == "" {
			return fmt.Errorf("vector point needs an id and a subject id")
		}
		if len(p.Vector) != len(points[0].Vector) || len(p.Vector) == 0 {
			return fmt.Errorf("vector point %s: every vector in a batch needs the same, non-zero dimension", p.ID)
		}
		rows[i] = map[string]any{"id": p.ID, "subject": string(p.SubjectID), "vector": p.Vector, "text": p.Text, "payload": nonNil(p.Payload)}
	}
	if err := s.ensureIndex(ctx, len(points[0].Vector)); err != nil {
		return err
	}
	_, err := s.q.Query(ctx, `
BEGIN TRANSACTION;
FOR $p IN $points {
	UPSERT type::record('vector', $p.id) CONTENT {
		subject: $p.subject, vector: $p.vector, text: $p.text, payload: $p.payload
	};
};
COMMIT TRANSACTION;`, map[string]any{"points": rows})
	return err
}

type hitRow struct {
	ID        string              `json:"id"`
	SubjectID contracts.SubjectID `json:"subject_id"`
	Vector    []float32           `json:"vector"`
	Text      string              `json:"text"`
	Payload   map[string]any      `json:"payload"`
	Score     float32             `json:"score"`
}

// Search uses the HNSW index. Score is cosine similarity, higher is closer.
// A kind filter is applied inside the nearest-neighbour search, so it still
// returns up to Limit results of that kind.
func (s *Store) Search(ctx context.Context, q contracts.VectorQuery) ([]contracts.VectorHit, error) {
	s.mu.Lock()
	dim := s.dim
	s.mu.Unlock()
	if dim == 0 {
		return nil, nil // nothing indexed yet
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	kinds := make([]string, len(q.Kinds))
	for i, k := range q.Kinds {
		kinds[i] = string(k)
	}
	filter := ""
	if len(kinds) > 0 {
		filter = ` AND payload.kind IN $kinds`
	}
	// K and EF must be literals; both are ints.
	sql := fmt.Sprintf(`SELECT record::id(id) AS id, subject AS subject_id, vector, text, payload,
	vector::similarity::cosine(vector, $v) AS score
FROM vector WHERE vector <|%d,%d|> $v%s ORDER BY score DESC LIMIT %d`, limit, max(limit*4, 40), filter, limit)
	res, err := s.q.Query(ctx, sql, map[string]any{"v": q.Vector, "kinds": kinds})
	if err != nil {
		return nil, err
	}
	var rows []hitRow
	if err := decode(res[0], &rows); err != nil {
		return nil, fmt.Errorf("decode hits: %w", err)
	}
	hits := make([]contracts.VectorHit, len(rows))
	for i, r := range rows {
		hits[i] = contracts.VectorHit{Score: r.Score, Point: contracts.VectorPoint{
			ID: r.ID, SubjectID: r.SubjectID, Vector: r.Vector, Text: r.Text, Payload: r.Payload,
		}}
	}
	return hits, nil
}

// DeleteBySubject implements contracts.VectorIndex.
func (s *Store) DeleteBySubject(ctx context.Context, id contracts.SubjectID) error {
	_, err := s.q.Query(ctx, `DELETE vector WHERE subject = $id`, map[string]any{"id": string(id)})
	return err
}

// Repoint implements contracts.VectorIndex.
func (s *Store) Repoint(ctx context.Context, from, to contracts.SubjectID) error {
	_, err := s.q.Query(ctx, `UPDATE vector SET subject = $to WHERE subject = $from`, map[string]any{"from": string(from), "to": string(to)})
	return err
}

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// ErrEmbeddedUnavailable is returned by OpenEmbedded in binaries built
// without the surrealembed tag.
var ErrEmbeddedUnavailable = errors.New("this build has no embedded SurrealDB; rebuild with -tags surrealembed (needs CGO and libsurrealdb_c) or connect to a SurrealDB server")

// randomToken returns 128 random bits as hex.
func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("surrealstore: read random bits: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

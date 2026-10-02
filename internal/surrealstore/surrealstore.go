// Package surrealstore backs both GraphStore and VectorIndex with one
// SurrealDB database. Entities are records, facts are graph edges between
// them (so SurrealQL can traverse ownership and dependencies directly), and
// vectors live in an HNSW index next to the graph.
//
// The store talks to SurrealDB through a Querier, so the same code runs
// against a SurrealDB server (pure Go, see Dial) or an embedded engine
// (CGO, built with the surrealembed tag).
package surrealstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

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
	q   Querier
	now func() time.Time

	mu  sync.Mutex
	dim int // vector dimension, fixed by the first Upsert
}

var (
	_ contracts.GraphStore  = (*Store)(nil)
	_ contracts.VectorIndex = (*Store)(nil)
)

// schema is idempotent and runs every time a store opens.
const schema = `
DEFINE TABLE IF NOT EXISTS entity SCHEMALESS;
DEFINE TABLE IF NOT EXISTS alias SCHEMALESS;
DEFINE FIELD IF NOT EXISTS entity ON alias TYPE record<entity>;
DEFINE INDEX IF NOT EXISTS alias_entity ON alias FIELDS entity;
DEFINE TABLE IF NOT EXISTS fact TYPE RELATION IN entity OUT entity SCHEMALESS;
DEFINE INDEX IF NOT EXISTS fact_relation ON fact FIELDS relation;
DEFINE TABLE IF NOT EXISTS fact_version SCHEMALESS;
DEFINE INDEX IF NOT EXISTS fact_version_subject ON fact_version FIELDS subject, seq UNIQUE;
DEFINE TABLE IF NOT EXISTS vector SCHEMALESS;
DEFINE INDEX IF NOT EXISTS vector_entity ON vector FIELDS entity;
`

// New prepares the schema and returns a store that uses q. The store owns q
// and closes it on Close.
func New(ctx context.Context, q Querier) (*Store, error) {
	if _, err := q.Query(ctx, schema, nil); err != nil {
		return nil, fmt.Errorf("surrealstore: define schema: %w", err)
	}
	s := &Store{q: q, now: time.Now}
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

// Thrown errors that start with this prefix mean "not found".
const notFound = "bearing: not found: "

func (s *Store) query(ctx context.Context, sql string, vars map[string]any) ([]any, error) {
	res, err := s.q.Query(ctx, sql, vars)
	if err != nil {
		if strings.Contains(err.Error(), notFound) {
			return nil, fmt.Errorf("%s: %w", strings.TrimSpace(after(err.Error(), notFound)), contracts.ErrNotFound)
		}
		return nil, err
	}
	return res, nil
}

func after(s, sep string) string {
	if _, rest, ok := strings.Cut(s, sep); ok {
		return strings.Trim(rest, `"' `)
	}
	return s
}

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

// Entities

type entityRow struct {
	ID         contracts.EntityID `json:"id"`
	Kind       model.Kind         `json:"kind"`
	Aliases    []model.Key        `json:"aliases"`
	Attributes map[string]any     `json:"attributes"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

func (r entityRow) entity() contracts.Entity {
	return contracts.Entity{ID: r.ID, Kind: r.Kind, Aliases: r.Aliases, Attributes: r.Attributes, UpdatedAt: r.UpdatedAt}
}

const entityFields = `record::id(id) AS id, kind, aliases, attributes, updated_at`

// UpsertEntity implements contracts.GraphStore.
func (s *Store) UpsertEntity(ctx context.Context, e contracts.Entity) error {
	if e.ID == "" {
		return fmt.Errorf("entity id is required")
	}
	aliases := make([]string, len(e.Aliases))
	for i, k := range e.Aliases {
		aliases[i] = string(k)
	}
	_, err := s.query(ctx, `
BEGIN TRANSACTION;
LET $e = type::record('entity', $id);
LET $taken = (SELECT VALUE record::id(entity) FROM alias WHERE record::id(id) IN $aliases AND entity != $e);
IF array::len($taken) > 0 { THROW 'alias already belongs to ' + <string> $taken[0] };
DELETE alias WHERE entity = $e;
UPSERT $e CONTENT { kind: $kind, aliases: $aliases, attributes: $attributes, updated_at: <datetime> $updated_at };
FOR $k IN $aliases { UPSERT type::record('alias', $k) SET entity = $e; };
COMMIT TRANSACTION;`, map[string]any{
		"id": string(e.ID), "kind": string(e.Kind), "aliases": aliases,
		"attributes": nonNil(e.Attributes), "updated_at": timestamp(e.UpdatedAt),
	})
	return err
}

// GetEntity implements contracts.GraphStore.
func (s *Store) GetEntity(ctx context.Context, id contracts.EntityID) (contracts.Entity, error) {
	res, err := s.query(ctx, `SELECT `+entityFields+` FROM type::record('entity', $id)`, map[string]any{"id": string(id)})
	if err != nil {
		return contracts.Entity{}, err
	}
	return oneEntity(res[0], "entity "+string(id))
}

// ResolveKey implements contracts.GraphStore.
func (s *Store) ResolveKey(ctx context.Context, key model.Key) (contracts.Entity, error) {
	res, err := s.query(ctx, `SELECT `+entityFields+` FROM (SELECT VALUE entity FROM type::record('alias', $key))`,
		map[string]any{"key": string(key)})
	if err != nil {
		return contracts.Entity{}, err
	}
	return oneEntity(res[0], "key "+string(key))
}

func oneEntity(v any, what string) (contracts.Entity, error) {
	var rows []entityRow
	if err := decode(v, &rows); err != nil {
		return contracts.Entity{}, fmt.Errorf("decode %s: %w", what, err)
	}
	if len(rows) == 0 {
		return contracts.Entity{}, fmt.Errorf("%s: %w", what, contracts.ErrNotFound)
	}
	return rows[0].entity(), nil
}

// Facts

type factRow struct {
	Subject    contracts.EntityID `json:"subject"`
	Relation   model.RelationType `json:"relation"`
	Object     contracts.EntityID `json:"object"`
	Confidence float64            `json:"confidence"`
	Asserted   bool               `json:"asserted"`
	Sources    []contracts.Source `json:"sources"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

func (r factRow) fact() contracts.Fact {
	return contracts.Fact{
		Subject: r.Subject, Relation: r.Relation, Object: r.Object, Confidence: r.Confidence,
		Asserted: r.Asserted, Sources: r.Sources, UpdatedAt: r.UpdatedAt,
	}
}

const factFields = `record::id(in) AS subject, relation, record::id(out) AS object, confidence, asserted, sources, updated_at`

// factVars returns the query variables that identify and describe f. The
// edge's record ID is [subject, relation, object], so there is at most one
// edge per triple.
func factVars(f contracts.Fact) (map[string]any, error) {
	// Sources go through JSON so their shape matches what decode expects.
	var sources []any
	if err := decode(f.Sources, &sources); err != nil {
		return nil, err
	}
	return map[string]any{
		"subject": string(f.Subject), "relation": string(f.Relation), "object": string(f.Object),
		"fact": map[string]any{
			"relation": string(f.Relation), "confidence": f.Confidence, "asserted": f.Asserted,
			"sources": nonNilSlice(sources), "updated_at": timestamp(f.UpdatedAt),
		},
	}, nil
}

// UpsertFact implements contracts.GraphStore.
func (s *Store) UpsertFact(ctx context.Context, f contracts.Fact) error {
	if f.UpdatedAt.IsZero() {
		f.UpdatedAt = s.now()
	}
	vars, err := factVars(f)
	if err != nil {
		return err
	}
	_, err = s.query(ctx, `
BEGIN TRANSACTION;
LET $s = type::record('entity', $subject);
LET $o = type::record('entity', $object);
IF !record::exists($s) { THROW '`+notFound+`fact references entity ' + $subject };
IF !record::exists($o) { THROW '`+notFound+`fact references entity ' + $object };
LET $edge = type::record('fact', [$subject, $relation, $object]);
LET $fact = object::extend($fact, { updated_at: <datetime> $fact.updated_at });
DELETE $edge;
RELATE $s->$edge->$o CONTENT $fact;
LET $seq = (SELECT VALUE seq FROM fact_version WHERE subject = $subject ORDER BY seq DESC LIMIT 1)[0] ?? 0;
CREATE fact_version CONTENT {
	subject: $subject, seq: $seq + 1, retracted: false, at: $fact.updated_at,
	fact: object::extend($fact, { subject: $subject, object: $object })
};
COMMIT TRANSACTION;`, vars)
	return err
}

// RetractFact implements contracts.GraphStore.
func (s *Store) RetractFact(ctx context.Context, subject contracts.EntityID, rel model.RelationType, object contracts.EntityID) error {
	_, err := s.query(ctx, `
BEGIN TRANSACTION;
LET $edge = type::record('fact', [$subject, $relation, $object]);
LET $f = (SELECT `+factFields+` FROM $edge)[0];
IF $f = NONE { THROW '`+notFound+`fact ' + $subject + ' ' + $relation + ' ' + $object };
DELETE $edge;
LET $seq = (SELECT VALUE seq FROM fact_version WHERE subject = $subject ORDER BY seq DESC LIMIT 1)[0] ?? 0;
CREATE fact_version CONTENT { subject: $subject, seq: $seq + 1, retracted: true, at: <datetime> $at, fact: $f };
COMMIT TRANSACTION;`, map[string]any{
		"subject": string(subject), "relation": string(rel), "object": string(object), "at": timestamp(s.now()),
	})
	return err
}

// Facts implements contracts.GraphStore.
func (s *Store) Facts(ctx context.Context, q contracts.FactQuery) ([]contracts.Fact, error) {
	var where []string
	vars := map[string]any{}
	if q.Subject != "" {
		where = append(where, "in = type::record('entity', $subject)")
		vars["subject"] = string(q.Subject)
	}
	if q.Relation != "" {
		where = append(where, "relation = $relation")
		vars["relation"] = string(q.Relation)
	}
	if q.Object != "" {
		where = append(where, "out = type::record('entity', $object)")
		vars["object"] = string(q.Object)
	}
	if q.AssertedOnly {
		where = append(where, "asserted = true")
	}
	if q.MinConfidence > 0 {
		where = append(where, "confidence >= $min")
		vars["min"] = q.MinConfidence
	}
	sql := `SELECT ` + factFields + ` FROM fact`
	if len(where) > 0 {
		sql += ` WHERE ` + strings.Join(where, " AND ")
	}
	sql += ` ORDER BY subject, relation, object`
	res, err := s.query(ctx, sql, vars)
	if err != nil {
		return nil, err
	}
	var rows []factRow
	if err := decode(res[0], &rows); err != nil {
		return nil, fmt.Errorf("decode facts: %w", err)
	}
	facts := make([]contracts.Fact, len(rows))
	for i, r := range rows {
		facts[i] = r.fact()
	}
	return facts, nil
}

// History implements contracts.GraphStore.
func (s *Store) History(ctx context.Context, subject contracts.EntityID) ([]contracts.FactVersion, error) {
	res, err := s.query(ctx, `SELECT fact, retracted, at, seq FROM fact_version WHERE subject = $subject ORDER BY seq`,
		map[string]any{"subject": string(subject)})
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Fact      factRow   `json:"fact"`
		Retracted bool      `json:"retracted"`
		At        time.Time `json:"at"`
	}
	if err := decode(res[0], &rows); err != nil {
		return nil, fmt.Errorf("decode history: %w", err)
	}
	out := make([]contracts.FactVersion, len(rows))
	for i, r := range rows {
		out[i] = contracts.FactVersion{Fact: r.Fact.fact(), Retracted: r.Retracted, At: r.At}
	}
	return out, nil
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
	if _, err := s.query(ctx, sql, nil); err != nil {
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
		if p.ID == "" || p.EntityID == "" {
			return fmt.Errorf("vector point needs an id and an entity id")
		}
		if len(p.Vector) != len(points[0].Vector) || len(p.Vector) == 0 {
			return fmt.Errorf("vector point %s: every vector in a batch needs the same, non-zero dimension", p.ID)
		}
		rows[i] = map[string]any{"id": p.ID, "entity": string(p.EntityID), "vector": p.Vector, "text": p.Text, "payload": nonNil(p.Payload)}
	}
	if err := s.ensureIndex(ctx, len(points[0].Vector)); err != nil {
		return err
	}
	_, err := s.query(ctx, `
BEGIN TRANSACTION;
FOR $p IN $points {
	UPSERT type::record('vector', $p.id) CONTENT {
		entity: type::record('entity', $p.entity), vector: $p.vector, text: $p.text, payload: $p.payload
	};
};
COMMIT TRANSACTION;`, map[string]any{"points": rows})
	return err
}

type hitRow struct {
	ID       string             `json:"id"`
	EntityID contracts.EntityID `json:"entity_id"`
	Vector   []float32          `json:"vector"`
	Text     string             `json:"text"`
	Payload  map[string]any     `json:"payload"`
	Score    float32            `json:"score"`
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
	sql := fmt.Sprintf(`SELECT record::id(id) AS id, record::id(entity) AS entity_id, vector, text, payload,
	vector::similarity::cosine(vector, $v) AS score
FROM vector WHERE vector <|%d,%d|> $v%s ORDER BY score DESC LIMIT %d`, limit, max(limit*4, 40), filter, limit)
	res, err := s.query(ctx, sql, map[string]any{"v": q.Vector, "kinds": kinds})
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
			ID: r.ID, EntityID: r.EntityID, Vector: r.Vector, Text: r.Text, Payload: r.Payload,
		}}
	}
	return hits, nil
}

// DeleteByEntity implements contracts.VectorIndex.
func (s *Store) DeleteByEntity(ctx context.Context, id contracts.EntityID) error {
	_, err := s.query(ctx, `DELETE vector WHERE entity = type::record('entity', $id)`, map[string]any{"id": string(id)})
	return err
}

// timestamp formats t for a <datetime> cast in SurrealQL. Passing time.Time
// directly loses sub-second precision in the Go SDK's CBOR encoding.
func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func nonNilSlice(s []any) []any {
	if s == nil {
		return []any{}
	}
	return s
}

// ErrEmbeddedUnavailable is returned by OpenEmbedded in binaries built
// without the surrealembed tag.
var ErrEmbeddedUnavailable = errors.New("this build has no embedded SurrealDB; rebuild with -tags surrealembed (needs CGO and libsurrealdb_c) or connect to a SurrealDB server")

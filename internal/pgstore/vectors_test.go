package pgstore

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
	"bearing.example/pkg/model"
)

func TestPostgresVectorConformance(t *testing.T) {
	t.Parallel()
	conformance.VectorIndex(t, func(t *testing.T) contracts.VectorIndex { return openVectorStore(t, 4) })
}

func point(id, subject string, kind model.Kind, v ...float32) contracts.VectorPoint {
	return contracts.VectorPoint{ID: id, SubjectID: contracts.SubjectID(subject), Vector: v, Text: "text of " + id, Payload: map[string]any{"kind": string(kind)}}
}

// A store without dimensions has a graph and no vector index.
func TestVectorMethodsNeedDimensions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Upsert(ctx, []contracts.VectorPoint{point("a", "a", model.KindTeam, 1, 0)}); !errors.Is(err, ErrNoVectorIndex) {
		t.Fatalf("got %v, want ErrNoVectorIndex", err)
	}
	if _, err := s.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0}}); !errors.Is(err, ErrNoVectorIndex) {
		t.Fatalf("got %v, want ErrNoVectorIndex", err)
	}
	if err := s.DeleteBySubject(ctx, "a"); !errors.Is(err, ErrNoVectorIndex) {
		t.Fatalf("got %v, want ErrNoVectorIndex", err)
	}
	if err := s.Repoint(ctx, "a", "b"); !errors.Is(err, ErrNoVectorIndex) {
		t.Fatalf("got %v, want ErrNoVectorIndex", err)
	}
}

func TestUpsertRefusesVectorsTheIndexCannotHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openVectorStore(t, 3)
	good := point("good", "s", model.KindTeam, 1, 2, 3)
	for name, bad := range map[string]contracts.VectorPoint{
		"too short":   point("a", "s", model.KindTeam, 1, 2),
		"too long":    point("a", "s", model.KindTeam, 1, 2, 3, 4),
		"NaN":         point("a", "s", model.KindTeam, 1, float32(math.NaN()), 3),
		"infinite":    point("a", "s", model.KindTeam, 1, float32(math.Inf(1)), 3),
		"all zeros":   point("a", "s", model.KindTeam, 0, 0, 0),
		"no ID":       point("", "s", model.KindTeam, 1, 2, 3),
		"no subject":  point("a", "", model.KindTeam, 1, 2, 3),
		"NUL in text": {ID: "a", SubjectID: "s", Vector: []float32{1, 2, 3}, Text: "a\x00b"},
	} {
		if err := s.Upsert(ctx, []contracts.VectorPoint{good, bad}); err == nil {
			t.Errorf("%s: stored the batch", name)
		}
	}
	// None of the refused batches stored its good point.
	hits, err := s.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 2, 3}})
	if err != nil || len(hits) != 0 {
		t.Fatalf("got %d hits, %v, want none", len(hits), err)
	}
	for name, q := range map[string][]float32{"short": {1, 2}, "zero": {0, 0, 0}, "NaN": {float32(math.NaN()), 1, 1}} {
		if _, err := s.Search(ctx, contracts.VectorQuery{Vector: q}); err == nil {
			t.Errorf("%s query: searched", name)
		}
	}
}

func TestUpsertKeepsTheLastOfTwoPointsWithOneID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openVectorStore(t, 2)
	first, second := point("p", "s1", model.KindTeam, 1, 0), point("p", "s2", model.KindDocument, 0, 1)
	if err := s.Upsert(ctx, []contracts.VectorPoint{first, second}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 1}})
	if err != nil || len(hits) != 1 {
		t.Fatalf("got %d hits, %v, want 1", len(hits), err)
	}
	if got := hits[0].Point; got.SubjectID != "s2" || got.Payload["kind"] != string(model.KindDocument) || !slices.Equal(got.Vector, []float32{0, 1}) {
		t.Fatalf("got %+v, want the second point", got)
	}
}

// IDs, subject IDs and text are the caller's and have no length bound, as in
// the reference store; none may be in a btree index.
func TestVectorPointsWithLongIDsWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openVectorStore(t, 2)
	random := func() string {
		b := make([]byte, 4000)
		if _, err := cryptorand.Read(b); err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(b)
	}
	id, subject, other := random(), random(), random()
	p := point(id, subject, model.KindTeam, 1, 0)
	p.Text = strings.Repeat("long text ", 10000)
	if err := s.Upsert(ctx, []contracts.VectorPoint{p, p}); err != nil {
		t.Fatal(err)
	}
	if err := s.Repoint(ctx, contracts.SubjectID(subject), contracts.SubjectID(other)); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0}, Limit: 5})
	if err != nil || len(hits) != 1 || hits[0].Point.ID != id || string(hits[0].Point.SubjectID) != other || hits[0].Point.Text != p.Text {
		t.Fatalf("got %d hits, %v, want the one point on the new subject", len(hits), err)
	}
	if err := s.DeleteBySubject(ctx, contracts.SubjectID(other)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM vector_point`); n != 0 {
		t.Fatalf("got %d points after the delete, want none", n)
	}
}

func TestPayloadsComeBackAsStored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openVectorStore(t, 2)
	p := contracts.VectorPoint{ID: "p", SubjectID: "s", Vector: []float32{1, 0}, Payload: map[string]any{
		"kind": string(model.KindTeam), "n": float64(3), "ok": true, "tags": []any{"a", "b"}, "nested": map[string]any{"x": "y"},
	}}
	bare := contracts.VectorPoint{ID: "q", SubjectID: "s", Vector: []float32{0, 1}}
	if err := s.Upsert(ctx, []contracts.VectorPoint{p, bare}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0}})
	if err != nil || len(hits) != 2 {
		t.Fatalf("got %d hits, %v", len(hits), err)
	}
	if fmt.Sprint(hits[0].Point.Payload) != fmt.Sprint(p.Payload) {
		t.Fatalf("got payload %v, want %v", hits[0].Point.Payload, p.Payload)
	}
	if hits[1].Point.Payload != nil {
		t.Fatalf("got payload %v for a point without one", hits[1].Point.Payload)
	}
	if hits[0].Score <= hits[1].Score || hits[0].Score < 0.99 {
		t.Fatalf("got scores %v and %v, want cosine similarity with the query's twin near 1", hits[0].Score, hits[1].Score)
	}
}

// A filter on kind returns as many hits as exist, however many closer points
// of other kinds there are: the index answers a filtered search by scanning
// further (pgvector 0.8) or the search scans exactly.
func TestFilteredSearchFindsEveryPointOfTheKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openVectorStore(t, 2)
	var points []contracts.VectorPoint
	for i := range 400 {
		points = append(points, point(fmt.Sprintf("near-%d", i), fmt.Sprintf("n%d", i), model.KindComponent, 1, float32(i)/1000))
	}
	for i := range 5 {
		points = append(points, point(fmt.Sprintf("far-%d", i), fmt.Sprintf("f%d", i), model.KindTeam, 0.01, 1))
	}
	if err := s.Upsert(ctx, points); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0}, Limit: 5, Kinds: []model.Kind{model.KindTeam}})
	if err != nil || len(hits) != 5 {
		t.Fatalf("got %d hits, %v, want the 5 teams", len(hits), err)
	}
	// An unlimited search, one past the index's limit, and a plain search
	// that wants more than the index's default.
	for _, limit := range []int{0, maxEfSearch + 1, 100, 405} {
		hits, err = s.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0}, Limit: limit})
		want := 405
		if limit > 0 {
			want = min(limit, 405)
		}
		if err != nil || len(hits) != want {
			t.Fatalf("limit %d: got %d hits, %v, want %d", limit, len(hits), err, want)
		}
		if !slices.IsSortedFunc(hits, func(a, b contracts.VectorHit) int { return -cmpFloat(a.Score, b.Score) }) {
			t.Fatalf("limit %d: scores are not in descending order", limit)
		}
	}
}

func cmpFloat(a, b float32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestVectorDimensionsAreFixedWhenTheIndexIsMade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ensureVectorExtension(t)
	s, o := openTestStoreWith(t, scopedTests(), func(o *Options) { o.VectorDimensions = 4 })
	if err := s.Upsert(ctx, []contracts.VectorPoint{point("a", "a", model.KindTeam, 1, 0, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	// Opening again with the same dimensions finds the points; a different
	// number is refused, and so is a number the index cannot take.
	again := reopen(t, withDims(o, 4), nil)
	hits, err := again.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0, 0, 0}})
	if err != nil || len(hits) != 1 {
		t.Fatalf("got %d hits, %v, want the stored point", len(hits), err)
	}
	if _, err := Open(ctx, withDims(o, 8)); err == nil || !strings.Contains(err.Error(), "made for 4 dimensions") {
		t.Fatalf("got %v, want a refusal naming the stored dimensions", err)
	}
	for _, dims := range []int{-1, MaxVectorDimensions + 1} {
		if _, err := Open(ctx, withDims(o, dims)); err == nil {
			t.Errorf("opened with %d dimensions", dims)
		}
	}
	// The same schema opened without dimensions still serves the graph.
	graphOnly := reopen(t, withDims(o, 0), nil)
	if _, err := graphOnly.Head(ctx); err != nil {
		t.Fatal(err)
	}
}

func withDims(o Options, dims int) Options {
	o.VectorDimensions = dims
	return o
}

// Without the extension the store says what an administrator must do.
func TestOpeningTheVectorIndexNeedsPgvector(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	admin := adminOptions(t)
	cfg, err := poolConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	suffix := make([]byte, 6)
	if _, err := cryptorand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "novec_" + hex.EncodeToString(suffix)
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q TEMPLATE template0`, name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
	})
	o := admin
	o.Database, o.Schema, o.VectorDimensions = name, "bearing", 4
	_, err = Open(ctx, o)
	if err == nil || !strings.Contains(err.Error(), "pgvector extension is not installed") {
		t.Fatalf("got %v, want a refusal that says pgvector is not installed", err)
	}
}

func TestParsingPgvectorVersions(t *testing.T) {
	t.Parallel()
	for v, want := range map[string][3]int{"0.5.0": {0, 5, 0}, "0.8.1": {0, 8, 1}, "1.0": {1, 0, 0}, "0.7.0-dev": {0, 7, 0}} {
		if got, ok := parseVersion(v); !ok || got != want {
			t.Errorf("parseVersion(%q) = %v, %v, want %v", v, got, ok, want)
		}
	}
	if _, ok := parseVersion("next"); ok {
		t.Error("parsed a version that is not one")
	}
	if compareVersions([3]int{0, 5, 0}, [3]int{0, 8, 0}) >= 0 || compareVersions([3]int{0, 8, 0}, [3]int{0, 8, 0}) != 0 {
		t.Error("versions compare wrongly")
	}
}

// Whatever statement fails, a vector operation reports it, changes nothing,
// and the index works afterwards.
func TestEveryVectorStatementMayFailWithoutHarmingTheIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openVectorStore(t, 2)
	if err := s.Upsert(ctx, []contracts.VectorPoint{point("a", "s1", model.KindTeam, 1, 0), point("b", "s2", model.KindTeam, 0, 1)}); err != nil {
		t.Fatal(err)
	}
	ops := map[string]func(*Store) error{
		"Upsert": func(s *Store) error {
			return s.Upsert(ctx, []contracts.VectorPoint{point("c", "s3", model.KindDocument, 1, 1)})
		},
		"Search": func(s *Store) error {
			_, err := s.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0}, Limit: 3, Kinds: []model.Kind{model.KindTeam}})
			return err
		},
		"Exact search": func(s *Store) error {
			_, err := s.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0}})
			return err
		},
		"DeleteBySubject": func(s *Store) error { return s.DeleteBySubject(ctx, "s3") },
		"Repoint":         func(s *Store) error { return s.Repoint(ctx, "s1", "s9") },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			counter := &faultPool{}
			if err := op(withPool(s, counter)); err != nil {
				t.Fatal(err)
			}
			for at := 1; at <= counter.calls; at++ {
				before := count(t, s, `SELECT count(*) FROM vector_point`)
				f := &faultPool{failAt: at}
				if err := op(withPool(s, f)); !errors.Is(err, errInjected) {
					t.Fatalf("statement %d of %d: got %v, want the injected failure", at, counter.calls, err)
				}
				if got := count(t, s, `SELECT count(*) FROM vector_point`); got != before {
					t.Fatalf("statement %d: the failed operation changed the point count from %d to %d", at, before, got)
				}
				if err := op(s); err != nil {
					t.Fatalf("statement %d: retry after failure: %v", at, err)
				}
			}
		})
	}
}

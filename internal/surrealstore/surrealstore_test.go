package surrealstore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
)

// The conformance suites need a SurrealDB to talk to. Set
// BEARING_TEST_SURREALDB to a server URL (for example ws://127.0.0.1:8000,
// started with `surreal start --user root --pass root memory`) and
// BEARING_TEST_SURREALDB_USER and BEARING_TEST_SURREALDB_PASS to sign in, or
// build with -tags surrealembed to use an embedded engine. Otherwise the
// tests skip.
var dbSeq atomic.Int64

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A fresh database per test keeps them independent.
	db := fmt.Sprintf("t%d_%d", time.Now().UnixNano(), dbSeq.Add(1))
	var (
		s   *Store
		err error
	)
	switch url := os.Getenv("BEARING_TEST_SURREALDB"); {
	case url != "":
		s, err = Dial(ctx, ServerOptions{
			URL: url, Namespace: "bearing_test", Database: db,
			Username: os.Getenv("BEARING_TEST_SURREALDB_USER"), Password: os.Getenv("BEARING_TEST_SURREALDB_PASS"),
		})
	case EmbeddedAvailable:
		s, err = OpenEmbedded(ctx, "mem://", "bearing_test", db)
	default:
		t.Skip("set BEARING_TEST_SURREALDB or build with -tags surrealembed")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

func TestDialRejectsBadCredentials(t *testing.T) {
	url, user := os.Getenv("BEARING_TEST_SURREALDB"), os.Getenv("BEARING_TEST_SURREALDB_USER")
	if url == "" || user == "" {
		t.Skip("set BEARING_TEST_SURREALDB and BEARING_TEST_SURREALDB_USER")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Dial(ctx, ServerOptions{
		URL: url, Namespace: "bearing_test", Database: "bad_credentials",
		Username: user, Password: os.Getenv("BEARING_TEST_SURREALDB_PASS") + "-wrong",
	})
	if err == nil {
		_ = s.Close(ctx)
		t.Fatal("signed in with the wrong password")
	}
	if !strings.Contains(err.Error(), "surrealstore: sign in: ") || strings.Contains(err.Error(), user) {
		t.Fatalf("got %v, want a sign-in error that does not name the user", err)
	}
}

func TestGraphConformance(t *testing.T) {
	conformance.GraphStore(t, func(t *testing.T) contracts.GraphStore { return newTestStore(t) })
}

func TestVectorConformance(t *testing.T) {
	var current *Store
	conformance.VectorIndex(t,
		func(t *testing.T) contracts.VectorIndex {
			current = newTestStore(t)
			return current
		},
		// Vectors link to graph entities, so the entities must exist.
		func(t *testing.T, ids ...contracts.EntityID) {
			for _, id := range ids {
				if err := current.UpsertEntity(context.Background(), contracts.Entity{ID: id, Kind: "Component"}); err != nil {
					t.Fatal(err)
				}
			}
		})
}

// One store answers a question that needs both halves: which team owns the
// component closest to a query vector.
func TestGraphAndVectorsTogether(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, e := range []contracts.Entity{{ID: "payments-api", Kind: "Component"}, {ID: "team-payments", Kind: "Team"}} {
		if err := s.UpsertEntity(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertFact(ctx, contracts.Fact{Subject: "payments-api", Relation: "owned_by", Object: "team-payments", Confidence: 0.95, Asserted: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(ctx, []contracts.VectorPoint{{ID: "p", EntityID: "payments-api", Vector: []float32{1, 0}, Text: "refunds"}}); err != nil {
		t.Fatal(err)
	}
	res, err := s.q.Query(ctx, `
SELECT VALUE (entity->fact[WHERE relation = 'owned_by' AND asserted]->entity).map(|$e| record::id($e))
FROM vector WHERE vector <|1,40|> $v`, map[string]any{"v": []float32{0.9, 0.1}})
	if err != nil {
		t.Fatal(err)
	}
	var owners [][]string
	if err := decode(res[0], &owners); err != nil {
		t.Fatal(err)
	}
	if len(owners) != 1 || len(owners[0]) != 1 || owners[0][0] != "team-payments" {
		t.Fatalf("owners = %v", owners)
	}
}

func TestReopenKeepsVectorDimension(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.UpsertEntity(ctx, contracts.Entity{ID: "a", Kind: "Team"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(ctx, []contracts.VectorPoint{{ID: "a", EntityID: "a", Vector: []float32{1, 0, 0}}}); err != nil {
		t.Fatal(err)
	}
	again, err := New(ctx, s.q)
	if err != nil {
		t.Fatal(err)
	}
	if again.dim != 3 {
		t.Fatalf("dim = %d, want 3", again.dim)
	}
	if err := again.Upsert(ctx, []contracts.VectorPoint{{ID: "b", EntityID: "a", Vector: []float32{1, 0}}}); err == nil {
		t.Fatal("expected a dimension mismatch error")
	}
}

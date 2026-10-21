package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

const (
	githubSource  = "github-acme"
	catalogSource = "catalog-acme"
)

var (
	// seedAt is when the world's sources first observed it, movedAt when
	// payments-api's CODEOWNERS moved from the payments team to platform,
	// and nowAt the clock when the commands run.
	seedAt  = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	movedAt = seedAt.Add(2 * time.Hour)
	nowAt   = seedAt.Add(3 * time.Hour)
)

// world is a small Acme in an in-memory store: two teams, a person and
// repositories as GitHub observed them, and components that depend on each
// other as a catalog did.
type world struct {
	t       testing.TB
	clock   *testkit.FakeClock
	store   *duckstore.Store
	adapter adapter.Adapter
	cfg     json.RawMessage
}

// stubAdapter is a source the server knows but never syncs in these tests.
type stubAdapter struct{}

func (stubAdapter) Describe(context.Context) (adapter.DescribeResult, error) {
	return adapter.DescribeResult{}, nil
}

func (stubAdapter) Sync(context.Context, adapter.SyncParams) (adapter.SyncResult, error) {
	return adapter.SyncResult{Done: true}, nil
}

func (stubAdapter) Handle(context.Context, adapter.HandleParams) (adapter.HandleResult, error) {
	return adapter.HandleResult{}, adapter.ErrNotSupported
}

func newWorld(t testing.TB) *world {
	t.Helper()
	clk := testkit.NewClock(nowAt)
	s, err := duckstore.Open(context.Background(), duckstore.Config{Now: clk.Now, NewID: testkit.NewSeqIDs("e").NewID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	w := &world{t: t, clock: clk, store: s, adapter: stubAdapter{}, cfg: json.RawMessage(`{"org":"acme"}`)}
	const (
		payments = "github:team/acme/payments"
		platform = "github:team/acme/platform"
		api      = "github:repo/acme/payments-api"
	)
	w.ingest(githubSource, seedAt,
		entity("Team", payments, "Payments"),
		entity("Team", platform, "Platform"),
		entity("Person", "github:user/jdoe", "Jane Doe", rel("member_of", payments)),
		entity("Repository", api, "payments-api", attr("language", "Go"), rel("approves_changes", payments)),
	)
	w.ingest(catalogSource, seedAt.Add(time.Minute),
		entity("Repository", "github:repo/acme/billing", "billing", rel("owned_by", platform)),
		entity("Component", "catalog:component/payments", "", relQ("depends_on", "catalog:component/ledger", map[string]any{"protocol": "http", "port": 8080, "via": "env:LEDGER_URL"})),
		entity("Component", "catalog:component/ledger", "", rel("depends_on", "catalog:component/db")),
		entity("Component", "catalog:component/db", ""),
	)
	moved := entity("Repository", api, "payments-api", rel("approves_changes", platform))
	moved.Relations = append(moved.Relations, &modelv1alpha1.Relation{Type: "approves_changes", End: &modelv1alpha1.Relation_To{To: payments}, Absent: true})
	w.ingest(githubSource, movedAt, moved)
	return w
}

// entity is the data of an observation of one entity, with a name attribute
// unless name is empty (a Component has none; the store names it from its
// key).
func entity(kind, key, name string, opts ...func(*modelv1alpha1.ObservationData)) *modelv1alpha1.ObservationData {
	d := &modelv1alpha1.ObservationData{Entity: &modelv1alpha1.Entity{Kind: kind, Key: key, Attributes: map[string]*structpb.Value{}}}
	if name != "" {
		d.Entity.Attributes["name"] = structpb.NewStringValue(name)
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

func attr(name, value string) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) { d.Entity.Attributes[name] = structpb.NewStringValue(value) }
}

func rel(typ, to string) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) {
		d.Relations = append(d.Relations, &modelv1alpha1.Relation{Type: typ, End: &modelv1alpha1.Relation_To{To: to}})
	}
}

// relQ is rel with qualifiers.
func relQ(typ, to string, q map[string]any) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) {
		s, err := structpb.NewStruct(q)
		if err != nil {
			panic(err)
		}
		d.Relations = append(d.Relations, &modelv1alpha1.Relation{Type: typ, End: &modelv1alpha1.Relation_To{To: to}, Attributes: s.Fields})
	}
}

// ingest validates and stores observations of source made at.
func (w *world) ingest(source string, at time.Time, data ...*modelv1alpha1.ObservationData) {
	w.t.Helper()
	obs := make([]*eventv1alpha1.Observation, len(data))
	for i, d := range data {
		obs[i] = model.NewObservation("adapter/"+source, at, d)
		if err := model.ValidateObservation(obs[i]); err != nil {
			w.t.Fatalf("observation %d: %v", i, err)
		}
	}
	if _, err := w.store.Ingest(context.Background(), source, obs); err != nil {
		w.t.Fatal(err)
	}
}

// cli runs a query command against the world's store and returns its output.
func (w *world) cli(args ...string) (string, error) {
	w.t.Helper()
	env := queryEnv{
		Open: func(_ context.Context, url string) (graph.Graph, func(context.Context) error, error) {
			if url != "mem://" {
				return nil, nil, fmt.Errorf("test store: unexpected URL %q", url)
			}
			return w.store, func(context.Context) error { return nil }, nil
		},
		Getenv: func(string) string { return "" },
		Now:    w.clock.Now,
	}
	var out bytes.Buffer
	err := runWith(context.Background(), env, append([]string{args[0], "--store", "mem://"}, args[1:]...), &out)
	return out.String(), err
}

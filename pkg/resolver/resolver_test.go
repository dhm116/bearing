package resolver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// readDeclaration loads one of the reference declarations.
func readDeclaration(t testing.TB, name string) *modelv1alpha1.AdapterDeclaration {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "declarations", name+".json")) //nolint:gosec // G304: reference declarations under testdata
	if err != nil {
		t.Fatal(err)
	}
	d := &modelv1alpha1.AdapterDeclaration{}
	if err := model.DecodeJSON(b, d); err != nil {
		t.Fatal(err)
	}
	return d
}

// testConfig is the fictional org's configuration: one GitHub source and
// one Authentik source that issues SAML IDs and links GitHub's.
func testConfig(t testing.TB) Config {
	t.Helper()
	return Config{
		Declarations: []*modelv1alpha1.AdapterDeclaration{readDeclaration(t, "github"), readDeclaration(t, "authentik")},
		Sources: map[string]*Source{
			"github-acme": {Name: "github-acme", Adapter: "github"},
			"authentik-acme": {
				Name: "authentik-acme", Adapter: "authentik",
				Issues: []Namespace{{Name: "authentik-saml", IssuerType: "saml"}},
				Links:  []Namespace{{Name: "github", IssuerType: "github"}},
			},
		},
	}
}

// env is a resolver over a fresh in-memory store.
type env struct {
	t     testing.TB
	store *memstore.Store
	clock *testkit.FakeClock
	r     *Resolver
}

func newEnv(t testing.TB) *env {
	t.Helper()
	clk := testkit.NewClock(time.Date(2026, 9, 28, 1, 30, 2, 0, time.UTC))
	s := memstore.New()
	s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	r, err := New(testConfig(t), s)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, store: s, clock: clk, r: r}
}

// fixture reads one of the example observations in testdata/observations.
func fixture(t testing.TB, name string) *eventv1alpha1.Observation {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "observations", "valid", name)) //nolint:gosec // G304: example observations under testdata
	if err != nil {
		t.Fatal(err)
	}
	o, err := model.DecodeObservation(b)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// event wraps an observation in the event the core would deliver.
func event(source string, o *eventv1alpha1.Observation) Event {
	return Event{ID: source + "/" + o.GetId(), Source: source, Observation: o}
}

func (e *env) apply(ev Event) Applied {
	e.t.Helper()
	got, err := e.r.Apply(context.Background(), ev)
	if err != nil {
		e.t.Fatalf("apply %s: %v", ev.ID, err)
	}
	e.clock.Advance(time.Second)
	return got
}

// resolveKey returns the subject key maps to at valid time v, or "".
func (e *env) resolveKey(key string, v time.Time) string {
	e.t.Helper()
	s, err := e.store.ResolveKey(context.Background(), model.Key(key), v, time.Time{})
	if err != nil {
		return ""
	}
	return s.GetSubjectId()
}

func (e *env) bindings(alias string) []*modelv1alpha1.Binding {
	e.t.Helper()
	rows, err := e.store.Bindings(context.Background(), []model.Key{model.Key(alias)}, nil, time.Time{})
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// obsAt builds an observation of an entity with the given keys.
func obsAt(at, kind, key string, aliases ...string) *eventv1alpha1.Observation {
	return model.NewObservation("adapter/test", ts(at), &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: kind, Key: key, Aliases: aliases},
	})
}

// withRelation adds a relation to an observation.
func withRelation(o *eventv1alpha1.Observation, typ, to string) *eventv1alpha1.Observation {
	o = proto.CloneOf(o)
	o.Data.Relations = append(o.Data.Relations, &modelv1alpha1.Relation{Type: typ, End: &modelv1alpha1.Relation_To{To: to}})
	return o
}

func wantSubject(t testing.TB, what, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %q, want %q", what, got, want)
	}
}

var _ contracts.GraphStore = (*memstore.Store)(nil)

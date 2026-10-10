package resolver

import (
	"cmp"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
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
// one Authentik source that issues SAML IDs and links GitHub's, and a
// second GitHub source.
func testConfig(t testing.TB) Config {
	t.Helper()
	return Config{
		Declarations: []*modelv1alpha1.AdapterDeclaration{readDeclaration(t, "github"), readDeclaration(t, "authentik")},
		Sources: map[string]*Source{
			"github-acme": {Name: "github-acme", Adapter: "github"},
			// A second source of the same system, for facts two sources claim.
			"github-mirror": {Name: "github-mirror", Adapter: "github"},
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
	// dropped counts the writes applies have reported as ignored, and
	// lastConfirmed is the latest key time among the confirmations they fell
	// among.
	dropped       int
	lastConfirmed time.Time
	// takesEffect is, by event ID, the valid time from which the latest of the
	// event's claims holds.
	takesEffect map[string]time.Time
}

func newEnv(t testing.TB) *env {
	t.Helper()
	return newEnvWith(t, testConfig(t))
}

// newEnvWith is newEnv with another configuration.
func newEnvWith(t testing.TB, cfg Config) *env {
	t.Helper()
	clk := testkit.NewClock(time.Date(2026, 9, 28, 1, 30, 2, 0, time.UTC))
	s := memstore.New()
	s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	r, err := New(cfg, s)
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
	if e.takesEffect == nil {
		e.takesEffect = map[string]time.Time{}
	}
	e.takesEffect[ev.ID] = claimsHoldFrom(ev.Observation)
	e.dropped += len(got.Dropped)
	for _, d := range got.Dropped {
		// A dropped write falls strictly among the confirmations it names.
		if model.CompareOrderingKeys(withoutHash(d.First), withoutHash(d.Key)) >= 0 || model.CompareOrderingKeys(withoutHash(d.Key), withoutHash(d.Last)) >= 0 {
			e.t.Errorf("event %s: dropped write %v is not among confirmations %v to %v", ev.ID, d.Key, d.First, d.Last)
		}
		// The run's latest confirmation decides from its observation time on, or
		// from its claim's valid_from when that is later.
		at := d.Last.GetObservedAt().AsTime()
		if v := e.takesEffect[d.Last.GetEventId()]; v.After(at) {
			at = v
		}
		if at.After(e.lastConfirmed) {
			e.lastConfirmed = at
		}
	}
	return got
}

// claimsHoldFrom is the latest valid_from among an observation's claims, or
// its observation time when none is later.
func claimsHoldFrom(o *eventv1alpha1.Observation) time.Time {
	out := o.GetTime().AsTime()
	for _, r := range o.GetData().GetRelations() {
		if v := r.GetValidFrom(); v != nil && v.AsTime().After(out) {
			out = v.AsTime()
		}
	}
	for _, c := range o.GetData().GetAttributeClaims() {
		if v := c.GetValidFrom(); v != nil && v.AsTime().After(out) {
			out = v.AsTime()
		}
	}
	return out
}

// sameOrDropped checks that an apply order gave the facts of the in-order
// apply (per valid time, in the samples taken at times), unless apply reported
// ignoring a write, and then only at valid times before the claims of the
// latest confirmation it fell among take effect (docs/spec/data-model.md,
// "Confirmations"). It reports whether the two were equal, and whether the
// order failed the check.
func (e *env) sameOrDropped(got, want string, times []time.Time) (same, failed bool) {
	if got == want {
		return true, false
	}
	g, w := strings.Split(got, "== "), strings.Split(want, "== ")
	if e.dropped == 0 || len(g) != len(w) || len(g) != len(times)+1 {
		return false, true
	}
	for i := range times {
		if g[i+1] != w[i+1] && !times[i].Before(e.lastConfirmed) {
			e.t.Logf("facts differ at %s, not before the latest confirmation dropped writes fell among (%s)", times[i], e.lastConfirmed)
			return false, true
		}
	}
	return false, false
}

// sourceOrderShuffle returns a random order of events that keeps each
// source's events in ordering-key order: no event of a source arrives after a
// later one of the same source, so no claim can be ignored.
func sourceOrderShuffle(rng *rand.Rand, events []Event) []int {
	bySource := map[string][]int{}
	for i, ev := range events {
		bySource[ev.Source] = append(bySource[ev.Source], i)
	}
	for _, idx := range bySource {
		slices.SortFunc(idx, func(a, b int) int {
			x, y := events[a].Observation, events[b].Observation
			return cmp.Or(x.GetTime().AsTime().Compare(y.GetTime().AsTime()), strings.Compare(x.GetId(), y.GetId()), strings.Compare(events[a].ID, events[b].ID))
		})
	}
	var out []int
	for len(out) < len(events) {
		var live []string
		for src, idx := range bySource {
			if len(idx) > 0 {
				live = append(live, src)
			}
		}
		slices.Sort(live)
		src := live[rng.Intn(len(live))]
		out = append(out, bySource[src][0])
		bySource[src] = bySource[src][1:]
	}
	return out
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

package e2e

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"bearing.example/adapters/kubernetes"
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/internal/fakes"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/graph"
)

// cluster is Acme's Kubernetes cluster synced into a store.
type cluster struct {
	t       *testing.T
	clock   *testkit.FakeClock
	cluster *fakes.Cluster
	a       *kubernetes.Adapter
	cfg     json.RawMessage
	store   *duckstore.Store
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	clock := testkit.NewClock(fakes.ClusterCreatedAt)
	c := fakes.NewCluster(clock)
	srv := fakes.NewKubernetes(t, c, fakes.KubernetesOptions{Token: "tok"})
	cfg, err := json.Marshal(kubernetes.Config{Cluster: fakes.ClusterID, APIURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	store, err := duckstore.Open(t.Context(), duckstore.Config{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a := &kubernetes.Adapter{HTTP: srv.Client(), Now: clock.Now, Getenv: func(string) string { return "tok" }}
	return &cluster{t: t, clock: clock, cluster: c, a: a, cfg: cfg, store: store}
}

func (c *cluster) sync() {
	c.t.Helper()
	var all []*eventv1alpha1.Observation
	cur := ""
	for range 100 {
		res, err := c.a.Sync(c.t.Context(), adapter.SyncParams{Config: c.cfg, Cursor: cur})
		if err != nil {
			c.t.Fatal(err)
		}
		all = append(all, res.Observations...)
		if res.Done {
			w := &world{t: c.t, store: c.store}
			w.ingest("kubernetes-acme", all)
			return
		}
		cur = res.NextCursor
	}
	c.t.Fatal("sync did not finish")
}

// playUntil applies the cluster's story up to and including at.
func (c *cluster) playUntil(at time.Time) {
	c.t.Helper()
	for _, s := range fakes.KubernetesStory() {
		if s.At.After(at) {
			break
		}
		c.clock.Set(s.At)
		if s.Apply != nil {
			if err := s.Apply(c.cluster); err != nil {
				c.t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
}

func workload(kind, ns, name string) string {
	return "kubernetes:workload/" + fakes.ClusterID + "/" + ns + "/" + kind + "/" + name
}

// walk returns "name (protocol)" for each fact a walk crosses, sorted.
func (c *cluster) walk(from string, dir graph.Direction) []string {
	c.t.Helper()
	w := &world{t: c.t, store: c.store}
	steps, err := c.store.Walk(c.t.Context(), graph.WalkRequest{From: w.id(from), Predicates: []string{"depends_on"}, Direction: dir})
	if err != nil {
		c.t.Fatal(err)
	}
	var out []string
	for _, s := range steps {
		end := s.Fact.Object
		if dir == graph.In {
			end = s.Fact.Subject
		}
		e, err := c.store.Entity(c.t.Context(), string(end))
		if err != nil {
			c.t.Fatal(err)
		}
		out = append(out, e.Name+" ("+fmtProtocol(s.Fact.Qualifiers)+")")
	}
	slices.Sort(out)
	return out
}

func fmtProtocol(q map[string]any) string {
	p, _ := q["protocol"].(string)
	return p
}

func TestAcmeAnswersWhatAServiceDependsOnAndHow(t *testing.T) {
	c := newCluster(t)
	c.sync()
	billing := workload("deployment", "billing", "billing-api")
	if got, want := c.walk(billing, graph.Out), []string{"ledger (http)", "postgres (postgres)"}; !slices.Equal(got, want) {
		t.Fatalf("billing-api depends on %q, want %q", got, want)
	}
	if got, want := c.walk(workload("statefulset", "billing", "postgres"), graph.In), []string{"billing-api (postgres)"}; !slices.Equal(got, want) {
		t.Fatalf("postgres is depended on by %q, want %q", got, want)
	}
}

func TestAcmeDependencyEndsWhenTheReferenceIsRemoved(t *testing.T) {
	c := newCluster(t)
	c.sync()
	c.playUntil(fakes.DependencyRemovedAt)
	c.sync()
	billing := workload("deployment", "billing", "billing-api")
	if got, want := c.walk(billing, graph.Out), []string{"postgres (postgres)"}; !slices.Equal(got, want) {
		t.Fatalf("after the change billing-api depends on %q, want %q", got, want)
	}
	changes, err := c.store.Changes(t.Context(), graph.ChangesRequest{Since: fakes.DependencyRemovedAt})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(changes, func(ch graph.Change) bool {
		return ch.Kind == graph.Closed && ch.Fact.Predicate == "depends_on"
	}) {
		t.Fatalf("changes since the removal %+v, want the depends_on closed", changes)
	}
}

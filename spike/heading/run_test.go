package heading

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"bearing.example/adapters/kubernetes"
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/internal/fakes"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/server"
)

const token = "spike-token"

// acmeMapping is the heading.yaml an author would keep for the kinds the
// hand-written adapter reads. Everything here but Attributes and Refs came
// from `heading init` (testdata/out/kubernetes.heading.yaml); Services list
// per namespace only because the Acme fake's RBAC allows nothing wider.
func acmeMapping() Mapping {
	cursor := &Cursor{Param: "continue", Field: "metadata.continue"}
	nsRef := []RefMap{{Predicate: "namespace", Field: "metadata.namespace", Kind: "Namespace"}}
	nsKey := []string{"metadata.namespace", "metadata.name"}
	image := map[string]string{"image": "spec.template.spec.containers[*].image"}
	return Mapping{Namespace: "kubernetes", Instance: fakes.ClusterID, Kinds: []KindMap{
		{Kind: "Namespace", Schema: "io.k8s.api.core.v1.Namespace", List: "/api/v1/namespaces", Items: "items", Key: []string{"metadata.name"}, Cursor: cursor},
		{
			Kind: "Service", Schema: "io.k8s.api.core.v1.Service", List: "/api/v1/namespaces/{namespace}/services",
			Params: map[string]From{"namespace": {Kind: "Namespace", Field: "metadata.name"}}, Items: "items", Key: nsKey, Cursor: cursor, Refs: nsRef,
		},
		{Kind: "Deployment", Schema: "io.k8s.api.apps.v1.Deployment", List: "/apis/apps/v1/deployments", Items: "items", Key: nsKey, Cursor: cursor, Attributes: image, Refs: nsRef},
		{Kind: "StatefulSet", Schema: "io.k8s.api.apps.v1.StatefulSet", List: "/apis/apps/v1/statefulsets", Items: "items", Key: nsKey, Cursor: cursor, Attributes: image, Refs: nsRef},
		{Kind: "DaemonSet", Schema: "io.k8s.api.apps.v1.DaemonSet", List: "/apis/apps/v1/daemonsets", Items: "items", Key: nsKey, Cursor: cursor, Attributes: image, Refs: nsRef},
	}}
}

// TestGeneratedAdapterSeesWhatTheHandWrittenOneSees runs the mapping and
// the hand-written adapter against the same Acme cluster, writes a
// comparison to testdata/out/kubernetes.compare.txt, and ingests the
// generated observations with an operator's extends to show the records.
func TestGeneratedAdapterSeesWhatTheHandWrittenOneSees(t *testing.T) {
	ctx := context.Background()
	clock := testkit.NewClock(fakes.ClusterCreatedAt.Add(time.Hour))
	cluster := fakes.NewCluster(clock)
	srv := fakes.NewKubernetes(t, cluster, fakes.KubernetesOptions{Token: token})

	gen, err := (&Runner{HTTP: srv.Client(), Base: srv.URL, Token: token, Now: clock.Now, Source: "adapter/heading-kubernetes", Spec: loadAll(t, kubernetesSpecs)}).Run(ctx, acmeMapping())
	if err != nil {
		t.Fatal(err)
	}
	hand := handWritten(t, srv, clock)

	// Every workload the hand-written adapter reports, the generated one reports too.
	genByWorkload := map[string]*modelv1alpha1.ObservationData{}
	for _, o := range gen {
		e := o.GetData().GetEntity()
		ns, name := attr(e, "metadata_namespace"), attr(e, "metadata_name")
		genByWorkload[strings.ToLower(strings.TrimPrefix(e.GetKind(), "kubernetes."))+" "+ns+"/"+name] = o.GetData()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "generated: %d observations; hand-written: %d\n\n", len(gen), len(hand))
	fmt.Fprintf(&b, "%-44s %-34s %s\n", "hand-written claim", "generated equivalent", "covered")
	gaps := map[string]int{}
	workloads := 0
	for _, o := range hand {
		e := o.GetData().GetEntity()
		if e.GetKind() != string(model.KindComponent) {
			continue
		}
		workloads++
		wk := strings.ToLower(attr(e, "workload_kind"))
		key := strings.Split(e.GetKey(), "/")
		g := genByWorkload[wk+" "+key[len(key)-3]+"/"+attr(e, "workload_name")]
		if g == nil {
			t.Errorf("%s: no generated observation", e.GetKey())
			continue
		}
		for _, row := range compare(o.GetData(), g) {
			if workloads == 1 {
				fmt.Fprintf(&b, "%-44s %-34s %v\n", row.claim, row.equiv, row.ok)
			}
			if !row.ok {
				gaps[row.claim]++
			}
		}
	}
	fmt.Fprintf(&b, "\n(rows above are for the first workload; gaps across all %d workloads:)\n", workloads)
	for _, k := range sortedKeys(gaps) {
		fmt.Fprintf(&b, "  %-44s missing for %d\n", k, gaps[k])
	}
	if workloads == 0 {
		t.Fatal("the hand-written adapter reported no workloads")
	}

	// The hand-written adapter never reports a value it doesn't need; the
	// generated one flattens whatever the schema allows.
	genJSON, handJSON := marshal(t, gen), marshal(t, hand)
	fmt.Fprintf(&b, "\nfake canary values leaked: generated %d of %d, hand-written %d\n",
		len(testkit.FindLeaks(genJSON, fakes.Canaries()...)), len(fakes.Canaries()), len(testkit.FindLeaks(handJSON, fakes.Canaries()...)))

	// Ingest with the operator's extends: Deployments become Components.
	b.WriteString("\n" + ingest(ctx, t, clock, gen))
	write(t, "testdata/out/kubernetes.compare.txt", b.String())
	t.Log("\n" + b.String())
}

func handWritten(t *testing.T, srv *fakes.Kubernetes, clock *testkit.FakeClock) []*eventv1alpha1.Observation {
	t.Helper()
	cfg, _ := json.Marshal(kubernetes.Config{Cluster: fakes.ClusterID, APIURL: srv.URL})
	a := &kubernetes.Adapter{HTTP: srv.Client(), Now: clock.Now, Getenv: func(string) string { return token }}
	var all []*eventv1alpha1.Observation
	cur := ""
	for range 200 {
		res, err := a.Sync(context.Background(), adapter.SyncParams{Config: cfg, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, res.Observations...)
		if res.Done {
			return all
		}
		cur = res.NextCursor
	}
	t.Fatal("sync did not finish")
	return nil
}

type row struct {
	claim, equiv string
	ok           bool
}

// compare lines up what the hand-written adapter says about a workload with
// what the generated one says.
func compare(hand, gen *modelv1alpha1.ObservationData) []row {
	he, ge := hand.GetEntity(), gen.GetEntity()
	rows := []row{
		{"workload_name", "metadata_name", attr(he, "workload_name") == attr(ge, "metadata_name")},
		{"workload_kind", "the kind itself", strings.EqualFold("kubernetes."+attr(he, "workload_kind"), ge.GetKind())},
		{"image", "image (mapping projection)", fmt.Sprint(he.GetAttributes()["image"].AsInterface()) == fmt.Sprint(ge.GetAttributes()["image"].AsInterface())},
	}
	if r, ok := he.GetAttributes()["replicas"]; ok {
		rows = append(rows, row{"replicas", "spec_replicas", fmt.Sprint(r.AsInterface()) == fmt.Sprint(ge.GetAttributes()["spec_replicas"].AsInterface())})
	}
	genRels := map[string]bool{}
	for _, r := range gen.GetRelations() {
		genRels[r.GetType()] = true
	}
	equiv := map[string]string{"deployed_to": "namespace"}
	for _, r := range hand.GetRelations() {
		claim := "relation " + r.GetType()
		e := equiv[r.GetType()]
		rows = append(rows, row{claim, orNone(e), e != "" && genRels[e]})
	}
	return dedupe(rows)
}

func dedupe(rows []row) []row {
	seen := map[string]bool{}
	var out []row
	for _, r := range rows {
		if !seen[r.claim] {
			seen[r.claim] = true
			out = append(out, r)
		}
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func attr(e *modelv1alpha1.Entity, name string) string {
	v := e.GetAttributes()[name]
	if v == nil {
		return ""
	}
	return fmt.Sprint(v.AsInterface())
}

// ingest applies the generated observations through the server's applier
// with the generated kinds and an operator config that extends the
// workloads to Component and the namespace to Environment, and renders one
// record as the store holds it.
func ingest(ctx context.Context, t *testing.T, clock *testkit.FakeClock, obs []*eventv1alpha1.Observation) string {
	t.Helper()
	extends := map[string]model.Kind{"Deployment": model.KindComponent, "StatefulSet": model.KindComponent, "DaemonSet": model.KindComponent}
	var defs []model.KindDef
	var namespaced []model.Kind
	for _, km := range acmeMapping().Kinds {
		k := model.Kind("kubernetes." + km.Kind)
		defs = append(defs, model.KindDef{Name: k, Parent: extends[km.Kind]})
		if km.Kind != "Namespace" {
			namespaced = append(namespaced, k)
		}
	}
	ns := model.Predicate{
		Name: "namespace", Relation: true, Domain: namespaced, Range: []model.Kind{"kubernetes.Namespace"},
		Cardinality: modelv1alpha1.Cardinality_CARDINALITY_ONE, Conflict: modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_ONE,
	}
	reg, err := model.Core().Extend(defs, []model.Predicate{ns})
	if err != nil {
		t.Fatal(err)
	}
	store, err := duckstore.Open(ctx, duckstore.Config{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	read, err := store.SyncVocabulary(ctx, reg, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	store.SetRegistry(read)
	ap := server.StoreApplier{Store: store, Registry: read}
	if err := ap.Apply(ctx, "heading-kubernetes", obs, server.Completion{}); err != nil {
		t.Fatal(err)
	}
	comps, err := store.Records(ctx, model.KindComponent)
	if err != nil {
		t.Fatal(err)
	}
	if len(comps) == 0 {
		t.Fatal("got no Components: extends did not apply")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "store: %d records answer as Component through extends\n", len(comps))
	ent, err := store.Entity(ctx, "kubernetes:deployment/"+fakes.ClusterID+"/payments/payments-api")
	if err != nil {
		t.Fatal(err)
	}
	facts, err := store.Facts(ctx, graph.FactFilter{Subject: ent.ID})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "record %s (%s), %d facts:\n", ent.Name, ent.Kind, len(facts))
	sort.Slice(facts, func(i, j int) bool { return facts[i].Predicate < facts[j].Predicate })
	for _, f := range facts {
		v := fmt.Sprint(f.Value)
		if f.Object != "" {
			v = "-> " + string(f.Object)
		}
		if len(v) > 70 {
			v = v[:67] + "..."
		}
		fmt.Fprintf(&b, "  %-36s %s\n", f.Predicate, v)
	}
	return b.String()
}

func marshal(t *testing.T, obs []*eventv1alpha1.Observation) []byte {
	t.Helper()
	var b []byte
	for _, o := range obs {
		j, err := protojson.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, j...)
	}
	return b
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

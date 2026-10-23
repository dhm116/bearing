package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

const testToken = "test-token-3f9a" //nolint:gosec // a fake

type rig struct {
	t       *testing.T
	clock   *testkit.FakeClock
	cluster *fakes.Cluster
	srv     *fakes.Kubernetes
	a       *Adapter
	cfg     json.RawMessage
}

func newRig(t *testing.T, mod ...func(*Config)) *rig {
	t.Helper()
	c := testkit.NewClock(fakes.ClusterCreatedAt)
	cluster := fakes.NewCluster(c)
	srv := fakes.NewKubernetes(t, cluster, fakes.KubernetesOptions{Token: testToken})
	cfg := Config{Cluster: fakes.ClusterID, APIURL: srv.URL}
	for _, m := range mod {
		m(&cfg)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := &Adapter{HTTP: srv.Client(), Now: c.Now, Getenv: func(k string) string {
		if k == "KUBERNETES_TOKEN" {
			return testToken
		}
		return ""
	}}
	return &rig{t: t, clock: c, cluster: cluster, srv: srv, a: a, cfg: raw}
}

// playUntil applies the story's steps up to and including t.
func (r *rig) playUntil(t time.Time) {
	r.t.Helper()
	for _, s := range fakes.KubernetesStory() {
		if s.At.After(t) {
			break
		}
		r.clock.Set(s.At)
		if s.Apply != nil {
			if err := s.Apply(r.cluster); err != nil {
				r.t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
	r.clock.Set(t)
}

func (r *rig) trySync() (adapter.Observations, adapter.SyncResult, error) {
	var all adapter.Observations
	cur := ""
	for range 100 {
		res, err := r.a.Sync(context.Background(), adapter.SyncParams{Config: r.cfg, Cursor: cur})
		if err != nil {
			return all, res, err
		}
		all = append(all, res.Observations...)
		if res.Done {
			return all, res, nil
		}
		cur = res.NextCursor
	}
	return all, adapter.SyncResult{}, errors.New("sync did not finish")
}

func (r *rig) sync() (adapter.Observations, adapter.SyncResult) {
	r.t.Helper()
	obs, last, err := r.trySync()
	if err != nil {
		r.t.Fatal(err)
	}
	return obs, last
}

func byKey(obs adapter.Observations) map[string]*modelv1alpha1.ObservationData {
	m := map[string]*modelv1alpha1.ObservationData{}
	for _, o := range obs {
		m[o.GetData().GetEntity().GetKey()] = o.GetData()
	}
	return m
}

func relTargets(d *modelv1alpha1.ObservationData, typ string) []string {
	var out []string
	for _, r := range d.GetRelations() {
		if r.GetType() == typ {
			out = append(out, r.GetTo())
		}
	}
	return out
}

// TestSyncMatchesTheSpecSamples: at the spec samples' time the adapter's
// observations for payments-api, billing-core, legacy-billing, payments and
// the payments team are the example files, field for field.
func TestSyncMatchesTheSpecSamples(t *testing.T) {
	r := newRig(t)
	r.playUntil(fakes.ImageBumpedAt)
	obs, last := r.sync()
	if !last.Done || last.CompleteSync == nil {
		t.Fatalf("last page done %v, complete_sync %v: want done and complete", last.Done, last.CompleteSync)
	}
	if got, want := last.CompleteSync.GetKinds(), []string{"Component", "Environment", "Team"}; !slices.Equal(got, want) {
		t.Errorf("complete_sync kinds %v, want %v", got, want)
	}
	got := map[string]*modelv1alpha1.ObservationData{}
	for _, o := range obs {
		got[o.GetId()] = o.GetData()
	}
	for _, f := range []string{"9-kubernetes-workload-annotation", "10-kubernetes-workload-image-hint", "11-kubernetes-workload-no-repository", "12-kubernetes-namespace", "13-kubernetes-team-label", "14-kubernetes-workload-dependencies"} {
		raw, err := os.ReadFile(filepath.Join("../../testdata/observations/valid", f+".json")) //nolint:gosec // a test fixture
		if err != nil {
			t.Fatal(err)
		}
		want, err := model.DecodeObservation(raw)
		if err != nil {
			t.Fatal(err)
		}
		d, ok := got[want.GetId()]
		if !ok {
			t.Errorf("%s: no observation with id %s", f, want.GetId())
			continue
		}
		if !proto.Equal(d, want.GetData()) {
			t.Errorf("%s:\ngot  %v\nwant %v", f, d, want.GetData())
		}
	}
	// The excluded namespace and its workloads are not read.
	for k := range byKey(obs) {
		if strings.Contains(k, "kube-system") {
			t.Errorf("observed %s from an excluded namespace", k)
		}
	}
}

// TestSyncStaysInsideTheDeclarationAndLeaksNothing validates every
// observation against the declaration and searches them for the canaries.
func TestSyncStaysInsideTheDeclarationAndLeaksNothing(t *testing.T) {
	r := newRig(t)
	obs, _ := r.sync()
	raw, err := os.ReadFile("../../testdata/declarations/kubernetes.json")
	if err != nil {
		t.Fatal(err)
	}
	decl := &modelv1alpha1.AdapterDeclaration{}
	if err := model.DecodeJSON(raw, decl); err != nil {
		t.Fatal(err)
	}
	declared := model.DeclaredAttributes(decl)
	var all strings.Builder
	for _, o := range obs {
		if err := model.ValidateObservationWith(o, declared); err != nil {
			t.Errorf("%s: %v", o.GetId(), err)
		}
		b, err := model.EncodeJSON(o)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
	}
	testkit.AssertNoLeaks(t, all.String(), fakes.Canaries()...)
	for _, req := range r.srv.Requests() {
		if strings.Contains(req.Path, "kube-system") || req.Path == "/apis/apps/v1/deployments" {
			t.Errorf("requested %s: want namespace-by-namespace lists of included namespaces only", req.Path)
		}
	}
}

func TestSyncPagesByCursorWhateverThePageSize(t *testing.T) {
	ids := func(size int, expire bool) []string {
		r := newRig(t, func(c *Config) { c.PageSize = size })
		r.srv.ExpireContinue(expire)
		obs, _ := r.sync()
		var out []string
		for _, o := range obs {
			out = append(out, o.GetId())
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	all := ids(500, false)
	if len(all) == 0 {
		t.Fatal("no observations")
	}
	for name, got := range map[string][]string{"page size 1": ids(1, false), "page size 1, continue tokens expired": ids(1, true)} {
		if !slices.Equal(got, all) {
			t.Errorf("%s gave %v, want %v", name, got, all)
		}
	}
}

// TestSecondSyncEndsWhatTheClusterEnded: after the team label is cleared and
// legacy-billing deleted, the next sync snapshots owned_by with no target
// and no longer lists the workload, and says it is complete.
func TestSecondSyncEndsWhatTheClusterEnded(t *testing.T) {
	r := newRig(t)
	r.playUntil(fakes.ImageBumpedAt)
	first, _ := r.sync()
	if _, ok := byKey(first)["kubernetes:workload/k3d-acme/billing/deployment/legacy-billing"]; !ok {
		t.Fatal("first sync lacks legacy-billing")
	}
	r.playUntil(fakes.WorkloadDeletedAt)
	second, last := r.sync()
	got := byKey(second)
	if _, ok := got["kubernetes:workload/k3d-acme/billing/deployment/legacy-billing"]; ok || last.CompleteSync == nil {
		t.Errorf("second sync still lists legacy-billing, or is not complete (%v)", last.CompleteSync)
	}
	core := got["kubernetes:workload/k3d-acme/billing/statefulset/billing-core"]
	if core == nil {
		t.Fatal("second sync lacks billing-core")
	}
	if owners := relTargets(core, "owned_by"); len(owners) != 0 {
		t.Errorf("billing-core owned_by %v, want none after the label was cleared", owners)
	}
	if len(core.GetSnapshots()) != 1 || !slices.Contains(core.GetSnapshots()[0].GetPredicates(), "owned_by") {
		t.Errorf("billing-core snapshots %v, want one covering owned_by so the old fact ends", core.GetSnapshots())
	}
	pay := got["kubernetes:workload/k3d-acme/payments/deployment/payments-api"]
	if repos := relTargets(pay, "defined_in"); !slices.Equal(repos, []string{"github:repo/acme/payments-api"}) {
		// The edited annotation is not a slug, so the image hint takes over.
		t.Errorf("payments-api defined_in %v, want the image hint's repository", repos)
	}
	if q := pay.GetRelations()[len(pay.GetRelations())-1].GetAttributes(); q["image"] == nil || q["annotation"] != nil {
		t.Errorf("payments-api defined_in qualifiers %v, want the image hint", q)
	}
	// Excluding billing is the adapter's setting: nothing of it is listed.
	r2 := newRig(t, func(c *Config) { c.ExcludeNamespaces = fakes.ExcludedNamespaces(fakes.NamespaceExcludedAt) })
	for k := range byKey(func() adapter.Observations { o, _ := r2.sync(); return o }()) {
		if strings.Contains(k, "/billing") {
			t.Errorf("observed %s from the excluded billing namespace", k)
		}
	}
}

func TestSyncFailuresAreErrorsNeverCompleteSyncs(t *testing.T) {
	canary := testkit.Canary("env:KUBERNETES_TOKEN")
	tests := []struct {
		name  string
		setup func(r *rig)
		want  string
	}{
		{"bad token", func(r *rig) { r.a.Getenv = func(string) string { return canary } }, "401"},
		{"no token", func(r *rig) { r.a.Getenv = func(string) string { return "" } }, "401"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			tt.setup(r)
			obs, last, err := r.trySync()
			var rpcErr *adapter.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != adapter.CodeUpstream || !strings.Contains(rpcErr.Message, tt.want) {
				t.Fatalf("got %v, want an upstream error containing %q", err, tt.want)
			}
			if len(obs) != 0 || last.Done || last.CompleteSync != nil {
				t.Fatalf("got %d observations, done %v: want nothing", len(obs), last.Done)
			}
			testkit.AssertNoLeaks(t, err.Error(), canary)
		})
	}
}

func TestParseConfigRejectsBadInput(t *testing.T) {
	for name, raw := range map[string]string{ //nolint:gosec // G101: variable names, not credentials
		"no cluster": `{"api_url":"https://x"}`, "no api_url": `{"cluster":"c"}`, "bad namespace": `{"cluster":"c","api_url":"https://x","namespace":"A B"}`,
		"not json":                `{`,
		"plain http off loopback": `{"cluster":"c","api_url":"http://10.0.0.1:6443"}`,
		"plain http to localhost": `{"cluster":"c","api_url":"http://localhost:6443"}`,
		"no scheme":               `{"cluster":"c","api_url":"kube.example:6443"}`,
		"https with no host":      `{"cluster":"c","api_url":"https:///api"}`,
		"a lower-case token_env":  `{"cluster":"c","api_url":"https://x","token_env":"kube_token"}`,
		"a token_env with a dash": `{"cluster":"c","api_url":"https://x","token_env":"KUBE-TOKEN"}`,
		"Bearing's own variable":  `{"cluster":"c","api_url":"https://x","token_env":"BEARING_ADAPTER_TOKEN"}`,
		"the store password":      `{"cluster":"c","api_url":"https://x","token_env":"BEARING_STORE_PASSWORD"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var e *adapter.Error
			if _, err := parseConfig(json.RawMessage(raw)); !errors.As(err, &e) || e.Code != adapter.CodeInvalidParams {
				t.Fatalf("got %v, want an invalid-params error", err)
			}
		})
	}
	for name, raw := range map[string]string{ //nolint:gosec // G101: variable names, not credentials
		"https":                      `{"cluster":"c","api_url":"https://kube.example:6443"}`,
		"plain http to 127.0.0.1":    `{"cluster":"c","api_url":"http://127.0.0.1:6443"}`,
		"plain http to ::1":          `{"cluster":"c","api_url":"http://[::1]:6443"}`,
		"a secret the core hands it": `{"cluster":"c","api_url":"https://x","token_env":"BEARING_SECRET_KUBE_TOKEN"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig(json.RawMessage(raw)); err != nil {
				t.Fatalf("got %v, want the config accepted", err)
			}
		})
	}
}

func TestDescribeMatchesTheDeclaration(t *testing.T) {
	d, err := New().Describe(context.Background())
	if err != nil || d.Webhooks || len(d.Emits) != 3 {
		t.Fatalf("got %+v (%v), want three kinds and no webhooks", d, err)
	}
	if _, err := New().Handle(context.Background(), adapter.HandleParams{}); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("Handle: got %v, want ErrNotSupported", err)
	}
}

package fakes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bearing.example/internal/testkit"
)

const kubeToken = "k8s-test-token" //nolint:gosec // G101: a fake bearer token

// kubeFeedPath is the recorded Kubernetes lists, shared by tests and demos.
const kubeFeedPath = "../../testdata/acme/kubernetes.ndjson"

func newKubernetes(t *testing.T, opts KubernetesOptions) (*Kubernetes, *Cluster, *testkit.FakeClock) {
	t.Helper()
	c := testkit.NewClock(Start)
	k := NewCluster(c)
	if opts.Token == "" {
		opts.Token = kubeToken
	}
	return NewKubernetes(t, k, opts), k, c
}

// kubeDo sends a request with token (none if empty) and returns the status
// and body.
func kubeDo(t *testing.T, kb *Kubernetes, method, path, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, kb.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

// kubeGet lists path with the right token and requires a 200.
func kubeGet(t *testing.T, kb *Kubernetes, path string) map[string]any {
	t.Helper()
	status, body := kubeDo(t, kb, http.MethodGet, path, kubeToken)
	if status != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200: %s", path, status, body)
	}
	return decode[map[string]any](t, body)
}

// kubeItems returns the objects of a List.
func kubeItems(t *testing.T, list map[string]any) []map[string]any {
	t.Helper()
	raw, _ := list["items"].([]any)
	out := make([]map[string]any, len(raw))
	for i, it := range raw {
		out[i], _ = it.(map[string]any)
	}
	return out
}

// kubeNames returns "namespace/name" (or "name" for a namespace) of each item.
func kubeNames(t *testing.T, list map[string]any) []string {
	t.Helper()
	var out []string
	for _, it := range kubeItems(t, list) {
		name, _ := field(it, "metadata", "name").(string)
		if ns, _ := field(it, "metadata", "namespace").(string); ns != "" {
			name = ns + "/" + name
		}
		out = append(out, name)
	}
	return out
}

// kubeWorkload returns the served object for ref.
func kubeWorkload(t *testing.T, kb *Kubernetes, ref Ref) map[string]any {
	t.Helper()
	resource := strings.ToLower(ref.Kind) + "s"
	list := kubeGet(t, kb, "/apis/apps/v1/namespaces/"+ref.Namespace+"/"+resource)
	for _, it := range kubeItems(t, list) {
		if field(it, "metadata", "name") == ref.Name {
			return it
		}
	}
	return nil
}

// playTo applies the story's cluster changes up to and including those at
// or before until, setting the clock to each step's time.
func playTo(t *testing.T, k *Cluster, c *testkit.FakeClock, until time.Time) {
	t.Helper()
	for _, s := range KubernetesStory() {
		if s.At.After(until) {
			break
		}
		c.Set(s.At)
		if s.Apply != nil {
			if err := s.Apply(k); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
	c.Set(until)
}

// at follows string keys and int indexes through decoded JSON; nil if the
// path leaves it.
func at(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			l, _ := v.([]any)
			if k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

func strMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := map[string]string{}
	for k, val := range m {
		out[k], _ = val.(string)
	}
	return out
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func TestKubernetesServesTheSpecExample(t *testing.T) {
	kb, k, c := newKubernetes(t, KubernetesOptions{})
	playTo(t, k, c, ImageBumpedAt)

	// The spec's example input (docs/spec/adapter-protocol.md, Kubernetes).
	got := kubeWorkload(t, kb, Ref{KindDeployment, "payments", "payments-api"})
	if got == nil {
		t.Fatal("payments-api is not served in payments")
	}
	wantLabels := map[string]string{"team": "payments", "tier": "gold"}
	if g := strMap(field(got, "metadata", "labels")); !equalMaps(g, wantLabels) {
		t.Errorf("labels: got %v, want %v", g, wantLabels)
	}
	wantAnnotations := map[string]string{"github.com/project-slug": "acme/payments-api", "deployment.kubernetes.io/revision": "7"}
	if g := strMap(field(got, "metadata", "annotations")); !equalMaps(g, wantAnnotations) {
		t.Errorf("annotations: got %v, want %v", g, wantAnnotations)
	}
	if g := field(got, "spec", "replicas"); g != float64(3) {
		t.Errorf("replicas: got %v, want 3", g)
	}
	containers, _ := field(got, "spec", "template", "spec", "containers").([]any)
	if len(containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(containers))
	}
	ctr, _ := containers[0].(map[string]any)
	if ctr["name"] != "api" || ctr["image"] != "ghcr.io/acme/payments-api:1.4.2" {
		t.Errorf("container: got name %v image %v", ctr["name"], ctr["image"])
	}
	if args, _ := ctr["args"].([]any); len(args) != 1 || args[0] != "--token=not-a-real-token" {
		t.Errorf("args: got %v", ctr["args"])
	}
	env, _ := ctr["env"].([]any)
	if len(env) != 1 || !equalMaps(strMap(env[0]), map[string]string{"name": "DATABASE_PASSWORD", "value": "not-a-real-password"}) {
		t.Errorf("env: got %v", ctr["env"])
	}
}

// sampleFile is a sample observation of the spec, read as the oracle for
// what the adapter will report from the cluster.
type sampleFile struct {
	Data struct {
		Entity struct {
			Kind       string         `json:"kind"`
			Key        string         `json:"key"`
			Attributes map[string]any `json:"attributes"`
		} `json:"entity"`
		Relations []struct {
			Type          string         `json:"type"`
			To            string         `json:"to"`
			ConfidencePPM int            `json:"confidence_ppm"`
			Attributes    map[string]any `json:"attributes"`
		} `json:"relations"`
	} `json:"data"`
}

func readSample(t *testing.T, name string) sampleFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "observations", "valid", name)) //nolint:gosec // G304: a fixed test directory
	if err != nil {
		t.Fatal(err)
	}
	return decode[sampleFile](t, b)
}

// The owner and repository expected below restate the spec's rules (the
// label, then the annotation, then the image hint) so a fake that drifts
// from the samples fails; a change to those rules is made here too.
//
// TestKubernetesAgreesWithTheSpecSamples reads the cluster at the samples'
// time and checks that each workload's served fields lead to the sample the
// spec shows for it: the same name, kind, images, replicas, owner and
// repository, and the same namespace Environment and Team.
func TestKubernetesAgreesWithTheSpecSamples(t *testing.T) {
	kb, k, c := newKubernetes(t, KubernetesOptions{})
	playTo(t, k, c, ImageBumpedAt)

	tests := []struct {
		sample string
		ref    Ref
	}{
		{"9-kubernetes-workload-annotation.json", Ref{KindDeployment, "payments", "payments-api"}},
		{"10-kubernetes-workload-image-hint.json", Ref{KindStatefulSet, "billing", "billing-core"}},
		{"11-kubernetes-workload-no-repository.json", Ref{KindDeployment, "billing", "legacy-billing"}},
	}
	for _, tt := range tests {
		t.Run(tt.ref.Name, func(t *testing.T) {
			s := readSample(t, tt.sample)
			served := kubeWorkload(t, kb, tt.ref)
			if served == nil {
				t.Fatalf("%v is not served", tt.ref)
			}
			attrs := s.Data.Entity.Attributes
			wantKey := fmt.Sprintf("kubernetes:workload/%s/%s/%s/%s", ClusterID, tt.ref.Namespace, strings.ToLower(tt.ref.Kind), tt.ref.Name)
			if s.Data.Entity.Key != wantKey {
				t.Errorf("sample key %q, want %q", s.Data.Entity.Key, wantKey)
			}
			if attrs["workload_name"] != tt.ref.Name || attrs["workload_kind"] != tt.ref.Kind {
				t.Errorf("sample names %v %v, want %s %s", attrs["workload_name"], attrs["workload_kind"], tt.ref.Name, tt.ref.Kind)
			}
			if got := field(served, "spec", "replicas"); got != attrs["replicas"] {
				t.Errorf("replicas: served %v, sample %v", got, attrs["replicas"])
			}
			var images []any
			for i := 0; at(served, "spec", "template", "spec", "containers", i) != nil; i++ {
				images = append(images, at(served, "spec", "template", "spec", "containers", i, "image"))
			}
			if fmt.Sprint(images) != fmt.Sprint(attrs["image"]) {
				t.Errorf("images: served %v, sample %v", images, attrs["image"])
			}

			labels := strMap(field(served, "metadata", "labels"))
			annotations := strMap(field(served, "metadata", "annotations"))
			var gotNS, gotTeam, gotRepo, gotRepoVia string
			for _, r := range s.Data.Relations {
				switch r.Type {
				case "deployed_to":
					gotNS = r.To
				case "owned_by":
					gotTeam = r.To
					if r.Attributes["label"] != "team" {
						t.Errorf("owned_by qualifier %v, want label team", r.Attributes)
					}
				case "defined_in":
					gotRepo = r.To
					for k, v := range r.Attributes {
						gotRepoVia = fmt.Sprintf("%s=%v/%d", k, v, r.ConfidencePPM)
					}
				}
			}
			if want := "kubernetes:namespace/" + ClusterID + "/" + tt.ref.Namespace; gotNS != want {
				t.Errorf("deployed_to: sample %q, want %q", gotNS, want)
			}
			wantTeam := ""
			if team := labels["team"]; team != "" {
				wantTeam = "kubernetes:team/" + team
			}
			if gotTeam != wantTeam {
				t.Errorf("owned_by: sample %q, served label gives %q", gotTeam, wantTeam)
			}
			wantRepo, wantVia := "", ""
			if slug := annotations["github.com/project-slug"]; slug != "" {
				wantRepo, wantVia = "github:repo/"+slug, "annotation=github.com/project-slug/900000"
			} else if img := fmt.Sprint(images[0]); strings.HasPrefix(img, "ghcr.io/") {
				path, _, _ := strings.Cut(img, ":")
				wantRepo, wantVia = "github:repo/"+strings.TrimPrefix(path, "ghcr.io/"), "image="+path+"/300000"
			}
			if gotRepo != wantRepo || gotRepoVia != wantVia {
				t.Errorf("defined_in: sample %q via %q, served fields give %q via %q", gotRepo, gotRepoVia, wantRepo, wantVia)
			}
		})
	}

	t.Run("environment", func(t *testing.T) {
		s := readSample(t, "12-kubernetes-namespace.json")
		if s.Data.Entity.Key != "kubernetes:namespace/"+ClusterID+"/payments" ||
			s.Data.Entity.Attributes["name"] != "payments" || s.Data.Entity.Attributes["cluster"] != ClusterID {
			t.Errorf("sample %+v", s.Data.Entity)
		}
		if names := kubeNames(t, kubeGet(t, kb, "/api/v1/namespaces")); !slices.Contains(names, "payments") {
			t.Errorf("namespaces %v do not include payments", names)
		}
	})
	t.Run("team", func(t *testing.T) {
		s := readSample(t, "13-kubernetes-team-label.json")
		if s.Data.Entity.Key != "kubernetes:team/payments" || s.Data.Entity.Attributes["name"] != "payments" {
			t.Errorf("sample %+v", s.Data.Entity)
		}
	})
}

func TestKubernetesListShape(t *testing.T) {
	kb, _, _ := newKubernetes(t, KubernetesOptions{})

	list := kubeGet(t, kb, "/apis/apps/v1/namespaces/payments/deployments")
	if list["kind"] != "DeploymentList" || list["apiVersion"] != "apps/v1" {
		t.Errorf("got %v %v, want DeploymentList apps/v1", list["kind"], list["apiVersion"])
	}
	rv, _ := field(list, "metadata", "resourceVersion").(string)
	if _, err := strconv.Atoi(rv); err != nil {
		t.Errorf("list resourceVersion %q is not a number", rv)
	}
	for _, it := range kubeItems(t, list) {
		if it["kind"] != nil || it["apiVersion"] != nil {
			t.Errorf("list item carries kind or apiVersion, which a real list's items do not: %v", it)
		}
	}

	if got := kubeNames(t, kubeGet(t, kb, "/api/v1/namespaces")); !slices.Equal(got, []string{"billing", "kube-system", "payments"}) {
		t.Errorf("namespaces: got %v", got)
	}
	// A namespace that doesn't exist lists as empty, as on a real cluster.
	if got := kubeNames(t, kubeGet(t, kb, "/apis/apps/v1/namespaces/nowhere/deployments")); len(got) != 0 {
		t.Errorf("nowhere: got %v, want none", got)
	}
	// Each kind is its own list; a DaemonSet has no replicas.
	if got := kubeNames(t, kubeGet(t, kb, "/apis/apps/v1/namespaces/billing/statefulsets")); !slices.Equal(got, []string{"billing/billing-core"}) {
		t.Errorf("statefulsets: got %v", got)
	}
	ds := kubeWorkload(t, kb, Ref{KindDaemonSet, "kube-system", "svclb-traefik"})
	if ds == nil {
		t.Fatal("no DaemonSet in kube-system")
	}
	if at(ds, "spec", "replicas") != nil {
		t.Error("a DaemonSet is served with spec.replicas")
	}
	// All namespaces at once, in namespace and name order.
	if got := kubeNames(t, kubeGet(t, kb, "/apis/apps/v1/deployments")); !slices.Equal(got, []string{"billing/legacy-billing", "kube-system/coredns", "payments/payments-api"}) {
		t.Errorf("deployments: got %v", got)
	}
	// Only v1 of apps is served.
	if status, _ := kubeDo(t, kb, http.MethodGet, "/apis/apps/v1beta1/deployments", kubeToken); status != http.StatusNotFound {
		t.Errorf("apps/v1beta1: status %d, want 404", status)
	}
}

func TestKubernetesAuthentication(t *testing.T) {
	kb, _, _ := newKubernetes(t, KubernetesOptions{})
	tests := []struct {
		name, token string
		want        int
	}{
		{"right token", kubeToken, http.StatusOK},
		{"wrong token", "other", http.StatusUnauthorized},
		{"no token", "", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := kubeDo(t, kb, http.MethodGet, "/api/v1/namespaces", tt.token)
			if status != tt.want {
				t.Fatalf("got %d, want %d: %s", status, tt.want, body)
			}
			if tt.want == http.StatusUnauthorized {
				st := decode[map[string]any](t, body)
				if st["kind"] != "Status" || st["reason"] != "Unauthorized" || st["code"] != float64(401) {
					t.Errorf("got %v, want a 401 Status", st)
				}
			}
		})
	}
	t.Run("authentication comes before authorization", func(t *testing.T) {
		// A caller who isn't known is told so, not what it may not do.
		for _, token := range []string{"other", ""} {
			if status, _ := kubeDo(t, kb, http.MethodGet, "/api/v1/namespaces/payments/secrets", token); status != http.StatusUnauthorized {
				t.Errorf("token %q on a forbidden path: got %d, want 401", token, status)
			}
		}
	})
	t.Run("an empty Token accepts any token", func(t *testing.T) {
		c := testkit.NewClock(Start)
		open := NewKubernetes(t, NewCluster(c), KubernetesOptions{})
		if status, _ := kubeDo(t, open, http.MethodGet, "/api/v1/namespaces", "whatever"); status != http.StatusOK {
			t.Errorf("got %d, want 200", status)
		}
		if status, _ := kubeDo(t, open, http.MethodGet, "/api/v1/namespaces", ""); status != http.StatusUnauthorized {
			t.Errorf("without a token: got %d, want 401", status)
		}
	})
}

// TestKubernetesForbidsAnythingButTheFourLists is the read-only token the
// spec asks for: it may list namespaces and the three workload kinds, and
// every other call is a 403 that names what was refused and is recorded.
func TestKubernetesForbidsAnythingButTheFourLists(t *testing.T) {
	kb, _, _ := newKubernetes(t, KubernetesOptions{})
	tests := []struct {
		method, path string
		message      string
	}{
		{"GET", "/api/v1/namespaces/payments/secrets", `secrets is forbidden: User "` + ServiceAccountUser + `" cannot list resource "secrets" in API group "" in the namespace "payments"`},
		{"GET", "/api/v1/secrets", `cannot list resource "secrets" in API group "" at the cluster scope`},
		{"GET", "/api/v1/namespaces/payments/secrets/payments-db", `cannot get resource "secrets"`},
		{"GET", "/api/v1/namespaces/payments/configmaps", `configmaps is forbidden`},
		{"GET", "/api/v1/namespaces/billing/configmaps/billing-config", `cannot get resource "configmaps"`},
		{"GET", "/api/v1/namespaces/payments/pods", `pods is forbidden`},
		{"GET", "/apis/apps/v1/namespaces/payments/replicasets", `replicasets.apps is forbidden`},
		{"GET", "/apis/batch/v1/namespaces/payments/jobs", `jobs.batch is forbidden`},
		{"GET", "/apis/apps/v1/namespaces/payments/deployments/payments-api", `cannot get resource "deployments"`},
		{"GET", "/api/v1/namespaces/payments", `cannot get resource "namespaces"`},
		{"GET", "/apis/apps/v1/namespaces/payments/deployments?watch=true", `cannot watch resource "deployments"`},
		{"POST", "/apis/apps/v1/namespaces/payments/deployments", `cannot create resource "deployments"`},
		{"PATCH", "/apis/apps/v1/namespaces/payments/deployments/payments-api", `cannot patch resource "deployments"`},
		{"DELETE", "/apis/apps/v1/namespaces/payments/deployments/payments-api", `cannot delete resource "deployments"`},
		{"GET", "/version", `cannot get path "/version"`},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			status, body := kubeDo(t, kb, tt.method, tt.path, kubeToken)
			if status != http.StatusForbidden {
				t.Fatalf("got %d, want 403: %s", status, body)
			}
			st := decode[map[string]any](t, body)
			if st["kind"] != "Status" || st["reason"] != "Forbidden" || st["code"] != float64(403) {
				t.Errorf("got %v, want a 403 Status", st)
			}
			if msg, _ := st["message"].(string); !strings.Contains(msg, tt.message) {
				t.Errorf("message %q does not contain %q", msg, tt.message)
			}
			// The request log has the call, so a test can show the adapter
			// never made it or that it was refused.
			last := kb.Requests()[len(kb.Requests())-1]
			wantPath, _, _ := strings.Cut(tt.path, "?")
			if last.Method != tt.method || last.Path != wantPath {
				t.Errorf("request log ends with %s %s, want %s %s", last.Method, last.Path, tt.method, wantPath)
			}
		})
	}
}

func TestKubernetesPagesWithLimitAndContinue(t *testing.T) {
	kb, k, _ := newKubernetes(t, KubernetesOptions{})
	for _, name := range []string{"ledger", "invoices", "refunds", "payouts"} {
		err := k.AddWorkload(Workload{
			Kind: KindDeployment, Namespace: "payments", Name: name, Replicas: 1,
			Containers: []Container{{Name: name, Image: "ghcr.io/acme/" + name + ":1"}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	const path = "/apis/apps/v1/namespaces/payments/deployments"
	want := []string{"payments/invoices", "payments/ledger", "payments/payments-api", "payments/payouts", "payments/refunds"}

	var got []string
	var sizes []int
	next := ""
	for {
		q := "?limit=2"
		if next != "" {
			q += "&continue=" + url.QueryEscape(next)
		}
		page := kubeGet(t, kb, path+q)
		names := kubeNames(t, page)
		got = append(got, names...)
		sizes = append(sizes, len(names))
		next, _ = field(page, "metadata", "continue").(string)
		remaining := field(page, "metadata", "remainingItemCount")
		if next == "" {
			if remaining != nil {
				t.Errorf("last page has remainingItemCount %v", remaining)
			}
			break
		}
		if wantRemaining := float64(len(want) - len(got)); remaining != wantRemaining {
			t.Errorf("after %d items: remainingItemCount %v, want %v", len(got), remaining, wantRemaining)
		}
	}
	if !slices.Equal(got, want) || !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("got %v in pages of %v, want %v in pages of [2 2 1]", got, sizes, want)
	}

	t.Run("a change between pages moves nothing already served", func(t *testing.T) {
		first := kubeGet(t, kb, path+"?limit=2")
		tok, _ := field(first, "metadata", "continue").(string)
		// Something sorting before the items served arrives, which would
		// push ledger onto the second page if the token counted items, and
		// an item not yet served is deleted, which must not come back.
		if err := k.AddWorkload(Workload{Kind: KindDeployment, Namespace: "payments", Name: "audit", Replicas: 1, Containers: []Container{{Name: "a", Image: "x:1"}}}); err != nil {
			t.Fatal(err)
		}
		if err := k.DeleteWorkload(Ref{KindDeployment, "payments", "payouts"}); err != nil {
			t.Fatal(err)
		}
		rest := kubeNames(t, kubeGet(t, kb, path+"?limit=10&continue="+url.QueryEscape(tok)))
		if want := []string{"payments/payments-api", "payments/refunds"}; !slices.Equal(rest, want) {
			t.Errorf("second page: got %v, want %v", rest, want)
		}
	})

	t.Run("lists across namespaces", func(t *testing.T) {
		var all []string
		next := ""
		for range 10 {
			q := "?limit=3"
			if next != "" {
				q += "&continue=" + url.QueryEscape(next)
			}
			page := kubeGet(t, kb, "/apis/apps/v1/deployments"+q)
			all = append(all, kubeNames(t, page)...)
			if next, _ = field(page, "metadata", "continue").(string); next == "" {
				break
			}
		}
		if len(all) != 7 || !slices.IsSorted(all) {
			t.Errorf("got %v, want 7 deployments in order", all)
		}
	})

	t.Run("a bad limit or token is a 400", func(t *testing.T) {
		first := kubeGet(t, kb, path+"?limit=1")
		tok, _ := field(first, "metadata", "continue").(string)
		for _, q := range []string{"?limit=abc", "?limit=-1", "?continue=not-a-token", "?continue=" + base64.RawURLEncoding.EncodeToString([]byte(`{"p":"/elsewhere","s":"x"}`))} {
			if status, body := kubeDo(t, kb, http.MethodGet, path+q, kubeToken); status != http.StatusBadRequest {
				t.Errorf("%s: got %d, want 400: %s", q, status, body)
			}
		}
		// A token made for one list can't continue another.
		if status, _ := kubeDo(t, kb, http.MethodGet, "/apis/apps/v1/namespaces/billing/deployments?continue="+url.QueryEscape(tok), kubeToken); status != http.StatusBadRequest {
			t.Errorf("a token from another list: got %d, want 400", status)
		}
	})
}

func TestKubernetesExpiredContinueAnswers410(t *testing.T) {
	kb, k, _ := newKubernetes(t, KubernetesOptions{ExpireContinue: true})
	if err := k.AddWorkload(Workload{Kind: KindDeployment, Namespace: "payments", Name: "ledger", Replicas: 1, Containers: []Container{{Name: "l", Image: "x:1"}}}); err != nil {
		t.Fatal(err)
	}
	const path = "/apis/apps/v1/namespaces/payments/deployments"
	// The first page has no token to expire.
	page := kubeGet(t, kb, path+"?limit=1")
	tok, _ := field(page, "metadata", "continue").(string)
	if tok == "" {
		t.Fatal("first page has no continue token")
	}
	status, body := kubeDo(t, kb, http.MethodGet, path+"?limit=1&continue="+url.QueryEscape(tok), kubeToken)
	if status != http.StatusGone {
		t.Fatalf("got %d, want 410: %s", status, body)
	}
	st := decode[map[string]any](t, body)
	if st["kind"] != "Status" || st["reason"] != "Expired" || st["code"] != float64(410) {
		t.Errorf("got %v, want an Expired Status", st)
	}
	if msg, _ := st["message"].(string); !strings.Contains(msg, "continue parameter is too old") {
		t.Errorf("message %q", msg)
	}
	// As on a real API server, the reply carries a token that resumes the
	// list, inconsistently; an adapter must not take it.
	if resume, _ := at(st, "metadata", "continue").(string); resume == "" {
		t.Error("the 410 carries no continue token")
	}

	kb.ExpireContinue(false)
	if rest := kubeNames(t, kubeGet(t, kb, path+"?limit=1&continue="+url.QueryEscape(tok))); !slices.Equal(rest, []string{"payments/payments-api"}) {
		t.Errorf("after turning it off: got %v", rest)
	}
}

// TestKubernetesStoryLandsAtItsTimes plays the story step by step and
// checks that each change is invisible before its time, served after it,
// and stamped with the clock's time in the object's managedFields.
func TestKubernetesStoryLandsAtItsTimes(t *testing.T) {
	kb, k, c := newKubernetes(t, KubernetesOptions{})
	payments := Ref{KindDeployment, "payments", "payments-api"}
	core := Ref{KindStatefulSet, "billing", "billing-core"}
	legacy := Ref{KindDeployment, "billing", "legacy-billing"}

	image := func() string {
		return fmt.Sprint(at(kubeWorkload(t, kb, payments), "spec", "template", "spec", "containers", 0, "image"))
	}
	annotation := func() string {
		return strMap(field(kubeWorkload(t, kb, payments), "metadata", "annotations"))["github.com/project-slug"]
	}
	replicas := func() any { return field(kubeWorkload(t, kb, payments), "spec", "replicas") }
	teamLabel := func() (string, bool) {
		v, ok := strMap(field(kubeWorkload(t, kb, core), "metadata", "labels"))["team"]
		return v, ok
	}
	modified := func(ref Ref) string {
		return fmt.Sprint(at(kubeWorkload(t, kb, ref), "metadata", "managedFields", 0, "time"))
	}

	// Before the story: the first sync's state.
	if image() != "ghcr.io/acme/payments-api:1.4.1" || replicas() != float64(2) || annotation() != "acme/payments-api" {
		t.Fatalf("initial payments-api: image %s, replicas %v, annotation %s", image(), replicas(), annotation())
	}
	if v, ok := teamLabel(); !ok || v != "billing" {
		t.Fatalf("initial billing-core team label: %q, %v", v, ok)
	}
	if kubeWorkload(t, kb, legacy) == nil {
		t.Fatal("legacy-billing is not served at the start")
	}
	genBefore, _ := at(kubeWorkload(t, kb, payments), "metadata", "generation").(float64)

	steps := KubernetesStory()
	if !slices.IsSortedFunc(steps, func(a, b ClusterStep) int { return a.At.Compare(b.At) }) {
		t.Error("the story is not in time order")
	}
	if steps[0].At.Before(Start) {
		t.Errorf("the story starts at %v, before the org's first sync at %v", steps[0].At, Start)
	}
	if !ImageBumpedAt.Equal(time.Date(2026, 10, 6, 14, 10, 0, 0, time.UTC)) || ImageBumpedAt.Weekday() != time.Tuesday {
		t.Errorf("the image bump is at %v (%s), want Tuesday 2026-10-06T14:10:00Z", ImageBumpedAt, ImageBumpedAt.Weekday())
	}

	step := func(at time.Time) {
		t.Helper()
		for _, s := range steps {
			if s.At.Equal(at) {
				c.Set(at)
				if s.Apply != nil {
					if err := s.Apply(k); err != nil {
						t.Fatalf("%s: %v", s.Name, err)
					}
				}
				return
			}
		}
		t.Fatalf("no step at %v", at)
	}

	// Setting the clock changes nothing by itself.
	c.Set(ReplicasScaledAt.Add(-time.Minute))
	if replicas() != float64(2) {
		t.Errorf("replicas before the change: %v, want 2", replicas())
	}
	step(ReplicasScaledAt)
	if replicas() != float64(3) || image() != "ghcr.io/acme/payments-api:1.4.1" {
		t.Errorf("after the scale: replicas %v image %s", replicas(), image())
	}
	if got, want := modified(payments), kubeTime(ReplicasScaledAt); got != want {
		t.Errorf("payments-api modified at %s, want %s", got, want)
	}

	step(ImageBumpedAt)
	if image() != "ghcr.io/acme/payments-api:1.4.2" {
		t.Errorf("after the bump: image %s", image())
	}
	if rev := strMap(field(kubeWorkload(t, kb, payments), "metadata", "annotations"))["deployment.kubernetes.io/revision"]; rev != "7" {
		t.Errorf("revision after the bump: %s, want 7", rev)
	}
	if got, want := modified(payments), kubeTime(ImageBumpedAt); got != want {
		t.Errorf("payments-api modified at %s, want %s", got, want)
	}
	if after, _ := at(kubeWorkload(t, kb, payments), "metadata", "generation").(float64); after != genBefore+2 {
		t.Errorf("generation %v after two spec changes, want %v", after, genBefore+2)
	}

	step(TeamLabelClearedAt)
	// A cleared label stays on the object with an empty value: the adapter
	// must treat that as absent.
	if v, ok := teamLabel(); !ok || v != "" {
		t.Errorf("billing-core team label after clearing: %q, present %v, want an empty value", v, ok)
	}
	if got, want := modified(core), kubeTime(TeamLabelClearedAt); got != want {
		t.Errorf("billing-core modified at %s, want %s", got, want)
	}

	step(AnnotationEditedAt)
	if annotation() != "TODO" {
		t.Errorf("annotation after the edit: %q, want TODO", annotation())
	}
	if rev := strMap(field(kubeWorkload(t, kb, payments), "metadata", "annotations"))["deployment.kubernetes.io/revision"]; rev != "7" {
		t.Errorf("an annotation edit moved the revision to %s", rev)
	}

	if kubeWorkload(t, kb, legacy) == nil {
		t.Fatal("legacy-billing is gone before it is deleted")
	}
	step(WorkloadDeletedAt)
	if kubeWorkload(t, kb, legacy) != nil {
		t.Error("legacy-billing is still served after it is deleted")
	}
	if got := kubeNames(t, kubeGet(t, kb, "/apis/apps/v1/namespaces/billing/deployments")); len(got) != 0 {
		t.Errorf("billing deployments after the delete: %v", got)
	}

	// The last step changes the adapter's settings, not the cluster.
	if got := ExcludedNamespaces(NamespaceExcludedAt.Add(-time.Second)); !slices.Equal(got, DefaultExcludedNamespaces) {
		t.Errorf("exclude_namespaces before: %v, want the defaults", got)
	}
	if got := ExcludedNamespaces(NamespaceExcludedAt); !slices.Equal(got, append(slices.Clone(DefaultExcludedNamespaces), "billing")) {
		t.Errorf("exclude_namespaces from the step: %v, want the defaults and billing", got)
	}
	rv := k.ResourceVersion()
	step(NamespaceExcludedAt)
	if k.ResourceVersion() != rv {
		t.Error("the exclude_namespaces step changed the cluster")
	}
	// The namespace and its workload are still on the cluster: only the
	// adapter stops reading them.
	if !slices.Contains(kubeNames(t, kubeGet(t, kb, "/api/v1/namespaces")), "billing") || kubeWorkload(t, kb, core) == nil {
		t.Error("billing or billing-core is gone from the cluster")
	}
}

// TestKubernetesServesTheCanaries checks the fake does hold the secrets the
// adapter must not copy, so a canary search over the adapter's output proves
// something, and that it refuses the places a Secret would be read from.
func TestKubernetesServesTheCanaries(t *testing.T) {
	kb, _, _ := newKubernetes(t, KubernetesOptions{})

	var served strings.Builder
	for _, path := range []string{
		"/apis/apps/v1/namespaces/payments/deployments",
		"/apis/apps/v1/namespaces/billing/statefulsets",
	} {
		status, body := kubeDo(t, kb, http.MethodGet, path, kubeToken)
		if status != http.StatusOK {
			t.Fatalf("%s: %d", path, status)
		}
		served.Write(body)
	}
	for _, canary := range Canaries() {
		if !strings.Contains(served.String(), canary) {
			t.Errorf("canary %q is not served by any workload list", canary)
		}
	}

	core := kubeWorkload(t, kb, Ref{KindStatefulSet, "billing", "billing-core"})
	ctr := at(core, "spec", "template", "spec", "containers", 0)
	if got := at(ctr, "env", 0, "value"); got != CanaryEnvPassword {
		t.Errorf("env: got %v", got)
	}
	if got := at(ctr, "args", 0); got != "--db-password="+CanaryArgPassword {
		t.Errorf("args: got %v", got)
	}
	if got := at(ctr, "envFrom", 0, "secretRef", "name"); got != CanarySecretName {
		t.Errorf("envFrom: got %v", got)
	}

	if got := at(core, "spec", "template", "spec", "serviceAccountName"); got != CanaryServiceAccount {
		t.Errorf("serviceAccountName: got %v", got)
	}
	if got := at(core, "spec", "template", "spec", "volumes", 0, "secret", "secretName"); got != CanaryVolumeSecret {
		t.Errorf("volume: got %v", got)
	}
	if got := at(core, "metadata", "labels", "cost-center"); got != CanaryLabel {
		t.Errorf("label: got %v", got)
	}
	if got := at(core, "metadata", "annotations", "acme.example/runbook"); got != CanaryAnnotation {
		t.Errorf("annotation: got %v", got)
	}

	// A Secret and a ConfigMap are always refused, and the refusals are in
	// the request log.
	secretPath := "/api/v1/namespaces/billing/secrets/" + CanarySecretName
	configPath := "/api/v1/namespaces/billing/configmaps"
	for _, path := range []string{secretPath, configPath} {
		status, body := kubeDo(t, kb, http.MethodGet, path, kubeToken)
		if status != http.StatusForbidden {
			t.Errorf("GET %s: got %d, want 403", path, status)
		}
		for _, canary := range Canaries() {
			if bytes.Contains(body, []byte(canary)) && !strings.Contains(path, canary) {
				t.Errorf("the refusal of %s repeats canary %q", path, canary)
			}
		}
	}
	var logged []string
	for _, r := range kb.Requests() {
		logged = append(logged, r.Method+" "+r.Path)
	}
	for _, want := range []string{"GET " + secretPath, "GET " + configPath} {
		if !slices.Contains(logged, want) {
			t.Errorf("request log %v lacks %q", logged, want)
		}
	}
	if kb.Requests()[0].Header.Get("Authorization") != "Bearer "+kubeToken {
		t.Error("the request log lacks the Authorization header")
	}
}

// TestKubernetesRecordedLists keeps testdata/acme/kubernetes.ndjson equal to
// what the fake serves at the spec samples' time: the lists an adapter makes
// in one sync, namespace by namespace, excluded namespaces left out.
func TestKubernetesRecordedLists(t *testing.T) {
	kb, k, c := newKubernetes(t, KubernetesOptions{})
	playTo(t, k, c, ImageBumpedAt)

	type line struct {
		Path     string          `json:"path"`
		Status   int             `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	var buf bytes.Buffer
	record := func(path string) {
		t.Helper()
		status, body := kubeDo(t, kb, http.MethodGet, path, kubeToken)
		b, err := json.Marshal(line{Path: path, Status: status, Response: body})
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	record("/api/v1/namespaces")
	excluded := ExcludedNamespaces(ImageBumpedAt)
	for _, ns := range k.Namespaces() {
		if slices.Contains(excluded, ns.Name) {
			continue
		}
		for _, resource := range []string{"deployments", "statefulsets", "daemonsets"} {
			record("/apis/apps/v1/namespaces/" + ns.Name + "/" + resource)
		}
	}

	if *update {
		if err := os.WriteFile(kubeFeedPath, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(kubeFeedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("%s is out of date; run go test ./internal/fakes -run TestKubernetesRecordedLists -update", kubeFeedPath)
	}
}

func TestClusterMutationsRefuseWhatCannotHappen(t *testing.T) {
	k := NewCluster(nil) // the real clock
	ctr := []Container{{Name: "c", Image: "x:1"}}
	payments := Ref{KindDeployment, "payments", "payments-api"}

	if err := k.AddNamespace("payments"); err == nil {
		t.Error("a second payments namespace was created")
	}
	if err := k.AddNamespace("staging"); err != nil {
		t.Fatal(err)
	}
	for name, w := range map[string]Workload{
		"unknown kind":      {Kind: "CronJob", Namespace: "payments", Name: "x", Containers: ctr},
		"unknown namespace": {Kind: KindDeployment, Namespace: "nowhere", Name: "x", Containers: ctr},
		"duplicate":         {Kind: KindDeployment, Namespace: "payments", Name: "payments-api", Containers: ctr},
	} {
		if err := k.AddWorkload(w); err == nil {
			t.Errorf("%s: the workload was created", name)
		}
	}
	// The same name in another namespace or kind is a different workload.
	for _, w := range []Workload{
		{Kind: KindDeployment, Namespace: "staging", Name: "payments-api", Replicas: 1, Containers: ctr},
		{Kind: KindStatefulSet, Namespace: "payments", Name: "payments-api", Replicas: 1, Containers: ctr},
	} {
		if err := k.AddWorkload(w); err != nil {
			t.Errorf("%v: %v", w.Ref(), err)
		}
	}
	if err := k.SetReplicas(Ref{KindDaemonSet, "kube-system", "svclb-traefik"}, 2); err == nil {
		t.Error("a DaemonSet was given replicas")
	}
	if err := k.SetImage(Ref{KindDeployment, "payments", "nope"}, "x:2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetImage on a missing workload: %v, want ErrNotFound", err)
	}
	if err := k.DeleteWorkload(Ref{KindDeployment, "payments", "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteWorkload on a missing workload: %v, want ErrNotFound", err)
	}

	// A failed mutation changes nothing, and Workload returns copies.
	before := k.ResourceVersion()
	if err := k.SetReplicas(Ref{KindDaemonSet, "kube-system", "svclb-traefik"}, 2); err == nil {
		t.Fatal("expected an error")
	}
	if k.ResourceVersion() != before {
		t.Error("a refused mutation moved the resource version")
	}
	w, ok := k.Workload(payments)
	if !ok {
		t.Fatal("payments-api is missing")
	}
	w.Labels["team"] = "changed"
	w.Containers[0].Image = "changed"
	if again, _ := k.Workload(payments); again.Labels["team"] != "payments" || again.Containers[0].Image == "changed" {
		t.Error("changing a returned copy changed the cluster")
	}
}

func TestClusterIsSafeForConcurrentUse(t *testing.T) {
	kb, k, _ := newKubernetes(t, KubernetesOptions{})
	payments := Ref{KindDeployment, "payments", "payments-api"}
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for n := range 20 {
				if err := k.SetReplicas(payments, i+n); err != nil {
					t.Error(err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 20 {
				if status, _ := kubeDo(t, kb, http.MethodGet, "/apis/apps/v1/deployments?limit=1", kubeToken); status != http.StatusOK {
					t.Errorf("list: status %d", status)
					return
				}
				k.Workloads(KindDeployment, "")
			}
		}()
	}
	wg.Wait()
}

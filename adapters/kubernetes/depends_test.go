package kubernetes

import (
	"encoding/json"
	"slices"
	"testing"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/fakes"
)

func TestParseReferenceReadsServiceNames(t *testing.T) {
	tests := []struct {
		name, value string
		want        reference
		ok          bool
	}{
		{"svc.ns", "ledger.billing", reference{Service: "ledger", Namespace: "billing"}, true},
		{"svc.ns with port", "ledger.billing:8080", reference{Service: "ledger", Namespace: "billing", Port: 8080}, true},
		{"svc.ns.svc", "ledger.billing.svc:80", reference{Service: "ledger", Namespace: "billing", Port: 80}, true},
		{"full name, trailing dot", "ledger.billing.svc.cluster.local.", reference{Service: "ledger", Namespace: "billing"}, true},
		{"url", "http://ledger.billing.svc.cluster.local:8080/v1/x?y=z#f", reference{Service: "ledger", Namespace: "billing", Port: 8080, Scheme: "http"}, true},
		{"scheme is lower-cased", "HTTPS://Ledger.Billing", reference{Service: "ledger", Namespace: "billing", Scheme: "https"}, true},
		{"bare name in a url takes the workload's namespace", "postgres://db:5432/app", reference{Service: "db", Namespace: "own", Port: 5432, Scheme: "postgres"}, true},
		{"userinfo is dropped", "http://u:p@ledger:8080", reference{Service: "ledger", Namespace: "own", Port: 8080, Scheme: "http"}, true},
		{"bare name without a scheme", "ledger", reference{}, false},
		{"bare name and port without a scheme", "ledger:8080", reference{}, false},
		{"external host", "api.github.com", reference{}, false},
		{"four labels", "a.b.svc.local", reference{}, false},
		{"ip address", "10.43.0.12", reference{}, false},
		{"ip in a url", "http://10.43.0.12:80", reference{}, false},
		{"bad port", "ledger.billing:http", reference{}, false},
		{"port out of range", "ledger.billing:70000", reference{}, false},
		{"ipv6", "http://[::1]:80", reference{}, false},
		{"not dns", "Not A Host.billing", reference{}, false},
		{"password", "s3cr3t-pa$$word", reference{}, false},
		{"scheme with junk", "ht tp://ledger", reference{}, false},
		{"empty", "  ", reference{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseReference(tt.value, "own")
			if ok != tt.ok || got != tt.want {
				t.Fatalf("got %+v, %v, want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestInjectedVariablesAreRecognized(t *testing.T) {
	for name, want := range map[string]bool{
		"LEDGER_SERVICE_HOST": true, "LEDGER_SERVICE_PORT": true, "LEDGER_PORT_8080_TCP_ADDR": true,
		"LEDGER_URL": false, "LEDGER_PORT": false, "PORT_FORWARD": false,
	} {
		if got := injected(name); got != want {
			t.Errorf("injected(%q) = %v, want %v", name, got, want)
		}
	}
}

func dependsOn(d *modelv1alpha1.ObservationData) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, r := range d.GetRelations() {
		if r.GetType() != "depends_on" {
			continue
		}
		q := map[string]any{"ppm": r.GetConfidencePpm()}
		for k, v := range r.GetAttributes() {
			q[k] = v.AsInterface()
		}
		out[r.GetTo()] = q
	}
	return out
}

const (
	billingAPI = "kubernetes:workload/k3d-acme/billing/deployment/billing-api"
	ledger     = "kubernetes:workload/k3d-acme/billing/deployment/ledger"
	postgres   = "kubernetes:workload/k3d-acme/billing/statefulset/postgres"
)

// TestSyncEmitsWhatAWorkloadDependsOn: billing-api reaches the ledger by a
// bare name in an http URL and postgres by its full cluster name, and its
// injected-style and credential variables add nothing.
func TestSyncEmitsWhatAWorkloadDependsOn(t *testing.T) {
	r := newRig(t)
	r.playUntil(fakes.ImageBumpedAt)
	obs, _ := r.sync()
	got := byKey(obs)
	want := map[string]map[string]any{
		ledger:   {"ppm": uint32(900000), "protocol": "http", "port": float64(8080), "service": "billing/ledger", "via": "env:LEDGER_URL"},
		postgres: {"ppm": uint32(900000), "protocol": "postgres", "port": float64(5432), "service": "billing/postgres", "via": "env:DATABASE_URL"},
	}
	deps := dependsOn(got[billingAPI])
	if len(deps) != len(want) {
		t.Fatalf("billing-api depends on %v, want %v", deps, want)
	}
	for to, q := range want {
		b1, _ := json.Marshal(deps[to])
		b2, _ := json.Marshal(q)
		if string(b1) != string(b2) {
			t.Errorf("billing-api -> %s: got %s, want %s", to, b1, b2)
		}
	}
	for key, d := range got {
		if d.GetEntity().GetKind() != "Component" {
			continue
		}
		if s := d.GetSnapshots(); len(s) != 1 || !slices.Contains(s[0].GetPredicates(), "depends_on") {
			t.Errorf("%s snapshots %v, want depends_on covered so a removed reference closes", key, s)
		}
		if key != billingAPI && len(dependsOn(d)) != 0 {
			t.Errorf("%s depends on %v, want nothing", key, dependsOn(d))
		}
	}
}

// TestSyncEndsADependencyTheClusterEnded: after billing-api drops LEDGER_URL
// its observation keeps only postgres, with depends_on still in the snapshot.
func TestSyncEndsADependencyTheClusterEnded(t *testing.T) {
	r := newRig(t)
	r.playUntil(fakes.DependencyRemovedAt)
	obs, _ := r.sync()
	api := byKey(obs)[billingAPI]
	if deps := dependsOn(api); len(deps) != 1 || deps[postgres] == nil {
		t.Fatalf("billing-api depends on %v, want only postgres", deps)
	}
	if s := api.GetSnapshots(); len(s) != 1 || !slices.Contains(s[0].GetPredicates(), "depends_on") {
		t.Errorf("snapshots %v, want depends_on covered", s)
	}
}

// TestSyncSkipsReferencesThatResolveNowhere: ExternalName and selectorless
// Services, a Service no workload matches, and a Service in an excluded
// namespace add no edge; a reference across namespaces and several to one
// target give one relation with the first one's qualifiers.
func TestSyncSkipsReferencesThatResolveNowhere(t *testing.T) {
	r := newRig(t)
	k := r.cluster
	ports := []fakes.ServicePort{{Name: "web", Port: 9000}}
	for _, sv := range []fakes.Service{
		{Namespace: "billing", Name: "ext", Type: "ExternalName", Ports: ports},
		{Namespace: "billing", Name: "headless", Ports: ports},
		{Namespace: "billing", Name: "orphan", Selector: map[string]string{"app": "nobody"}, Ports: ports},
		{Namespace: "kube-system", Name: "dns", Selector: map[string]string{"app": "coredns"}, Ports: ports},
	} {
		if err := k.AddService(sv); err != nil {
			t.Fatal(err)
		}
	}
	err := k.AddWorkload(fakes.Workload{
		Kind: fakes.KindDeployment, Namespace: "payments", Name: "caller", Replicas: 1,
		Containers: []fakes.Container{{Name: "c", Image: "x:1", Env: map[string]string{
			"A_EXT": "ext.billing", "B_HEADLESS": "headless.billing", "C_ORPHAN": "orphan.billing",
			"D_DNS": "dns.kube-system", "E_LEDGER": "ledger.billing:8080", "F_LEDGER": "https://ledger.billing:8080",
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	obs, _ := r.sync()
	deps := dependsOn(byKey(obs)["kubernetes:workload/k3d-acme/payments/deployment/caller"])
	if len(deps) != 1 || deps[ledger] == nil {
		t.Fatalf("caller depends on %v, want only the ledger", deps)
	}
	if q := deps[ledger]; q["via"] != "env:E_LEDGER" || q["protocol"] != "http" || q["port"] != float64(8080) {
		t.Errorf("qualifiers %v, want the first reference's: via E_LEDGER, protocol from the port's name", q)
	}
}

func TestServiceInfoResolvesPortAndProtocol(t *testing.T) {
	sv := serviceInfo{Ports: []servicePort{{Name: "metrics", Port: 9090}, {Name: "web", Port: 80, AppProtocol: "h2c"}}}
	tests := []struct {
		name     string
		ref      reference
		wantPort int
		wantProt string
	}{
		{"given port, appProtocol wins over name", reference{Port: 80}, 80, "h2c"},
		{"given port, name", reference{Port: 9090}, 9090, "metrics"},
		{"scheme default the service exposes", reference{Scheme: "http"}, 80, "http"},
		{"no port, no scheme: first port", reference{}, 9090, "metrics"},
		{"port the service lacks", reference{Port: 1234}, 1234, "tcp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if port, prot := sv.resolve(tt.ref); port != tt.wantPort || prot != tt.wantProt {
				t.Fatalf("got %d %q, want %d %q", port, prot, tt.wantPort, tt.wantProt)
			}
		})
	}
}

// TestValueFromIsNeverRead: a variable that takes its value from a ConfigMap
// has no literal value, so a ConfigMap name or key that reads like a Service
// adds no edge; the literal variable beside it shows the workload was read.
func TestValueFromIsNeverRead(t *testing.T) {
	r := newRig(t)
	err := r.cluster.AddWorkload(fakes.Workload{
		Kind: fakes.KindDeployment, Namespace: "payments", Name: "caller", Replicas: 1,
		Containers: []fakes.Container{{
			Name: "c", Image: "x:1",
			Env: map[string]string{"DB_URL": "postgres://postgres.billing:5432/app"},
			EnvFromConfigMaps: map[string]fakes.ConfigMapKey{
				"LEDGER_URL": {Name: "ledger.billing", Key: "http://ledger.billing:8080"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	obs, _ := r.sync()
	deps := dependsOn(byKey(obs)["kubernetes:workload/k3d-acme/payments/deployment/caller"])
	if len(deps) != 1 || deps[postgres] == nil {
		t.Fatalf("caller depends on %v, want only postgres, from its literal variable", deps)
	}
}

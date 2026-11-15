package e2e

import (
	"fmt"
	"math"
	"slices"
	"testing"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/vocab"
)

// The coverage trail's predicates, as the joiner reads them. defined_in is
// the core term: the Kubernetes adapter's repository reads as it through
// the exact_match the operator accepted.
var (
	watchesPred   = vocab.Full(fakes.DatadogNamespace, "watches")
	pagesPred     = vocab.Full(fakes.DatadogNamespace, "pages")
	describesPred = vocab.Full(fakes.TerraformNamespace, "describes")
	coveredBy     = vocab.Full(fakes.OperatorNamespace, "covered_by")
	definedIn     = string(model.RelDefinedIn)
	coverageTrail = vocab.Trail{Watches: watchesPred, Pages: pagesPred, Describes: describesPred, DefinedIn: definedIn}
)

const coverageRule = "trail-coverage@1"

// Templates over the coverage observations. The Datadog tags name the
// workload's cluster, namespace and deployment, so the monitor watches the
// workload; the repository comes from the workload's own defined_in.
const (
	workloadTagKey = `{{ $t := toJson .tags }}{{ with regexFind "kube_deployment:[a-z0-9-]+" $t | trimPrefix "kube_deployment:" }}kubernetes:workload/{{ regexFind "kube_cluster_name:[a-z0-9-]+" $t | trimPrefix "kube_cluster_name:" }}/{{ regexFind "kube_namespace:[a-z0-9-]+" $t | trimPrefix "kube_namespace:" }}/deployment/{{ . }}{{ end }}`
	handleKey      = `{{ with regexFind "@pagerduty-[A-Za-z0-9_-]+" .message | trimPrefix "@pagerduty-" | lower }}pagerduty:service/{{ . }}{{ end }}`
	monitorKey     = `datadog:monitor/{{ .monitor_id }}`
	repoKey        = `{{ with regexFind "github\\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+" .module_source | trimPrefix "github.com/" }}github:repo/{{ . }}{{ end }}`
)

// guess is a proposing derives rule, in force.
func guess(id, subject, object, kind, key string) vocab.Mapping {
	return vocab.Mapping{
		ID: id, Version: 1, Subject: subject, Predicate: vocab.Derives, Object: object,
		Status: vocab.Asserted, Guess: true, Target: &vocab.Target{Kind: kind, Key: key},
	}
}

var (
	monitorKind  = vocab.Full(fakes.DatadogNamespace, "Monitor")
	resourceKind = vocab.Full(fakes.TerraformNamespace, "Resource")
	serviceKind  = vocab.Full(fakes.PagerDutyNamespace, "Service")
	deployKind   = "io.k8s:Deployment"
	repoKind     = vocab.Full(vocab.CoreNamespace, "Repository")

	datadogRules = []vocab.Mapping{
		guess("r-dd-watches", monitorKind, watchesPred, deployKind, workloadTagKey),
		guess("r-dd-pages", monitorKind, pagesPred, serviceKind, handleKey),
	}
	terraformConfigRules = []vocab.Mapping{
		guess("r-tf-describes", resourceKind, describesPred, monitorKind, monitorKey),
		guess("r-tf-watches", resourceKind, watchesPred, deployKind, workloadTagKey),
		guess("r-tf-pages", resourceKind, pagesPred, serviceKind, handleKey),
	}
	terraformRepoRule = guess("r-tf-repo", resourceKind, definedIn, repoKind, repoKey)

	// The operator accepts the workload kinds as components (so the
	// repository's domain fits defined_in's) and the Kubernetes adapter's
	// suggested exact_match.
	workloadMappings = []vocab.Mapping{
		{ID: "m-deploy-component", Subject: deployKind, Predicate: vocab.Broader, Object: vocab.CoreNamespace + ":Component"},
		{ID: "m-daemonset-component", Subject: "io.k8s:DaemonSet", Predicate: vocab.Broader, Object: vocab.CoreNamespace + ":Component"},
		{ID: "m-statefulset-component", Subject: "io.k8s:StatefulSet", Predicate: vocab.Broader, Object: vocab.CoreNamespace + ":Component"},
	}
)

// coverageWorld is Acme's cluster in namespaced mode, synced, with the
// coverage adapters' modules and the operator's covered_by booted, and
// PagerDuty's checkout service ingested.
type coverageWorld struct {
	*vocabWorld
	workloadObs string // the Kubernetes observation of payments-api's workload
}

func newCoverageWorld(t *testing.T) *coverageWorld {
	t.Helper()
	v := newVocabWorld(t, nil)
	v.Modules = append(v.Modules, fakes.CoverageModules()...)
	v.Modules = append(v.Modules, fakes.CoverageOperator())
	v.Sources = map[string]string{
		fakes.DatadogNamespace: fakes.DatadogSource, fakes.TerraformNamespace: fakes.TerraformSource, fakes.PagerDutyNamespace: fakes.PagerDutySource,
	}
	v.Settings = workloadMappings
	v.boot() // stores the adapters' suggestions as proposed
	// Accepting the repository suggestion is the operator's call.
	if err := v.store.DecideMapping(t.Context(), "k8s-repository-is-defined-in", vocab.Asserted, "settings", "Acme names the source repo on every workload", v.clock.Now()); err != nil {
		t.Fatal(err)
	}
	v.boot()
	w := &coverageWorld{vocabWorld: v}
	for _, o := range v.sync() {
		if o.GetData().GetEntity().GetKey() == paymentsWorkload {
			w.workloadObs = o.GetId()
		}
	}
	if w.workloadObs == "" {
		t.Fatalf("sync has no observation of %s", paymentsWorkload)
	}
	w.ingest(fakes.PagerDutySource, fakes.PagerDutyService(v.clock.Now()))
	return w
}

func (w *coverageWorld) ingest(source string, obs ...*eventv1alpha1.Observation) {
	w.t.Helper()
	for _, o := range obs {
		if err := model.ValidateObservation(w.reg, o); err != nil {
			w.t.Fatalf("%s: %v", o.GetId(), err)
		}
	}
	if _, err := w.store.Ingest(w.t.Context(), graph.IngestRequest{Source: source, Observations: obs}); err != nil {
		w.t.Fatal(err)
	}
}

// rules puts more derives rules in force (a boot, as settings would be
// edited and the server restarted), then ingests o, whose rules run inside
// Ingest.
func (w *coverageWorld) rules(source string, o *eventv1alpha1.Observation, more ...vocab.Mapping) {
	w.t.Helper()
	w.Settings = append(w.Settings, more...)
	w.boot()
	w.ingest(source, o)
}

func (w *coverageWorld) join() ([]vocab.Coverage, []vocab.Break) {
	w.t.Helper()
	edges, err := w.store.TrailEdges(w.t.Context(), []string{watchesPred, pagesPred, describesPred, definedIn})
	if err != nil {
		w.t.Fatal(err)
	}
	return coverageTrail.Join(edges)
}

// cites lists a claim's evidence as "<predicate> <observation> <rule>",
// describes and workload hops included, sorted.
func cites(c vocab.Claim) []string {
	var out []string
	for _, e := range slices.Concat(c.Facts, c.Via, c.Hop) {
		for _, ct := range e.Cites {
			out = append(out, fmt.Sprintf("%s %s %s", e.Predicate, ct.Observation, ct.Rule))
		}
	}
	slices.Sort(out)
	return out
}

// TestVocabCoverageTrail joins a Datadog monitor's proposed steps into
// "payments-api covered_by checkout", scored per claim: the monitor watches
// the workload, which the Kubernetes adapter says is defined in the
// payments repo; Terraform's copy of the monitor's configuration backs the
// same steps and leaves the score as it is; Terraform's own reference to the
// repo is a second path and raises it. A stale handle breaks the trail, and
// the break names its step and observations.
func TestVocabCoverageTrail(t *testing.T) {
	t.Parallel()

	t.Run("covered", func(t *testing.T) {
		t.Parallel()
		w := newCoverageWorld(t)
		now := w.clock.Now()
		dd, tf := fakes.DatadogMonitor(now, fakes.CheckoutHandle), fakes.TerraformMonitor(now, fakes.CheckoutHandle)
		ddID, tfID := dd.GetId(), tf.GetId()

		w.rules(fakes.DatadogSource, dd, datadogRules...)
		datadogOnly := w.proposeOne(0.25)

		w.rules(fakes.TerraformSource, tf, terraformConfigRules...)
		withConfig := w.proposeOne(0.25)

		w.rules(fakes.TerraformSource, tf, terraformRepoRule)
		c := w.proposeOne(0.5 * 2.0 / 3)
		t.Logf("scores: datadog %.4f, +terraform config %.4f, +repo path %.4f", datadogOnly.Score, withConfig.Score, c.Score)

		if c.ServiceKey != "github:repo/acme/payments-api" || c.MonitorKey != fakes.MonitorKey || c.PagerKey != fakes.CheckoutKey {
			t.Fatalf("trail = %s -> %s -> %s, want payments-api -> %s -> %s", c.ServiceKey, c.MonitorKey, c.PagerKey, fakes.MonitorKey, fakes.CheckoutKey)
		}
		// The evidence path: each step's observations and rules. The workload's
		// defined_in is the Kubernetes adapter's, stated, so it names no rule.
		want := map[string][]string{
			"reach " + watchesPred: {
				describesPred + " " + tfID + " r-tf-describes@1",
				watchesPred + " " + ddID + " r-dd-watches@1",
				watchesPred + " " + tfID + " r-tf-watches@1",
				definedIn + " " + w.workloadObs + " ",
			},
			"reach " + definedIn: {
				describesPred + " " + tfID + " r-tf-describes@1",
				definedIn + " " + tfID + " r-tf-repo@1",
			},
			"page " + pagesPred: {
				describesPred + " " + tfID + " r-tf-describes@1",
				pagesPred + " " + ddID + " r-dd-pages@1",
				pagesPred + " " + tfID + " r-tf-pages@1",
			},
		}
		got := map[string][]string{}
		for _, cl := range c.Reach {
			got["reach "+cl.Predicate] = cites(cl)
		}
		for _, cl := range c.Page {
			got["page "+cl.Predicate] = cites(cl)
		}
		for k, wv := range want {
			if !slices.Equal(got[k], slices.Sorted(slices.Values(wv))) {
				t.Errorf("%s evidence = %q, want %q", k, got[k], wv)
			}
		}
		if len(got) != len(want) {
			t.Errorf("steps = %v, want %d", got, len(want))
		}
		// The hop is workload -> repo, and the workload is payments-api's.
		for _, cl := range c.Reach {
			if cl.Predicate == watchesPred {
				if len(cl.Hop) != 1 || cl.Hop[0].SubjectKey != paymentsWorkload || cl.Hop[0].ObjectKey != c.ServiceKey || cl.Facts[0].ObjectKey != paymentsWorkload {
					t.Errorf("watches claim = %+v, want monitor -> %s -> %s", cl, paymentsWorkload, c.ServiceKey)
				}
			}
		}

		// One proposal, kept across the three joins, citing the monitor's
		// observations and the workload's.
		fs, err := w.store.Facts(t.Context(), graph.FactFilter{Predicate: coveredBy, WithProposed: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(fs) != 1 || fs[0].Status != graph.Proposed || math.Abs(fs[0].Confidence-c.Score) > 1e-9 || fs[0].ID != datadogOnly.fact {
			t.Fatalf("covered_by facts = %+v, want one proposed fact %s at %.4f", fs, datadogOnly.fact, c.Score)
		}
		var obs []string
		for _, s := range fs[0].Sources {
			obs = append(obs, s.ObservationID)
		}
		slices.Sort(obs)
		if wantObs := []string{ddID, tfID, w.workloadObs}; !slices.Equal(obs, slices.Sorted(slices.Values(wantObs))) {
			t.Errorf("proposal sources = %v, want %v", obs, wantObs)
		}
		q := fs[0].Qualifiers
		if path, _ := q["path"].([]any); q["monitor"] != fakes.MonitorKey || q["rule"] != coverageRule || len(path) != len(c.Facts()) {
			t.Errorf("qualifiers = %v, want monitor, rule and the %d-fact path", q, len(c.Facts()))
		}
		// A deciding answer reads only asserted facts.
		if fs, err := w.store.Facts(t.Context(), graph.FactFilter{Predicate: coveredBy}); err != nil || len(fs) != 0 {
			t.Errorf("asserted covered_by = %v, %v; want none", fs, err)
		}
	})

	t.Run("stale handle", func(t *testing.T) {
		t.Parallel()
		w := newCoverageWorld(t)
		now := w.clock.Now()
		dd, tf := fakes.DatadogMonitor(now, fakes.StaleHandle), fakes.TerraformMonitor(now, fakes.StaleHandle)
		w.Settings = append(w.Settings, datadogRules...)
		w.Settings = append(w.Settings, terraformConfigRules...)
		w.Settings = append(w.Settings, terraformRepoRule)
		w.boot()
		w.ingest(fakes.DatadogSource, dd)
		w.ingest(fakes.TerraformSource, tf)

		cs, breaks := w.join()
		if len(cs) != 0 {
			t.Fatalf("coverage = %+v, want none", cs)
		}
		if len(breaks) != 1 {
			t.Fatalf("breaks = %+v, want one", breaks)
		}
		b := breaks[0]
		if b.Step != pagesPred || b.MonitorKey != fakes.MonitorKey || b.Claim.Facts[0].ObjectKey != "pagerduty:service/checkout-old" {
			t.Errorf("break = %s %s -> %s, want %s %s -> pagerduty:service/checkout-old", b.MonitorKey, b.Step, b.Claim.Facts[0].ObjectKey, fakes.MonitorKey, pagesPred)
		}
		wantCites := []string{
			describesPred + " " + tf.GetId() + " r-tf-describes@1",
			pagesPred + " " + dd.GetId() + " r-dd-pages@1",
			pagesPred + " " + tf.GetId() + " r-tf-pages@1",
		}
		if got := cites(b.Claim); !slices.Equal(got, slices.Sorted(slices.Values(wantCites))) {
			t.Errorf("break evidence = %q, want %q", got, wantCites)
		}
	})
}

// proposed is a joined trail and the fact it was stored as.
type proposed struct {
	vocab.Coverage
	fact string
}

// proposeOne joins, wants exactly one trail scored score, stores it, and
// returns it with its fact's ID.
func (w *coverageWorld) proposeOne(score float64) proposed {
	w.t.Helper()
	cs, breaks := w.join()
	if len(cs) != 1 || len(breaks) != 0 {
		w.t.Fatalf("join = %+v, breaks %+v; want one trail, no break", cs, breaks)
	}
	if math.Abs(cs[0].Score-score) > 1e-9 {
		w.t.Errorf("score = %.4f (features %v), want %.4f", cs[0].Score, cs[0].Features, score)
	}
	if err := w.store.ProposeCoverage(w.t.Context(), coveredBy, coverageRule, cs); err != nil {
		w.t.Fatal(err)
	}
	fs, err := w.store.Facts(w.t.Context(), graph.FactFilter{Predicate: coveredBy, WithProposed: true})
	if err != nil || len(fs) != 1 {
		w.t.Fatalf("covered_by facts = %v, %v; want one", fs, err)
	}
	return proposed{cs[0], fs[0].ID}
}

package resolver

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// kubernetesConfig adds the Kubernetes adapter to the GitHub one. Its source
// links the github namespace, because its defined_in claims name repositories
// there (docs/spec/data-model.md, "Declarations").
func kubernetesConfig(t testing.TB) Config {
	t.Helper()
	return Config{
		Declarations: []*modelv1alpha1.AdapterDeclaration{readDeclaration(t, "github"), readDeclaration(t, "kubernetes")},
		Sources: map[string]*Source{
			"github-acme":     {Name: "github-acme", Adapter: "github"},
			"kubernetes-acme": {Name: "kubernetes-acme", Adapter: "kubernetes", Links: []Namespace{{Name: "github", IssuerType: "github"}}},
		},
	}
}

func applyClean(e *env, source string, o *eventv1alpha1.Observation) {
	e.t.Helper()
	if got := e.apply(event(source, o)); len(got.Rejections) != 0 {
		e.t.Fatalf("%s: got rejections %v, want none", o.GetId(), got.Rejections)
	}
}

// The spec's Kubernetes sample (fixtures 9 to 13): a workload names its
// repository by annotation and its team by label, and the claims resolve to
// the GitHub repository and the namespace's Environment.
func TestKubernetesWorkloadResolvesToItsRepositoryEnvironmentAndTeam(t *testing.T) {
	e := newEnvWith(t, kubernetesConfig(t))
	applyClean(e, "github-acme", fixture(t, "1-repository-codeowners.json"))
	for _, name := range []string{"12-kubernetes-namespace.json", "13-kubernetes-team-label.json", "9-kubernetes-workload-annotation.json"} {
		applyClean(e, "kubernetes-acme", fixture(t, name))
	}
	w := "kubernetes:workload/k3d-acme/payments/deployment/payments-api(Component)"
	const one = "kubernetes-acme:1000000"
	got := factsAt(t, e, ts("2026-10-07T00:00:00Z"))
	for _, want := range []string{
		w + ` exists -> {"type":"VALUE_TYPE_BOOL","value":true} ASSERTED/NONE 1000000 [` + one + `]`,
		w + ` kubernetes.workload_name -> {"type":"VALUE_TYPE_STRING","value":"payments-api"} ASSERTED/NONE 1000000 [` + one + `]`,
		w + ` kubernetes.workload_kind -> {"type":"VALUE_TYPE_STRING","value":"Deployment"} ASSERTED/NONE 1000000 [` + one + `]`,
		w + ` kubernetes.image -> {"type":"VALUE_TYPE_STRING","value":"ghcr.io/acme/payments-api:1.4.2"} ASSERTED/NONE 1000000 [` + one + `]`,
		w + ` kubernetes.replicas -> {"type":"VALUE_TYPE_FLOAT","value":3} ASSERTED/NONE 1000000 [` + one + `]`,
		w + ` deployed_to -> kubernetes:namespace/k3d-acme/payments(Environment) ASSERTED/NONE 1000000 [` + one + `]`,
		// The annotation is strong enough to assert the repository.
		w + ` defined_in -> github:repo_node/R_kgDOH1a2bw(Repository) ASSERTED/NONE 900000 [kubernetes-acme:900000]`,
		// A label alone is not: ownership answers read only asserted facts.
		w + ` owned_by -> kubernetes:team/payments(Team) CANDIDATE/BELOW_THRESHOLD 800000 [kubernetes-acme:800000]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in\n%s", want, got)
		}
	}
}

// A hint taken from the image is below the threshold, and a workload with
// neither annotation nor label claims neither relation (fixtures 10 and 11).
func TestKubernetesImageHintStaysACandidateAndAnUnlabelledWorkloadClaimsNothing(t *testing.T) {
	e := newEnvWith(t, kubernetesConfig(t))
	applyClean(e, "kubernetes-acme", fixture(t, "10-kubernetes-workload-image-hint.json"))
	applyClean(e, "kubernetes-acme", fixture(t, "11-kubernetes-workload-no-repository.json"))
	got := factsAt(t, e, ts("2026-10-07T00:00:00Z"))
	hint := "kubernetes:workload/k3d-acme/billing/statefulset/billing-core(Component) defined_in -> github:repo/acme/billing-core(Repository) CANDIDATE/BELOW_THRESHOLD 300000 [kubernetes-acme:300000]"
	if !strings.Contains(got, hint) {
		t.Errorf("want %q in\n%s", hint, got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "legacy-billing(Component) defined_in") || strings.Contains(line, "legacy-billing(Component) owned_by") {
			t.Errorf("an unlabelled workload claims %q", line)
		}
	}
}

// Removing the annotation and the label ends the claims, because each
// workload observation lists every repository, team and namespace it claims
// (the snapshot over deployed_to, owned_by and defined_in).
func TestKubernetesRemovedAnnotationAndLabelEndTheirClaims(t *testing.T) {
	e := newEnvWith(t, kubernetesConfig(t))
	applyClean(e, "github-acme", fixture(t, "1-repository-codeowners.json"))
	first := fixture(t, "9-kubernetes-workload-annotation.json")
	applyClean(e, "kubernetes-acme", first)

	later := proto.CloneOf(first)
	later.Id = "kubernetes:workload/k3d-acme/payments/deployment/payments-api@2026-10-08T09:00:00.000000Z"
	later.Time = timestamppb.New(ts("2026-10-08T09:00:00Z"))
	later.Data.Relations = later.Data.Relations[:1] // deployed_to only
	applyClean(e, "kubernetes-acme", later)

	if got := factsAt(t, e, ts("2026-10-07T00:00:00Z")); !strings.Contains(got, "owned_by -> kubernetes:team/payments") || !strings.Contains(got, "defined_in -> ") {
		t.Errorf("before the second sync, want both claims in\n%s", got)
	}
	got := factsAt(t, e, ts("2026-10-09T00:00:00Z"))
	for _, gone := range []string{" owned_by -> kubernetes:team/payments", " defined_in -> "} {
		if strings.Contains(got, gone) {
			t.Errorf("after the second sync, want no %q in\n%s", gone, got)
		}
	}
	if !strings.Contains(got, " deployed_to -> kubernetes:namespace/k3d-acme/payments") {
		t.Errorf("after the second sync, want deployed_to to remain in\n%s", got)
	}
}

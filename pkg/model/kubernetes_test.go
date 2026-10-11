package model

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// The confidences docs/spec/adapter-protocol.md ("Kubernetes") gives each
// kind of claim, by the qualifier that names where the claim came from.
const (
	ppmAnnotation = 900000
	ppmImage      = 300000
	ppmLabel      = 800000
)

// secretLookalikes are the values of the fields the spec's sample Deployment
// carries and the adapter must never send (docs/spec/adapter-protocol.md,
// "Kubernetes").
var secretLookalikes = []string{"not-a-real-password", "not-a-real-token", "DATABASE_PASSWORD", "--token"}

func readKubernetesDeclaration(t *testing.T) *modelv1alpha1.AdapterDeclaration {
	t.Helper()
	d := &modelv1alpha1.AdapterDeclaration{}
	if err := DecodeJSON(readFixture(t, "../../testdata/declarations/kubernetes.json"), d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestKubernetesFixturesStayInsideTheDeclaration is the fixed field list as a
// test: every kind, key, attribute, relation and snapshot predicate in the
// example observations is one the Kubernetes declaration names, and none of
// the example's secret lookalikes appears.
func TestKubernetesFixturesStayInsideTheDeclaration(t *testing.T) {
	decl := readKubernetesDeclaration(t)
	kinds := map[string]*modelv1alpha1.KindDeclaration{}
	for _, k := range decl.GetKinds() {
		kinds[k.GetKind()] = k
	}
	files, err := filepath.Glob("../../testdata/observations/valid/*kubernetes*.json")
	if err != nil || len(files) < 5 {
		t.Fatalf("got %d Kubernetes fixtures (%v), want at least 5", len(files), err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw := readFixture(t, file)
			for _, s := range secretLookalikes {
				if strings.Contains(string(raw), s) {
					t.Errorf("fixture contains %q, which the adapter must never send", s)
				}
			}
			o, err := DecodeObservation(raw)
			if err != nil {
				t.Fatal(err)
			}
			checkAgainstKubernetesDeclaration(t, kinds, o)
		})
	}
}

func checkAgainstKubernetesDeclaration(t *testing.T, kinds map[string]*modelv1alpha1.KindDeclaration, o *eventv1alpha1.Observation) {
	t.Helper()
	d := o.GetData()
	e := d.GetEntity()
	k := kinds[e.GetKind()]
	if k == nil {
		t.Fatalf("kind %s is not declared", e.GetKind())
	}
	declared := map[string]Predicate{}
	field := func(predicate string, dir modelv1alpha1.Direction) bool {
		return slices.ContainsFunc(k.GetFields(), func(f *modelv1alpha1.FieldDeclaration) bool {
			return f.GetPredicate() == predicate && direction(f) == dir
		})
	}
	for _, f := range k.GetFields() {
		if _, registered := LookupPredicate(f.GetPredicate()); !registered {
			declared[f.GetPredicate()] = Predicate{Name: f.GetPredicate(), Type: f.GetType(), Cardinality: f.GetCardinality()}
		}
	}
	if err := ValidateObservationWith(o, declared); err != nil {
		t.Fatalf("declared attribute types not respected: %v", err)
	}

	ns, keyType, id, err := Key(e.GetKey()).Parse()
	if err != nil || ns != "kubernetes" {
		t.Fatalf("entity key %q: got namespace %q (%v), want kubernetes", e.GetKey(), ns, err)
	}
	if !slices.ContainsFunc(k.GetKeys(), func(kt *modelv1alpha1.KeyTypeDeclaration) bool { return kt.GetKeyType() == keyType }) {
		t.Errorf("key type %s is not declared for %s", keyType, e.GetKind())
	}
	for name := range e.GetAttributes() {
		if !field(name, modelv1alpha1.Direction_DIRECTION_OUT) {
			t.Errorf("attribute %s is not declared for %s", name, e.GetKind())
		}
	}
	for _, r := range d.GetRelations() {
		if r.GetFrom() != "" || !field(r.GetType(), modelv1alpha1.Direction_DIRECTION_OUT) {
			t.Errorf("relation %s is not declared as an outgoing field of %s", r.GetType(), e.GetKind())
		}
	}
	for _, s := range d.GetSnapshots() {
		if s.GetDirection() != modelv1alpha1.Direction_DIRECTION_OUT {
			t.Errorf("snapshot direction %v, want out", s.GetDirection())
		}
		for _, p := range s.GetPredicates() {
			if !field(p, s.GetDirection()) {
				t.Errorf("snapshot predicate %s is not declared for %s", p, e.GetKind())
			}
		}
	}

	switch e.GetKind() {
	case "Component":
		checkWorkload(t, id, o)
	case "Environment":
		if cluster := e.GetAttributes()["cluster"].GetStringValue(); !strings.HasPrefix(id, cluster+"/") {
			t.Errorf("namespace key id %q does not start with its cluster %q", id, cluster)
		}
	}
}

// checkWorkload checks the workload key's shape and that its claims match the
// spec's table: the name and kind agree with the key, the namespace is the
// Environment, and each repository and team claim carries the confidence and
// qualifier the spec gives its source.
func checkWorkload(t *testing.T, id string, o *eventv1alpha1.Observation) {
	t.Helper()
	d := o.GetData()
	attrs := d.GetEntity().GetAttributes()
	parts := strings.Split(id, "/")
	if len(parts) != 4 || !slices.Contains([]string{"deployment", "statefulset", "daemonset"}, parts[2]) {
		t.Fatalf("workload key id %q, want <cluster>/<namespace>/<kind>/<name> with a lower-case workload kind", id)
	}
	if got := strings.ToLower(attrs["workload_kind"].GetStringValue()); got != parts[2] {
		t.Errorf("workload_kind %q does not match the key's kind %q", got, parts[2])
	}
	if got := attrs["workload_name"].GetStringValue(); got != parts[3] {
		t.Errorf("workload_name %q does not match the key's name %q", got, parts[3])
	}
	seen := map[string]bool{}
	for _, r := range d.GetRelations() {
		seen[r.GetType()] = true
		q := r.GetAttributes()
		switch r.GetType() {
		case "deployed_to":
			if want := "kubernetes:namespace/" + parts[0] + "/" + parts[1]; r.GetTo() != want {
				t.Errorf("deployed_to %q, want %q", r.GetTo(), want)
			}
		case "owned_by":
			if !strings.HasPrefix(r.GetTo(), "kubernetes:team/") || r.GetConfidencePpm() != ppmLabel || q["label"] == nil {
				t.Errorf("owned_by %q at %d with qualifiers %v, want a kubernetes team at %d with a label qualifier", r.GetTo(), r.GetConfidencePpm(), q, ppmLabel)
			}
		case "defined_in":
			_, _, repo, err := Key(r.GetTo()).Parse()
			if err != nil || !strings.HasPrefix(r.GetTo(), "github:repo/") || strings.Count(repo, "/") != 1 {
				t.Errorf("defined_in %q, want a github:repo/<owner>/<name> key (%v)", r.GetTo(), err)
			}
			switch {
			case q["annotation"] != nil && r.GetConfidencePpm() == ppmAnnotation:
			case q["image"] != nil && r.GetConfidencePpm() == ppmImage:
				if !strings.HasSuffix(q["image"].GetStringValue(), "/"+repo) {
					t.Errorf("image hint %q does not end in the repository %s", q["image"].GetStringValue(), repo)
				}
			default:
				t.Errorf("defined_in at %d with qualifiers %v, want annotation at %d or image at %d", r.GetConfidencePpm(), q, ppmAnnotation, ppmImage)
			}
		}
	}
	if !seen["deployed_to"] {
		t.Error("a workload without deployed_to")
	}
}

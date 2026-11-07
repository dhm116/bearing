package boot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
)

const k8sService = `{"apiVersion": "bearing/v1alpha1", "kind": "EntityKind", "metadata": {"name": "k8s-service"},
  "spec": {"name": "k8s.Service", "extends": "Component",
    "attributes": [{"name": "cluster_ip", "type": "VALUE_TYPE_STRING", "cardinality": "CARDINALITY_ONE", "conflict": "CONFLICT_POLICY_NONE"}],
    "relations": [{"name": "exposes", "range": ["Component"], "cardinality": "CARDINALITY_MANY", "conflict": "CONFLICT_POLICY_NONE"}]}}`

func kindsSettings(kinds ...string) *Settings {
	s := &Settings{}
	for _, k := range kinds {
		s.EntityKinds = append(s.EntityKinds, json.RawMessage(k))
	}
	return s
}

func TestSettingsRegistryBuildsConfiguredKinds(t *testing.T) {
	t.Parallel()
	reg, err := kindsSettings(k8sService).Registry()
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Valid("k8s.Service") || !reg.IsA("k8s.Service", model.KindComponent) {
		t.Fatalf("k8s.Service is not a Component in %v", reg.Kinds())
	}
	if p, ok := reg.Predicate("exposes"); !ok || !p.Relation || !reg.InDomain(p, "k8s.Service") || reg.InDomain(p, model.KindTeam) {
		t.Errorf("exposes = %+v, %v, want a relation with k8s.Service in its domain", p, ok)
	}
	if reg, _ := kindsSettings().Registry(); reg != model.Core() {
		t.Error("settings with no kinds should give the core registry itself")
	}
}

func TestSettingsRegistryRefusesWithTheCause(t *testing.T) {
	t.Parallel()
	edit := func(from, to string) string { return strings.Replace(k8sService, from, to, 1) }
	for _, tt := range []struct {
		name  string
		kinds []string
		want  string
	}{
		{"not an EntityKind", []string{edit(`"EntityKind"`, `"Source"`)}, "kind EntityKind are required"},
		{"a bad resource name", []string{edit(`"k8s-service"`, `"K8s Service"`)}, "metadata.name"},
		{"the same resource twice", []string{k8sService, k8sService}, "used twice"},
		{"a bare name", []string{edit(`"k8s.Service"`, `"Service"`)}, "invalid kind name"},
		{"the reserved namespace", []string{edit(`"k8s.Service"`, `"bearing.Service"`)}, "reserved"},
		{"a missing parent", []string{edit(`"Component"`, `"Missing"`)}, "unknown kind"},
		{"a relation without a range", []string{edit(`"range": ["Component"], `, ``)}, "needs a range"},
		{"an attribute without a type", []string{edit(`"type": "VALUE_TYPE_STRING", `, ``)}, "incomplete predicate shape"},
		{"a second shape for a core attribute", []string{edit(`"cluster_ip", "type": "VALUE_TYPE_STRING", "cardinality": "CARDINALITY_ONE"`, `"name", "type": "VALUE_TYPE_STRING", "cardinality": "CARDINALITY_MANY"`)}, "shape conflict"},
		{"an unknown field", []string{edit(`"extends"`, `"extend"`)}, "unknown field"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := kindsSettings(tt.kinds...).Registry()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestSettingsRegistryAllowsAParentDefinedLater(t *testing.T) {
	t.Parallel()
	child := strings.NewReplacer(`"k8s-service"`, `"k8s-pod"`, `"k8s.Service"`, `"k8s.Pod"`, `"Component"`, `"k8s.Service"`,
		`"cluster_ip"`, `"pod_ip"`, `"exposes"`, `"runs"`).Replace(k8sService)
	if _, err := kindsSettings(child, k8sService).Registry(); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsValidateChecksEntityKinds(t *testing.T) {
	t.Parallel()
	with := func(kind, extra string) *Settings {
		s, err := Parse([]byte(strings.Replace(goodSettings, `"sources"`, `"entity_kinds": [`+kind+`], `+extra+`"sources"`, 1)))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if err := with(strings.Replace(k8sService, `"Component"`, `"Missing"`, 1), "").Validate(); err == nil || !strings.Contains(err.Error(), "entity_kinds") {
		t.Fatalf("got %v, want a refusal naming entity_kinds", err)
	}
	if err := with(k8sService, `"matching": {"half_life": {"k8s.Service": "P30D"}}, `).Validate(); err != nil {
		t.Fatalf("half_life for a configured kind: %v", err)
	}
}

func decl(kinds ...*modelv1alpha1.KindDeclaration) *modelv1alpha1.AdapterDeclaration {
	return &modelv1alpha1.AdapterDeclaration{Name: "a", IssuerType: "a", Kinds: kinds}
}

func TestExtendDeclaredKeepsOneShapePerNameAcrossAdapters(t *testing.T) {
	t.Parallel()
	str, num := modelv1alpha1.ValueType_VALUE_TYPE_STRING, modelv1alpha1.ValueType_VALUE_TYPE_FLOAT
	one := modelv1alpha1.Cardinality_CARDINALITY_ONE
	mk := func(typ modelv1alpha1.ValueType) *modelv1alpha1.AdapterDeclaration {
		return decl(&modelv1alpha1.KindDeclaration{Kind: "Team", Fields: []*modelv1alpha1.FieldDeclaration{{Predicate: "zone", Type: typ, Cardinality: one}}})
	}
	s := &Settings{Adapters: []AdapterSettings{{Name: "a"}, {Name: "b"}}}
	for _, tt := range []struct {
		name string
		b    modelv1alpha1.ValueType
		want error
	}{
		{"the same shape", str, nil},
		{"another type", num, model.ErrShapeConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reg, err := extendDeclared(model.Core(), s, map[string]*modelv1alpha1.AdapterDeclaration{"a": mk(str), "b": mk(tt.b)})
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if tt.want != nil {
				return
			}
			if _, ok := reg.Predicate("zone"); !ok {
				t.Fatal("zone is not in the registry")
			}
		})
	}
	if reg, err := extendDeclared(model.Core(), s, nil); err != nil || reg != model.Core() {
		t.Errorf("no declared attributes: got %v, %v, want the core registry itself", reg, err)
	}
}

func TestExtendDeclaredSkipsAttributesUnderKindsTheRegistryLacks(t *testing.T) {
	t.Parallel()
	zone := func(kind string, typ modelv1alpha1.ValueType) *modelv1alpha1.AdapterDeclaration {
		return decl(&modelv1alpha1.KindDeclaration{Kind: kind, Optional: kind != "Team", Fields: []*modelv1alpha1.FieldDeclaration{
			{Predicate: "zone", Type: typ, Cardinality: modelv1alpha1.Cardinality_CARDINALITY_ONE},
		}})
	}
	s := &Settings{Adapters: []AdapterSettings{{Name: "a"}, {Name: "b"}}}
	// a's zone is a float under an inactive kind; it must not clash with b's string.
	reg, err := extendDeclared(model.Core(), s, map[string]*modelv1alpha1.AdapterDeclaration{
		"a": zone("k8s.Service", modelv1alpha1.ValueType_VALUE_TYPE_FLOAT),
		"b": zone("Team", modelv1alpha1.ValueType_VALUE_TYPE_STRING),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := reg.Predicate("zone"); !ok || p.Type != modelv1alpha1.ValueType_VALUE_TYPE_STRING {
		t.Errorf("zone = %+v, %v, want b's string", p, ok)
	}
}

type noRegistryStore struct{ store.Store }

func TestGiveRegistryRefusesAStoreThatCannotTakeAVocabularyBeyondCore(t *testing.T) {
	t.Parallel()
	reg, err := kindsSettings(k8sService).Registry()
	if err != nil {
		t.Fatal(err)
	}
	if err := giveRegistry(noRegistryStore{}, reg); err == nil || !strings.Contains(err.Error(), "cannot take a vocabulary") {
		t.Errorf("got %v, want a refusal", err)
	}
	if err := giveRegistry(noRegistryStore{}, model.Core()); err != nil {
		t.Errorf("core registry: got %v, want nil", err)
	}
}

// writeDecl rewrites the world's declaration (the github one) through mod.
func writeDecl(t *testing.T, w *world, mod func(*modelv1alpha1.AdapterDeclaration)) {
	t.Helper()
	b, err := os.ReadFile(w.settings.Adapters[0].Declaration)
	if err != nil {
		t.Fatal(err)
	}
	d := &modelv1alpha1.AdapterDeclaration{}
	if err := model.DecodeJSON(b, d); err != nil {
		t.Fatal(err)
	}
	mod(d)
	out, err := model.EncodeJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.settings.Adapters[0].Declaration, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAssembleComputesActiveKindsPerSource(t *testing.T) {
	t.Parallel()
	svc := func(optional bool) func(*modelv1alpha1.AdapterDeclaration) {
		return func(d *modelv1alpha1.AdapterDeclaration) {
			d.Kinds = append(d.Kinds, &modelv1alpha1.KindDeclaration{Kind: "k8s.Service", Optional: optional})
		}
	}
	base := []model.Kind{"Repository", "Team", "Person"}
	for _, tt := range []struct {
		name    string
		edit    func(*modelv1alpha1.AdapterDeclaration)
		kinds   []string
		want    []model.Kind
		wantErr string
		ran     bool
	}{
		{"core kinds only", func(*modelv1alpha1.AdapterDeclaration) {}, nil, base, "", true},
		{"a required kind that is not enabled", svc(false), nil, nil, `"k8s.Service" is not a registered kind: define it with an EntityKind`, false},
		{"an optional kind that is not enabled", svc(true), nil, base, "", true},
		{"an optional kind that is enabled", svc(true), []string{k8sService}, append(slices.Clone(base), "k8s.Service"), "", true},
		{"a required kind that is enabled", svc(false), []string{k8sService}, append(slices.Clone(base), "k8s.Service"), "", true},
		{"no active kinds", func(d *modelv1alpha1.AdapterDeclaration) {
			d.Kinds = []*modelv1alpha1.KindDeclaration{{Kind: "k8s.Service", Optional: true}}
		}, nil, []model.Kind{}, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, func(s *Settings) { s.EntityKinds = kindsSettings(tt.kinds...).EntityKinds })
			writeDecl(t, w, tt.edit)
			r, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got, ok := r.ActiveKinds["github-acme"]
			if !ok || !slices.Equal(got, tt.want) {
				t.Errorf("active kinds got %v (present %v), want %v", got, ok, tt.want)
			}
			if has := r.srv.HasSource("github-acme"); has != tt.ran {
				t.Errorf("source is synced: got %v, want %v", has, tt.ran)
			}
			// The world's source has a webhook_secret, so a route exists only if it runs.
			if has := r.listen.Webhooks != nil; has != tt.ran {
				t.Errorf("source gets a webhook route: got %v, want %v", has, tt.ran)
			}
		})
	}
}

func TestCheckEmitsRefusesAKindTheDeclarationLacks(t *testing.T) {
	t.Parallel()
	d := decl(&modelv1alpha1.KindDeclaration{Kind: "Team"})
	if err := checkEmits("s", []model.Kind{"Team", "Person"}, d); err == nil || !strings.Contains(err.Error(), `emits "Person"`) {
		t.Errorf("got %v, want a refusal naming Person", err)
	}
	if err := checkEmits("s", []model.Kind{"Team"}, d); err != nil {
		t.Errorf("got %v, want nil", err)
	}
}

// A vocabulary the stored one forbids stops the start with the store's
// message; the same kinds on the next start do not.
func TestAssembleRefusesAVocabularyThatChangesAStoredKind(t *testing.T) {
	t.Parallel()
	url := "duckdb://" + filepath.Join(t.TempDir(), "bearing.duckdb")
	boot := func(kind string) error {
		w := newWorld(t, func(s *Settings) {
			s.Store.URL = url
			s.EntityKinds = kindsSettings(kind).EntityKinds
		})
		r, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv})
		if err == nil {
			r.Close()
		}
		return err
	}
	if err := boot(k8sService); err != nil {
		t.Fatal(err)
	}
	if err := boot(k8sService); err != nil {
		t.Fatalf("the same vocabulary again: got %v, want a start", err)
	}
	err := boot(strings.Replace(k8sService, `"extends": "Component"`, `"extends": "Repository"`, 1))
	if !errors.Is(err, store.ErrVocabularyChanged) || !strings.Contains(err.Error(), "k8s.Service") {
		t.Fatalf("got %v, want a refusal naming k8s.Service", err)
	}
}

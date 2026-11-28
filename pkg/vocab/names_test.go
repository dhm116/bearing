package vocab

import (
	"errors"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func names(t *testing.T, namespaces []string, aliases map[string]string) *Names {
	t.Helper()
	n, err := NewNames(namespaces, aliases)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNamesResolveShortFullCoreAndBareNames(t *testing.T) {
	t.Parallel()
	n := names(t, []string{"io.k8s", "com.github"}, nil)
	for _, tc := range []struct{ written, want string }{
		{"k8s:Service", "io.k8s:Service"},
		{"io.k8s:Service", "io.k8s:Service"},
		{"github:Repository", "com.github:Repository"},
		{"core:Component", "Component"},
		{CoreNamespace + ":owned_by", "owned_by"},
		{"Component", "Component"},
	} {
		if got, err := n.Resolve(tc.written); err != nil || got != tc.want {
			t.Errorf("Resolve(%q) got %q, %v, want %q", tc.written, got, err, tc.want)
		}
	}
	if _, err := n.Resolve("vault:Policy"); !errors.Is(err, ErrUnknownNamespace) {
		t.Errorf("got %v, want ErrUnknownNamespace for a namespace not loaded", err)
	}
	for _, empty := range []string{"core:", "k8s:", "io.k8s:"} {
		if got, err := n.Resolve(empty); !errors.Is(err, ErrNamespace) || !strings.HasPrefix(err.Error(), "vocab: ") {
			t.Errorf("Resolve(%q) got %q, %v, want a vocab: error wrapping ErrNamespace for an empty term", empty, got, err)
		}
	}
}

func TestNamesRefuseABareNamespace(t *testing.T) {
	t.Parallel()
	for _, ns := range []string{"k8s", "", "IO.k8s", "io..k8s", "io.k8s.", "io.k8s-", "io-.k8s"} {
		if _, err := NewNames([]string{ns}, nil); !errors.Is(err, ErrNamespace) {
			t.Errorf("NewNames(%q) got %v, want ErrNamespace", ns, err)
		}
	}
}

func TestNamesAmbiguousShortNameFailsNamingBoth(t *testing.T) {
	t.Parallel()
	n := names(t, []string{"io.k8s", "com.example.k8s"}, nil)
	_, err := n.Resolve("k8s:Service")
	if !errors.Is(err, ErrAmbiguous) || !strings.Contains(err.Error(), "com.example.k8s or io.k8s") {
		t.Fatalf("got %v, want ErrAmbiguous naming com.example.k8s and io.k8s", err)
	}
	if got, err := n.Resolve("io.k8s:Service"); err != nil || got != "io.k8s:Service" {
		t.Errorf("a full name got %q, %v, want it to still resolve", got, err)
	}
}

func TestNamesAliasWinsOverAShortName(t *testing.T) {
	t.Parallel()
	n := names(t, []string{"io.k8s", "com.example.k8s"}, map[string]string{"k8s": "io.k8s", "ex": "com.example.k8s"})
	for written, want := range map[string]string{"k8s:Service": "io.k8s:Service", "ex:Service": "com.example.k8s:Service"} {
		if got, err := n.Resolve(written); err != nil || got != want {
			t.Errorf("Resolve(%q) got %q, %v, want %q", written, got, err, want)
		}
	}
	for alias, ns := range map[string]string{"gh": "com.github", "my.alias": "io.k8s", "own-core": CoreNamespace, "k8s-": "io.k8s"} {
		if _, err := NewNames([]string{"io.k8s"}, map[string]string{alias: ns}); !errors.Is(err, ErrNamespace) {
			t.Errorf("alias %s to %s got %v, want ErrNamespace", alias, ns, err)
		}
	}
}

func TestNamesKeepCoreReserved(t *testing.T) {
	t.Parallel()
	if _, err := NewNames([]string{"io.k8s"}, map[string]string{CoreAlias: "io.k8s"}); !errors.Is(err, ErrNamespace) {
		t.Errorf("got %v, want an alias named core refused", err)
	}
	// A namespace ending in core would be shadowed by core:, so it is refused
	// rather than making core: ambiguous.
	_, err := NewNames([]string{"com.example.core"}, nil)
	if !errors.Is(err, ErrNamespace) || !strings.Contains(err.Error(), "com.example.core") || !strings.HasPrefix(err.Error(), "vocab: ") {
		t.Errorf("got %v, want a vocab: error refusing com.example.core", err)
	}
	n := names(t, []string{"com.example.corex"}, nil)
	if got, err := n.Resolve("core:Component"); err != nil || got != "Component" {
		t.Errorf("got %q, %v, want core:Component to stay the core term", got, err)
	}
}

func TestCoreModuleHoldsTheCoreTermsWithTheirShapes(t *testing.T) {
	t.Parallel()
	m := Core()
	if m.Namespace != CoreNamespace || m.Version == "" {
		t.Fatalf("got %s@%s, want %s with a version", m.Namespace, m.Version, CoreNamespace)
	}
	terms := map[string]Term{}
	for _, tm := range m.Terms {
		terms[tm.Name] = tm
	}
	if c := terms["Component"]; c.Type != modelv1alpha1.TermType_TERM_TYPE_KIND || c.Cardinality != 0 || c.Domain != nil {
		t.Errorf("Component got %+v, want a kind with no shape", c)
	}
	owned := terms["owned_by"]
	if owned.Type != modelv1alpha1.TermType_TERM_TYPE_RELATION || owned.Conflict != modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET ||
		owned.Domain != nil || !slices.Equal(owned.Range, []string{"Team", "Person"}) {
		t.Errorf("owned_by got %+v, want a relation from any kind to Team and Person, conflict set", owned)
	}
	if a := terms["archived"]; a.Type != modelv1alpha1.TermType_TERM_TYPE_ATTRIBUTE || a.ValueType != modelv1alpha1.ValueType_VALUE_TYPE_BOOL ||
		a.Cardinality != modelv1alpha1.Cardinality_CARDINALITY_ONE || a.Conflict != modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_ONE ||
		!slices.Equal(a.Domain, []string{"Repository"}) {
		t.Errorf("archived got %+v, want a bool attribute of Repository, cardinality one, conflict one", a)
	}
}

func TestCommitOfNamesOnlyACleanBuildsRevision(t *testing.T) {
	t.Parallel()
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "4207eb6"}
	for _, tc := range []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{"clean, with a revision", []debug.BuildSetting{rev, {Key: "vcs.modified", Value: "false"}}, "4207eb6"},
		{"dirty", []debug.BuildSetting{rev, {Key: "vcs.modified", Value: "true"}}, ""},
		{"no revision", []debug.BuildSetting{{Key: "GOOS", Value: "linux"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := commitOf(tc.settings); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

package vocab

import (
	"errors"
	"testing"
)

func TestCheckAdapterRefusesCoreNamespaceAndBadSuggestions(t *testing.T) {
	t.Parallel()
	ok := Module{Namespace: "io.k8s", Version: "1", Terms: []Term{{Name: "Deployment", Is: IsKind}}}
	sug := func(sub, obj string) []Mapping {
		return []Mapping{{ID: "s", Subject: sub, Predicate: Broader, Object: obj}}
	}
	for _, tc := range []struct {
		name string
		mod  func(Module) Module
		want error
	}{
		{"fine", func(m Module) Module { m.Suggests = sug("io.k8s:Deployment", CoreNamespace+":Component"); return m }, nil},
		{"core namespace", func(m Module) Module { m.Namespace = CoreNamespace; return m }, ErrNamespace},
		{"malformed", func(m Module) Module { m.Version = ""; return m }, ErrModule},
		{"foreign subject", func(m Module) Module { m.Suggests = sug("io.other:X", CoreNamespace+":Component"); return m }, ErrMapping},
		{"object in own namespace", func(m Module) Module { m.Suggests = sug("io.k8s:Deployment", "io.k8s:Other"); return m }, ErrMapping},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.mod(ok).CheckAdapter(); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

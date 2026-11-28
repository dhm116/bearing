package vocab

import (
	"errors"
	"strings"
	"testing"
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
}

func TestNamesRefuseABareNamespace(t *testing.T) {
	t.Parallel()
	for _, ns := range []string{"k8s", "", "IO.k8s", "io..k8s", "io.k8s."} {
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
	for alias, ns := range map[string]string{"gh": "com.github", "my.alias": "io.k8s", "own-core": CoreNamespace} {
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
	// A namespace ending in core never makes core: ambiguous.
	n := names(t, []string{"com.example.core"}, nil)
	if got, err := n.Resolve("core:Component"); err != nil || got != "Component" {
		t.Errorf("got %q, %v, want core:Component to stay the core term", got, err)
	}
	if got, err := n.Resolve("com.example.core:Widget"); err != nil || got != "com.example.core:Widget" {
		t.Errorf("got %q, %v, want the full name of com.example.core", got, err)
	}
}

func TestCoreModuleIsTheCoreNamespace(t *testing.T) {
	t.Parallel()
	m := Core()
	if m.Namespace != CoreNamespace || m.Version == "" || len(m.Terms) == 0 {
		t.Fatalf("got %s@%s with %d terms, want %s with a version and terms", m.Namespace, m.Version, len(m.Terms), CoreNamespace)
	}
}

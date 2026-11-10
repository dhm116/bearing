package heading

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// specs are the documents the spike reads. GitHub's is 14 MB, so it is
// fetched, not committed: set HEADING_GITHUB_SPEC to its path (see FINDINGS.md).
func specs(t *testing.T) map[string][]string {
	t.Helper()
	m := map[string][]string{"kubernetes": kubernetesSpecs}
	if p := os.Getenv("HEADING_GITHUB_SPEC"); p != "" {
		m["github"] = []string{p}
	}
	return m
}

// kubernetesSpecs are the core and apps group documents of Kubernetes 1.31,
// as a cluster serves them at /openapi/v3/api/v1 and /openapi/v3/apis/apps/v1.
var kubernetesSpecs = []string{"testdata/k8s-v1.31.0-api__v1_openapi.json", "testdata/k8s-v1.31.0-apis__apps__v1_openapi.json"}

func loadAll(t *testing.T, paths []string) *Spec {
	t.Helper()
	var all []*Spec
	for _, p := range paths {
		s, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, s)
	}
	return Merge(all...)
}

// TestAnalyzeReportsWhatTheSpecsImply writes each spec's report and mapping
// skeleton to testdata/out/, for FINDINGS.md.
func TestAnalyzeReportsWhatTheSpecsImply(t *testing.T) {
	if err := os.MkdirAll("testdata/out", 0o750); err != nil {
		t.Fatal(err)
	}
	for name, paths := range specs(t) {
		t.Run(name, func(t *testing.T) {
			r := Analyze(loadAll(t, paths))
			t.Log("\n" + r.String())
			if len(r.Kinds) == 0 {
				t.Fatal("got no kinds")
			}
			write(t, filepath.Join("testdata/out", name+".report.txt"), r.String())
			write(t, filepath.Join("testdata/out", name+".heading.yaml"), Skeleton(r))
		})
	}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Skeleton renders the mapping heading init would write: what was guessed,
// and a TODO for each thing that wasn't.
func Skeleton(r *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# heading init: %s\nnamespace: %s\nkinds:\n", r.Title, r.Namespace)
	for _, k := range r.Kinds {
		fmt.Fprintf(&b, "  - kind: %s.%s\n    schema: %s\n", r.Namespace, k.Name, k.Schema)
		if len(k.Lists) == 0 {
			b.WriteString("    list: TODO  # no list route; reachable only through another kind\n")
		} else {
			fmt.Fprintf(&b, "    list: GET %s\n    paging: %s\n", shortest(k.Lists).Path, paging(shortest(k.Lists)))
		}
		if len(k.Key) == 0 {
			b.WriteString("    key: TODO\n")
		} else {
			fmt.Fprintf(&b, "    key: [%s]  # from %s\n", strings.Join(k.Key, ", "), k.KeyFrom)
		}
		for _, f := range k.Refs {
			if f.Asserted {
				fmt.Fprintf(&b, "    - ref: %s -> %s  # $ref, asserted\n", f.Field, f.Target)
			} else {
				fmt.Fprintf(&b, "    - ref: %s -> TODO  # guessed from the name, proposed\n", f.Field)
			}
		}
	}
	return b.String()
}

func shortest(ops []Operation) Operation {
	best := ops[0]
	for _, o := range ops[1:] {
		if len(o.PathParams()) < len(best.PathParams()) || (len(o.PathParams()) == len(best.PathParams()) && len(o.Path) < len(best.Path)) {
			best = o
		}
	}
	return best
}

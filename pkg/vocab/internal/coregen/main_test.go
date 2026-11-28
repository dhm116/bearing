package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestCoreGenMatchesCoreYAML(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("../../core.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want, err := render(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../core_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("core_gen.go differs from what core.yaml generates: run make generate and commit the result")
	}
}

func TestCoreGenRefusesAMalformedModule(t *testing.T) {
	t.Parallel()
	head := "namespace: com.dougmccall.bearing.core\nversion: 1.0.0\nterms:\n"
	for _, tc := range []struct{ name, yaml, want string }{
		{"another namespace", "namespace: io.k8s\nversion: 1\n", "namespace must be"},
		{"no version", "namespace: com.dougmccall.bearing.core\n", "version"},
		{"an unknown field", head + "  - {name: X, is: kind, parent: Y}\n", "parent"},
		{"a term twice", head + "  - {name: X, is: kind}\n  - {name: X, is: kind}\n", "defined twice"},
		{"an unknown is", head + "  - {name: X, is: thing}\n", `"thing" is not a term type`},
		{"a kind with a shape", head + "  - {name: X, is: kind, cardinality: one}\n", "a kind has no"},
		{"an attribute without a type", head + "  - {name: x, is: attribute, cardinality: one, conflict: none}\n", "needs a type"},
		{"a relation with a type", head + "  - {name: x, is: relation, type: string, cardinality: one, conflict: none}\n", "needs a type"},
		{"an attribute with a range", head + "  - {name: x, is: attribute, type: string, cardinality: one, conflict: none, range: [X]}\n", "no range"},
		{"an unknown value type", head + "  - {name: x, is: attribute, type: text, cardinality: one, conflict: none}\n", `"text" is not a value type`},
		{"no cardinality", head + "  - {name: x, is: relation, conflict: none}\n", "cardinality"},
		{"an unknown conflict", head + "  - {name: x, is: relation, cardinality: many, conflict: all}\n", "conflict policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := render([]byte(tc.yaml)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

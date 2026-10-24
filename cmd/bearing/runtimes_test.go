package main

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/query"
)

func TestRuntimesListsEntitiesBelowAVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	decl := func(lang, version, file string) *structpb.Value {
		v, err := structpb.NewValue(map[string]any{"language": lang, "version": version, "file": file})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	runtimes := func(vs ...*structpb.Value) func(*modelv1alpha1.ObservationData) {
		return func(d *modelv1alpha1.ObservationData) {
			d.Entity.Attributes["runtime"] = structpb.NewListValue(&structpb.ListValue{Values: vs})
		}
	}
	w.ingest(githubSource, seedAt,
		entity("Repository", "github:repo/acme/old", "old", runtimes(decl("go", "1.21", "go.mod"))),
		entity("Repository", "github:repo/acme/new", "new", runtimes(decl("go", "1.23", "go.mod"))),
		entity("Repository", "github:repo/acme/odd", "odd", runtimes(decl("go", "tip", "go.mod"))),
	)
	out, err := w.cli("runtimes", "--language", "golang", "--below", "1.22")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"go below 1.22", "old [Repository", "1.21 (go.mod)", "version not readable", "tip (go.mod)", "1 at or above"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	var r query.Runtimes
	ask(t, w, &r, "runtimes", "--language", "go", "--below", "1.22")
	if len(r.Below) != 1 || len(r.Unknown) != 1 || r.AtOrAbove != 1 {
		t.Errorf("got %+v, want one below, one unknown and one at or above", r)
	}
	if _, err := w.cli("runtimes", "--below", "1.22"); !errors.Is(err, errUsage) {
		t.Errorf("without --language: got %v, want a usage error", err)
	}
}

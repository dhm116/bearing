package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"bearing.example/pkg/query"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// checkGolden compares got with testdata/query/<name>.golden.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "query", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: golden files under testdata
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from %s (run with -update to rewrite it):\n--- got\n%s\n--- want\n%s", name, path, got, want)
	}
}

// TestQueryGoldens pins the text of each command on the small Acme world.
func TestQueryGoldens(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"get-repo", []string{"get", "github:repo/acme/payments-api"}},
		{"get-repo-before-the-move", []string{"get", "github:repo/acme/payments-api", "--as-of", "2026-10-01T01:00:00Z"}},
		{"owner-from-codeowners", []string{"owner", "acme/payments-api"}},
		{"owner-from-codeowners-before-the-move", []string{"owner", "acme/payments-api", "--as-of", "2026-10-01T01:00:00Z"}},
		{"owner-from-owned-by", []string{"owner", "acme/billing"}},
		{"related-depends-on", []string{"related", "catalog:component/payments", "--predicate", "depends_on", "--depth", "2"}},
		{"related-in", []string{"related", "github:team/acme/payments", "--direction", "in"}},
		{"changes-repo", []string{"changes", "github:repo/acme/payments-api"}},
		{"changes-nothing", []string{"changes", "--since", "2026-09-01", "--as-of", "2026-09-02"}},
		{"find", []string{"find", "payments", "--kind", "Component"}},
		{"get-json", []string{"get", "github:user/jdoe", "--json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			got, err := w.cli(tt.args...)
			if err != nil {
				t.Fatalf("bearing %s: %v", strings.Join(tt.args, " "), err)
			}
			checkGolden(t, tt.name, "$ bearing "+strings.Join(tt.args, " ")+"\n\n"+got)
		})
	}
}

// ask runs a command with --json and decodes the answer into out.
func ask[T any](t *testing.T, w *world, out *T, args ...string) {
	t.Helper()
	got, err := w.cli(append(args, "--json")...)
	if err != nil {
		t.Fatalf("bearing %s: %v", strings.Join(args, " "), err)
	}
	if err := json.Unmarshal([]byte(got), out); err != nil {
		t.Fatalf("bearing %s: bad JSON: %v\n%s", strings.Join(args, " "), err, got)
	}
}

func TestOwnerTakesARepoOrAKey(t *testing.T) {
	w := newWorld(t)
	var byRepo, byKey query.Ownership
	ask(t, w, &byRepo, "owner", "acme/payments-api")
	ask(t, w, &byKey, "owner", "github:repo/acme/payments-api")
	if byRepo.Subject.ID == "" || byRepo.Subject.ID != byKey.Subject.ID {
		t.Errorf("owner acme/payments-api is %q, and by key %q: want the same entity", byRepo.Subject.ID, byKey.Subject.ID)
	}
	if len(byRepo.Owners) != 1 || byRepo.Owners[0].Object.Name != "Platform" || byRepo.Owners[0].Predicate != "approves_changes" {
		t.Fatalf("owners = %+v, want Platform through CODEOWNERS", byRepo.Owners)
	}
	if _, err := w.cli("owner", "acme/payments-api", "--namespace", "nope"); !errors.Is(err, query.ErrNotFound) {
		t.Errorf("owner in an unknown namespace: got %v, want not found", err)
	}
}

func TestEveryFactCarriesItsSources(t *testing.T) {
	w := newWorld(t)
	var e query.Entity
	ask(t, w, &e, "get", "github:repo/acme/payments-api")
	if len(e.Facts) == 0 {
		t.Fatal("no facts")
	}
	for _, f := range e.Facts {
		if len(f.Sources) == 0 {
			t.Errorf("fact %s has no sources", f.Predicate)
		}
		for _, s := range f.Sources {
			if s.Source != githubSource || s.ObservationID == "" || s.ObservedAt.IsZero() {
				t.Errorf("fact %s: source %+v lacks a source, an observation or an observed time", f.Predicate, s)
			}
		}
	}
	var c query.Changes
	ask(t, w, &c, "changes", "--since", "2026-10-01T01:00:00Z")
	if len(c.Changes) != 2 {
		t.Fatalf("got %d changes, want the CODEOWNERS move: one closed, one opened", len(c.Changes))
	}
	for _, ch := range c.Changes {
		if len(ch.Fact.Sources) == 0 {
			t.Errorf("change %s %s has no sources", ch.Kind, ch.Fact.Predicate)
		}
	}
}

func TestRelatedJSONCarriesQualifiers(t *testing.T) {
	w := newWorld(t)
	var got query.Relations
	ask(t, w, &got, "related", "catalog:component/payments", "--predicate", "depends_on", "--depth", "1")
	want := map[string]any{"protocol": "http", "port": 8080.0, "via": "env:LEDGER_URL"}
	if len(got.Steps) != 1 || !reflect.DeepEqual(got.Steps[0].Fact.Qualifiers, want) {
		t.Fatalf("got %+v, want qualifiers %v", got.Steps, want)
	}
}

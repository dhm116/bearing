package e2e

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bearing.example/pkg/query"
)

var update = flag.Bool("update", false, "rewrite the golden answers")

// redact drops the entity IDs, which the store makes up at random.
func redact(r *query.Runtimes) {
	for _, list := range [][]query.RuntimeEntity{r.Entities, r.Below, r.Unknown} {
		for i := range list {
			list[i].Subject.ID = ""
		}
	}
}

// TestAcmeAnswersWhichServicesUseALanguageBelowAVersion is question 2
// (go below 1.22 is golden in questions_test.go):
// payments-api declares Go 1.21 and sandbox 1.23.1; web has a node range
// (>=18) next to .nvmrc 20; handbook's .nvmrc is lts/hydrogen, which cannot
// be compared and is reported as unknown, not guessed.
func TestAcmeAnswersWhichServicesUseALanguageBelowAVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	beforeSync := w.clock.Now().Add(-time.Hour)
	w.syncGitHub()
	q := &query.Querier{Graph: w.store, Now: w.clock.Now}
	for _, tc := range []struct {
		name string
		req  query.RuntimesRequest
	}{
		{"node-below-20", query.RuntimesRequest{Language: "nodejs", Below: "20"}},
		{"node-inventory", query.RuntimesRequest{Language: "node"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := q.Runtimes(t.Context(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			redact(got)
			b, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			b = append(b, '\n')
			path := filepath.Join("testdata", "runtimes-"+tc.name+".golden.json")
			if *update {
				if err := os.MkdirAll("testdata", 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path) //nolint:gosec // a fixed golden file
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != string(want) {
				t.Errorf("answer differs from %s (rewrite with -update):\n%s", path, b)
			}
		})
	}
	// The thresholds the goldens stand for, read plainly.
	// Before the first sync nothing was known.
	if early, err := q.Runtimes(t.Context(), query.RuntimesRequest{Language: "go", Below: "1.22", At: beforeSync}); err != nil || len(early.Below)+len(early.Unknown) != 0 || *early.AtOrAbove != 0 {
		t.Fatalf("before the sync: got %+v, %v, want an empty answer", early, err)
	}
	got, err := q.Runtimes(t.Context(), query.RuntimesRequest{Language: "go", Below: "1.22"})
	if err != nil || len(got.Below) != 1 || got.Below[0].Subject.Name != "payments-api" || *got.AtOrAbove != 1 {
		t.Fatalf("got %+v, %v, want payments-api below and sandbox at or above", got, err)
	}
}

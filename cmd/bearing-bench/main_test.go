package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bearing.example/internal/memstore"
)

func TestTableNumbersMatchMemstore(t *testing.T) {
	for _, c := range []struct {
		name string
		got  int
		want memstore.Table
	}{
		{"bindings", tblBindings, memstore.TableBindings},
		{"supports", tblSupports, memstore.TableSupports},
		{"facts", tblFacts, memstore.TableFacts},
		{"conflicts", tblConflicts, memstore.TableConflicts},
		{"issues", tblIssues, memstore.TableIssues},
		{"state", tblState, memstore.TableState},
	} {
		if c.got != int(c.want) {
			t.Errorf("%s: got table %d, want %d", c.name, c.got, c.want)
		}
		if tblNames[c.got] != c.name {
			t.Errorf("table %d: got name %q, want %q", c.got, tblNames[c.got], c.name)
		}
	}
}

func TestParseCounts(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    []int64
		wantErr bool
	}{
		{"1,20, 300", []int64{1, 20, 300}, false},
		{"", nil, false},
		{"5,,6", []int64{5, 6}, false},
		{"0", nil, true},
		{"x", nil, true},
	} {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseCounts(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("got error %v, want error %v", err, c.wantErr)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestQuantile(t *testing.T) {
	xs := []float64{9, 1, 5, 3, 7}
	for _, c := range []struct{ q, want float64 }{{0, 1}, {0.5, 5}, {1, 9}} {
		if got := quantile(xs, c.q); got != c.want {
			t.Errorf("quantile %v: got %v, want %v", c.q, got, c.want)
		}
	}
	if got := quantile(nil, 0.5); got != 0 {
		t.Errorf("empty: got %v, want 0", got)
	}
	if xs[0] != 9 {
		t.Errorf("quantile reordered its input: %v", xs)
	}
}

func TestRedactURLDropsThePassword(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // G101: a made-up password the test expects to be dropped
		"postgres://bench:secret@host:5432/db?schema=x": "postgres://bench@host:5432/db?schema=x",
		"postgres://bench@host/db":                      "postgres://bench@host/db",
		"mem://":                                        "mem://",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q): got %q, want %q", in, got, want)
		}
	}
}

func smallOrg() orgConfig {
	o := defaultOrg()
	o.Repos, o.People, o.Teams, o.ChangesPerDay = 12, 8, 3, 6
	return o
}

func TestStreamIsDeterministic(t *testing.T) {
	a, b := newStream(smallOrg()), newStream(smallOrg())
	for i := range 400 {
		x, y := a.Next(), b.Next()
		if x.ID != y.ID {
			t.Fatalf("event %d: got ID %q, want %q", i, x.ID, y.ID)
		}
		xb, _ := json.Marshal(x.Observation.GetData())
		yb, _ := json.Marshal(y.Observation.GetData())
		if !bytes.Equal(xb, yb) {
			t.Fatalf("event %d: observations differ", i)
		}
	}
	if a.Count != 400 {
		t.Fatalf("got count %d, want 400", a.Count)
	}
}

// TestLoadSmoke runs the whole load on the in-memory store, with the bulk
// path from day one, and reads the results back.
func TestLoadSmoke(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.ndjson")
	var log bytes.Buffer
	err := run(context.Background(), []string{
		"load", "--store", "mem://", "--declarations", testDeclarations, "--out", out,
		"--repos", "12", "--people", "8", "--teams", "3", "--changes-per-day", "6",
		"--bulk-after-day", "1", "--events", "120", "--quick",
	}, &log)
	if err != nil {
		t.Fatalf("load: %v\n%s", err, log.String())
	}
	b, err := os.ReadFile(out) //nolint:gosec // G304: a file the test just made
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("result line %q: %v", line, err)
		}
		kinds[r.Kind]++
	}
	if kinds["run"] != 1 || kinds["load_window"] == 0 {
		t.Fatalf("got result kinds %v, want one run record and a load window", kinds)
	}
}

func TestRunRejectsBadArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"load"}, {"load", "--store", "mem://", "extra"}, {"load", "--nope"}} {
		if err := run(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Errorf("run(%v): got no error, want a usage error", args)
		}
	}
}

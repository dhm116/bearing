package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseDiff(t *testing.T) {
	diff := `diff --git a/pkg/a.go b/pkg/a.go
index 1..2 100644
--- a/pkg/a.go
+++ b/pkg/a.go
@@ -3,0 +4,2 @@ func A() {
+	x := 1
+	_ = x
@@ -10 +12 @@ func B() {
-	old()
+	new()
diff --git a/pkg/gone.go b/pkg/gone.go
deleted file mode 100644
--- a/pkg/gone.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package pkg
diff --git "a/pkg/caf\303\251.go" "b/pkg/caf\303\251.go"
new file mode 100644
--- /dev/null
+++ "b/pkg/caf\303\251.go"
@@ -0,0 +1 @@
+package pkg
`
	got, err := parseDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[int]bool{"pkg/a.go": {4: true, 5: true, 12: true}, "pkg/café.go": {1: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseProfileMergesDuplicates(t *testing.T) {
	profile := `mode: set
bearing.example/pkg/a.go:3.10,5.2 2 0
bearing.example/pkg/a.go:6.10,6.20 1 0
bearing.example/pkg/a.go:3.10,5.2 2 1
`
	got, err := parseProfile(strings.NewReader(profile), "bearing.example")
	if err != nil {
		t.Fatal(err)
	}
	want := []block{
		{file: "pkg/a.go", startLine: 3, endLine: 5, stmts: 2, covered: true},
		{file: "pkg/a.go", startLine: 6, endLine: 6, stmts: 1, covered: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if pct := totalCoverage(got); pct < 66.6 || pct > 66.7 {
		t.Fatalf("total %.2f, want 66.67", pct)
	}
}

func TestReadProfilesMergesPerBlockAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Two blocks share lines but not columns; they are different blocks.
	// The second file has no final newline.
	a := write("a.out", "mode: set\nm/a.go:3.2,3.10 1 1\nm/a.go:3.12,3.20 1 0\n")
	b := write("b.out", "mode: set\nm/a.go:3.2,3.10 1 0\nm/a.go:3.12,3.20 1 1")
	got, err := readProfiles([]string{a, b}, "m")
	if err != nil {
		t.Fatal(err)
	}
	want := []block{
		{file: "a.go", startLine: 3, endLine: 3, stmts: 1, covered: true},
		{file: "a.go", startLine: 3, endLine: 3, stmts: 1, covered: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := readProfiles([]string{a, write("bad.out", "mode: set\nnot a profile line\n")}, "m"); err == nil || !strings.Contains(err.Error(), "bad.out") {
		t.Fatalf("got %v, want an error naming the bad profile", err)
	}
	if _, err := readProfiles([]string{filepath.Join(dir, "missing.out")}, "m"); err == nil {
		t.Fatal("want an error for a missing profile")
	}
}

func TestParseProfileRejectsGarbage(t *testing.T) {
	if _, err := parseProfile(strings.NewReader("mode: set\nnot a profile line\n"), "m"); err == nil {
		t.Fatal("want an error")
	}
}

func TestChangedCoverage(t *testing.T) {
	blocks := []block{
		{file: "pkg/a.go", startLine: 3, endLine: 5, stmts: 2, covered: true},
		{file: "pkg/a.go", startLine: 7, endLine: 8, stmts: 1, covered: false},
		// Two blocks on one line: covered if either ran.
		{file: "pkg/a.go", startLine: 10, endLine: 10, stmts: 1, covered: false},
		{file: "pkg/a.go", startLine: 10, endLine: 10, stmts: 1, covered: true},
		{file: "pkg/b.go", startLine: 1, endLine: 1, stmts: 1, covered: false},
	}
	changed := map[string]map[int]bool{
		"pkg/a.go": {1: true, 4: true, 8: true, 10: true}, // line 1 holds no statement
		"pkg/b.go": {1: true},
	}
	covered, total, missed := changedCoverage(blocks, changed)
	if covered != 2 || total != 4 {
		t.Fatalf("covered %d of %d, want 2 of 4", covered, total)
	}
	if want := []string{"pkg/a.go:8", "pkg/b.go:1"}; !reflect.DeepEqual(missed, want) {
		t.Fatalf("missed %v, want %v", missed, want)
	}
}

func TestMeasured(t *testing.T) {
	for path, want := range map[string]bool{
		"pkg/store/store.go":      true,
		"pkg/store/store_test.go": false,
		"cmd/bearing/main.go":     false,
		"gen/proto/x.pb.go":       false,
		"pkg/api/gen/types.go":    true, // only the root gen/ is excluded
		"pkg/cmd/thing.go":        true,
		"tools/covergate/main.go": false,
		"docs/README.md":          false,
	} {
		if got := measured(path); got != want {
			t.Errorf("measured(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestParseDiffRejectsBadInput(t *testing.T) {
	for _, diff := range []string{
		"+++ b/a.go\n@@ nonsense @@\n",
		"+++ \"b/unterminated\n",
	} {
		if _, err := parseDiff(diff); err == nil {
			t.Errorf("parseDiff(%q) succeeded, want an error", diff)
		}
	}
}

func TestSkipEscapesWorkflowCommands(t *testing.T) {
	var out strings.Builder
	p := &printer{w: &out, actions: true}
	p.skip("base %s not found", "100%\r\n::error::injected")
	want := "::warning title=coverage gate skipped::covergate: base 100%25%0D%0A::error::injected not found\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
	out.Reset()
	p.actions = false
	p.skip("100%% done")
	if out.String() != "covergate: 100% done\n" {
		t.Fatalf("got %q without Actions, want it unescaped", out.String())
	}
}

func TestInert(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"README.md", true},
		{"pkg/model/README.md", true},
		{"docs/spec/data-model.md", true},
		{"docs/adr/diagrams/0001.svg", true},
		{"docs/adr/diagrams/src/gen.py", true},
		{"LICENSE", true},
		{"NOTICE", true},
		{".github/pull_request_template.md", true},
		{".github/reviewers.yml", true},
		{".github/workflows/ci.yml", false},
		{".github/actions/setup/action.yml", false},
		{"docs/testdata/example.md", false},
		{"testdata/README.md", false},
		{"pkg/LICENSE", false},
		{"docsx/a.md", true}, // still Markdown
		{"docsx/a.txt", false},
		{"go.mod", false},
		{"go.sum", false},
		{"go.work", false},
		{"go.work.sum", false},
		{"tools/go.mod", false},
		{"pkg/model/model_test.go", false},
		{"testdata/observations.ndjson", false},
		{"schema/observation.v1.schema.json", false},
		{"Makefile", false},
		{".golangci.yml", false},
	} {
		if got := inert(tc.path); got != tc.want {
			t.Errorf("inert(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

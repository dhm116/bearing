package main

import (
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
`
	got := parseDiff(diff)
	want := map[string]map[int]bool{"pkg/a.go": {4: true, 5: true, 12: true}}
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
		"pkg/api/gen/types.go":    false,
		"tools/covergate/main.go": false,
		"docs/README.md":          false,
	} {
		if got := measured(path); got != want {
			t.Errorf("measured(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestRound1(t *testing.T) {
	if round1(80.04999) != 80.0 || round1(79.96) != 80.0 {
		t.Fatal("round1 rounds to one decimal place")
	}
}

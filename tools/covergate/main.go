// Command covergate is the coverage gate `make check` runs. It fails when
//
//   - less than -min percent of the Go lines changed since the merge base
//     with -base are covered by tests, or
//   - the module's total statement coverage is lower than it is at the merge
//     base.
//
// It reads the coverage profile `go test -coverprofile` wrote for the
// working tree, and measures the merge base by exporting it with git archive
// into a temporary directory and running -test there. Main packages (cmd/),
// generated code (gen/ and files marked "Code generated ... DO NOT EDIT.")
// and test files are left out of both measures.
//
// When the base ref does not exist, or nothing has changed since the merge
// base (a push to main, for example), the gate reports that and passes.
//
// Usage, from the module root:
//
//	go test -coverpkg=./... -coverprofile=cover.out ./...
//	covergate -profile cover.out -base origin/main -min 80
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("covergate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "cover.out", "coverage profile of the working tree")
	base := fs.String("base", "origin/main", "git ref whose merge base with HEAD is the baseline")
	minPct := fs.Float64("min", 80, "minimum percent of changed lines that must be covered")
	test := fs.String("test", "go test -count=1 -coverpkg=./... -coverprofile={profile} ./...",
		"command that writes the baseline's profile to {profile}, run in the exported merge base")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	g := gate{root: ".", stdout: stdout, stderr: stderr}
	ok, err := g.check(*profile, *base, *minPct, *test)
	if err != nil {
		fmt.Fprintf(stderr, "covergate: %v\n", err)
		return 2
	}
	if !ok {
		return 1
	}
	return 0
}

type gate struct {
	root           string
	stdout, stderr io.Writer
}

func (g gate) check(profilePath, baseRef string, minPct float64, testCmd string) (bool, error) {
	if _, err := g.git("rev-parse", "--verify", "--quiet", baseRef+"^{commit}"); err != nil {
		fmt.Fprintf(g.stdout, "covergate: base %s not found; skipping\n", baseRef)
		return true, nil
	}
	mergeBase, err := g.git("merge-base", "HEAD", baseRef)
	if err != nil {
		return false, err
	}
	mergeBase = strings.TrimSpace(mergeBase)

	changed, err := g.changedLines(mergeBase)
	if err != nil {
		return false, err
	}
	if len(changed) == 0 {
		fmt.Fprintf(g.stdout, "covergate: no Go changes since merge base %.12s; skipping\n", mergeBase)
		return true, nil
	}

	module, err := modulePath(filepath.Join(g.root, "go.mod"))
	if err != nil {
		return false, err
	}
	head, err := readProfile(profilePath, module)
	if err != nil {
		return false, err
	}
	head = filterBlocks(head, g.root)
	ok := true

	// Changed lines.
	covered, total, missed := changedCoverage(head, changed)
	if total == 0 {
		fmt.Fprintln(g.stdout, "covergate: changed lines: none are statements; nothing to measure")
	} else {
		pct := percent(covered, total)
		fmt.Fprintf(g.stdout, "covergate: changed lines: %d of %d covered (%.1f%%, minimum %.0f%%)\n", covered, total, pct, minPct)
		if pct < minPct {
			ok = false
			fmt.Fprintln(g.stdout, "covergate: uncovered changed lines:")
			for _, m := range missed {
				fmt.Fprintf(g.stdout, "  %s\n", m)
			}
		}
	}

	// Total against the merge base.
	headPct := totalCoverage(head)
	basePct, err := g.baseCoverage(mergeBase, module, testCmd)
	if err != nil {
		// A broken baseline should not block the change that fixes it.
		fmt.Fprintf(g.stdout, "covergate: total: %.1f%%; baseline unavailable (%v), not compared\n", headPct, err)
		return ok, nil
	}
	// Compare at the precision reported, so float noise can't fail a build.
	h, b := round1(headPct), round1(basePct)
	fmt.Fprintf(g.stdout, "covergate: total: %.1f%% (merge base %.12s: %.1f%%)\n", h, mergeBase, b)
	if h < b {
		ok = false
		fmt.Fprintf(g.stdout, "covergate: total coverage dropped by %.1f points\n", b-h)
	}
	if ok {
		fmt.Fprintln(g.stdout, "covergate: pass")
	} else {
		fmt.Fprintln(g.stdout, "covergate: FAIL")
	}
	return ok, nil
}

// changedLines lists the added or modified lines of each measured Go file
// between mergeBase and the working tree, including untracked files.
func (g gate) changedLines(mergeBase string) (map[string]map[int]bool, error) {
	diff, err := g.git("diff", "--no-color", "--no-ext-diff", "-U0", mergeBase, "--", "*.go")
	if err != nil {
		return nil, err
	}
	changed := parseDiff(diff)
	untracked, err := g.git("ls-files", "--others", "--exclude-standard", "--", "*.go")
	if err != nil {
		return nil, err
	}
	for _, f := range strings.Fields(untracked) {
		b, err := os.ReadFile(filepath.Join(g.root, f))
		if err != nil {
			return nil, err
		}
		lines := map[int]bool{}
		for i := range bytes.Count(b, []byte("\n")) + 1 {
			lines[i+1] = true
		}
		changed[f] = lines
	}
	for f := range changed {
		if !measured(f) || generated(filepath.Join(g.root, f)) {
			delete(changed, f)
		}
	}
	return changed, nil
}

// baseCoverage exports mergeBase, runs testCmd in it and returns its total.
func (g gate) baseCoverage(mergeBase, module, testCmd string) (float64, error) {
	dir, err := os.MkdirTemp("", "covergate-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	archive := exec.Command("git", "archive", "--format=tar", mergeBase)
	archive.Dir = g.root
	tar := exec.Command("tar", "-x", "-C", dir)
	tar.Stdin, err = archive.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := tar.Start(); err != nil {
		return 0, err
	}
	if err := archive.Run(); err != nil {
		return 0, fmt.Errorf("git archive %s: %w", mergeBase, err)
	}
	if err := tar.Wait(); err != nil {
		return 0, fmt.Errorf("extract %s: %w", mergeBase, err)
	}
	profile := filepath.Join(dir, "covergate-base.out")
	fmt.Fprintf(g.stdout, "covergate: measuring merge base %.12s\n", mergeBase)
	cmd := exec.Command("sh", "-c", strings.ReplaceAll(testCmd, "{profile}", profile))
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		_, _ = g.stderr.Write(out.Bytes())
		if _, statErr := os.Stat(profile); statErr != nil {
			return 0, fmt.Errorf("baseline tests: %w", err)
		}
		// go test still writes the profile when tests fail. Failing tests
		// can only lower the baseline, so comparing with it stays fair to
		// this change.
		fmt.Fprintf(g.stdout, "covergate: baseline tests failed (%v); using the coverage they reported\n", err)
	}
	blocks, err := readProfile(profile, module)
	if err != nil {
		return 0, err
	}
	return totalCoverage(filterBlocks(blocks, dir)), nil
}

func (g gate) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// measured reports whether a repo-relative path counts toward coverage.
func measured(path string) bool {
	path = filepath.ToSlash(path)
	if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return false
	}
	for _, dir := range []string{"cmd/", "gen/", "tools/"} {
		if strings.HasPrefix(path, dir) || strings.Contains(path, "/"+dir) {
			return false
		}
	}
	return true
}

var generatedRE = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// generated reports whether the file at path is marked as generated.
func generated(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return generatedRE.Match(b)
}

var hunkRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// parseDiff returns the new-side line numbers of each file in a unified diff.
func parseDiff(diff string) map[string]map[int]bool {
	changed := map[string]map[int]bool{}
	var file string
	for line := range strings.Lines(diff) {
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "+++ "):
			file = ""
			if p, ok := strings.CutPrefix(line, "+++ b/"); ok {
				file = p
			}
		case strings.HasPrefix(line, "@@ ") && file != "":
			m := hunkRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			if changed[file] == nil {
				changed[file] = map[int]bool{}
			}
			for i := range n {
				changed[file][start+i] = true
			}
		}
	}
	return changed
}

// block is one statement block of a coverage profile.
type block struct {
	file               string // repo-relative
	startLine, endLine int
	stmts              int
	covered            bool
}

// readProfile reads a coverage profile, merging the duplicate blocks that
// -coverpkg produces, and makes file names relative to the module root.
func readProfile(path, module string) ([]block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return parseProfile(f, module)
}

func parseProfile(r io.Reader, module string) ([]block, error) {
	type key struct {
		file string
		pos  string
	}
	merged := map[key]*block{}
	var order []key
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if n == 1 && strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		// name.go:line.col,line.col numStmts count
		colon := strings.LastIndex(line, ":")
		fields := strings.Fields(line[colon+1:])
		if colon < 0 || len(fields) != 3 {
			return nil, fmt.Errorf("profile line %d: malformed %q", n, line)
		}
		var b block
		var c1, c2 int
		if _, err := fmt.Sscanf(fields[0], "%d.%d,%d.%d", &b.startLine, &c1, &b.endLine, &c2); err != nil {
			return nil, fmt.Errorf("profile line %d: %w", n, err)
		}
		stmts, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("profile line %d: %w", n, err)
		}
		b.stmts, b.covered = stmts, count > 0
		b.file = strings.TrimPrefix(strings.TrimPrefix(line[:colon], module), "/")
		k := key{b.file, fields[0]}
		if m, ok := merged[k]; ok {
			m.covered = m.covered || b.covered
			continue
		}
		merged[k] = &b
		order = append(order, k)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	blocks := make([]block, 0, len(order))
	for _, k := range order {
		blocks = append(blocks, *merged[k])
	}
	return blocks, nil
}

// filterBlocks drops blocks of files that do not count toward coverage.
func filterBlocks(blocks []block, root string) []block {
	gen := map[string]bool{}
	out := blocks[:0:0]
	for _, b := range blocks {
		if !measured(b.file) {
			continue
		}
		g, ok := gen[b.file]
		if !ok {
			g = generated(filepath.Join(root, b.file))
			gen[b.file] = g
		}
		if !g {
			out = append(out, b)
		}
	}
	return out
}

// changedCoverage counts the changed lines that hold statements and how many
// of them ran. A line is covered when any block that spans it ran.
func changedCoverage(blocks []block, changed map[string]map[int]bool) (covered, total int, missed []string) {
	type state struct{ any, ran bool }
	lines := map[string]map[int]*state{}
	for _, b := range blocks {
		ch := changed[b.file]
		if ch == nil {
			continue
		}
		for l := b.startLine; l <= b.endLine; l++ {
			if !ch[l] {
				continue
			}
			if lines[b.file] == nil {
				lines[b.file] = map[int]*state{}
			}
			s := lines[b.file][l]
			if s == nil {
				s = &state{}
				lines[b.file][l] = s
			}
			s.any = true
			s.ran = s.ran || b.covered
		}
	}
	for file, ls := range lines {
		for l, s := range ls {
			total++
			if s.ran {
				covered++
			} else {
				missed = append(missed, fmt.Sprintf("%s:%d", file, l))
			}
		}
	}
	sort.Slice(missed, func(i, j int) bool {
		fi, li := splitLoc(missed[i])
		fj, lj := splitLoc(missed[j])
		if fi != fj {
			return fi < fj
		}
		return li < lj
	})
	return covered, total, missed
}

func splitLoc(s string) (string, int) {
	i := strings.LastIndex(s, ":")
	n, _ := strconv.Atoi(s[i+1:])
	return s[:i], n
}

// totalCoverage is the percentage of statements that ran.
func totalCoverage(blocks []block) float64 {
	var ran, all int
	for _, b := range blocks {
		all += b.stmts
		if b.covered {
			ran += b.stmts
		}
	}
	return percent(ran, all)
}

func percent(n, of int) float64 {
	if of == 0 {
		return 100
	}
	return 100 * float64(n) / float64(of)
}

func round1(f float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 1, 64), 64)
	return v
}

// modulePath reads the module directive of a go.mod file.
func modulePath(gomod string) (string, error) {
	b, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(b)) {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(p), `"`), nil
		}
	}
	return "", fmt.Errorf("%s: no module directive", gomod)
}

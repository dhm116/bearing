// Command covergate is the coverage gate `make check` runs. It fails when
//
//   - less than -min percent of the Go lines changed since the merge base
//     with -base are covered by tests, or
//   - the module's total statement coverage is more than 0.05 points lower
//     than it is at the merge base.
//
// It reads the coverage profile `go test -coverprofile` wrote for the
// working tree, and measures the merge base by exporting it with git archive
// into a temporary directory and running -test there. Main packages (cmd/),
// generated code (gen/ and files marked "Code generated ... DO NOT EDIT."),
// the tools/ module and test files are left out of both measures.
//
// When nothing has changed since the merge base (a push to main, for
// example) the gate skips. It also skips when the base ref does not exist,
// unless -require-base is set, and skips the total comparison when the
// baseline cannot be measured, unless -require-baseline is set. CI sets both.
// Every skip is reported, as a ::warning:: annotation under GitHub Actions.
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

// tolerance is how far, in percentage points, total coverage may fall below
// the baseline before the gate fails. It absorbs float noise, not real drops.
const tolerance = 0.05

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

// config is what the flags set.
type config struct {
	profile, base, test string
	min                 float64
	requireBase         bool
	requireBaseline     bool
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("covergate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c config
	fs.StringVar(&c.profile, "profile", "cover.out", "coverage profile of the working tree")
	fs.StringVar(&c.base, "base", "origin/main", "git ref whose merge base with HEAD is the baseline")
	fs.Float64Var(&c.min, "min", 80, "minimum percent of changed lines that must be covered")
	fs.StringVar(&c.test, "test", "go test -count=1 -coverpkg=./... -coverprofile={profile} ./...",
		"command that writes the baseline's profile to {profile}, run in the exported merge base")
	fs.BoolVar(&c.requireBase, "require-base", false, "fail, rather than skip, when the base ref is missing")
	fs.BoolVar(&c.requireBaseline, "require-baseline", false, "fail, rather than skip the total comparison, when the baseline can't be measured")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	out := &printer{w: stdout, actions: getenv("GITHUB_ACTIONS") == "true"}
	g := gate{root: ".", out: out, stderr: stderr}
	ok, err := g.check(c)
	if err != nil {
		// Exit status 2 reports the failure even if stderr is gone.
		_, _ = fmt.Fprintf(stderr, "covergate: %v\n", err)
		return 2
	}
	if out.err != nil {
		return 2
	}
	if !ok {
		return 1
	}
	return 0
}

// printer writes the report and remembers the first write error, so callers
// need not check each line.
type printer struct {
	w       io.Writer
	actions bool // running under GitHub Actions
	err     error
}

func (p *printer) printf(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}

// skip reports that part of the gate did not run, as an annotation under
// GitHub Actions so it shows on the pull request.
func (p *printer) skip(format string, args ...any) {
	msg := "covergate: " + fmt.Sprintf(format, args...)
	if p.actions {
		msg = "::warning title=coverage gate skipped::" + escapeData(msg)
	}
	p.printf("%s\n", msg)
}

// escapeData escapes a workflow command's message, so text such as a git
// error can't end the annotation early or inject another command.
var escapeData = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace

type gate struct {
	root   string
	out    *printer
	stderr io.Writer // baseline test output, when it fails
}

func (g gate) check(c config) (bool, error) {
	if _, err := g.git("rev-parse", "--verify", "--quiet", c.base+"^{commit}"); err != nil {
		if c.requireBase {
			return false, fmt.Errorf("base %s not found; fetch it (actions/checkout needs fetch-depth: 0)", c.base)
		}
		g.out.skip("base %s not found; skipping", c.base)
		return true, nil
	}
	mergeBase, err := g.git("merge-base", "HEAD", c.base)
	if err != nil {
		return false, err
	}
	mergeBase = strings.TrimSpace(mergeBase)

	changed, err := g.changedLines(mergeBase)
	if err != nil {
		return false, err
	}
	if len(changed) == 0 {
		g.out.skip("no measured Go changes since merge base %.12s; skipping", mergeBase)
		return true, nil
	}

	module, err := modulePath(filepath.Join(g.root, "go.mod"))
	if err != nil {
		return false, err
	}
	profile := c.profile
	if !filepath.IsAbs(profile) {
		profile = filepath.Join(g.root, profile)
	}
	head, err := readProfile(profile, module)
	if err != nil {
		return false, err
	}
	head = filterBlocks(head, g.root)
	ok := true

	// Changed lines.
	covered, total, missed := changedCoverage(head, changed)
	if total == 0 {
		g.out.printf("covergate: changed lines: none are statements; nothing to measure\n")
	} else {
		pct := percent(covered, total)
		g.out.printf("covergate: changed lines: %d of %d covered (%.1f%%, minimum %.0f%%)\n", covered, total, pct, c.min)
		if pct < c.min {
			ok = false
			g.out.printf("covergate: uncovered changed lines:\n")
			for _, m := range missed {
				g.out.printf("  %s\n", m)
			}
		}
	}

	// Total against the merge base.
	headPct := totalCoverage(head)
	basePct, err := g.baseCoverage(mergeBase, module, c.test)
	switch {
	case err != nil && c.requireBaseline:
		return false, fmt.Errorf("measure merge base %.12s: %w", mergeBase, err)
	case err != nil:
		// A broken baseline should not block the change that fixes it.
		g.out.printf("covergate: total: %.2f%%\n", headPct)
		g.out.skip("baseline unavailable (%v); total not compared", err)
	default:
		g.out.printf("covergate: total: %.2f%% (merge base %.12s: %.2f%%)\n", headPct, mergeBase, basePct)
		if headPct < basePct-tolerance {
			ok = false
			g.out.printf("covergate: total coverage dropped by %.2f points\n", basePct-headPct)
		}
	}
	if ok {
		g.out.printf("covergate: pass\n")
	} else {
		g.out.printf("covergate: FAIL\n")
	}
	return ok, nil
}

// changedLines lists the added or modified lines of each measured Go file
// between mergeBase and the working tree, including untracked files.
func (g gate) changedLines(mergeBase string) (map[string]map[int]bool, error) {
	// Fixed prefixes and repo-root paths whatever the user's git config
	// says (diff.noprefix, diff.relative, external diff drivers, color).
	diff, err := g.git("diff", "--no-color", "--no-ext-diff", "--no-relative",
		"--src-prefix=a/", "--dst-prefix=b/", "-U0", mergeBase, "--", "*.go")
	if err != nil {
		return nil, err
	}
	changed, err := parseDiff(diff)
	if err != nil {
		return nil, err
	}
	untracked, err := g.git("ls-files", "-z", "--others", "--exclude-standard", "--", "*.go")
	if err != nil {
		return nil, err
	}
	for f := range strings.SplitSeq(untracked, "\x00") {
		if f == "" {
			continue
		}
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
	g.out.printf("covergate: measuring merge base %.12s\n", mergeBase)
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
		g.out.printf("covergate: baseline tests failed (%v); using the coverage they reported\n", err)
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
		if strings.HasPrefix(path, dir) {
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

// parseDiff returns the new-side line numbers of each file in a unified diff
// made with the a/ and b/ prefixes.
func parseDiff(diff string) (map[string]map[int]bool, error) {
	changed := map[string]map[int]bool{}
	var file string
	for line := range strings.Lines(diff) {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "+++ "):
			name, err := diffPath(strings.TrimPrefix(line, "+++ "))
			if err != nil {
				return nil, err
			}
			file = ""
			if p, ok := strings.CutPrefix(name, "b/"); ok {
				file = p
			}
		case strings.HasPrefix(line, "@@ ") && file != "":
			m := hunkRE.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("malformed hunk header %q", line)
			}
			start, err := strconv.Atoi(m[1])
			if err != nil {
				return nil, err
			}
			n := 1
			if m[2] != "" {
				if n, err = strconv.Atoi(m[2]); err != nil {
					return nil, err
				}
			}
			if changed[file] == nil {
				changed[file] = map[int]bool{}
			}
			for i := range n {
				changed[file][start+i] = true
			}
		}
	}
	return changed, nil
}

// diffPath decodes a path from a diff header. git C-quotes paths with
// unusual bytes ("b/caf\303\251.go"), using escapes Go's own string literals
// share.
func diffPath(s string) (string, error) {
	if !strings.HasPrefix(s, `"`) {
		return s, nil
	}
	p, err := strconv.Unquote(s)
	if err != nil {
		return "", fmt.Errorf("decode diff path %s: %w", s, err)
	}
	return p, nil
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
	type loc struct {
		file string
		line int
	}
	ran := map[loc]bool{}
	for _, b := range blocks {
		ch := changed[b.file]
		for l := b.startLine; l <= b.endLine; l++ {
			if ch[l] {
				k := loc{b.file, l}
				ran[k] = ran[k] || b.covered
			}
		}
	}
	locs := make([]loc, 0, len(ran))
	for k := range ran {
		locs = append(locs, k)
	}
	sort.Slice(locs, func(i, j int) bool {
		if locs[i].file != locs[j].file {
			return locs[i].file < locs[j].file
		}
		return locs[i].line < locs[j].line
	})
	for _, k := range locs {
		total++
		if ran[k] {
			covered++
		} else {
			missed = append(missed, fmt.Sprintf("%s:%d", k.file, k.line))
		}
	}
	return covered, total, missed
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

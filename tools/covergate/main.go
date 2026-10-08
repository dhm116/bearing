// Command covergate is the coverage gate `make check` runs. It fails when
//
//   - less than -min percent of the Go lines changed since the merge base
//     with -base are covered by tests, or
//   - the module's total statement coverage is more than 0.05 points lower
//     than it is at the merge base.
//
// It reads the coverage profile `go test -coverprofile` wrote for the
// working tree, and measures the merge base by exporting it with git archive
// into a temporary directory and running -test there. That run costs as much
// as the head's, so CI does it in a job of its own, in parallel with the
// head's tests: -measure-base writes the merge base's profile, and -base-profile
// hands it to the gate instead of measuring in the gate. Main packages (cmd/),
// generated code (gen/ and files marked "Code generated ... DO NOT EDIT."),
// the tools/ module and test files are left out of both measures.
//
// When no measured Go line has changed, the changed-lines check has nothing
// to measure, but the total comparison still runs: deleting or weakening
// tests, or changing testdata, go.mod or a schema, can lower coverage
// without touching a measured line. Only when every changed path is in
// inertPaths (documentation, licenses, and .github outside its workflows
// and actions, but never testdata), or nothing changed at all (a push to
// main), does the gate skip. Skipping saves the baseline test run; `make
// cover` has already measured the head.
//
// The gate also skips when the base ref does not exist, unless
// -require-base is set, and skips the total comparison when the baseline
// cannot be measured, unless -require-baseline is set. CI sets both. Every
// skip is reported, as a ::warning:: annotation under GitHub Actions.
//
// The gate can't defend against edits to the CI workflow itself, because CI
// runs the pull request's own workflow.
//
// Usage, from the module root:
//
//	go test -coverpkg=./... -coverprofile=cover.out ./...
//	covergate -profile cover.out -base origin/main -min 80
//
// or, with the baseline measured elsewhere (when the gate would skip,
// -measure-base writes nothing), and the tests run in shards that each wrote
// a profile (a block is covered if any shard covers it):
//
//	covergate -base origin/main -measure-base base-1.out -test '...'
//	covergate -base origin/main -measure-base base-2.out -test '...'
//	covergate -profile cover-1.out,cover-2.out -base origin/main -base-profile base-1.out,base-2.out
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
	"path"
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
	baseProfile         string // a merge-base profile measured earlier, instead of measuring here
	measureBase         string // write the merge-base profile here and do nothing else
	min                 float64
	requireBase         bool
	requireBaseline     bool
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("covergate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c config
	fs.StringVar(&c.profile, "profile", "cover.out", "coverage profile of the working tree; several, comma-separated, are merged")
	fs.StringVar(&c.base, "base", "origin/main", "git ref whose merge base with HEAD is the baseline")
	fs.Float64Var(&c.min, "min", 80, "minimum percent of changed lines that must be covered")
	fs.StringVar(&c.test, "test", "go test -count=1 -coverpkg=./... -coverprofile={profile} ./...",
		"command that writes the baseline's profile to {profile}, run in the exported merge base")
	fs.StringVar(&c.baseProfile, "base-profile", "", "merge-base profiles that -measure-base wrote (comma-separated if several), used instead of running -test")
	fs.StringVar(&c.measureBase, "measure-base", "", "run -test at the merge base, write its profile to this file, and skip the gate")
	fs.BoolVar(&c.requireBase, "require-base", false, "fail, rather than skip, when the base ref is missing")
	fs.BoolVar(&c.requireBaseline, "require-baseline", false, "fail, rather than skip the total comparison, when the baseline can't be measured")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	out := &printer{w: stdout, actions: getenv("GITHUB_ACTIONS") == "true"}
	g := gate{root: ".", out: out, stderr: stderr}
	if c.measureBase != "" && c.baseProfile != "" {
		_, _ = fmt.Fprintln(stderr, "covergate: -measure-base and -base-profile are exclusive")
		return 2
	}
	var ok bool
	var err error
	if c.measureBase != "" {
		ok, err = true, g.measure(c)
	} else {
		ok, err = g.check(c)
	}
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

// plan says what the gate has to look at: the merge base with c.base, the
// lines changed since it, and whether the gate should skip because there is
// no base or nothing but inert paths changed.
type plan struct {
	mergeBase string
	changed   map[string]map[int]bool
	skip      bool
}

func (g gate) plan(c config) (plan, error) {
	if _, err := g.git("rev-parse", "--verify", "--quiet", c.base+"^{commit}"); err != nil {
		if c.requireBase {
			return plan{}, fmt.Errorf("base %s not found; fetch it (actions/checkout needs fetch-depth: 0)", c.base)
		}
		g.out.skip("base %s not found; skipping", c.base)
		return plan{skip: true}, nil
	}
	mergeBase, err := g.git("merge-base", "HEAD", c.base)
	if err != nil {
		return plan{}, err
	}
	mergeBase = strings.TrimSpace(mergeBase)

	changed, err := g.changedLines(mergeBase)
	if err != nil {
		return plan{}, err
	}
	if len(changed) == 0 {
		relevant, err := g.anyTestRelevantChange(mergeBase)
		if err != nil {
			return plan{}, err
		}
		if !relevant {
			g.out.skip("no changes outside documentation and other inert paths since merge base %.12s; skipping", mergeBase)
			return plan{mergeBase: mergeBase, skip: true}, nil
		}
	}
	return plan{mergeBase: mergeBase, changed: changed}, nil
}

func (g gate) check(c config) (bool, error) {
	pl, err := g.plan(c)
	if err != nil || pl.skip {
		return err == nil, err
	}
	mergeBase, changed := pl.mergeBase, pl.changed

	module, err := modulePath(filepath.Join(g.root, "go.mod"))
	if err != nil {
		return false, err
	}
	var profiles []string
	for _, p := range splitList(c.profile) {
		if !filepath.IsAbs(p) {
			p = filepath.Join(g.root, p)
		}
		profiles = append(profiles, p)
	}
	if len(profiles) == 0 {
		return false, errors.New("no coverage profile given")
	}
	head, err := readProfiles(profiles, module)
	if err != nil {
		return false, err
	}
	head = filterBlocks(head, g.root)
	ok := true

	// Changed lines.
	covered, total, missed := changedCoverage(head, changed)
	switch {
	case len(changed) == 0:
		g.out.printf("covergate: changed lines: no measured Go lines changed; nothing to measure\n")
	case total == 0:
		g.out.printf("covergate: changed lines: none are statements; nothing to measure\n")
	default:
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
	basePct, err := g.baseCoverage(mergeBase, module, c)
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

// inertPaths are the paths whose changes cannot affect test results, as
// globs in which "**" matches any number of directories. A change touching
// only these paths skips the gate. Anything else, testdata and schemas
// included, runs the total comparison. A path matching inertExceptions is
// never inert.
var (
	inertPaths      = []string{"**/*.md", "docs/**", "LICENSE", "NOTICE", ".github/**"}
	inertExceptions = []string{".github/workflows/**", ".github/actions/**", "**/testdata/**"}
)

// inert reports whether a repo-relative, slash-separated path is in
// inertPaths.
func inert(p string) bool {
	matchAny := func(globs []string) bool {
		for _, g := range globs {
			if globMatch(strings.Split(g, "/"), strings.Split(p, "/")) {
				return true
			}
		}
		return false
	}
	return matchAny(inertPaths) && !matchAny(inertExceptions)
}

// globMatch matches path segments against pattern segments, where a "**"
// segment matches zero or more path segments and any other segment is a
// path.Match pattern.
func globMatch(pattern, segs []string) bool {
	if len(pattern) == 0 {
		return len(segs) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if globMatch(pattern[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], segs[0])
	return err == nil && ok && globMatch(pattern[1:], segs[1:])
}

// anyTestRelevantChange reports whether any path outside inertPaths differs
// between mergeBase and the working tree, counting deleted and untracked
// files. Renames count as a deletion and an addition, so moving a file
// into docs/ still counts.
func (g gate) anyTestRelevantChange(mergeBase string) (bool, error) {
	diff, err := g.git("diff", "--no-ext-diff", "--no-relative", "--no-renames", "--name-only", "-z", mergeBase)
	if err != nil {
		return false, err
	}
	untracked, err := g.git("ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return false, err
	}
	for f := range strings.SplitSeq(diff+"\x00"+untracked, "\x00") {
		if f != "" && !inert(f) {
			return true, nil
		}
	}
	return false, nil
}

// goFiles is the pathspec for Go files in any directory, explicit so that it
// doesn't depend on git's default wildcard matching across slashes.
const goFiles = ":(glob)**/*.go"

// changedLines lists the added or modified lines of each measured Go file
// between mergeBase and the working tree, including untracked files.
func (g gate) changedLines(mergeBase string) (map[string]map[int]bool, error) {
	// Fixed prefixes and repo-root paths whatever the user's git config
	// says (diff.noprefix, diff.relative, external diff drivers, color).
	diff, err := g.git("diff", "--no-color", "--no-ext-diff", "--no-relative",
		"--src-prefix=a/", "--dst-prefix=b/", "-U0", mergeBase, "--", goFiles)
	if err != nil {
		return nil, err
	}
	changed, err := parseDiff(diff)
	if err != nil {
		return nil, err
	}
	untracked, err := g.git("ls-files", "-z", "--others", "--exclude-standard", "--", goFiles)
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

// export extracts mergeBase into a new temporary directory, which the caller
// removes.
func (g gate) export(mergeBase string) (string, error) {
	dir, err := os.MkdirTemp("", "covergate-")
	if err != nil {
		return "", err
	}
	archive := exec.Command("git", "archive", "--format=tar", mergeBase)
	archive.Dir = g.root
	tar := exec.Command("tar", "-x", "-C", dir)
	tar.Stdin, err = archive.StdoutPipe()
	if err == nil {
		err = tar.Start()
	}
	if err == nil {
		if err = archive.Run(); err != nil {
			err = fmt.Errorf("git archive %s: %w", mergeBase, err)
		}
		if werr := tar.Wait(); err == nil && werr != nil {
			err = fmt.Errorf("extract %s: %w", mergeBase, werr)
		}
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// runBaseline runs testCmd in dir, an exported merge base, and returns the
// profile it wrote.
func (g gate) runBaseline(dir, mergeBase, testCmd string) (string, error) {
	profile := filepath.Join(dir, "covergate-base.out")
	g.out.printf("covergate: measuring merge base %.12s\n", mergeBase)
	cmd := exec.Command("sh", "-c", strings.ReplaceAll(testCmd, "{profile}", profile))
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		_, _ = g.stderr.Write(out.Bytes())
		if _, statErr := os.Stat(profile); statErr != nil {
			return "", fmt.Errorf("baseline tests: %w", err)
		}
		// go test still writes the profile when tests fail. Failing tests
		// can only lower the baseline, so comparing with it stays fair to
		// this change.
		g.out.printf("covergate: baseline tests failed (%v); using the coverage they reported\n", err)
	}
	return profile, nil
}

// mergeBaseSuffix names the file beside a -measure-base profile that holds
// the commit it measured, so the gate can refuse a profile of another one.
const mergeBaseSuffix = ".mergebase"

// measure is -measure-base: it runs the tests at the merge base and writes
// their profile to c.measureBase, unless the gate would skip, in which case
// it writes nothing and -base-profile is not needed either.
func (g gate) measure(c config) error {
	pl, err := g.plan(c)
	if err != nil || pl.skip {
		return err
	}
	dir, err := g.export(pl.mergeBase)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	profile, err := g.runBaseline(dir, pl.mergeBase, c.test)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(profile)
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.measureBase, b, 0o600); err != nil { //nolint:gosec // G703: the flag names the file to write, as for any command line tool
		return err
	}
	return os.WriteFile(c.measureBase+mergeBaseSuffix, []byte(pl.mergeBase+"\n"), 0o600)
}

// baseCoverage returns the total coverage at mergeBase: from the profile
// -measure-base wrote when c.baseProfile is set, else by exporting mergeBase
// and running c.test in it.
func (g gate) baseCoverage(mergeBase, module string, c config) (float64, error) {
	dir, err := g.export(mergeBase)
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	profiles := splitList(c.baseProfile)
	if len(profiles) > 0 {
		for _, profile := range profiles {
			got, err := os.ReadFile(profile + mergeBaseSuffix)
			if err != nil {
				return 0, fmt.Errorf("base profile: %w", err)
			}
			if strings.TrimSpace(string(got)) != mergeBase {
				return 0, fmt.Errorf("base profile %s was measured at %.12s, not at merge base %.12s", profile, strings.TrimSpace(string(got)), mergeBase)
			}
		}
	} else {
		profile, err := g.runBaseline(dir, mergeBase, c.test)
		if err != nil {
			return 0, err
		}
		profiles = []string{profile}
	}
	blocks, err := readProfiles(profiles, module)
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

// splitList splits a comma-separated list of files, ignoring empty items.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readProfiles reads coverage profiles, as one run of the tests would write
// them, or several runs of parts of it (CI shards the tests): a block is
// covered if any profile covers it. File names are made relative to the
// module root.
func readProfiles(paths []string, module string) ([]block, error) {
	m := newProfile()
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		err = m.add(f, module)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return m.blocks(), nil
}

// parseProfile reads one coverage profile, merging the duplicate blocks that
// -coverpkg produces.
func parseProfile(r io.Reader, module string) ([]block, error) {
	m := newProfile()
	if err := m.add(r, module); err != nil {
		return nil, err
	}
	return m.blocks(), nil
}

// profile accumulates blocks from coverage profiles; a block that appears
// more than once is covered if any copy is.
type profile struct {
	merged map[profileKey]*block
	order  []profileKey
}

type profileKey struct {
	file string
	pos  string
}

func newProfile() *profile { return &profile{merged: map[profileKey]*block{}} }

func (p *profile) blocks() []block {
	blocks := make([]block, 0, len(p.order))
	for _, k := range p.order {
		blocks = append(blocks, *p.merged[k])
	}
	return blocks
}

// add reads one profile into p.
func (p *profile) add(r io.Reader, module string) error {
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
			return fmt.Errorf("profile line %d: malformed %q", n, line)
		}
		var b block
		var c1, c2 int
		if _, err := fmt.Sscanf(fields[0], "%d.%d,%d.%d", &b.startLine, &c1, &b.endLine, &c2); err != nil {
			return fmt.Errorf("profile line %d: %w", n, err)
		}
		stmts, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err := errors.Join(err1, err2); err != nil {
			return fmt.Errorf("profile line %d: %w", n, err)
		}
		b.stmts, b.covered = stmts, count > 0
		b.file = strings.TrimPrefix(strings.TrimPrefix(line[:colon], module), "/")
		k := profileKey{b.file, fields[0]}
		if m, ok := p.merged[k]; ok {
			m.covered = m.covered || b.covered
			continue
		}
		p.merged[k] = &b
		p.order = append(p.order, k)
	}
	return sc.Err()
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

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// buildRepoSite builds the real site from this repository without GitHub.
func buildRepoSite(t *testing.T, base string) (*Builder, string) {
	t.Helper()
	cfg, err := LoadConfig("site.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rf, err := LoadRoadmap("roadmap.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "_site")
	b := &Builder{
		Root: "..", SiteDir: ".", Out: out, Base: base, Config: cfg,
		Roadmap: BuildRoadmap(rf, nil),
		Build:   BuildInfo{Commit: "0123456789abcdef", Time: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)},
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, out
}

func TestRepoSiteBuildsWithoutProblems(t *testing.T) {
	b, out := buildRepoSite(t, "/bearing/")
	for _, p := range b.Problems() {
		t.Errorf("problem: %s", p)
	}
	for _, page := range []string{
		"index.html", "how-it-works/index.html", "roadmap/index.html", "404.html",
		"spec/index.html", "spec/data-model/index.html", "search.json", ".nojekyll",
	} {
		if _, err := os.Stat(filepath.Join(out, page)); err != nil {
			t.Errorf("got no %s: %v", page, err)
		}
	}
	home, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(home), `href="/bearing/spec/data-model/"`) {
		t.Error("got home links without the /bearing/ base")
	}
}

// The hand-written pages are for readers new to Bearing. They describe the
// finished product, so they never mention milestones or decision records,
// and they avoid jargon the project has agreed to drop.
func TestHandWrittenPagesStayInPlainTerms(t *testing.T) {
	_, out := buildRepoSite(t, "/")
	// Checked against the text readers see, with tags removed (SVG path data
	// is full of things like "M4").
	inText := map[string]*regexp.Regexp{
		"a milestone":          regexp.MustCompile(`\bM\d+\b|(?i)milestone`),
		"a decision record":    regexp.MustCompile(`\bADRs?\b|(?i)decision record`),
		"the word \"surface\"": regexp.MustCompile(`(?i)\bsurfaces?\b`),
	}
	// Checked against the markup.
	inMarkup := map[string]*regexp.Regexp{
		"a link to a decision record": regexp.MustCompile(`href="[^"]*(?:/adr/|decisions/)`),
		// Diagrams animate classed elements with CSS transforms, which
		// replace an SVG transform attribute on the same element and move
		// it to 0,0. So position with a transform on an unclassed wrapper.
		"a transform on a classed element": regexp.MustCompile(`<[^>]*class="[^"]*"[^>]*\stransform=|<[^>]*\stransform="[^"]*"[^>]*class=`),
	}
	tags := regexp.MustCompile(`<[^>]*>`)
	// Attribute text people also read: screen readers, tooltips, images.
	attrs := regexp.MustCompile(`\s(?:aria-label|title|alt|placeholder)="([^"]*)"`)
	for _, page := range []string{"index.html", "how-it-works/index.html", "roadmap/index.html", "404.html"} {
		raw, err := os.ReadFile(filepath.Join(out, page))
		if err != nil {
			t.Fatal(err)
		}
		text := tags.ReplaceAll(raw, []byte(" "))
		for _, m := range attrs.FindAllSubmatch(raw, -1) {
			text = append(append(text, ' '), m[1]...)
		}
		for what, re := range inText {
			if m := re.Find(text); m != nil {
				t.Errorf("%s: got %s (%q)", page, what, m)
			}
		}
		for what, re := range inMarkup {
			if m := re.Find(raw); m != nil {
				t.Errorf("%s: got %s (%q)", page, what, m)
			}
		}
	}
}

func TestRepoSiteSearchIndexCoversSpecAndRoadmap(t *testing.T) {
	_, out := buildRepoSite(t, "/")
	raw, err := os.ReadFile(filepath.Join(out, "search.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []searchEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	var spec, roadmap bool
	for _, e := range entries {
		spec = spec || strings.HasPrefix(e.URL, "/spec/data-model/#")
		roadmap = roadmap || strings.HasPrefix(e.Section, "Roadmap")
	}
	if !spec || !roadmap {
		t.Fatalf("got spec headings %v and roadmap features %v, want both", spec, roadmap)
	}
}

// fixtureRepo writes files into a temporary repository with the brand mark
// the build needs, and returns its root.
func fixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files["docs/brand/bearing-mark.svg"] = "<svg/>"
	files["docs/brand/bearing-mark-animated.svg"] = "<svg/>"
	if _, ok := files["docs/adr/README.md"]; !ok {
		files["docs/adr/README.md"] = "# Decisions\n"
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// buildFixture builds the docs collections of a fixture repository with
// the real templates and no hand-written pages.
func buildFixture(t *testing.T, root string) (*Builder, string) {
	t.Helper()
	cfg := &Config{
		Title: "T", Repo: "o/r", Branch: "main", Brand: "docs/brand",
		// The 404 template stands in for a home page, which every page links to.
		Pages: []PageSpec{{Template: "404", Path: "", Title: "Home"}},
		Collections: []Collection{
			{ID: "spec", Title: "Specification", Short: "Spec", Dir: "docs/spec", Path: "spec", Order: []string{"b.md"}},
			{ID: "decisions", Title: "Decisions", Short: "Decisions", Dir: "docs/adr", Path: "decisions"},
		},
		Nav: []Link{{Label: "Spec", Href: "spec/"}, {Label: "Elsewhere", Href: "https://example.com/x"}},
	}
	out := filepath.Join(t.TempDir(), "_site")
	b := &Builder{
		Root: root, SiteDir: ".", Out: out, Base: "/b/", Config: cfg,
		Roadmap: BuildRoadmap(&RoadmapFile{}, nil),
		Build:   BuildInfo{Commit: "0123456789abcdef", Time: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)},
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, out
}

func TestBuildReportsBrokenLinksAndAnchors(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		"docs/spec/a.md": "# A\n\n[ok](b.md#real), [title](b.md#b), [bad anchor](b.md#nope), " +
			"[missing](gone.md), ![image](missing.png), [root](/docs/spec/b.md#real)\n",
		"docs/spec/b.md": "# B\n\n## Real\n\nText.\n",
	})
	b, out := buildFixture(t, root)
	page, err := os.ReadFile(filepath.Join(out, "spec", "b", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`<h1 id="b">`, `href="https://example.com/x"`} {
		if !strings.Contains(string(page), w) {
			t.Errorf("got spec/b/ without %s", w)
		}
	}
	// The external nav link is used as is, so it adds no problem.
	want := []string{
		"/spec/a/: link to /spec/b/#nope, which has no such heading",
		"docs/spec/a.md: link gone.md points at docs/spec/gone.md, which does not exist",
		"docs/spec/a.md: image missing.png does not exist",
	}
	got := strings.Join(b.Problems(), "\n")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("got problems:\n%s\nwant one containing %q", got, w)
		}
	}
	if len(b.Problems()) != len(want) {
		t.Errorf("got %d problems, want %d:\n%s", len(b.Problems()), len(want), got)
	}
}

func TestBuildReadsDocMetadataAndCopiesImages(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		"docs/spec/README.md":   "# Spec\n\n- [B](b.md): The b document.\n- [A](a.md): The a document.\n",
		"docs/spec/a.md":        "# A doc\n\nVersion 0.2 (draft). Lede of a.\n\n## Part\n\nFirst words of the part.\n",
		"docs/spec/b.md":        "# B doc\n\n![map](img/map.svg)\n",
		"docs/spec/img/map.svg": "<svg/>",
		"docs/adr/0001-first.md": "# 1. First choice\n\nDate: 2026-09-28 · Status: accepted · Superseded in part by [ADR 2](0002-second.md)\n\n" +
			"## Context\n\nWhy.\n\n## Decision\n\nWe chose the first.\n",
		"docs/adr/0002-second.md": "# 2. Second\n\nDate: 2026-09-29 · Status: proposed\n\n## Decision\n\nAnd the second.\n",
	})
	b, out := buildFixture(t, root)
	if p := b.Problems(); len(p) != 0 {
		t.Fatalf("got problems %v", p)
	}
	a, adr := b.docs["docs/spec/a.md"], b.docs["docs/adr/0001-first.md"]
	checks := []struct{ name, got, want string }{
		{"ADR status", adr.Status, "accepted"},
		{"ADR status note", adr.StatusNote, "Superseded in part by ADR 2"},
		{"ADR date", adr.Date, "2026-09-28"},
		{"ADR summary from its decision", adr.Summary, "We chose the first."},
		{"spec version", a.Version, "0.2"},
		{"spec summary from the README", a.Summary, "The a document."},
		{"TOC snippet", a.TOC[0].Snippet, "First words of the part."},
		{"order: b.md first, then a.md", b.docs["docs/spec/b.md"].Next.Path, "spec/a/"},
		{"prev of a.md", a.Prev.Path, "spec/b/"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "assets", "docs", "spec", "img", "map.svg")); err != nil {
		t.Errorf("got no copied image: %v", err)
	}
}

func TestResolveLinkMapsRepoFiles(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		"docs/spec/README.md":     "# Spec\n",
		"docs/spec/data-model.md": "# Data model\n\n## Terms\n",
		"pkg/model/model.go":      "package model\n",
		"LICENSE":                 "Apache-2.0\n",
	})
	b, _ := buildFixture(t, root)
	cases := map[string]string{
		"data-model.md#terms":      "/b/spec/data-model/#terms",
		"/docs/spec/data-model.md": "/b/spec/data-model/",
		"../../pkg/model":          "https://github.com/o/r/tree/main/pkg/model",
		"../../LICENSE":            "https://github.com/o/r/blob/main/LICENSE",
		"https://example.com/":     "https://example.com/",
		"../../../outside.md":      "../../../outside.md",
		"no-such-file.md":          "no-such-file.md",
	}
	for dest, want := range cases {
		t.Run(dest, func(t *testing.T) {
			if got := b.ResolveLink("docs/spec/README.md", dest, false); got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
	got := strings.Join(b.Problems(), "\n")
	for _, w := range []string{"link ../../../outside.md leaves the repository", "link no-such-file.md points at docs/spec/no-such-file.md"} {
		if !strings.Contains(got, w) {
			t.Errorf("got problems:\n%s\nwant one containing %q", got, w)
		}
	}
}

func TestSnippetQuotesTheFirstCodeBlockUnderAHeading(t *testing.T) {
	root := t.TempDir()
	doc := "# Doc\n\n```sh\n# a comment, not a heading\necho skip\n```\n\n## Example\n\nText.\n\n```json\n{\"a\": 1}\n```\n\n```json\n{\"b\": 2}\n```\n"
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &Builder{Root: root}
	t.Cleanup(func() { _ = b.Close() })
	got, err := b.snippet("doc.md", "example")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"a": 1}` {
		t.Fatalf("got %q, want %q", got, `{"a": 1}`)
	}
	if _, err := b.snippet("doc.md", "a-comment-not-a-heading"); err == nil {
		t.Fatal("got a snippet for a comment inside a code block")
	}
}

func TestPrepareOutRefusesSourceDirectories(t *testing.T) {
	repo := t.TempDir()
	site := filepath.Join(repo, "site")
	for _, out := range []string{repo, site, filepath.Dir(repo)} {
		b := &Builder{Root: repo, SiteDir: site, Out: out}
		if err := b.prepareOut(); err == nil {
			t.Errorf("got nil error clearing %s, want a refusal", out)
		}
	}
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "spec.md"), []byte("# Spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &Builder{Root: repo, SiteDir: site, Out: docs}
	if err := b.prepareOut(); err == nil {
		t.Error("got nil error clearing docs/, want a refusal for a directory no build wrote")
	}
	if _, err := os.Stat(filepath.Join(docs, "spec.md")); err != nil {
		t.Fatalf("got docs/spec.md removed: %v", err)
	}

	out := filepath.Join(site, "_site")
	b = &Builder{Root: repo, SiteDir: site, Out: out}
	if err := b.prepareOut(); err != nil {
		t.Fatalf("got %v clearing a new site/_site, want nil", err)
	}
	for _, f := range []string{".nojekyll", "old.html"} {
		if err := os.WriteFile(filepath.Join(out, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.prepareOut(); err != nil {
		t.Fatalf("got %v clearing an earlier build, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(out, "old.html")); err == nil {
		t.Fatal("got old.html left behind, want the earlier build cleared")
	}
}

func TestRepoReadsStayInsideTheRepository(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.svg"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret.svg"), filepath.Join(repo, "map.svg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	out := t.TempDir()
	b := &Builder{Root: repo, Out: out, Base: "/", assets: map[string]bool{}, static: map[string]string{}}
	t.Cleanup(func() { _ = b.Close() })
	got := b.ResolveLink("README.md", "map.svg", true)
	if got != "map.svg" || len(b.problems) != 1 {
		t.Fatalf("got %q and problems %v, want the link refused", got, b.problems)
	}
	if _, err := os.Stat(filepath.Join(out, "assets", "map.svg")); err == nil {
		t.Fatal("got the file outside the repository copied into the site")
	}
}

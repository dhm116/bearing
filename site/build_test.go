package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
		Roadmap: BuildRoadmap(rf, nil, cfg.Repo),
		Build:   BuildInfo{Commit: "0123456789abcdef", Time: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	return b, out
}

func TestRepoSiteBuildsWithoutProblems(t *testing.T) {
	b, out := buildRepoSite(t, "/bearing/")
	for _, p := range b.Problems() {
		t.Errorf("problem: %s", p)
	}
	for _, page := range []string{
		"index.html", "how-it-works/index.html", "roadmap/index.html", "404.html",
		"spec/index.html", "spec/data-model/index.html", "decisions/index.html",
		"search.json", ".nojekyll",
	} {
		if _, err := os.Stat(filepath.Join(out, page)); err != nil {
			t.Errorf("got no %s: %v", page, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "decisions/template/index.html")); err == nil {
		t.Error("got a page for the excluded ADR template")
	}
	home, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(home), `href="/bearing/spec/data-model/"`) {
		t.Error("got home links without the /bearing/ base")
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

func TestResolveLinkReportsMissingTargets(t *testing.T) {
	b, _ := buildRepoSite(t, "/")
	b.problems = nil
	got := b.ResolveLink("docs/spec/README.md", "no-such-file.md", false)
	if got != "no-such-file.md" || len(b.problems) != 1 {
		t.Fatalf("got %q and problems %v, want the link unchanged and one problem", got, b.problems)
	}
	got = b.ResolveLink("docs/spec/README.md", "../../../outside.md", false)
	if len(b.problems) != 2 || !strings.Contains(b.problems[1], "leaves the repository") {
		t.Fatalf("got %q and problems %v", got, b.problems)
	}
}

func TestResolveLinkMapsRepoFiles(t *testing.T) {
	b, _ := buildRepoSite(t, "/b/")
	cases := map[string]string{
		"data-model.md#terms":                     "/b/spec/data-model/#terms",
		"../adr/0009-wasm-adapters.md":            "/b/decisions/0009-wasm-adapters/",
		"../../pkg/model":                         "https://github.com/dhm116/bearing/tree/main/pkg/model",
		"../../schema/observation.v1.schema.json": "https://github.com/dhm116/bearing/blob/main/schema/observation.v1.schema.json",
		"https://example.com/":                    "https://example.com/",
	}
	for dest, want := range cases {
		t.Run(dest, func(t *testing.T) {
			if got := b.ResolveLink("docs/spec/README.md", dest, false); got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}

func TestSnippetQuotesTheFirstCodeBlockUnderAHeading(t *testing.T) {
	root := t.TempDir()
	doc := "# Doc\n\n```sh\n# a comment, not a heading\necho skip\n```\n\n## Example\n\nText.\n\n```json\n{\"a\": 1}\n```\n\n```json\n{\"b\": 2}\n```\n"
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &Builder{Root: root}
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
	b := &Builder{Root: repo, SiteDir: site, Out: filepath.Join(site, "_site")}
	if err := b.prepareOut(); err != nil {
		t.Fatalf("got %v clearing site/_site, want nil", err)
	}
}

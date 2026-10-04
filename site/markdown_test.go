package main

import (
	"strings"
	"testing"
)

func TestSlugifyMatchesGitHub(t *testing.T) {
	cases := map[string]string{
		"Wire mapping":                 "wire-mapping",
		"State, determinism and apply": "state-determinism-and-apply",
		"observed_at":                  "observed_at",
		"Renames, moves and reuse":     "renames-moves-and-reuse",
		"ADR 9: WASM adapters":         "adr-9-wasm-adapters",
		"Un-merge":                     "un-merge",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := slugify(in); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestSluggerNumbersRepeats(t *testing.T) {
	s := newSlugger()
	got := []string{s.slug("Example"), s.slug("Example"), s.slug("Example")}
	want := []string{"example", "example-1", "example-2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSplitLinkKeepsOnlyRelativeLinks(t *testing.T) {
	cases := []struct {
		dest, path, frag string
		ok               bool
	}{
		{"contracts.md", "contracts.md", "", true},
		{"../adr/0009-wasm-adapters.md#amendment", "../adr/0009-wasm-adapters.md", "amendment", true},
		{"#terms", "", "", false},
		{"https://example.com/x.md", "", "", false},
		{"mailto:someone@example.com", "", "", false},
		{"/docs/spec/a.md#x", "/docs/spec/a.md", "x", true},
		{"//example.com/x.md", "", "", false},
		{"x.md?raw=1", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.dest, func(t *testing.T) {
			p, frag, ok := splitLink(c.dest)
			if p != c.path || frag != c.frag || ok != c.ok {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", p, frag, ok, c.path, c.frag, c.ok)
			}
		})
	}
}

// fakeResolver records links and rewrites them to a recognizable form.
type fakeResolver struct{ seen []string }

func (f *fakeResolver) ResolveLink(_, dest string, image bool) string {
	f.seen = append(f.seen, dest)
	if image {
		return "/img/" + dest
	}
	return "/page/" + dest
}

func TestRenderMarkdownTakesTitleLedeAndTOC(t *testing.T) {
	src := []byte("# Data model\n\nVersion 0.3. The model.\n\n## Terms\n\nSee [contracts](contracts.md).\n\n### Keys\n\n#### Deep\n\n## Terms\n\n![map](map.svg)\n")
	res := &fakeResolver{}
	r, err := RenderMarkdown("docs/spec/data-model.md", src, res)
	if err != nil {
		t.Fatal(err)
	}
	if r.Title != "Data model" {
		t.Fatalf("got title %q, want %q", r.Title, "Data model")
	}
	if strings.Contains(r.HTML, "<h1") {
		t.Fatalf("got an h1 in the body, want it removed: %s", r.HTML)
	}
	if r.Lede != "Version 0.3. The model." {
		t.Fatalf("got lede %q", r.Lede)
	}
	var toc []string
	for _, h := range r.TOC {
		toc = append(toc, h.ID)
	}
	if got, want := strings.Join(toc, ","), "terms,keys,terms-1"; got != want {
		t.Fatalf("got TOC %s, want %s", got, want)
	}
	if !r.IDs["deep"] {
		t.Fatalf("got IDs %v, want h4 ids recorded for link checks", r.IDs)
	}
	for _, want := range []string{`href="/page/contracts.md"`, `src="/img/map.svg"`, `id="terms-1"`} {
		if !strings.Contains(r.HTML, want) {
			t.Fatalf("got HTML without %s: %s", want, r.HTML)
		}
	}
}

func TestRenderMarkdownDropsADRMetadataLine(t *testing.T) {
	src := []byte("# 7. Durable events\n\nDate: 2026-09-29 · Status: proposed · see [ADR 3](0003.md)\n\n## Context\n\nBody.\n")
	res := &fakeResolver{}
	r, err := RenderMarkdown("docs/adr/0007.md", src, res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.HTML, "Date:") {
		t.Fatalf("got the metadata line in the body: %s", r.HTML)
	}
	if len(res.seen) != 0 {
		t.Fatalf("got links resolved in the dropped line: %v", res.seen)
	}
	if !strings.Contains(r.HTML, "Body.") {
		t.Fatalf("got HTML without the body: %s", r.HTML)
	}
}

func TestRenderMarkdownWrapsTablesAndEscapesHTML(t *testing.T) {
	src := []byte("# T\n\n| a | b |\n| - | - |\n| 1 | 2 |\n\n<script>alert(1)</script>\n")
	r, err := RenderMarkdown("x.md", src, &fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.HTML, `<div class="table-scroll" tabindex="0"><table>`) {
		t.Fatalf("got table without its scroll box: %s", r.HTML)
	}
	if strings.Contains(r.HTML, "<script>") {
		t.Fatalf("got raw HTML passed through: %s", r.HTML)
	}
}

func TestClipCutsAtAWord(t *testing.T) {
	got := clip("one two three four", 10)
	if got != "one two…" {
		t.Fatalf("got %q, want %q", got, "one two…")
	}
	if got := clip("short", 10); got != "short" {
		t.Fatalf("got %q, want %q", got, "short")
	}
}

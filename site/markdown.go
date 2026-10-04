package main

import (
	"bytes"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

// Heading is an entry in a page's table of contents.
type Heading struct {
	Level int
	ID    string
	Text  string
	// Snippet is the start of the section's first paragraph, for search.
	Snippet string
}

// Rendered is one Markdown document turned into HTML.
type Rendered struct {
	Title string
	// TitleID is the title's heading ID, which GitHub links can target.
	TitleID string
	HTML    string
	TOC     []Heading
	IDs     map[string]bool
	// Lede is the first paragraph's text.
	Lede string
}

// Resolver maps a link found in a document to its published target.
type Resolver interface {
	// ResolveLink returns the href for dest, a link in the file at repo
	// path src. Image is true for image sources, which must be copied.
	ResolveLink(src, dest string, image bool) string
}

// RenderMarkdown renders src (repo path file) to HTML with GitHub-style
// heading IDs, removes the first H1 (the page template shows the title)
// and rewrites relative links through res.
func RenderMarkdown(file string, src []byte, res Resolver) (*Rendered, error) {
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	doc := md.Parser().Parse(text.NewReader(src))

	out := &Rendered{IDs: map[string]bool{}}
	slugs := newSlugger()
	var title, meta ast.Node
	seenSection := false
	err := ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Heading:
			txt := plainText(n, src)
			id := slugs.slug(txt)
			out.IDs[id] = true
			if n.Level == 1 && title == nil {
				// The title is removed from the body, but GitHub gives it an
				// ID, so links to it and later repeats are numbered alike.
				title, out.Title, out.TitleID = n, txt, id
				return ast.WalkSkipChildren, nil
			}
			n.SetAttributeString("id", []byte(id))
			seenSection = true
			if n.Level >= 2 && n.Level <= 3 {
				out.TOC = append(out.TOC, Heading{Level: n.Level, ID: id, Text: txt})
			}
		case *ast.Paragraph:
			if n.Parent() != doc {
				break
			}
			// An ADR's "Date: … · Status: …" line becomes the page's
			// header chips, so it isn't repeated in the body.
			if title != nil && n.PreviousSibling() == title && strings.HasPrefix(plainText(n, src), "Date:") {
				meta = n
				return ast.WalkSkipChildren, nil
			}
			if out.Lede == "" && !seenSection && title != nil {
				out.Lede = plainText(n, src)
			}
		case *ast.Link:
			n.Destination = []byte(res.ResolveLink(file, string(n.Destination), false))
		case *ast.Image:
			n.Destination = []byte(res.ResolveLink(file, string(n.Destination), true))
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, err
	}
	if meta != nil {
		doc.RemoveChild(doc, meta)
	}
	if title != nil {
		title.Parent().RemoveChild(title.Parent(), title)
	}
	fillSnippets(doc, src, out.TOC)

	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, doc); err != nil {
		return nil, fmt.Errorf("render %s: %w", file, err)
	}
	html := buf.String()
	// Wide tables scroll inside their own box rather than the page.
	html = strings.ReplaceAll(html, "<table>", `<div class="table-scroll" tabindex="0"><table>`)
	html = strings.ReplaceAll(html, "</table>", "</table></div>")
	out.HTML = html
	return out, nil
}

// fillSnippets gives each TOC entry the start of its first paragraph.
func fillSnippets(doc ast.Node, src []byte, toc []Heading) {
	i := -1
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		switch n := n.(type) {
		case *ast.Heading:
			if n.Level >= 2 && n.Level <= 3 {
				i++
			}
		case *ast.Paragraph:
			if i >= 0 && i < len(toc) && toc[i].Snippet == "" {
				toc[i].Snippet = clip(plainText(n, src), 180)
			}
		}
	}
}

// plainText is the visible text of n, code spans included.
func plainText(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch c := c.(type) {
		case *ast.Text:
			b.Write(c.Segment.Value(src))
			if c.SoftLineBreak() || c.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(c.Value)
		case *ast.AutoLink:
			b.Write(c.Label(src))
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:") + "…"
}

// slugger makes heading IDs the way GitHub does, so links written for
// GitHub (#wire-mapping) work on the site too.
type slugger struct{ seen map[string]int }

func newSlugger() *slugger { return &slugger{seen: map[string]int{}} }

func (s *slugger) slug(text string) string {
	base := slugify(text)
	id := base
	if n, ok := s.seen[base]; ok {
		id = fmt.Sprintf("%s-%d", base, n)
		s.seen[base] = n + 1
	} else {
		s.seen[base] = 1
	}
	return id
}

// slugify lowercases text, drops punctuation and turns spaces into hyphens.
func slugify(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

var schemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// splitLink splits a repository link into its path and fragment; a path
// starting with "/" is relative to the repository root, as on GitHub. ok is
// false for URLs with a scheme or host, fragments only and anything with a
// query.
func splitLink(dest string) (p, frag string, ok bool) {
	if dest == "" || strings.HasPrefix(dest, "#") || strings.HasPrefix(dest, "//") || schemeRE.MatchString(dest) {
		return "", "", false
	}
	u, err := url.Parse(dest)
	if err != nil || u.RawQuery != "" {
		return "", "", false
	}
	return u.Path, u.Fragment, true
}

// repoPath resolves a link path relative to the file at repo path src, or
// to the repository root when it starts with "/".
func repoPath(src, p string) string {
	if strings.HasPrefix(p, "/") {
		return path.Clean(strings.TrimPrefix(p, "/"))
	}
	return path.Clean(path.Join(path.Dir(src), p))
}

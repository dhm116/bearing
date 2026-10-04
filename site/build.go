package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Builder renders the site from the repository into Out.
type Builder struct {
	// Root is the repository root; collection dirs are relative to it.
	Root string
	// SiteDir holds site.yaml, roadmap.yaml, templates/ and static/.
	SiteDir string
	Out     string
	// Base is the URL path the site is served under, with slashes at both
	// ends ("/" or "/bearing/").
	Base    string
	Config  *Config
	Roadmap *Roadmap
	Build   BuildInfo

	docs     map[string]*DocPage // by repo path
	cols     []*CollectionView
	pageIDs  map[string]map[string]bool // page path -> heading IDs; nil map means unchecked
	links    []linkRef
	assets   map[string]bool // repo paths copied to assets/
	static   map[string]string
	problems []string
	// repo reads repository files; os.Root refuses paths, symlinks
	// included, that lead outside Root, so nothing outside it is published.
	repo *os.Root
}

// BuildInfo says what the site was built from.
type BuildInfo struct {
	Commit string
	Time   time.Time
}

// ShortCommit is the first seven characters of the commit.
func (b BuildInfo) ShortCommit() string {
	if len(b.Commit) > 7 {
		return b.Commit[:7]
	}
	return b.Commit
}

// DocPage is a Markdown file published as a page.
type DocPage struct {
	Collection *Collection
	Source     string // repo path
	Path       string // site path, "spec/data-model/"
	Name       string // file name without .md
	Index      bool
	Order      int
	*Rendered
	Version string
	Status  string
	// StatusNote is the rest of an ADR's status line, such as
	// "Superseded in part by ADR 9".
	StatusNote string
	Date       string
	Number     string
	// Summary is one line about the page: its entry in the collection's
	// README, else its Decision section, else its first paragraph.
	Summary    string
	Prev, Next *DocPage
}

// Label is the title without an ADR's leading "6. ".
func (d *DocPage) Label() string {
	if d.Number != "" {
		return strings.TrimPrefix(d.Title, d.Number+". ")
	}
	return d.Title
}

// CollectionView is a collection and its pages in order.
type CollectionView struct {
	*Collection
	Index *DocPage
	Pages []*DocPage
}

type linkRef struct {
	from, to, frag string
}

// View is what every template receives.
type View struct {
	Site        *Config
	Base        string
	Path        string
	Title       string
	Description string
	Section     string
	Doc         *DocPage
	Collection  *CollectionView
	Collections []*CollectionView
	Roadmap     *Roadmap
	Build       BuildInfo
}

var (
	versionRE = regexp.MustCompile(`(?m)^(?:Protocol version|Version)\s+([0-9][0-9.]*)`)
	statusRE  = regexp.MustCompile(`(?m)\bStatus:\s*([A-Za-z]+)(.*)$`)
	mdLinkRE  = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	dateRE    = regexp.MustCompile(`(?m)\bDate:\s*(\d{4}-\d{2}-\d{2})`)
	adrNumRE  = regexp.MustCompile(`^(\d+)-`)
)

// Run builds the whole site. Broken internal links are reported as
// problems; the caller decides whether they fail the build.
func (b *Builder) Run() error {
	if err := b.run(); err != nil {
		return fmt.Errorf("site: build: %w", err)
	}
	return nil
}

// Close releases the repository handle that methods called after Run,
// such as ResolveLink, open on demand.
func (b *Builder) Close() error {
	if b.repo == nil {
		return nil
	}
	err := b.repo.Close()
	b.repo = nil
	return err
}

func (b *Builder) run() error {
	b.docs = map[string]*DocPage{}
	b.pageIDs = map[string]map[string]bool{}
	b.assets = map[string]bool{}
	b.static = map[string]string{}

	if err := b.prepareOut(); err != nil {
		return err
	}
	if _, err := b.repoFS(); err != nil {
		return err
	}
	defer func() { _ = b.Close() }()
	if err := b.copyStatic(); err != nil {
		return err
	}
	if err := b.collectDocs(); err != nil {
		return err
	}
	if err := b.renderDocs(); err != nil {
		return err
	}
	if err := b.renderPages(); err != nil {
		return err
	}
	if err := b.writeSearchIndex(); err != nil {
		return err
	}
	b.checkLinks()
	return b.writeOut(".nojekyll", nil)
}

// Problems lists broken links and other things a strict build fails on.
func (b *Builder) Problems() []string { return b.problems }

func (b *Builder) problem(format string, args ...any) {
	b.problems = append(b.problems, fmt.Sprintf(format, args...))
}

func (b *Builder) prepareOut() error {
	abs, err := filepath.Abs(b.Out)
	if err != nil {
		return err
	}
	root, _ := filepath.Abs(b.Root)
	site, _ := filepath.Abs(b.SiteDir)
	// Out is cleared, so it must not hold the repository or the site sources.
	if within(abs, root) || within(abs, site) {
		return fmt.Errorf("refusing to clear output directory %s: it contains the sources", abs)
	}
	// Only clear a directory a previous build wrote, marked by .nojekyll.
	entries, err := os.ReadDir(abs)
	if err == nil && len(entries) > 0 {
		if _, err := os.Stat(filepath.Join(abs, ".nojekyll")); err != nil {
			return fmt.Errorf("refusing to clear output directory %s: it isn't empty and wasn't written by a site build", abs)
		}
	}
	if err := os.RemoveAll(abs); err != nil {
		return err
	}
	return os.MkdirAll(abs, 0o755) //nolint:gosec // The output is a public website.
}

// within reports whether p is dir or inside it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// repoFS is the repository as an fs.FS confined to Root.
func (b *Builder) repoFS() (fs.FS, error) {
	if b.repo == nil {
		r, err := os.OpenRoot(b.Root)
		if err != nil {
			return nil, fmt.Errorf("open repository: %w", err)
		}
		b.repo = r
	}
	return b.repo.FS(), nil
}

// readRepo reads a file at a slash-separated repository path.
func (b *Builder) readRepo(rel string) ([]byte, error) {
	fsys, err := b.repoFS()
	if err != nil {
		return nil, err
	}
	return fs.ReadFile(fsys, rel)
}

// copyStatic copies static/ and the brand marks, remembering a content
// hash per file for cache busting.
func (b *Builder) copyStatic() error {
	site, err := os.OpenRoot(b.SiteDir)
	if err != nil {
		return fmt.Errorf("copy static: %w", err)
	}
	defer func() { _ = site.Close() }()
	static, err := fs.Sub(site.FS(), "static")
	if err != nil {
		return fmt.Errorf("copy static: %w", err)
	}
	err = fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return b.copyFile(static, p, path.Join("static", p))
	})
	if err != nil {
		return fmt.Errorf("copy static: %w", err)
	}
	repo, err := b.repoFS()
	if err != nil {
		return err
	}
	marks, _ := fs.Glob(repo, path.Join(b.Config.Brand, "bearing-mark*.svg"))
	if len(marks) == 0 {
		return fmt.Errorf("no bearing-mark*.svg in %s", b.Config.Brand)
	}
	for _, m := range marks {
		if err := b.copyFile(repo, m, path.Join("static", "brand", path.Base(m))); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies from in fsys to rel under Out.
func (b *Builder) copyFile(fsys fs.FS, from, rel string) error {
	data, err := fs.ReadFile(fsys, from)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	b.static[rel] = hex.EncodeToString(sum[:4])
	return b.writeOut(rel, data)
}

func (b *Builder) collectDocs() error {
	for i := range b.Config.Collections {
		col := &b.Config.Collections[i]
		repo, err := b.repoFS()
		if err != nil {
			return err
		}
		entries, err := fs.ReadDir(repo, col.Dir)
		if err != nil {
			return fmt.Errorf("collection %s: %w", col.ID, err)
		}
		cv := &CollectionView{Collection: col}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".md") || slices.Contains(col.Exclude, name) {
				continue
			}
			dp := &DocPage{Collection: col, Source: path.Join(col.Dir, name), Name: strings.TrimSuffix(name, ".md")}
			if name == "README.md" {
				dp.Index, dp.Path = true, col.Path+"/"
				cv.Index = dp
			} else {
				dp.Path = col.Path + "/" + dp.Name + "/"
				cv.Pages = append(cv.Pages, dp)
			}
			dp.Order = len(col.Order)
			if i := slices.Index(col.Order, name); i >= 0 {
				dp.Order = i
			}
			if m := adrNumRE.FindStringSubmatch(name); m != nil {
				dp.Number = strings.TrimLeft(m[1], "0")
			}
			b.docs[dp.Source] = dp
		}
		sort.SliceStable(cv.Pages, func(i, j int) bool {
			a, c := cv.Pages[i], cv.Pages[j]
			if a.Order != c.Order {
				return a.Order < c.Order
			}
			return a.Name < c.Name
		})
		b.cols = append(b.cols, cv)
	}
	return nil
}

func (b *Builder) renderDocs() error {
	for _, cv := range b.cols {
		pages := cv.Pages
		if cv.Index != nil {
			pages = append([]*DocPage{cv.Index}, pages...)
		}
		for _, dp := range pages {
			raw, err := b.readRepo(dp.Source)
			if err != nil {
				return err
			}
			r, err := RenderMarkdown(dp.Source, raw, b)
			if err != nil {
				return err
			}
			dp.Rendered = r
			if dp.Title == "" {
				dp.Title = dp.Name
			}
			// Metadata lives in the lines right under the title.
			head := firstLines(raw, 8)
			if m := versionRE.FindSubmatch(head); m != nil {
				dp.Version = string(m[1])
			}
			if m := statusRE.FindSubmatch(head); m != nil {
				dp.Status = strings.ToLower(string(m[1]))
				note := mdLinkRE.ReplaceAllString(string(m[2]), "$1")
				dp.StatusNote = strings.Trim(note, " ·,")
			}
			if m := dateRE.FindSubmatch(head); m != nil {
				dp.Date = string(m[1])
			}
			b.pageIDs[dp.Path] = r.IDs
		}
		if cv.Index == nil {
			b.pageIDs[cv.Path+"/"] = map[string]bool{}
		}
	}
	for _, cv := range b.cols {
		b.summarize(cv)
		pages := cv.Pages
		if cv.Index != nil {
			pages = append([]*DocPage{cv.Index}, pages...)
		}
		for i, dp := range pages {
			if i > 0 {
				dp.Prev = pages[i-1]
			}
			if i+1 < len(pages) {
				dp.Next = pages[i+1]
			}
		}
		for _, dp := range pages {
			v := b.view(dp.Path, dp.Title, clip(dp.Summary, 200), cv.ID)
			v.Doc, v.Collection = dp, cv
			if err := b.renderTemplate("doc", dp.Path, v); err != nil {
				return err
			}
		}
		if cv.Index == nil {
			v := b.view(cv.Path+"/", cv.Title, cv.Description, cv.ID)
			v.Collection = cv
			if err := b.renderTemplate("collection", cv.Path+"/", v); err != nil {
				return err
			}
		}
	}
	return nil
}

var readmeEntryRE = regexp.MustCompile(`(?m)^\s*(?:\d+\.|[-*])\s+\[[^\]]+\]\(([^)#]+\.md)\):\s*(.+)$`)

// summarize sets each page's Summary; see DocPage.
func (b *Builder) summarize(cv *CollectionView) {
	fromReadme := map[string]string{}
	if cv.Index != nil {
		raw, err := b.readRepo(cv.Index.Source)
		if err == nil {
			for _, m := range readmeEntryRE.FindAllSubmatch(raw, -1) {
				fromReadme[path.Join(cv.Dir, string(m[1]))] = strings.TrimSpace(string(m[2]))
			}
		}
	}
	all := cv.Pages
	if cv.Index != nil {
		all = append([]*DocPage{cv.Index}, all...)
	}
	for _, dp := range all {
		if s, ok := fromReadme[dp.Source]; ok {
			dp.Summary = sentence(s)
			continue
		}
		for _, h := range dp.TOC {
			if h.ID == "decision" && h.Snippet != "" {
				dp.Summary = h.Snippet
				break
			}
		}
		if dp.Summary == "" {
			dp.Summary = stripVersion(dp.Lede)
		}
	}
}

// sentence capitalizes s and ends it with a period.
func sentence(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

var versionSentenceRE = regexp.MustCompile(`^(?:Protocol version|Version)\s+[0-9.]+[^.]*\.\s*`)

func stripVersion(s string) string { return versionSentenceRE.ReplaceAllString(s, "") }

func (b *Builder) renderPages() error {
	for _, p := range b.Config.Pages {
		// Hand-written pages declare their anchors in templates; their
		// fragments aren't checked.
		b.pageIDs[p.Path] = nil
	}
	for _, p := range b.Config.Pages {
		v := b.view(p.Path, p.Title, p.Description, p.Path)
		out := p.Path
		if p.Template == "404" {
			out = "404.html"
		}
		if err := b.renderTemplate(p.Template, out, v); err != nil {
			return err
		}
	}
	return nil
}

func (b *Builder) view(p, title, desc, section string) *View {
	return &View{
		Site: b.Config, Base: b.Base, Path: p, Title: title, Description: desc,
		Section: section, Collections: b.cols, Roadmap: b.Roadmap, Build: b.Build,
	}
}

// renderTemplate executes templates/<name>.html inside the base layout and
// writes it to out (a site path; a trailing slash means index.html).
func (b *Builder) renderTemplate(name, out string, v *View) error {
	dir := filepath.Join(b.SiteDir, "templates")
	t := template.New("base.html").Funcs(b.funcs(v.Path))
	files := []string{filepath.Join(dir, "base.html"), filepath.Join(dir, name+".html")}
	partials, _ := filepath.Glob(filepath.Join(dir, "_*.html"))
	t, err := t.ParseFiles(append(files, partials...)...)
	if err != nil {
		return fmt.Errorf("template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base.html", v); err != nil {
		return fmt.Errorf("template %s for %s: %w", name, out, err)
	}
	if strings.HasSuffix(out, "/") || out == "" {
		out += "index.html"
	}
	return b.writeOut(out, buf.Bytes())
}

func (b *Builder) funcs(from string) template.FuncMap {
	return template.FuncMap{
		// url makes a site path absolute and records it for the link check.
		"url": func(p string) string {
			if schemeRE.MatchString(p) {
				return p
			}
			target, frag, _ := strings.Cut(p, "#")
			b.links = append(b.links, linkRef{from: from, to: target, frag: frag})
			return b.Base + p
		},
		"static": func(p string) (string, error) {
			h, ok := b.static["static/"+p]
			if !ok {
				return "", fmt.Errorf("static file %s does not exist", p)
			}
			return b.Base + "static/" + p + "?v=" + h, nil
		},
		"html":        func(s string) template.HTML { return template.HTML(s) }, //nolint:gosec // Rendered from repository Markdown with raw HTML disabled.
		"statusLabel": StatusLabel,
		"snippet":     b.snippet,
		"github": func(kind, p string) string {
			return fmt.Sprintf("https://github.com/%s/%s/%s/%s", b.Config.Repo, kind, b.Config.Branch, p)
		},
		"repoURL": func() string { return "https://github.com/" + b.Config.Repo },
		"active": func(section, href string) bool {
			return section != "" && strings.HasPrefix(href, strings.TrimSuffix(section, "/"))
		},
		"clip": clip,
		"collection": func(id string) (*CollectionView, error) {
			for _, cv := range b.cols {
				if cv.ID == id {
					return cv, nil
				}
			}
			return nil, fmt.Errorf("no collection %q", id)
		},
		"area": func(name string) (*AreaView, error) {
			if b.Roadmap != nil {
				for _, a := range b.Roadmap.Areas {
					if a.Name == name {
						return a, nil
					}
				}
			}
			return nil, fmt.Errorf("no roadmap area %q", name)
		},
		"asset": func(p string) (string, error) {
			if !b.assets[p] {
				repo, err := b.repoFS()
				if err != nil {
					return "", err
				}
				if err := b.copyFile(repo, p, path.Join("assets", p)); err != nil {
					return "", fmt.Errorf("asset %s: %w", p, err)
				}
				b.assets[p] = true
			}
			return b.Base + "assets/" + p, nil
		},
		"date": func(t time.Time) string { return t.UTC().Format("2006-01-02") },
		"add":  func(a, c int) int { return a + c },
		"pct": func(n, d int) int {
			if d == 0 {
				return 0
			}
			return n * 100 / d
		},
	}
}

// snippet returns the first fenced code block under the heading with the
// given ID in a repository Markdown file, so pages can quote the spec
// without copying it.
func (b *Builder) snippet(file, id string) (string, error) {
	raw, err := b.readRepo(file)
	if err != nil {
		return "", fmt.Errorf("snippet: %w", err)
	}
	slugs := newSlugger()
	in, fenced := false, false
	var code []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case fenced:
			// "#" lines inside any code block are code, not headings.
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				if in {
					return strings.Join(code, "\n"), nil
				}
				fenced = false
				continue
			}
			if in {
				code = append(code, line)
			}
		case strings.HasPrefix(line, "```"):
			fenced = true
		case strings.HasPrefix(line, "#"):
			title := strings.TrimSpace(strings.TrimLeft(line, "#"))
			in = slugs.slug(strings.ReplaceAll(title, "`", "")) == id
		}
	}
	return "", fmt.Errorf("snippet: no code block under #%s in %s", id, file)
}

// ResolveLink implements Resolver.
func (b *Builder) ResolveLink(src, dest string, image bool) string {
	from := ""
	if dp := b.docs[src]; dp != nil {
		from = dp.Path
	}
	if strings.HasPrefix(dest, "#") {
		b.links = append(b.links, linkRef{from: from, to: from, frag: dest[1:]})
		return dest
	}
	p, frag, ok := splitLink(dest)
	if !ok {
		return dest
	}
	rp := repoPath(src, p)
	if strings.HasPrefix(rp, "..") {
		b.problem("%s: link %s leaves the repository", src, dest)
		return dest
	}
	withFrag := func(u string) string {
		if frag != "" {
			return u + "#" + frag
		}
		return u
	}
	if image {
		repo, err := b.repoFS()
		if err == nil {
			_, err = fs.Stat(repo, rp)
		}
		if err != nil {
			b.problem("%s: image %s does not exist", src, dest)
			return dest
		}
		if !b.assets[rp] {
			b.assets[rp] = true
			if err := b.copyFile(repo, rp, path.Join("assets", rp)); err != nil {
				b.problem("%s: copy %s: %v", src, rp, err)
			}
		}
		return b.Base + "assets/" + rp
	}
	target := b.docs[rp]
	if target == nil {
		target = b.docs[path.Join(rp, "README.md")]
	}
	if target != nil {
		b.links = append(b.links, linkRef{from: from, to: target.Path, frag: frag})
		return withFrag(b.Base + target.Path)
	}
	var info fs.FileInfo
	repo, err := b.repoFS()
	if err == nil {
		info, err = fs.Stat(repo, rp)
	}
	if err != nil {
		b.problem("%s: link %s points at %s, which does not exist", src, dest, rp)
		return dest
	}
	kind := "blob"
	if info.IsDir() {
		kind = "tree"
	}
	return withFrag(fmt.Sprintf("https://github.com/%s/%s/%s/%s", b.Config.Repo, kind, b.Config.Branch, rp))
}

func (b *Builder) checkLinks() {
	for _, l := range b.links {
		ids, ok := b.pageIDs[l.to]
		switch {
		case !ok:
			b.problem("%s: link to /%s, which is not a page", orRoot(l.from), l.to)
		case l.frag != "" && ids != nil && !ids[l.frag]:
			b.problem("%s: link to /%s#%s, which has no such heading", orRoot(l.from), l.to, l.frag)
		}
	}
}

func orRoot(p string) string {
	if p == "" {
		return "/"
	}
	return "/" + p
}

// searchEntry is one result in search.json, kept terse because the
// browser downloads the whole file.
type searchEntry struct {
	Title   string `json:"t"`
	Heading string `json:"h,omitempty"`
	URL     string `json:"u"`
	Section string `json:"s"`
	Text    string `json:"x,omitempty"`
}

func (b *Builder) writeSearchIndex() error {
	var entries []searchEntry
	for _, p := range b.Config.Pages {
		if p.Template == "404" {
			continue
		}
		entries = append(entries, searchEntry{Title: p.Title, URL: b.Base + p.Path, Section: "Site", Text: p.Description})
	}
	if b.Roadmap != nil {
		for _, a := range b.Roadmap.Areas {
			for _, f := range a.Features {
				entries = append(entries, searchEntry{
					Title: f.Name, URL: b.Base + "roadmap/#" + f.Slug,
					Section: "Roadmap · " + StatusLabel(f.Status), Text: f.Summary,
				})
			}
		}
	}
	for _, cv := range b.cols {
		pages := cv.Pages
		if cv.Index != nil {
			pages = append([]*DocPage{cv.Index}, pages...)
		}
		for _, dp := range pages {
			entries = append(entries, searchEntry{Title: dp.Title, URL: b.Base + dp.Path, Section: cv.Short, Text: clip(dp.Lede, 180)})
			for _, h := range dp.TOC {
				entries = append(entries, searchEntry{
					Title: dp.Title, Heading: h.Text, URL: b.Base + dp.Path + "#" + h.ID,
					Section: cv.Short, Text: h.Snippet,
				})
			}
		}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return b.writeOut("search.json", data)
}

// writeOut writes a file under Out. os.Root keeps every write inside Out
// whatever the path says.
func (b *Builder) writeOut(rel string, data []byte) error {
	root, err := os.OpenRoot(b.Out)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	rel = filepath.FromSlash(rel)
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return root.WriteFile(rel, data, 0o644)
}

func firstLines(raw []byte, n int) []byte {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	var out bytes.Buffer
	for i := 0; i < n && sc.Scan(); i++ {
		out.Write(sc.Bytes())
		out.WriteByte('\n')
	}
	return out.Bytes()
}

var _ Resolver = (*Builder)(nil)

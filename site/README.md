# Project site

The Bearing website, published to GitHub Pages by
[`.github/workflows/pages.yml`](../.github/workflows/pages.yml). A small Go
generator (its own module, so the root module's dependencies don't change)
reads the repository and writes static HTML. Most of the site is the
repository itself, so it stays current without edits here:

| Page | Comes from |
| --- | --- |
| Spec | Every Markdown file in [`docs/spec/`](../docs/spec/), rendered as is |
| Roadmap | [`roadmap.yaml`](roadmap.yaml); statuses follow milestones and issues read from GitHub at build time |
| Home, How it works | Hand-written templates in [`templates/`](templates/) |
| Logo, colors, type | [`docs/brand/`](../docs/brand/) (the logo SVGs are copied, the tokens are mirrored in [`static/css/tokens.css`](static/css/tokens.css)) |

The site rebuilds on every push to `main`, and once a day so roadmap
statuses follow GitHub. Pull requests build it without publishing, so a
change that breaks a docs link fails there.

## Preview locally

```sh
cd site
go run . -serve localhost:8080      # build into site/_site and serve it
go run . -offline -serve localhost:8080  # without calling the GitHub API
```

Flags: `-out` (default `_site`, cleared on each build), `-base` (the URL
path the site is served under; the workflow passes the Pages base path),
`-strict` (fail on a broken link or anchor, or if GitHub can't be read; CI
uses it), `-offline`. `GITHUB_TOKEN`, if set, only raises the API rate
limit.

Before pushing a change to the generator, run what the workflow runs:

```sh
cd site
go vet ./... && go test ./...
go run -modfile=../tools/go.mod github.com/golangci/golangci-lint/v2/cmd/golangci-lint run --config ../.golangci.yml ./...
go run -modfile=../tools/go.mod golang.org/x/vuln/cmd/govulncheck ./...
```

The generator is a developer command like those in `tools/`, not part of
Bearing, so it logs with plain `slog` and calls GitHub without `otelhttp`.

`go test` builds the real site, so it also fails on broken links in
`docs/`.

## Writing for the site

The site is for people who are new to Bearing, and may be new to software
engineering. The hand-written pages and `roadmap.yaml` follow these rules,
and `TestHandWrittenPagesStayInPlainTerms` checks the mechanical ones:

- Lead with the everyday problem and how Bearing helps. Prefer a diagram
  to a paragraph.
- Describe the finished product in the present tense. Never mention
  milestones or build steps; a feature is only available, in progress or
  planned, and the roadmap page says which.
- Don't link or cite the decision records in `docs/adr`. Technical detail
  belongs in the Spec section.
- Explain a technical word the first time it appears, or use a plain one.
  Say "connector" rather than "adapter" outside the spec, and avoid
  "surface" altogether.
- Don't name project members.

## Common changes

**A spec document.** Add or edit the Markdown in `docs/spec/`.
Nothing to change here. Link between documents with relative `.md` links as you would
for GitHub (relative, or from the repository root with a leading `/`); the
generator rewrites them to site pages, sends links to code and other files
to GitHub, and checks every anchor. To set the order of
spec pages, edit `order` under the `spec` collection in
[`site.yaml`](site.yaml); unlisted files follow alphabetically.

**A roadmap feature.** Add an entry under `features` in
[`roadmap.yaml`](roadmap.yaml) with its `area`, a one-line `summary` written
from a user's point of view, and either the GitHub `issues` that track it or
the `milestone` it belongs to. The page never shows those; they only decide
the status:

- With issues: all closed is **available**; some closed, or its milestone
  is in progress, is **in progress**; otherwise **planned**.
- With only a milestone: closed is **available**, in progress is **in
  progress**, otherwise **planned**.

A milestone is in progress when it is the first open one, or when any of
its issues are closed.

Set `status` by hand only for work GitHub doesn't track: things that
already work, or ideas with no milestone yet. Leave out internal
engineering work (CI, refactors) that a user wouldn't notice. The build
refuses unknown areas, milestones, statuses and fields.

**A milestone.** Create it on GitHub, then add its number under
`milestones` in `roadmap.yaml`, in the order the work happens.

**A new docs folder.** Add a collection to `site.yaml` (`dir`, `path`,
titles) and, if it belongs in the top bar, a `nav` entry.

**A hand-written page.** Add `templates/<name>.html` defining `main`, and a
`pages` entry in `site.yaml` naming the template and its path.

## How it fits together

| File | Role |
| --- | --- |
| `main.go` | Flags, the GitHub fetch and the preview server |
| `config.go` | Loads `site.yaml` |
| `roadmap.go` | Loads `roadmap.yaml` and derives feature statuses |
| `github.go` | Reads milestones and issues from the REST API |
| `markdown.go` | Renders Markdown (GitHub-flavored, raw HTML off) with GitHub-style heading IDs, and highlights code blocks that name a language |
| `build.go` | Lays out pages, rewrites and checks links, writes `search.json` |
| `templates/` | `base.html` wraps every page; `_*.html` are partials |
| `static/` | CSS, the search and table-of-contents script, and self-hosted fonts |

Templates get these functions besides Go's built-ins: `url` (a site path,
checked at build time), `static` (a cache-busted asset URL), `snippet file
heading-id` (the first code block under a heading in a repository file),
`github kind path` (a link into the repository), `collection id` and
`lower`.

Pages follow the reader's light or dark system setting, as the logo does.
Diagrams on the hand-written pages are HTML and inline SVG, animated with
CSS only; `templates/_icons.html` holds their icons. The design notes are at the top of
[`static/css/site.css`](static/css/site.css).

## Turning on Pages

Once, in the repository's **Settings → Pages**, set **Source** to **GitHub
Actions**. The workflow does the rest.

<!-- diagram-design-profile
name: Bearing
slug: bearing
source-url: https://github.com/dhm116/bearing/tree/main/docs/brand
created: 2026-09-30
updated: 2026-09-30
notes: Deep sea teal brand from docs/brand and the logo concepts (Fix, Heading, Race, Letter b, Prompt)
-->
# Style Guide

**The single source of truth for colors, typography, and tokens.** Every diagram draws from this — not from hex values inlined in other reference files. If you want to change the visual skin of Diagram Design, change this file.

This is the **Bearing** skin. It follows the brand guide in the Bearing repository (`docs/brand/README.md`): cool, slightly teal-leaning neutrals, deep sea teal accent, Bricolage Grotesque for titles, IBM Plex Sans for names and IBM Plex Mono for anything technical. The motifs in [Bearing motifs](#bearing-motifs) come from the five logo concepts (Fix, Heading, Race, Letter b, Prompt) and give diagrams the same vocabulary as the mark.

---

## Tokens

### Semantic roles

Every token is referred to by **semantic role**, not by its hex value. Type references (`type-*.md`) and SKILL.md say `accent`, not `#1D5E74`.

| Role | Purpose | Default (light) | Default (dark) |
|---|---|---|---|
| `paper` | Page background, default node fill | `#F5F7F6` (brand background) | `#11191C` (brand background) |
| `paper-2` | Diagram container bg, secondary fill | `#EDF1F0` (brand sunk) | `#172226` (brand surface) |
| `ink` | Primary text, primary stroke | `#18242A` | `#E4ECEA` |
| `ink-strong` | High-contrast text on accent fills | `#FFFFFF` | `#0C1316` |
| `muted` | Secondary text, default arrow stroke | `#55646B` | `#98A8AD` |
| `soft` | Sublabels, boundary labels | `#6B7C82` | `#7C8F94` |
| `rule` | Hairline borders | `rgba(24,36,42,0.12)` | `rgba(228,236,234,0.12)` |
| `rule-solid` | Stronger borders, baselines | `#D3DCDA` (brand line) | `#2A383D` (brand line) |
| `accent` | Focal / 1–2 max per diagram | `#1D5E74` (deep sea teal) | `#7FC0D6` |
| `accent-tint` | Fill for accent-bordered boxes | `rgba(29,94,116,0.08)` | `rgba(127,192,214,0.10)` |
| `link` | HTTP/API calls, external arrows | `#4B5198` | `#A4A9E3` |

> **Brand palette source:** `paper`, `paper-2`, `ink`, `muted`, `rule-solid` and `accent` are the Bearing core tokens (`background`, `sunk`/`surface`, `ink`, `muted`, `line`, `accent`). `soft` sits between `muted` and `line` (the dark value is the brand's `term-dim`). `accent-tint` is the accent at low opacity and lands close to the brand's `accent-soft` (`#E2EEF1` / `#1B3740`). `link` is derived: the brand uses the accent for links, but in a diagram the accent is reserved for the focal node, so external calls get an indigo that stays clearly apart from the teal. `ink-strong` flips to white on the dark teal because the Bearing accent is dark in light mode.

> **Contrast (checked):** `ink` on `paper` 14.7:1 light, 14.8:1 dark. `muted` 5.7:1 / 7.2:1. `soft` 4.0:1 / 5.3:1. `accent` 6.7:1 / 8.8:1. `link` 6.7:1 / 7.9:1. `ink-strong` on `accent` 7.2:1 / 8.8:1.

### Inversion rule (light → dark)

Any `rgba(24,36,42, X)` in light becomes `rgba(228,236,234, X)` in dark. Same opacities, RGB flipped. The accent moves from deep sea teal `#1D5E74` to the lighter `#7FC0D6`, the same pair the mark uses.

### Status (Bearing extension)

Bearing keeps status colors separate from the accent and uses them **only for state**: policy results, standards checks, incident severity, pass/fail traces. Never use them for decoration or to tell ordinary nodes apart.

| Token | Light | Dark | Soft light | Soft dark |
|---|---|---|---|---|
| `good` | `#2F6B45` | `#8DCB9F` | `#E3F0E7` | `#1C3325` |
| `warn` | `#A5561A` | `#E3A56E` | `#F7EBDF` | `#3A2819` |
| `bad` | `#A33A3A` | `#E89A9A` | `#F6E3E3` | `#3B1F1F` |

A status node uses the soft value as its fill and the full value as its stroke and label. It does not count against the 1–2 accent budget, but the same restraint applies: status marks the nodes whose state is the point.

### Series palette (multi-series chart types only)

A small set of desaturated colors for chart types that genuinely need to distinguish multiple overlapping entities (currently: **radar**). The "1-focal" rule still holds — `accent` is reserved for the focal series; the palette below covers the rest. The values lean cool to sit next to the teal.

| Token | Light | Dark | Notes |
|---|---|---|---|
| `series-1` | `#6F8A7A` (sea-sage) | `#93AE9E` | Non-focal series |
| `series-2` | `#5E6F9B` (slate-indigo) | `#8C9CC4` | Non-focal series |
| `series-3` | `#A88B5A` (sand) | `#CBAE7E` | Non-focal series |
| `series-4` | `#8A6A78` (mauve) | `#AE8E9C` | Non-focal series |
| `series-5` | `#5C6E74` (fog) | `#8A9CA2` | Non-focal series |

Fills sit at `0.18` opacity light, `0.22` dark; strokes use the full color. **Don't backfill these tokens to non-chart types** — architecture, swimlane, etc. continue to use muted-ink variants.

### Terminal skin (opt-in alternate)

A self-contained palette for the terminal-window primitive (see [primitive-terminal.md](primitive-terminal.md)). For Bearing this is the **Prompt** concept's register: the CLI and the PR come first, so terminal-framed diagrams suit `bearing` command walkthroughs, agent transcripts and social cards. Values come from the brand's Terminal tokens.

| Token | Hex | Purpose |
|---|---|---|
| `terminal-page` | `#0C1316` | Page background behind the window |
| `terminal-paper` | `#0F171A` | Window body, node fill (brand `term-bg`) |
| `terminal-bar` | `#152024` | Titlebar strip |
| `terminal-border` | `#2A383D` | Window border, hairlines |
| `terminal-ink` | `#D6E2E0` | Primary text, primary stroke (brand `term-ink`) |
| `terminal-muted` | `#7C8F94` | Secondary text, sublabels, prompts (brand `term-dim`) |
| `terminal-soft` | `#4A5B60` | Tertiary — inactive dots, spokes |
| `terminal-accent` | `#7FC0D6` | The one accent — entity names, prompt chevron, active dot |
| `terminal-accent-tint` | `rgba(127,192,214,0.12)` | Fill for accent-bordered boxes |

**1-accent rule still holds.** The brand's `term-ok` (`#8DCB9F`) and `term-warn` (`#E3A56E`) may mark success or warning lines in CLI output, as state, never as decoration.

---

## Typography

| Role | Family | Size | Weight | Usage |
|---|---|---|---|---|
| `title` | Bricolage Grotesque | 1.75rem | 700, tracked −0.015em | Page H1 |
| `node-name` | IBM Plex Sans | 12px | 600 | Human-readable labels |
| `sublabel` | IBM Plex Mono | 9px | 400 | Port, protocol, URL, field type |
| `eyebrow` | IBM Plex Mono | 7–8px | 500, tracked 0.12em, uppercase | Type tags, axis labels |
| `arrow-label` | IBM Plex Mono | 8px | 400, tracked 0.06em | Arrow annotations |
| `callout` | IBM Plex Sans *italic* | 14px | 400 | Editorial asides only |

### Font stack

```html
<link href="https://fonts.googleapis.com/css2?family=Bricolage+Grotesque:opsz,wght@12..96,500;12..96,700&family=IBM+Plex+Sans:ital,wght@0,400;0,500;0,600;1,400&family=IBM+Plex+Mono:wght@400;500&family=Noto+Sans+KR:wght@400;500;600&family=Noto+Sans+TC:wght@400;500;600&display=swap" rel="stylesheet">
```

CSS variables for templates:

```css
--font-display: 'Bricolage Grotesque', 'IBM Plex Sans', 'Noto Sans KR', 'Noto Sans TC', system-ui, sans-serif;
--font-sans:    'IBM Plex Sans', 'Noto Sans KR', 'Noto Sans TC', system-ui, sans-serif;
--font-mono:    'IBM Plex Mono', ui-monospace, Menlo, monospace;
```

Templates that name `--font-serif` for the title should point it at `--font-display`'s stack; the title is a grotesque in this skin, set at weight 700 instead of 400.

**Load-bearing rule:** Mono is for *technical* content (ports, commands, URLs, field types, entity keys like `github:repo/acme/api`). Names go in IBM Plex Sans. The page title is Bricolage Grotesque, and nothing else in the diagram uses it. Italic IBM Plex Sans is reserved for annotation callouts (see [primitive-annotation.md](primitive-annotation.md)). **Never JetBrains Mono** as a blanket "dev" font.

> **Deviation from the shipped guide:** the shipped skin uses a serif for `title` and `callout` so the contrast with the sans is obvious. Bearing's brand has no serif, and the brand asks for three families. The contrast comes from Bricolage Grotesque's heavier, wider display cut for the title and from a true italic for callouts. Don't add Instrument Serif back as a fourth family.

### Korean and Chinese labels

IBM Plex Sans, IBM Plex Mono and Bricolage Grotesque carry no Hangul or Han. A Korean or Chinese `<text>` element extends its own family — never swap the skin:

```svg
<text font-family="'IBM Plex Sans', 'Noto Sans KR', 'Apple SD Gothic Neo', 'Malgun Gothic', sans-serif">결제 서비스</text>
<text font-family="'IBM Plex Sans', 'Noto Sans TC', 'PingFang TC', 'Microsoft JhengHei', sans-serif">請求項比對</text>
```

Both Noto Sans faces ship in the font link above. Titles use the display stack, which already falls back to them. Simplified Chinese uses `'Noto Sans SC'`, `'PingFang SC'`, `'Microsoft YaHei'`; that face does not ship in the link.

**Width budget.** Measure per character, not per script: **every Unicode wide or full-width character costs 1em (full-width punctuation included), every other character costs its face's Latin advance** (budget 0.60em sans, 0.62em mono; IBM Plex Mono's true advance is exactly 0.60em, so the mono budget has a little slack), and nonspacing/enclosing marks cost nothing. Sum over the string and multiply by the font size for the text width, then add padding and round the box up to the next multiple of 4.

Three rules follow from CJK metrics:

- **Sublabels stay Latin.** Ports, protocols, field types, and URLs stay in IBM Plex Mono and are not translated.
- **Floor of 12px.** Hangul and Han go muddy below 12px. If a name doesn't fit at 12px, cut the name — don't shrink the type.
- **Arrow labels, eyebrows, and legend text switch register.** A Korean or Chinese label in one of those 7–8px mono slots becomes 12px IBM Plex Sans at weight 500 with no tracking and no uppercase transform, and its mask rect grows to match (16px tall, width from the budget above, rounded to a multiple of 4). Latin labels in the same diagram keep the mono treatment.

### Cyrillic labels

IBM Plex Sans and IBM Plex Mono ship Cyrillic, so names, sublabels, arrow labels, eyebrows and legend text keep the Latin treatment with no register switch. Bricolage Grotesque has no Cyrillic; a Cyrillic title falls through to IBM Plex Sans in the display stack. That is acceptable for a title, but set it at weight 700 so the halves match.

**Preserve printed labels.** A label the reader matches against a physical or system thing (a hostname, a repo name, a cabinet) carries the exact string. Don't transliterate or re-case it.

---

## Stroke, radius, spacing

| Token | Value | Use |
|---|---|---|
| `stroke-thin` | `0.8` | Tag-box outlines, leaf nodes |
| `stroke-default` | `1` | Most strokes |
| `stroke-strong` | `1.2` | Emphasis strokes |
| `radius-sm` | `4` | Small tags |
| `radius-md` | `6` | Node boxes |
| `radius-lg` | `8` | Containers, rings |
| `grid` | `4` | Every coord, size, and gap is divisible by 4 (hard rule) |

---

## Node type → treatment

Semantic role combinations — reference these by name in type specs.

| Type | Fill | Stroke |
|---|---|---|
| `focal` (1–2 max) | `accent-tint` | `accent` |
| `backend` | `#FFFFFF` light / `#172226` dark (brand surface) | `ink` |
| `store` | `ink @ 0.05` | `muted` |
| `external` | `ink @ 0.03` | `ink @ 0.30` |
| `input` | `muted @ 0.10` | `soft` |
| `optional` | `ink @ 0.02` | `ink @ 0.20` dashed `4,3` |
| `security` | `accent @ 0.05` | `accent @ 0.50` dashed `4,4` |

---

## Bearing motifs

Small details taken from the logo concepts. They make a diagram read as Bearing's without adding color. Each is optional; use one or two per diagram, never all of them.

- **The fix (from Fix).** Where several sources confirm one fact or entity (identity resolution, a fact asserted by GitHub, AWS and PagerDuty together), draw the source arrows converging on the focal node and mark the node with a ringed point: an `accent` ring (r 7, stroke 2) on a `paper` disc with an `accent` center dot (r 2.6), placed at the node's top-right corner. This is the one place Bearing draws a symbol inside a diagram, and it only ever marks the focal node.
- **The pin (from Letter b and the wordmark).** A small `accent` dot (r 3) beside a focal node's label is the lightweight version of the fix. It echoes the center pin of the mark and the dot on the wordmark's i. Use it instead of a thicker stroke when a focal node needs one more cue.
- **The 040° heading (from Heading).** When a diagram needs a diagonal (a leader line, an axis tick, a decorative needle), angle it at 40° from vertical. Every concept that shows a direction uses 040°, so diagrams share it. Orthogonal routing stays the default for arrows.
- **North is up (from Letter b).** Prefer layouts where sources feed the graph left-to-right or bottom-to-top, so the graph or store they feed sits at the right or top. Arrowheads are small filled isosceles triangles, the shape of the mark's north arrow, in the arrow's stroke color.
- **The race (from Race).** For hub-and-spoke or loop diagrams where adapters surround the core, place the spokes evenly on a ring around the hub and color exactly one of them `accent` (the one the diagram is about), matching the one highlighted ball in the Race concept.
- **Measured arcs (from Heading).** A dotted arc (`stroke-dasharray: 0.1 4`, round caps, `muted`) may show an angle, a range or a retry loop. Don't use dotted lines for anything else; `optional` keeps its `4,3` dash.

**Using the mark itself.** Diagrams don't include the logo. When a diagram is a standalone page or social card and needs a sign-off, place the mark from `docs/brand/bearing-mark.svg` in the footer at 16–24px with no wordmark, following the brand guide's clear-space rule. Never redraw or recolor it.

---

## Customizing the skin

Four options:

1. **Run onboarding** — see [`onboarding.md`](onboarding.md). Drop a URL; the skill extracts the palette + fonts and rewrites this file.
2. **Edit by hand** — change the hex values in the tables above. Run the pre-output taste gate afterward to verify the accent still reads as "focal" against the new paper color.
3. **Brand handoff** — paste your existing design-token JSON into a new section here and map its tokens to the semantic roles above.
4. **Client profiles** — save and switch named skins, or bind one to a project, using [`profiles.md`](profiles.md).

For Bearing, change `docs/brand/README.md` first and then update this profile to match, so the brand guide stays the source of truth.

### Constraints (don't break these)

- **Contrast**: `ink` must hit WCAG AA on `paper`. `muted` must hit AA on `paper` for 11px+ text.
- **One accent**: deep sea teal is the accent. Status colors mark state only; `link` marks external calls only.
- **No rainbow palette**: paper, ink and teal carry the diagram. Everything else is a `muted` variant.
- **Display + sans + mono**: three families, not more (see the typography deviation note).
- **Paper leans cool, not pure white**: Bearing's paper is `#F5F7F6`, a grey with a hint of teal, not the shipped guide's warm cream. Pure white is for `backend` node fills only.
- **Dot pattern is optional, not default**: the 22×22 dot pattern is an opt-in "dotted paper" variant (it reads like a chart grid, which suits Bearing's navigation theme on hero diagrams). The default background is a clean `paper` fill. When enabled, the dots sit at ~10% opacity of `ink` on `paper`.
- **Container is clean by default**: the diagram sits directly on the page paper, no secondary container background or border. A framed variant (`paper-2` bg + `rule` border + 8px radius + padding) is available as an opt-in for card-heavy layouts.

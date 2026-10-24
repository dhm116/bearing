---
name: Bearing
description: One live, sourced answer for every engineer and every agent.
colors:
  ink: "#18242A"
  accent: "#1D5E74"
  accent-soft: "#E2EEF1"
  background: "#F5F7F6"
  surface: "#FFFFFF"
  sunk: "#EDF1F0"
  muted: "#55646B"
  soft: "#6B7C82"
  line: "#D3DCDA"
  on-accent: "#FFFFFF"
  good: "#2F6B45"
  good-soft: "#E3F0E7"
  warn: "#A5561A"
  warn-soft: "#F7EBDF"
  bad: "#A33A3A"
  bad-soft: "#F6E3E3"
  term-bg: "#0F171A"
  term-ink: "#D6E2E0"
typography:
  display:
    fontFamily: "Bricolage Grotesque, Segoe UI, system-ui, sans-serif"
    fontSize: "clamp(2.6rem, 1.4rem + 4.4vw, 5.25rem)"
    fontWeight: 700
    lineHeight: 1.02
    letterSpacing: "-0.035em"
  headline:
    fontFamily: "Bricolage Grotesque, Segoe UI, system-ui, sans-serif"
    fontSize: "clamp(2rem, 1.3rem + 2.4vw, 3rem)"
    fontWeight: 700
    lineHeight: 1.12
    letterSpacing: "-0.03em"
  title:
    fontFamily: "Bricolage Grotesque, Segoe UI, system-ui, sans-serif"
    fontSize: "clamp(1.625rem, 1.2rem + 1.4vw, 2.125rem)"
    fontWeight: 700
    lineHeight: 1.12
    letterSpacing: "-0.02em"
  body:
    fontFamily: "IBM Plex Sans, Segoe UI, system-ui, sans-serif"
    fontSize: "1.0625rem"
    fontWeight: 400
    lineHeight: 1.65
  lede:
    fontFamily: "IBM Plex Sans, Segoe UI, system-ui, sans-serif"
    fontSize: "1.25rem"
    fontWeight: 400
    lineHeight: 1.55
  label:
    fontFamily: "IBM Plex Mono, ui-monospace, Menlo, monospace"
    fontSize: "0.75rem"
    fontWeight: 500
    lineHeight: 1.4
    letterSpacing: "0.08em"
  code:
    fontFamily: "IBM Plex Mono, ui-monospace, Menlo, monospace"
    fontSize: "0.88em"
    fontWeight: 400
rounded:
  sm: "4px"
  md: "8px"
  lg: "14px"
  pill: "999px"
spacing:
  3xs: "0.125rem"
  2xs: "0.25rem"
  xs: "0.5rem"
  sm: "0.75rem"
  md: "1rem"
  lg: "1.5rem"
  xl: "2.5rem"
  2xl: "4rem"
  3xl: "6rem"
  4xl: "9rem"
components:
  button:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    rounded: "{rounded.pill}"
    padding: "0 1.15rem"
    height: "2.75rem"
  button-primary:
    backgroundColor: "{colors.accent}"
    textColor: "{colors.on-accent}"
    rounded: "{rounded.pill}"
    padding: "0 1.15rem"
    height: "2.75rem"
  button-primary-hover:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.background}"
  chip:
    backgroundColor: "{colors.sunk}"
    textColor: "{colors.muted}"
    typography: "{typography.label}"
    rounded: "{rounded.sm}"
    padding: "0.15rem 0.55rem"
  chip-good:
    backgroundColor: "{colors.good-soft}"
    textColor: "{colors.good}"
  nav-link:
    textColor: "{colors.ink}"
    rounded: "{rounded.pill}"
    padding: "0.45rem 0.7rem"
  nav-link-current:
    backgroundColor: "{colors.accent-soft}"
    textColor: "{colors.accent}"
  search-pill:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.muted}"
    rounded: "{rounded.pill}"
    height: "2.5rem"
  card:
    backgroundColor: "{colors.surface}"
    rounded: "{rounded.md}"
    padding: "1rem 1.5rem"
---

# Design System: Bearing

## Overview

**Creative North Star: "The Sourced Answer"**

Bearing's surfaces read like a well-kept reference: a plain question, a
direct answer, and the citation right beside it. The page is cool paper
tinted toward the brand's deep sea teal; ink is near-black with the same
lean, so every grey looks deliberate next to the mark. Decoration earns its
place only when it carries information: a status colour on a verdict, a
dashed line for a footstep, a mono label naming where something came from.

Density is calm. Running text keeps a reading measure (46rem), headings in
Bricolage Grotesque are rare and confident, and IBM Plex carries everything
else. Mono is the voice of provenance: times, file names, sources, labels
and code. Controls are precise and unhurried: pills and small radii, hairline
rules, quick ease-out transitions, nothing that shouts.

The system follows the reader's light or dark setting everywhere, as the
logo and diagrams do. Depth is softly layered: tonal steps between
background, surface and sunk do most of the work, hairline rules separate,
and gentle ambient shadows lift what floats.

**Key Characteristics:**
- Cool, teal-leaning neutrals; one accent, used sparingly.
- Status colours (good, warn, bad) only for state, never as decoration.
- Mono for provenance and labels; display type only for titles and headings.
- Pills for actions, small radii for data, larger radii for figures and dialogs.
- Light and dark from one token set via `prefers-color-scheme`.

## Colors

A restrained, cool palette: one deep sea teal against neutrals that lean
toward it, with a separate status set.

### Primary
- **Deep Sea Teal** (`accent`): links, the primary button, the current nav
  item, focus rings, the mark's arrow and pin, the dot on the wordmark's i.
  In dark mode it lightens to a pale harbour teal (#7FC0D6).
- **Sea Mist** (`accent-soft`): selected and current backgrounds (nav,
  docs index, search results), text selection.

### Neutral
- **Chart Ink** (`ink`): body text and headings; the mark's bowl and stem.
- **Cool Paper** (`background`): the page.
- **Clean Sheet** (`surface`): cards, panels, code blocks, the footer, dialogs.
- **Inset Grey** (`sunk`): inset areas, table headers, chips, hover fills.
- **Slate Note** (`muted`): secondary text, captions, labels, metadata.
- **Soft Slate** (`soft`): dashed connectors, quiet hover borders.
- **Hairline** (`line`): borders and dividers (`1px solid`).

### Status
- **Found Green** (`good` / `good-soft`), **Partial Amber** (`warn` /
  `warn-soft`), **Dead-end Red** (`bad` / `bad-soft`): verdicts, roadmap
  status, ADR status chips. Each pairs a strong ink with a soft fill.

### Terminal
- **Night Console** (`term-bg`) with `term-ink`: CLI output and terminal
  mockups (also `term-dim` #7C8F94, `term-accent` #7FC0D6, `term-ok`
  #8DCB9F, `term-warn` #E3A56E).

### Named Rules
**The One Accent Rule.** Teal is the only brand colour. It marks what you
can act on or where you are; it is never a fill for large areas.

**The State-Only Rule.** Green, amber and red mean a state (found, partial,
dead end, accepted, proposed). Never use them for emphasis or decoration.

**The Mark Is Fixed Rule.** Use the SVGs in `docs/brand/`; never recolour
the ink parts in the accent, rotate the mark, or add outlines, shadows or
gradients. Never below 16px.

## Typography

**Display Font:** Bricolage Grotesque (with Segoe UI, system-ui)
**Body Font:** IBM Plex Sans (with Segoe UI, system-ui)
**Label/Mono Font:** IBM Plex Mono (with ui-monospace, Menlo)

**Character:** A characterful grotesque for the few words that title a page,
set tight (−0.02 to −0.035em) and balanced; a sober, technical sans for
reading; a mono that says "this came from somewhere".

### Hierarchy
- **Display** (700, clamp 2.6–5.25rem, 1.02): home hero and 404 only, max 14ch.
- **Headline** (700, clamp 2–3rem, 1.12): doc titles and home section titles.
- **Title** (700, clamp 1.625–2.125rem): prose `h2`, ruled above; `h3` 1.5rem, `h4` 1.25rem.
- **Lede** (400, 1.25rem, 1.55, muted): the paragraph under a title, max 40rem.
- **Body** (400, 1.0625rem, 1.65): running text, max 46rem measure.
- **Label** (500 mono, 0.75rem, 0.08em, uppercase): section eyebrows, docs
  index heading, table headers (0.06em), search result sections (0.6875rem).

### Named Rules
**The Sparing Display Rule.** Bricolage Grotesque is for the wordmark, page
titles and section headings only. Never for body, buttons or labels.

**The Mono Means Source Rule.** Times, file paths, sources, counts and labels
are mono with tabular numerals; prose is never mono.

## Layout

A centred shell up to 104rem with a fluid gutter (clamp 1–2.75rem).
Spacing follows a 4pt scale from 0.125rem to 9rem; sections are separated by
`3xl` (6rem). The sticky nav is 3.75rem tall.

Breakpoints are content-driven: 40rem (two-column trails, full search pill),
60rem (hero splits 7:5, footer splits), 64rem (desktop nav; docs gain a
15.5rem sticky index), 72rem (three-column trails), 88rem (docs gain a 15rem
"on this page" rail). On narrow screens the article comes before the docs
index, and search becomes a full-screen dialog. Pages never scroll
horizontally.

## Elevation & Depth

Softly layered. Most depth comes from tonal steps (background → surface →
sunk) and hairline rules. Ambient shadows are soft, low-contrast and tinted
with the ink colour, with a negative spread so they read as a lift, not an
outline. Today they appear on floating layers only (the menu and the search
dialog); gentle ambient lift on cards is allowed by this system but not yet
used on the site.

### Shadow Vocabulary
- **Menu lift** (`box-shadow: 0 12px 32px -16px var(--shadow)`): dropdown menus.
- **Dialog lift** (`box-shadow: 0 24px 64px -24px var(--shadow)`): the search
  dialog, over a scrim (`--scrim`).
- `--shadow` is `rgb(24 36 42 / 0.08)` light, `rgb(0 0 0 / 0.35)` dark.

### Named Rules
**The Quiet Lift Rule.** A shadow is never the only edge: pair it with a
hairline rule, keep it at or below the menu lift on cards, and never stack
shadows.

## Shapes

Three radii and a pill. Small (4px) for data: chips, kbd, inline code, docs
index items. Medium (8px) for containers: cards, menus, blockquotes, inline
tables of contents. Large (14px) for figures and the search dialog. Pills
for anything you press or type into: buttons, nav links, the search pill,
verdict badges. Borders are 1px hairlines; a 4px top border in a status
colour marks a card's verdict.

## Components

### Buttons
Precise and unhurried.
- **Shape:** pill (999px), 2.75rem min height, 0 1.15rem padding, 500 weight body font.
- **Default:** clean sheet fill, hairline border, ink text; hover turns the border teal.
- **Primary:** teal fill and border, white text; hover swaps to ink fill with paper text.
- **Active:** nudges down 1px. Transitions 120ms ease-out.

### Chips
- **Style:** mono 0.75rem, 0.04em tracking, 4px radius, inset-grey fill, hairline border, muted text.
- **State:** status variants use the soft fill with the strong ink and no border; retired states go transparent.

### Cards / Containers
- **Corner Style:** 8px (figures 14px).
- **Background:** clean sheet on cool paper.
- **Shadow Strategy:** flat at rest today; see Elevation.
- **Border:** hairline; verdict cards add a 4px status-coloured top border.
- **Internal Padding:** 1rem × 1.5rem.

### Navigation
- **Top bar:** sticky, paper background, hairline bottom rule; wordmark left,
  centred search pill, pill links right. Current page uses sea mist with
  teal text; hover fills inset grey. Below 64rem the links fold into a
  "Menu" pill opening a menu card.
- **Docs index:** mono uppercase heading, numbered items with tabular mono
  numerals, current item sea mist and 600 weight.
- **On this page:** muted links with a 2px left rule that turns teal on the
  current section.

### Search Pill and Dialog
A pill-shaped trigger showing the shortcut in `kbd`, opening a centred
dialog (40rem, 14px radius, dialog lift, scrim). Results highlight matches
in teal and mark the selected row in sea mist. Full screen on phones.

### Verdict Trail (signature)
The home page's "hunt": cards in sequence, each with a mono time, a tool
name in display type, what was found, and a pill verdict in a status
colour. A dashed soft-slate connector joins cards when stacked. It shows the
problem honestly before Bearing's sourced answer.

## Do's and Don'ts

### Do:
- **Do** take every colour and font from the tokens in `site/static/css/tokens.css`, which mirror `docs/brand/README.md`; change the brand file first.
- **Do** show where an answer came from, in mono, next to the answer.
- **Do** keep running text to the 46rem measure and leave tables and code the full column.
- **Do** design light and dark together; check both.
- **Do** honour `prefers-reduced-motion`; keep transitions at 120–420ms with the ease-out curve.
- **Do** keep a visible 2px teal focus ring offset 2px.

### Don't:
- **Don't** use green, amber or red for anything but state.
- **Don't** set body, buttons or labels in Bricolage Grotesque.
- **Don't** redraw, recolour, rotate or decorate the mark, or show it under 16px.
- **Don't** use the animated mark as a loading spinner or more than once per page.
- **Don't** fill large areas with the accent.
- **Don't** rely on a shadow alone for an edge, or stack shadows.

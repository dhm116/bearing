# Bearing brand

The logo, colors and type for Bearing. Use these files and values for the
README, docs sites, the CLI, the PR bot's avatar and any web surface, so
every place Bearing appears looks like one product.

## The mark

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="bearing-mark-dark.svg">
  <img src="bearing-mark-light.svg" alt="Bearing logo: a lowercase b whose stem ends in a north arrow" width="96">
</picture>

The mark is a lowercase **b**. Its stem ends in a north arrow and its bowl
is a compass ring with a center pin. It stands for getting your bearings:
knowing where you are in an engineering organization, what a service is,
who owns it and what changed.

### Files

| File | Use |
| --- | --- |
| [`bearing-mark.svg`](bearing-mark.svg) | Default. Switches between the light and dark colors with the viewer's system setting |
| [`bearing-mark-light.svg`](bearing-mark-light.svg) | Fixed colors, for light backgrounds |
| [`bearing-mark-dark.svg`](bearing-mark-dark.svg) | Fixed colors, for dark backgrounds |
| [`bearing-mark-animated.svg`](bearing-mark-animated.svg) | 7.4-second intro that plays once (see [Motion](#motion)) |

On GitHub, use a `<picture>` element with the light and dark files, as this
page and the top-level README do. GitHub serves SVGs as images, so the
default file can't see the reader's GitHub theme, only their system setting.

### Construction

The mark is drawn on a 64 × 64 grid.

| Part | Geometry | Color |
| --- | --- | --- |
| Bowl | Circle, center (34, 42), radius 15, stroke 6.5 | Ink |
| Stem | Vertical line at x = 19, from y = 20 to y = 42, stroke 6.5 | Ink |
| North arrow | Triangle (19, 4), (27.5, 21), (10.5, 21) | Accent |
| Center pin | Circle, center (34, 42), radius 4 | Accent |

The stem meets the bowl at the bowl's left edge. Only the arrow and the pin
use the accent color.

### Using the mark

- Leave clear space around the mark equal to the bowl's stroke width
  doubled (13 units on the 64 grid, about a fifth of the mark's height).
- Don't show the mark smaller than 16 px. From 16 to 24 px, use the mark
  alone, without the wordmark.
- Use the light or dark colors below. Don't recolor the ink parts in the
  accent color, and don't add outlines, shadows or gradients.
- Don't rotate the mark. The arrow always points up.

## Wordmark

The wordmark is **bearing**, all lowercase, set in Bricolage Grotesque Bold
with letter spacing of −0.02 em. The dot on the i is set in the accent
color. When the wordmark sits next to the mark, make the mark about 1.2
times the wordmark's cap height and leave a gap of about a third of the
mark's width between them.

In plain-text places such as the CLI, a commit message or a Markdown heading,
write the name as "Bearing" with a capital B in running text and `bearing`
for commands.

## Colors

The accent is a deep sea teal. Neutrals lean slightly toward it, so greys
look intentional next to the mark.

### Core

| Token | Light | Dark | Use |
| --- | --- | --- | --- |
| `ink` | `#18242A` | `#E4ECEA` | Text, and the bowl and stem of the mark |
| `accent` | `#1D5E74` | `#7FC0D6` | Links, primary buttons, and the mark's arrow and pin |
| `accent-soft` | `#E2EEF1` | `#1B3740` | Tag and highlight backgrounds |
| `background` | `#F5F7F6` | `#11191C` | Page background |
| `surface` | `#FFFFFF` | `#172226` | Cards, panels, code blocks |
| `sunk` | `#EDF1F0` | `#131C1F` | Inset areas, table headers |
| `muted` | `#55646B` | `#98A8AD` | Secondary text, captions, labels |
| `line` | `#D3DCDA` | `#2A383D` | Borders and dividers |

### Status

Status colors are separate from the accent. Use them only for state, such as
policy results, standards checks and incident severity.

| Token | Light | Dark | Soft light | Soft dark |
| --- | --- | --- | --- | --- |
| `good` | `#2F6B45` | `#8DCB9F` | `#E3F0E7` | `#1C3325` |
| `warn` | `#A5561A` | `#E3A56E` | `#F7EBDF` | `#3A2819` |
| `bad` | `#A33A3A` | `#E89A9A` | `#F6E3E3` | `#3B1F1F` |

### Terminal

For CLI output, screenshots and terminal mockups.

| Token | Value | Use |
| --- | --- | --- |
| `term-bg` | `#0F171A` | Background |
| `term-ink` | `#D6E2E0` | Default text |
| `term-dim` | `#7C8F94` | Prompts, hints, provenance notes |
| `term-accent` | `#7FC0D6` | Entity names |
| `term-ok` | `#8DCB9F` | Success, high confidence |
| `term-warn` | `#E3A56E` | Warnings, open incidents |

### CSS

```css
:root {
  --ink: #18242A; --accent: #1D5E74; --accent-soft: #E2EEF1;
  --background: #F5F7F6; --surface: #FFFFFF; --sunk: #EDF1F0;
  --muted: #55646B; --line: #D3DCDA;
  --good: #2F6B45; --warn: #A5561A; --bad: #A33A3A;
}
@media (prefers-color-scheme: dark) {
  :root {
    --ink: #E4ECEA; --accent: #7FC0D6; --accent-soft: #1B3740;
    --background: #11191C; --surface: #172226; --sunk: #131C1F;
    --muted: #98A8AD; --line: #2A383D;
    --good: #8DCB9F; --warn: #E3A56E; --bad: #E89A9A;
  }
}
```

## Type

All three families are free on Google Fonts under the SIL Open Font License.

| Role | Family | Weights | Use |
| --- | --- | --- | --- |
| Display | [Bricolage Grotesque](https://fonts.google.com/specimen/Bricolage+Grotesque) | 500, 700 | The wordmark and headings |
| Body | [IBM Plex Sans](https://fonts.google.com/specimen/IBM+Plex+Sans) | 400, 500, 600 | Running text and UI |
| Mono | [IBM Plex Mono](https://fonts.google.com/specimen/IBM+Plex+Mono) | 400, 500 | Code, CLI output, labels and small uppercase eyebrows |

Fallback stacks:

```css
--display: "Bricolage Grotesque", "Segoe UI", system-ui, sans-serif;
--body: "IBM Plex Sans", "Segoe UI", system-ui, sans-serif;
--mono: "IBM Plex Mono", ui-monospace, Menlo, monospace;
```

Use Bricolage Grotesque sparingly: the wordmark, page titles and section
headings. Set uppercase mono labels with about 0.1 em letter spacing.

## Motion

[`bearing-mark-animated.svg`](bearing-mark-animated.svg) is an intro for
splash screens, docs landing pages and talks. It's a single SVG with CSS
keyframes and no scripts.

1. **Start on a heading.** A compass ring with a north notch and a needle at 040°.
2. **Search.** The needle snaps between headings, like someone turning to get oriented.
3. **Spin up.** The needle spins faster and faster.
4. **Collapse.** The needle shrinks into the hub while the ring shrinks into the bowl of the b.
5. **Hand-off.** The north notch sweeps clockwise around the ring and reaches the bowl's left edge just as the stem shoots up from it.
6. **Arrive.** The north arrow pops out on top and the mark holds.

The animation follows the viewer's light or dark system setting. With
reduced motion turned on, it shows the finished mark straight away. Use it
once per page, never as a loading spinner.

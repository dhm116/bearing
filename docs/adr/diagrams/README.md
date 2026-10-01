# ADR diagrams

The SVGs here are the diagrams embedded in ADRs 6–10 and
[`docs/architecture-proposed.md`](../../architecture-proposed.md). They follow
the [diagram-design](https://github.com/cathrynlavery/diagram-design) skill
and the Bearing profile in
[`docs/brand/diagram-design/bearing.md`](../../brand/diagram-design/bearing.md).

Each SVG is self-contained: its own styles, light and dark colors (it follows
the reader's `prefers-color-scheme`), and the IBM Plex glyphs it uses
embedded as a font subset. That is what lets it render on GitHub, where an
image can't load web fonts or page CSS.

## Editing a diagram

Don't edit the SVGs by hand. Coordinates are placed by hand in
[`src/diagrams.py`](src/diagrams.py) (one function per diagram), and
[`src/ddlib.py`](src/ddlib.py) draws the primitives: nodes, zones, orthogonal
arrows with rounded corners, masked labels and the legend. It warns when
text is likely to overflow its box.

Regenerate after a change:

```sh
python3 -m pip install fonttools brotli
python3 docs/adr/diagrams/src/fetch_fonts.py /tmp/plex   # IBM Plex from Google Fonts (OFL)
python3 docs/adr/diagrams/src/export_svg.py docs/adr/diagrams /tmp/plex
```

Then check the result in a browser in light and dark mode. The skill's
`scripts/verify-geometry.py` and `scripts/self_check.py` catch labels
clipped by nodes and missing accessibility markup; run them on one diagram
at a time.

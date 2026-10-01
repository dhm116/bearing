"""Tiny helper for hand-placed diagram-design SVGs in the Bearing skin.

Colors come from CSS classes (see STYLE) so one SVG works in light and dark.
Coordinates are placed by hand; this only draws primitives and warns when text
is likely to overflow its box.
"""
import html, math, sys

W_SANS12 = 7.0      # IBM Plex Sans 600 @12px, per char (generous)
W_MONO9 = 5.4
W_LBL = 5.28        # mono 8px + 0.06em tracking
W_TAG = 4.76        # mono 7px + 0.08em tracking
W_LIST = 6.2        # sans 500 @11px


def c4(v):
    return int(math.ceil(v / 4.0) * 4)


def esc(s):
    return html.escape(s, quote=True)


class D:
    def __init__(self, slug, w, h, title, desc):
        self.slug, self.w, self.h, self.title, self.desc = slug, w, h, title, desc
        self.zones, self.arrows, self.labels, self.nodes, self.extra = [], [], [], [], []
        self.legend_items = []

    def warn(self, msg):
        print(f"[{self.slug}] WARN {msg}", file=sys.stderr)

    # ---------- zones ----------
    def zone(self, x, y, w, h, label):
        lw = c4(len(label) * 7 * 0.74 + 12)
        self.zones.append(
            f'<rect class="zone" x="{x}" y="{y}" width="{w}" height="{h}" rx="8"/>'
            f'<rect class="mask" x="{x+12}" y="{y+4}" width="{lw}" height="12" rx="2"/>'
            f'<text class="t-zone" x="{x+12+lw/2}" y="{y+13}" text-anchor="middle">{esc(label)}</text>')

    # ---------- arrows ----------
    def path(self, pts, kind="", marker=True, r=8):
        d = f"M {pts[0][0]},{pts[0][1]}"
        n = len(pts)
        for i in range(1, n - 1):
            (px, py), (cx, cy), (nx, ny) = pts[i - 1], pts[i], pts[i + 1]
            lin = abs(cx - px) + abs(cy - py)
            lout = abs(nx - cx) + abs(ny - cy)
            if px != cx and py != cy or cx != nx and cy != ny:
                self.warn(f"diagonal segment near {pts[i]}")
            rr = min(r, lin if i == 1 else lin / 2, lout if i == n - 2 else lout / 2)
            dix, diy = (cx - px) / lin, (cy - py) / lin
            dox, doy = (nx - cx) / lout, (ny - cy) / lout
            d += f" L {cx - dix*rr:g},{cy - diy*rr:g} Q {cx},{cy} {cx + dox*rr:g},{cy + doy*rr:g}"
        d += f" L {pts[-1][0]},{pts[-1][1]}"
        if n == 2 and pts[0][0] != pts[1][0] and pts[0][1] != pts[1][1]:
            self.warn(f"diagonal line {pts}")
        mk = ""
        if marker:
            k = kind.split()[0] if kind and kind.split()[0] in ("acc", "link", "bad") else "m"
            mk = f' marker-end="url(#{self.slug}-{k})"'
        self.arrows.append(f'<path class="a {kind}" d="{d}"{mk}/>')

    def label(self, cx, cy, text, kind=""):
        """Arrow label centred at (cx, cy); the mask is 12px tall."""
        w = c4(len(text) * W_LBL + 12)
        x = cx - w / 2
        self.labels.append(
            f'<rect class="mask" x="{x:g}" y="{cy-6}" width="{w}" height="12" rx="2"/>'
            f'<text class="t-lbl {kind}" x="{cx}" y="{cy+3}" text-anchor="middle">{esc(text)}</text>')
        return x, w

    # ---------- nodes ----------
    def node(self, x, y, w, h, kind, tag, name, subs=(), pin=False, sub_cls="t-sub"):
        if isinstance(subs, str):
            subs = [subs]
        cx = x + w / 2
        out = [f'<g class="k-{kind}">',
               f'<rect class="mask" x="{x}" y="{y}" width="{w}" height="{h}" rx="6"/>',
               f'<rect class="box n-{kind}" x="{x}" y="{y}" width="{w}" height="{h}" rx="6"/>']
        if tag:
            tw = c4(len(tag) * W_TAG + 10)
            out.append(f'<rect class="chip" x="{x+8}" y="{y+8}" width="{tw}" height="12" rx="2"/>'
                       f'<text class="t-tag" x="{x+8+tw/2}" y="{y+16.5}" text-anchor="middle">{esc(tag)}</text>')
        if tag:
            top = y + 36
        else:
            block = 9 + (16 + 13 * (len(subs) - 1) if subs else 0)
            top = y + (h - block) / 2 + 9
        if len(name) * W_SANS12 > w - 16:
            self.warn(f"name overflow '{name}' {len(name)*W_SANS12:.0f}>{w-16}")
        out.append(f'<text class="t-name" x="{cx}" y="{top}" text-anchor="middle">{esc(name)}</text>')
        if pin:
            nw = len(name) * 6.6
            out.append(f'<circle class="pin" cx="{cx + nw/2 + 8:g}" cy="{top-4}" r="3"/>')
        for i, s in enumerate(subs):
            if len(s) * W_MONO9 > w - 16:
                self.warn(f"sub overflow '{s}' {len(s)*W_MONO9:.0f}>{w-16}")
            out.append(f'<text class="{sub_cls}" x="{cx}" y="{top + 16 + 13*i}" text-anchor="middle">{esc(s)}</text>')
        last = top + 16 + 13 * (len(subs) - 1) if subs else top
        if last + 10 > y + h:
            self.warn(f"text runs past bottom of '{name}' ({last+10}>{y+h})")
        out.append('</g>')
        self.nodes.append("".join(out))

    def card(self, x, y, w, kind, tag, name, sub, rows, key_w=None):
        """A node with a left-aligned list of rows: (left, right) pairs.
        left is sans (a name) or mono when key_w is given (key: value)."""
        h = c4(52 + (16 if sub else 0) + 18 * len(rows) + 4)
        out = [f'<g class="k-{kind}">',
               f'<rect class="mask" x="{x}" y="{y}" width="{w}" height="{h}" rx="6"/>',
               f'<rect class="box n-{kind}" x="{x}" y="{y}" width="{w}" height="{h}" rx="6"/>']
        tw = c4(len(tag) * W_TAG + 10)
        out.append(f'<rect class="chip" x="{x+8}" y="{y+8}" width="{tw}" height="12" rx="2"/>'
                   f'<text class="t-tag" x="{x+8+tw/2}" y="{y+16.5}" text-anchor="middle">{esc(tag)}</text>')
        out.append(f'<text class="t-name" x="{x+16}" y="{y+40}">{esc(name)}</text>')
        ry = y + 40
        if sub:
            ry += 15
            out.append(f'<text class="t-sub" x="{x+16}" y="{ry}">{esc(sub)}</text>')
        ry += 10
        out.append(f'<line class="hair" x1="{x+16}" y1="{ry}" x2="{x+w-16}" y2="{ry}"/>')
        for i, (l, r) in enumerate(rows):
            yy = ry + 18 + 18 * i
            rc = ""
            if isinstance(r, tuple):
                r, rc = r
            if key_w:
                out.append(f'<text class="t-key" x="{x+16}" y="{yy}">{esc(l)}</text>')
                out.append(f'<text class="t-val {rc}" x="{x+16+key_w}" y="{yy}">{esc(r)}</text>')
                if (key_w + len(r) * W_MONO9) > w - 32:
                    self.warn(f"row overflow '{r}'")
            else:
                out.append(f'<text class="t-list" x="{x+16}" y="{yy}">{esc(l)}</text>')
                if r:
                    out.append(f'<text class="t-sub" x="{x+w-16}" y="{yy}" text-anchor="end">{esc(r)}</text>')
                if len(l) * W_LIST + len(r) * W_MONO9 > w - 40:
                    self.warn(f"row overflow '{l} {r}'")
        out.append('</g>')
        self.nodes.append("".join(out))
        return h

    def diamond(self, cx, cy, w, h, kind, name, subs=()):
        pts = f"{cx},{cy-h/2} {cx+w/2},{cy} {cx},{cy+h/2} {cx-w/2},{cy}"
        out = [f'<g class="k-{kind}"><polygon class="mask" points="{pts}"/><polygon class="box n-{kind}" points="{pts}"/>',
               f'<text class="t-name" x="{cx}" y="{cy+ (0 if subs else 4)}" text-anchor="middle">{esc(name)}</text>']
        for i, s in enumerate(subs):
            out.append(f'<text class="t-sub" x="{cx}" y="{cy+14+12*i}" text-anchor="middle">{esc(s)}</text>')
        out.append('</g>')
        self.nodes.append("".join(out))

    def raw(self, s, layer="extra"):
        getattr(self, layer).append(s)

    def text(self, x, y, s, cls, anchor="start"):
        self.extra.append(f'<text class="{cls}" x="{x}" y="{y}" text-anchor="{anchor}">{esc(s)}</text>')

    # ---------- legend ----------
    def legend(self, items):
        """items: ('node', kind, text) | ('arrow', kind, text)"""
        self.legend_items = items

    def _legend_svg(self):
        if not self.legend_items:
            return ""
        y = self.h - 36
        out = [f'<line class="hair" x1="24" y1="{y-12}" x2="{self.w-24}" y2="{y-12}"/>',
               f'<text class="t-zone" x="24" y="{y+12}">LEGEND</text>']
        x = 96
        for kind, cls, text in self.legend_items:
            if kind == "node":
                out.append(f'<g class="k-{cls}"><rect class="box n-{cls}" x="{x}" y="{y+2}" width="20" height="12" rx="3"/></g>')
            elif kind == "zone":
                out.append(f'<rect class="zone" x="{x}" y="{y+2}" width="20" height="12" rx="3"/>')
            else:
                k = cls.split()[0] if cls and cls.split()[0] in ("acc", "link", "bad") else "m"
                out.append(f'<path class="a {cls}" d="M {x},{y+8} L {x+22},{y+8}" marker-end="url(#{self.slug}-{k})"/>')
            out.append(f'<text class="t-legend" x="{x+30}" y="{y+12}">{esc(text)}</text>')
            x += 30 + len(text) * 5.6 + 28
        if x > self.w:
            self.warn(f"legend too wide {x}")
        return "".join(out)

    def svg(self):
        s = self.slug
        markers = "".join(
            f'<marker id="{s}-{k}" markerWidth="8" markerHeight="6" refX="7" refY="3" orient="auto">'
            f'<polygon class="mk-{k}" points="0 0, 8 3, 0 6"/></marker>' for k in ("m", "acc", "link", "bad"))
        return (f'<svg class="dd" viewBox="0 0 {self.w} {self.h}" style="min-width:{self.w}px" '
                f'xmlns="http://www.w3.org/2000/svg" role="img" aria-labelledby="{s}-title {s}-desc">'
                f'<title id="{s}-title">{esc(self.title)}</title><desc id="{s}-desc">{esc(self.desc)}</desc>'
                f'<defs>{markers}</defs>'
                f'<rect class="bg" x="0" y="0" width="{self.w}" height="{self.h}"/>'
                + "\n<!-- zones -->\n" + "\n".join(self.zones)
                + "\n<!-- arrows -->\n" + "\n".join(self.arrows)
                + "\n<!-- labels -->\n" + "\n".join(self.labels)
                + "\n<!-- nodes -->\n" + "\n".join(self.nodes)
                + "\n" + "\n".join(self.extra)
                + self._legend_svg() + "</svg>")


STYLE = r"""
svg.dd { display:block; width:100%; height:auto; }
.dd .bg, .dd .mask { fill: var(--paper); }
.dd .zone { fill: color-mix(in srgb, var(--ink) 2%, transparent); stroke: color-mix(in srgb, var(--ink) 12%, transparent); stroke-width: .8; }
.dd .hair { stroke: var(--rule); stroke-width: .8; }
.dd .box { stroke-width: 1; }
.dd .n-backend { fill: var(--surface); stroke: var(--ink); }
.dd .n-focal { fill: var(--accent-tint); stroke: var(--accent); stroke-width: 1.2; }
.dd .n-store { fill: color-mix(in srgb, var(--ink) 5%, transparent); stroke: var(--muted); }
.dd .n-external { fill: color-mix(in srgb, var(--ink) 3%, transparent); stroke: color-mix(in srgb, var(--ink) 30%, transparent); }
.dd .n-input { fill: color-mix(in srgb, var(--muted) 10%, transparent); stroke: var(--soft); }
.dd .n-optional { fill: color-mix(in srgb, var(--ink) 2%, transparent); stroke: color-mix(in srgb, var(--ink) 30%, transparent); stroke-dasharray: 4 3; }
.dd .n-security { fill: color-mix(in srgb, var(--accent) 5%, transparent); stroke: color-mix(in srgb, var(--accent) 50%, transparent); stroke-dasharray: 4 4; }
.dd .n-bad { fill: var(--bad-soft); stroke: var(--bad); }
.dd .n-warn { fill: var(--warn-soft); stroke: var(--warn); }
.dd .chip { fill: none; stroke-width: .8; stroke: color-mix(in srgb, var(--ink) 35%, transparent); }
.dd .t-tag { font: 500 7px var(--font-mono); letter-spacing: .08em; fill: var(--ink); }
.dd .k-focal .chip { stroke: color-mix(in srgb, var(--accent) 50%, transparent); } .dd .k-focal .t-tag { fill: var(--accent); }
.dd .k-store .chip { stroke: color-mix(in srgb, var(--muted) 50%, transparent); } .dd .k-store .t-tag { fill: var(--muted); }
.dd .k-external .t-tag, .dd .k-input .t-tag, .dd .k-optional .t-tag { fill: var(--soft); }
.dd .k-external .chip, .dd .k-input .chip, .dd .k-optional .chip { stroke: color-mix(in srgb, var(--soft) 45%, transparent); }
.dd .k-security .chip { stroke: color-mix(in srgb, var(--accent) 45%, transparent); } .dd .k-security .t-tag { fill: var(--accent); }
.dd .k-bad .chip { stroke: color-mix(in srgb, var(--bad) 50%, transparent); } .dd .k-bad .t-tag, .dd .k-bad .t-name { fill: var(--bad); }
.dd .k-warn .chip { stroke: color-mix(in srgb, var(--warn) 50%, transparent); } .dd .k-warn .t-tag { fill: var(--warn); }
.dd .t-name { font: 600 12px var(--font-sans); fill: var(--ink); }
.dd .t-list { font: 500 11px var(--font-sans); fill: var(--ink); }
.dd .t-sub, .dd .t-val { font: 400 9px var(--font-mono); fill: var(--muted); }
.dd .t-key { font: 500 9px var(--font-mono); fill: var(--soft); }
.dd .t-val.bad { fill: var(--bad); font-weight: 500; } .dd .t-val.warn { fill: var(--warn); font-weight: 500; }
.dd .t-lbl { font: 400 8px var(--font-mono); letter-spacing: .06em; fill: var(--muted); }
.dd .t-lbl.acc { fill: var(--accent); } .dd .t-lbl.link { fill: var(--link); } .dd .t-lbl.bad { fill: var(--bad); }
.dd .t-zone { font: 500 7px var(--font-mono); letter-spacing: .14em; fill: color-mix(in srgb, var(--ink) 50%, transparent); }
.dd .t-eyebrow { font: 500 8px var(--font-mono); letter-spacing: .12em; fill: var(--soft); }
.dd .t-legend { font: 400 10px var(--font-sans); fill: var(--muted); }
.dd .t-callout { font: italic 400 13px var(--font-sans); fill: var(--muted); }
.dd .a { fill: none; stroke: var(--muted); stroke-width: 1.2; }
.dd .a.acc { stroke: var(--accent); stroke-width: 1.4; }
.dd .a.link { stroke: var(--link); }
.dd .a.bad { stroke: var(--bad); }
.dd .a.dash { stroke-dasharray: 4 3; stroke-width: 1; }
.dd .mk-m { fill: var(--muted); } .dd .mk-acc { fill: var(--accent); } .dd .mk-link { fill: var(--link); } .dd .mk-bad { fill: var(--bad); }
.dd .pin { fill: var(--accent); }
.dd .life { stroke: color-mix(in srgb, var(--ink) 22%, transparent); stroke-width: 1; stroke-dasharray: 3 3; }
.dd .act { fill: color-mix(in srgb, var(--ink) 6%, var(--paper)); stroke: var(--muted); stroke-width: .8; }
.dd .frame { fill: none; stroke: color-mix(in srgb, var(--ink) 22%, transparent); stroke-width: 1; }
.dd .frame-tab { fill: var(--paper); stroke: color-mix(in srgb, var(--ink) 22%, transparent); stroke-width: 1; }
.dd .guard { font: 400 8px var(--font-mono); letter-spacing: .04em; fill: var(--muted); }
"""

TOKENS = r"""
:root {
  --paper:#F5F7F6; --paper-2:#EDF1F0; --surface:#FFFFFF; --ink:#18242A; --muted:#55646B; --soft:#6B7C82;
  --rule:rgba(24,36,42,.12); --rule-solid:#D3DCDA; --accent:#1D5E74; --accent-tint:rgba(29,94,116,.08);
  --link:#4B5198; --warn:#A5561A; --warn-soft:#F7EBDF; --bad:#A33A3A; --bad-soft:#F6E3E3;
  --font-display:'Bricolage Grotesque','IBM Plex Sans',system-ui,sans-serif;
  --font-sans:'IBM Plex Sans',system-ui,sans-serif;
  --font-mono:'IBM Plex Mono',ui-monospace,Menlo,monospace;
}
@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) {
  --paper:#11191C; --paper-2:#172226; --surface:#172226; --ink:#E4ECEA; --muted:#98A8AD; --soft:#7C8F94;
  --rule:rgba(228,236,234,.12); --rule-solid:#2A383D; --accent:#7FC0D6; --accent-tint:rgba(127,192,214,.10);
  --link:#A4A9E3; --warn:#E3A56E; --warn-soft:#3A2819; --bad:#E89A9A; --bad-soft:#3B1F1F; color-scheme:dark; } }
:root[data-theme="dark"] {
  --paper:#11191C; --paper-2:#172226; --surface:#172226; --ink:#E4ECEA; --muted:#98A8AD; --soft:#7C8F94;
  --rule:rgba(228,236,234,.12); --rule-solid:#2A383D; --accent:#7FC0D6; --accent-tint:rgba(127,192,214,.10);
  --link:#A4A9E3; --warn:#E3A56E; --warn-soft:#3A2819; --bad:#E89A9A; --bad-soft:#3B1F1F; color-scheme:dark; }
"""

FONTS = '<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Bricolage+Grotesque:opsz,wght@12..96,500;12..96,700&family=IBM+Plex+Sans:ital,wght@0,400;0,500;0,600;1,400&family=IBM+Plex+Mono:wght@400;500&display=swap">'

"""Export each diagram as a self-contained SVG for the docs.

The SVG carries its own styles, light and dark colors (prefers-color-scheme)
and IBM Plex subsets as data URIs, so it renders the same as an <img> on
GitHub, where external fonts and page CSS can't reach it.

Usage: python3 export_svg.py OUT_DIR FONT_DIR
FONT_DIR holds the faces fetch_fonts.py downloads.
"""
import base64, html, io, os, re, sys

from fontTools import subset
from fontTools.ttLib import TTFont

import ddlib
import diagrams
from fetch_fonts import face_file

FACES = [  # family, style, weight
    ("IBM Plex Sans", "normal", 400), ("IBM Plex Sans", "normal", 500),
    ("IBM Plex Sans", "normal", 600), ("IBM Plex Sans", "italic", 400),
    ("IBM Plex Mono", "normal", 400), ("IBM Plex Mono", "normal", 500),
]


def light_dark_tokens():
    """The page tokens, minus the [data-theme] block a standalone SVG never sees."""
    t = ddlib.TOKENS
    light = re.search(r":root \{(.*?)\n\}", t, re.S).group(1)
    dark = re.search(r':root:not\(\[data-theme="light"\]\) \{(.*?)\} \}', t, re.S).group(1)
    return f"svg {{{light}}}\n@media (prefers-color-scheme: dark) {{ svg {{{dark}}} }}\n"


def font_faces(font_dir, text):
    chars = "".join(sorted(set(text))) + " "
    out = []
    for family, style, weight in FACES:
        font = TTFont(os.path.join(font_dir, face_file(family, style, weight)))
        opts = subset.Options()
        opts.flavor = "woff2"
        opts.layout_features = ["kern", "liga"]
        opts.name_IDs = []
        opts.notdef_outline = True
        opts.hinting = False
        sub = subset.Subsetter(opts)
        sub.populate(text=chars)
        sub.subset(font)
        buf = io.BytesIO()
        font.flavor = "woff2"
        font.save(buf)
        b64 = base64.b64encode(buf.getvalue()).decode()
        out.append(f"@font-face{{font-family:'{family}';font-style:{style};font-weight:{weight};"
                   f"src:url(data:font/woff2;base64,{b64}) format('woff2')}}")
    return "\n".join(out)


def export(d, font_dir):
    body = d.svg()
    visible = html.unescape(re.sub(r"<[^>]+>", " ", body))
    css = font_faces(font_dir, visible) + "\n" + light_dark_tokens() + ddlib.STYLE
    css = css.replace("svg.dd {", "svg.dd { font-family: var(--font-sans);")
    body = body.replace(f' style="min-width:{d.w}px"', f' width="{d.w}" height="{d.h}"')
    # <style> goes after <title>/<desc> so they stay the first children.
    i = body.index("<defs>")
    return '<?xml version="1.0" encoding="UTF-8"?>\n' + body[:i] + f"<style>{css}</style>" + body[i:] + "\n"


if __name__ == "__main__":
    out, font_dir = sys.argv[1], sys.argv[2]
    os.makedirs(out, exist_ok=True)
    for f in diagrams.ALL:
        d = f()
        path = os.path.join(out, d.slug + ".svg")
        with open(path, "w") as fh:
            fh.write(export(d, font_dir))
        print(f"{path}  {os.path.getsize(path)//1024} KB")

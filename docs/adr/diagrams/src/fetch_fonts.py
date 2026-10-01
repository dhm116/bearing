"""Download the IBM Plex faces the diagrams use from Google Fonts.

Usage: python3 fetch_fonts.py FONT_DIR

Saves one TTF per face (SIL Open Font License) as <family>-<style>-<weight>.ttf,
for example ibm-plex-sans-normal-600.ttf. export_svg.py subsets them.
"""
import os, re, sys, urllib.request

CSS = ("https://fonts.googleapis.com/css2?family=IBM+Plex+Sans:ital,wght@0,400;0,500;0,600;1,400"
       "&family=IBM+Plex+Mono:wght@400;500&display=swap")
FACE = re.compile(r"font-family: '([^']+)';\s*font-style: (\w+);\s*font-weight: (\d+);.*?url\(([^)]+)\)", re.S)


def face_file(family, style, weight):
    return f"{family.lower().replace(' ', '-')}-{style}-{weight}.ttf"


def main(out):
    os.makedirs(out, exist_ok=True)
    css = urllib.request.urlopen(CSS).read().decode()
    faces = FACE.findall(css)
    if not faces:
        sys.exit("fetch_fonts: no faces in the Google Fonts response")
    for family, style, weight, url in faces:
        path = os.path.join(out, face_file(family, style, weight))
        with open(path, "wb") as fh:
            fh.write(urllib.request.urlopen(url).read())
        print(path)


if __name__ == "__main__":
    main(sys.argv[1])

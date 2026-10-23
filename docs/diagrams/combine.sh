#!/bin/sh
# Makes NAME.svg follow the viewer's colour scheme by adding NAME-dark.svg's
# drawing to it, shown only under prefers-color-scheme: dark. Run it after
# exporting both SVGs: sh docs/diagrams/combine.sh NAME...
set -eu
cd "$(dirname "$0")"
for name in "$@"; do
	light=$name.svg dark=$name-dark.svg
	if grep -q 'class="theme-dark"' "$light"; then
		echo "$light: already combined" >&2
		continue
	fi
	tmp=$(mktemp)
	awk -v dark="$dark" '
		/<\/desc>/ && !head {
			print
			print "<style>.theme-dark{display:none}@media (prefers-color-scheme: dark){.theme-light{display:none}.theme-dark{display:inline}}</style>"
			print "<g class=\"theme-light\">"
			head = 1
			next
		}
		/^<\/svg>/ {
			print "</g>"
			print "<g class=\"theme-dark\">"
			body = 0
			while ((getline line < dark) > 0) {
				if (line ~ /^<\/svg>/) break
				if (body) print line
				if (line ~ /<\/desc>/) body = 1
			}
			print "</g>"
		}
		{ print }
	' "$light" >"$tmp"
	mv "$tmp" "$light"
done

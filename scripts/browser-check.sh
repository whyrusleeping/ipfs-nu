#!/bin/sh
# Prepares the browser origin-isolation checks of docs/validation.md (0035): adds two
# variants of scripts/browser-check/ (two root CIDs = two site origins) and, for B8, a
# tree with names differing only by case, then prints what to open. Needs `nu daemon`
# running on the default ports and `nu` on PATH (or NU=path/to/nu).
#
# Public seed (0043): with PUBLIC=<public.domain> set, the daemon must also run the
# public listener for that domain; both check trees are marked public (`nu public add`)
# and the public URLs are printed so the same rows (plus B9) run against
# https://<cid>.<domain>/ from a machine without Nu.
set -eu
cd "$(dirname "$0")/.."
NU=${NU:-nu}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cp -R scripts/browser-check "$tmp/a"
cp -R scripts/browser-check "$tmp/b"
echo "variant b" > "$tmp/b/variant.txt"
mkdir "$tmp/case"
echo lower > "$tmp/case/a.txt"
echo UPPER > "$tmp/case/A.txt"
a=$($NU --json add -r "$tmp/a" | sed -n 's/.*"cid": *"\([^"]*\)".*/\1/p' | head -1)
b=$($NU --json add -r "$tmp/b" | sed -n 's/.*"cid": *"\([^"]*\)".*/\1/p' | head -1)
if [ "$(ls "$tmp/case" | wc -l)" -eq 2 ]; then
	c=$($NU --json add -r "$tmp/case" | sed -n 's/.*"cid": *"\([^"]*\)".*/\1/p' | head -1)
	b8="$NU get $c -o /tmp/nu-case && ls /tmp/nu-case   (fetch this CID on a case-insensitive macOS volume: expect one file and a 'skipped' line)"
else
	b8="this filesystem folds case; add the case tree on Linux (or a case-sensitive volume) and fetch it here by share link"
fi
port=$(sed -n 's/^listen *= *"[^:]*:\([0-9]*\)"/\1/p' "${NU_PATH:-$HOME/.nu}/config.toml" 2>/dev/null | sed -n 2p)
port=${port:-8171}
cat <<EOF
site A:  http://$a.localhost:$port/        (paste B into the page: $b)
site B:  http://$b.localhost:$port/
B7:      http://127.0.0.1:$port/tree/$a/index.html  (must download)
         http://127.0.0.1:$port/tree/$a/            (listing renders)
B8:      $b8
EOF
if [ -n "${PUBLIC:-}" ]; then
	$NU public add "$a" >/dev/null
	$NU public add "$b" >/dev/null
	cat <<EOF
public seed (0043), from a machine without Nu:
site A:  https://$a.$PUBLIC/        (paste B into the page: $b)
site B:  https://$b.$PUBLIC/
B7:      https://$PUBLIC/tree/$a/index.html  (must download)
         https://$PUBLIC/tree/$a/            (listing renders; "open as site" links to site A)
B9:      runs on site A (RASL read cross-origin + SHA-256 check)
unmark afterwards: $NU public rm $a; $NU public rm $b
EOF
fi
echo 'Record results in docs/validation.md "Browser origin-isolation checks".'

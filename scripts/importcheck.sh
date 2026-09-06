#!/bin/sh
# Test-import isolation: *_test.go may import only stdlib packages,
# "testing", and the root package github.com/gbon-format/gbon-go. Denies
# internal/ subpackages and all third-party imports.
set -e

MODULE="github.com/gbon-format/gbon-go"
ALLOW=""

status=0
for f in $(find . \( -path './showcase' -o -path './.git' \) -prune -o -name '*_test.go' -type f -print); do
  awk -v F="$f" -v M="$MODULE" -v ALLOW="$ALLOW" '
    function allowed(p) {
      if (index("\n" ALLOW "\n", "\n" F ":" p "\n") > 0) return 1
      return 0
    }
    function check(p,   seg, first) {
      if (p == "") return
      split(p, seg, "/")
      first = seg[1]
      if (first ~ /\./) {
        # External module path: only the root module package is allowed,
        # plus the pinned competitor codecs in the comparative benches.
        if (p != M && !allowed(p)) { printf "%s: forbidden import %s\n", F, p; bad = 1 }
      }
      # Stdlib paths (no dot in first segment) are allowed.
    }
    /^import[ \t]*\(/ { inp = 1; next }
    inp && /^\)/ { inp = 0; next }
    inp || /^import[ \t]+"/ {
      line = $0
      gsub(/\/\/.*/, "", line)
      if (match(line, /"[^"]+"/)) {
        check(substr(line, RSTART + 1, RLENGTH - 2))
      }
    }
    END { exit bad ? 1 : 0 }
  ' "$f" || status=1
done

if [ $status -ne 0 ]; then
  echo "importcheck: violations found (tests may import only stdlib and $MODULE)"
  exit 1
fi
echo "importcheck: ok"

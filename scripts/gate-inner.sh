#!/bin/sh
# Inner gate script: executes inside the container at the repo root.
# Invoked by gate.sh with THRESHOLD in the environment.
set -e
: "${THRESHOLD:=80}"

# Modernization gate: the module holds no modernization
# opportunities — the fix-diff of the module is empty. The
# reflect-iterator rewrites are excluded from this module's
# modernization canon: the runtime iterator mechanics add per-iteration
# allocations on hot reflect paths. The phase self-tests under
# --modern-check: an injected modernization file is caught (red), a
# clean tree stays empty (green), twice (idempotence).
modern_check() {
  fix_out=$(go fix -diff -stditerators=false ./... || true)
  if [ -n "$fix_out" ]; then
    echo "$fix_out"
    echo "modernization gate failed: go fix reports modernization opportunities"
    exit 1
  fi
}

modern_self_test() {
  local S pass failn cc label expect out
  S="$(mktemp -d)"
  pass=0; failn=0
  probe() { # label expected-verdict [inject-command...]
    label="$1"; expect="$2"; shift 2
    cc="$S/$label"; rm -rf "$cc"; cp -r . "$cc"
    if [ $# -gt 0 ]; then ( cd "$cc" && sh -c "$*" ) >/dev/null 2>&1 || true; fi
    out=$( ( cd "$cc" && go fix -diff -stditerators=false ./... ) 2>/dev/null || true )
    if { [ "$expect" = red ] && [ -n "$out" ]; } \
       || { [ "$expect" = green ] && [ -z "$out" ]; }; then
      pass=$((pass+1)); echo "modern-check $label: $expect as expected"
    else
      failn=$((failn+1)); echo "modern-check $label: failed, expected $expect"
      [ -n "$out" ] && printf '%s\n' "$out" | sed -n '1,12p'
    fi
  }
  probe clean green
  probe clean-again green
  probe any red 'mkdir probe && printf "package probe\n\nfunc P() interface{} { return nil }\n" > probe/probe_modern.go'
  probe splitseq red 'mkdir probe && printf "package probe\n\nimport \"strings\"\n\nfunc P(s string) { for _, p := range strings.Split(s, \",\") { _ = p } }\n" > probe/probe_modern.go'
  echo "modern-check summary: $pass passed, $failn failed"
  [ $failn -eq 0 ]
}

case "${1:-}" in
  --modern-check) modern_self_test; exit $? ;;
esac

# Marker gate: internal process markers must not leak into the
# product tree.
marker_re='INV[- ][A-Z]?-?[0-9]|TK-[0-9]|CYC-[0-9]|RL-[0-9]|verify cyc|spec row|R-[0-9] of|topology-c[0-9]|arch-c[0-9]|product-c[0-9]|docs-c[0-9]|cyc[0-9]|контрпри[м]ер|находк[аи]|спек[аеу]'
if find . \( -name '*.go' -o -name '*.md' -o -name '*.sh' -o -name 'Makefile' -o -path './.github/*' \) -type f -print0 \
  | xargs -0 -r grep -En "$marker_re" | grep -v 'marker_re='; then
  echo "marker gate failed: internal markers present"
  exit 1
fi

go build ./...
go vet ./...
fmt_out=$(gofmt -l .)
if [ -n "$fmt_out" ]; then
  echo "$fmt_out"
  echo "fmt gate failed: unformatted files present"
  exit 1
fi
modern_check
./scripts/importcheck.sh
go test -race -coverprofile=cover.out -covermode=atomic ./...
# Heap property leg: the streaming payload bound (≤2× wire) executes
# without -race and without coverage instrumentation — under both it
# self-skips (heap accounting distortion), so the gate runs a
# dedicated leg where the property actually executes.
go test -run '^TestStreamingLargePayload$' -count=1 .
# Scaling leg: the encoder work-growth band t(2N)/t(N) ∈ [1.5, 3.0]
# executes without -race (wall-clock distortion); the probe takes the
# median of three passes per size and retries once on a band violation.
go test -run '^TestEncodeScalingProbe$' -count=1 .
total=$(go tool cover -func=cover.out | awk '/^total:/ {sub(/%/,"",$3); print $3}')
echo "coverage total: ${total}%"
if awk -v t="$total" -v th="$THRESHOLD" 'BEGIN { exit (t + 0 < th + 0) ? 1 : 0 }'; then :; else
  echo "coverage gate failed: ${total}% < ${THRESHOLD}%"
  exit 1
fi
command -v golangci-lint >/dev/null 2>&1 || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
golangci-lint run
# Deadcode gate: RTA reachability over the module's test binaries; the
# report must be empty (no unreachable functions). buildvcs=false: the
# container mounts the sources without the VCS metadata.
dead_out=$(GOFLAGS=-buildvcs=false go run golang.org/x/tools/cmd/deadcode@v0.49.0 -test ./... 2>&1 | grep -v '^$' | grep -v '^go: downloading' || true)
if [ -n "$dead_out" ]; then
  echo "deadcode gate failed: unreachable functions:"
  echo "$dead_out"
  exit 1
fi
# Bench smoke: benchmarks must run (one iteration each); performance is not
# gated — this only guards against bitrot of the benchmark corpus.
go test -run '^$' -bench . -benchtime=2x . > /dev/null
echo "gate: GREEN"

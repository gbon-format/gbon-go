#!/bin/sh
# Measurement campaign orchestrator. Runs the campaign phases inside the
# pinned Docker image with the same memory cap and cache volumes as
# scripts/gate.sh; the host only orchestrates. Phases: metrics, strings,
# pprof, bench, benchstat, all (default). Artifacts land in the results
# directory (outside the repository); the baseline side of every bench
# comparison is a clean extraction of the v0.0.4 tag plus the campaign
# test files, so the same script measures any later revision against the
# same baseline without harness edits.
#
# Environment:
#   CAMPAIGN_RESULTS  absolute path of the results directory (required)
#   CAMPAIGN_TAG      baseline tag (default v0.0.4)
set -e

IMAGE=golang:1.27-bookworm
MEMCAP=4g
BENCHSTAT_PIN=v0.0.0-20260908200009-22c9c6c9d4da
RESULTS="${CAMPAIGN_RESULTS:?CAMPAIGN_RESULTS must be set}"
TAG="${CAMPAIGN_TAG:-v0.0.4}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$RESULTS/work"
PHASE="${1:-all}"

mkdir -p "$RESULTS/metrics" "$RESULTS/strings" "$RESULTS/pprof" "$RESULTS/bench" "$WORK"

docker volume create gbon-gomod >/dev/null 2>&1 || true
docker volume create gbon-gobin >/dev/null 2>&1 || true
docker volume create gbon-gocache >/dev/null 2>&1 || true
docker volume create gbon-gosumdb >/dev/null 2>&1 || true

# drun <workdir-mount> <command...>: one fixed-environment container run.
# Worktree co-mount: resolve the main repo's .git by the pointer file so
# in-container git works from a worktree checkout (gate-in-worktree recipe).
SRC="$(cd "$(dirname "$0")/.." && pwd)"
EXTRA_MOUNT=""
if [ -f "$SRC/.git" ]; then
  MAIN_GIT="$(dirname "$(dirname "$(sed 's/^gitdir: //' "$SRC/.git")")")"
  EXTRA_MOUNT="-v $MAIN_GIT:$MAIN_GIT"
fi
drun() {
  wd="$1"; shift
  docker run --rm -m "$MEMCAP" \
    --user "$(id -u):$(id -g)" \
    -v "$wd":/w \
    $EXTRA_MOUNT \
    -v "$RESULTS":/results \
    -v gbon-gomod:/go/pkg/mod \
    -v gbon-gobin:/go/bin \
    -v gbon-gosumdb:/go/pkg/sumdb \
    -v gbon-gocache:/gocache \
    -e GOCACHE=/gocache \
    -e GOBIN=/go/bin \
    -w /w \
    "$IMAGE" \
    sh -c "$*"
}

# baseline_tree materializes the baseline side: tag extraction plus the
# full campaign test surface (all three campaign files, so the baseline
# binary layout matches the measured tree — an identical-copy A/A per the
# discipline; for later code cycles the same file set rides on the tag).
baseline_tree() {
  rm -rf "$WORK/A"
  mkdir -p "$WORK/A"
  git -C "$REPO" archive "$TAG" | tar -x -C "$WORK/A"
  cp "$REPO/codec_campaign_bench_test.go" "$WORK/A/codec_campaign_bench_test.go"
  cp "$REPO/codec_campaign_internal_test.go" "$WORK/A/codec_campaign_internal_test.go"
  cp "$REPO/codec_campaign_strings_test.go" "$WORK/A/codec_campaign_strings_test.go"
}

phase_metrics() {
  echo "== campaign phase: metrics (grid counters + repro + streams dump)"
  drun "$REPO" 'GBON_CAMPAIGN_OUT=/results/metrics go test -run TestCampaignGridMetrics -count=1 .'
}

phase_strings() {
  echo "== campaign phase: strings (census original/decoded)"
  drun "$REPO" 'GBON_CAMPAIGN_STRINGS_OUT=/results/strings go test -run TestCampaignStringsCensus -count=1 .'
}

phase_pprof() {
  echo "== campaign phase: pprof (phase profiles per scenario)"
  for sc in sharing-heavy map-heavy mono-vector; do
    for leg in encode decode; do
      echo "-- profile $sc/$leg"
      drun "$REPO" "go test -run '^\$' -bench '^BenchmarkCampaignPhase/$sc/$leg\$' -benchtime=300000x -count=1 -cpuprofile /results/pprof/$sc-$leg.pb . > /dev/null"
      drun "$REPO" "go tool pprof -top -nodecount=80 /results/pprof/$sc-$leg.pb > /results/pprof/$sc-$leg.top.txt 2>/dev/null"
    done
  done
}

# Fixed-iteration bench command, identical on both sides. -buildvcs=false
# removes the VCS stamp difference between the archived baseline side (no
# .git) and the working tree (with .git): with stamping on, the two sides
# build different test binaries and layout effects masquerade as deltas.
BENCH_CMD='GOFLAGS=-buildvcs=false go test -run "^\$" -bench "^BenchmarkCampaign" -benchtime=100x -count=10 .'

phase_bench() {
  echo "== campaign phase: bench (interleaved full campaigns, A=tag B=tree)"
  baseline_tree
  rm -f "$RESULTS"/bench/A-*.txt "$RESULTS"/bench/B-*.txt
  for i in 1 2 3; do
    echo "-- interleave round $i: A then B"
    drun "$WORK/A" "$BENCH_CMD > /results/bench/A-$i.txt"
    drun "$REPO" "$BENCH_CMD > /results/bench/B-$i.txt"
  done
  cat "$RESULTS"/bench/A-1.txt "$RESULTS"/bench/A-2.txt "$RESULTS"/bench/A-3.txt > "$RESULTS/bench/A-all.txt"
  cat "$RESULTS"/bench/B-1.txt "$RESULTS"/bench/B-2.txt "$RESULTS"/bench/B-3.txt > "$RESULTS/bench/B-all.txt"
}

# grid_mirror_check guards the mirrored grid tables: the metrics grid
# (internal test package) and the bench grid (external test package) are
# two copies of one table with no cross-package test channel; their
# artifact outputs must name the same scenario cells, or the campaign
# legs silently measure different grids.
grid_mirror_check() {
  mtmp=$(mktemp) ; btmp=$(mktemp)
  awk -F'\t' '{print $1"/"$2}' "$RESULTS/metrics/metrics.tsv" | sort > "$mtmp"
  grep '^BenchmarkCampaignGrid/' "$RESULTS/bench/A-all.txt" \
    | awk '{print $1}' | sed 's/-8$//; s|^BenchmarkCampaignGrid/||' | sort -u > "$btmp"
  if ! diff -u "$mtmp" "$btmp" > "$RESULTS/bench/grid-mirror.diff"; then
    echo "grid mirror check FAILED: metrics and bench grids diverge (results/bench/grid-mirror.diff)"
    rm -f "$mtmp" "$btmp"
    exit 1
  fi
  rm -f "$mtmp" "$btmp" ; rm -f "$RESULTS/bench/grid-mirror.diff"
}

# benchstat_gate applies the geomean gate to one benchstat output:
# one-sided per the campaign's regression contract — a regression beyond
# +2% in either the sec/op or the allocs/op geomean fails, an improvement
# of any magnitude passes. Sections are identified by their header lines
# ("<metric> ... <metric> vs base"), never by ordinal position, and must
# appear in the canonical order (sec/op before allocs/op). Per-line
# deltas at 5% or more form the advisory layer: reported to the advisory
# file, never gating.
benchstat_gate() {
  awk -v advfile="$2" '
    function absf(x) { return x < 0 ? -x : x }
    /sec\/op.*vs base/ { cur = "time"; seen_time = 1; if (seen_allocs) order_bad = 1; next }
    /B\/s.*vs base/ { cur = "Bs"; next }
    /B\/op.*vs base/ { cur = "Bop"; next }
    /allocs\/op.*vs base/ { cur = "allocs"; seen_allocs = 1; next }
    /^geomean/ {
      d = $NF; sub(/%/, "", d)
      if (cur == "time") { t = d + 0 }
      if (cur == "allocs") { a = d + 0 }
    }
    !/^geomean/ && match($0, /[-+][0-9]+\.[0-9]+%/) {
      v = substr($0, RSTART, RLENGTH); sub(/%/, "", v)
      if (absf(v + 0) >= 5.0) {
        adv++
        print "advisory |delta|>=5%: " $1 " " v "%" > advfile
      }
    }
    END {
      have = (seen_time && seen_allocs && !order_bad && t != "" && a != "")
      ok = (have && t <= 2.0 && a <= 2.0)
      why = (!seen_time) ? "no sec/op section" : (!seen_allocs) ? "no allocs/op section" : \
        (order_bad) ? "section order not canonical" : (!have) ? "no geomean row" : ""
      printf "geomean gate: time=%+.2f%% allocs=%+.2f%% (regression limit +2%% each): %s%s; advisory rows |delta|>=5%%: %d\n",
        t, a, (ok ? "PASS" : "FAIL"), (why == "" ? "" : " (" why ")"), adv
      exit ok ? 0 : 1
    }' "$1"
}

# phase_gate_selftest exercises the geomean gate on fixed benchstat
# fixtures: a regression beyond +2% must fail, a large improvement must
# pass — a green run on live artifacts alone proves nothing about the
# checker itself.
phase_gate_selftest() {
  neg=$(mktemp); pos=$(mktemp); advneg=$(mktemp); advpos=$(mktemp)
  cat > "$neg" <<'FIXTURE'
benchmark                                          │       sec/op        │   sec/op     vs base                │
BenchmarkCampaignGrid/control-noshare-8              10.0n ± 0%   12.1n ± 0%  +21.00% (p=0.000 n=30)
geomean                                              10.0n        12.1n       +21.00%

benchmark                                          │      allocs/op       │  allocs/op    vs base                │
BenchmarkCampaignGrid/control-noshare-8              1.000Ki ± 0%  1.210Ki ± 0%  +21.00% (p=0.000 n=30)
geomean                                              1.000Ki       1.210Ki      +21.00%
FIXTURE
  cat > "$pos" <<'FIXTURE'
benchmark                                          │       sec/op        │   sec/op     vs base                │
BenchmarkCampaignGrid/control-noshare-8              20.2µ ± 0%   17.2µ ± 0%  -15.17% (p=0.000 n=30)
geomean                                              20.2µ        17.2µ       -15.17%

benchmark                                          │      allocs/op       │  allocs/op    vs base                │
BenchmarkCampaignGrid/control-noshare-8              104.0 ± 0%   86.7 ± 0%   -16.62% (p=0.000 n=30)
geomean                                              104.0        86.7        -16.62%
FIXTURE
  if benchstat_gate "$neg" "$advneg" >/dev/null 2>&1; then
    echo "gate selftest FAILED: +21% regression accepted"
    rm -f "$neg" "$pos" "$advneg" "$advpos"
    exit 1
  fi
  if ! benchstat_gate "$pos" "$advpos" >/dev/null 2>&1; then
    echo "gate selftest FAILED: -15% improvement rejected"
    rm -f "$neg" "$pos" "$advneg" "$advpos"
    exit 1
  fi
  rm -f "$neg" "$pos" "$advneg" "$advpos"
  echo "gate selftest: regression rejected, improvement accepted"
}

phase_benchstat() {
  echo "== campaign phase: benchstat (geomean-gate + advisory layer)"
  phase_gate_selftest
  drun "$REPO" "command -v benchstat >/dev/null 2>&1 || go install golang.org/x/perf/cmd/benchstat@$BENCHSTAT_PIN"
  drun "$REPO" "benchstat /results/bench/A-all.txt /results/bench/B-all.txt | tee /results/bench/benchstat-aa.txt"
  grid_mirror_check
  rm -f "$RESULTS/bench/benchstat-advisory.txt"
  if benchstat_gate "$RESULTS/bench/benchstat-aa.txt" "$RESULTS/bench/benchstat-advisory.txt"; then
    :
  else
    echo "benchstat geomean-gate FAILED: time or allocs geomean regressed beyond +2%, or section format unexpected"
    exit 1
  fi
}

case "$PHASE" in
  metrics) phase_metrics ;;
  strings) phase_strings ;;
  pprof) phase_pprof ;;
  bench) phase_bench ;;
  benchstat) phase_benchstat ;;
  gate-selftest) phase_gate_selftest ;;
  all)
    phase_metrics
    phase_strings
    phase_pprof
    phase_bench
    phase_benchstat
    echo "campaign: ALL PHASES DONE, artifacts in $RESULTS"
    ;;
  *) echo "unknown phase: $PHASE (use metrics|strings|pprof|bench|benchstat|all)"; exit 2 ;;
esac

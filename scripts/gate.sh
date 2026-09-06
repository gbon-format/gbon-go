#!/bin/sh
# Local gate: runs the full toolchain in Docker. Docker is required.
# Cached volumes keep toolchain downloads between runs. The container
# runs under an explicit memory cap so runaway allocations surface as
# OOM kills instead of silent host churn.
set -e

IMAGE=golang:1.27-bookworm
THRESHOLD=80
MEMCAP=4g

# Conformance corpus mount (spec repository checkout beside this repo);
# absent directory keeps the gate runnable standalone.
SPEC_SRC="$(cd "$(dirname "$0")/../../spec" 2>/dev/null && pwd)"
MOUNT=""
[ -n "$SPEC_SRC" ] && MOUNT="-v $SPEC_SRC:/spec:ro"

docker volume create gbon-gomod >/dev/null 2>&1 || true
docker volume create gbon-gobin >/dev/null 2>&1 || true
docker volume create gbon-gocache >/dev/null 2>&1 || true
docker volume create gbon-gosumdb >/dev/null 2>&1 || true

# Unprivileged run: the container user matches the host user, so gate
# artifacts (cover.out, module/build caches) never appear root-owned.
docker run --rm \
  -m "$MEMCAP" \
  --user "$(id -u):$(id -g)" \
  -v "$(cd "$(dirname "$0")/.." && pwd)":/w \
  $MOUNT \
  -v gbon-gomod:/go/pkg/mod \
  -v gbon-gobin:/go/bin \
  -v gbon-gosumdb:/go/pkg/sumdb \
  -v gbon-gocache:/gocache \
  -e THRESHOLD="$THRESHOLD" \
  -e GOCACHE=/gocache \
  -e GOLANGCI_LINT_CACHE=/gocache/golangci-lint \
  -w /w \
  "$IMAGE" \
  sh scripts/gate-inner.sh

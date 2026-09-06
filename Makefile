# Development commands. Everything runs the module's own toolchain; the
# only dependency beyond the Go toolchain is golangci-lint (pinned version
# in scripts/gate-inner.sh) for `make lint`.

.PHONY: build test test-race vet lint cover bench fmt fmt-fix gate

build:
	go build ./...

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run

cover:
	go test -coverprofile=cover.out -covermode=atomic ./...
	go tool cover -func=cover.out | tail -1

bench:
	go test -run '^$$' -bench . -benchtime=100x .

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; echo "fmt gate failed: run make fmt-fix"; exit 1; fi

fmt-fix:
	gofmt -l -w .

# Maintainer-only: the full gate (build, vet, import isolation, race tests
# with coverage threshold, marker gate, lint, bench smoke) inside the
# pinned Docker image. Requires Docker.
gate:
	./scripts/gate.sh

# yore — build/test entry points.
# NOTE (this machine): Go is installed via gobrew; run `source ~/.gobrew` first
# if `go` is not on your PATH.

GO         ?= go
BIN        := bin/yore
COVERFILE  := coverage.out
TESTREPORT := unit-test-report.json
PKGS       := ./...
LDFLAGS    := -s -w -X yore/internal/cli.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)

# Run recipes under bash with pipefail so a failure inside the `go test | gotestfmt`
# pipe fails the recipe instead of being masked by gotestfmt's exit status.
SHELL       := /bin/bash
.SHELLFLAGS := -o pipefail -c

.PHONY: build test vet fmt lint coverage coverage-check bench clean docker release drone

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/yore

# Unit tests, formatted for humans by gotestfmt (github.com/GoTestTools/gotestfmt)
# from `go test -json`. Two passes on purpose: a coverage pass (also writes the
# profile `coverage-check` enforces, teeing the raw JSON to $(TESTREPORT) for
# inspection) then a -race pass. pipefail (set above) makes a `go test` failure
# fail the recipe even though gotestfmt exits 0. Needs gotestfmt on PATH (install:
# go install github.com/gotesttools/gotestfmt/v2/cmd/gotestfmt@latest).
test:
	$(GO) test -shuffle=on -count=1 -coverprofile=$(COVERFILE) -covermode=atomic -json $(PKGS) \
		| tee $(TESTREPORT) | gotestfmt
	$(GO) test -shuffle=on -count=1 -race -json $(PKGS) | gotestfmt

vet:
	$(GO) vet $(PKGS)

fmt:
	gofmt -w .
	goimports -w .

# golangci-lint's gofmt/goimports formatters and govet linter cover the fmt/vet gate.
lint:
	golangci-lint run --timeout 5m $(PKGS)

coverage:
	$(GO) test $(PKGS) -coverprofile=$(COVERFILE) -covermode=atomic
	$(GO) tool cover -html=$(COVERFILE) -o coverage.html
	@echo "Coverage report: coverage.html"

# Enforces the coverage floor against the profile `test` already produced — it does
# NOT re-run the suite. In CI, run after the `test` step (same workspace); locally,
# run `make test` first.
coverage-check:
	go-covercheck $(COVERFILE)

bench:
	$(GO) test -bench=. -benchmem -run=^$$ $(PKGS)

clean:
	rm -rf bin dist $(COVERFILE) coverage.html $(TESTREPORT)

docker:
	docker build -f docker/Dockerfile -t yore:latest .

# Tier-1 release matrix: linux+darwin+freebsd, amd64+arm64 where it matters.
# WSL runs the linux binaries. Windows native is experimental/later.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 freebsd/amd64

release:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=dist/yore-$$os-$$arch; \
		echo "  $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/yore || exit 1; \
	done

# Run the Drone pipeline locally (needs the `drone` CLI + docker). Pass a
# comma-separated steps= to run only those:  make drone steps=lint,test
drone:
	@steps="$(steps)"; include=""; \
	for s in $$(echo "$$steps" | tr ',' ' '); do include="$$include --include=$$s"; done; \
	drone exec $$include .drone.yml

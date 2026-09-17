# yore; build/test entry points. Requires Go 1.26+.

GO         ?= go
BIN        := bin/yore
COVERFILE  := coverage.out
TESTREPORT := unit-test-report.json
PKGS       := ./...
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS    := -s -w -X github.com/mach6/yore/internal/cli.Version=$(VERSION)

# Server image. IMAGE_TAG names the local build; push-docker retags it as
# $(REGISTRY)/$(IMAGE):<tag> for every tag in PUSH_TAGS. REGISTRY includes any
# owner or namespace (for example ghcr.io/<owner>).
IMAGE      ?= yore
IMAGE_TAG  ?= dev
REGISTRY   ?=
PUSH_TAGS  ?= latest-build

# Pinned tool versions, installed by `make tools`. CI installs exactly these.
GOLANGCI_LINT_VERSION ?= v2.12.2
GOTESTFMT_VERSION     ?= v2.5.0
GO_COVERCHECK_VERSION ?= v0.6.1
GOIMPORTS_VERSION     ?= v0.50.0

# Run recipes under bash with pipefail so a failure inside the `go test | gotestfmt`
# pipe fails the recipe instead of being masked by gotestfmt's exit status.
SHELL       := /bin/bash
.SHELLFLAGS := -o pipefail -c

.PHONY: build test vet fmt lint yamllint coverage coverage-check ci tools bench clean \
	build-docker test-docker push-docker release stress fleet

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/yore

# Unit tests, formatted for humans by gotestfmt (github.com/GoTestTools/gotestfmt)
# from `go test -json`. Two passes on purpose: a coverage pass (also writes the
# profile `coverage-check` enforces, teeing the raw JSON to $(TESTREPORT) for
# inspection) then a -race pass, which needs cgo and a C compiler. pipefail (set
# above) makes a `go test` failure fail the recipe even though gotestfmt exits 0.
# Needs gotestfmt on PATH (`make tools`).
test:
	$(GO) test -shuffle=on -count=1 -coverprofile=$(COVERFILE) -covermode=atomic -json $(PKGS) \
		| tee $(TESTREPORT) | gotestfmt
	$(GO) test -shuffle=on -count=1 -race -json $(PKGS) | gotestfmt

vet:
	$(GO) vet $(PKGS)

fmt:
	gofmt -w .
	goimports -w .

# Every gate CI runs before it builds the image, in CI's order.
ci: yamllint lint test coverage-check

# Installs the Go tools the gates need into $(go env GOPATH)/bin. yamllint is a
# Python tool and is not installed here (pip install yamllint).
tools:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GO) install github.com/gotesttools/gotestfmt/v2/cmd/gotestfmt@$(GOTESTFMT_VERSION)
	$(GO) install github.com/mach6/go-covercheck/cmd/go-covercheck@$(GO_COVERCHECK_VERSION)
	$(GO) install golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION)

yamllint:
	yamllint -c .yamllint.yml .

# golangci-lint's gofmt/goimports formatters and govet linter cover the fmt/vet gate.
lint:
	golangci-lint run --timeout 5m $(PKGS)

coverage:
	$(GO) test $(PKGS) -coverprofile=$(COVERFILE) -covermode=atomic
	$(GO) tool cover -html=$(COVERFILE) -o coverage.html
	@echo "Coverage report: coverage.html"

# Enforces the coverage floor against the profile `test` already produced: it does
# NOT re-run the suite. In CI, run after the `test` step (same workspace); locally,
# run `make test` first.
coverage-check:
	go-covercheck $(COVERFILE)

bench:
	$(GO) test -bench=. -benchmem -run=^$$ $(PKGS)

clean:
	rm -rf bin dist $(COVERFILE) coverage.html $(TESTREPORT)

build-docker:
	docker build -f docker/Dockerfile --build-arg VERSION=$(VERSION) -t $(IMAGE):$(IMAGE_TAG) .

# Smoke the image: the entrypoint is `yore server`, so override it to run a
# trivial subcommand that must exit 0.
test-docker:
	docker run --rm --entrypoint /yore $(IMAGE):$(IMAGE_TAG) version

# Needs a prior `docker login` to the registry.
push-docker:
	@test -n "$(REGISTRY)" || { echo "push-docker: set REGISTRY" >&2; exit 1; }
	@for t in $(PUSH_TAGS); do \
		echo "  $(REGISTRY)/$(IMAGE):$$t"; \
		docker tag $(IMAGE):$(IMAGE_TAG) $(REGISTRY)/$(IMAGE):$$t || exit 1; \
		docker push $(REGISTRY)/$(IMAGE):$$t || exit 1; \
	done

# MANUAL stress/soak harness: NOT part of CI. Rebuilds the
# current binary into a fresh 3-container sandbox, hammers it with N records per
# host (normal + secrets + tags + drop-cases), syncs, then verifies correctness
# and prints timings. Needs docker + docker compose. Tears the sandbox down at
# the end (KEEP=1 leaves it up for a post-mortem).
#   make stress            # N=5000 per host (a real run)
#   make stress N=10000    # heavier
#   make stress N=150      # quick smoke
#   make stress KEEP=1     # leave the sandbox running afterwards
stress:
	N=$(N) KEEP=$(KEEP) docker/sandbox/stress.sh

# MANUAL fleet harness: the same idea at scale: TWENTY machines over eight Linux
# distributions, half zsh and half bash, split between TWO users (server tenants
# with their own isolated groups). Enrolls all twenty, drives half a million
# randomized commands through them, syncs, then measures throughput/latency/size
# and verifies convergence, redaction, and cross-user isolation. Leaves the fleet
# UP by default: the end state is the thing worth looking at.
#   make fleet                 # 500,000 records across 20 nodes
#   make fleet TOTAL=20000     # quick run
#   make fleet DOWN=1          # tear the fleet down when it finishes
fleet:
	TOTAL=$(TOTAL) docker/sandbox/fleet.sh $(if $(DOWN),--down,)

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

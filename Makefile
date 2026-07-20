# yore — build/test entry points.
# NOTE (this machine): Go is installed via gobrew; run `source ~/.gobrew` first
# if `go` is not on your PATH.

GO      ?= go
BIN     := bin/yore
LDFLAGS := -s -w -X yore/internal/cli.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)

.PHONY: build test vet fmt bench clean docker

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/yore

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

bench:
	$(GO) test -bench=. -benchmem -run=^$$ ./...

clean:
	rm -rf bin dist

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

docker:
	docker build -f docker/Dockerfile -t yore:latest .

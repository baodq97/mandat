# Mandat build + gate harness. `make check` is the single aggregate gate;
# CI calls the same targets, so the gate has one definition (ADR-0001).
BINARY  := mandat
BIN_DIR := $(CURDIR)/bin

# golangci-lint must be a pinned binary install — its docs rule out
# go install and the go.mod tool directive. govulncheck, by contrast,
# IS pinned via the tool directive in go.mod. The binary path carries the
# version so bumping the pin forces a fresh install; a stale binary can
# never satisfy the target.
GOLANGCI_LINT_VERSION := v2.12.2
GOLANGCI_LINT := $(BIN_DIR)/golangci-lint-$(GOLANGCI_LINT_VERSION)

# Go 1.24+ auto-stamps the VCS version into the binary; the -X override
# gives release builds a human-friendly tag and covers tarball builds.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/baodq97/mandat/internal/buildinfo.version=$(VERSION)

.PHONY: all build fmt fmt-check lint test tidy-check vuln deps-check check clean

all: build

# CGO_ENABLED=0 is a gate, not an optimization: D3 (single static binary)
# and D4 (pure-Go SQLite) fail the moment a dependency drags in cgo. The
# ./... compile covers every plane package, so a cgo dep breaks the gate even
# before it is wired into cmd/mandat; the second build produces the binary.
build:
	CGO_ENABLED=0 go build ./...
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)

fmt: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt

fmt-check: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt --diff

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

test:
	go test -race -shuffle=on -count=1 ./...

# -diff is read-only and exits non-zero when go.mod/go.sum need tidying
tidy-check:
	go mod tidy -diff

vuln:
	go tool govulncheck ./...

# Advisory only (ADR-0002): reports direct deps with newer versions available.
# Never part of `check` — blocking on upstream release timing is a false-positive
# generator; Dependabot owns the actual bumps.
deps-check:
	@go list -u -m -f '{{if and (not .Indirect) .Update}}{{.Path}}: {{.Version}} -> {{.Update.Version}}{{end}}' all

check: fmt-check lint test tidy-check vuln build

# no curl|sh: a failed download must fail THIS target, not surface as a
# confusing 127 at the first consumer
$(GOLANGCI_LINT):
	mkdir -p $(BIN_DIR)
	curl -sSfL -o $(BIN_DIR)/golangci-install.sh https://golangci-lint.run/install.sh
	sh $(BIN_DIR)/golangci-install.sh -b $(BIN_DIR) $(GOLANGCI_LINT_VERSION)
	mv $(BIN_DIR)/golangci-lint $(GOLANGCI_LINT)
	rm $(BIN_DIR)/golangci-install.sh

clean:
	rm -rf $(BIN_DIR)

# kloo — canonical build/test/lint targets. Thin wrappers over the Go toolchain.
# Release/build paths force CGO_ENABLED=0 (single static cross-compilable binary).

GO        ?= go
CGO_ENABLED ?= 0
BIN       := bin/kloo

# Version stamp for `make binary` so `kloo --version` reports a real build. Local
# builds always carry a "-dev" suffix (e.g. "v0.9.2-dev") so they're never mistaken
# for a release — the exact commit/date sit in the parens. Release binaries are
# stamped by goreleaser with the bare tag (no "-dev").
VERSION   ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)-dev
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VPKG      := github.com/lokalhub/kloo/internal/cli
LDFLAGS   := -X $(VPKG).version=$(VERSION) -X $(VPKG).commit=$(COMMIT) -X $(VPKG).date=$(DATE)

# Where `make install` puts the binary. Override either half:
#   make install PREFIX=/usr/local     -> /usr/local/bin/kloo
#   make install BINDIR=$HOME/bin
PREFIX    ?= $(HOME)/.local
BINDIR    ?= $(PREFIX)/bin

.PHONY: build binary run test vet fmt fmtcheck lint tidy check clean bench-assert install

# Plain `make` builds the runnable binary (./bin/kloo) — the intuitive default.
# `make build` remains the no-artifact compile gate; `make check` the full gate.
.DEFAULT_GOAL := binary

# Compile every package (no artifact). The zero-lag build gate.
build:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build ./...

# Build the runnable binary (version-stamped so `kloo --version` works).
binary:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) .

# Build and put the binary on PATH. Prints the version before and after, because
# the usual way to be confused about kloo is to run a stale install: a copy from a
# previous build, or a release binary this checkout is already ahead of. An already
# running TUI keeps the binary it started with — relaunch it to pick this up.
# Installs a "-dev" stamp by default; pass the tag to install a release stamp:
#   make install VERSION=v1.2.3
install: binary
	@printf 'before: '; "$(BINDIR)/kloo" --version 2>/dev/null || echo "not installed"
	@mkdir -p "$(BINDIR)"
	@install -m 0755 "$(BIN)" "$(BINDIR)/kloo"
	@printf 'after:  '; "$(BINDIR)/kloo" --version
	@p=$$(command -v kloo 2>/dev/null); \
	 if [ -z "$$p" ]; then \
	   printf 'note: %s is not on your PATH\n' "$(BINDIR)"; \
	 elif [ "$$p" != "$(BINDIR)/kloo" ]; then \
	   printf 'note: PATH finds kloo at %s, not the copy just installed\n' "$$p"; \
	 fi

# Run kloo, forwarding args:  make run ARGS='--model snappy "say hi"'
run:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) run . $(ARGS)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# Rewrite files in place.
fmt:
	$(GO) fmt ./...

# CI gate: fail if anything is unformatted (gofmt -l prints offending files).
fmtcheck:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

# golangci-lint if installed, otherwise no-op (go vet is the floor gate).
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else echo "golangci-lint not installed; skipping (go vet is the floor gate)"; fi

tidy:
	$(GO) mod tidy

# The full local gate, mirroring CI.
check: build vet fmtcheck test

clean:
	rm -rf bin

# Run the acceptance benchmark's structural assertion harness (Gate A, part 2) over
# an app src/ dir.  Default targets the reference solution (must PASS).
#   make bench-assert                     # -> benchmark/reference/src  (PASS, exit 0)
#   make bench-assert SRC=benchmark/fixture/src   # untouched skeleton  (FAIL, exit 1)
SRC ?= benchmark/reference/src
bench-assert:
	bash benchmark/assert.sh $(SRC)

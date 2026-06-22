# kloo — canonical build/test/lint targets. Thin wrappers over the Go toolchain.
# Release/build paths force CGO_ENABLED=0 (single static cross-compilable binary).

GO        ?= go
CGO_ENABLED ?= 0
BIN       := bin/kloo

.PHONY: build binary run test vet fmt fmtcheck lint tidy check clean bench-assert

# Compile every package (no artifact). The zero-lag build gate.
build:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build ./...

# Build the runnable binary.
binary:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -o $(BIN) .

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

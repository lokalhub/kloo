package tools

import "testing"

func TestReadOnlyCommandClassification(t *testing.T) {
	readOnly := []string{
		"go test ./...",
		"go test ./... 2>&1 | tail -20",
		"go test ./... 2>&1 | head -50",
		"go vet ./... && go test -run TestMaxWindow -v ./...",
		"gofmt -l .",
		"git status",
		"git diff --name-only",
		"ls -la",
		"cat window.go",
		"sed -n '1,25p' window.go",
		"grep -rn MaxWindow .",
		"GOFLAGS=-mod=mod go test ./...",
		"/usr/local/go/bin/go test ./...",
		"find . -name '*.go' | head -20",
		"npm test",
		"cargo check",
	}
	for _, c := range readOnly {
		if !IsReadOnlyCommand(c) {
			t.Errorf("IsReadOnlyCommand(%q) = false, want true (diagnostic turns must not count as action)", c)
		}
	}

	// Conservative by design: anything that mutates, or that we cannot reason
	// about, must read as ACTING. A false "read-only" would let a mutating loop
	// run unchecked, so these matter more than the cases above.
	acting := []string{
		"rm -rf build",
		"sed -i 's/a/b/' window.go",
		"gofmt -w .",
		"go build -o kloo .",
		"go generate ./...",
		"git checkout -- window.go",
		"git commit -m x",
		"npm install",
		"mkdir -p out",
		"cp a b",
		"mv a b",
		"touch new.go",
		"chmod +x run.sh",
		"go test ./... > results.txt", // redirect writes a file
		"cat window.go > copy.go",     // ditto
		"ls && rm -rf build",          // one mutating stage taints the chain
		"go test ./... | tee out.txt", // tee writes
		"echo $(rm -rf x)",            // command substitution: refuse to reason
		"echo `rm -rf x`",             // backticks: ditto
		"go",                          // bare, no subcommand: ambiguous
		"git",                         // ditto
		"",                            // nothing to classify
		"   ",
	}
	for _, c := range acting {
		if IsReadOnlyCommand(c) {
			t.Errorf("IsReadOnlyCommand(%q) = true, want false (must never misclassify a mutating command)", c)
		}
	}
}

// TestCdPrefixedInspectionIsReadOnly: `cd <dir> && <reader>` is how a model
// inspects a subdirectory, and it must count as reading.
//
// While `cd` was missing from the allowlist every one of these classified as
// ACTING, which reset the exploration counters on every turn — so the explore
// rails, whose whole job is to convert reading into an edit, could not fire at
// all for a model that prefixed its greps with a cd. Observed twice in one
// session: two runs, 6 and 3 minutes, zero edits, zero nudges.
func TestCdPrefixedInspectionIsReadOnly(t *testing.T) {
	readOnly := []string{
		`cd frontend && grep -n "login-container" src/app/login.page.ts | head -60`,
		`cd frontend && sed -n 1,25p src/app/login.page.ts`,
		`cd /tmp/x && cat karma.conf.js`,
		`cd .. && ls`,
	}
	for _, c := range readOnly {
		if !IsReadOnlyCommand(c) {
			t.Errorf("should be read-only: %s", c)
		}
	}
	// cd does not launder a mutation that follows it.
	acting := []string{
		`cd frontend && npm run build`,
		`cd frontend && rm -rf dist`,
		`cd frontend && cat a.txt > b.txt`,
	}
	for _, c := range acting {
		if IsReadOnlyCommand(c) {
			t.Errorf("should count as acting: %s", c)
		}
	}
}

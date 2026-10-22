#!/usr/bin/env bash
# Gate: a run that can write NOTHING must not be nudged to change something.
#
# Measured: a validation run under `--allow 'nothing/**'`, whose prompt says
# "Do not edit any file", degenerated into four repeated `finish` calls each
# carrying the whole answer. Rails: confirm-finish x1, no-edit-finish x1,
# promise-to-act x3. Cause: taskWantsChange matched 26 change verbs in the
# prompt because the prompt QUOTES THE SPEC BEING VALIDATED --
#   [Write wire add Add implement add add change create change correct ...]
# so the no-edit rail told a validator to go make edits it was forbidden to make.
# The task text can never be a reliable signal here. The write scope can.
set -uo pipefail
cd "$(dirname "$0")" || exit 1
fail() { echo "GATE FAIL: $*"; exit 1; }

command -v go >/dev/null || fail "(ENVIRONMENT, not this change) go is not on PATH"
for f in internal/agent/finish_noedit.go internal/agent/loop.go internal/tools/scope.go; do
  [ -f "$f" ] || fail "(ENVIRONMENT, not this change) $f is missing"
done

# --- the scope must answer "can this run write anything at all?" CORRECTLY.
# A pattern-only answer is not good enough: measured, the probe-path version
# returned false for --allow 'internal/tools/registry.go', which would have
# silently disabled the no-edit rails for EVERY scoped run.
[ "$(grep -c 'func (p \*ScopePolicy) AllowsAnyWrite(root string)' internal/tools/scope.go || true)" -gt 0 ] \
  || fail "AllowsAnyWrite must take an explicit root: AllowsAnyWrite(root string) bool"
[ "$(grep -c 'probePaths' internal/tools/scope.go || true)" -eq 0 ] \
  || fail "AllowsAnyWrite still answers from synthetic probe paths; it must consult the real workspace"

# BEHAVIOURAL: the semantics, against this very repo.
cat > internal/tools/kloogate2_scope_test.go <<'T'
package tools
import "testing"
// AllowsAnyWrite(root) means: at least one allow pattern matches an EXISTING
// file under root. The root is an explicit argument -- an implicit/absent root
// has to fail open to true, which made the false cases below unsatisfiable. Measured defect: a probe-path implementation said
// false for a single real file, which would disable the no-edit rails for every
// scoped run.
func TestKloogate2AllowsAnyWriteSemantics(t *testing.T) {
	cases := []struct{ name string; globs []string; want bool }{
		{"a single real file (the common case)", []string{"internal/tools/registry.go"}, true},
		{"two real files", []string{"internal/tools/registry.go", "internal/cli/root.go"}, true},
		{"a real directory glob", []string{"internal/tools/*.go"}, true},
		{"the read-only sentinel matches nothing", []string{"nothing/**"}, false},
		{"a path that does not exist", []string{"no/such/file.go"}, false},
	}
	for _, c := range cases {
		p, err := NewScopePolicy(c.globs, nil, nil)
		if err != nil { t.Fatalf("%s: %v", c.name, err) }
		// go test runs with the PACKAGE directory as cwd, so the repo root --
		// the tree these globs are spelled against -- is two levels up. "." here
		// would be internal/tools, where "internal/tools/registry.go" names
		// nothing: a gate defect, not an implementation one.
		if got := p.AllowsAnyWrite("../.."); got != c.want {
			t.Errorf("%s: AllowsAnyWrite()=%v, want %v (globs %v)", c.name, got, c.want, c.globs)
		}
	}
}
T
trap 'rm -f internal/tools/kloogate2_scope_test.go internal/agent/kloogate2_test.go /tmp/kloogate2_test.go' EXIT
sem=$(cd "$(dirname "$0")" && go test ./internal/tools/ -run TestKloogate2AllowsAnyWriteSemantics 2>&1)
[ "$(printf '%s\n' "$sem" | grep -c '^ok' || true)" -gt 0 ] \
  || { printf '%s\n' "$sem" | head -12; fail "AllowsAnyWrite has the wrong semantics (see the cases above)"; }

# --- BEHAVIOURAL: the real prompt that degenerated must not read as a change task ---
go build ./... || fail "go build ./... failed"
cat > /tmp/kloogate2_test.go <<'T'
package agent
import "testing"
// The exact shape that degenerated: a read-only validation prompt quoting a spec.
func TestReadOnlyScopeSuppressesChangeRails(t *testing.T) {
	prompt := "You are the VALIDATOR. Do not edit any file. " +
		"RISK: write NONE if you have none. ## The spec: add a cart, implement totals, " +
		"create the resolver, change the schema, wire it up, fix the rounding."
	if !taskWantsChange(prompt) {
		t.Skip("taskWantsChange no longer matches this prompt; the scope path is what this gate pins")
	}
	if changeRailsApply(prompt, false) {
		t.Fatal("a run that may write NOTHING must not be nudged to change something")
	}
	if !changeRailsApply(prompt, true) {
		t.Fatal("a run that MAY write must keep the no-edit rails")
	}
}
T
cp /tmp/kloogate2_test.go internal/agent/kloogate2_test.go
trap 'rm -f internal/agent/kloogate2_test.go /tmp/kloogate2_test.go' EXIT
out=$(go test ./internal/agent/ -run TestReadOnlyScopeSuppressesChangeRails 2>&1)
[ "$(printf '%s\n' "${out}" | grep -cE '^(ok|---[[:space:]]+SKIP)' || true)" -gt 0 ] \
  || { printf '%s\n' "$out" | tail -15; fail "the read-only-scope behaviour is not implemented (needs changeRailsApply(task, mayWrite))"; }

# --- PERMANENT tests, not just the one this gate injects ---
[ -f internal/tools/scope_test.go ] || fail "internal/tools/scope_test.go is missing"
[ "$(grep -c 'AllowsAnyWrite' internal/tools/scope_test.go || true)" -gt 0 ] \
  || fail "internal/tools/scope_test.go does not test AllowsAnyWrite; the gate's own injected test is temporary"
[ "$(grep -c 'changeRailsApply' internal/agent/finish_noedit_test.go 2>/dev/null || true)" -gt 0 ] \
  || fail "internal/agent/finish_noedit_test.go does not test changeRailsApply"
# and they must actually RUN (a test file with no matching -run name proves nothing)
named=$(go test ./internal/tools/ ./internal/agent/ -run 'AllowsAnyWrite|ChangeRails' 2>&1)
[ "$(printf '%s\n' "$named" | grep -c 'no tests to run' || true)" -eq 0 ] \
  || { printf '%s\n' "$named" | head -5; fail "the new tests do not run under their own names"; }

# --- the whole suite ---
all=$(go test ./... 2>&1)
if [ "$(printf '%s\n' "${all}" | grep -cE '^(FAIL|---[[:space:]]+FAIL)' || true)" -gt 0 ]; then
  echo "--- failing packages and tests ---"
  printf '%s\n' "$all" | grep -E '^(FAIL|---[[:space:]]+FAIL)' | head -20
  fail "go test ./... has failures, named above"
fi
[ "$(printf '%s\n' "${all}" | grep -c '^ok' || true)" -gt 0 ] || { printf '%s\n' "$all" | tail -20; fail "go test ./... produced no ok lines"; }

echo "GATE OK (a read-only run is no longer told to change something)"

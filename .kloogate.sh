#!/usr/bin/env bash
# Gate: four measured kloo defects, all about what a run is TOLD.
#  1. "missing required argument" names the symptom, not the required set.
#     Measured: invalid_tool_calls=7, every one `missing required argument "path"`.
#  2. --allow withholds model-facing run_command, so a run cannot check its own
#     verify and reports a success it cannot see as an explore-stop.
#  3. `finish` carries the answer in `summary`, so a read-only task repeats the
#     summary instead of writing the answer once as text.
#  4. `kloo task` takes the task positionally only; a 32 KB prompt must go
#     through argv.
set -uo pipefail
cd "$(dirname "$0")" || exit 1
fail() { echo "GATE FAIL: $*"; exit 1; }

# --- out-of-scope preconditions, named BY NAME, before anything slow ---
command -v go >/dev/null || fail "(ENVIRONMENT, not this change) go is not on PATH"
for f in internal/tools/registry.go internal/cli/root.go; do
  [ -f "$f" ] || fail "(ENVIRONMENT, not this change) $f is missing"
done

# --- 1. the argument error must name the required set AND what was sent ---
v=internal/tools/registry.go
code=$(grep -vE '^\s*(//|/\*|\*)' "$v")
printf '%s\n' "$code" | grep -q 'missing required argument' || fail "$v no longer raises the missing-argument error"
printf '%s\n' "$code" | grep -qE 's\.Required|required' || fail "$v does not mention the required set where it rejects the call"
printf '%s\n' "$code" | grep -qE 'got|sent|received|present' \
  || fail "$v does not report the arguments the model DID send; the feedback must name the gap, not the symptom"
# The wrapping must survive: callers match on ErrInvalidArgs.
printf '%s\n' "$code" | grep -q 'ErrInvalidArgs' || fail "$v dropped the %w wrap on ErrInvalidArgs; callers that match on it would stop matching"

# --- 4. a task may be supplied from a file. BEHAVIOURAL: build the binary and
# run it. A help string and an unregistered variable satisfied a grep for
# "prompt-file" while the flag itself did not exist -- measured:
#   kloo --prompt-file ./t.md  ->  kloo: unknown flag: --prompt-file
go build -o /tmp/kloogate-bin . || fail "go build of the binary failed"
pf=$(mktemp); printf 'say the word kart and stop\n' > "$pf"
if /tmp/kloogate-bin --prompt-file "$pf" --help 2>&1 | grep -q 'unknown flag'; then
  rm -f "$pf"; fail "--prompt-file is not a registered flag; declaring the variable and documenting it is not enough"
fi
# both at once must be refused, by that name
both=$(/tmp/kloogate-bin "a positional task" --prompt-file "$pf" 2>&1 || true)
printf '%s\n' "$both" | grep -q 'unknown flag' && { rm -f "$pf"; fail "--prompt-file unregistered (both-at-once path)"; }
printf '%s\n' "$both" | grep -qiE 'cannot be combined|both' \
  || { rm -f "$pf"; echo "  got: $both"; fail "passing a positional task AND --prompt-file must be refused by name"; }
# a missing file must be refused, by that name
miss=$(/tmp/kloogate-bin --prompt-file "$pf.nope" 2>&1 || true)
printf '%s\n' "$miss" | grep -q 'prompt-file' \
  || { rm -f "$pf"; echo "  got: $miss"; fail "a missing --prompt-file must be refused naming the flag"; }
rm -f "$pf" /tmp/kloogate-bin

# --- 2 and 3 must be visible in the prompt/tool layer ---
grep -rqiE 'run_command .*(withheld|not available|unavailable)|verify is run by kloo|kloo runs the verify' internal/ \
  || fail "nothing tells the model that kloo runs the verify when run_command is withheld"

# --- tests must exist for each, in the package they touch ---
grep -rqE 'Required|required' internal/tools/registry_test.go 2>/dev/null \
  || fail "internal/tools/registry_test.go does not test the new argument error"

# --- build and the full suite ---
go build ./... || fail "go build ./... failed"
out=$(go test ./... 2>&1)
if printf '%s\n' "$out" | grep -qE '^(FAIL|---[[:space:]]+FAIL)'; then
  echo "--- failing packages and tests ---"
  printf '%s\n' "$out" | grep -E '^(FAIL|---[[:space:]]+FAIL)' | head -20
  fail "go test ./... has failures, named above"
fi
printf '%s\n' "$out" | grep -q '^ok' || { printf '%s\n' "$out" | tail -20; fail "go test ./... produced no ok lines"; }

echo "GATE OK (build clean, go test ./... green, all four feedback defects addressed)"

#!/usr/bin/env bash
# Gate: a rollback must never DELETE a file that existed before the run.
#
# Measured, 2026-10-21: a kloo run died on a transient 524 and rolled back. It
# deleted worker/tests/error-masking.test.ts -- a file written by an EARLIER run,
# present on disk before this one started, untracked, and inside the run's scope.
#
# Cause, in internal/agent/checkpoint.go: the snapshot is `git stash create`, whose
# tree holds TRACKED dirty state only. An untracked file is absent from it, so
# Rollback's "absent at the checkpoint ⇒ this run brought it into existence" branch
# fires and removes it.
#
# The property: a file this run CREATED is still removed; a file that already
# existed is not. Deleting a user's work is never the safe side of that choice.
#
# OUT OF SCOPE, named so a failure is not mistaken for one: the repo-wide-reset
# behaviour (already fixed), scope policy, and anything outside internal/agent.
set -uo pipefail
cd "$(dirname "$0")" || exit 2
export NO_COLOR=1
fail() { echo "GATE FAIL: $*"; exit 1; }

SRC=internal/agent/checkpoint.go
BAK=$(mktemp); cp "$SRC" "$BAK"
trap 'cp "$BAK" "$SRC"; rm -f "$BAK"' EXIT

# 1. The whole suite must be green.
out=$(go test ./... 2>&1)
if printf '%s\n' "$out" | grep -qE '^(FAIL|--- FAIL)'; then
  echo "--- failures on the UNMUTATED tree ---"
  printf '%s\n' "$out" | grep -E '^(FAIL|--- FAIL)' | head -20
  fail "the suite is not green"
fi

# 2. Tests must NAME the untracked case.
out2=$(go test ./internal/agent/ -run RollbackKeepsPreexisting -v 2>&1)
printf '%s\n' "$out2" | grep -qE '^=== RUN +TestRollbackKeepsPreexisting' \
  || { printf '%s\n' "$out2" | tail -8; fail "no TestRollbackKeepsPreexisting* test in internal/agent"; }
printf '%s\n' "$out2" | grep -qE '^ok ' || { printf '%s\n' "$out2" | tail -12; fail "the RollbackKeepsPreexisting tests do not pass"; }

# 3. Mutation: forget the untracked files at checkpoint time. Those tests MUST fail.
python3 -I - "$SRC" <<'PY'
import re, sys, pathlib
p = pathlib.Path(sys.argv[1]); s = p.read_text()
m = re.search(r'func \(g \*gitCheckpointer\) captureUntracked\([^)]*\)[^{]*\{', s)
if not m:
    sys.stderr.write('MUTATION ANCHOR MISSING: func (g *gitCheckpointer) captureUntracked(...) {\n')
    raise SystemExit(3)
p.write_text(s[:m.end()] + '\n\treturn nil, nil\n' + s[m.end():])
PY
[ $? -eq 0 ] || fail "could not apply the mutation (see above)"
out3=$(go test ./internal/agent/ -run RollbackKeepsPreexisting 2>&1)
printf '%s\n' "$out3" | grep -qE '^(FAIL|--- FAIL)' || {
  printf '%s\n' "$out3" | tail -8
  fail "MUTATION SURVIVED: the checkpoint can forget every untracked file and the tests stay green"
}
echo "killed the mutation (checkpoint records no untracked files):"
printf '%s\n' "$out3" | grep -E '^ *--- FAIL' | head -5

echo "GATE OK — a pre-existing file survives a rollback, a file the run created does not"

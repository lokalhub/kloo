#!/usr/bin/env bash
# kloo — working-memory A/B harness (overview §9, Phase P00 / task T00.4 / D3).
#
# OPERATOR-RUN CONFIRMATION, *not* a CI gate. The merge bar is the offline
# DoD test `go test ./internal/agent -run TestMemoryDoD` (D1 + D2, mocked LLM,
# real loop). This script corroborates that result against a REAL local model on
# a deliberately pinned small context window (snappy on llama.cpp, --ctx-size
# 8192), the most context-constrained realistic case the feature targets.
#
# THE METRIC (precise). Cumulative tokens always rise (a running sum). The claim
# the harness checks is about the PER-STEP prompt size: with working memory on it
# must stay bounded by the window every turn; the baseline grows until it
# overflows. Headless prints a cumulative `tokens N/M` line each step, so the
# per-step usage is its first difference (Δ = tokensₖ − tokensₖ₋₁) ≈ that turn's
# prompt+completion. We assert the AFTER run's Δ stays bounded while the BASELINE
# run's Δ climbs past the window.
#
# WHY TWO BINARIES (no runtime flag in P00). Working memory ships ON by default
# behind a nil gate; the explicit `--memory on/off` flag arrives with the BYO
# port in P02. So the "memory off" baseline is the PRE-P00 binary. Build it from
# the parent commit (or any ref without internal/agent/memory.go) and point
# KLOO_BASELINE_BIN at it; the "after" binary is built from the working tree.
#
#   usage:
#     # 1) serve snappy ALONE on a pinned window (watch `free -m`; keep /tmp off tmpfs):
#     #    llama-server -m snappy.gguf --ctx-size 8192
#     # 2) build the pre-P00 baseline binary, e.g.:
#     #    git worktree add /tmp/kloo-pre HEAD~1 && (cd /tmp/kloo-pre && go build -o /tmp/kloo-baseline .)
#     # 3) run the A/B:
#     KLOO_BASELINE_BIN=/tmp/kloo-baseline \
#     KLOO_ENDPOINT=http://127.0.0.1:8080/v1 KLOO_MODEL=snappy \
#     TASK="<a multi-file refactor that overflows 8k mid-run>" \
#     VERIFY="<the verify command>" \
#       benchmark/memory/run.sh
#
#   assert-only (re-check already-captured artifacts, no model needed):
#     benchmark/memory/run.sh --assert
#
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
ART="$HERE/artifacts"
mkdir -p "$ART"
WINDOW="${WINDOW:-8192}"          # must match llama.cpp --ctx-size AND the profile's maxContextTokens
SLACK="${SLACK:-2048}"            # completion + estimator headroom allowed on top of the prompt window
BASELINE="$ART/baseline.txt"
AFTER="$ART/after.txt"
PROFILE="$ART/profile.json"

# max_step_delta FILE — the largest per-step jump in the cumulative `tokens N/M`
# series (≈ the peak per-step prompt size). Empty/short series ⇒ 0.
max_step_delta() {
  grep -oE 'tokens [0-9]+' "$1" 2>/dev/null | awk '{print $2}' | awk '
    NR==1 { prev=$1; max=0; next }
    { d=$1-prev; if (d>max) max=d; prev=$1 }
    END { print max+0 }'
}

assert() {
  local ok=1
  if [ ! -s "$AFTER" ]; then
    echo "MISSING: $AFTER (run the after pass first)"; return 2
  fi
  local da; da="$(max_step_delta "$AFTER")"
  echo "after   peak per-step Δtokens: $da   (window=$WINDOW, slack=$SLACK)"
  if [ "$da" -le "$((WINDOW + SLACK))" ]; then
    echo "  PASS: memory-on per-step prompt stays bounded ≤ window(+slack)"
  else
    echo "  FAIL: memory-on per-step prompt exceeded the window"; ok=0
  fi
  if [ -s "$BASELINE" ]; then
    local db; db="$(max_step_delta "$BASELINE")"
    echo "baseline peak per-step Δtokens: $db"
    if [ "$db" -gt "$((WINDOW + SLACK))" ]; then
      echo "  PASS: baseline per-step prompt climbs PAST the window (the failure P00 removes)"
    else
      echo "  NOTE: baseline did not overflow — pick a bigger task / smaller --ctx-size to reproduce §9"
    fi
    if [ "$da" -lt "$db" ]; then
      echo "  PASS: after-run peak ($da) < baseline peak ($db) — the curve flattened"
    else
      echo "  FAIL: after-run did not flatten relative to baseline"; ok=0
    fi
  else
    echo "baseline: (none) — set KLOO_BASELINE_BIN to capture it for the full A/B"
  fi
  [ "$ok" -eq 1 ]
}

if [ "${1:-}" = "--assert" ]; then
  assert; exit $?
fi

: "${KLOO_ENDPOINT:?set KLOO_ENDPOINT (e.g. http://127.0.0.1:8080/v1)}"
: "${KLOO_MODEL:?set KLOO_MODEL (e.g. snappy)}"
: "${TASK:?set TASK to a multi-file refactor big enough to overflow the window mid-run}"
: "${VERIFY:?set VERIFY to the verify command}"

# Pin kloo's window to the model's --ctx-size via a one-off profile (no flag in
# P00). The profile is keyed by MODEL name (config.loadProfileEntry).
cat > "$PROFILE" <<JSON
{ "$KLOO_MODEL": { "maxContextTokens": $WINDOW } }
JSON

run_pass() {  # $1=binary  $2=outfile
  echo "running: $1  (endpoint=$KLOO_ENDPOINT model=$KLOO_MODEL window=$WINDOW)"
  "$1" --headless --profile "$PROFILE" --endpoint "$KLOO_ENDPOINT" --model "$KLOO_MODEL" \
       --verify "$VERIFY" "$TASK" | tee "$2"
}

if [ -n "${KLOO_BASELINE_BIN:-}" ]; then
  run_pass "$KLOO_BASELINE_BIN" "$BASELINE" || true   # baseline may legitimately overflow/fail
fi

AFTER_BIN="${KLOO_AFTER_BIN:-$ART/kloo-after}"
if [ ! -x "$AFTER_BIN" ]; then
  echo "building the after binary from the working tree → $AFTER_BIN"
  ( cd "$HERE/../.." && CGO_ENABLED=0 go build -o "$AFTER_BIN" . )
fi
run_pass "$AFTER_BIN" "$AFTER"

echo
assert

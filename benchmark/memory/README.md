# Working-memory A/B harness (overview §9 — Phase P00)

This is the **operator-run confirmation** for the working-memory feature: a real
local model on a deliberately pinned small context window. It is **not** a CI
gate.

## What is the gate, and what is this?

| | Gate? | What it proves | How to run |
|---|---|---|---|
| **D1 + D2** (`internal/agent/memory_dod_test.go`) | **Yes — merge bar** | Offline (mocked LLM, real loop): memory-on bounds every step's prompt ≤ window and the run completes; the no-memory control's per-step prompt climbs past the window. | `go test ./internal/agent -run TestMemoryDoD` (part of `make check`) |
| **D3** (`run.sh`, this dir) | **No — confirmation only** | Live snappy + llama.cpp on `--ctx-size 8192`: the after-run's per-step prompt stays bounded while the baseline overflows. | `benchmark/memory/run.sh` (operator) |

Reviewers must **not** treat an un-run `run.sh` as a missing gate. The merge bar
is D1/D2, which run offline in `make check`.

## The metric (precise)

Cumulative tokens always rise (a running sum), so "the curve flattens" is **not**
about the cumulative total. It is about the **per-step prompt size** (`sys+msgs`
fed to the model each turn):

- **memory on** → bounded by `maxContextTokens` every turn;
- **baseline (off)** → grows until it overflows the window.

Headless prints a cumulative `tokens N/M` line per step, so the per-step size is
its **first difference** `Δ = tokensₖ − tokensₖ₋₁` ≈ that turn's prompt+completion.
`run.sh` asserts the after-run's peak Δ stays within `window + SLACK` while the
baseline's peak Δ climbs past it.

## Why two binaries (no `--memory` flag in P00)

Working memory ships **on by default** behind a nil gate; the explicit
`--memory on/off` flag arrives with the BYO recall port in **P02**. So the
"memory off" baseline is the **pre-P00 binary** — build it from a ref that
predates `internal/agent/memory.go` and point `KLOO_BASELINE_BIN` at it.

## Procedure

```sh
# 1) Serve snappy ALONE on a pinned window. Do NOT co-load the 32B `smart` model;
#    keep /tmp off tmpfs (the OOM trap); watch `free -m`.
llama-server -m snappy.gguf --ctx-size 8192          # or --ctx-size 4096 to stress harder

# 2) Build the pre-P00 baseline binary (memory off):
git worktree add /tmp/kloo-pre HEAD~1
( cd /tmp/kloo-pre && CGO_ENABLED=0 go build -o /tmp/kloo-baseline . )

# 3) Run the A/B. Pick a TASK big enough to overflow 8k mid-run — a multi-file
#    refactor, NOT the small Ionic 3-tab benchmark (that fits).
KLOO_BASELINE_BIN=/tmp/kloo-baseline \
KLOO_ENDPOINT=http://127.0.0.1:8080/v1 KLOO_MODEL=snappy \
TASK="<a multi-file refactor>" VERIFY="<verify command>" \
  benchmark/memory/run.sh
```

Artifacts land in `artifacts/`: `baseline.txt`, `after.txt`, and the generated
`profile.json` (which pins kloo's `maxContextTokens` to the model's `--ctx-size`,
since P00 has no flag for it). Re-check captured runs without a model via
`benchmark/memory/run.sh --assert`.

**Success** = the after-run completes and its per-step prompt series stays ≤
window (flattens), while the baseline climbs/overflows.

## Knobs

| Env | Default | Meaning |
|---|---|---|
| `WINDOW` | `8192` | Must match llama.cpp `--ctx-size` and the profile's `maxContextTokens`. |
| `SLACK` | `2048` | Completion + estimator headroom allowed on top of the prompt window. |
| `KLOO_BASELINE_BIN` | — | Pre-P00 binary for the "memory off" pass (optional; without it only the after-run is asserted). |
| `KLOO_AFTER_BIN` | built from the tree | The "memory on" binary. |

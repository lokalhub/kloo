# Benchmarking kloo

This guide is for running repeatable automation jobs across local and hosted
OpenAI-compatible models.

## Quick benchmark run

```sh
mkdir -p runs/qwen3/001
kloo --benchmark --provider local --model qwen3-coder --ctx 32768 \
  --verify "go test ./..." \
  "fix the benchmark fixture" | tee runs/qwen3/001/stdout.log
```

`--benchmark` runs the task loop, emits a final `KLOO_RESULT_JSON` line, and uses
stable exit codes. `--run-dir` is not an implemented flag in this build; the Gate 4
review deferred run directory artifacts, so capture stdout/stderr with shell tools
or tmux until that feature returns.

## Preflight with doctor

```sh
kloo doctor --json --provider local --model qwen3-coder --ctx 32768 \
  > runs/qwen3/doctor.json
```

Doctor resolves config without starting the model, MCP, TUI, task loop, or verify.
It redacts API keys and shows retry, MCP, and memory hook settings. See
[configuration.md](configuration.md#doctor-dry-run).

## Capability probe

```sh
kloo probe --json --provider local --model qwen3-coder \
  > runs/qwen3/probe.json
```

`kloo probe` runs cheap `tool_call`, `file_edit`, and `json_only` checks in a temp
workspace and removes that workspace before returning. A failed probe is a signal
to tune `toolFormat`, `--no-think`, model template settings, or provider choice
before spending a full benchmark slot.

## Run directories and artifacts

`--run-dir` is deferred. For now, create a directory per run and capture the public
artifacts yourself:

```sh
RUNROOT="runs/qwen3/001"
mkdir -p "$RUNROOT"
kloo doctor --json --provider local --model qwen3-coder > "$RUNROOT/doctor.json"
kloo probe --json --provider local --model qwen3-coder > "$RUNROOT/probe.json" || true
kloo --benchmark --provider local --model qwen3-coder "fix fixture" \
  > "$RUNROOT/stdout.log" 2> "$RUNROOT/stderr.log"
```

## Parsing KLOO_RESULT_JSON

```sh
rg '^KLOO_RESULT_JSON ' runs/qwen3/001/stdout.log \
  | sed 's/^KLOO_RESULT_JSON //' \
  | jq '{success, failure_code, steps, tokens, tool_counters, rail_fires}'
```

The summary includes `failure_code`, `failure_detail`, `tool_counters`,
`rail_fires`, verify result, token/step counts, and transcript tail. The same
schema is documented in [configuration.md](configuration.md#task-loop-and-benchmark-output).

## Exit codes and failure codes

Use the process exit code for coarse harness routing and `failure_code` for
analysis. Important benchmark exits:

| Exit | Meaning |
|---:|---|
| 0 | Verified success. |
| 10 | Verify failed or run was unverified. |
| 11 | Model transport/API/retry failure. |
| 12 | Tool-call/tool-dispatch failure. |
| 14 | Repetition/edit halt. |
| 15 | JSON-only failure. |
| 17 | Config or CLI usage error. |

See [configuration.md](configuration.md#task-loop-and-benchmark-output) for the
full exit-code and `failure_code` tables.

## Local llama.cpp / LM Studio models

```sh
kloo doctor --json --endpoint http://127.0.0.1:8080/v1 --model qwen3-coder --ctx 32768
kloo probe --endpoint http://127.0.0.1:8080/v1 --model qwen3-coder --ctx 32768
kloo --benchmark --endpoint http://127.0.0.1:8080/v1 --model qwen3-coder \
  --ctx 32768 --llm-cold-load-timeout 5m --llm-stream-idle-timeout 10m \
  --verify "go test ./..." "fix the failing test"
```

Match `--ctx` to the server context window. For thinking models that put private
reasoning where the endpoint exposes it as empty content, try `--no-think`.

## Hosted OpenRouter-style models

Use env vars for secrets:

```sh
export KLOO_API_KEY="$OPENROUTER_API_KEY"
kloo doctor --json --endpoint https://openrouter.ai/api/v1 \
  --model deepseek/deepseek-v4-flash
kloo probe --endpoint https://openrouter.ai/api/v1 \
  --model deepseek/deepseek-v4-flash
kloo --benchmark --endpoint https://openrouter.ai/api/v1 \
  --model deepseek/deepseek-v4-flash \
  --verify "go test ./..." "fix the failing test"
```

Profiles can name the provider once under `providers`; the model id stays verbatim.
Do not place API keys in run logs or committed profile files.

## Context, no-think, and retry tuning

Use `--ctx` for the real model window, `--no-think` for endpoints that support
`reasoning_effort:"none"`, and retry knobs for cold local runtimes:

```sh
kloo --benchmark --ctx 32768 --no-think \
  --llm-max-retries 4 \
  --llm-retry-codes 408,429,500,502,503,504 \
  --llm-retry-base-delay 2s \
  --llm-retry-max-delay 30s \
  --llm-cold-load-timeout 5m \
  --llm-stream-idle-timeout 10m \
  "fix fixture"
```

Set `--llm-max-retries 0` when a harness needs exactly one model-call attempt.

## tmux recipe

```sh
tmux new-session -d -s kloo-bench \
  'kloo --benchmark --provider local --model qwen3-coder "fix fixture"'
tmux capture-pane -pt kloo-bench -S - > runs/qwen3/001/tmux-pane.txt
tmux attach -t kloo-bench
```

tmux is useful for long local runs where you want live observation and a pane log.

## Verify is the success signal

kloo only marks a run successful after a real verify command passes. The model's
`finish` call or prose answer is not enough. Always set `--verify` in benchmark
harnesses unless auto-detection is exactly what you want.

## Memory hooks

Optional BYO memory uses MCP. Configure `mcpServers` and the reserved `memory`
block, then confirm with:

```sh
kloo doctor --json --profile profiles.json | jq '.memory'
```

See [mcp.md](mcp.md) and [memory.md](memory.md) for the trust boundary and profile
schema.

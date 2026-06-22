# kloo setup & requirements

What you need to run kloo, and the two ways to point it at a model.

## Prerequisites

| Requirement | Why | Notes |
|---|---|---|
| The `kloo` binary | — | `make binary` (needs Go, `CGO_ENABLED=0`) or a release binary. Single static binary, no runtime deps. |
| An OpenAI-compatible endpoint | kloo is BYO-inference; it speaks `/v1/chat/completions` with SSE streaming. | Local (llama.cpp, Ollama, vLLM, LM Studio…) or hosted (OpenRouter, OpenAI…). See below. |
| A git repo in the working dir | Checkpoint + rollback snapshot the tree with `git stash create` before edits and restore on abort. | **Strongly recommended.** Without a repo, checkpoint/rollback is disabled (`ErrNotGitRepo`) — the loop still runs, but a bad run can't be auto-reverted. |
| A meaningful `--verify` command | It is the loop's only success signal. | See [The verify command is the spec](#the-verify-command-is-the-spec). |

## Option A — local model

The default. kloo ships pointed at `http://127.0.0.1:8080/v1` (the llama.cpp
server's default), with the placeholder model `local`.

1. Serve a coder model on any OpenAI-compatible local runtime —
   [llama.cpp's server](https://github.com/ggml-org/llama.cpp) (the one we test
   with), Ollama, vLLM, or LM Studio. For native tool-calling on llama.cpp, launch
   with `--jinja`. A good local choice is a Qwen2.5-Coder / Qwen3-Coder model.
2. Point kloo at it. A single-model llama.cpp server ignores the model name, so the
   `local` default just works; for Ollama/vLLM, pass the served name:
   ```sh
   kloo "say hi"                          # llama.cpp single-model — uses what's loaded
   kloo --model qwen2.5-coder "say hi"    # Ollama/vLLM — name the served model
   ```
3. Run a task in the TUI:
   ```sh
   kloo --verify 'npm run build && bash benchmark/assert.sh src'
   ```

No API key is needed — a local server has no auth.

## Option B — hosted provider (OpenRouter, OpenAI, …)

kloo works with any OpenAI-compatible hosted endpoint. You set three things:
the endpoint, the model, and a bearer token.

```sh
export KLOO_API_KEY="$OPENROUTER_API_KEY"      # or OPENAI_API_KEY (used as fallback)
kloo --effort heavy \
     --endpoint https://openrouter.ai/api/v1 \
     --model deepseek/deepseek-v4-flash \
     --verify 'npm run build && bash benchmark/assert.sh src'
```

- `KLOO_API_KEY` is the bearer token; if unset, kloo falls back to `OPENAI_API_KEY`.
- The hosted model name goes in `--model` verbatim (e.g. `deepseek/deepseek-v4-flash`).
- Hosted models don't run on your RAM — useful for big refactors that would OOM a
  local 32B. (They do cost per token; the effort budgets bound the spend.)

## The verify command is the spec

`--verify` is the single most important setup decision. The autonomous loop trusts
**only** this command's exit code to decide "am I done?" — not the model's
self-report. So the verify command must:

- **FAIL on the unsolved state**, and
- **PASS only when the task is genuinely complete.**

The classic trap: using a build-only verify for a task that the *starting* code
already builds. `npm run build` passes on an untouched Ionic skeleton, so the loop
declares success after one step having changed nothing. The fix is to **conjoin a
structural check that fails until the work is actually done**:

```sh
--verify 'npm run build && bash benchmark/assert.sh src'
```

Here `assert.sh` fails (exit 1) until the rename/rework is structurally complete, so
the loop keeps working until *both* build-green and structure-correct hold. Design
your verify the same way: build/lint/test for correctness **plus** an assertion that
encodes the goal.

See **[configuration.md](configuration.md)** for the full flag/env/profile reference.

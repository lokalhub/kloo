# kloo setup & requirements

What you need to run kloo, and the two ways to point it at a model.

## Prerequisites

| Requirement | Why | Notes |
|---|---|---|
| The `kloo` binary | — | Build from source with [Go 1.25+](https://go.dev/dl/) (`make binary` / `go install github.com/lokalhub/kloo@latest`; ensure `go` is on your `PATH`), **or** download a prebuilt [release binary](https://github.com/lokalhub/kloo/releases) (no Go needed). Single static binary, `CGO_ENABLED=0`, no runtime deps. |
| An OpenAI-compatible endpoint | kloo is BYO-inference; it speaks `/v1/chat/completions` with SSE streaming. | Local (llama.cpp, Ollama, vLLM, LM Studio…) or hosted (OpenRouter, OpenAI…). See below. |
| A git repo in the working dir | Checkpoint + rollback snapshot the tree with `git stash create` before edits and restore on abort. | **Strongly recommended.** Without a repo, checkpoint/rollback is disabled (`ErrNotGitRepo`) — the loop still runs, but a bad run can't be auto-reverted. |
| A recognised project (or `--verify`) | A real build/test exit code is the loop's only success signal. kloo auto-detects the project's command; pass `--verify` to override it or to cover an unrecognised project. | See [The verify command is the spec](#the-verify-command-is-the-spec). |

## Option A — local model

The default. kloo ships pointed at `http://127.0.0.1:8080/v1` (the llama.cpp
server's default), with the placeholder model `local`.

1. Serve a coder model on any OpenAI-compatible local runtime —
   [llama.cpp's server](https://github.com/ggml-org/llama.cpp) (the one we test
   with), Ollama, vLLM, or LM Studio. For native tool-calling on llama.cpp, launch
   with `--jinja`. A good local choice is a Qwen2.5-Coder / Qwen3-Coder model.
2. Point kloo at it — **and mind the model name** (see the box below):
   ```sh
   kloo "say hi"                          # single-model llama.cpp — uses what's loaded
   kloo --model snappy "say hi"           # multi-model server (llama-swap/Ollama/vLLM) — name it
   ```
3. Run a task in the TUI:
   ```sh
   kloo --model <served-name>     # verify auto-detected from the project; --verify to override
   ```

No API key is needed — a local server has no auth.

> **Single-model vs multi-model servers — `--model` matters.**
> The default `--model local` is a placeholder that works **only with a single-model
> server** (a plain `llama-server -m model.gguf`), which *ignores* the model field and
> serves whatever's loaded.
>
> **Multi-model servers route by the model name** and will reject an unknown one:
> - **llama-swap** (serves `snappy`/`smart`, swaps on demand) → `local` returns
>   `{"error":"no router for requested model"}`.
> - **Ollama** → must match a pulled tag (e.g. `qwen2.5-coder:32b`).
> - **vLLM / LM Studio** → must match the loaded model id.
>
> So on a multi-model endpoint, **always pass `--model <served-name>`** (or set
> `KLOO_MODEL=<name>` / `/model <name>` in the TUI). Symptom if you forget: a 1-step
> run that errors immediately with a routing / unknown-model error.

## Option B — hosted provider (OpenRouter, OpenAI, …)

kloo works with any OpenAI-compatible hosted endpoint. You set three things:
the endpoint, the model, and a bearer token.

```sh
export KLOO_API_KEY="$OPENROUTER_API_KEY"      # or OPENAI_API_KEY (used as fallback)
kloo --endpoint https://openrouter.ai/api/v1 --model deepseek/deepseek-v4-flash
```

Or name the endpoint+key once in a `providers` block in `profiles.json` and select
it with `--provider` (see [configuration.md](configuration.md#providers--endpointkey--model-aliases)):

```sh
kloo --provider openrouter --model dsv4        # endpoint + key + real model id from the profile
```

- `KLOO_API_KEY` is the bearer token; if unset, kloo falls back to `OPENAI_API_KEY`.
- The hosted model name goes in `--model` verbatim (e.g. `deepseek/deepseek-v4-flash`).
- Verify is auto-detected; add `--verify '<cmd>'` only to override (e.g. to conjoin a
  structural check, as in [the verify section](#the-verify-command-is-the-spec)).
- Hosted models don't run on your RAM — useful for big refactors that would OOM a
  local 32B. (They do cost per token; the effort budgets bound the spend.)

## The verify command is the spec

The verify command is the single most important part of a run. The autonomous loop
trusts **only** this command's exit code to decide "am I done?" — not the model's
self-report. kloo **auto-detects** it from the project (e.g. an Ionic/Angular app →
`npm run build`); `--verify` overrides the detected command when you need a
stricter or task-specific check. A good verify command must:

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

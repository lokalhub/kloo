# kloo

Autonomous coding CLI for small **local** LLMs. kloo drives any OpenAI-compatible
endpoint (llama.cpp, Ollama, vLLM, OpenAI, OpenRouter…) to edit and verify code on
its own, in an interactive
[Bubble Tea](https://github.com/charmbracelet/bubbletea) TUI that aims for a
Claude-Code-style feel. (We test against [llama.cpp](https://github.com/ggml-org/llama.cpp)
with Qwen-Coder models.)

Single static Go binary, `CGO_ENABLED=0`, no runtime dependencies.

## How it works

The intelligence lives in the **harness**, not the model: kloo runs the loop and
verifies every step, while the model does one bounded thing per turn.

```mermaid
flowchart TD
    Task([your task]) --> Plan[plan one step]
    Plan --> Tool["one tool call<br/>read · edit · ls · run_command"]
    Tool --> Apply[apply edit / capture output]
    Apply --> Verify{{"verify<br/>auto-detected build/test"}}
    Verify -- passes --> Done([done ✓])
    Verify -- fails --> Rails{budget or churn<br/>exceeded?}
    Rails -- no --> Plan
    Rails -- yes --> Stop[stop &amp; report<br/>· optional git rollback]
    Tool <-->|OpenAI-compatible| Model[("your model<br/>llama.cpp · Ollama · OpenAI · OpenRouter")]
```

A real build/test exit code is the **only** success signal kloo trusts — not the
model's self-report. kloo auto-detects the project's command (e.g. an Ionic app →
`npm run build`); `--verify` overrides it, and an unrecognised project runs
unverified (finish stops it, but no run is marked success). See
[docs/setup.md](docs/setup.md#the-verify-command-is-the-spec).

To keep a small-context model (e.g. 8k) on-task while it auto-traverses the
codebase, kloo rebuilds the prompt each turn — pinning the goal, the current file
(re-read fresh), and the last verify result, while folding old exploration into a
running summary. **[docs/memory.md](docs/memory.md)** has the diagrams. The model
navigates with `search` (jailed regex grep → `file:line` results), `read_file`,
`list_dir`, and **`read_dir`** (bulk-reads a whole folder in one call). All are
skip-aware (deps/build dirs + `.gitignore`) and bounded — so `search` to locate →
`read_dir` the area → `edit_file` is the find-read-change loop, fast even for a
big-context model.

For UI/e2e work the agent can **bring up its own stack**: `run_command` with
`background: true` starts a long-running process (a dev server, worker sim, watcher)
**detached** and returns an id immediately, instead of blocking until it exits;
**`command_output`** then reads that process's new output (to wait until it is ready)
and stops it. Anything started this way is **auto-stopped when the run ends**, so a
server never leaks across runs. This is what lets kloo build → serve → run e2e →
report on its own.

If your repo has an **`AGENTS.md`**, kloo loads it into the system prompt and
follows it every turn — checked in the launch directory *and* immediate
subdirectories, so a project that lives in a subdir (`./myApp`) still has its rules
applied. An `AGENTS.md` can **`@import`** other files (shared convention docs, deep
project rules) so their content is pinned every turn too — by default within the
workspace, or from outside it with `--allowed-dirs`. See
**[docs/configuration.md](docs/configuration.md#project-instructions-agentsmd)**.

kloo also remembers the conversation **across runs and restarts**, per workspace,
so follow-ups ("what's the issue?", "continue") resume with context. Sessions live
in `{workspace}/.kloo/sessions/` (git-ignored automatically). Each `kloo` launch
starts a **fresh** session; on exit it prints the session id so you can reopen it
with `--resume <id>`. See **[docs/sessions.md](docs/sessions.md)**.

kloo is also an **MCP client**: declare external [MCP](https://modelcontextprotocol.io)
servers in your profile and their tools join the vocabulary as namespaced
builtins (`<server>__<tool>`) — with a small-model-safe default that keeps a big
server's many schemas out of the window until you curate them. MCP tools run
outside kloo's workspace sandbox, so only connect servers you trust; `--no-mcp`
disables it. See **[docs/mcp.md](docs/mcp.md)**.

## Quick start

**Requires [Go 1.25+](https://go.dev/dl/)** to build or `go install` from source —
make sure it's on your `PATH` (`go version` should print a version). Don't have Go?
Grab a prebuilt binary from [Releases](https://github.com/lokalhub/kloo/releases)
instead — no Go needed.

Install with Go:

```sh
go install github.com/lokalhub/kloo@latest   # → $(go env GOPATH)/bin/kloo
```

Or build from a checkout:

```sh
make binary          # build ./bin/kloo  (needs Go 1.25+ on PATH)
./bin/kloo           # interactive TUI session
./bin/kloo "say hi"  # one-shot, streamed to stdout
```

kloo talks to an OpenAI-compatible endpoint (default
`http://127.0.0.1:8080/v1`, model `local` — a placeholder a single-model llama.cpp
server ignores). Point it at your own with `--endpoint` / `--model` or the
`KLOO_*` env vars. For a **hosted** provider (OpenRouter, OpenAI, …) also set a
bearer token:

```sh
export KLOO_API_KEY="$OPENROUTER_API_KEY"   # falls back to OPENAI_API_KEY
kloo --endpoint https://openrouter.ai/api/v1 --model deepseek/deepseek-v4-flash
```

Better, name your endpoints once in `profiles.json` under a **`providers`** block
(endpoint + key + per-provider model aliases) and select one with `--provider` —
so the same model served by several providers is just one alias each:

```sh
kloo --provider openrouter --model dsv4   # endpoint + key + real model id from the profile
```

(No `--verify` needed — kloo auto-detects the project's build/test; see below.) See
**[docs/setup.md](docs/setup.md)** for prerequisites and the local/hosted recipes,
and **[docs/configuration.md](docs/configuration.md)** for the `providers` schema.

## Usage

| Invocation | What it does |
|---|---|
| `kloo` | Launch the interactive TUI session (autonomous loop under the UI). |
| `kloo "<task>"` | One-shot: stream a single completion to stdout (scripting). |
| `kloo --headless "<task>"` | Run the autonomous loop non-interactively, streaming progress to stdout (verify auto-detected; `--verify` to override). |

### Common flags

| Flag | Default | Meaning |
|---|---|---|
| `--effort` | `medium` | Effort tier (`fast`\|`medium`\|`heavy`) — seeds step/token budgets + churn patience. |
| `--model` | `local` | Model your endpoint serves (placeholder `local` for single-model llama.cpp). With `--provider`, a model alias from the profile. |
| `--provider` | _(unset)_ | Named provider from the profile's `providers` block — sets endpoint + key and scopes the `--model` alias lookup. |
| `--endpoint` | `http://127.0.0.1:8080/v1` | OpenAI-compatible base URL (or supplied by `--provider`). |
| `--mode` | `auto` | Run mode (`auto`\|`manual`). |
| `--max-steps` | `500` | Max autonomous steps (also seeded by `--effort`: fast 50 · medium 500 · heavy 1000). |
| `--ctx` | `8000` | Per-step context window — set it to match your server's `-c`. Needed for a llama-swap/Ollama **alias** (`snappy`, `smart`) the bundled defaults can't size by model id; otherwise kloo over-compacts to 8k on a 32k server. |
| `--temperature` | `0.1` | Sampling temperature. |
| `--verify` | _(auto-detected)_ | Override the verify command the loop runs each step (the real success signal); auto-detected from the project when unset. |
| `--lint` | _(auto-detected)_ | Override the fast **advisory** lint command run on edited files after each edit (see below); auto-detected from the project when unset. |
| `--no-lint` | `false` | Disable the fast advisory lint step (lint is on by default when a linter is detected). |
| `--headless` | `false` | Run the loop non-interactively (requires a task arg). |
| `--new` | `false` | Start a fresh session (the default; saved sessions are no longer auto-resumed). |
| `--resume` | _(unset)_ | Resume a specific saved session by id (printed on exit; see `{workspace}/.kloo/sessions`). |
| `--no-mcp` | `false` | Disable all [MCP servers](docs/mcp.md) for this run (overrides `KLOO_MCP` + profile). |
| `--profile` | _(unset)_ | Path to `profiles.json` (default `~/.kloo/profiles.json`, falls back to `~/.config/kloo/`). |

Config precedence is **flags > env (`KLOO_*`) > profile file > bundled per-model
defaults > built-in defaults**. kloo **bundles per-model defaults** for common
coding models (Qwen2.5-Coder, Qwen3-Coder, Devstral, DeepSeek…), so they run with
sensible `toolFormat`/`temperature`/`maxContextTokens` **without a hand-written
profile** — your profile/env/flags always win, and unknown models are unchanged.
`toolFormat` also accepts `"auto"` (a safe alias for unset/auto-select).
Env vars include `KLOO_ENDPOINT`, `KLOO_MODEL`, `KLOO_PROVIDER`, `KLOO_EFFORT`, `KLOO_CONTEXT_TOKENS` (= `--ctx`), and
`KLOO_API_KEY` (bearer token for hosted endpoints; falls back to
`OPENAI_API_KEY`); `KLOO_MCP=0` disables [MCP](docs/mcp.md); `KLOO_LINT=<cmd>`
overrides the advisory lint command and `KLOO_NO_LINT=1` disables it; `NO_COLOR`
disables all TUI colour (see below).

### Fast advisory lint

After each successful edit, kloo runs a **fast, auto-detected lint on the file(s)
you just edited** and feeds the output back to the model as a hint, so it can fix
obvious syntax/style mistakes on the next turn — a separate, quick signal from the
slower build/test **verify** gate (mirrors aider's lint-after-edit).

- **Advisory, not a gate.** Lint never decides success: a run still succeeds only
  on a green **verify** plus an edit, and lint output is never fed to the
  churn detector, so it can't stall a progressing run.
- **Auto-detected per project:** Go → `gofmt -l`; TypeScript → `tsc --noEmit`;
  JavaScript → `eslint`; Python → `ruff check` (else `flake8`). None detected ⇒ no
  lint step.
- **Configurable:** `--lint "<cmd>"` (or `KLOO_LINT`) overrides the detected
  command; `--no-lint` (or `KLOO_NO_LINT=1`) opts out. The resolved mode is logged
  once at startup. A repo with no linter, or `--no-lint`, behaves exactly as before.

Effort tiers seed the loop budgets in one switch (the model is independent).
**Churn detection is the primary "stop when stuck" guard**; tokens are unbounded
and steps/wall-clock are generous so long small-model runs aren't cut off:
`fast` (50 steps), `medium` (500 — the default), `heavy` (1000). Set `maxTokens`
in the profile for a hard cost cap.

Churn covers three degenerate patterns a small model can fall into: the same
**verify failure** repeated with no new edit, the same **edit** re-tried, and the
same **tool call** fired over and over (e.g. re-reading one empty file) — the last
catches read-only spins that leave no edit or verify signal, nudging the model
once before halting. kloo also **self-corrects edits**: when a `SEARCH/REPLACE`
doesn't match, it re-reads the file and hands the model the actual contents to
retry against — and when the target is empty, it tells the model to `write_file`
the contents instead of searching a void.

A **transient** model-call failure — an endpoint timeout, a cold model load, a 5xx,
or a dropped connection — is **retried** with exponential backoff (a couple of
attempts) rather than ending the run, so a local server that's slow or reloading
doesn't throw away a long session. Deterministic errors (4xx auth/bad-request) are
never retried. Common with `llama.cpp`/`llama-swap`, where the first call after a
model swap can be slow.

In the interactive TUI, a message that **isn't an actionable task** — a greeting,
"thanks", "looks good", or a question you can answer from the conversation — is
handled by a single **no-tools turn**: kloo replies directly instead of launching a
tool-driven run. This stops a small model from re-doing finished work when you just
say "thanks" on a resumed session. The most obvious acknowledgments are answered
instantly (no model call); everything else is classified by that one gated turn,
which has no tools and so *cannot* start work.
The **full reference** — every flag, env var, the effort table, and the
`profiles.json` schema — is in **[docs/configuration.md](docs/configuration.md)**.

## Interactive TUI

The TUI shows a live header (version · model · effort · running token total ·
step · mode), an animated thinking line, and a transcript of colour-coded tool cards,
diffs, command output, and assistant prose. Slash commands while running:
`/add`, `/model`, `/mode`, `/stop`, `/diff`; `Esc`/`Ctrl-C` interrupts;
`Ctrl-O` expands truncated command output. **Scroll** the transcript with the
mouse wheel or `PgUp`/`PgDn` — it sticks to the newest output unless you scroll
up. **Copy:** `Ctrl-Y` copies the last assistant reply to the clipboard (OSC 52,
works over SSH); or **`Shift`+drag** for native terminal selection (the mouse-wheel
scroll captures plain drag, so hold `Shift`). When a run stops on an error, the
report shows a plain-language reason (e.g. "Couldn't reach the model endpoint…"),
not a bare `ERROR`.

See **[docs/tui.md](docs/tui.md)** for the full TUI experience — the live token
counter, the semantic colour theme and `NO_COLOR` degrade, and the transcript
card/diff/output/markdown surfaces.

## Development

```sh
make          # build the binary → ./bin/kloo  (default target)
make check    # compile + vet + gofmt + test (mirrors CI; produces no binary)
make test
make run ARGS='"say hi"'
```

All automated gates are zero-lag: `go build ./...`, `go test ./...`,
`go vet ./...`, and `gofmt -l .` must be clean. Code lives under `internal/**`;
`main.go` is a thin entrypoint.

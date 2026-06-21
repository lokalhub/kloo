# kloo

Autonomous coding CLI for small **local** LLMs. kloo drives a local
[llama-swap](https://github.com/mostlygeek/llama-swap) (or any OpenAI-compatible)
endpoint to edit and verify code on its own, in an interactive
[Bubble Tea](https://github.com/charmbracelet/bubbletea) TUI that aims for a
Claude-Code-style feel.

Single static Go binary, `CGO_ENABLED=0`, no runtime dependencies.

## Quick start

```sh
make binary          # build ./bin/kloo
./bin/kloo           # interactive TUI session
./bin/kloo --model snappy "say hi"   # one-shot, streamed to stdout
```

kloo talks to an OpenAI-compatible endpoint (default
`http://127.0.0.1:8080/v1`, model `snappy`). Point it at your own with
`--endpoint` / `--model` or the `KLOO_*` env vars.

## Usage

| Invocation | What it does |
|---|---|
| `kloo` | Launch the interactive TUI session (autonomous loop under the UI). |
| `kloo "<task>"` | One-shot: stream a single completion to stdout (scripting). |
| `kloo --headless --verify '<cmd>' "<task>"` | Run the autonomous loop non-interactively, streaming progress to stdout. |

### Common flags

| Flag | Default | Meaning |
|---|---|---|
| `--effort` | `medium` | Effort tier (`fast`\|`medium`\|`heavy`) — seeds model + step/token budgets + churn patience. |
| `--model` | `snappy` | Model name; overrides the tier's model. |
| `--endpoint` | `http://127.0.0.1:8080/v1` | OpenAI-compatible base URL. |
| `--mode` | `auto` | Run mode (`auto`\|`manual`). |
| `--max-steps` | `40` | Max autonomous steps. |
| `--temperature` | `0.1` | Sampling temperature. |
| `--verify` | `go test ./...` | Verify command the loop runs each step (the real success signal). |
| `--headless` | `false` | Run the loop non-interactively (requires a task arg). |

Config precedence is **flags > env (`KLOO_*`) > profile file > defaults**.
Env vars include `KLOO_ENDPOINT`, `KLOO_MODEL`, and `KLOO_EFFORT`;
`NO_COLOR` disables all TUI colour (see below).

## Interactive TUI

The TUI shows a live header (model · effort · running token total · step ·
mode), an animated thinking line, and a transcript of colour-coded tool cards,
diffs, command output, and assistant prose. Slash commands while running:
`/add`, `/model`, `/mode`, `/stop`, `/diff`; `Esc`/`Ctrl-C` interrupts;
`Ctrl-O` expands truncated command output.

See **[docs/tui.md](docs/tui.md)** for the full TUI experience — the live token
counter, the semantic colour theme and `NO_COLOR` degrade, and the transcript
card/diff/output/markdown surfaces.

## Development

```sh
make check    # build + vet + gofmt check + test (mirrors CI)
make test
make run ARGS='--model snappy "say hi"'
```

All automated gates are zero-lag: `go build ./...`, `go test ./...`,
`go vet ./...`, and `gofmt -l .` must be clean. Code lives under `internal/**`;
`main.go` is a thin entrypoint.

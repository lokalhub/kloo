# AGENTS.md — working on kloo

kloo is an autonomous coding CLI for **small local LLMs**. It drives any
OpenAI-compatible endpoint to edit and verify code on its own, in a Bubble Tea TUI.
Single static Go binary, `CGO_ENABLED=0`, no runtime dependencies.

> This file is pinned into kloo's own system prompt on every turn and never
> compacted. Keep it short. If a section stops earning its tokens, cut it.

## The constraint that explains everything

**The model is small.** Assume it will not infer, will not remember, and will take
the shortest path that looks like success. Every design decision follows from that:

- **Judgment lives in Go, not in the prompt.** The churn rail, stall backstop,
  budget ceilings, scope policy, and clobber guard are deterministic and testable.
  Never "solve" a reliability problem by adding a sentence asking the model to be
  careful.
- **Tools fail loudly.** A tool that silently does nothing is the worst possible
  outcome — the model reports success and the loop spins. If a tool cannot do the
  thing, it returns an error saying so.
- **Prompts are prescriptive.** Say what to do, in order. Small models do not
  respond to nuance, hedging, or "consider whether".

## Commands

```bash
make check     # the full gate: build + vet + fmtcheck + test — run before every commit
make binary    # version-stamped build to ./bin/kloo (plain `go build` reports "dev")
make run ARGS='--model snappy "say hi"'
go test ./internal/tui/ -update    # regenerate golden frames after an intentional UI change
```

`make binary`, not `go build` — the version is stamped via ldflags, and an
unstamped binary reports `dev` and wastes your time when you're checking a fix
actually shipped. Relaunch the TUI to pick up a new binary.

## Layout

| Package | Role |
|---|---|
| `internal/agent` | The autonomous loop and its safety rails (`loop.go` is the spine; `churn.go`, `verify.go`, `budget.go`, `repair.go`, `memory.go`). |
| `internal/tools` | Agent-facing tools: files, search, `run_command` (+ background), scope enforcement, tool-call dialect parsing. |
| `internal/edit` | Deterministic SEARCH/REPLACE edit engine. |
| `internal/llm` | Hand-rolled OpenAI-compatible client: streaming, retries, cold-load. |
| `internal/tui` | Bubble Tea UI: transcript, cards, streaming, brand. |
| `internal/config` | Runtime resolution: flags → env → profile → defaults. |
| `internal/repomap` | Workspace → context repo map, under a token budget. |
| `internal/session` | Persisted conversations in `{workspace}/.kloo/sessions`. |
| `internal/mcp` | MCP client — the ONLY place MCP protocol knowledge lives. |
| `internal/cli` | Cobra wiring, headless mode, `probe`/`doctor`/`tokens`. |

## Invariants

Most are pinned by tests. Breaking one should fail the build, not a review.

- **Colour lives in `internal/tui/theme.go` only.** No other file in the package
  constructs a `lipgloss.Color` — a convention, not a scan, so hold the line in
  review. The codes themselves *are* pinned (`TestPaletteColourCodes`), which makes
  a retune deliberate. `brandColor` (kloo's identity — the splash and the `k>`
  prompt) stays distinct from `accentColor` (tool cards).
- **Capacity and appetite are separate numbers.** `MaxContextTokens` is what the
  model can hold; `CuratorBudgetTokens` is what kloo chooses to assemble. Never
  budget the repo map from the window — that is how a 900k-window model came to
  authorise a 252k-token map on every turn.
- **The prompt is ordered by volatility, ascending.** Stable content first, the
  re-curated repo map last. Providers cache a byte-identical prompt *prefix*, so
  anything volatile placed early invalidates everything after it.
- **Token counts are estimated, then corrected.** `internal/tokens` is
  entropy-aware (hashes cost ~2x what prose does) and calibrates against reported
  `prompt_tokens`. Never reintroduce a flat chars/N: it undercounts a lockfile by
  half, and the budget built on it overflows the real window.
- **Every workspace file read is size-capped.** `read_file` at 5 MiB, the repomap
  walk at 1 MiB. An uncapped read once OOM-killed kloo at 44 GB next to a models
  directory. Any new read path gets a cap.
- **Writes are jailed.** All file access goes through `tools.Workspace`; scope
  policy (`--allow`/`--deny`/`.kloo/scope.yaml`) narrows it further. Never bypass
  it for convenience.
- **`.kloo/` self-ignores.** Session transcripts can hold sensitive context and
  must never be committed.
- **Tool names are `snake_case`** and match what the prompt and the parsers expect.

## Testing

- **Table tests by default**, with the case name saying what behaviour is pinned.
- **Golden frames** for the TUI live in `internal/tui/testdata/`. Review the diff
  after `-update` — a golden regenerated without reading it is a test that no
  longer tests anything.
- **No sleeps.** Inject a clock (see `agent/budget.go`) rather than waiting.
- **Test the failure**, not just the happy path. Most of kloo's value is in what it
  does when the model misbehaves, so that is where the coverage belongs.

## Editing this codebase

- **Match the surrounding comment density.** This codebase explains *why* — the
  constraint, the failure it prevents, the thing that bit us. Comments that
  restate the code are noise; comments that carry a reason are the point.
- **Prefer extending a rail to adding a special case.** If a fix only works for one
  provider or one model, it is in the wrong layer.
- **Domain-free packages stay domain-free.** Nothing in `internal/` should know
  about a specific downstream consumer.

## Git

Conventional commits (`feat(agent):`, `fix(tools):`, `docs:`). Work on a branch,
open a PR, squash-merge — `master` is not committed to directly. Releases are tags
(`vX.Y.Z`) published with goreleaser.

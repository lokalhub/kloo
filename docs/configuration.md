# kloo configuration reference

Every knob kloo reads, where it comes from, and what it does. Source of truth:
`internal/config/config.go` and `internal/config/effort.go`.

## Precedence

```
flags  >  env (KLOO_*)  >  profile file  >  built-in defaults
```

```mermaid
flowchart LR
    D[built-in<br/>defaults] --> P["profile file<br/>~/.config/kloo/profiles.json"]
    P --> E["env<br/>KLOO_*"]
    E --> F[CLI flags]
    F --> R([effective config])
    style F fill:#d4edda,stroke:#28a745
    style R fill:#cce5ff,stroke:#004085
```

Each layer overrides the one before it — the rightmost source that sets a field wins.

The **effort tier** is resolved first and seeds the loop budgets (steps/tokens/
churn/wall-clock). The **model is a separate axis** — flags/env/profile set it
independently of the tier. An unset effort is `medium` (generous budgets, with
churn detection as the primary guard).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--effort` | `medium` | Effort tier (`fast`\|`medium`\|`heavy`) — seeds step/token budgets + churn patience (see table below). |
| `--model` | `local` | Model your endpoint serves. `local` is a neutral placeholder — a single-model llama.cpp server ignores it; set a real name for Ollama/OpenAI/OpenRouter. |
| `--endpoint` | `http://127.0.0.1:8080/v1` | OpenAI-compatible base URL. |
| `--mode` | `auto` | Run mode (`auto`\|`manual`). |
| `--max-steps` | `40` | Max autonomous steps. |
| `--temperature` | `0.1` | Sampling temperature. |
| `--verify` | `go test ./...` | Verify command run each step — **the real success signal** (see [setup.md](setup.md#the-verify-command-is-the-spec)). |
| `--headless` | `false` | Run the loop non-interactively (requires a task arg). |
| `--profile` | _(unset)_ | Path to `profiles.json`; defaults to `~/.config/kloo/profiles.json`. |

## Environment variables

| Var | Effect |
|---|---|
| `KLOO_ENDPOINT` | OpenAI-compatible base URL (same as `--endpoint`). |
| `KLOO_MODEL` | Model name (same as `--model`). |
| `KLOO_EFFORT` | Effort tier (same as `--effort`). |
| `KLOO_API_KEY` | Bearer token for the endpoint. Required for hosted providers (OpenRouter, OpenAI, …); not needed for a local llama.cpp / Ollama server, which has no auth. |
| `OPENAI_API_KEY` | Fallback bearer token used only when `KLOO_API_KEY` is unset. |
| `XDG_CONFIG_HOME` | If set, the profile file lives at `$XDG_CONFIG_HOME/kloo/profiles.json`. |
| `NO_COLOR` | Disables all TUI colour (see [tui.md](tui.md)). |

## Effort tiers

Selecting a tier seeds the loop budgets in one switch. **Churn detection (no
progress) is the primary "stop when stuck" signal**; the other budgets are loose
backstops — **tokens are unbounded** and steps/wall-clock are generous so a slow
local model isn't cut off mid-progress. A tier does **not** set the model — that's
a separate axis (`--model` / `KLOO_MODEL` / profile). Any field is overridable per
tier via the `efforts` section of the profile file.

| Tier | Max steps | Churn rounds | Max tokens | Wall-clock |
|---|---|---|---|---|
| `fast` | 50 | 2 | 0 (unbounded) | 900 s |
| `medium` _(default)_ | 500 | 3 | 0 (unbounded) | 3600 s |
| `heavy` | 1000 | 10 | 0 (unbounded) | 7200 s |

- **fast** — quick & decisive; low churn patience, bail early if stuck.
- **medium** — the balanced default; generous budgets.
- **heavy** — patient & thorough; for hard multi-file work.

Tokens default to **unbounded** because cost is the endpoint/service's domain (like
other CLI agents) and the working-memory feature is built to let small models run
long, many-step tasks — a token cap would cut those short. Set `maxTokens` in the
profile if you want a hard kloo-side cost cap.

## Budgets and context

| Knob | Default | Meaning |
|---|---|---|
| `maxContextTokens` | `8000` | Per-step context **window** (the hard ceiling for the whole assembled prompt). Also the working-memory compaction trigger — see below. Conservative for small local models. |
| `maxTokens` | `0` (unbounded) | Cumulative prompt+completion tokens per run. `0` ⇒ unbounded — the default; cost is the service's domain, churn/steps/wall-clock guard runaways. |
| `maxWallClockSeconds` | `3600` | Wall-clock ceiling per run — the final net for a churn-evading loop. `0` ⇒ unbounded. |
| `churnRounds` | `3` | Repeated-failure / repeated-edit rounds before the loop halts and reports. |

`maxTokens`, `maxWallClockSeconds`, and `churnRounds` are seeded by the effort tier;
`maxContextTokens` is a flat default. All are overridable per-model in the profile.

### `maxContextTokens` and working memory

As of the working-memory feature (P00), `maxContextTokens` governs **whole-prompt
compaction**, not just the repo map. Each turn kloo assembles a pin-hot set (the
task, the last verify result, the file under edit re-read fresh from disk, and the
recent turns) plus a running summary, and keeps the **entire** prompt under
`maxContextTokens`:

- When the projected prompt crosses **~70%** of the window, kloo folds the cold
  middle of the transcript into a deterministic running summary (keeping applied
  diffs and verify outcomes verbatim; stubbing raw file dumps — files are re-read
  from disk on demand). No model call is involved.
- The window is a **hard ceiling**: the repo map is capped at a fraction of it
  (so it can no longer consume the whole window), and content is shed in a fixed
  order to stay under it. The goal (the task) is never dropped.
- Set `maxContextTokens` to your model's real context size (e.g. match
  llama.cpp's `--ctx-size`). A larger window means later/less compaction; a small
  one means aggressive, early compaction — the manager manufactures a bigger
  effective window for small local models.

A headless run prints `compactions: N` in its report only when memory compacted
(`N > 0`); the TUI status line shows a `⟲N` indicator while it happens.

## Profile file

Optional. Default location `~/.config/kloo/profiles.json` (or
`$XDG_CONFIG_HOME/kloo/profiles.json`). A **missing** file is not an error —
defaults apply. A malformed file is an error.

Two sections, both optional:

- **Per-model entries** (keyed by model name) — overrides applied when that model
  is the resolved model.
- **`efforts`** — per-tier budget overrides applied to the built-in tier before the
  env/flag layers.

```jsonc
{
  // per-model overrides (key = model name as passed to --model / KLOO_MODEL)
  "qwen2.5-coder": {
    "toolFormat": "native",        // native | xml  (tool-call adapter)
    "temperature": 0.2,
    "fewShotPath": "/path/to/fewshot.txt",  // optional gold examples for the system prompt
    "maxContextTokens": 8000,
    "maxTokens": 200000,
    "maxWallClockSeconds": 600,
    "churnRounds": 3
  },
  "deepseek/deepseek-v4-flash": {
    "toolFormat": "native",
    "temperature": 0.1
  },

  // per-tier budget overrides (adjust a built-in effort tier)
  "efforts": {
    "heavy": {
      "maxSteps": 120,
      "churnRounds": 15,
      "maxTokens": 800000,
      "maxWallClockSeconds": 3600
    }
  }
}
```

Per-model fields: `toolFormat`, `temperature`, `fewShotPath`, `maxContextTokens`,
`maxTokens`, `maxWallClockSeconds`, `churnRounds`.
Per-tier (`efforts`) fields: `maxSteps`, `churnRounds`, `maxTokens`,
`maxWallClockSeconds` (budgets only — no model).

See **[setup.md](setup.md)** for prerequisites and the local/hosted endpoint
recipes.

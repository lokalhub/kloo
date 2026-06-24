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
| `--model` | `local` | Model your endpoint serves. `local` is a neutral placeholder — a single-model llama.cpp server ignores it; set a real name for Ollama/OpenAI/OpenRouter. With `--provider`, this is a **model alias** looked up in that provider's `models` map (see [providers](#providers--endpointkey--model-aliases)). |
| `--provider` | _(unset)_ | Named provider from the profile's `providers` block. Selecting one sets the endpoint + bearer key and scopes the `--model` alias lookup, so the same model on different providers is just one alias per provider. |
| `--endpoint` | `http://127.0.0.1:8080/v1` | OpenAI-compatible base URL. Set directly, or via `--provider` (a provider's `endpoint` wins over this default but loses to an explicit `--endpoint`/`KLOO_ENDPOINT`). |
| `--mode` | `auto` | Run mode (`auto`\|`manual`). |
| `--max-steps` | `40` | Max autonomous steps. |
| `--temperature` | `0.1` | Sampling temperature. |
| `--verify` | _(auto-detected)_ | Override the verify command run each step — **the real success signal**. When unset, kloo auto-detects the project's build/test (`package.json`→`npm run build`/`npm test`, `go.mod`→`go test ./...`, `Cargo.toml`→`cargo build`, `pyproject.toml`→`python -m pytest`). If nothing is recognised the run is **unverified** — `finish` stops it calmly, but no run is marked success. See [setup.md](setup.md#the-verify-command-is-the-spec). |
| `--headless` | `false` | Run the loop non-interactively (requires a task arg). |
| `--no-mcp` | `false` | Disable all [MCP servers](mcp.md) for this run (overrides `KLOO_MCP` and the profile's `mcpServers`). |
| `--profile` | _(unset)_ | Path to `profiles.json`; defaults to `~/.config/kloo/profiles.json`. |

## Environment variables

| Var | Effect |
|---|---|
| `KLOO_ENDPOINT` | OpenAI-compatible base URL (same as `--endpoint`). |
| `KLOO_MODEL` | Model name / alias (same as `--model`). |
| `KLOO_PROVIDER` | Named provider from the profile's `providers` block (same as `--provider`). |
| `KLOO_EFFORT` | Effort tier (same as `--effort`). |
| `KLOO_API_KEY` | Bearer token for the endpoint. Required for hosted providers (OpenRouter, OpenAI, …); not needed for a local llama.cpp / Ollama server, which has no auth. |
| `OPENAI_API_KEY` | Fallback bearer token used only when `KLOO_API_KEY` is unset. |
| `KLOO_MCP` | Set to `0` / `false` to disable all [MCP servers](mcp.md). `--no-mcp` overrides it; both override the profile. |
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

### `providers` — endpoint+key + model aliases

A reserved top-level key, **`providers`**, decouples *where you send a request*
(endpoint + bearer key) from *which model* you ask for. The same model is served
by many providers (OpenRouter, Together, Fireworks, the direct vendor API…), so
keying config by model name alone can't tell them apart — `providers` fixes that.

Each entry is a provider you name (e.g. `or` for OpenRouter), holding an
`endpoint`, an `apiKey`, and that provider's own `models` map of **aliases**. You
select one with `--provider <name>` / `KLOO_PROVIDER`, and `--model <alias>` then
resolves to that provider's real model id plus its tuning:

```jsonc
{
  "providers": {
    "or": {
      "endpoint": "https://openrouter.ai/api/v1",
      "apiKey": "${OPENROUTER_API_KEY}",   // ${VAR}/~ expanded — never inline the raw secret
      "models": {
        "dsv4": {                          // alias → real id + per-model tuning
          "model": "deepseek/deepseek-v4-flash",
          "toolFormat": "native",
          "temperature": 0.1,
          "maxContextTokens": 128000
        }
      }
    },
    "together": {
      "endpoint": "https://api.together.xyz/v1",
      "apiKey": "${TOGETHER_API_KEY}",
      "models": { "dsv4": { "model": "deepseek-ai/DeepSeek-V4-Flash" } }
    }
  }
}
```

```sh
kloo --provider or       --model dsv4   # → OpenRouter, deepseek/deepseek-v4-flash
kloo --provider together --model dsv4   # → Together,  deepseek-ai/DeepSeek-V4-Flash
```

The same alias (`dsv4`) maps to each provider's own slug, so switching providers
is one token. Notes:

- **Precedence is unchanged:** a provider sets endpoint/key at the *profile*
  layer, so `KLOO_ENDPOINT`/`KLOO_API_KEY` and `--endpoint` still override it
  (`flags > env > profile > defaults`). `KLOO_API_KEY` overrides a provider's key;
  the `OPENAI_API_KEY` fallback only applies when nothing else set a key.
- **`apiKey` is `expandValue`'d** (`${ENV_VAR}` / leading `~`), exactly like
  `mcpServers` headers — keep real secrets out of the committed file.
- **A `--model` with no matching alias is used verbatim** as the model id, so
  `--provider or --model gpt-4o` works without an alias entry.
- **An unknown `--provider` is a hard error** (not a silent fallback).
- Per-model fields inside an alias are the same set as the legacy top-level
  entries: `toolFormat`, `temperature`, `fewShotPath`, `maxContextTokens`,
  `maxTokens`, `maxWallClockSeconds`, `churnRounds`.

Legacy top-level model-keyed entries still work unchanged when no `--provider` is
given — `providers` is purely additive.

### `mcpServers` — external MCP tool servers

A third optional, reserved top-level key, **`mcpServers`**, declares external
[MCP](mcp.md) servers whose tools kloo consumes as builtins. Each entry is either a
stdio server (`command` + optional `args`/`env`) or an HTTP server (`url` +
optional HTTP-only `headers`), with an `exposeMode` (`curated`\|`lazy`\|`all`)
controlling how many of its tools enter the model's window. An optional sibling
`mcp` object holds the global cap (`maxExposedTools`, default 16).

```jsonc
{
  "mcpServers": {
    "mempalace": { "command": "mempalace-mcp", "args": ["--db", "~/.mempalace"],
                   "exposeMode": "curated", "expose": ["recall", "remember"] },
    "docs":      { "url": "https://mcp.example.com/mcp",
                   "headers": { "Authorization": "Bearer ${MCP_TOKEN}" },
                   "exposeMode": "lazy" }
  },
  "mcp": { "maxExposedTools": 16 }
}
```

`mcpServers` follows the same precedence as everything else (it's a profile-layer
block); the global on/off switch is `--no-mcp` / `KLOO_MCP` above it. Because kloo
**launches** the `command`/`args`/`env` you put here, MCP tools run outside the
workspace sandbox and your profile file is a trust root. Header values receive the
same `~` / `$VAR` expansion as stdio values, so prefer `${MCP_TOKEN}`-style
references instead of inline secrets; kloo never logs header values. The **full
reference, including the security model, curated-vs-lazy guidance, static header
auth details, and troubleshooting, is in [mcp.md](mcp.md)**.

See **[setup.md](setup.md)** for prerequisites and the local/hosted endpoint
recipes.

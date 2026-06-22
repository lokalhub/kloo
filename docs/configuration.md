# kloo configuration reference

Every knob kloo reads, where it comes from, and what it does. Source of truth:
`internal/config/config.go` and `internal/config/effort.go`.

## Precedence

```
flags  >  env (KLOO_*)  >  profile file  >  built-in defaults
```

One subtlety worth knowing: the **effort tier** is resolved first and seeds the
baseline (model + loop budgets + churn). Then the normal chain layers on top — so
`--model`, env, and the profile file still override what the tier picked. An unset
effort is `medium`, which equals kloo's historical flat defaults, so it changes
nothing for an existing setup.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--effort` | `medium` | Effort tier (`fast`\|`medium`\|`heavy`) — seeds model + step/token budgets + churn patience (see table below). |
| `--model` | `snappy` | Model name; overrides the tier's model. |
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
| `KLOO_API_KEY` | Bearer token for the endpoint. Required for hosted providers (OpenRouter, OpenAI, …); ignored by a local llama-swap, which has no auth. |
| `OPENAI_API_KEY` | Fallback bearer token used only when `KLOO_API_KEY` is unset. |
| `XDG_CONFIG_HOME` | If set, the profile file lives at `$XDG_CONFIG_HOME/kloo/profiles.json`. |
| `NO_COLOR` | Disables all TUI colour (see [tui.md](tui.md)). |

## Effort tiers

Selecting a tier seeds the model + all four loop budgets at once. Any field is
overridable per tier via the `efforts` section of the profile file.

| Tier | Model | Max steps | Churn rounds | Max tokens | Wall-clock |
|---|---|---|---|---|---|
| `fast` | `snappy` | 20 | 2 | 80 000 | 300 s |
| `medium` _(default)_ | `snappy` | 40 | 3 | 200 000 | 600 s |
| `heavy` | `smart` | 80 | 10 | 500 000 | 1800 s |

- **fast** — quick & decisive on the small model; bail early if stuck.
- **medium** — the balanced default (equals the legacy flat defaults).
- **heavy** — patient & thorough on the stronger model; for hard multi-file work.

## Budgets and context

| Knob | Default | Meaning |
|---|---|---|
| `maxContextTokens` | `8000` | Per-step context window the repo-map curator must stay under. Conservative for small local models. |
| `maxTokens` | `200000` | Cumulative prompt+completion tokens per run. `0` ⇒ unbounded. |
| `maxWallClockSeconds` | `600` | Wall-clock ceiling per run. `0` ⇒ unbounded. |
| `churnRounds` | `3` | Repeated-failure / repeated-edit rounds before the loop halts and reports. |

These are seeded by the effort tier (except `maxContextTokens`, which is a flat
default) and can be overridden per-model in the profile file.

## Profile file

Optional. Default location `~/.config/kloo/profiles.json` (or
`$XDG_CONFIG_HOME/kloo/profiles.json`). A **missing** file is not an error —
defaults apply. A malformed file is an error.

Two sections, both optional:

- **Per-model entries** (keyed by model name) — overrides applied when that model
  is the resolved model.
- **`efforts`** — per-tier overrides applied to the built-in tier before the
  per-model/env/flag layers.

```jsonc
{
  // per-model overrides (key = model name as passed to --model / KLOO_MODEL)
  "snappy": {
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

  // per-tier overrides (adjust a built-in effort tier)
  "efforts": {
    "heavy": {
      "model": "smart",
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
Per-tier (`efforts`) fields: `model`, `maxSteps`, `churnRounds`, `maxTokens`,
`maxWallClockSeconds`.

See **[setup.md](setup.md)** for prerequisites and the local/hosted endpoint
recipes.

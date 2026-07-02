# kloo profiles

A **profile file** (`profiles.json`) names your endpoints once and tunes per-model
behaviour, so you don't repeat `--endpoint`/`--model`/`--ctx` on every run. kloo reads
it from `~/.kloo/profiles.json` (falling back to `~/.config/kloo/profiles.json`), or
from an explicit `--profile <path>`.

Two independent axes:

- **`providers`** — a named `{endpoint, apiKey}` bundle. Select one with
  `--provider <name>` (or `KLOO_PROVIDER`); it sets the endpoint + bearer key. The
  model is chosen separately, so the same provider serves many models.
- **Top-level per-model entries** — keyed by the **real model id**, they tune
  `toolFormat`, `temperature`, `maxContextTokens`, retry policy, etc. for that model.

> **Secrets:** put API keys in the environment and reference them as `${VAR}` — never
> paste a raw key into the file. kloo expands `${VAR}` (and a leading `~`) when it
> loads the profile. kloo never prints keys in the TUI, logs, `doctor`, or the
> `KLOO_RESULT_JSON` summary.

## Precedence

```
flags > env (KLOO_*) > loaded profile > bundled per-model defaults > built-in defaults
```

An explicit launch flag (e.g. `--model`) always wins over the profile — including
after a live `/profile` switch (see below).

## Switching profiles live in the TUI

Two slash commands, deliberately distinct:

| Command | Scope |
|---|---|
| `/provider <name>` | Switch the endpoint + key to another provider **within the currently loaded profile**, for the next run. |
| `/profile <path>` | **Reload a different `profiles.json`** and re-resolve provider, endpoint, key, model tuning, context, temperature, and tool format for subsequent runs. |

`/profile` never writes to disk — it is a runtime override for the rest of the
session. It preserves your launch flags (so a `--model` you started with still wins),
keeps the current runtime intact if the new file fails to load, and shows a redacted
confirmation (`provider=… model=… endpoint=… ctx=…`, never the key). After a
`/profile` switch, `/provider` lists the **new** file's providers.

## Reference entries (copy-paste)

A single file can hold every entry below; kloo uses only the provider/model you select
at runtime.

### OpenRouter — Qwen Next Coder

```json
{
  "providers": {
    "openrouter": {
      "endpoint": "https://openrouter.ai/api/v1",
      "apiKey": "${OPENROUTER_API_KEY}"
    }
  },
  "qwen/qwen-next-coder": {
    "maxContextTokens": 128000,
    "temperature": 0.1,
    "toolFormat": "native",
    "llmMaxRetries": 2
  }
}
```

```sh
kloo --provider openrouter --model qwen/qwen-next-coder "fix the failing test"
```

### Local Qwen Next — 32k context (llama.cpp / llama-swap)

```json
{
  "providers": {
    "local": { "endpoint": "http://127.0.0.1:8080/v1" }
  },
  "qwen-next-32k": {
    "maxContextTokens": 32000,
    "temperature": 0.1,
    "toolFormat": "native"
  }
}
```

```sh
# Match maxContextTokens to the server's -c / --ctx-size.
kloo --provider local --model qwen-next-32k "add a health endpoint"
```

### Local Qwen Next — 128k context

```json
{
  "providers": {
    "local": { "endpoint": "http://127.0.0.1:8080/v1" }
  },
  "qwen-next-128k": {
    "maxContextTokens": 128000,
    "temperature": 0.1,
    "toolFormat": "native"
  }
}
```

```sh
kloo --provider local --model qwen-next-128k "refactor the parser package"
```

### DeepSeek

```json
{
  "providers": {
    "deepseek": {
      "endpoint": "https://api.deepseek.com/v1",
      "apiKey": "${DEEPSEEK_API_KEY}"
    }
  },
  "deepseek-chat": {
    "maxContextTokens": 64000,
    "temperature": 0.1,
    "toolFormat": "native",
    "llmMaxRetries": 2
  }
}
```

```sh
kloo --provider deepseek --model deepseek-chat "write unit tests for internal/edit"
```

## See also

- [configuration.md](configuration.md#profile-file) — the full `profiles.json` schema
  (providers, per-model tuning, `efforts`, `mcpServers`, `memory`).
- [configuration.md](configuration.md#providers--endpointkey) — the `providers` block.
- [tui.md](tui.md) — the interactive session, including `/provider` and `/profile`.

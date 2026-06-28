# MCP — connecting external tool servers

kloo is an **MCP (Model Context Protocol) _client_**: it connects to external MCP
servers and consumes their tools as if they were kloo builtins — same registry,
same tool cards, same loop. A connected server's tools appear to the model exactly
like `read_file` / `run_command`, just under a namespaced name.

kloo is **client-only**. It never runs as an MCP _server_ and never exposes itself
or your workspace to other MCP clients. Adding a server is an explicit opt-in you
make in your profile file.

> **Source of truth:** the loader is `internal/config/config.go`
> (`MCPServerEntry`, `loadMCPServers`) and the client is `internal/mcp`. This page
> matches that code; if they ever disagree, the code wins — open an issue.

## What MCP gives you

- **More tools, no kloo changes.** Point kloo at any MCP server (a memory backend,
  a docs index, a database tool, …) and its tools join the vocabulary.
- **Bring-your-own-memory.** kloo can call configured MCP recall/store tools at
  task-run boundaries through the reserved `memory` profile block.

If you don't configure any server, nothing changes — kloo behaves exactly as
before (the five builtins + `finish`).

## Configure servers

MCP servers are declared in kloo's existing profile JSON file
(`~/.kloo/profiles.json` by default, or `--profile <path>` — see
[configuration.md](configuration.md#profile-file)) under a reserved top-level
**`mcpServers`** key. It sits alongside your per-model entries and the `efforts`
block; the model-name lookup ignores it (just like `efforts`).

```jsonc
{
  "qwen3-coder": { "toolFormat": "native", "temperature": 0.1 },  // your existing per-model entry
  "efforts":     { "heavy": { "churnRounds": 15 } },              // your existing efforts block

  "mcpServers": {                                   // ← MCP servers live here
    "mempalace": {                                  // stdio server (kloo launches the command)
      "command": "mempalace-mcp",                   // required for stdio
      "args": ["--db", "~/.mempalace"],             // optional; ~ and $VAR are expanded
      "env": { "MEMPALACE_LOG": "warn" },           // optional; merged over kloo's environment
      "exposeMode": "curated",                      // curated | lazy | all   (see below)
      "expose": ["recall", "remember", "list_rooms"], // curated allowlist (original tool names)
      "timeoutSeconds": 30,                         // per-call timeout (default 30)
      "disabled": false                             // per-server kill-switch (default false)
    },
    "docs": {                                       // HTTP server (kloo connects to a URL)
      "url": "http://127.0.0.1:9000/mcp",           // required for HTTP (mutually exclusive with command)
      "headers": {                                  // optional; HTTP-only; values expand ~ and $VAR
        "Authorization": "Bearer ${MCP_TOKEN}"
      },
      "exposeMode": "lazy"                          // big server → lazy by default
    }
  },

  "mcp": { "maxExposedTools": 16 }                  // optional global cap (default 16) — see Curated vs lazy
}
```

For BYO memory, add a reserved top-level `memory` block that points at one of
those MCP servers:

```jsonc
{
  "mcpServers": {
    "memory": {"command": "mempalace-mcp", "expose": ["recall", "store"]}
  },
  "memory": {
    "enabled": true,
    "server": "memory",
    "recallTool": "recall",
    "storeTool": "store",
    "maxRecallBytes": 4096,
    "storeOnFailure": true
  }
}
```

`kloo doctor` shows this block without connecting to the server. During a task run,
recall/store failures are logged and the run continues.

### Server fields

| Field | Type | Applies to | Default | Meaning |
|---|---|---|---|---|
| `command` | string | stdio | — | Executable kloo launches (no shell). **Exactly one** of `command` / `url` per server. |
| `args` | string[] | stdio | `[]` | Arguments passed to `command`. |
| `env` | object | stdio | `{}` | Extra environment variables, merged over kloo's own environment. |
| `url` | string | HTTP | — | MCP endpoint kloo connects to. Mutually exclusive with `command`. |
| `headers` | object | HTTP | `{}` | Static HTTP headers sent to this server's configured origin. Header values expand `~` and `$VAR`; header names stay literal. |
| `exposeMode` | `curated`\|`lazy`\|`all` | both | _(see below)_ | How the server's tools enter the window. |
| `expose` | string[] | both | `[]` | Curated allowlist — the **original** MCP tool names to expose first-class. |
| `timeoutSeconds` | int | both | `30` | Per-call timeout for one tool invocation. |
| `disabled` | bool | both | `false` | Skip this server entirely. |

A server that declares **neither or both** of `command`/`url` is a config error: it
is **skipped non-fatally** (logged) and the run continues. `headers` is valid only
for HTTP servers; putting it on a stdio server is also skipped non-fatally as a
bad server config. Servers connect with a **10 s connect timeout**; a server that
fails to connect is likewise skipped — the run always proceeds with the builtins
(and any healthy servers).

### `~` and `$VAR` expansion

kloo launches stdio servers with `exec` (**no shell**), so a literal `~` or `$VAR`
would otherwise be passed through verbatim. HTTP auth headers also commonly need
secrets from the environment. To match expectation, the loader expands `command`,
every `args` element, every `env` **value**, and every HTTP `headers` **value**:

- a **leading** `~` or `~/` → your home directory, and
- `$VAR` / `${VAR}` → the environment value (`os.ExpandEnv`).

There is **no** globbing and **no** word-splitting — the expanded string is passed
literally. A non-leading `~` (e.g. `a~b`) is left as-is. So the example's
`"~/.mempalace"` resolves to `<home>/.mempalace`, and
`"Bearer ${MCP_TOKEN}"` resolves using your `MCP_TOKEN` environment variable.
Header names are not expanded.

### Precedence

The usual chain applies: **flags > env (`KLOO_*`) > profile > defaults**. The
profile is where servers are declared; the global on/off switch can be overridden
from the env or a flag (see [Disabling MCP](#disabling-mcp)).

## Curated vs lazy — read this if your server has many tools

**The problem (matters most for small models).** Every tool's JSON schema sent to
the model costs ~80–400 tokens. A 33-tool server is **~3k–13k tokens of schemas
every turn** — kloo targets 8–32k windows on small local models, where
tool-selection quality collapses long before the window is full. Dumping every
server's schemas would wreck exactly what kloo optimizes for. So exposure is a
first-class choice.

| Mode | What lands in the window | Cost | Use when |
|---|---|---|---|
| **`curated`** _(recommended)_ | Only the tools in `expose`, as full first-class tools | bounded by your allowlist | You know the 3–6 tools that matter. |
| **`lazy`** _(safe default for big servers)_ | A fixed **3-tool meta-trio**, regardless of server size | ~constant (~300 tokens) | Large or not-yet-curated servers. |
| **`all`** _(escape hatch)_ | Every discovered tool | unbounded (warns) | Tiny servers; debugging. |

**Default (when `exposeMode` is unset):** **`curated` if `expose` is non-empty,
else `lazy`.** Consequence: a freshly-added 33-tool server **never** dumps 33
schemas by accident — it starts lazy, and you promote the few tools you want into
`expose` (which flips it to curated). This is the small-model-safe default.

### The lazy meta-trio

A lazy server registers exactly three small tools (namespaced per server) instead
of its full tool list:

- `<server>__list_tools` — the server's tool **names + one-line summaries**
  (capped, with a cursor for the rest). Cheap; no full schemas.
- `<server>__describe_tool` `{name}` — the full JSON schema for **one** tool, on
  demand.
- `<server>__call_tool` `{name, arguments}` — invoke any of the server's tools by
  name (arguments as a JSON object, or a JSON-string of one).

The model's flow is **list → (describe) → call**, so the window only ever holds
three small schemas per lazy server regardless of how many tools it has.

### Global cap — `mcp.maxExposedTools`

`maxExposedTools` (default **16**) caps the **total** number of first-class MCP
tools across **all** servers. It lives under a reserved top-level **`mcp`** object:

```jsonc
{ "mcpServers": { /* … */ }, "mcp": { "maxExposedTools": 16 } }
```

> It is nested under `mcp` (not a bare top-level number) on purpose: kloo decodes
> the whole profile keyed by model name, and a top-level numeric key would break
> that. Use `"mcp": { "maxExposedTools": N }`.

If curated/`all` selections would exceed the cap, kloo registers tools in
declaration order up to the cap, then **demotes the rest of that server to lazy**
and **logs exactly what was demoted** — never a silent truncation. Builtins are
never counted against the cap.

## Tool names

A server's tool is exposed to the model as **`<server>__<tool>`** (double
underscore), sanitized to the function-name charset `^[a-zA-Z0-9_-]{1,64}$` — so
`.`, spaces, and other illegal characters become `_`, and over-long names are
truncated (with a short deterministic suffix to avoid collisions). Examples:
`mempalace__recall`, `docs__list_tools`. The namespacing prevents collisions with
builtins (e.g. a server's own `read_file`) and across servers.

In the TUI these render through the same generic tool card as any builtin — the
only visible difference is the namespaced name.

## Security & trust

MCP meaningfully widens kloo's trust boundary. Read this before adding a server.

1. **MCP tools run OUTSIDE kloo's workspace sandbox.** kloo's builtins are jailed
   to your workspace directory; **MCP tools are not.** A tool runs inside its
   server's own process, which can read and write anywhere that process can — a
   filesystem or shell MCP server effectively has the access its process has, not
   your workspace jail. Only connect servers you would trust with that access.

2. **Your profile file is the trust root.** kloo launches the exact
   `command` / `args` / `env` you put in `profiles.json`. Write access to that file
   is therefore equivalent to **arbitrary code execution** on your machine. Protect
   `profiles.json` like a credential (don't make it world-writable; be wary of
   committing it or syncing it to untrusted locations).

3. **Only add servers you trust**, from sources you trust, the same way you'd vet
   any program you run.

4. **kloo tells you what connected.** At startup, kloo prints one line per server
   to stderr — name, transport, exposed-tool count — plus a one-time note that MCP
   tools run outside the sandbox:

   ```
   kloo: mcp · connected "mempalace" (stdio) — 3 tools
   kloo: mcp · server "mempalace" exposed 3 tool(s) curated: mempalace__recall, mempalace__remember, mempalace__list_rooms
   kloo: mcp · skipped "broken" — connect failed: exec: "broken-mcp": executable file not found in $PATH (run continues)
   kloo: mcp · NOTE: MCP tools run inside their server process, OUTSIDE kloo's workspace sandbox.
   ```

5. **HTTP static header auth is supported; OAuth is not.** For HTTP MCP servers,
   set a `headers` object when the server expects a bearer token, API key, or
   other static header:

   ```jsonc
   {
     "mcpServers": {
       "docs": {
         "url": "https://mcp.example.com/mcp",
         "headers": { "Authorization": "Bearer ${MCP_TOKEN}" },
         "exposeMode": "lazy"
       }
     }
   }
   ```

   Prefer environment expansion (`${MCP_TOKEN}`) over inline secrets so tokens do
   not end up committed in profile files. kloo never logs header values. Full
   OAuth 2.1 flows — dynamic client registration, PKCE, token refresh, and SDK
   OAuth handler wiring — remain out of scope for this phase.

### Disabling MCP

To turn MCP off entirely for a run (overrides env and the profile):

```sh
kloo --no-mcp "fix the bug"
# or
KLOO_MCP=0 kloo "fix the bug"      # "0" or "false" disables; precedence: --no-mcp > KLOO_MCP > profile
```

With MCP disabled — or with no `mcpServers` configured, or all servers
`"disabled": true` — kloo registers **zero** MCP tools and prints **no** `mcp ·`
lines: output is identical to pre-MCP kloo. Per-server, set `"disabled": true` to
skip just that one.

## Example — mempalace, curated

mempalace exposes ~33 memory tools over stdio — far too many to dump into a small
model's window. Curate the handful you want:

```jsonc
{
  "mcpServers": {
    "mempalace": {
      "command": "mempalace-mcp",
      "args": ["--db", "~/.mempalace"],
      "exposeMode": "curated",
      "expose": ["recall", "remember"]
    }
  }
}
```

The model now sees two first-class tools, `mempalace__recall` and
`mempalace__remember`, alongside the builtins — and can call them like any other
tool. To browse the rest without curating, set `"exposeMode": "lazy"` and let the
model use `mempalace__list_tools` → `mempalace__describe_tool` → `mempalace__call_tool`.

## Troubleshooting

- **A server won't connect.** Non-fatal: kloo logs `kloo: mcp · skipped "<name>" —
  connect failed: … (run continues)` and starts without it. Check the message — a
  missing stdio binary shows `executable file not found in $PATH`; fix the
  `command`/`PATH`, or the `url`. The run still works with your builtins.
- **Too many tools / the model gets confused.** The server is probably in `all`
  mode or has a large `expose`. Switch it to `curated` with a short `expose` list,
  or `lazy`. If you legitimately need more first-class tools, raise
  `mcp.maxExposedTools` — but more schemas in-window lowers small-model reliability.
- **A tool hangs.** Each call is bounded by `timeoutSeconds` (default 30 s); a
  slow/hung tool returns a timeout error the model can recover from. Lower it for a
  flaky server.
- **HTTP vs stdio.** Use `command` for a local program kloo should launch; use
  `url` for an HTTP MCP endpoint. Add HTTP-only `headers` when the server expects
  static bearer/API-key auth. If a remote server returns unauthorized, check that
  the referenced env var is set and that the header name/value matches the
  server's requirements.
- **`headers` on a stdio server.** Non-fatal: kloo treats this as a bad server
  config, skips that server, logs the skip reason, and continues the run.
- **An MCP tool errored.** Tool errors (including the server's own `isError`
  results) surface to the model as a normal tool error, so it can self-correct —
  the same channel as a failing builtin.

## Limits in v1 (watch-items)

- **Static HTTP headers supported; OAuth out of scope** — bearer/API-key headers
  can be configured per HTTP server, but full OAuth 2.1 token lifecycle support is
  a future task.
- **No `tools/list_changed`** — a server's tool list is snapshotted at connect; a
  server that adds/removes tools mid-session isn't reflected until you restart kloo.
- **No interactive per-call approval** — MCP calls aren't gated behind the TUI
  permission dial in v1; the trust decision is made when you configure the server.

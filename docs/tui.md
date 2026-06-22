# kloo TUI — Claude-Code-style experience

This document describes the interactive TUI as delivered by the *tui-claude*
plan: a correct **live token counter**, a centralized **colour theme** with real
visual hierarchy, **progress reframed** as continuous activity, and airy polish.

The autonomous loop itself (its state machine, churn/effort tiers, budgets, and
tool adapters) is unchanged — the TUI only **reads** those signals and renders
them. Launch the TUI by running `kloo` with no task argument.

## At a glance

```
┌ kloo v0.2.0  qwen2.5-coder · medium     step 18/500 · 14.4k tok · auto ┐
└────────────────────────────────────────────────────────────────────────┘

  … transcript (scroll: wheel / PgUp · PgDn) — assistant prose, tool cards, diffs …

  ⠹ Moonwalking…  editing src/app/app.ts · 12s · 14.4k tok · esc to interrupt
```

The header leads with the build version (`kloo v0.2.0`), so you can always see
which kloo you're running. A plain `go build` / `make binary` shows `kloo dev`;
release binaries are stamped with their semver by goreleaser. (`kloo --version`
prints the full `version (commit, date)` line.)

## 1. Live token counter

Previously the token total was always `0` on the streaming path (the path the
TUI always uses). Two defects caused it; both are fixed:

- **The request now opts into usage.** `llm.ChatRequest` carries an optional
  `StreamOptions{IncludeUsage bool}` field, serialized as
  `stream_options.include_usage`. `Client.Stream` sets it to `true` (unless the
  caller already set its own options); `Complete` never sets it, so
  non-streaming requests serialize byte-identically to before.
  Under the OpenAI/llama.cpp streaming protocol a server only emits the final
  `usage` chunk when the client opts in this way.
- **The response now keeps usage.** The SSE reader parses the final
  `usage`-bearing chunk (which carries `choices: []` and a populated `usage`)
  and accumulates it into the returned `ChatResponse.Usage`.

### Estimate fallback

Some OpenAI-compatible endpoints ignore `include_usage` and still report no
usage. To keep the counter from freezing at zero, `internal/agent` substitutes a
**client-side estimate** *only when a turn reports `TotalTokens == 0`*:

- Real server usage is authoritative and is always used when present.
- The estimate is computed at the loop's result seam from the turn's prompt +
  completion text (including tool-call names/arguments) via the project's
  existing `repomap.ApproxTokens` heuristic.
- This computes a *number* only — the loop's decision logic (`Budget.AddTokens`,
  step/budget gating) is untouched; the fix only makes the number truthful.

The result is a non-zero, monotonically non-decreasing total that surfaces in
both the header and the thinking line.

## 2. Colour theme & degrade

All TUI colour lives in a single file, `internal/tui/theme.go` — the one place a
`lipgloss.Color` literal is written (a test guards that no other file in the
package constructs one). Callers reference **semantic styles**, not raw colours:

| Style | Colour | Used for |
|---|---|---|
| `accent` | pink/magenta (212) | brand accent, thinking verb, headings |
| `success` | green (2) | added diff lines, pass markers, inline code |
| `danger` | red (1) | removed diff lines, fail markers, error border |
| `warning` | amber (3) | attention/report border |
| `muted` | dim grey (244) | secondary text (paths, commands, metadata) |

Each tool also has a card accent + glyph (e.g. `run_command` → `⌘`,
`edit_file` → `✎`, `read_file` → `👁`, verify → `✓`); unknown tools fall back to
a muted bullet.

### `NO_COLOR` / non-TTY degrade

Colour resolves through the active `termenv` profile, so degrade is automatic.
At startup `kloo` forces the **ascii** profile (no ANSI escapes) when either:

- the `NO_COLOR` environment variable is present (any value, per
  [no-color.org](https://no-color.org)), or
- stdout is not a TTY (piped or redirected).

Layout and glyphs stay intact; only colour is dropped. Markdown markers strip to
plain text in this mode, which also keeps the golden-frame tests deterministic.

## 3. Transcript hierarchy

The transcript renders distinct, colour-coded surfaces instead of flat text.

### Tool cards
Each tool call becomes a bordered card with a chip header — the tool's glyph +
name in its accent colour — followed by dim secondary text (the path, command,
or one-line summary).

### Diff cards (`edit_file`)
A `✎ <path>` header in the edit accent, then the SEARCH/REPLACE blocks rendered
as a diff: red `- ` removed lines, green `+ ` added lines, with a dim rule
between consecutive hunks. An empty SEARCH is the new-file form (all `+`). The
raw fence/markers are never shown — only the clean diff.

### Command output (`run_command`)
A `⌘ run_command` chip, the dim command, and a coloured result marker:
green `exit 0 ✓` on success, red `exit N ✗` on failure (with a red border tint).
On failure a few dim stderr lines follow, **truncated** after 4 lines with a
`… +K more lines  ctrl+o to expand` hint. Press **`Ctrl-O`** to toggle the full
output.

### Scrolling the transcript
The transcript is a scrollable viewport: **mouse wheel** or **`PgUp`/`PgDn`** move
through history. Auto-scroll is **sticky-bottom** — new output follows the tail
only while you're already at the bottom; scroll up to read earlier turns and kloo
won't yank you back down.

### Copying text
**`Ctrl-Y`** copies the **last assistant reply** to the system clipboard via OSC 52
(no external tool, works over SSH; needs a terminal that honours OSC 52 — kitty,
iTerm2, WezTerm, Alacritty, recent VTE, tmux with `set-clipboard on`). For arbitrary
selections, or where OSC 52 isn't supported, hold **`Shift`** while click-dragging:
that bypasses kloo's mouse capture (which the wheel-scroll needs) and uses the
terminal's native selection + copy.

### Assistant prose (light markdown)
Assistant text is run through a hand-rolled *light* markdown styler (not
glamour/CommonMark): `#`/`##`/`###` headers, `-`/`*` bullets, inline `**bold**`,
and inline `` `code` ``. Wrapping happens on plain text before styling so ANSI
escapes are never split mid-sequence. An unterminated inline marker (common
mid-stream) degrades to literal text rather than producing a dangling style.

## 4. Progress reframe

The header **leads** with the durable context — `kloo · model · effort` and the
live token total — and **demotes** `step N/max` to a dim secondary field. The
mechanical step index is no longer the primary signal.

The animated **thinking line** is the primary in-flight signal:

```
⠹ Moonwalking…  editing src/app/app.ts · 12s · 14.4k tok · esc to interrupt
```

- a rotating whimsical verb (changes every ~3s) + braille spinner,
- a compact **activity** phrase derived from the current tool
  (e.g. *editing `<file>`*, *running `<cmd>`*, *reading `<path>`*) — this is
  display-only, sourced TUI-side, with no loop signal or `internal/agent` change,
- elapsed seconds, the live token total, and the interrupt hint.

The line self-terminates when a run ends.

## Verifying

- Automated (offline, deterministic): `go test ./...` covers the SSE usage
  parsing and request-body opt-in, the agent estimate fallback, and TUI golden
  frames (`Model.View()` under the ascii profile) plus `teatest` for the
  `Ctrl-O` expand toggle and `NO_COLOR` degrade.
- Live smoke: run `kloo "<small task>"` against a local OpenAI-compatible server
  (e.g. llama.cpp) and watch the header/thinking token count start non-zero and
  increase over the run.

# Sessions

kloo remembers your conversation **across runs and across restarts**, per
workspace. Ask a follow-up — *"what's the issue?"*, *"now do the other file"*,
*"why did you do that?"* — and it resumes with the prior context instead of
starting blind. This builds on the working-memory layer ([docs/memory.md](memory.md)):
the session is just the durable conversation that working memory fits under the
window each turn.

## Where sessions live

```
{workspace}/.kloo/
  .gitignore          ← generated: "*"  (so transcripts are never committed)
  sessions/
    20260622-143012.json   ← one file per session: messages + metadata
```

- **Workspace-scoped:** each project gets its own sessions, under its own
  `.kloo/`. The dir is **self-ignored from git** (kloo writes `.kloo/.gitignore`
  = `*` on first use), so conversation transcripts — which can hold sensitive
  context — never land in your repo or a commit.
- **Global config** lives under `~/.kloo/` (e.g. `~/.kloo/profiles.json`). The
  older `~/.config/kloo/` path still works as a fallback.

A session file holds the message history plus `id`, `title` (from the first
task), `model`, `verify`, run count, and timestamps.

## Launch behavior

Running plain `kloo` in a workspace:

| Saved sessions | What happens |
|---|---|
| none | starts a fresh session |
| exactly one | **auto-resumes** it (banner: *resumed session · ‹title› · N run(s) · last active …*) |
| several | **prompts** you to pick one or start new |

Flags override the policy:

| Flag | Effect |
|---|---|
| `--new` | always start a fresh session |
| `--resume <id>` | resume a specific session (ids are the filenames under `.kloo/sessions/`) |

A new session isn't written until its first run completes, so launching and
quitting without doing anything leaves no clutter.

## What resuming gives the model

On resume, kloo seeds the loop with the session's transcript plus a compact
**outcome note** for each prior run (stop reason + error + failing-verify output).
Working memory then folds older turns into a running summary while pinning the
current task and re-reading the file under edit fresh — so even a small-context
model stays anchored. See [docs/memory.md](memory.md) for how that packing works.

## Interaction with git checkpointing

A non-success stop in a **git** workspace rolls the working tree back to the
pre-run checkpoint. The *conversation* still persists (so you can ask "what
happened?"), but the *file changes* from that run are rolled back — resume
continues the dialogue, not a half-applied edit. In a non-git workspace there's
no checkpoint, so partial edits remain on disk.

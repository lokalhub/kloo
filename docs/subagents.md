# Subagents

kloo can hand a subtask to a **child agent**: a fresh loop with its own context,
budget and churn state, working in the same repository. The child returns only a
summary, so its reads never enter the parent's window.

**Everything here is off by default.** With no flags set the tool vocabulary, the
system prompt and the loop behaviour are exactly as they were before.

## Two ways in

**Model-initiated** (`KLOO_SUBAGENTS=1`) offers a `task` tool the model may call
when it decides a subtask is separable.

**Harness-initiated** (`KLOO_AUTO_DELEGATE=1`) hands off *without* offering the tool
or changing the prompt: when the model has taken N read-only turns without editing,
the loop delegates on its behalf. This is the mode that was measured.

```bash
export KLOO_AUTO_DELEGATE=1
export KLOO_DELEGATE_AFTER_READS=18
export KLOO_SUBAGENT_STEPS=25
export KLOO_SUBAGENT_MODEL=qwen3.8-27b-nvfp4   # optional: route the child elsewhere
```

## What the child does and does not share

Shares the parent's client, adapter, tool registry and workspace — it edits the same
repo. Delegation is about context isolation, not sandboxing.

Does **not** share:

* the conversation (the point: the child's reads stay out of the parent's window)
* the verifier — the parent owns the success gate, so a child cannot declare the
  task done or make a run succeed by weakening a test
* the budget — its own ceiling, so one child cannot consume the whole run
* working memory and churn state

A child may never delegate again. Two independent guards enforce this: the depth
limit gates tool registration, and `AutoDelegate` is cleared on the child.

## Model routing

`KLOO_SUBAGENT_MODEL` runs delegated work on a different model than the parent. The
CLI owns credentials, so it builds the routed child's client; without that the child
would hit the endpoint unauthenticated.

This is the configuration that produced the measured gain, and it is worth being
precise about why — see below.

## What the gain actually is

Auto-delegation measurably helps a small parent — **but it is a hybrid, and that
matters more than the headline.** A glimmer parent delegating to a *qwen* child
means the capable model is doing the work the small one could not. With a
*glimmer* child the gain is much smaller, because delegating glimmer→glimmer
reproduces the same behaviour: parent and child both read without editing.

**The win came from model substitution, not decomposition.** Do not present it as
a like-for-like result for the small model alone.

## Counters

Emitted in `KLOO_RESULT_JSON` so an inert configuration is distinguishable from a
failing one:

| counter | meaning |
|---|---|
| `auto_delegations` | harness-initiated handoffs that ran |
| `subagent_steps` | total steps consumed by children |
| `rescue_delegations` | handoffs fired at the explore rail |
| `restarts` / `restart_rescues` | whole-run restarts, and how many succeeded |

A failed child also logs its **error**, not just `reason=error` — the cause used to
be discarded, which made a dead child indistinguishable from an idle one.

## Tuning notes

* **`KLOO_DELEGATE_AFTER_READS=18`** was measured. At the legacy trigger (6) it
  fired on the large majority of runs glimmer already solves, costing both cases
  and time (one case went 1593s→263s when moved to 18, and timeouts became rare).
* **`KLOO_SUBAGENT_STEPS=25`**: the default (half a 60-step parent) let delegated
  cases run to 2381s against a 2400s ceiling — a pass 19 seconds from the wire is a
  coin flip.
* **`KLOO_MAX_HANDOFFS`** allows bounded *sequential* handoffs. Each child is fresh
  and none are nested. Default 1.

## See also

* [configuration.md](configuration.md) — all `KLOO_*` variables
* [benchmarking.md](benchmarking.md) — how these numbers are produced
* [beat-grok plan](../../../docs/apps/kloo/plans/beat-grok/README.md) — the open
  work, the closed negatives, and the measurement rules that make results
  trustworthy

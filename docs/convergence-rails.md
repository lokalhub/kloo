# Convergence rails

Seven behaviours that stop kloo defeating itself on a task it can otherwise do.
**They are ON by default** as of v0.22.0. Any one can be disabled with
`KLOO_<NAME>=0`.

Measured on [kloo-bench](https://github.com/lokalhub/kloo-bench) against
`muse-glimmer-30b`, paired against grok 1.0.34 on the same model and endpoint:

| | score |
|---|---|
| kloo before | 6–10 / 21 |
| kloo with these seven | **16 / 21** |
| grok, same model, same sweep | 17 / 21 |

One case separates them, and that case (A29) is one kloo passes 4 times in 5 — a
coin flip, inside the bench's ±3.5-case noise floor. Treat the two as tied.

> **v0.23.0 update — some rail firings were a resource problem, not a reasoning
> one.** The repo map was re-sent and re-curated every turn, so kloo re-prefilled
> ~22,000 tokens on every model call. Runs were hitting `explore-stop`, `churn`
> and `budget-exceeded` because the budget went into re-processing a map, not
> because the model needed correcting. Pinning and freezing the map
> ([map position](configuration.md#map-position---map-position)) removed that
> cost and cut run time by roughly 40%.
>
> The rails below still earn their place — turning them off was measured far
> worse. But when one fires repeatedly, check what the turn is *spending* before
> assuming the model is stuck.

## The rails

### `KLOO_EDIT_RAIL=1` — the explore nudge demands an edit
When a run has read for several turns without changing anything, the corrective
asks for an edit rather than offering "or run a command, or call finish". It fires
on the read-only streak, so it applies equally before the first edit and after the
tenth. It also names the assertions still failing.

*Why:* a glimmer run read 26 files in a row, made zero edits, and was stopped by a
rail after 30 steps.

### `KLOO_FORCE_EDIT=1` — the rail refuses, it does not merely ask
From the second no-edit nudge of a run, kloo advertises only the tree-changing
tools AND refuses any other call for up to three turns, releasing the moment an
edit actually applies.

*Why:* narrowing the advertised tool list alone changed nothing — the model kept
calling `read_file` from the vocabulary it had already seen, and the loop ran it.

### `KLOO_PROTECT_VERIFY_PATHS=1` — never edit the file you are graded against
Refuses edits to any file the verify command names, and points at the source files
that test imports instead.

*Why:* told it must edit something, a stuck model reaches for the file it read most
recently — the failing test. One run edited the test three times, every edit a
no-op, and never touched a source file. This is the shape of an agent passing by
moving the goalposts.

### `KLOO_VERIFY_AUTHORITY=1` — a partial test run is not a pass
When the model runs a test command covering fewer files than the verify command and
it passes, kloo names the files it skipped.

*Why:* a run executed one of two test files four times, saw green each time, never
ran the other, and stopped as "answered" with the case red.

### `KLOO_EMPTY_TURN_RECOVERY=1` — a blank response is not fatal
An exhausted empty completion injects an observation and continues, bounded at
three recoveries, instead of ending the run.

*Why:* one blank response from a local endpoint discarded 14 good steps and a
correct edit with 45 steps of budget left.

### `KLOO_KEEP_WORK_ON_FAIL=1` — do not delete work you did correctly
Narrows the rollback to outcomes where the tree is untrustworthy (internal error,
interrupt, safety stop). A run that merely ran out of road keeps what it wrote.

*Why:* kloo rolls back on ANY non-success exit. On a case whose verify command also
named an unfixable Playwright spec, verify could never go green, so kloo **erased a
correct fix** and the grader measured unfixed code. grok, which has no rollback,
kept the same fix and passed.

**This is the one with real blast radius** — see Defaults.

### `KLOO_BUILD_BREAK_GUARD=1` — tell a broken build from a failing test
A verify output showing a compile/transform failure is reported as damage the agent
just did ("YOUR LAST EDIT BROKE THE BUILD"), with the errors quoted and the
force-edit rail armed at once. It also rolls back when the final tree does not
compile, even under `KLOO_KEEP_WORK_ON_FAIL`.

*Why:* an edit introduced duplicate declarations, the file stopped transforming, the
test runner collected ZERO tests, and kloo read sixteen more times without
repairing it.

## Defaults and how to turn one off

All seven are ON from v0.22.0. To restore the previous behaviour for any of them:

```sh
KLOO_KEEP_WORK_ON_FAIL=0 kloo "..."     # roll back on ANY non-success exit
```

### The one with real blast radius

`KLOO_KEEP_WORK_ON_FAIL` changes what kloo leaves on disk after a run that did not
succeed. Before v0.22.0 kloo restored the tree on every non-success exit; now it
restores only when the tree cannot be trusted — an internal error, an interrupt, a
safety stop, or a final state that does not compile. A run that hit its step budget
or churned **leaves its edits in place**.

That is the better default for a developer (you want the diff, not an empty tree,
and your work is in git anyway), and it is worth a large amount on the benchmark —
kloo was erasing fixes that were already correct. But it IS a behaviour change, and
`=0` restores the old contract exactly.

### Two corrections made when the defaults were flipped

Turning the flags on ran them against kloo's whole suite for the first time and
found two real problems:

1. **`KLOO_PROTECT_VERIFY_PATHS` blocked deliverables.** A verify command of
   `grep -qx right answer.txt` names `answer.txt` — the file the task must create —
   and the guard refused every write to it, churning the run. It now only protects
   paths that actually look like tests (`.test.`, `.spec.`, `_test.go`, `tests/`,
   `__tests__/`, …). Config files named by a verify command are no longer protected
   either; they are not specs.
2. **`KLOO_FORCE_EDIT` punished legitimate exploration.** It refused read-only calls
   after a nudge regardless of whether they covered new ground, which is exactly the
   over-eager behaviour kloo removed in v0.17.1 (`TestExploreRailAllowsManyDistinctReads`
   pins the lesson). It now lets a read of something not yet seen through, and
   refuses only RE-reads — which is what every trace behind the rail actually shows.

Neither was visible while the flags were opt-in. Flipping the defaults was worth it
for that alone.

## Measured and REJECTED

Kept in the tree, flag-gated, off, so the negative is not re-discovered:

| flag | verdict |
|---|---|
| `KLOO_TIGHT_SEARCH` | bounded search output. Paired on 4 cases: identical outcomes, **slower in 4 of 4**, 2x on two. |
| `KLOO_BOUND_CMD_OUTPUT` | bounded command output, same experiment, same verdict. |
| `KLOO_SAME_FAILURE_REDIRECT` | on a repeated identical failure, name the other files the test imports. Helped one case (0/2 → 4/5), inert on the case it was designed from. |
| `KLOO_GROK_PROMPT`, `KLOO_SIMPLE_EDIT`, `KLOO_LINE_ANCHORS` | grok-alignment changes — action-first prompt, grok's three-field `search_replace`, `LINE_NUMBER→` anchors. **Never showed a benefit.** The original hypothesis was that kloo needed grok's agent design; it did not. |
| `KLOO_PIN_WINDOW` | window the whole-file pin. Built, never needed by a failing case. |

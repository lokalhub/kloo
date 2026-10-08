# Open defects in prompt accounting and the map budget

Measured 2026-10-06, during the work that added the verify baseline
(`internal/agent/baseline.go`). None of these are fixed. They are recorded here
because each one needs its own measured change, and because the evidence cost
real GPU time to produce.

The incident that surfaced them: a scoped run on an Ionic app
(`--allow 'frontend/src/app/pages/game/**'`, `--working-set-tokens 12000`,
ctx 131072) read twelve distinct files before its first edit.

## 1. mapBudgetTokens ignores the working-set cap — FIXED

`mapBudgetTokens` derived from the curator budget and reconstructed the compaction
trigger as `curator × triggerFrac`. It did not move when the user lowered
`--working-set-tokens`, so the cap the user set did not bound the single largest
section of the prompt.

It is now the SMALLER of `mapBudgetFrac × triggerTokens` — the real trigger, with
the working-set cap in it — and the appetite figure `curator × triggerFrac ×
mapBudgetFrac` that shipped in v0.27.0. The min is what keeps the fix MONOTONE: at
every window and every curator value the budget either stays byte-identical or goes
down, never up. Taking the trigger arm alone is the coherent formula but it is +43%
wherever the curator budget is the smaller input — including the stock CLI default
and the `muse-glimmer-30b` profile — and a correctness fix must not pay for itself
in tokens that nothing measured says buy anything.

Measured on kloo's own tree with `kloo context --json "fix the bug"`, 2026-10-07
(`map` is the budget, `hot` is `hotBudgetTokens`, both against the real trigger):

    ctx=131072 curator=32768 (stock)  before: trigger=58617 map=  6881  after: map=  6881  byte-identical
    ctx=131072 curator=52428 (glimmer) before: trigger=58617 map= 11009  after: map= 11009  byte-identical
    ctx=131072 curator=104857          before: trigger=58617 map= 22019  after: map= 17585  FIXED
    ctx=131072 --working-set-tokens 12000
                                       before: trigger=12000 map= 22019  after: map=  3600  FIXED
    ctx=1048576 curator=838860         before: trigger=165794 map=176160 after: map= 49738  FIXED
    ctx=8000                           before: trigger= 4480 map=  1344  after: map=  1344  byte-identical
    ctx=32768                          before: trigger=18349 map=  5504  after: map=  5504  byte-identical

Every pathology stays fixed and nothing grows. Monotonicity is swept over 1,496
(window, curator, working-set) combinations in
`TestMapBudgetNeverExceedsTheAppetiteFormula`; the largest reduction at the built-in
working-set curve is 281,981 tokens, at a 2M declared window with no curator cap,
where the appetite arm authorised 352,321 tokens of repo map.

What the trigger arm would have cost, measured end to end at the stock default with
`--ctx 131072 --curator-budget 32768`: the assembled first turn goes from 34,526 to
43,115 chars, +8,589 bytes of repo map (+24.9%) on every turn, for +2,959 tokens.

`map + hot` is at most `mapBudgetFrac + hotBudgetFrac` = 75% of the trigger, which is
a CEILING and not the sum. Measured: 75.0% where the working-set cap binds with the
curator cap lifted, 66.0% at ctx 8000 and ctx 32768 where the cap is a no-op — 66
rather than 75 because `hotBudgetTokens` is handed the already-usable window and
applies `usableWindow` a second time (defect 6 below), so hot lands at 36% of the
trigger there instead of 45%. Before this change the ceiling was breached: at
ctx 131072 with the curator cap lifted, map was 37.6% and hot 45%, summing to 82.6%.

The trigger and hot figures above are the ones the code computes, which are NOT
`workingSetFor(declared)`: `hotBudgetTokens` and `triggerTokens` are both handed the
USABLE window (`loop.go` passes `usableWindow(ctx)` to `Assemble`), so at ctx 131072
the cap is `sqrt(104857 × 32768)` = 58,617 and not `sqrt(131072 × 32768)` = 65,536.
An earlier version of this table derived them from the declared window and so
reported trigger=65536 / hot=29491.

`kloo doctor` reported the explicit flag as "BINDING — holds the prompt here", which
was the third thing wrong here: the cap holds the compaction TRIGGER, and the trigger
bounds only the history compaction can shed. At this configuration doctor claimed
12,000 while `kloo context` measured the first turn at 26,370; it is 7,864 now, under
the trigger. That line says where compaction starts and points at `kloo context` for
the prompt.

This is the second time that phrase has been wrong. The first was `hotBudgetTokens`
ignoring the cap entirely (fixed in fc608bf). Finding that one was worth more than
any tuning; this one was the same shape.

## 2. The pinned map's tokens never enter the budget arithmetic

`act()` pulls `pinnedMap` out of `sysContent` and splices it in AFTER assembly, so
`nonHistoryTokens` — and through it `MemoryInput.SystemTokens`, the floor, the soft
trigger and the hard ceiling — under-reports every pinned-map prompt by the full
map size. Tail and system placements ARE counted; only the shipped default is not.

Taken with defect 1: the largest section of the prompt is neither bounded by the
cap the user set nor counted in the arithmetic that decides when to compact. That
can push the assembled prompt past the window, which is the failure `memory.go:51`
already records (a real 33570-token prompt on a 32768 server returned a 400).

A correctness bug, not a metrics bug.

## 3. The explore rail cannot bind on distinct reads, by design

`coversNewGround` exempts every read of an unseen target, so a new-ground read sets
`exploreStreak = 0` and the `exploreStreak >= 16` abort is unreachable. `saturated()`
needs `newGround/window < 0.35`; twelve distinct reads give 8/8 = 1.0.
`DefaultExploreTokenCap = 2_000_000` is structurally unreachable at any realistic
window — its own comment concedes this.

So the twelve-read opening is a convergence question, not a token question.

DO NOT narrow exploration on this evidence. Tightening the explore cap has already
been measured in this campaign and was harmful: kloo's existing rail would have
aborted a competitor's winning C07 run, and a grok-sized budget took C07 from 4/10
to 8/10. The map is also well under its cap for the incident repository — it renders
identically at a 22k and a 100k budget — so no cap change could have reduced the
cost anyway.

## 4. editOnlyLeft is decremented by the reads it exempts

`isEditTool ? 0 : editOnlyLeft--`. Three exempted new-ground reads burn the whole
`editOnlyBudget = 3` having refused nothing: the rail arms, expires, and
`RailExplore` is recorded as "fired". Fixing it would make the rail bind, i.e.
tighten exploration, which defect 3 says not to do on this evidence. Left alone
deliberately.

## 5. The one sentence that discourages reading is behind a flag

The default system prompt supplies no downward pressure on reading. "Reading is not
progress" sits behind `KLOO_GROK_PROMPT`.

## 6. hotBudgetTokens applies usableWindow twice

`hotBudgetTokens(window)` computes
`capWorkingSet(usableWindow(window) × triggerFrac, window) × hotBudgetFrac`, but the
`window` it receives is ALREADY the usable window — `loop.go` hands `Assemble`
`usableWindow(ctx)`. So `usableWindow` is applied a second time and the hot budget is
~20% below the fraction it documents. Its own comment records this and declines to
fix it, which is the right call (it moves every small-ctx run and there is no evidence
a larger hot budget helps there), but it has a consequence worth writing down: the hot
budget is NOT on the same base as `triggerTokens`, so the claim that `mapBudgetFrac`
and `hotBudgetFrac` partition one number is only true where the working-set cap binds.

Measured against the real trigger: hot is 45.0% of it at ctx 131072 (cap binding) and
36.0% at ctx 8000 and ctx 32768 (cap a no-op). The map budget is now on
`triggerTokens` proper, so the two halves of the "partition" are derived differently.
Fixing this means raising the hot budget on every small window, which needs its own
bench baseline.

## A measurement dispute worth settling

kloo's own two instruments disagree about how much of that run was real work.
`reprefill_tokens_per_turn: 2993` with `reprefill_cached_fraction: 0.81` implies
~36k genuinely re-prefilled over twelve turns. `prompt_tokens: 386620` against
`cached_prompt_tokens: 135424` (`cache_hit_rate: 0.35`) implies ~11k uncached per
turn, i.e. ~131k. Those cannot both be right.

Until that is settled, treat any "N tokens before the first edit" headline as
cumulative and overstated: it re-counts the whole prompt every turn.

## How to fix any of these

Assert that the ASSEMBLED PROMPT SHRANK, not that a counter moved. v0.25.6
compaction folded 241 messages to 80 and the prompt GREW; the counter said it
worked.

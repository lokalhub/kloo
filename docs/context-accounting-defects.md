# Open defects in prompt accounting and the map budget

Measured 2026-10-06, during the work that added the verify baseline
(`internal/agent/baseline.go`). None of these are fixed. They are recorded here
because each one needs its own measured change, and because the evidence cost
real GPU time to produce.

The incident that surfaced them: a scoped run on an Ionic app
(`--allow 'frontend/src/app/pages/game/**'`, `--working-set-tokens 12000`,
ctx 131072) read twelve distinct files before its first edit.

## 1. mapBudgetTokens ignores the working-set cap

`mapBudgetTokens` derives from the curator budget. It does not move when the user
lowers `--working-set-tokens`, so the cap the user set does not bound the single
largest section of the prompt.

    ctx=131072 default                    trigger=65536 map=22019 hot=29491  map+hot =  79% of trigger
    ctx=131072 --working-set-tokens 12000  trigger=12000 map=22019 hot= 5400  map+hot = 228%
                                                                              map ALONE = 183%

The default is close to what the comment in `workingset.go` claims. The explicit
flag is not, and the explicit flag is what `kloo doctor` reports as
"BINDING — holds the prompt here".

This is the second time that phrase has been wrong. The first was `hotBudgetTokens`
ignoring the cap entirely (fixed in fc608bf). Finding that one was worth more than
any tuning; this one is the same shape.

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

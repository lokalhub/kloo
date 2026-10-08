package agent

import (
	"context"

	"github.com/lokalhub/kloo/internal/llm"
)

// WINDOW OCCUPANCY, MEASURED FROM THE ASSEMBLED PROMPT.
//
// kloo already had two numbers about context and neither answered "how much of my
// window is in use, and what is using it":
//
//   - `kloo tokens` printed `headroom: usable - approx(task)`. approx is the TASK
//     STRING ALONE — no repo map, no system prompt, no AGENTS.md, no history. At
//     --ctx 131072 it reported ~104,848 tokens free on a window whose FIRST turn
//     already spends thousands of tokens on the repo map before the task is read.
//     Measured on kloo's own tree at --ctx 131072: 9,869 tokens of map at the stock
//     --curator-budget, 17,660 with the curator cap removed.
//   - the run display `── step 44/500 tokens 1553679/480000` is CUMULATIVE SPEND
//     against the run budget. The whole prompt is counted every turn, so that
//     counter routinely exceeds the window by 10x. It is a bill, not a gauge.
//
// So this is the gauge. Two properties shape the whole file.
//
// (1) IT IS COMPUTED FROM THE ASSEMBLED MESSAGES, NEVER FROM THE BUDGET CONSTANTS.
//
// That is not a style preference, it is the one way this can avoid being the
// fiction `kloo doctor` already tells. doctor derives its working-set line from the
// same constants the assembler derives its behaviour from, and for hotBudgetTokens
// that derivation was WRONG for three releases: a 23-step run at
// --working-set-tokens 12000 shed nothing and compacted zero times while doctor
// printed "BINDING — holds the prompt here" (see memory.go:134). An instrument that
// shares its inputs with the thing it measures cannot catch that. This one takes
// the []llm.Message that (*Loop).buildPrompt is about to send, plus the marshalled
// tool schemas from that same request, and adds them up. mapBudgetFrac,
// hotBudgetFrac and triggerFrac appear nowhere in the arithmetic — MapBudget is
// carried only to be COMPARED against the measured map, which is the one place the
// budget and the reality are allowed to disagree out loud.
//
// (2) IT ADDS NOTHING TO THE PROMPT. Not one byte. The gauge is a sum over values
// that already exist at the end of buildPrompt; it appends no message, edits none,
// and runs AFTER markCacheBreakpoint so it cannot perturb the request. This matters
// more here than anywhere: a ~22k-token repo map re-prefilling every call was the
// worst performance bug kloo has had — pinning and freezing it took two bench cases
// from 8/16 to 16/16 and cut runtime ~40% — and on the user's lokalai a 30k prompt
// is 1.05s with an unchanged prefix against 17.37s when the head changes (~15x). A
// gauge that cost one token at the top of the prompt would cost more than it could
// ever explain.
//
// TestContextGaugeAddsNoBytesToThePrompt and
// TestContextGaugeIsMeasuredNotDerivedFromBudgets pin both properties.
//
// TWO HONEST CAVEATS, stated because an instrument that hides its own limits is the
// thing this file is replacing.
//
// (a) THE SECTION SPLIT IS PRE-TRANSPORT. The gauge measures the message list
// buildPrompt produced. llm.normalizeMessages (client.go:288) then merges adjacent
// same-role messages before the request goes out, so on the wire the task and the
// pinned repo map — both role=user and adjacent — arrive as ONE user message.
// Totals are unaffected: the merge joins with a single "\n", i.e. one character per
// merge, so the sum this file reports is the wire's sum to within a token. The SPLIT
// is therefore kloo's own structure, not a field of the HTTP body — which is the
// more useful thing to report (the body cannot tell you the map is 60% of your
// window) but it is derived, and saying so is the difference between this and
// `headroom`.
//
// (b) THE DENOMINATOR IS THE BINDING BUDGET, AND BOTH ARE PRINTED. There are two
// candidate windows and `kloo tokens` picked the wrong one: at --ctx 600000, doctor
// reports usable_prompt_tokens 480000 while compact_at_tokens is 125413, because the
// absolute working-set cap (workingset.go) is tighter than 0.70 x usable. So
// `headroom` reported ~480k free against a budget that starts shedding at 125k — a
// window 3.8x larger than the one actually enforced. Reporting occupancy against
// `usable` alone would repeat that bug with better arithmetic. So Binding names
// which of the two is in force, Free is measured against USABLE (the hard ceiling
// the request must not exceed) and FreeToCompaction against the TRIGGER (the soft
// one that fires first and is what a user feels). Both are rendered; neither is
// implied.

// Prompt-section labels. Every message placed into the final prompt is tagged with
// exactly one of these AS IT IS PLACED, which is what makes the attribution exact
// rather than inferred.
const (
	// sectSystem: the lead system message (base prompt + scope suffix + AGENTS.md +
	// any memory-recall section). When the map is folded in here (MapPositionSystem)
	// the map's own tokens are subtracted out, so the map is counted once.
	sectSystem = "system"
	// sectMap: the curated repo map, wherever it was placed.
	sectMap = "repo-map"
	// sectHot: everything REGENERATED every turn — the task, the verify pin, the
	// current-file pin, the todo list. A per-turn tax for the rest of the run.
	sectHot = "pinned-hot"
	// sectHistory: the accumulated conversation — the running-summary slot and the
	// recent tail. This is what compaction sheds.
	sectHistory = "history"
)

// promptSections is the assembled prompt kept in LABELLED parts, produced by
// buildPrompt as a by-product of building it.
//
// Kinds is parallel to Msgs: Kinds[i] is the section Msgs[i] belongs to, recorded
// at the point buildPrompt appends it. The labels are kept rather than recovered
// afterwards because they cannot honestly be recovered — MemoryStats.PinnedMessages
// counts TRAILING messages of the HISTORY, and by the time the todo list and the
// tail map have been appended the pins are no longer trailing anything. A post-hoc
// classifier would have to re-derive the layout from the budget constants, which is
// the exact failure this file exists to avoid.
//
// Msgs is the prompt as it will be sent. Every token the gauge reports is summed
// from it.
type promptSections struct {
	Msgs  []llm.Message
	Kinds []string

	// MapSection is the curated repo map as assembled this turn, header included,
	// whatever its placement; "" when there is none (KLOO_NO_MAP=1, or no root).
	// Carried separately because under MapPositionSystem it is INSIDE Msgs[0] and
	// there is otherwise no way to charge it to the map.
	MapSection   string
	MapPlacement string // "pinned" | "tail" | "system" | "none"

	Window    int // the model's declared context window (l.ContextTokens)
	Usable    int // the prompt budget the assembler was actually handed
	MapBudget int // the repo-map budget used this turn
}

// add appends a message under a section label. Every placement goes through here,
// so a message can never enter the prompt without being accounted for.
func (ps *promptSections) add(kind string, msgs ...llm.Message) {
	for _, m := range msgs {
		ps.Msgs = append(ps.Msgs, m)
		ps.Kinds = append(ps.Kinds, kind)
	}
}

// ContextGauge is window occupancy as measured from one assembled prompt.
//
// Every token field is a sum over promptSections.Msgs plus the request's tool
// schemas, so the numbers describe the request that was built, not the budget that
// authorised it. The two can disagree — RepoMapTokens against MapBudget is exactly
// where they do, and reporting both is the point.
type ContextGauge struct {
	// Window is the DECLARED context window; Usable is the slice of it kloo
	// assembles against (usableWindowFrac of Window, reserving the completion, the
	// tool schemas and estimation slack); Trigger is where compaction begins.
	// Occupancy is reported against Usable because that is the budget the prompt was
	// built to — reporting it against Window would understate it by ~25%.
	Window  int `json:"window"`
	Usable  int `json:"usable"`
	Trigger int `json:"compact_trigger"`
	// Binding names which budget is actually in force at this window: "working-set"
	// when the absolute cap (workingset.go) is tighter than the fraction,
	// "trigger-fraction" when the fraction is, "none" when compaction is off. It is
	// here because the two differ by 3.8x at --ctx 600000 and nothing previously said
	// which one a reader was looking at.
	Binding string `json:"binding"`

	SystemTokens int `json:"system_tokens"`
	// SchemaTokens is the tool/function schemas attached to the request. They are
	// NOT messages — no sum over the message list can find them — but the provider
	// counts them in prompt_tokens, which is why (*Loop).act has always included
	// them in lastPromptChars. Counting message text alone made the measured
	// chars/token ratio collapse on short conversations, where the schemas dominate.
	SchemaTokens int `json:"schema_tokens"`

	// RepoMapTokens is what the map actually costs this turn; MapBudget is what it
	// was authorised to cost. MapOverBudgetPct is how far over, 0 when within.
	RepoMapTokens int `json:"repo_map_tokens"`
	MapBudget     int `json:"map_budget"`
	// MapOverBudget is RepoMapTokens - MapBudget when positive, and
	// MapOverBudgetPct the same as a percentage — a FLOAT, because the real overshoot
	// is a fraction of a percent and an int truncated it to 0, which is how a
	// measured number came to be computed and then hidden.
	//
	// A small PROPORTIONAL overshoot is two estimators disagreeing, not an unenforced
	// cap. Measured across a 3.2x change in the curator budget: 9,830 budget / 9,869
	// actual (+0.40%) and 17,585 / 17,660 (+0.43%). An unenforced cap would overshoot
	// by whatever the next file happened to be, not by a constant fraction.
	// repomap.Assemble stays within budget by summing PER-ENTRY estimates; this gauge
	// re-estimates the concatenated whole, and tokens.Estimate is not additive over
	// concatenation. repoMapSection also prepends a 37-char header the budget was
	// never charged for. So it is reported, and reported as small, rather than either
	// hidden or alarming.
	MapOverBudget    int     `json:"map_over_budget"`
	MapOverBudgetPct float64 `json:"map_over_budget_pct"`
	MapPlacement     string  `json:"map_placement"`

	// PinnedHotTokens is everything regenerated every turn; HistoryTokens is the
	// accumulated conversation. The split is the one memory.go works in and it is
	// the actionable one: hot state is a per-turn tax configuration can shrink,
	// history is what compaction sheds.
	PinnedHotTokens int `json:"pinned_hot_tokens"`
	HistoryTokens   int `json:"history_tokens"`

	// Used is the measured total: every message plus the schemas, summed
	// INDEPENDENTLY of the per-section walk so the two can be compared. Free is
	// Usable - Used. Unattributed is Used minus the sections and exists to be
	// nonzero when this file is wrong: it absorbs the rounding of a per-section
	// estimate against a whole-prompt one, and anything larger means the layout
	// moved and the attribution did not. Reported, never hidden.
	Used int `json:"used"`
	Free int `json:"free"`
	// FreeToCompaction is Trigger - Used: room left before the compactor starts
	// shedding. This is the number a user feels, and it is the one `headroom` got
	// wrong by 3.8x at a large --ctx. Negative means the next turn compacts.
	FreeToCompaction int `json:"free_to_compaction"`
	Unattributed     int `json:"unattributed"`

	Messages int `json:"messages"`

	// Ratio is the BASE chars-per-token the estimator starts from, and Calibrated
	// whether it was measured against reported usage on a previous run in this
	// workspace or is the cold-start assumption.
	//
	// Ratio alone is not what this prompt was actually charged at, and saying it was
	// would be this feature's own failure mode. tokens.EstimateAt charges
	// high-entropy words at ~1.8 chars/token (estimate.go: Go source measured at
	// 3.75, go.sum hashes at 1.78), and a repo map is almost nothing but long paths
	// and identifiers — so the effective rate runs below the base. Measured on kloo's
	// own tree: 8,659 chars of system prompt at 3.82, 85,940 chars of repo map at
	// 3.88, against a base of 4.00.
	//
	// So Chars is reported and EffectiveRatio is Chars/Used — derived from this
	// prompt rather than from a constant, which makes it the one ratio figure that
	// cannot be wrong about this prompt.
	Ratio          float64 `json:"base_chars_per_token"`
	EffectiveRatio float64 `json:"effective_chars_per_token"`
	Chars          int     `json:"chars"`
	Calibrated     bool    `json:"calibrated"`

	// ── NOT WINDOW OCCUPANCY ──────────────────────────────────────────────────
	// RSSBytes is the process's resident set and CeilingBytes the memory guard in
	// force (memguard.go). They sit here, labelled apart, rather than folded into a
	// section, because they are the one cost this gauge structurally CANNOT see:
	// what kloo retains in its own address space — captured command output, the two
	// session transcripts, the repo-map content map — is not in the prompt, and no
	// sum over Msgs will ever find it. Measured: assembling the map for a
	// 36,961-file workspace peaks at 1.55 GB of RSS to emit 22,137 tokens of map.
	// Folding that into "used" would make the gauge lie in a new way; omitting it
	// entirely would make the gauge look like the whole story.
	RSSBytes     uint64 `json:"rss_bytes"`
	CeilingBytes int64  `json:"memory_ceiling_bytes"`
}

// gauge measures the assembled prompt. schemaTokens is the request's marshalled
// tool definitions sized with the same estimator as the messages, so both halves of
// Used are on one scale — mixing a calibrated count with an uncalibrated one is the
// bug MemoryInput.Estimate exists to prevent.
func (ps promptSections) gauge(schemaTokens, schemaChars int, est func(string) int) ContextGauge {
	g := ContextGauge{
		Window:       ps.Window,
		Usable:       ps.Usable,
		Trigger:      CompactTriggerTokens(ps.Usable),
		SchemaTokens: schemaTokens,
		MapBudget:    ps.MapBudget,
		MapPlacement: ps.MapPlacement,
		Messages:     len(ps.Msgs),
	}
	if g.MapPlacement == "" {
		g.MapPlacement = "none"
	}
	// The map, sized from the section text itself. Whatever the placement, this is
	// the same string: its own pinned message, its own trailing message, or
	// concatenated onto the system prompt.
	if ps.MapSection != "" {
		g.RepoMapTokens = est(ps.MapSection)
	}

	for i, m := range ps.Msgs {
		if i >= len(ps.Kinds) {
			break // cannot happen via add(); guarded rather than panicking on a gauge
		}
		t := est(m.Content)
		switch ps.Kinds[i] {
		case sectSystem:
			// Subtract the map when it was concatenated in here, so it is counted once
			// wherever it sits. The subtraction is of the SECTION's own estimate, so the
			// handful of tokens that concatenation rounding moves lands in Unattributed
			// instead of being quietly absorbed into "system".
			if ps.MapPlacement == "system" {
				t -= g.RepoMapTokens
			}
			g.SystemTokens += t
		case sectMap:
			// Already counted from MapSection above; counting the message too would
			// double it.
		case sectHot:
			g.PinnedHotTokens += t
		case sectHistory:
			g.HistoryTokens += t
		}
	}

	// Used is measured over the WHOLE prompt, independently of the walk above. That
	// independence is what makes Unattributed a real check rather than a tautology.
	for _, m := range ps.Msgs {
		g.Used += est(m.Content)
	}
	g.Used += schemaTokens
	// Characters, over the SAME set the tokens were summed over, so the effective
	// rate below is this prompt's own and not a constant's.
	g.Chars = messageChars(ps.Msgs) + schemaChars
	if g.Used > 0 {
		g.EffectiveRatio = float64(g.Chars) / float64(g.Used)
	}
	g.Free = g.Usable - g.Used
	g.FreeToCompaction = g.Trigger - g.Used
	// Which budget binds. capWorkingSet only ever LOWERS the fractional trigger, so
	// the cap is binding exactly when the trigger came out below the fraction.
	_, tfrac := ContextFractions()
	switch frac := int(tfrac * float64(ps.Usable)); {
	case g.Trigger <= 0:
		g.Binding = "none"
	case g.Trigger < frac:
		g.Binding = "working-set"
	default:
		g.Binding = "trigger-fraction"
	}
	g.Unattributed = g.Used - (g.SystemTokens + g.SchemaTokens + g.RepoMapTokens + g.PinnedHotTokens + g.HistoryTokens)
	if g.RepoMapTokens > 0 && ps.MapBudget > 0 && g.RepoMapTokens > ps.MapBudget {
		g.MapOverBudget = g.RepoMapTokens - ps.MapBudget
		g.MapOverBudgetPct = 100 * float64(g.MapOverBudget) / float64(ps.MapBudget)
	}
	return g
}

// ContextGauge returns the occupancy of the most recently assembled prompt, or the
// zero value when no turn has been assembled yet. Read-only: it hands back what
// buildPrompt already measured, so asking never re-assembles anything.
func (l *Loop) ContextGauge() ContextGauge { return l.lastGauge }

// BuildPromptForMeasurement assembles TURN ONE for `task` and nothing else: it runs
// the loop's own buildPrompt and returns how many messages and schema tokens came
// out, leaving the measurement on the Loop for ContextGauge to read. No model call,
// no tool call, no write.
//
// It exists so `kloo context` can measure the prompt the loop would send WITHOUT a
// second assembly path. The alternative — a CLI that rebuilds the prompt from the
// budget constants — is the exact mistake `kloo doctor` made about the working-set
// cap, where the report and the behaviour were derived from the same numbers and
// agreed with each other while both were wrong.
//
// Turn one is the honest thing to report for a task that has not run, and it is also
// the turn that matters: the system prompt, AGENTS.md and the repo map are what is
// paid again on every turn after it.
func (l *Loop) BuildPromptForMeasurement(ctx context.Context, task string) (messages, schemaTokens int, err error) {
	ps, req, err := l.buildPrompt(ctx, task,
		[]llm.Message{{Role: llm.RoleUser, Content: task}}, // convo[0] is the task
		VerifyResult{}, "") // no verify signal yet, no file under edit
	if err != nil {
		return 0, 0, err
	}
	return len(ps.Msgs), l.estimatedPromptTokens(l.toolSchemaChars(req.Tools)), nil
}

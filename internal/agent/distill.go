package agent

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/lokalhub/kloo/internal/llm"
)

// Distillation: when the running summary outgrows its budget, hand the oldest
// entries to the MODEL and keep what it writes back, instead of deleting them.
//
// kloo's compaction has only ever been able to shrink text, never to preserve
// meaning: summarizeCold truncates each entry to maxKeepItemTokens and the budget
// then drops the oldest outright. That is why a measured fold of 241 messages into
// 80 returned the prompt 60714 -> 60865 tokens — it ran and gave back nothing. The
// standard remedy is the summary-buffer: keep the recent turns verbatim, replace
// the old ones with a model-written brief. Every agent framework ships it.
//
// Two constraints shape WHERE it runs, and they are the only kloo-specific part:
//
//  1. It runs at the FOLD BOUNDARY, never per turn. Rewriting the middle of a
//     prompt invalidates the prefix KV cache, and on a cached endpoint appending
//     measured ~15x cheaper than changing the head (1.05s vs 17.37s on a 30k
//     prompt). Hysteresis already holds that boundary to roughly three moves per
//     run, so this costs ~3 extra model calls, not one per step.
//  2. It FAILS OPEN. A summarizer that errors, times out or returns junk must cost
//     nothing but the old behaviour — the entries are dropped exactly as before. A
//     run must never die because its own bookkeeping call did.
const distillSystem = "You compress an autonomous coding agent's run log.\n" +
	"You are given the OLDEST entries of a run that no longer fit in the agent's working memory.\n" +
	"Rewrite them as a dense brief the agent can act on, in at most %d words.\n\n" +
	"Keep, in this order of priority:\n" +
	"  1. Files edited or written, and what the change was.\n" +
	"  2. Commands run and whether they passed or failed, with the failing message.\n" +
	"  3. Conclusions already established — what was ruled out, what was confirmed.\n" +
	"  4. Anything the agent still intends to do.\n\n" +
	"Drop: file contents, directory listings, repeated reads, narration, apologies.\n" +
	"Never invent a fact that is not in the entries. If an entry is ambiguous, omit it.\n" +
	"Write terse notes, not prose. No preamble, no heading, no markdown."

// distillMaxWords bounds the brief. Words rather than tokens because the model is
// being asked, not clamped, and a word budget is the instruction it follows best;
// the hard bound is maxTokens on the request.
const distillMaxWords = 220

// distillTimeoutTokens caps the completion. Generous against distillMaxWords so a
// model that overshoots is truncated by the sampler rather than refused.
const distillMaxTokens = 700

// distillEnabled reports whether the distillation pass is on (KLOO_DISTILL=0 to
// disable). On by default: dropping the oldest half of a run's record is strictly
// worse than summarising it, and the call is bounded and fails open.
func distillEnabled() bool { return envOnDefault("KLOO_DISTILL") }

// distiller returns the closure Assemble calls when the summary overflows, or nil
// when distillation is off or there is no client to call. Bound to ctx so an
// interrupt cancels the summariser with the run.
func (l *Loop) distiller(ctx context.Context) func([]string) (string, error) {
	if l.Client == nil || !distillEnabled() {
		return nil
	}
	return func(entries []string) (string, error) {
		if len(entries) == 0 {
			return "", fmt.Errorf("agent: nothing to distill")
		}
		req := llm.ChatRequest{
			Model: l.Model,
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: fmt.Sprintf(distillSystem, distillMaxWords)},
				{Role: llm.RoleUser, Content: strings.Join(entries, "\n")},
			},
			Temperature: 0,
			MaxTokens:   distillMaxTokens,
		}
		// Complete, never the streaming path: this is bookkeeping and must not appear
		// in the transcript as if the model were talking to the user.
		resp, err := l.Client.Complete(ctx, l.withThinkingControl(req))
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("agent: distiller returned no choices")
		}
		msg := resp.Choices[0].Message
		msg.FinalizeReasoning()
		out := strings.TrimSpace(msg.Content)
		if out == "" {
			return "", fmt.Errorf("agent: distiller returned nothing")
		}
		l.observeUsage(resp.Usage)
		logDistillation(entries, out)
		return out, nil
	}
}

// distilledPrefix marks a model-written brief in the summary. It is matched by
// durableSummaryEntry so a later overflow folds it into the NEXT brief (a summary
// of summaries) rather than deleting the only compacted record of the early run.
const distilledPrefix = "[earlier in this run] "

// distilledEntry wraps a brief as a summary entry.
func distilledEntry(brief string) string {
	return distilledPrefix + strings.TrimSpace(brief)
}

// logDistillation appends what the summariser was given and what it wrote to
// KLOO_DISTILL_LOG, when set. This pass REWRITES the run's own memory, and a
// rewrite you cannot inspect is one you cannot trust: the only way to know whether
// a brief kept the facts or quietly invented them is to read it next to its input.
// Off unless the variable is set, and a logging failure is ignored — observability
// must never be able to break the run it is observing.
func logDistillation(entries []string, brief string) {
	path := os.Getenv("KLOO_DISTILL_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n=== distilled %d entries -> %d chars ===\n--- in ---\n%s\n--- brief ---\n%s\n",
		len(entries), len(brief), strings.Join(entries, "\n"), brief)
}

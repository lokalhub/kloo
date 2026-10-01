package agent

import (
	"context"
	"strings"

	"github.com/lokalhub/kloo/internal/llm"
)

// Answering when the run is cut short.
//
// Report.Summary was populated in exactly ONE place: the model calling the finish
// tool (loop.go). On a budget stop, a rail stop, churn or an error it stayed
// empty, so the terminal showed a counters box and nothing else — the model had
// been working for minutes and the user was told only how much it cost. That is
// the same defect as the dropped finish summary fixed in v0.24.2, arriving by a
// third route, and the content was never actually missing: Report.Transcript sits
// on the same struct.
//
// Two tiers, in this order:
//
//  1. salvageAnswer — free. The last substantive assistant prose already in the
//     conversation. No model call, no new failure mode.
//  2. finalAnswer — one bounded, TOOL-FREE call asking the model to answer with
//     what it already has.
//
// Tier 1 is also tier 2's fallback: when the endpoint is the reason the run
// stopped, asking it again is pointless, and a second error reported on top of the
// first is worse than the first alone.

// finalAnswerMaxTokens bounds the closing reply. Deliberately small: this is a
// summary of work already done, not new work, and the run has by definition just
// exhausted something.
const finalAnswerMaxTokens = 1024

// FinalAnswerReserve is the token allowance held back from the run budget so the
// closing answer fits INSIDE the ceiling the user set.
//
// Tripping the budget at maxTokens and then spending more on a courtesy reply
// would mean --max-tokens 600000 actually costs 600000 plus however much the reply
// came to. A budget that quietly overshoots itself is the same complaint one layer
// down, so the reserve is subtracted from the ceiling instead: the budget trips
// slightly early and the answer is paid for out of it.
const FinalAnswerReserve = 2048

// salvageAnswer returns the last substantive assistant prose in convo, or "".
//
// It is NOT presented as a conclusion. The last thing a model said may be a
// fragment mid-investigation ("Let me check the worker config next"), and
// rendering that under a heading implying a final answer would be worse than
// silence — it would look like kloo concluded something it did not. The renderers
// label it as the last thing kloo said, explicitly not a final answer.
func salvageAnswer(convo []llm.Message) string {
	for i := len(convo) - 1; i >= 0; i-- {
		if convo[i].Role != llm.RoleAssistant {
			continue
		}
		if c := strings.TrimSpace(convo[i].Content); c != "" {
			return c
		}
	}
	return ""
}

// finalAnswerPrompt asks for the answer, and explicitly PERMITS an incomplete one.
//
// That permission is the load-bearing part. A model told only "answer now" will
// produce an answer whether or not it has one — which on a run that was stopped
// for spinning is precisely when it has least to say. Told it may report what it
// found and what is still unknown, it usually reports exactly that.
const finalAnswerPrompt = "This run has stopped: you are out of budget and no further tools are available. " +
	"Reply now, in prose, using ONLY what you already know from this conversation.\n" +
	"- If you can answer the task, answer it.\n" +
	"- If you cannot, say what you DID establish and what is still unknown.\n" +
	"Do not describe the steps you took, and do not claim anything you have not seen."

// finalAnswer makes one bounded, tool-free call for a closing reply, falling back
// to the salvaged prose on any failure.
//
// No tools are offered, so the model physically cannot resume reading. One attempt
// only, and no retry: a run stopped by a dead or saturated endpoint must not pay
// the retry ladder a second time — that is the 6m15s-for-zero-tokens pattern.
func (l *Loop) finalAnswer(ctx context.Context, system, convo []llm.Message) (string, bool) {
	salvaged := salvageAnswer(convo)
	if l.Client == nil || l.NoFinalAnswer {
		return salvaged, false
	}
	// BOUNDED, and carrying the system prompt. Sending the raw convo was wrong on
	// both counts: TestLoopBoundsConversationHistory caught it putting 25 messages
	// on the wire where every other request is held to maxConv, which on a long run
	// is exactly the window overflow this feature is supposed to be reporting. The
	// system prompt matters too — without it the model answers with no idea what it
	// is or what the workspace is.
	msgs := append([]llm.Message{}, system...)
	msgs = append(msgs, boundedHistory(convo, l.maxConv())...)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: finalAnswerPrompt})
	resp, err := l.Client.Complete(ctx, l.withThinkingControl(llm.ChatRequest{
		Model:       l.Model,
		Messages:    msgs,
		Temperature: l.Temperature,
		MaxTokens:   finalAnswerMaxTokens,
	}))
	if err != nil {
		return salvaged, false
	}
	if len(resp.Choices) == 0 {
		return salvaged, false
	}
	if answer := strings.TrimSpace(resp.Choices[0].Message.Content); answer != "" {
		return answer, true
	}
	return salvaged, false
}

// answerableStop reports whether a terminal reason deserves a closing answer.
//
// Success is excluded: it already has a summary and a green verify. ReasonError is
// excluded because the endpoint is usually WHY the run stopped — a dead host or an
// "upstream at capacity" 503 — and asking it again buys another timeout and
// reports a second error on top of the first.
func answerableStop(reason Reason) bool {
	switch reason {
	case ReasonBudgetExceeded, ReasonExploreStop, ReasonChurn, ReasonUnverified:
		return true
	default:
		return false
	}
}

// finalAnswerSystem is the base system prompt WITHOUT the repo map.
//
// The map is reference material for finding things to read, and this call cannot
// read: no tools are offered and the run is already over. Re-sending it would add
// tens of thousands of tokens to the one request made specifically because the run
// ran out of them.
func (l *Loop) finalAnswerSystem() []llm.Message {
	if strings.TrimSpace(l.System) == "" {
		return nil
	}
	return []llm.Message{{Role: llm.RoleSystem, Content: l.System}}
}

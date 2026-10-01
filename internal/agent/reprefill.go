package agent

import (
	"fmt"
	"os"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tokens"
)

// Re-prefill accounting.
//
// A provider's prefix KV cache makes everything BEFORE the first byte that differs
// from the previous request free, and everything after it paid for again, in full,
// every turn. So the number that decides whether kloo is leveraging the context the
// model already has is not the prompt size — it is the size of the SUFFIX that
// changed.
//
// kloo already protects the prefix structurally (prefix_stability_test.go enforces
// append-only ordering and keeps volatile blocks below stable ones) and already
// REPORTS the provider's own cache figures (cached_prompt_tokens, cache_hit_rate).
// What was missing is kloo's own measurement of what it re-sends, which needs no
// provider and no network, so it can gate a change the way the stability invariants
// do rather than waiting on a benchmark.
//
// This exists to be a baseline. Two blocks dominate it today and neither changes
// between turns: the frozen repo map at the tail (~22k tokens) and the file pin,
// which is re-read from disk and re-sent in full every turn (22-24k on kloo-bench
// C66). Measuring first means the fix can be proven rather than believed.

// promptBytes renders a message list the way the comparison needs it: role and
// content concatenated, in order. It is not the wire format — it does not need to
// be. It only needs to change exactly when the wire prompt changes, and to do so at
// the same POSITION, which concatenating role+content in order does.
func promptBytes(msgs []llm.Message) []byte {
	var out []byte
	for _, m := range msgs {
		out = append(out, m.Role...)
		out = append(out, '\x00')
		out = append(out, m.Content...)
		out = append(out, '\x00')
	}
	return out
}

// commonPrefixLen is the number of leading bytes a and b share.
func commonPrefixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// RePrefill is one turn's accounting.
type RePrefill struct {
	// PromptBytes is the whole prompt; CachedBytes is the part a prefix cache can
	// reuse; ChangedBytes is what must be prefilled again.
	PromptBytes  int
	CachedBytes  int
	ChangedBytes int
}

// rePrefill compares this turn's prompt with the previous turn's.
//
// Byte granularity, not message granularity, and deliberately: a prefix cache
// matches on tokens, so a message that is appended to (rather than replaced) still
// shares its leading bytes. Counting whole messages as changed would overstate the
// cost of exactly the append-only growth kloo is built around.
//
// The first turn has nothing to compare against, so all of it is changed — which is
// true: nothing was cached.
func rePrefill(prev, cur []llm.Message) RePrefill {
	curB := promptBytes(cur)
	if prev == nil {
		return RePrefill{PromptBytes: len(curB), ChangedBytes: len(curB)}
	}
	shared := commonPrefixLen(promptBytes(prev), curB)
	return RePrefill{
		PromptBytes:  len(curB),
		CachedBytes:  shared,
		ChangedBytes: len(curB) - shared,
	}
}

// RePrefillStats is a run's accounting, carried on the Report.
type RePrefillStats struct {
	// Turns is how many prompts were measured.
	Turns int
	// ChangedBytes/PromptBytes are cumulative across the run.
	ChangedBytes int
	PromptBytes  int
	// ChangedTokens is ChangedBytes converted at the run's measured chars-per-token
	// ratio, so the headline number is in the unit everything else is budgeted in.
	ChangedTokens int
	// PeakChangedBytes is the worst single turn. The mean hides the shape: a run
	// with one compaction has a single enormous turn among cheap ones, and that turn
	// is the one that costs wall clock.
	PeakChangedBytes int
}

// Mean re-prefilled bytes per turn, 0 when nothing was measured.
func (s RePrefillStats) MeanChangedBytes() int {
	if s.Turns == 0 {
		return 0
	}
	return s.ChangedBytes / s.Turns
}

// CachedFraction is the share of all prompt bytes a prefix cache could serve. This
// is the number to move: 1.0 means every turn re-sends only what is new.
func (s RePrefillStats) CachedFraction() float64 {
	if s.PromptBytes == 0 {
		return 0
	}
	return float64(s.PromptBytes-s.ChangedBytes) / float64(s.PromptBytes)
}

// observeRePrefill records one turn against the previous one.
//
// KLOO_REPREFILL_LOG=1 prints the per-turn shape. The mean hides what matters: a
// run whose prefix is stable costs a few hundred tokens a turn, and a run that
// invalidates it costs the whole prompt — the same average can come from either.
func (l *Loop) observeRePrefill(msgs []llm.Message) {
	r := rePrefill(l.lastPromptMsgs, msgs)
	if os.Getenv("KLOO_REPREFILL_LOG") == "1" {
		pct := 0.0
		if r.PromptBytes > 0 {
			pct = float64(r.ChangedBytes) / float64(r.PromptBytes) * 100
		}
		fmt.Fprintf(os.Stderr, "[reprefill] turn=%d prompt=%dB changed=%dB (%.0f%%) cached=%dB\n",
			l.rePrefill.Turns+1, r.PromptBytes, r.ChangedBytes, pct, r.CachedBytes)
	}
	l.rePrefill.Turns++
	l.rePrefill.ChangedBytes += r.ChangedBytes
	l.rePrefill.PromptBytes += r.PromptBytes
	if r.ChangedBytes > l.rePrefill.PeakChangedBytes {
		l.rePrefill.PeakChangedBytes = r.ChangedBytes
	}
	// Keep a copy: msgs is rebuilt each turn, but the slice's backing array is not
	// guaranteed to be, and comparing a mutated snapshot would silently report a
	// perfect cache.
	l.lastPromptMsgs = append([]llm.Message(nil), msgs...)
}

// rePrefillStats finalises the run's accounting, converting bytes to tokens at the
// ratio this run actually measured (tokens.Calibrator) rather than a constant, so
// the headline number is comparable with every other token figure kloo reports.
func (l *Loop) rePrefillStats() RePrefillStats {
	st := l.rePrefill
	ratio := l.tokenRatio()
	if ratio <= 0 {
		ratio = tokens.DefaultCharsPerToken
	}
	st.ChangedTokens = int(float64(st.ChangedBytes) / ratio)
	return st
}

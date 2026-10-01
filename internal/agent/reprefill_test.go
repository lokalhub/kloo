package agent

import (
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

func msgs(pairs ...string) []llm.Message {
	out := make([]llm.Message, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, llm.Message{Role: pairs[i], Content: pairs[i+1]})
	}
	return out
}

// The first turn has nothing to compare against, and reporting it as fully changed
// is the truth: nothing was cached.
func TestRePrefillFirstTurnIsAllChanged(t *testing.T) {
	cur := msgs("system", "you are kloo", "user", "the task")
	r := rePrefill(nil, cur)
	if r.CachedBytes != 0 || r.ChangedBytes != r.PromptBytes {
		t.Errorf("first turn = %+v, want everything changed", r)
	}
}

// THE SHAPE KLOO IS BUILT AROUND. Appending a turn must leave everything before it
// cacheable — if this regresses, the whole append-only design has stopped paying.
func TestRePrefillAppendOnlyChangesOnlyTheTail(t *testing.T) {
	prev := msgs("system", "you are kloo", "user", "the task")
	cur := msgs("system", "you are kloo", "user", "the task", "assistant", "ok")
	r := rePrefill(prev, cur)
	if r.CachedBytes != len(promptBytes(prev)) {
		t.Errorf("cached %d bytes, want the whole previous prompt (%d)", r.CachedBytes, len(promptBytes(prev)))
	}
	if r.ChangedBytes >= r.PromptBytes {
		t.Errorf("changed %d of %d — an append must not invalidate the prefix", r.ChangedBytes, r.PromptBytes)
	}
}

// A block that changes EARLY invalidates everything after it, which is why the
// volatility ordering in prefix_stability_test.go exists. The metric has to show
// that cost, or it cannot defend the invariant.
func TestRePrefillEarlyChangeInvalidatesTheRest(t *testing.T) {
	prev := msgs("system", "you are kloo", "user", "a", "user", "b", "user", "c")
	cur := msgs("system", "you are KLOO", "user", "a", "user", "b", "user", "c")
	r := rePrefill(prev, cur)
	if r.CachedBytes > 20 {
		t.Errorf("cached %d bytes after an early change — the suffix must be re-prefilled", r.CachedBytes)
	}
}

// Byte granularity, not message granularity: a message that GROWS still shares its
// leading bytes, and counting the whole message as changed would overstate exactly
// the append-only growth kloo depends on.
func TestRePrefillIsByteGranular(t *testing.T) {
	prev := msgs("user", "line one\n")
	cur := msgs("user", "line one\nline two\n")
	r := rePrefill(prev, cur)
	if r.CachedBytes < len("line one\n") {
		t.Errorf("cached %d bytes, want at least the shared leading text", r.CachedBytes)
	}
}

// A regenerated block at the TAIL costs only itself; the same block moved to the
// HEAD costs the whole prompt. This is the repo map / file pin decision, as a
// number — and it is what the metric exists to put a price on.
func TestRePrefillTailVsHeadPlacement(t *testing.T) {
	history := []string{"user", "turn 1", "assistant", "reply 1", "user", "turn 2", "assistant", "reply 2"}
	bigA, bigB := "MAP-VERSION-A"+str2048(), "MAP-VERSION-B"+str2048()

	tailPrev := msgs(append(append([]string{}, history...), "user", bigA)...)
	tailCur := msgs(append(append([]string{}, history...), "user", bigB)...)
	tail := rePrefill(tailPrev, tailCur)

	headPrev := msgs(append([]string{"user", bigA}, history...)...)
	headCur := msgs(append([]string{"user", bigB}, history...)...)
	head := rePrefill(headPrev, headCur)

	if !(tail.ChangedBytes < head.ChangedBytes) {
		t.Fatalf("tail changed %d, head changed %d — placing a volatile block early must cost MORE",
			tail.ChangedBytes, head.ChangedBytes)
	}
	if tail.CachedBytes == 0 {
		t.Error("a tail-placed volatile block must still leave the history cacheable")
	}
}

func str2048() string {
	b := make([]byte, 2048)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func TestRePrefillStatsAggregates(t *testing.T) {
	var s RePrefillStats
	if s.MeanChangedBytes() != 0 || s.CachedFraction() != 0 {
		t.Error("zero value must not divide by zero")
	}
	s = RePrefillStats{Turns: 2, ChangedBytes: 300, PromptBytes: 1000, PeakChangedBytes: 200}
	if got := s.MeanChangedBytes(); got != 150 {
		t.Errorf("mean = %d, want 150", got)
	}
	if got := s.CachedFraction(); got != 0.7 {
		t.Errorf("cached fraction = %v, want 0.7", got)
	}
}

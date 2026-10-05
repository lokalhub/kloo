package agent

// THE ABSOLUTE WORKING-SET CAP.
//
// Compaction used to trigger at a FRACTION of the declared context window, and
// nothing else. That is right for a small window and wrong for a large one,
// because the fraction scales with the window while the useful working set does
// not. At --ctx 8000 the trigger lands at 4480 tokens and sheds almost at once;
// at --ctx 131072 it lands at 73399 tokens, which means "let the prompt reach
// ~290KB before doing anything".
//
// Measured on a read-only question against kloo's own tree (qwen3.8-flash-next,
// --ctx 131072), the prompt grew monotonically and nothing was ever shed:
//
//	turn=1   prompt=82977B   changed=82977B (100%)
//	turn=7   prompt=141549B  changed=7980B  (6%)
//	turn=13  prompt=175607B  changed=5506B  (3%)
//	turn=16  prompt=176218B  changed=149B   (0%)   tokens 661087
//	turn=23  prompt=217569B  changed=311B   (0%)
//
// The prefix cache did its job — `changed` stayed at a few percent, so prefill
// LATENCY was fine. But the whole prompt is counted every turn, so total spend is
// the SUM of the prompt sizes: 661k tokens by step 16, and 149 bytes of new
// information on the turn that cost 176KB. Growth is linear, spend is quadratic.
//
// So the trigger needs a second, window-independent bound: hold the working set
// near an absolute target no matter how large the window is. The model still gets
// the full window for the one call that needs it; it stops carrying thirty turns
// of stale read dumps there.
//
// This is a MINIMUM against the fractional trigger, never a maximum, which makes
// it a strict no-op wherever the fraction is already tighter — every window at or
// below ~47k tokens declared. The small-ctx behaviour all of kloo's bench history
// was measured against is therefore unchanged, and nothing needs re-baselining.
const defaultWorkingSetTokens = 32768

// workingSetTokens is the cap in force. Sign convention, as everywhere else in
// kloo: 0 ⇒ the built-in default, negative ⇒ disabled (pure fractional trigger,
// the pre-v0.25.6 behaviour).
var workingSetTokens = defaultWorkingSetTokens

// SetWorkingSetTokens overrides the cap. 0 restores the built-in default and a
// negative value disables the cap entirely, so a bad config degrades to the old
// fraction-only behaviour rather than to a working set of zero.
func SetWorkingSetTokens(n int) {
	switch {
	case n == 0:
		workingSetTokens = defaultWorkingSetTokens
	default:
		workingSetTokens = n
	}
}

// WorkingSetTokens reports the cap in force, for `kloo doctor`. A non-positive
// return means the cap is off and the trigger is the fraction alone.
func WorkingSetTokens() int { return workingSetTokens }

// capWorkingSet applies the absolute cap to a fractional trigger. It only ever
// LOWERS the trigger: a window whose fraction is already below the cap is
// untouched.
func capWorkingSet(trigger int) int {
	if workingSetTokens > 0 && trigger > workingSetTokens {
		return workingSetTokens
	}
	return trigger
}

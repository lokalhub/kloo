package agent

import "math"

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
// 32768 is the BASE of the curve below, not the answer for every window.
const defaultWorkingSetTokens = 32768

// workingSetFor is the cap for a given window.
//
// A flat constant was the first cut and it is wrong in the other direction: it
// made the working set window-BLIND. At --ctx 131072 it held the prompt to 25% of
// the window; at the 900k a profile can declare, 3.6%. kloo was refusing to use
// context it had already paid for, and no amount of context made it any better at
// a long task — the opposite of what a bigger model is bought for.
//
// The honest shape is in between: the useful working set GROWS with the window,
// but slower than the window does, because effective context never keeps up with
// the declared number. The geometric mean of the window and the base is exactly
// that curve — it doubles the cap for every 4x of window.
//
// `window` here is the USABLE window the loop assembles against (0.80 of declared,
// memory.go usableWindow), which is what triggerTokens is given:
//
//	declared    usable  fraction       cap   effective share of declared
//	   32768     26214     18349     29308     56%
//	   65536     52428     36699     41448     56%
//	  131072    104857     73399     58617     45%
//	  262144    209715    146800     82897     32%
//	 1048576    838860    587202    165794     16%
//
// Below ~65k declared the fraction is already tighter, so this is a strict no-op
// there and every bench number measured at a small ctx still stands.
func workingSetFor(window int) int {
	// An explicitly configured cap is a fixed number by definition: someone who
	// typed --working-set-tokens 40000 means 40000, not a curve through it. Tracked
	// by a FLAG rather than by comparing against the default, because 32768 is a
	// legitimate thing to ask for and comparing values made that request silently
	// mean "use the curve" instead.
	if workingSetExplicit || window <= 0 {
		return workingSetTokens
	}
	return int(math.Sqrt(float64(window) * float64(defaultWorkingSetTokens)))
}

// workingSetTokens is the cap in force. Sign convention, as everywhere else in
// kloo: 0 ⇒ the built-in default, negative ⇒ disabled (pure fractional trigger,
// the pre-v0.25.6 behaviour).
var workingSetTokens = defaultWorkingSetTokens

// workingSetExplicit records that the cap was set by configuration, which pins it
// to that exact number instead of letting it follow the window.
var workingSetExplicit bool

// SetWorkingSetTokens overrides the cap. 0 restores the built-in default and a
// negative value disables the cap entirely, so a bad config degrades to the old
// fraction-only behaviour rather than to a working set of zero.
func SetWorkingSetTokens(n int) {
	switch {
	case n == 0:
		workingSetTokens, workingSetExplicit = defaultWorkingSetTokens, false
	default:
		workingSetTokens, workingSetExplicit = n, true
	}
}

// WorkingSetTokens reports the configured cap, for `kloo doctor`. A non-positive
// return means the cap is off and the trigger is the fraction alone. Use
// WorkingSetTokensFor when the window is known — at the default the cap is a
// function of it.
func WorkingSetTokens() int { return workingSetTokens }

// WorkingSetTokensFor reports the cap that will actually apply at this window.
func WorkingSetTokensFor(window int) int { return workingSetFor(window) }

// capWorkingSet applies the absolute cap to a fractional trigger. It only ever
// LOWERS the trigger: a window whose fraction is already below the cap is
// untouched.
func capWorkingSet(trigger int, window int) int {
	cap := workingSetFor(window)
	if cap > 0 && trigger > cap {
		return cap
	}
	return trigger
}

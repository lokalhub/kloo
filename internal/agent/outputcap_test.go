package agent

import "testing"

// THE PROPERTY THAT MOTIVATED A COMPUTED DEFAULT. A fixed fraction is wrong at
// both ends: 20% of 8000 is 1600, enough to truncate a legitimate write_file,
// while 20% of 131072 bounds nothing worth bounding. Sized from the ACTUAL prompt,
// a small prompt leaves nearly the whole window to write into.
func TestComputeOutputCapAdaptsToThePrompt(t *testing.T) {
	const window = 8000
	small := computeOutputCap(window, 2000)
	large := computeOutputCap(window, 6000)
	if small <= 1600 {
		t.Errorf("small prompt cap = %d, want well above the flat 20%% (1600)", small)
	}
	if large >= small {
		t.Errorf("a fuller prompt must tighten the cap: %d (full) vs %d (small)", large, small)
	}
	// It must never promise space the window does not have.
	if got := 6000 + large; got > window {
		t.Errorf("prompt+cap = %d exceeds the window %d", got, window)
	}
}

// window <= 0 means the endpoint reported no context length, so the window is
// kloo's ASSUMPTION. Capping a reply from an assumption is how a file write gets
// truncated for no reason — send nothing and keep the previous behaviour.
func TestComputeOutputCapSendsNothingOnAnUnknownWindow(t *testing.T) {
	for _, w := range []int{0, -1} {
		if got := computeOutputCap(w, 1000); got != 0 {
			t.Errorf("window %d: cap = %d, want 0 (send nothing)", w, got)
		}
	}
}

// With the prompt nearly filling the window, a cramped cap guarantees a truncated
// reply. Send nothing and let the context-overflow handler give its better error.
func TestComputeOutputCapSendsNothingWhenThereIsNoRoom(t *testing.T) {
	if got := computeOutputCap(8000, 7900); got != 0 {
		t.Errorf("cap = %d, want 0 when the prompt has filled the window", got)
	}
}

// Slack exists for estimator error and must survive at small windows, where the
// percentage alone would round to almost nothing.
func TestComputeOutputCapKeepsAFloorOfSlack(t *testing.T) {
	const window, prompt = 2000, 100
	got := computeOutputCap(window, prompt)
	if want := window - prompt - outputCapMinSlack; got != want {
		t.Errorf("cap = %d, want %d (the 256 floor, not 5%% of 2000 = 100)", got, want)
	}
}

// The resolver's sign convention matches every other kloo knob: 0 = built-in
// (here, computed), negative = off.
func TestLoopOutputCapResolution(t *testing.T) {
	l := &Loop{ContextTokens: 8000}
	if got := l.outputCap(2000); got == 0 {
		t.Error("default (0) should compute a cap")
	}
	if got := (&Loop{ContextTokens: 8000, MaxOutputTokens: -1}).outputCap(2000); got != 0 {
		t.Errorf("negative = %d, want 0 (never send)", got)
	}
	if got := (&Loop{ContextTokens: 8000, MaxOutputTokens: 512}).outputCap(2000); got != 512 {
		t.Errorf("explicit = %d, want 512 verbatim", got)
	}
}

// Sizing must use the CALIBRATED ratio, and the char count it is given already
// includes the tool schemas — leaving them out would overstate the room left on
// every request.
func TestEstimatedPromptTokensUsesTheColdStartRatioWhenUncalibrated(t *testing.T) {
	l := &Loop{}
	if got, want := l.estimatedPromptTokens(4000), 1000; got != want {
		t.Errorf("estimatedPromptTokens(4000) = %d, want %d at 4.0 chars/token", got, want)
	}
	if got := l.estimatedPromptTokens(0); got != 0 {
		t.Errorf("zero chars = %d, want 0", got)
	}
}

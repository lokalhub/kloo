package agent

import "github.com/lokalhub/kloo/internal/tokens"

// Per-request completion cap (the OpenAI max_tokens).
//
// kloo never sent one. memory.go budgets the PROMPT to 80% of the window with the
// explicit comment "reserving the rest for the COMPLETION (n_ctx holds prompt +
// output)" — so it reserved headroom for a reply and then never bounded the reply
// to it. The server chose instead, and llama.cpp's choice is "until EOS or the
// context runs out".
//
// That is not only a runaway-output risk. The 400 kloo already parses reads:
//
//	this request needs ~132449 tokens, above the per-request limit of 131072.
//	Retrying will not help; shorten the prompt or lower max_tokens.
//
// "this request needs" is prompt PLUS max_tokens. With none sent, the server's own
// default goes into that sum and the request can be rejected before generating
// anything. Sending a computed cap makes it admissible — so this recovers a class
// of request that currently fails outright, as well as bounding replies.
const (
	// outputCapSlackFrac is held back for ESTIMATOR ERROR, not caution for its own
	// sake: the prompt figure below is an estimate (chars/ratio), and it undercounts
	// dense code and markup. The calibrator exposes Drift() so this can be checked
	// against real data rather than assumed.
	outputCapSlackFrac = 0.05
	outputCapMinSlack  = 256
	// outputCapFloor is the point below which kloo sends NOTHING rather than a
	// cramped cap. If this little room is left, the prompt is already filling the
	// window — that is compaction's problem, and the existing context-overflow
	// handler produces a far better error than a truncated reply would.
	outputCapFloor = 256
)

// computeOutputCap returns the max_tokens to send for a request whose prompt is
// estimated at promptTokens against a context window of window, or 0 meaning
// "send nothing".
//
// It is computed from the ACTUAL prompt rather than as a fixed share of the
// window, because a fixed share is wrong at both ends. At --ctx 8000, a flat 20%
// is 1600 tokens — enough to truncate a legitimate write_file — while the same
// 20% of a 131072 window is ~26k, which bounds nothing worth bounding. Sized from
// the prompt, an early turn with a small prompt gets nearly the whole window to
// write a file into, and a late turn with a full context tightens automatically.
//
// window <= 0 returns 0 DELIBERATELY. An endpoint that reports no context length
// (lokalai reports none for any model) means the window is kloo's assumption from
// a profile or the bundled table, not the server's fact. Capping a reply from an
// assumption is how a file write gets truncated for no reason, so when the input
// is a guess, nothing is sent and the previous behaviour stands.
func computeOutputCap(window, promptTokens int) int {
	if window <= 0 || promptTokens < 0 {
		return 0
	}
	slack := int(float64(window) * outputCapSlackFrac)
	if slack < outputCapMinSlack {
		slack = outputCapMinSlack
	}
	cap := window - promptTokens - slack
	if cap < outputCapFloor {
		return 0
	}
	return cap
}

// outputCap resolves the per-request completion cap for this loop:
//
//	MaxOutputTokens < 0  ⇒ never send (the escape hatch, and kloo's historical behaviour)
//	MaxOutputTokens > 0  ⇒ send it verbatim
//	MaxOutputTokens == 0 ⇒ computed from the window and the estimated prompt
func (l *Loop) outputCap(promptTokens int) int {
	if l.MaxOutputTokens < 0 {
		return 0
	}
	if l.MaxOutputTokens > 0 {
		return l.MaxOutputTokens
	}
	return computeOutputCap(l.ContextTokens, promptTokens)
}

// estimatedPromptTokens converts the character count of the request just built
// into tokens at the calibrator's CURRENT ratio — the ratio learned from this
// model's own reported usage (tokens.Calibrator.Observe), not a constant. It falls
// back to the cold-start ratio before anything has been observed.
//
// chars is lastPromptChars, which already includes the tool schemas; sizing the
// cap against messages alone would overstate the room left by the schemas' worth
// of tokens on every single request.
func (l *Loop) estimatedPromptTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	ratio := l.tokenRatio()
	if ratio <= 0 {
		ratio = tokens.DefaultCharsPerToken
	}
	return int(float64(chars) / ratio)
}

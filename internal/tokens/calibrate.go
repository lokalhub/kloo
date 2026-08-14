package tokens

import "sync"

// Calibrator learns the real chars-per-token ratio for the model in use.
//
// Every response carries usage.prompt_tokens — the exact token count for a
// prompt whose character count kloo also knows. That is a free measurement of
// the ratio, and kloo used to throw it away and keep guessing 4.0. After one
// turn the estimate stops being a guess.
//
// Cumulative rather than windowed: token accounting cares about totals, and a
// running total is stable against one weird turn while still moving steadily if
// a run's content shifts (a burst of lockfiles pulls the ratio down).
//
// Safe for concurrent use: the TUI observes from the run goroutine while the
// status line may read the ratio.
type Calibrator struct {
	mu     sync.Mutex
	chars  int64
	tokens int64
	seed   float64 // ratio used before any observation
}

// NewCalibrator returns a calibrator seeded with a starting ratio. Pass 0 (or a
// ratio outside the sane band) to start from DefaultCharsPerToken — that is the
// cold-start case, e.g. a model kloo has never talked to.
func NewCalibrator(seed float64) *Calibrator {
	if seed <= 0 {
		seed = DefaultCharsPerToken
	}
	return &Calibrator{seed: Clamp(seed)}
}

// Observe records one turn of ground truth: the characters kloo sent as the
// prompt, and the prompt token count the provider reported. Non-positive values
// are ignored — a provider that reports no usage teaches us nothing, and must
// not be allowed to poison the ratio with a zero.
func (c *Calibrator) Observe(promptChars, promptTokens int) {
	if promptChars <= 0 || promptTokens <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chars += int64(promptChars)
	c.tokens += int64(promptTokens)
}

// Ratio is the current chars-per-token estimate: measured once there is data,
// the seed until then, always clamped to the sane band.
func (c *Calibrator) Ratio() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ratioLocked()
}

func (c *Calibrator) ratioLocked() float64 {
	if c.tokens <= 0 {
		return c.seed
	}
	return Clamp(float64(c.chars) / float64(c.tokens))
}

// Calibrated reports whether the ratio is measured rather than assumed. Callers
// use it to decide whether to present a number as measured or approximate.
func (c *Calibrator) Calibrated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens > 0
}

// Estimate is the token estimate for s at the current ratio. This is the
// function the loop budgets with.
func (c *Calibrator) Estimate(s string) int {
	return EstimateAt(s, c.Ratio())
}

// Drift is the estimator's signed relative error over everything observed so
// far: what it would have predicted versus what was actually charged. 0 when
// nothing has been observed. Reported so the estimate's accuracy is a number
// you can see rather than a property that is assumed.
func (c *Calibrator) Drift(estimated int) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens <= 0 || estimated <= 0 {
		return 0
	}
	return float64(estimated)/float64(c.tokens) - 1
}

// Totals returns the cumulative characters and reported tokens observed.
func (c *Calibrator) Totals() (chars, tokens int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chars, c.tokens
}

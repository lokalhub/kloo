package tokens

import (
	"math"
	"sync"
	"testing"
)

// TestCalibratorLearnsFromReportedUsage: after one turn of ground truth the
// ratio is measured, not assumed.
func TestCalibratorLearnsFromReportedUsage(t *testing.T) {
	c := NewCalibrator(0)
	if c.Calibrated() {
		t.Error("a fresh calibrator has measured nothing")
	}
	if r := c.Ratio(); r != DefaultCharsPerToken {
		t.Errorf("cold-start ratio = %v, want %v", r, DefaultCharsPerToken)
	}

	c.Observe(8000, 2135) // the real go-source measurement
	if !c.Calibrated() {
		t.Error("should be calibrated after an observation")
	}
	if got, want := c.Ratio(), 8000.0/2135.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("ratio = %v, want %v", got, want)
	}
}

// TestCalibratorIgnoresUselessObservations: an endpoint that reports no usage
// must not drag the ratio to zero.
func TestCalibratorIgnoresUselessObservations(t *testing.T) {
	c := NewCalibrator(0)
	c.Observe(8000, 0)
	c.Observe(0, 2000)
	c.Observe(-1, -1)
	if c.Calibrated() {
		t.Error("non-positive observations must be ignored entirely")
	}
	if r := c.Ratio(); r != DefaultCharsPerToken {
		t.Errorf("ratio = %v, want the untouched default", r)
	}
}

// TestCalibratorClampsOutliers: a nonsense response cannot wreck budgeting.
func TestCalibratorClampsOutliers(t *testing.T) {
	low := NewCalibrator(0)
	low.Observe(100, 1_000_000) // absurdly dense
	if r := low.Ratio(); r != minCharsPerToken {
		t.Errorf("ratio = %v, want clamped to %v", r, minCharsPerToken)
	}
	high := NewCalibrator(0)
	high.Observe(1_000_000, 1) // absurdly sparse
	if r := high.Ratio(); r != maxCharsPerToken {
		t.Errorf("ratio = %v, want clamped to %v", r, maxCharsPerToken)
	}
	if r := NewCalibrator(99).Ratio(); r != maxCharsPerToken {
		t.Errorf("an out-of-band seed should clamp too, got %v", r)
	}
}

// TestCalibratorIsCumulative: the ratio tracks totals, so it is steady against
// one odd turn but still moves when a run's content genuinely shifts.
func TestCalibratorIsCumulative(t *testing.T) {
	c := NewCalibrator(0)
	c.Observe(4000, 1000) // 4.0 chars/token
	first := c.Ratio()
	c.Observe(1000, 500) // a dense turn: 2.0
	second := c.Ratio()
	if second >= first {
		t.Errorf("a denser turn should pull the ratio down: %v → %v", first, second)
	}
	if want := 5000.0 / 1500.0; math.Abs(second-want) > 1e-9 {
		t.Errorf("ratio = %v, want the cumulative %v", second, want)
	}
	chars, toks := c.Totals()
	if chars != 5000 || toks != 1500 {
		t.Errorf("totals = (%d, %d), want (5000, 1500)", chars, toks)
	}
}

// TestCalibratorEstimateUsesLearnedRatio: the whole point — budgeting follows
// the measurement.
func TestCalibratorEstimateUsesLearnedRatio(t *testing.T) {
	s := "the quick brown fox jumps over the lazy dog and keeps on running for a while"
	c := NewCalibrator(0)
	before := c.Estimate(s)
	c.Observe(1000, 500) // learn a dense 2.0 ratio
	after := c.Estimate(s)
	if after <= before {
		t.Errorf("a denser learned ratio should raise the estimate: %d → %d", before, after)
	}
}

// TestCalibratorDrift reports the estimator's signed error over the run.
func TestCalibratorDrift(t *testing.T) {
	c := NewCalibrator(0)
	if d := c.Drift(100); d != 0 {
		t.Errorf("drift with no observations = %v, want 0", d)
	}
	c.Observe(4000, 1000)
	if d := c.Drift(900); math.Abs(d-(-0.1)) > 1e-9 {
		t.Errorf("drift = %v, want -0.1 (estimated 900 vs 1000 real)", d)
	}
}

// TestCalibratorConcurrent: the TUI reads the ratio while the run observes.
func TestCalibratorConcurrent(t *testing.T) {
	c := NewCalibrator(0)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); c.Observe(400, 100) }()
		go func() { defer wg.Done(); _ = c.Estimate("some text to size") }()
	}
	wg.Wait()
	if _, toks := c.Totals(); toks != 5000 {
		t.Errorf("tokens = %d, want 5000 (lost updates under concurrency)", toks)
	}
}

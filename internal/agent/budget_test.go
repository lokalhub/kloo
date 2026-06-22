package agent

import (
	"testing"
	"time"

	"github.com/lokal/kloo/internal/config"
)

// fakeClock returns a controllable clock.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)}
}

func TestBudgetStepsTrip(t *testing.T) {
	b := NewBudget(config.Config{MaxSteps: 3}, time.Now)
	for s := 1; s <= 3; s++ {
		b.Observe(s)
		if tripped, _ := b.Check(); tripped {
			t.Fatalf("tripped early at step %d", s)
		}
	}
	b.Observe(4)
	tripped, kind := b.Check()
	if !tripped || kind != BudgetSteps {
		t.Errorf("want steps trip at step 4, got tripped=%v kind=%q", tripped, kind)
	}
}

func TestBudgetTokensTrip(t *testing.T) {
	b := NewBudget(config.Config{MaxTokens: 100}, time.Now)
	b.AddTokens(60)
	if tripped, _ := b.Check(); tripped {
		t.Fatal("tripped under token limit")
	}
	b.AddTokens(50) // now 110 > 100
	tripped, kind := b.Check()
	if !tripped || kind != BudgetTokens {
		t.Errorf("want tokens trip, got tripped=%v kind=%q", tripped, kind)
	}
}

func TestBudgetWallClockTrip(t *testing.T) {
	clk := newClock()
	b := NewBudget(config.Config{MaxWallClockSeconds: 2}, clk.now)
	if tripped, _ := b.Check(); tripped {
		t.Fatal("tripped at t=0")
	}
	clk.advance(3 * time.Second) // exceed 2s
	tripped, kind := b.Check()
	if !tripped || kind != BudgetWallClock {
		t.Errorf("want wall-clock trip, got tripped=%v kind=%q", tripped, kind)
	}
}

func TestBudgetUnderAllLimitsContinues(t *testing.T) {
	clk := newClock()
	b := NewBudget(config.Config{MaxSteps: 10, MaxTokens: 1000, MaxWallClockSeconds: 60}, clk.now)
	b.Observe(5)
	b.AddTokens(500)
	clk.advance(10 * time.Second)
	if tripped, _ := b.Check(); tripped {
		t.Errorf("should not trip under all limits")
	}
}

func TestBudgetFirstToTripWins(t *testing.T) {
	// steps and tokens both exceeded; steps is checked first.
	b := NewBudget(config.Config{MaxSteps: 1, MaxTokens: 1}, time.Now)
	b.Observe(5)
	b.AddTokens(5)
	tripped, kind := b.Check()
	if !tripped || kind != BudgetSteps {
		t.Errorf("steps should win, got kind=%q", kind)
	}
}

func TestBudgetResolvesFromConfigChain(t *testing.T) {
	// Thresholds come from resolved config (the flag>env>profile>default chain is
	// proven in config_test; here we confirm NewBudget reads them).
	cfg, err := config.Resolve(config.Flags{}, func(string) string { return "" }, "/does/not/exist.json")
	if err != nil {
		t.Fatal(err)
	}
	b := NewBudget(cfg, time.Now)
	st := b.Stats()
	if st.MaxSteps != config.DefaultMaxSteps || st.MaxTokens != config.DefaultMaxTokens {
		t.Errorf("budget did not read config defaults: %+v", st)
	}
}

func TestBudgetStatsCarriesLimitVsObserved(t *testing.T) {
	b := NewBudget(config.Config{MaxSteps: 2}, time.Now)
	b.Observe(3)
	st := b.Stats()
	if st.MaxSteps != 2 || st.Steps != 3 {
		t.Errorf("stats should carry limit (2) vs observed (3): %+v", st)
	}
}

package agent

import (
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/config"
)

// THE POINT OF THE WHOLE FEATURE. A run that keeps changing files renews its
// allowance with every edit and is never stopped by a ceiling kloo inferred —
// long agentic coding legitimately costs millions of tokens.
func TestComputedCeilingNeverStopsAProductiveRun(t *testing.T) {
	b := NewBudget(config.Config{MaxTokens: 100_000, MaxTokensComputed: true}, time.Now)
	for range 50 {
		b.AddTokens(90_000) // each turn alone is most of the ceiling
		if over, _ := b.Check(); over {
			t.Fatalf("a run that is editing was stopped by the computed ceiling at %d total", b.Stats().Tokens)
		}
		b.NoteProgress() // a real change landed
	}
	if got := b.Stats().Tokens; got < 4_000_000 {
		t.Fatalf("premise broken: only %d tokens spent, wanted well past any flat ceiling", got)
	}
}

// And the run it IS for: reading in circles, producing nothing.
func TestComputedCeilingStopsAnUnproductiveRun(t *testing.T) {
	b := NewBudget(config.Config{MaxTokens: 100_000, MaxTokensComputed: true}, time.Now)
	for range 3 {
		b.AddTokens(40_000) // no NoteProgress — nothing is changing
	}
	over, kind := b.Check()
	if !over || kind != BudgetTokens {
		t.Fatalf("over=%v kind=%v, want the token budget to stop a run that changed nothing", over, kind)
	}
}

// An EXPLICIT ceiling still means total spend. If a number was asked for by name,
// it means what it says — editing must not buy an exemption from it.
func TestExplicitCeilingBoundsTotalSpendEvenWhenEditing(t *testing.T) {
	b := NewBudget(config.Config{MaxTokens: 100_000}, time.Now) // MaxTokensComputed false
	for range 3 {
		b.AddTokens(40_000)
		b.NoteProgress()
	}
	over, kind := b.Check()
	if !over || kind != BudgetTokens {
		t.Fatalf("over=%v kind=%v, want an explicit ceiling to bound the whole run", over, kind)
	}
}

// Progress renews the allowance but must not erase the run total — the receipt has
// to say what the run actually cost.
func TestNoteProgressClearsOnlyTheUnproductiveCounter(t *testing.T) {
	b := NewBudget(config.Config{MaxTokens: 100_000, MaxTokensComputed: true}, time.Now)
	b.AddTokens(30_000)
	b.NoteProgress()
	b.AddTokens(5_000)
	st := b.Stats()
	if st.Tokens != 35_000 {
		t.Errorf("run total = %d, want 35000 (progress must not erase it)", st.Tokens)
	}
	if st.UnproductiveTokens != 5_000 {
		t.Errorf("unproductive = %d, want 5000 (reset by the edit, then 5000 since)", st.UnproductiveTokens)
	}
}

// Reset must clear BOTH, or a reused Loop carries the previous run's allowance.
func TestResetClearsUnproductive(t *testing.T) {
	b := NewBudget(config.Config{MaxTokens: 100_000, MaxTokensComputed: true}, time.Now)
	b.AddTokens(50_000)
	b.Reset()
	if st := b.Stats(); st.Tokens != 0 || st.UnproductiveTokens != 0 {
		t.Errorf("after Reset: %+v, want both counters zero", st)
	}
}

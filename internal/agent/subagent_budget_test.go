package agent

import "testing"

// A child that keeps landing edits keeps earning room. It is doing the job it was
// delegated, and a fixed step count stops it mid-work — measured on kloo-bench C07,
// where a child edited the correct file and ended with 2 of 6 tests still failing.
func TestSubagentAllowanceRenewsOnProgress(t *testing.T) {
	b := &stepBudget{maxSteps: 10, hardMaxSteps: 30}
	for step := 1; step <= 25; step++ {
		b.Observe(step)
		if over, _ := b.Check(); over {
			t.Fatalf("a child that is editing was stopped at step %d", step)
		}
		b.NoteProgress() // a real change landed this step
	}
}

// THE INVARIANT THAT MAKES RENEWING SAFE. One child must never be able to consume
// the whole run, so the absolute ceiling is not renewable.
func TestSubagentHardCeilingIsNotRenewable(t *testing.T) {
	b := &stepBudget{maxSteps: 10, hardMaxSteps: 30}
	for step := 1; step <= 40; step++ {
		b.Observe(step)
		b.NoteProgress() // edits every single step
		if over, kind := b.Check(); over {
			if kind != BudgetSteps {
				t.Fatalf("stopped with kind %q, want steps", kind)
			}
			if step != 30 {
				t.Fatalf("hard ceiling tripped at step %d, want 30", step)
			}
			return
		}
	}
	t.Fatal("a child editing every step was never stopped — it can consume the whole run")
}

// A child that is NOT changing anything is stopped at the renewable allowance,
// exactly as before this change.
func TestSubagentUnproductiveChildStopsAtTheAllowance(t *testing.T) {
	b := &stepBudget{maxSteps: 10, hardMaxSteps: 30}
	for step := 1; step <= 10; step++ {
		b.Observe(step)
		if over, _ := b.Check(); over {
			if step != 10 {
				t.Fatalf("stopped at step %d, want 10 (the allowance)", step)
			}
			return
		}
	}
	t.Fatal("an unproductive child was never stopped")
}

// Progress part-way through extends the deadline from WHERE IT HAPPENED, not from
// zero: a child that edits once and then spins still stops.
func TestSubagentProgressExtendsFromWhereItLanded(t *testing.T) {
	b := &stepBudget{maxSteps: 10, hardMaxSteps: 100}
	b.Observe(5)
	b.NoteProgress() // one edit at step 5
	for step := 6; step <= 20; step++ {
		b.Observe(step)
		if over, _ := b.Check(); over {
			if step != 15 {
				t.Fatalf("stopped at step %d, want 15 (5 + the 10-step allowance)", step)
			}
			return
		}
	}
	t.Fatal("a child that edited once and then spun was never stopped")
}

func TestSubagentResetClearsTheAllowance(t *testing.T) {
	b := &stepBudget{maxSteps: 10, hardMaxSteps: 30}
	b.Observe(8)
	b.NoteProgress()
	b.AddTokens(100)
	b.Reset()
	if b.steps != 0 || b.unproductive != 0 || b.tokens != 0 {
		t.Errorf("after Reset: steps=%d unproductive=%d tokens=%d, want all zero", b.steps, b.unproductive, b.tokens)
	}
}

// The constructed budget must carry BOTH bounds; a zero hard ceiling would make
// renewing unlimited.
func TestNewStepBudgetSetsBothCeilings(t *testing.T) {
	b, ok := (&Loop{SubagentMaxSteps: 25}).subagentBudget().(*stepBudget)
	if !ok {
		t.Fatal("subagentBudget did not return a *stepBudget")
	}
	if b.maxSteps != 25 {
		t.Errorf("maxSteps = %d, want 25", b.maxSteps)
	}
	if b.hardMaxSteps != 25*subagentHardStepFactor {
		t.Errorf("hardMaxSteps = %d, want %d", b.hardMaxSteps, 25*subagentHardStepFactor)
	}
}

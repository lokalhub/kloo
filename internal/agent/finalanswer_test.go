package agent

import (
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
)

func TestSalvageAnswerTakesTheLastSubstantiveProse(t *testing.T) {
	convo := []llm.Message{
		{Role: llm.RoleUser, Content: "the task"},
		{Role: llm.RoleAssistant, Content: "an early thought"},
		{Role: llm.RoleUser, Content: "a tool observation"},
		{Role: llm.RoleAssistant, Content: "the latest finding"},
		{Role: llm.RoleAssistant, Content: "   "}, // blank is skipped, not returned
	}
	if got := salvageAnswer(convo); got != "the latest finding" {
		t.Errorf("salvageAnswer = %q, want the latest substantive prose", got)
	}
}

func TestSalvageAnswerEmptyWhenNoProse(t *testing.T) {
	if got := salvageAnswer(nil); got != "" {
		t.Errorf("nil convo = %q, want empty", got)
	}
	convo := []llm.Message{{Role: llm.RoleUser, Content: "only user turns"}}
	if got := salvageAnswer(convo); got != "" {
		t.Errorf("no assistant prose = %q, want empty", got)
	}
}

// Success already has a summary and a green verify. ReasonError is excluded
// because the endpoint is usually WHY the run stopped — a dead host or a 503 at
// capacity — and asking again buys another timeout and reports a second error on
// top of the first.
func TestAnswerableStopExcludesSuccessAndError(t *testing.T) {
	for _, r := range []Reason{ReasonBudgetExceeded, ReasonExploreStop, ReasonChurn, ReasonUnverified} {
		if !answerableStop(r) {
			t.Errorf("%s should get a closing answer", r)
		}
	}
	for _, r := range []Reason{ReasonSuccess, ReasonError} {
		if answerableStop(r) {
			t.Errorf("%s must NOT trigger a closing call", r)
		}
	}
}

// THE BUDGET MUST STAY TRUTHFUL. --max-tokens 600000 has to mean 600000 INCLUDING
// the closing answer, not 600000 plus whatever the reply cost.
func TestBudgetReservesRoomForTheClosingAnswer(t *testing.T) {
	b := NewBudget(config.Config{MaxTokens: 100_000}, time.Now)
	b.AddTokens(100_000 - FinalAnswerReserve - 1)
	if over, _ := b.Check(); over {
		t.Fatal("tripped before the reserve boundary")
	}
	b.AddTokens(2)
	over, kind := b.Check()
	if !over || kind != BudgetTokens {
		t.Fatalf("over=%v kind=%v, want the token budget to trip a reserve EARLY", over, kind)
	}
}

// A small ceiling must not be consumed entirely by its own reserve.
func TestBudgetReserveNeverExceedsHalfTheCeiling(t *testing.T) {
	b := NewBudget(config.Config{MaxTokens: 1000}, time.Now)
	b.AddTokens(400)
	if over, _ := b.Check(); over {
		t.Fatal("a 1000-token ceiling tripped at 400 — the reserve ate the budget")
	}
}

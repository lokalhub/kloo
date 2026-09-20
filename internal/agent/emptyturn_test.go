package agent

import "testing"

// TestEmptyTurnRecoveryIsBounded: a model that returns nothing forever must still
// end the run. The recovery exists to save a run from ONE transport hiccup, not to
// spin against a broken endpoint.
func TestEmptyTurnRecoveryIsBounded(t *testing.T) {
	if maxEmptyTurnRecoveries <= 0 || maxEmptyTurnRecoveries > 5 {
		t.Fatalf("implausible empty-turn recovery budget: %d", maxEmptyTurnRecoveries)
	}
}

// TestEmptyTurnRecoveryIsOptIn: until the bench says otherwise, an exhausted empty
// completion ends the run exactly as it does today.
func TestEmptyTurnRecoveryIsOptIn(t *testing.T) {
	t.Setenv("KLOO_EMPTY_TURN_RECOVERY", "")
	if emptyTurnRecovery() {
		t.Fatal("recovery active with the flag off")
	}
	t.Setenv("KLOO_EMPTY_TURN_RECOVERY", "1")
	if !emptyTurnRecovery() {
		t.Fatal("flag not honoured")
	}
}

// TestEmptyContentStaysRetryable: the recovery is the LAST line of defence. The
// retry classifier must still treat an empty completion as a hiccup first, so a
// single blank response never reaches the recovery path at all.
func TestEmptyContentStaysRetryable(t *testing.T) {
	if !retryableLLMError(ErrNoUsableContent, nil) {
		t.Fatal("an empty completion is no longer retried")
	}
}

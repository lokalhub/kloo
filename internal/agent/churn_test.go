package agent

import (
	"fmt"
	"testing"
)

func failTurn(out string) Turn { return Turn{VerifyOutput: out} }

// editTurn carries a repeating edit but a DISTINCT verify output each call, so
// the edit-churn signal is isolated from the failure-churn signal.
var editTurnSeq int

func editTurn(edit string) Turn {
	editTurnSeq++
	return Turn{VerifyOutput: fmt.Sprintf("distinct failure %d", editTurnSeq), Edit: edit}
}

func TestChurnRepeatedFailureHalts(t *testing.T) {
	c := NewChurnDetector(3)
	// Repeated-failure churn only counts once the agent has edited: prime with one
	// edit (the agent tried, it didn't fix the failure), then the same failure
	// repeats while the agent stops editing — that's the no-progress the rail halts.
	c.Observe(Turn{Edit: "edit_file a.go", VerifyOutput: "FAIL: assertion x != y"})
	for i := 0; i < 2; i++ {
		c.Observe(failTurn("FAIL: assertion x != y"))
		if churned, _ := c.Check(); churned {
			t.Fatalf("churned too early at round %d", i)
		}
	}
	c.Observe(failTurn("FAIL: assertion x != y")) // 3rd identical failure since the edit
	churned, kind := c.Check()
	if !churned || kind != ChurnRepeatedFailure {
		t.Errorf("want repeated-failure churn, got churned=%v kind=%q", churned, kind)
	}
	if c.Artifact() == "" {
		t.Errorf("artifact should carry the repeated failure")
	}
}

// TestChurnNoEditNeverFailureChurns guards the fix for the "hello churns" bug:
// before the agent makes ANY edit, a persistently-failing verify (e.g. the default
// `go test` failing on a non-Go app while the agent only reads/explores) is the
// project baseline, NOT no-progress — so it must never trip repeated-failure churn.
func TestChurnNoEditNeverFailureChurns(t *testing.T) {
	c := NewChurnDetector(2)
	for i := 0; i < 10; i++ {
		c.Observe(failTurn("FAIL: go test ./... exit 1 (no Go files)"))
		if churned, _ := c.Check(); churned {
			t.Fatalf("read-only run churned at round %d without any edit", i)
		}
	}
}

func TestChurnRepeatedEditHalts(t *testing.T) {
	c := NewChurnDetector(2)
	c.Observe(editTurn("edit_file a.go\n<<<<<<< SEARCH\nfoo\n=======\nbar\n>>>>>>> REPLACE"))
	if churned, _ := c.Check(); churned {
		t.Fatal("churned after one edit")
	}
	c.Observe(editTurn("edit_file a.go\n<<<<<<< SEARCH\nfoo\n=======\nbar\n>>>>>>> REPLACE")) // same edit twice
	churned, kind := c.Check()
	if !churned || kind != ChurnRepeatedEdit {
		t.Errorf("want repeated-edit churn, got churned=%v kind=%q", churned, kind)
	}
}

func TestChurnDistinctFailureResets(t *testing.T) {
	c := NewChurnDetector(3)
	c.Observe(failTurn("FAIL: A"))
	c.Observe(failTurn("FAIL: A"))
	c.Observe(failTurn("FAIL: B")) // distinct → resets the run
	c.Observe(failTurn("FAIL: B"))
	if churned, _ := c.Check(); churned {
		t.Errorf("distinct failure should reset the counter, but churned")
	}
}

func TestChurnProgressResets(t *testing.T) {
	c := NewChurnDetector(2)
	c.Observe(failTurn("FAIL: A"))
	c.Observe(Turn{VerifyOutput: ""}) // a passing verify = progress → reset
	c.Observe(failTurn("FAIL: A"))
	if churned, _ := c.Check(); churned {
		t.Errorf("progress (passing verify) should reset failure churn")
	}
}

func TestChurnNormalisationIgnoresVolatileBits(t *testing.T) {
	c := NewChurnDetector(2)
	// Prime everEdited (the rail counts failures only after an edit), then two
	// failures differing only in duration, temp path, and a hex blob.
	c.Observe(Turn{Edit: "edit_file a.go", VerifyOutput: "FAIL in 9s at /tmp/go-build0/main_test.go object cafecafecafe"})
	c.Observe(failTurn("FAIL in 1.23s at /tmp/go-build123/main_test.go object deadbeefcafe0001"))
	c.Observe(failTurn("FAIL in 0.04s at /tmp/go-build999/main_test.go object 0011223344aaff"))
	churned, kind := c.Check()
	if !churned || kind != ChurnRepeatedFailure {
		t.Errorf("normalised-identical failures should churn, got churned=%v kind=%q", churned, kind)
	}
}

// TestChurnResetClearsState guards the cross-run leak: a reused detector (the TUI
// runs many tasks against one Loop) must start each run fresh. Without Reset, a
// second task inherited the prior run's failure streak and churned at step 1.
func TestChurnResetClearsState(t *testing.T) {
	c := NewChurnDetector(2)
	c.Observe(Turn{Edit: "edit_file a.go", VerifyOutput: "FAIL: x"})
	c.Observe(failTurn("FAIL: x"))
	c.Observe(failTurn("FAIL: x"))
	if churned, _ := c.Check(); !churned {
		t.Fatal("setup: expected churn before reset")
	}
	c.Reset()
	if churned, _ := c.Check(); churned {
		t.Error("Reset did not clear the churn state")
	}
	// And everEdited is cleared: a fresh read-only failure streak must not churn.
	for i := 0; i < 5; i++ {
		c.Observe(failTurn("FAIL: y"))
	}
	if churned, _ := c.Check(); churned {
		t.Error("after Reset, a read-only failure streak churned (everEdited not cleared)")
	}
}

func TestChurnDistinctEditResets(t *testing.T) {
	c := NewChurnDetector(2)
	c.Observe(editTurn("edit_file a.go\nfoo→bar"))
	c.Observe(editTurn("edit_file a.go\nbaz→qux")) // different edit → reset
	if churned, _ := c.Check(); churned {
		t.Errorf("distinct edit should reset edit churn")
	}
}

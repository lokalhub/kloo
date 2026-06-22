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
	for i := 0; i < 2; i++ {
		c.Observe(failTurn("FAIL: assertion x != y"))
		if churned, _ := c.Check(); churned {
			t.Fatalf("churned too early at round %d", i)
		}
	}
	c.Observe(failTurn("FAIL: assertion x != y")) // 3rd identical failure
	churned, kind := c.Check()
	if !churned || kind != ChurnRepeatedFailure {
		t.Errorf("want repeated-failure churn, got churned=%v kind=%q", churned, kind)
	}
	if c.Artifact() == "" {
		t.Errorf("artifact should carry the repeated failure")
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
	// Two failures differing only in duration, temp path, and a hex blob.
	c.Observe(failTurn("FAIL in 1.23s at /tmp/go-build123/main_test.go object deadbeefcafe0001"))
	c.Observe(failTurn("FAIL in 0.04s at /tmp/go-build999/main_test.go object 0011223344aaff"))
	churned, kind := c.Check()
	if !churned || kind != ChurnRepeatedFailure {
		t.Errorf("normalised-identical failures should churn, got churned=%v kind=%q", churned, kind)
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

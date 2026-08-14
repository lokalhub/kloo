package tokens

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures in testdata/ were sent verbatim to a real tokenizer
// (deepseek/deepseek-v4-flash via OpenRouter, max_tokens=1) and the token counts
// below are what the provider reported for them. They are ground truth, not
// guesses — if the estimator drifts, these tests say by how much.
type fixture struct {
	name       string
	file       string
	realTokens int
	// tolerance is the accepted signed relative error. Ordinary text is held
	// tight; high-entropy text is allowed more slack because no length-based
	// heuristic can fully model BPE on random data — that residual is what the
	// Calibrator and the usable-window reserve exist to absorb.
	tolerance float64
}

var fixtures = []fixture{
	{"go source", "go-source.txt", 2135, 0.08},
	{"prose", "prose.txt", 2210, 0.08},
	{"hashes", "hashes.txt", 4045, 0.30},
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// TestEstimateAgainstRealTokenCounts is the accuracy contract.
func TestEstimateAgainstRealTokenCounts(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			got := Estimate(readFixture(t, f.file))
			err := float64(got)/float64(f.realTokens) - 1
			t.Logf("%s: estimated %d, real %d (%+.1f%%)", f.name, got, f.realTokens, err*100)
			if math.Abs(err) > f.tolerance {
				t.Errorf("estimate %d vs real %d = %+.1f%%, outside ±%.0f%%",
					got, f.realTokens, err*100, f.tolerance*100)
			}
		})
	}
}

// TestEstimateBeatsFlatCharsPerFour is the reason this package exists: the old
// flat chars/4 undercounted high-entropy content by more than half, and a budget
// built on that overflows the server's real context window.
func TestEstimateBeatsFlatCharsPerFour(t *testing.T) {
	for _, f := range fixtures {
		s := readFixture(t, f.file)
		flat := (len([]rune(s)) + 3) / 4
		got := Estimate(s)
		flatErr := math.Abs(float64(flat)/float64(f.realTokens) - 1)
		gotErr := math.Abs(float64(got)/float64(f.realTokens) - 1)
		if gotErr > flatErr {
			t.Errorf("%s: new estimator is worse than chars/4 (%.1f%% vs %.1f%%)",
				f.name, gotErr*100, flatErr*100)
		}
	}
	// And specifically on hashes, where the old estimate was dangerous.
	s := readFixture(t, "hashes.txt")
	if flat := (len([]rune(s)) + 3) / 4; float64(flat) > float64(4045)*0.6 {
		t.Fatal("fixture no longer exercises the undercount this guards")
	}
}

// TestIsDenseDiscriminates pins the classifier. Getting this wrong in the other
// direction is not harmless: over-charging ordinary identifiers would shrink the
// usable prompt on every single turn.
func TestIsDenseDiscriminates(t *testing.T) {
	dense := []string{
		"h1:BCLxZm4YKzYcXkX8kLsFPZUEQ0lWm8fMEcqRRHnDbXo=", // base64
		"a3f5b2c891de04667788aabbccddeeff00112233",        // lowercase hex
		"550e8400-e29b-41d4-a716-446655440000",            // uuid
	}
	ordinary := []string{
		"File3Func2DoesSomething",                 // CamelCase with digits
		"NewGitCheckpointer",                      // CamelCase
		"github.com/lokalhub/kloo/internal/agent", // import path
		"maxContextTokens",                        // lowerCamel
		"v1.2.3",                                  // version
		"the",                                     // short word
	}
	for _, w := range dense {
		if !estimateIsDense(w) {
			t.Errorf("%q should be classified high-entropy", w)
		}
	}
	for _, w := range ordinary {
		if estimateIsDense(w) {
			t.Errorf("%q must NOT be classified high-entropy — over-charging ordinary code costs context every turn", w)
		}
	}
}

// estimateIsDense re-derives the per-word classification the estimator applies,
// so the test exercises the same rule rather than a copy of it.
func estimateIsDense(w string) bool {
	var transitions, digits int
	var prev int8 = -1
	for _, r := range w {
		c := classOf(r)
		if prev >= 0 && c != prev {
			transitions++
		}
		if c == classDigit {
			digits++
		}
		prev = c
	}
	return isDense(len([]rune(w)), transitions, digits)
}

// TestEstimateEdgeCases: empty is free, anything non-empty costs at least one.
func TestEstimateEdgeCases(t *testing.T) {
	if got := Estimate(""); got != 0 {
		t.Errorf("Estimate(\"\") = %d, want 0", got)
	}
	if got := Estimate("a"); got < 1 {
		t.Errorf("Estimate(\"a\") = %d, want at least 1", got)
	}
	if got := EstimateAt("hello world", 0); got < 1 {
		t.Errorf("a non-positive ratio should fall back to the default, got %d", got)
	}
}

// TestEstimateAtScalesWithRatio: a denser ratio must cost more tokens.
func TestEstimateAtScalesWithRatio(t *testing.T) {
	s := readFixture(t, "prose.txt")
	loose, tight := EstimateAt(s, 4.5), EstimateAt(s, 3.0)
	if tight <= loose {
		t.Errorf("a denser ratio should raise the estimate: 3.0 → %d, 4.5 → %d", tight, loose)
	}
}

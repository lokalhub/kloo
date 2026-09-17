package repomap

import "testing"

// TestDeprioritiseTestsMovesSourceAbove: with the option on, a test file must not
// outrank a non-test file of equal relevance. Measured on kloo-bench C07, 10 of
// the top 12 mapped files were tests and the file that had to change ranked 26th.
func TestDeprioritiseTestsMovesSourceAbove(t *testing.T) {
	files := []Node{
		{Path: "src/a.test.ts"}, {Path: "src/b.ts"},
		{Path: "src/__tests__/c.ts"}, {Path: "src/d.ts"},
	}
	in := RankInput{Files: files, Symbols: map[string][]Symbol{}, Task: "unrelated task"}

	off := Rank(in)
	if !IsTestPath(off[0].Path) {
		t.Fatalf("precondition: expected a TEST file to rank first with the option off, got %q", off[0].Path)
	}

	in.DeprioritiseTests = true
	on := Rank(in)
	for i, want := range []string{"src/b.ts", "src/d.ts", "src/__tests__/c.ts", "src/a.test.ts"} {
		if on[i].Path != want {
			t.Errorf("rank %d = %q, want %q", i, on[i].Path, want)
		}
	}
}

// TestDeprioritiseTestsKeepsRelevancePrimary: the option reorders WITHIN the
// ranking, it must not override task relevance — a highly relevant test still
// beats an irrelevant source file... no: relevance is primary only among files of
// the same kind. This pins the deliberate choice: kind is checked FIRST, because
// these tasks forbid editing tests at all.
func TestDeprioritiseTestsIsUnconditional(t *testing.T) {
	files := []Node{{Path: "src/widget.test.ts"}, {Path: "src/unrelated.ts"}}
	in := RankInput{
		Files:   files,
		Symbols: map[string][]Symbol{"src/widget.test.ts": {{File: "src/widget.test.ts", Name: "widget"}}},
		Task:    "fix the widget",
	}
	if got := Rank(in)[0].Path; got != "src/widget.test.ts" {
		t.Fatalf("precondition: relevant test should rank first with the option off, got %q", got)
	}
	in.DeprioritiseTests = true
	if got := Rank(in)[0].Path; got != "src/unrelated.ts" {
		t.Errorf("with the option on, rank 0 = %q, want the non-test file", got)
	}
}

// TestIsTestPathDoesNotMisclassifySource guards the conservative matcher: a source
// file wrongly treated as a test would be pushed down and hidden.
func TestIsTestPathDoesNotMisclassifySource(t *testing.T) {
	for _, p := range []string{"src/latest.ts", "src/contest/service.ts", "src/protester.go", "src/testimony.ts", "src/attest.ts"} {
		if IsTestPath(p) {
			t.Errorf("IsTestPath(%q) = true, want false", p)
		}
	}
	for _, p := range []string{"a.test.ts", "a.spec.js", "pkg/a_test.go", "src/__tests__/a.ts", "tests/a.ts", "tests/qa/deep/a.ts"} {
		if !IsTestPath(p) {
			t.Errorf("IsTestPath(%q) = false, want true", p)
		}
	}
}

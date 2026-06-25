package tui

import (
	"strings"
	"testing"
)

// TestHunkLinesMinimalDiff: a SEARCH/REPLACE block that changes one line within
// surrounding context renders as a minimal diff (context shown once, only the
// changed line as -/+), not the whole SEARCH as "-" + whole REPLACE as "+".
func TestHunkLinesMinimalDiff(t *testing.T) {
	got := hunkLines(editPair{search: "alpha\nbeta\ngamma\n", replace: "alpha\nBETA\ngamma\n"})
	if len(got) != 4 { // alpha (ctx), -beta, +BETA, gamma (ctx)
		t.Fatalf("want 4 lines (2 context + 1 del + 1 add), got %d: %q", len(got), got)
	}
	j := strings.Join(got, "\n")
	for _, w := range []string{"- beta", "+ BETA"} {
		if !strings.Contains(j, w) {
			t.Errorf("missing %q in:\n%s", w, j)
		}
	}
	// Unchanged context lines must NOT be rendered as a +/- change.
	for _, bad := range []string{"- alpha", "+ alpha", "- gamma", "+ gamma"} {
		if strings.Contains(j, bad) {
			t.Errorf("unchanged context shown as %q (should be context):\n%s", bad, j)
		}
	}
}

// TestHunkLinesNewFile: an empty SEARCH (new-file edit) is all additions.
func TestHunkLinesNewFile(t *testing.T) {
	got := hunkLines(editPair{search: "", replace: "one\ntwo\n"})
	if len(got) != 2 || !strings.Contains(got[0], "+ one") || !strings.Contains(got[1], "+ two") {
		t.Errorf("new-file edit should be all additions, got %q", got)
	}
}

// TestStripToolMarkup: trailing tool-call markup (DSML / <function=>) is removed
// from displayed prose; markup-free prose is untouched.
func TestStripToolMarkup(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Done.\nTo run: ionic serve<｜DSML｜tool_calls>\n<｜DSML｜invoke name=\"finish\">x", "Done.\nTo run: ionic serve"},
		{"plain prose, no markup", "plain prose, no markup"},
		{"x <function=finish><parameter=summary>y</parameter></function>", "x"},
		{"y <tool_call>{}</tool_call>", "y"},
	}
	for _, c := range cases {
		if got := stripToolMarkup(c.in); got != c.want {
			t.Errorf("stripToolMarkup(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

package cli

import (
	"strings"
	"testing"
)

// TestJoinNotices: startup notices must reach the TUI banner, because anything
// written to stderr before Bubble Tea starts is wiped before anyone reads it —
// which is how a model-validation warning came to be raised and never seen.
func TestJoinNotices(t *testing.T) {
	cases := []struct {
		name    string
		banner  string
		notices []string
		want    string
	}{
		{"nothing at all", "", nil, ""},
		{"banner only", "resumed session abc", nil, "resumed session abc"},
		{"notice only", "", []string{"kloo: ctx-auto · …"}, "kloo: ctx-auto · …"},
		{"both, banner first", "resumed session abc", []string{"kloo: ctx-auto · …"},
			"resumed session abc\nkloo: ctx-auto · …"},
		{"blank parts dropped", "  ", []string{"", "  ", "real notice"}, "real notice"},
		{"multiple notices", "", []string{"one", "two"}, "one\ntwo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinNotices(tc.banner, tc.notices); got != tc.want {
				t.Errorf("joinNotices(%q, %v) = %q, want %q", tc.banner, tc.notices, got, tc.want)
			}
		})
	}
}

// TestJoinNoticesKeepsCleanLaunchUnchanged: a launch with nothing to say must
// render exactly as it did before, so the common path gains no noise.
func TestJoinNoticesKeepsCleanLaunchUnchanged(t *testing.T) {
	if got := joinNotices("", []string{}); got != "" {
		t.Errorf("a clean launch should produce an empty banner, got %q", got)
	}
	if got := joinNotices("", nil); strings.TrimSpace(got) != "" {
		t.Errorf("a nil notice slice should produce an empty banner, got %q", got)
	}
}

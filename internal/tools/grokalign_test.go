package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLineAnchorsMatchGrokScheme: with KLOO_LINE_ANCHORS the first line and every
// tenth line carry a "N→" prefix and nothing else does — grok's captured scheme.
// The sparse form matters: a prefix on every line would be a prefix on every line
// the edit tool then has to match.
func TestLineAnchorsMatchGrokScheme(t *testing.T) {
	var lines []string
	for i := 1; i <= 12; i++ {
		lines = append(lines, "line")
	}
	got := anchorLines(strings.Join(lines, "\n"), 1)
	out := strings.Split(got, "\n")
	if out[0] != "1→line" {
		t.Fatalf("first line not anchored: %q", out[0])
	}
	if out[9] != "10→line" {
		t.Fatalf("tenth line not anchored: %q", out[9])
	}
	if out[1] != "line" {
		t.Fatalf("second line should carry content only: %q", out[1])
	}
}

// TestLineAnchorsCountFromOffset: a paged read must number from the offset, or the
// anchors point at the wrong lines — worse than no anchors at all.
func TestLineAnchorsCountFromOffset(t *testing.T) {
	got := anchorLines("a\nb", 41)
	if !strings.HasPrefix(got, "41→a") {
		t.Fatalf("offset ignored: %q", got)
	}
}

// TestAnchorNoteOnlyWhenAnchorsOn: the warning is about a prefix that is only
// present when the flag is on; shipping it unconditionally would describe output
// the model never sees.
func TestAnchorNoteOnlyWhenAnchorsOn(t *testing.T) {
	t.Setenv("KLOO_LINE_ANCHORS", "")
	if anchorNote() != "" {
		t.Fatal("note present with anchors off")
	}
	t.Setenv("KLOO_LINE_ANCHORS", "1")
	if !strings.Contains(anchorNote(), "NOT part of the file") {
		t.Fatal("note missing with anchors on")
	}
}

// TestTightSearchBoundsOutput: KLOO_TIGHT_SEARCH must actually shrink what one
// search call can put in the window. 64 KiB is ~16k tokens, a third of the hot
// budget at ctx 131072.
func TestTightSearchBoundsOutput(t *testing.T) {
	t.Setenv("KLOO_TIGHT_SEARCH", "")
	m, o, _ := searchBounds()
	if m != searchMaxMatches || o != searchMaxOutput {
		t.Fatalf("default bounds changed: %d %d", m, o)
	}
	t.Setenv("KLOO_TIGHT_SEARCH", "1")
	m, o, _ = searchBounds()
	if m != tightSearchMaxMatches || o != tightSearchMaxOutput {
		t.Fatalf("tight bounds not applied: %d %d", m, o)
	}
	if o >= searchMaxOutput {
		t.Fatal("tight output bound is not tighter")
	}
}

// TestSearchHeadLimitCapsMatches: head_limit is grok's knob; it must cap this call
// without letting the model raise the standing bound.
func TestSearchHeadLimitCapsMatches(t *testing.T) {
	ws, root := newWS(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte(strings.Repeat("needle\n", 50)), 0o644); err != nil {
		t.Fatal(err)
	}
	st := searchTool{ws}
	res, err := st.Invoke(context.Background(), Call{Args: map[string]any{"query": "needle", "head_limit": 3}})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(res.Output, "a.txt:"); n != 3 {
		t.Fatalf("head_limit ignored: %d match lines", n)
	}
}

// TestEditOnlyViewAdvertisesOnlyEdits: the point of the view is that a reading
// model has nothing else to call. It must still DISPATCH the withheld tools, so a
// model that calls one anyway gets its real result rather than an unknown-tool
// error it cannot recover from.
func TestEditOnlyViewAdvertisesOnlyEdits(t *testing.T) {
	ws, _ := newWS(t)
	reg := DefaultRegistry(ws)
	view := reg.EditOnlyView(true)
	for _, tl := range view.Tools() {
		switch tl.Name() {
		case NameEditFile, NameWriteFile, "search_replace", NameFinish:
		default:
			t.Fatalf("edit-only view advertises %q", tl.Name())
		}
	}
	if len(view.Tools()) == 0 {
		t.Fatal("edit-only view advertises nothing")
	}
	if _, ok := view.Lookup(NameReadFile); !ok {
		t.Fatal("withheld tool is no longer dispatchable")
	}
	// A red verify must not leave finish on the list: it ends the run as a failure,
	// which is a one-call escape from the rail.
	for _, tl := range reg.EditOnlyView(false).Tools() {
		if tl.Name() == NameFinish {
			t.Fatal("finish offered on a forced-edit turn with a red verify")
		}
	}
}

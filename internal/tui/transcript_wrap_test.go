package tui

import (
	"strings"
	"testing"
)

// TestWrapLinesBreaksOnWords: the wrap must not saw a word in half, must not drop
// or duplicate a character, and must keep every line inside the width.
func TestWrapLinesBreaksOnWords(t *testing.T) {
	const text = "kloo was repeating itself without making progress, so it stopped. A more specific instruction usually unsticks it."
	for _, w := range []int{20, 31, 40, 72, 80} {
		got := wrapLines(text, w)
		for _, line := range strings.Split(got, "\n") {
			if n := len([]rune(line)); n > w {
				t.Errorf("width %d: line of %d runes: %q", w, n, line)
			}
		}
		// No word may be split: rejoining the lines with a space must give the words
		// back exactly.
		if a, b := strings.Join(strings.Fields(got), " "), strings.Join(strings.Fields(text), " "); a != b {
			t.Errorf("width %d: words changed\n got: %s\nwant: %s", w, a, b)
		}
	}
}

// TestWrapLinesHardSplitsAnUnbreakableWord: a path or URL longer than the line has
// nowhere to break, so it is split rather than overflowing — and still loses nothing.
func TestWrapLinesHardSplitsAnUnbreakableWord(t *testing.T) {
	long := strings.Repeat("a", 45) + "/" + strings.Repeat("b", 30)
	got := wrapLines("see "+long+" now", 20)
	for _, line := range strings.Split(got, "\n") {
		if n := len([]rune(line)); n > 20 {
			t.Errorf("line of %d runes: %q", n, line)
		}
	}
	if flat := strings.ReplaceAll(strings.ReplaceAll(got, "\n", ""), " ", ""); !strings.Contains(flat, long) {
		t.Errorf("the long token did not survive the split: %q", got)
	}
}

// TestWrapLinesKeepsNewlinesAndIndentation: existing line breaks are structure and
// so is leading whitespace (indented code in an answer).
func TestWrapLinesKeepsNewlinesAndIndentation(t *testing.T) {
	got := wrapLines("first line\n    indented second", 40)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 || lines[0] != "first line" {
		t.Fatalf("newlines not preserved: %q", got)
	}
	if !strings.HasPrefix(lines[1], "    indented") {
		t.Errorf("leading indentation lost: %q", lines[1])
	}
}

// TestHangingNoteIndentsContinuations: a wrapped note reads as one note, with the
// continuation under the text rather than under the ■.
func TestHangingNoteIndentsContinuations(t *testing.T) {
	got := hangingNote("■ ", "kloo stopped itself before finishing this task. Type continue to pick up where it left off.", 40)
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected the note to wrap: %q", got)
	}
	if !strings.HasPrefix(lines[0], "■ ") {
		t.Errorf("first line should carry the marker: %q", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "  ") {
			t.Errorf("continuation should be indented under the text: %q", l)
		}
	}
}

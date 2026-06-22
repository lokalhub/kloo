package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
)

// hasHunkSeparator reports whether the frame contains a diff hunk-separator line:
// a card *content* line (bordered by │, not the ┌/└ box edges) whose interior is
// all `─`. This distinguishes the inter-hunk rule from the card's own border.
func hasHunkSeparator(frame string) bool {
	for _, line := range strings.Split(frame, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "│") && strings.HasSuffix(t, "│") {
			inner := strings.TrimSpace(strings.Trim(t, "│"))
			if len(inner) >= 5 && strings.Trim(inner, "─") == "" {
				return true
			}
		}
	}
	return false
}

// tall returns a model sized tall enough that several cards fit the viewport.
func tall() Model {
	return sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, 44)
}

// TestToolCardsAccentGlyphChip (task 01): the four tool kinds each render a
// distinct leading glyph + label chip + dim secondary text. Under ascii the
// glyphs/labels are the intent carriers.
func TestToolCardsAccentGlyphChip(t *testing.T) {
	m := apply(tall(),
		toolEventMsg{Name: "edit_file", Path: "src/app/app.ts", Search: "old", Replace: "new"},
		toolEventMsg{Name: "run_command", Command: "npm run build", ExitCode: 0},
		toolEventMsg{Name: "read_file", Summary: "src/app/app.component.ts (88 lines)"},
		toolEventMsg{Name: "verify", Summary: "npm test    passed"},
	)
	v := m.View()
	for _, want := range []string{
		"✎ src/app/app.ts",
		"⌘ run_command", "npm run build",
		"👁 read_file", "src/app/app.component.ts (88 lines)",
		"✓ verify", "npm test    passed",
	} {
		if !contains(v, want) {
			t.Errorf("tool-cards frame missing %q:\n%s", want, v)
		}
	}
	requireGolden(t, "tool-cards-all.golden", v)
}

// TestUnknownToolFallsBackToBullet (task 01): an unrecognised tool gets the
// neutral • chip, not a panic or a missing glyph.
func TestUnknownToolFallsBackToBullet(t *testing.T) {
	v := apply(tall(), toolEventMsg{Name: "list_dir", Summary: "internal/ (12 entries)"}).View()
	if !contains(v, "• list_dir") {
		t.Errorf("unknown tool should fall back to a • chip:\n%s", v)
	}
}

// TestDiffCardHunkSeparator (task 02): two edit pairs are separated by a dim
// rule; the ✎ <path> header is present; no separator before the first hunk.
func TestDiffCardHunkSeparator(t *testing.T) {
	ec := editCardItem{
		path: "src/app/tabs/tabs.routes.ts",
		edits: []editPair{
			{search: "{ path: 'tab1' }", replace: "{ path: 'home' }"},
			{search: "{ path: 'tab2' }", replace: "{ path: 'apps' }"},
		},
	}
	v := apply(tall(), toolEventMsg{Name: "edit_file", Path: ec.path, Edits: ec.edits}).View()
	if !contains(v, "✎ src/app/tabs/tabs.routes.ts") {
		t.Errorf("diff card missing ✎ header:\n%s", v)
	}
	if !hasHunkSeparator(v) {
		t.Errorf("diff card missing a hunk separator rule:\n%s", v)
	}
	for _, want := range []string{"- { path: 'tab1' }", "+ { path: 'home' }", "- { path: 'tab2' }", "+ { path: 'apps' }"} {
		if !contains(v, want) {
			t.Errorf("diff card missing %q:\n%s", want, v)
		}
	}
	requireGolden(t, "diff-card-hunks.golden", v)
}

// TestDiffCardNewFileNoSeparator (task 02): a single new-file hunk renders all
// `+` lines with no spurious leading separator and no raw fence.
func TestDiffCardNewFileNoSeparator(t *testing.T) {
	v := apply(tall(), toolEventMsg{Name: "edit_file", Path: "home.ts", Search: "", Replace: "export const Home = 1;"}).View()
	if !contains(v, "+ export const Home = 1;") {
		t.Errorf("new-file diff should render a + line:\n%s", v)
	}
	if hasHunkSeparator(v) {
		t.Errorf("single-hunk new file should have no separator:\n%s", v)
	}
	for _, raw := range []string{"<<<<<<<", "=======", ">>>>>>>"} {
		if contains(v, raw) {
			t.Errorf("raw fence marker %q leaked:\n%s", raw, v)
		}
	}
}

// longFailCard is a run_command failure whose stderr exceeds the truncation cap.
func longFailCard() toolEventMsg {
	return toolEventMsg{
		Name: "run_command", Command: "npm run build", ExitCode: 1,
		Stderr: "ERROR in src/app/tab1/tab1.page.ts:12:5\nTS2304: Cannot find name 'Hom'.\nline-three\nline-four\nline-five\nline-six\nline-seven\nline-eight",
	}
}

// TestRunOutputTruncated (task 03): long output truncates to runBodyCap lines and
// shows the `… +K more lines  ctrl+o to expand` affordance; later lines hidden.
func TestRunOutputTruncated(t *testing.T) {
	v := apply(tall(), longFailCard()).View()
	if !contains(v, "+4 more lines") || !contains(v, "ctrl+o to expand") {
		t.Errorf("truncated card missing the expand affordance:\n%s", v)
	}
	if contains(v, "line-eight") {
		t.Errorf("the 8th stderr line must be hidden when truncated:\n%s", v)
	}
	requireGolden(t, "cmd-output-truncated.golden", v)
}

// TestRunOutputExpanded (task 03): after ctrl+o the full body shows and the hint
// disappears.
func TestRunOutputExpanded(t *testing.T) {
	v := apply(tall(), longFailCard(), tea.KeyMsg{Type: tea.KeyCtrlO}).View()
	if contains(v, "ctrl+o to expand") {
		t.Errorf("expanded card should hide the expand hint:\n%s", v)
	}
	if !contains(v, "line-eight") {
		t.Errorf("expanded card should show all stderr lines:\n%s", v)
	}
	requireGolden(t, "cmd-output-expanded.golden", v)
}

// TestCtrlOTogglesExpand (task 03): teatest drives ctrl+o and proves the frame
// toggles truncated ↔ full in both directions.
func TestCtrlOTogglesExpand(t *testing.T) {
	tm := teatest.NewTestModel(t, New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}),
		teatest.WithInitialTermSize(tw, 44))
	tm.Send(longFailCard())

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return contains(string(b), "ctrl+o to expand")
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO}) // expand
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return contains(string(b), "line-eight")
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO}) // collapse again
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return contains(string(b), "ctrl+o to expand")
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC}) // idle quit
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}

// TestVerifyColourReportLine (task 05): the stop-report `last verify` line shows
// the coloured ✓ (pass) / ✗ (fail) glyph at the report site.
func TestVerifyColourReportLine(t *testing.T) {
	pass := apply(tall(), reportMsg{Reason: "success", Steps: 1, VerifyCmd: "npm run build", VerifyExit: 0}).View()
	if !contains(pass, "last verify: npm run build → exit 0 ✓") {
		t.Errorf("report pass verify line missing coloured ✓:\n%s", pass)
	}
	fail := apply(tall(), reportMsg{Reason: "error", Steps: 1, VerifyCmd: "npm run build", VerifyExit: 1}).View()
	if !contains(fail, "last verify: npm run build → exit 1 ✗") {
		t.Errorf("report fail verify line missing coloured ✗:\n%s", fail)
	}
}

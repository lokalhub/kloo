package tui

import (
	"testing"

	"github.com/lokal/kloo/internal/tools"
)

func newSized() Model {
	return sized(New(Config{Model: "snappy", MaxSteps: 40, MaxTokens: 8000}), tw, th)
}

// editFileDiff is a real edit_file `diff` arg (a bare fenced SEARCH/REPLACE
// block), as the loop produces it.
const editFileDiff = "```\n<<<<<<< SEARCH\n{ path: 'tab1', loadComponent: ... }\n=======\n{ path: 'home', loadComponent: ... }\n>>>>>>> REPLACE\n```"

// TestCardEditDiff drives the REAL bridge path: a raw fenced `diff` arg goes
// through toolEvent() (edit.Parse), so the golden reflects the actual loop data
// — SEARCH as `-`, REPLACE as `+`, and NO raw fence/markers in the card.
func TestCardEditDiff(t *testing.T) {
	msg := toolEvent(tools.Call{Name: "edit_file", Args: map[string]any{
		"path": "src/app/tabs/tabs.routes.ts",
		"diff": editFileDiff,
	}}, tools.Result{})

	m := apply(newSized(), msg)
	v := m.View()
	if !contains(v, "✎ src/app/tabs/tabs.routes.ts") {
		t.Errorf("edit card ✎ <path> header missing:\n%s", v)
	}
	if !contains(v, "- { path: 'tab1'") || !contains(v, "+ { path: 'home'") {
		t.Errorf("diff -/+ lines missing:\n%s", v)
	}
	// The raw fence/markers must NOT leak into the card.
	for _, raw := range []string{"<<<<<<< SEARCH", "=======", ">>>>>>> REPLACE", "```"} {
		if contains(v, raw) {
			t.Errorf("raw diff marker %q leaked into the rendered card:\n%s", raw, v)
		}
	}
	requireGolden(t, "card-edit.golden", v)
}

// TestCardEditDiffNewFile: an empty-SEARCH (new-file) diff renders only `+`
// lines (no `-`), through the real bridge parse.
func TestCardEditDiffNewFile(t *testing.T) {
	diff := "```\n<<<<<<< SEARCH\n=======\nexport const Home = 1;\n>>>>>>> REPLACE\n```"
	msg := toolEvent(tools.Call{Name: "edit_file", Args: map[string]any{"path": "home.ts", "diff": diff}}, tools.Result{})
	v := apply(newSized(), msg).View()
	if !contains(v, "+ export const Home = 1;") {
		t.Errorf("new-file edit should render the content as a + line:\n%s", v)
	}
	if contains(v, "- ") {
		t.Errorf("empty-SEARCH new file should have no - lines:\n%s", v)
	}
	if contains(v, "<<<<<<<") || contains(v, "=======") {
		t.Errorf("raw markers leaked into the new-file card:\n%s", v)
	}
}

func TestCardRunOK(t *testing.T) {
	m := apply(newSized(), toolEventMsg{Name: "run_command", Command: "npm run build", ExitCode: 0})
	v := m.View()
	if !contains(v, "⌘ run_command") || !contains(v, "npm run build") || !contains(v, "exit 0 ✓") {
		t.Errorf("run-ok card missing fields:\n%s", v)
	}
	requireGolden(t, "card-run-ok.golden", v)
}

func TestCardRunFailVisiblyDistinct(t *testing.T) {
	m := apply(newSized(), toolEventMsg{
		Name:     "run_command",
		Command:  "npm run build",
		ExitCode: 1,
		Stderr:   "ERROR: 'tab1' is not a known element\nsrc/app/tabs/tabs.page.html:7:5 — error NG8001",
	})
	v := m.View()
	// Visibly distinct from success: exit N ✗ + captured stderr lines.
	if !contains(v, "exit 1 ✗") {
		t.Errorf("failure card should show `exit 1 ✗`:\n%s", v)
	}
	if !contains(v, "NG8001") {
		t.Errorf("failure card should show captured stderr lines:\n%s", v)
	}
	if contains(v, "exit 1 ✓") {
		t.Errorf("failure card must not show the success marker")
	}
	requireGolden(t, "card-run-fail.golden", v)
}

func TestCardGenericOneLine(t *testing.T) {
	m := apply(newSized(), toolEventMsg{Name: "read_file", Summary: "internal/app.go (42 lines)"})
	v := m.View()
	if !contains(v, "👁 read_file") || !contains(v, "internal/app.go") {
		t.Errorf("generic card missing:\n%s", v)
	}
}

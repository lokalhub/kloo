package tui

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Frame goldens are produced from Model.View() after applying messages directly
// through Update — deterministic and fast — while teatest drives the runtime
// behaviour (launch/quit/keys) in the per-task tests (decisions.md). TestMain
// forces the ascii colour profile so goldens carry no ANSI escapes.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.Ascii)
	os.Exit(m.Run())
}

func init() {
	if flag.Lookup("update") == nil {
		flag.Bool("update", false, "update golden files")
	}
}

func goldenUpdate() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

// requireGolden compares got against testdata/<name>, or rewrites it under -update.
func requireGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if goldenUpdate() {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run `go test ./internal/tui -update` to create): %v", name, err)
	}
	if got != string(want) {
		t.Errorf("golden %s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, string(want))
	}
}

// apply feeds messages through Update sequentially and returns the final model.
func apply(m Model, msgs ...tea.Msg) Model {
	var tm tea.Model = m
	for _, msg := range msgs {
		tm, _ = tm.Update(msg)
	}
	return tm.(Model)
}

// sized returns a model sized to w×h (a WindowSizeMsg applied).
func sized(m Model, w, h int) Model {
	return apply(m, tea.WindowSizeMsg{Width: w, Height: h})
}

const tw, th = 90, 24 // standard test terminal size

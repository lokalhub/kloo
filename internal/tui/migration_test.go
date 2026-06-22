package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoStrayColourLiterals walks every Go source file in internal/tui and fails
// if a lipgloss colour-constructor call appears anywhere except theme.go. This
// makes the Phase 01 centralization a permanent, automated guard: a future change
// (Phase 02 or later) that re-scatters a colour literal outside the palette fails
// CI here, not just a manual grep. It is the Go-test form of the phase's
// "colour literals only in theme.go" acceptance. (The needle is assembled from
// parts at runtime so this guard file does not trip on its own source.)
func TestNoStrayColourLiterals(t *testing.T) {
	// Built from parts so this guard file does not match its own needle.
	needle := "lipgloss.Color" + "("
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || name == "theme.go" {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		checked++
		if strings.Contains(string(b), needle) {
			t.Errorf("%s contains a stray %q — all colour literals must live in theme.go", name, needle)
		}
	}
	if checked == 0 {
		t.Fatal("walked zero source files — guard is not actually checking anything")
	}
	// Sanity: the palette file itself does define colour literals (the one place).
	pal, err := os.ReadFile("theme.go")
	if err != nil || !strings.Contains(string(pal), needle) {
		t.Errorf("theme.go should be the home of every colour literal")
	}
}

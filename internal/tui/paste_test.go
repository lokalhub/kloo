package tui

import (
	"strings"
	"testing"
)

func TestHandlePaste(t *testing.T) {
	// Short single-line paste is NOT collapsed — the input inserts it as-is.
	if _, handled := newSized().handlePaste("a short line"); handled {
		t.Errorf("short single-line paste should not collapse to a placeholder")
	}

	// Long/multi-line paste collapses to a placeholder; the full text is stashed
	// and expandPastes restores it for the model.
	long := strings.Repeat("x", 50) + "\n" + strings.Repeat("y", 50) + "\nthird line"
	m, handled := newSized().handlePaste(long)
	if !handled {
		t.Fatal("multi-line paste should collapse to a placeholder")
	}
	if len(m.pastes) != 1 || m.pastes[0].full != long {
		t.Fatalf("full paste not stashed: %+v", m.pastes)
	}
	if !strings.Contains(m.input.Value(), "[#1 pasted 3 lines") {
		t.Errorf("placeholder not inserted into input: %q", m.input.Value())
	}
	if got := m.expandPastes(m.input.Value()); got != long {
		t.Errorf("expandPastes did not restore the full text:\n got %q\nwant %q", got, long)
	}
}

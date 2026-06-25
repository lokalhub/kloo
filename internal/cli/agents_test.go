package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentsInstructions(t *testing.T) {
	// none → empty
	if s := agentsInstructions(t.TempDir(), nil); s != "" {
		t.Errorf("no file should give empty, got %q", s)
	}

	// root AGENTS.md → included; a CLAUDE.md is ignored entirely (AGENTS.md only).
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Use tabs. Run npm test.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude-only\n"), 0o644)
	s := agentsInstructions(dir, nil)
	if !strings.Contains(s, "Use tabs. Run npm test.") {
		t.Errorf("root AGENTS.md not applied: %q", s)
	}
	if strings.Contains(s, "claude-only") {
		t.Error("CLAUDE.md must be ignored — AGENTS.md only")
	}

	// a CLAUDE.md alone (no AGENTS.md) → nothing.
	dirC := t.TempDir()
	os.WriteFile(filepath.Join(dirC, "CLAUDE.md"), []byte("claude rules\n"), 0o644)
	if s := agentsInstructions(dirC, nil); s != "" {
		t.Errorf("CLAUDE.md alone must NOT be loaded, got %q", s)
	}

	// AGENTS.md in a SUBDIR (project-in-a-subdir layout) is discovered + labelled,
	// and a skipped dir's AGENTS.md is ignored.
	dir2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir2, "myApp"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir2, "myApp", "AGENTS.md"), []byte("Ionic app rules: prefix services.\n"), 0o644)
	os.MkdirAll(filepath.Join(dir2, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(dir2, "node_modules", "x", "AGENTS.md"), []byte("DO NOT READ\n"), 0o644)
	s2 := agentsInstructions(dir2, nil)
	if !strings.Contains(s2, "Ionic app rules") || !strings.Contains(s2, "myApp/AGENTS.md") {
		t.Errorf("subdir AGENTS.md not discovered/labelled: %q", s2)
	}
	if strings.Contains(s2, "DO NOT READ") {
		t.Error("node_modules AGENTS.md must be skipped")
	}
}

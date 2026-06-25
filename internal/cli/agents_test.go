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

	// root AGENTS.md → included; AGENTS.md preferred over CLAUDE.md in the same dir
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Use tabs. Run npm test.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude-only\n"), 0o644)
	s := agentsInstructions(dir, nil)
	if !strings.Contains(s, "Use tabs. Run npm test.") {
		t.Errorf("root AGENTS.md not applied: %q", s)
	}
	if strings.Contains(s, "claude-only") {
		t.Error("AGENTS.md should win over CLAUDE.md in the same dir")
	}

	// AGENTS.md in a SUBDIR (project-in-a-subdir layout) is discovered + labelled.
	dir2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir2, "myApp"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir2, "myApp", "AGENTS.md"), []byte("Ionic app rules: prefix services.\n"), 0o644)
	// and a skipped dir's AGENTS.md must be ignored
	os.MkdirAll(filepath.Join(dir2, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(dir2, "node_modules", "x", "AGENTS.md"), []byte("DO NOT READ\n"), 0o644)
	s2 := agentsInstructions(dir2, nil)
	if !strings.Contains(s2, "Ionic app rules") || !strings.Contains(s2, "myApp/AGENTS.md") {
		t.Errorf("subdir AGENTS.md not discovered/labelled: %q", s2)
	}
	if strings.Contains(s2, "DO NOT READ") {
		t.Error("node_modules AGENTS.md must be skipped")
	}

	// CLAUDE.md fallback when no AGENTS.md
	dir3 := t.TempDir()
	os.WriteFile(filepath.Join(dir3, "CLAUDE.md"), []byte("claude rules\n"), 0o644)
	if s := agentsInstructions(dir3, nil); !strings.Contains(s, "claude rules") {
		t.Errorf("CLAUDE.md fallback failed: %q", s)
	}
}

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentsInstructions(t *testing.T) {
	// none → empty
	if s := agentsInstructions(t.TempDir(), nil, 0, nil); s != "" {
		t.Errorf("no file should give empty, got %q", s)
	}

	// root AGENTS.md → included; a CLAUDE.md is ignored entirely (AGENTS.md only).
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Use tabs. Run npm test.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude-only\n"), 0o644)
	s := agentsInstructions(dir, nil, 0, nil)
	if !strings.Contains(s, "Use tabs. Run npm test.") {
		t.Errorf("root AGENTS.md not applied: %q", s)
	}
	if strings.Contains(s, "claude-only") {
		t.Error("CLAUDE.md must be ignored — AGENTS.md only")
	}

	// a CLAUDE.md alone (no AGENTS.md) → nothing.
	dirC := t.TempDir()
	os.WriteFile(filepath.Join(dirC, "CLAUDE.md"), []byte("claude rules\n"), 0o644)
	if s := agentsInstructions(dirC, nil, 0, nil); s != "" {
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
	s2 := agentsInstructions(dir2, nil, 0, nil)
	if !strings.Contains(s2, "Ionic app rules") || !strings.Contains(s2, "myApp/AGENTS.md") {
		t.Errorf("subdir AGENTS.md not discovered/labelled: %q", s2)
	}
	if strings.Contains(s2, "DO NOT READ") {
		t.Error("node_modules AGENTS.md must be skipped")
	}
}

// An @import inside the workspace is expanded in place (its content pinned), and
// the directive line itself is replaced by the imported body.
func TestAgentsImportInsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "conventions"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "conventions", "ui.md"), []byte("UI rule: no inline styles.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Project rules.\n@import conventions/ui.md\nEnd.\n"), 0o644)

	s := agentsInstructions(dir, nil, 0, nil)
	if !strings.Contains(s, "UI rule: no inline styles.") {
		t.Errorf("imported file content not inlined: %q", s)
	}
	if !strings.Contains(s, "imported: conventions/ui.md") {
		t.Errorf("import not labelled: %q", s)
	}
	if strings.Contains(s, "@import conventions/ui.md") {
		t.Errorf("raw @import directive should be replaced, not left literal: %q", s)
	}
}

// An @import pointing OUTSIDE the workspace is blocked by default and only read
// when its directory is whitelisted via --allowed-dirs (cfg.AllowedImportDirs).
func TestAgentsImportOutsideWorkspaceNeedsAllowedDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // a sibling temp dir, NOT under root
	os.WriteFile(filepath.Join(outside, "shared.md"), []byte("Shared convention X.\n"), 0o644)
	imp := "@import " + filepath.Join(outside, "shared.md") + "\n"
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Rules.\n"+imp), 0o644)

	// Default: outside the jail and not allowed → skipped.
	if s := agentsInstructions(root, nil, 0, nil); strings.Contains(s, "Shared convention X.") {
		t.Errorf("outside-workspace import must be blocked without --allowed-dirs: %q", s)
	}
	// Whitelisted: the outside dir is permitted → imported.
	s := agentsInstructions(root, []string{outside}, 0, nil)
	if !strings.Contains(s, "Shared convention X.") {
		t.Errorf("outside import should be read when its dir is in allowedDirs: %q", s)
	}
}

// A bare "@word" line that is not path-like (no "/" or ".") is left as prose, not
// treated as an import directive.
func TestAgentsNonImportAtLineLeftAlone(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Ping @channel on release.\n@channel\n"), 0o644)
	s := agentsInstructions(dir, nil, 0, nil)
	if !strings.Contains(s, "@channel") {
		t.Errorf("non-import @line must be preserved as prose: %q", s)
	}
}

// Import paths containing spaces work via the explicit @import form (with or
// without quotes) and the bare quoted form.
func TestAgentsImportPathWithSpaces(t *testing.T) {
	cases := []struct{ name, directive string }{
		{"explicit-unquoted", "@import my docs/ui rules.md"},
		{"explicit-quoted", `@import "my docs/ui rules.md"`},
		{"bare-quoted", `@"my docs/ui rules.md"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "my docs"), 0o755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(dir, "my docs", "ui rules.md"), []byte("Spaced path rule Z.\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Rules.\n"+c.directive+"\n"), 0o644)
			s := agentsInstructions(dir, nil, 0, nil)
			if !strings.Contains(s, "Spaced path rule Z.") {
				t.Errorf("spaced import not resolved for %q: %q", c.directive, s)
			}
		})
	}
}

func TestAgentsBudgetBytes(t *testing.T) {
	if got := agentsBudgetBytes(0); got != agentsBudgetFloor {
		t.Errorf("zero window → floor %d, got %d", agentsBudgetFloor, got)
	}
	if got := agentsBudgetBytes(8000); got != agentsBudgetFloor {
		t.Errorf("small window → floor %d, got %d", agentsBudgetFloor, got)
	}
	if got := agentsBudgetBytes(900000); got != agentsBudgetCeil {
		t.Errorf("huge window → ceil %d, got %d", agentsBudgetCeil, got)
	}
	if got := agentsBudgetBytes(200000); got != 24000 { // 200000*4*3/100
		t.Errorf("mid window → 24000, got %d", got)
	}
}

package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// maxAgentsBytes bounds the total project-instructions injected into the system
// prompt so large AGENTS.md files can't dominate a small model's context window.
const maxAgentsBytes = 16 * 1024

// agentsSkipDirs are subdirectories never scanned for an AGENTS.md (deps/build/VCS).
var agentsSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true, "out": true,
	"target": true, "vendor": true, "www": true, ".angular": true,
}

// agentsInstructions reads project-level agent instructions and returns a
// system-prompt section to append, or "" if none. It looks for AGENTS.md (the open
// agent-instructions convention) — falling back to CLAUDE.md — in the launch
// directory AND in each immediate subdirectory, so a project that lives in a subdir
// (e.g. kloo run at a parent while the app is in ./myApp) still has its AGENTS.md
// honoured. All found files are stacked (each labelled with its location), bounded,
// and logged so the user knows what was applied.
func agentsInstructions(dir string, logf func(string, ...any)) string {
	dirs := []string{dir} // the entered/launch directory first
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && !agentsSkipDirs[e.Name()] && !strings.HasPrefix(e.Name(), ".") {
				dirs = append(dirs, filepath.Join(dir, e.Name()))
			}
		}
	}

	var sections []string
	used := 0
	for _, d := range dirs {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			data, err := os.ReadFile(filepath.Join(d, name))
			if err != nil {
				continue
			}
			s := strings.TrimSpace(string(data))
			if s == "" {
				continue
			}
			rel, _ := filepath.Rel(dir, filepath.Join(d, name))
			if rel == "" {
				rel = name
			}
			if used+len(s) > maxAgentsBytes { // stay within the budget across all files
				if avail := maxAgentsBytes - used; avail > 0 {
					s = s[:avail] + "\n…[truncated]"
				} else {
					break
				}
			}
			used += len(s)
			sections = append(sections, "## "+rel+"\n"+s)
			if logf != nil {
				logf("instructions: loaded %s — applied to every turn", rel)
			}
			break // one file per directory (AGENTS.md preferred over CLAUDE.md)
		}
	}

	if len(sections) == 0 {
		return ""
	}
	return "\n\n# Project instructions — follow these for this repository:\n\n" + strings.Join(sections, "\n\n")
}

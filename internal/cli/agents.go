package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lokalhub/kloo/internal/tools"
)

// agentsBudget bounds the total project-instructions injected into the system
// prompt. The pinned AGENTS.md block (plus any @import-ed files) is re-sent on
// EVERY turn and never compacted, so it is deliberately kept a small slice of the
// window rather than the full window: ~3% (tokens→bytes ≈ ×4), clamped to a floor
// so small models keep the original 16 KiB, and a ceiling so a huge window can't
// pin a giant rulebook every turn (per-turn cost + attention dilution).
const (
	agentsBudgetFloor = 16 * 1024 // small-model floor (the original fixed cap; unchanged behaviour)
	agentsBudgetCeil  = 64 * 1024 // cap: pinned every turn, so bounded even on a 900k window
)

// agentsBudgetBytes is the byte budget for project instructions given the model's
// context window (in tokens). 0/unknown ⇒ the floor.
func agentsBudgetBytes(windowTokens int) int {
	b := windowTokens * 4 * 3 / 100 // ~3% of the window, tokens→bytes ≈ ×4
	if b < agentsBudgetFloor {
		return agentsBudgetFloor
	}
	if b > agentsBudgetCeil {
		return agentsBudgetCeil
	}
	return b
}

// maxImportBytes caps a single @import-ed file's read so a stray large file can't
// OOM the load or dominate the prompt (the agentsBudget still bounds the total).
const maxImportBytes = 256 * 1024

// agentsSkipDirs are subdirectories never scanned for an AGENTS.md (deps/build/VCS).
var agentsSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true, "out": true,
	"target": true, "vendor": true, "www": true, ".angular": true,
}

// agentsInstructions reads project-level agent instructions from AGENTS.md (the open
// agent-instructions convention) and returns a system-prompt section to append, or
// "" if none. It looks in the launch directory AND in each immediate subdirectory,
// so a project that lives in a subdir (e.g. kloo run at a parent while the app is in
// ./myApp) still has its AGENTS.md honoured. All found files are stacked (each
// labelled with its location), bounded by agentsBudgetBytes(windowTokens), and
// logged so the user knows what applied.
//
// An AGENTS.md may pull in other files with an `@import <path>` (or bare `@<path>`)
// directive on its own line: the path is resolved relative to that AGENTS.md's
// directory, confined to the workspace jail, and expanded IN PLACE so the imported
// content inherits AGENTS.md's pinning (immune to compaction). A path outside the
// jail is read only if it sits under one of allowedDirs (--allowed-dirs); otherwise
// it is skipped with a log line. Imports are NOT recursive (an imported file's own
// @lines stay literal), which sidesteps cycles. This widening is read-only and
// load-time only — it never affects the model's runtime file tools.
func agentsInstructions(dir string, allowedDirs []string, windowTokens int, logf func(string, ...any)) string {
	ws, wsErr := tools.NewWorkspace(dir) // jail for @import resolution; dir is the launch cwd
	budget := agentsBudgetBytes(windowTokens)

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
		data, err := os.ReadFile(filepath.Join(d, "AGENTS.md"))
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			continue
		}
		if wsErr == nil {
			s = expandImports(s, d, ws, allowedDirs, logf) // @import expansion (jailed; --allowed-dirs widens)
		}
		rel, _ := filepath.Rel(dir, filepath.Join(d, "AGENTS.md"))
		if rel == "" {
			rel = "AGENTS.md"
		}
		if used+len(s) > budget { // stay within the budget across all files
			avail := budget - used
			if avail <= 0 {
				break
			}
			s = s[:avail] + "\n…[truncated]"
		}
		used += len(s)
		sections = append(sections, "## "+rel+"\n"+s)
		logIf(logf, "instructions: loaded %s — applied to every turn", rel)
	}

	if len(sections) == 0 {
		return ""
	}
	return "\n\n# Project instructions — follow these for this repository:\n\n" + strings.Join(sections, "\n\n")
}

var (
	reImportKw    = regexp.MustCompile(`^@import\s+(.+)$`)     // explicit: @import <path> (rest of line; spaces allowed, optionally quoted)
	reImportBareQ = regexp.MustCompile(`^@("[^"]+"|'[^']+')$`) // bare quoted: @"a b/c.md" (spaces allowed inside quotes)
	reImportBare  = regexp.MustCompile(`^@(\S+)$`)             // bare: @<path> — single token, path-like only
)

// expandImports replaces every @import directive line in content with the imported
// file's contents (labelled), resolving each path relative to agentsDir, confined
// to ws (the workspace jail) or — when outside it — to one of allowed. A directive
// that can't be resolved/read is dropped with a log line; a non-import line (incl.
// a bare "@word" that isn't path-like) is left untouched.
func expandImports(content, agentsDir string, ws tools.Workspace, allowed []string, logf func(string, ...any)) string {
	if !strings.Contains(content, "@") {
		return content // fast path: no possible directive
	}
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		p := importPath(strings.TrimSpace(ln))
		if p == "" {
			out = append(out, ln)
			continue
		}
		abs, ok := resolveImport(agentsDir, p, ws, allowed)
		if !ok {
			logIf(logf, "instructions: import %s skipped — outside workspace (pass --allowed-dirs to permit it)", p)
			continue
		}
		body, ok := readImport(abs)
		if !ok {
			logIf(logf, "instructions: import %s skipped — unreadable, empty, or not a file", p)
			continue
		}
		out = append(out, "### imported: "+p+"\n"+body)
		logIf(logf, "instructions: imported %s — applied to every turn", p)
	}
	return strings.Join(out, "\n")
}

// importPath returns the import target from a trimmed line, or "" when the line is
// not an import directive. The explicit `@import <path>` form takes the rest of the
// line (so a path with SPACES works) and strips surrounding quotes. The bare form
// needs no quotes when the path is a single token (`@a/b.md`) and IS path-like
// (contains "/" or "."), or may be quoted to carry spaces (`@"a b/c.md"`); a bare
// non-path-like word like "@todo" is left as prose.
func importPath(trimmed string) string {
	if m := reImportKw.FindStringSubmatch(trimmed); m != nil {
		return unquote(strings.TrimSpace(m[1]))
	}
	if m := reImportBareQ.FindStringSubmatch(trimmed); m != nil {
		return unquote(m[1])
	}
	if m := reImportBare.FindStringSubmatch(trimmed); m != nil && strings.ContainsAny(m[1], "/.") {
		return m[1]
	}
	return ""
}

// unquote strips one matching pair of surrounding single or double quotes, so an
// import path may be quoted to carry spaces. A string without a matching pair is
// returned unchanged.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// resolveImport maps an import path (relative to agentsDir, or absolute) to an
// absolute file path that is either inside the workspace jail OR under one of the
// explicitly allowed dirs. It returns (path, false) when the target escapes both.
func resolveImport(agentsDir, p string, ws tools.Workspace, allowed []string) (string, bool) {
	cand := p
	if !filepath.IsAbs(cand) {
		cand = filepath.Join(agentsDir, p)
	}
	cand = filepath.Clean(cand)
	if abs, err := ws.Resolve(cand); err == nil { // inside the workspace jail
		return abs, true
	}
	if dirAllows(allowed, cand) { // explicitly whitelisted outside dir
		return cand, true
	}
	return "", false
}

// dirAllows reports whether target lies within any of the allowed dirs, comparing
// canonical (absolute, symlink-resolved) paths so a relative --allowed-dirs or a
// symlinked path still matches.
func dirAllows(allowed []string, target string) bool {
	tc := canonicalPath(target)
	for _, d := range allowed {
		if strings.TrimSpace(d) == "" {
			continue
		}
		dc := canonicalPath(d)
		if tc == dc || strings.HasPrefix(tc, dc+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// canonicalPath returns p as an absolute, symlink-resolved, cleaned path; it falls
// back to the lexical absolute path when the target does not exist yet.
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if ev, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(ev)
	}
	return filepath.Clean(abs)
}

// readImport reads an imported file's text, capped at maxImportBytes (a larger file
// is read up to the cap and marked truncated). Returns ("", false) for a missing
// file, a directory, a read error, or empty content.
func readImport(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxImportBytes))
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", false
	}
	if fi.Size() > maxImportBytes {
		s += fmt.Sprintf("\n…[import truncated at %d KiB]", maxImportBytes/1024)
	}
	return s, true
}

// logIf calls logf with the message when logf is non-nil (a no-op otherwise), so
// callers can pass nil to silence the loader (e.g. tests).
func logIf(logf func(string, ...any), format string, args ...any) {
	if logf != nil {
		logf(format, args...)
	}
}

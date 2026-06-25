package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/tools"
)

// buildVerifier returns the loop's Verifier for command, or a nil Verifier when
// command is empty — which the loop reads as "unverified mode" (honour finish, but
// label no run success). opts (e.g. a timeout) apply only when a command is set.
func buildVerifier(ws tools.Workspace, command string, opts ...agent.VerifyOption) agent.Verifier {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	return agent.NewCommandVerifier(ws, command, opts...)
}

// buildLinter returns the loop's fast advisory Linter for command, or a nil Linter
// when command is empty — which the loop reads as "no lint step" (byte-identical to
// pre-lint behaviour). perFile (from resolveLintCommand) decides whether the edited
// path is appended; the short lintTimeout default in NewCommandLinter applies.
// Mirrors buildVerifier, but advisory: lint never gates success.
func buildLinter(ws tools.Workspace, command string, perFile bool, opts ...agent.LintOption) agent.Linter {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	return agent.NewCommandLinter(ws, command, perFile, opts...)
}

// resolveVerifyCommand decides the verify command for a run. The explicit value
// (from the deprecated --verify flag) wins when set; otherwise kloo auto-detects
// the project's canonical build/test command (project awareness). When nothing is
// recognised it returns "" — the loop then runs "unverified": the model's finish
// is honoured as a calm stop, but no run is labelled success (nothing proved the
// change works). Either way the chosen mode is logged so the user is never
// surprised about what gates completion.
func resolveVerifyCommand(explicit, dir string, logf func(string, ...any)) string {
	if strings.TrimSpace(explicit) != "" {
		logf("verify: using --verify %q", explicit)
		return explicit
	}
	cmd := detectVerify(dir)
	if cmd != "" {
		logf("verify: auto-detected %q (override with --verify)", cmd)
	} else {
		logf("verify: no build/test detected — running UNVERIFIED (no run is marked success; pass --verify to gate on a command)")
	}
	return cmd
}

// detectVerify infers a project's canonical verify (build/test) command from
// well-known manifest files in dir, or "" when none is recognised. First match by
// priority wins; the user can always override with --verify. The command is run
// through the same jailed run_command tool as before, so its exit code remains the
// trusted, unfoolable success signal — only its *source* moved from a required
// flag to project awareness.
// detectVerify infers the project's verify command from dir, OR — when dir itself
// has no recognised project — from a single immediate subdirectory that does. The
// subdir case covers the common "kloo launched one dir up while the app lives in
// ./myApp" layout: the command is prefixed with `cd <subdir> && …` so it runs in
// the right place. If SEVERAL subdirs are projects (a monorepo), it stays ambiguous
// (returns "" → unverified) rather than guess wrong.
func detectVerify(dir string) string {
	if cmd := detectVerifyHere(dir); cmd != "" {
		return cmd
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	sub, cmd, n := "", "", 0
	for _, e := range entries {
		if !e.IsDir() || verifySkipDirs[e.Name()] || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if c := detectVerifyHere(filepath.Join(dir, e.Name())); c != "" {
			sub, cmd, n = e.Name(), c, n+1
		}
	}
	if n == 1 { // exactly one subdir project — unambiguous
		return "cd " + sub + " && " + cmd
	}
	return ""
}

// verifySkipDirs are subdirectories never scanned for a project when auto-detecting
// the verify command (deps/build/VCS).
var verifySkipDirs = map[string]bool{
	"node_modules": true, "dist": true, "build": true, "out": true,
	"target": true, "vendor": true, "www": true, "coverage": true,
}

// detectVerifyHere infers a project's verify (build/test) command from the manifest
// files IN dir (no recursion), or "" when none is recognised.
func detectVerifyHere(dir string) string {
	// Node/JS app — the common case: prefer a build script, then test (whichever
	// exists; the script's exit code is what gates completion).
	if scripts := nodeScripts(dir); scripts != nil {
		if _, ok := scripts["build"]; ok {
			return "npm run build"
		}
		if _, ok := scripts["test"]; ok {
			return "npm test"
		}
	}
	if fileExists(dir, "go.mod") {
		return "go test ./..."
	}
	if fileExists(dir, "Cargo.toml") {
		return "cargo build"
	}
	if fileExists(dir, "pyproject.toml") || fileExists(dir, "setup.py") || fileExists(dir, "setup.cfg") {
		return "python -m pytest"
	}
	return ""
}

// fileExists reports whether dir/name exists.
func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// nodeScripts returns the "scripts" map from dir/package.json, or nil when absent
// or unparseable.
func nodeScripts(dir string) map[string]any {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]any `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return nil
	}
	return pkg.Scripts
}

// lintCommand is a detected fast linter. Command is the base command; PerFile
// reports whether the edited file path(s) are appended when it runs (Phase 01) —
// true for the per-file linters (gofmt/eslint/ruff/flake8), false for tsc which
// is whole-project (`tsc --noEmit` is meaningless on a single file). The zero
// value ({"", false}) means no linter was recognised. ADVISORY only — never a gate.
type lintCommand struct {
	Command string
	PerFile bool
}

// resolveLintCommand decides the fast advisory lint command for a run, mirroring
// resolveVerifyCommand but for the advisory rail rather than the success gate.
// Precedence: a disable (--no-lint / KLOO_NO_LINT) forces "" (no lint step);
// otherwise an explicit override (--lint / KLOO_LINT) wins; else the detected
// linter; else "" (silent — no lint feedback). The returned bool reports whether
// the command is per-file (the edited path is appended in Phase 01); an explicit
// override is run verbatim, so it is reported per-file=false (the user controls it).
// The chosen mode is logged once so the user is never surprised about lint.
func resolveLintCommand(explicit string, disabled bool, dir string, logf func(string, ...any)) (cmd string, perFile bool) {
	if disabled {
		logf("lint: disabled (--no-lint)")
		return "", false
	}
	if strings.TrimSpace(explicit) != "" {
		logf("lint: using --lint %q", explicit)
		return explicit, false
	}
	lc := detectLint(dir)
	if lc.Command != "" {
		logf("lint: auto-detected %q (override with --lint, disable with --no-lint)", lc.Command)
		return lc.Command, lc.PerFile
	}
	logf("lint: no linter detected — skipping (no lint feedback)")
	return "", false
}

// detectLint infers a project's FAST lint command from well-known signals in dir,
// mirroring detectVerify but optimising for speed and per-file feedback rather
// than a full build/test. First match by priority wins; it returns the zero
// lintCommand ({"", false}) when nothing is recognised. Detection is pure —
// file/PATH lookups only, never executing a linter. The result feeds the advisory
// lint rail (Phase 01) and is NEVER a success/failure gate.
func detectLint(dir string) lintCommand {
	if fileExists(dir, "go.mod") {
		return lintCommand{"gofmt -l", true}
	}
	if fileExists(dir, "tsconfig.json") || fileExists(dir, filepath.Join("node_modules", ".bin", "tsc")) {
		return lintCommand{"tsc --noEmit", false}
	}
	if fileExists(dir, "package.json") && eslintPresent(dir) {
		return lintCommand{"eslint", true}
	}
	if pythonProject(dir) {
		if ruffPresent(dir) {
			return lintCommand{"ruff check", true}
		}
		return lintCommand{"flake8", true}
	}
	return lintCommand{}
}

// eslintPresent reports whether eslint is usable in dir: a locally installed
// binary, or an eslint config file (legacy .eslintrc* or flat eslint.config.*).
func eslintPresent(dir string) bool {
	if fileExists(dir, filepath.Join("node_modules", ".bin", "eslint")) {
		return true
	}
	for _, pat := range []string{".eslintrc*", "eslint.config.*"} {
		if matches, _ := filepath.Glob(filepath.Join(dir, pat)); len(matches) > 0 {
			return true
		}
	}
	return false
}

// pythonProject reports whether dir looks like a Python project.
func pythonProject(dir string) bool {
	return fileExists(dir, "pyproject.toml") || fileExists(dir, "setup.py") || fileExists(dir, "setup.cfg")
}

// ruffPresent reports whether ruff is the project's chosen/available linter: a
// ruff.toml, a [tool.ruff] table in pyproject.toml, or ruff on PATH.
func ruffPresent(dir string) bool {
	if fileExists(dir, "ruff.toml") {
		return true
	}
	if data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml")); err == nil && strings.Contains(string(data), "[tool.ruff]") {
		return true
	}
	return binOnPath("ruff")
}

// binOnPath reports whether name resolves to an executable on PATH.
func binOnPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

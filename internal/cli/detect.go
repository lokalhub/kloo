package cli

import (
	"encoding/json"
	"os"
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
func detectVerify(dir string) string {
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

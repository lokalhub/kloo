package cli

import (
	"strings"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/tools"
)

// applyScope resolves the A1/A2 file-scope policy (CLI flags overlaid on
// .kloo/scope.yaml under cwd) and the A4 patch-only flag, attaches them to ws, and
// logs one line describing the resulting model-facing write boundary. The returned
// workspace gates edit_file/write_file through the policy and — when scope is active
// or patch-only is set — causes DefaultRegistry to withhold the model-facing
// run_command. The verifier/linter build their own unscoped run_command, so harness
// gates keep running. An invalid glob or malformed manifest is returned as an error.
func applyScope(cfg config.Config, ws tools.Workspace, cwd string, logf func(string, ...any)) (tools.Workspace, error) {
	scopeCfg, err := config.ResolveScope(config.ScopeFlags{
		Allow:    cfg.ScopeAllow,
		Deny:     cfg.ScopeDeny,
		ReadOnly: cfg.ScopeReadOnly,
	}, cwd)
	if err != nil {
		return ws, err
	}
	policy, err := tools.NewScopePolicy(scopeCfg.Allow, scopeCfg.Deny, scopeCfg.ReadOnly)
	if err != nil {
		return ws, err
	}
	ws = ws.WithScope(policy).WithPatchOnly(cfg.PatchOnly)

	switch {
	case ws.ScopeActive():
		logf("scope: model edits restricted (allow=%s deny=%s read-only=%s) — model-facing run_command is disabled",
			joinScope(scopeCfg.Allow), joinScope(scopeCfg.Deny), joinScope(scopeCfg.ReadOnly))
	case cfg.PatchOnly:
		logf("patch-only: model may change files only via edit_file/write_file — model-facing run_command is disabled")
	}
	return ws, nil
}

func joinScope(globs []string) string {
	if len(globs) == 0 {
		return "-"
	}
	return strings.Join(globs, ",")
}

// agentStopPolicy maps the resolved config.StopPolicy (A7 --stop-on) onto the agent
// loop's StopPolicy — kept a separate type so internal/agent never imports config.
func agentStopPolicy(sp config.StopPolicy) agent.StopPolicy {
	return agent.StopPolicy{
		OffScopeEdit:   sp.OffScopeEdit,
		ReadOnlyEdit:   sp.ReadOnlyEdit,
		RepeatedVerify: sp.RepeatedVerify,
	}
}

// scopeSystemPromptSuffix returns a short instruction appended to the system prompt
// when a scope policy or patch-only mode is active, so the model is told up front it
// cannot use the shell and must change files with the exact-edit tools (A4).
func scopeSystemPromptSuffix(ws tools.Workspace) string {
	if !ws.ModelShellDisabled() {
		return ""
	}
	return "\n\nThis run restricts how you may change files: run_command is NOT available. " +
		"Make every code change with edit_file (SEARCH/REPLACE) or write_file only, and only " +
		"to files inside the permitted scope. Do not attempt shell commands to edit files."
}

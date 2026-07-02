package tools

import (
	"context"
	"errors"
	"fmt"
)

// ErrPatchOnlyForbidden is the sentinel a run_command call wraps when it is
// rejected because patch-only mode (A4) is active but no scope policy is. It is
// distinct from ErrOffScope: patch-only is a run-mode restriction, not a
// scope denial, so the headless classifier maps it to tool_call_invalid
// (class patch_only_forbidden_tool), not off_scope_edit.
var ErrPatchOnlyForbidden = errors.New("tools: run_command is disabled in patch-only mode")

// disabledRunCommand stands in for the real run_command when the model-facing
// shell is withheld (a scope policy is active, or patch-only mode is set). It is
// registered HIDDEN — absent from the advertised vocabulary (Registry.Tools), so
// the model is never offered it — yet still dispatchable, so a fallback adapter
// that emits run_command anyway is rejected BEFORE any process runs (rather than
// surfacing a generic unknown-tool error). scope selects the failure shape:
//   - scope==true  → a *ScopeError (class run_command_disabled_for_scope) wrapping
//     ErrOffScope, so the A7 off-scope-edit stop rule and the off_scope_edit
//     classifier both catch a scoped shell rejection.
//   - scope==false → ErrPatchOnlyForbidden (patch-only, no scope).
type disabledRunCommand struct {
	scope bool
}

func (disabledRunCommand) Name() string { return NameRunCommand }
func (disabledRunCommand) Description() string {
	// Never advertised (registered hidden), but Description must be non-panicking.
	return "run_command is disabled for this run."
}
func (disabledRunCommand) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{"command": {Type: "string", Description: "disabled"}},
		Required:   []string{"command"},
	}
}
func (d disabledRunCommand) Invoke(_ context.Context, _ Call) (Result, error) {
	if d.scope {
		return Result{}, &ScopeError{
			Class:   ScopeClassRunCommandDisabled,
			Tool:    NameRunCommand,
			Message: "run_command is disabled while a file-scope policy is active (--allow/--deny/--read-only or .kloo/scope.yaml); make changes with edit_file/write_file instead",
		}
	}
	return Result{}, fmt.Errorf("run_command is disabled in patch-only mode; make changes with edit_file/write_file instead: %w", ErrPatchOnlyForbidden)
}

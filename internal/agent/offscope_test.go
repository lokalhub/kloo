package agent

import (
	"context"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestOffScopeDenialsNameDistinctPaths: a scoped run records the PATHS it was
// refused, deduplicated and in first-denial order, alongside the allow list that
// was in force.
//
// The counter alone (off_scope_edits=N) was unactionable: twice in one evening a
// run was launched with a mis-scoped --allow — once with an Angular component's
// .ts allowed but its .html left out — and the path had to be dug out of mid-log
// before the one-line flag fix was obvious. The report now carries what the
// summary needs to name them.
func TestOffScopeDenialsNameDistinctPaths(t *testing.T) {
	srv := llmtest.Sequence(t,
		// Denied: outside the allowed scope.
		llmtest.Mock{Body: toolResp(t, 20, tcSpec{"write_file", map[string]any{
			"path": "frontend/src/app/pages/login/login.page.ts", "content": "export class LoginPage { x = 1 }\n",
		}})},
		// Denied again, SAME path: a repeat attempt must not produce a second entry.
		llmtest.Mock{Body: toolResp(t, 20, tcSpec{"write_file", map[string]any{
			"path": "frontend/src/app/pages/login/login.page.ts", "content": "export class LoginPage { x = 2 }\n",
		}})},
		// Denied: a second distinct path.
		llmtest.Mock{Body: toolResp(t, 20, tcSpec{"write_file", map[string]any{
			"path": "frontend/src/app/pages/login/login.page.scss", "content": "form { width: 50%; }\n",
		}})},
		llmtest.Mock{Body: toolResp(t, 20, finishSpec)},
	)
	v := &stubVerifier{results: []VerifyResult{redResult(karmaRed)}}
	loop := scopedLoopAt(t, t.TempDir(), srv, incidentTree(), []string{gameScope}, v)

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The existing counter counts ATTEMPTS and must be untouched by this reporting.
	if rep.ToolCounters.OffScopeEdits != 3 {
		t.Fatalf("off_scope_edits = %d, want 3 (the fixture must actually be denied 3 times)", rep.ToolCounters.OffScopeEdits)
	}
	os := rep.OffScope
	if os == nil {
		t.Fatal("rep.OffScope is nil: the run was denied 3 writes and named none of them")
	}
	if os.Distinct != 2 || len(os.Denials) != 2 {
		t.Fatalf("distinct = %d, denials = %d, want 2 and 2 (repeat attempts on one path collapse)", os.Distinct, len(os.Denials))
	}
	if got := os.Denials[0].Path; got != "frontend/src/app/pages/login/login.page.ts" {
		t.Fatalf("denials[0].Path = %q, want the FIRST path denied", got)
	}
	if got := os.Denials[1].Path; got != "frontend/src/app/pages/login/login.page.scss" {
		t.Fatalf("denials[1].Path = %q, want the second path denied", got)
	}
	if got := os.Denials[0].Class; got != "outside_allow" {
		t.Fatalf("denials[0].Class = %q, want outside_allow", got)
	}
	// Without the allow list the suggested flag would REPLACE the run's scope
	// rather than extend it, and pasting it would silently narrow the next run.
	if len(os.Allow) != 1 || os.Allow[0] != gameScope {
		t.Fatalf("Allow = %v, want the run's allow globs %v", os.Allow, []string{gameScope})
	}
}

// TestOffScopeNilWhenNothingDenied: an unscoped run (and any run that was never
// refused a write) carries no off-scope report, so the summary stays silent in the
// common case.
func TestOffScopeNilWhenNothingDenied(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 20, tcSpec{"write_file", map[string]any{
			"path": "frontend/src/app/pages/game/game.page.scss", "content": "ion-grid { --ion-grid-columns: 4 }\n",
		}})},
		llmtest.Mock{Body: toolResp(t, 20, finishSpec)},
	)
	v := &stubVerifier{results: []VerifyResult{{Passed: true}}}
	loop := scopedLoopAt(t, t.TempDir(), srv, incidentTree(), []string{gameScope}, v)

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.OffScope != nil {
		t.Fatalf("rep.OffScope = %+v, want nil when no write was denied", rep.OffScope)
	}
}

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// A ROLLED-BACK RUN MUST STILL BE ABLE TO SAY WHAT IT WROTE.
//
// THE INCIDENT, measured 2026-10-25. An orchestrator launched a scoped run:
//
//	kloo --benchmark --ctx 24576 \
//	  --allow src/app/catalog/livery.ts --allow src/app/catalog/livery.spec.ts \
//	  --verify 'node --test src/app/catalog/livery.spec.ts' \
//	  --stop-on off-scope-edit -- <task>
//
// The model wrote BOTH allowed files — they were watched appearing on disk while the
// run was live — then tried edit_file on package.json, which is outside --allow. The
// stop rule did its job and ended the run, and the rollback then removed the two
// files the run had created (that is deliberate: rollbackscope_test.go asserts it).
// kloo's JSON reported:
//
//	"failure_code": "off_scope_edit", "off_scope_edits": 1,
//	"files_changed": {"count": 0, "paths": []}
//
// files_changed is read from the git working tree AFTER the restore, so it reported
// the state of the tree rather than the history of the run. The harness consumed it
// and recorded four `must_contain "…" not found` failures for strings the model had
// in fact written, plus "no files[] content hash changed and no new: path was
// created". The diagnosis handed to the next reader — human or replanner — was "the
// model never did the work", about a run that did the work and then stepped out of
// bounds.
//
// The rollback is NOT the defect and does not change. The report losing the run's own
// record of its writes is. The loop already knows the paths (editedPaths +
// createdPaths, the very set it hands the restore), so the report carries them out.

// newGitScopedLoop builds a scoped loop over a REAL git repo with a REAL checkpointer,
// so the writes, the safety stop and the rollback all actually happen. seed maps
// workspace-relative path → contents and is committed before the run.
func newGitScopedLoop(t *testing.T, srv *llmtest.Server, seed map[string]string, allow []string) (*Loop, string) {
	t.Helper()
	haveGit(t)
	root := t.TempDir()
	for rel, content := range seed {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, p, content)
	}
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, canon, "init")
	gitIn(t, canon, "add", "-A")
	gitIn(t, canon, "commit", "-m", "seed")

	ws, err := tools.NewWorkspace(canon)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := tools.NewScopePolicy(allow, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws = ws.WithScope(policy)
	return &Loop{
		Client:     llm.New(srv.URL+"/v1", "test-model"),
		Adapter:    tools.NativeFCAdapter{},
		Registry:   tools.DefaultRegistry(ws),
		Budget:     &stubBudget{tripAt: 50},
		Churn:      &stubChurn{},
		Checkpoint: NewGitCheckpointer(canon),
		Root:       canon,
		Scope:      ws.Scope(),
		Endpoint:   srv.URL + "/v1",
		Model:      "test-model",
		System:     "you are kloo",
	}, canon
}

// TestOffScopeStopReportsTheWritesTheRollbackRemoved is the incident, reduced: two
// in-scope files written, then a denied edit outside --allow, then the stop and the
// rollback. The tree must come back clean (unchanged semantics) AND the report must
// still name both files.
func TestOffScopeStopReportsTheWritesTheRollbackRemoved(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "src/livery.ts", "content": "export const livery = 1;\n"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "src/livery.spec.ts", "content": "import {livery} from './livery';\n"}})},
		// Out of bounds: package.json is not in --allow. This ends the run.
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "package.json", "diff": "```\n{}\n```\n"}})},
	)
	loop, root := newGitScopedLoop(t, srv, map[string]string{"package.json": "{}\n"}, []string{"src/**"})
	loop.StopOn = StopPolicy{OffScopeEdit: true}

	rep, err := loop.Run(context.Background(), "write the livery module and its spec")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSafetyStop {
		t.Fatalf("Reason = %s, want safety-stop (%s)", rep.Reason, rep.String())
	}
	if !rep.RolledBack {
		t.Fatalf("a safety stop after edits must roll back")
	}
	// ROLLBACK SEMANTICS ARE UNCHANGED: the files the run created are gone.
	for _, rel := range []string{"src/livery.ts", "src/livery.spec.ts"} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the rollback — this test must not change rollback behaviour", rel)
		}
	}
	// THE FIX: the report still knows what the run wrote, in sorted order.
	want := []string{"src/livery.spec.ts", "src/livery.ts"}
	if got := rep.ChangedPaths; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ChangedPaths = %v, want %v — the run wrote both files and the report lost them, "+
			"which is what made the harness report that the model did nothing", got, want)
	}
}

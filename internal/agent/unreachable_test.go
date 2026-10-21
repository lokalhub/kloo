package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lokalhub/kloo/internal/tools"
)

// loopWithTree builds the smallest Loop the implicated-files analysis needs: a
// workspace on disk and a scope policy. No LLM, no registry — failingTestFiles and
// takeBaseline's reach verdict read files and ask the policy, nothing more.
func loopWithTree(t *testing.T, tree map[string]string, allow, deny []string) *Loop {
	t.Helper()
	root := t.TempDir()
	for rel, content := range tree {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := tools.NewScopePolicy(allow, deny, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Loop{Root: canon, Scope: policy}
}

// THE RED WAS REACHABLE ALL ALONG, TWO LEVELS DOWN.
//
// Measured on sofronios-legacy, 2026-10-21 (kloo v0.36.3). The task was to edit
// ONE source file, worker/lib/game-do.ts, against a gate in worker/tests/. The run
// was scoped with --deny on the test directories, which is the correct way to say
// "do not touch the gate". kloo reported:
//
//	baseline: verify ALREADY RED before the first edit (4 known failure(s), 5 out of scope)
//	rails: verify-out-of-scope×13
//
// and the model's closing report said, accurately from what it was told: "the
// verify failure ... comes from [the five test files] — all outside my write scope.
// I did not touch any of them." It made no edits and stopped EXPLORE-STOP after 34
// steps and 724,120 tokens.
//
// The five implicated files were the test file and the four modules it imports.
// game-do.ts — the one file the run was allowed to write, and the one the fix
// belonged in — was never implicated, because it is imported by the test's HELPER,
// not by the test itself, and importsOf only looks one level down. Every file the
// analysis found was denied, so the red was declared unreachable and the model was
// told, repeatedly, that its own task was not its problem.
//
// Reachability must be followed through the import graph, not just its first edge.
func TestImplicatedFilesFollowImportsTransitively(t *testing.T) {
	l := loopWithTree(t, map[string]string{
		"worker/tests/game-do-bot.test.ts": "import { makeDO } from './harness';\nimport { fakeD1 } from './fake-d1';\n",
		"worker/tests/harness.ts":          "import { GameDO } from '../lib/game-do';\nimport { fakeD1 } from './fake-d1';\n",
		"worker/tests/fake-d1.ts":          "export const fakeD1 = () => null;\n",
		"worker/lib/game-do.ts":            "export class GameDO {}\n",
	}, nil, []string{"worker/tests/**"})

	files := l.failingTestFiles(" FAIL  worker/tests/game-do-bot.test.ts > bot match: SETUP\n")
	var found bool
	for _, f := range files {
		if f == "worker/lib/game-do.ts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the writable source behind the test helper was never implicated: %v", files)
	}
	// And therefore the red is NOT out of reach: one implicated file is writable.
	if denied := l.scopeDenies(files); len(denied) == len(files) {
		t.Fatalf("every implicated file was denied, so the red is still misread as unreachable: %v", denied)
	}
}

// The walk must stay bounded. An import graph has cycles and a monorepo's has
// thousands of edges; the verdict needs enough of it to be confident, not all of it.
func TestTransitiveWalkIsBoundedAndCycleSafe(t *testing.T) {
	l := loopWithTree(t, map[string]string{
		// a.test.ts -> b -> c -> b  (a cycle), plus a long chain off c.
		"t/a.test.ts": "import './b';\n",
		"t/b.ts":      "import './c';\n",
		"t/c.ts":      "import './b';\nimport './d';\n",
		"t/d.ts":      "export const d = 1;\n",
	}, nil, nil)
	files := l.failingTestFiles("FAIL t/a.test.ts\n")
	if len(files) == 0 {
		t.Fatal("the walk found nothing at all")
	}
	if len(files) > maxFailingTestFiles {
		t.Fatalf("the walk blew its bound: %d files", len(files))
	}
	seen := map[string]int{}
	for _, f := range files {
		seen[f]++
		if seen[f] > 1 {
			t.Fatalf("%s appears twice — the cycle was walked more than once: %v", f, files)
		}
	}
}

// AN ALL-TEST IMPLICATED SET IS NOT EVIDENCE OF UNREACHABILITY.
//
// Measured on the kloo repo itself, 2026-10-21: the gate was a Go package that did
// not compile, and the compiler named only the _test.go file that called the
// missing methods. That file was denied (correctly — it is the gate). Nothing else
// resolved, so every implicated file was denied and kloo declared the red
// unreachable; the model never attempted the edit its brief described and stopped
// EXPLORE-STOP with 0 edits and 10 verify-out-of-scope rails.
//
// Where a failure is REPORTED is not where the fix belongs. When the implicated set
// contains no non-test file, the analysis has learned nothing about reachability and
// must not conclude anything — the conclusion needs at least one real source file
// in evidence.
func TestAllTestImplicatedSetIsNotUnreachable(t *testing.T) {
	l := loopWithTree(t, map[string]string{
		"internal/agent/reanchor_test.go": "package agent\n\nfunc TestX(t *testing.T) { l.reanchorRead(c) }\n",
		"internal/agent/loop.go":          "package agent\n",
	}, nil, []string{"internal/agent/*_test.go"})

	l.Verifier = &stubVerifier{results: []VerifyResult{{
		Command: "go test ./internal/agent/",
		Passed:  false,
		Stdout:  "internal/agent/reanchor_test.go:43:7: l.reanchorRead undefined\nFAIL\tgithub.com/x/internal/agent [build failed]\n",
	}}}
	b := l.takeVerifyBaseline(context.Background())
	if len(b.Unreachable) > 0 {
		t.Fatalf("a set of nothing but test files was taken as proof the red cannot be fixed: %v", b.Unreachable)
	}
}

// The real unreachable case must still work: a failing spec whose own file AND the
// source it covers are both outside the scope is genuinely not the run's to fix,
// and kloo must still say so. This is the case the feature was built for and the
// negative control for the two changes above.
func TestAGenuinelyUnreachableRedIsStillUnreachable(t *testing.T) {
	l := loopWithTree(t, map[string]string{
		"frontend/src/app/pages/login/login.page.spec.ts": "import { LoginPage } from './login.page';\n",
		"frontend/src/app/pages/login/login.page.ts":      "export class LoginPage {}\n",
		"frontend/src/app/pages/game/game.page.ts":        "export class GamePage {}\n",
	}, []string{"frontend/src/app/pages/game/**"}, nil)

	l.Verifier = &stubVerifier{results: []VerifyResult{{
		Command: "npm test",
		Passed:  false,
		Stdout:  "FAILED frontend/src/app/pages/login/login.page.spec.ts:53:5 expected true to be false\n",
	}}}
	b := l.takeVerifyBaseline(context.Background())
	if len(b.Unreachable) == 0 {
		t.Fatal("a red whose spec and source are both out of scope is no longer reported as unreachable")
	}
}

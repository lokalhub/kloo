package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// ─── The incident these tests exist for ──────────────────────────────────────
//
// 2026-10-06, an Ionic/Angular app. A run scoped with
// `--allow 'frontend/src/app/pages/game/**'` was told "make the game board mobile
// friendly at <=480px". The project's `npm test` was ALREADY red: one
// committed-red spec, login.page.spec.ts:53, out of scope and unrelated. kloo saw
// red, so the run could not succeed; the edit corrective ordered an edit anyway;
// the only failing test was the login one; the model edited login.page.ts 21 times
// and every attempt was denied by the scope. 1,553,679 tokens, all off-task, and a
// correct SCSS patch scored success:false.
//
// incidentTree is that project in miniature: one file inside the allowed scope,
// and a failing spec (plus the source it imports) outside it.
func incidentTree() map[string]string {
	return map[string]string{
		"frontend/src/app/pages/game/game.page.scss": "ion-grid {\n  --ion-grid-columns: 8;\n}\n",
		"frontend/src/app/pages/game/game.page.ts":   "export class GamePage {}\n",
		"frontend/src/app/pages/login/login.page.spec.ts": "import { LoginPage } from './login.page';\n" +
			"describe('LoginPage', () => { it('constrains the login form width', () => {}); });\n",
		"frontend/src/app/pages/login/login.page.ts":   "export class LoginPage {}\n",
		"frontend/src/app/pages/login/login.page.scss": "form { width: 100%; }\n",
		"frontend/src/app/app.component.ts":            "export class AppComponent {}\n",
		"frontend/src/app/services/auth.service.ts":    "export class AuthService {}\n",
	}
}

const gameScope = "frontend/src/app/pages/game/**"

// karmaRed is karma/jasmine output in its real shape: the failing test name is at
// the END of the line (so failingAssertions, written for vitest, extracts NOTHING
// from it) and the only trace of WHICH file failed is a stack frame. Both
// properties matter — this is the output the baseline has to work from.
const karmaRed = "Chrome Headless 140.0.0.0 (Linux x86_64) LoginPage constrains the login form width FAILED\n" +
	"\tError: Expected false to be true, 'an ancestor of <form> must cap width'\n" +
	"\t    at <Jasmine> frontend/src/app/pages/login/login.page.spec.ts:53:5\n" +
	"Chrome Headless 140.0.0.0 (Linux x86_64): Executed 44 of 44 (1 FAILED) (0.512 secs / 0.303 secs)\n"

func redResult(out string) VerifyResult {
	return VerifyResult{Command: "npm test", ExitCode: 1, Passed: false, Stdout: out, VerifyRan: true}
}

// TestBaselineRedOutOfScopeRunSucceeds is the decisive test for the red-verify
// trap: a project whose verify is RED before the run and still exactly as red
// after a correct, in-scope edit must be judged a SUCCESS.
//
// Fails on HEAD before this change: the success gate was `lastVerify.Passed &&
// edited`, so a run that could not turn an out-of-scope pre-existing failure green
// could never be anything but a failure, no matter what it correctly changed.
func TestBaselineRedOutOfScopeRunSucceeds(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{
			"path":    "frontend/src/app/pages/game/game.page.scss",
			"content": "@media (max-width: 480px) {\n  ion-grid { --ion-grid-columns: 4; }\n}\n",
		}})},
		llmtest.Mock{Body: toolResp(t, 5, finishSpec)},
	)
	// The SAME red every time: the baseline's red and the post-edit red are one
	// failure, unchanged.
	v := &countingVerifier{results: []VerifyResult{redResult(karmaRed)}}
	loop, _ := newScopedLoop(t, srv, incidentTree(), []string{gameScope}, nil, nil, false, v)

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly at <=480px")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %s, want success — the scoped change landed and the red is the SAME red the baseline recorded\n%s",
			rep.Reason, rep.String())
	}
	if rep.Baseline == nil || !rep.Baseline.Taken || rep.Baseline.Passed {
		t.Fatalf("baseline evidence = %+v, want a taken RED baseline", rep.Baseline)
	}
	if !rep.Baseline.Tolerated {
		t.Fatal("a success on a still-red verify must be reported as baseline-tolerated, not as a green verify")
	}
	if len(rep.Baseline.Unreachable) == 0 {
		t.Fatal("baseline did not identify the out-of-scope files the failure comes from")
	}
	var sawSpec bool
	for _, f := range rep.Baseline.Unreachable {
		if f == "frontend/src/app/pages/login/login.page.spec.ts" {
			sawSpec = true
		}
	}
	if !sawSpec {
		t.Fatalf("unreachable set %v does not name the failing spec", rep.Baseline.Unreachable)
	}
	// The report must not claim verification passed, because it did not.
	if s := rep.String(); strings.Contains(s, "verification passed") {
		t.Fatalf("report claims verification passed on a red verify:\n%s", s)
	}
	if rep.RolledBack {
		t.Fatal("a successful run must keep its work")
	}
}

// TestNewFailureAfterBaselineIsNotTolerated: the baseline excuses the red it
// recorded and NOTHING else. A failure that appears after the agent's edit is
// damage, and damage must still sink the run — otherwise the baseline would be a
// blanket amnesty rather than an attribution.
func TestNewFailureAfterBaselineIsNotTolerated(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{
			"path":    "frontend/src/app/pages/game/game.page.scss",
			"content": "ion-grid { --ion-grid-columns: 4 }\n",
		}})},
		llmtest.Mock{Body: toolResp(t, 5, finishSpec)},
	)
	base := "FAIL  frontend/src/app/pages/login/login.page.spec.ts\n × constrains the login form width\n"
	broke := base + " × the game board renders a grid\n"
	v := &countingVerifier{results: []VerifyResult{redResult(base), redResult(broke), redResult(broke)}}
	loop, _ := newScopedLoop(t, srv, incidentTree(), []string{gameScope}, nil, nil, false, v)

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonSuccess {
		t.Fatalf("a NEW failing test after the edit was tolerated as success:\n%s", rep.String())
	}
	if rep.Baseline != nil && rep.Baseline.Tolerated {
		t.Fatal("Tolerated must be false when a new failure appeared")
	}
}

// TestBaselineFailsOpenWhenVerifyCannotRun: a baseline that cannot be taken must
// change NOTHING. The run then behaves exactly as kloo did before baselines
// existed — judged on the verify alone.
//
// Two ways it cannot be taken, and both are real: the command is not runnable at
// all (VerifyResult.Err), and the shell ran but the RUNNER did not (exit 127 /
// 126 — the setup-error-vs-test-failure distinction lint.go already draws).
func TestBaselineFailsOpenWhenVerifyCannotRun(t *testing.T) {
	cases := []struct {
		name  string
		first VerifyResult
		want  string
	}{
		{"non-runnable", VerifyResult{Command: "npm test", Err: errors.New("exec: npm: not found")}, "verify did not run"},
		{"setup error", VerifyResult{Command: "npm test", ExitCode: 127, Stderr: "sh: npm: not found"}, "not runnable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := llmtest.Sequence(t,
				llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{
					"path":    "frontend/src/app/pages/game/game.page.scss",
					"content": "ion-grid { --ion-grid-columns: 4 }\n",
				}})},
				llmtest.Mock{Body: toolResp(t, 5, finishSpec)},
			)
			// The baseline attempt gets the unusable result; every later verify is the
			// ordinary out-of-scope red that WOULD have been tolerated with a baseline.
			v := &countingVerifier{results: []VerifyResult{tc.first, redResult(karmaRed), redResult(karmaRed)}}
			loop, _ := newScopedLoop(t, srv, incidentTree(), []string{gameScope}, nil, nil, false, v)

			rep, err := loop.Run(context.Background(), "make the game board mobile friendly")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rep.Baseline == nil || rep.Baseline.Taken {
				t.Fatalf("baseline = %+v, want attempted-but-not-taken", rep.Baseline)
			}
			if !strings.Contains(rep.Baseline.Skipped, tc.want) {
				t.Errorf("Skipped = %q, want it to mention %q", rep.Baseline.Skipped, tc.want)
			}
			if rep.Reason == ReasonSuccess {
				t.Fatalf("an unusable baseline must not license a success:\n%s", rep.String())
			}
			// Fail OPEN, not closed: the run proceeds normally rather than erroring out.
			if rep.Reason == ReasonError {
				t.Fatalf("a baseline that could not be taken must not fail the run: %v", rep.Err)
			}
		})
	}
}

// TestNoBaselineOnAnUnscopedRun: with no scope policy there is no such thing as a
// file the agent may not fix, and every kloo-bench case is red at step 0 by
// construction — a baseline that excused pre-existing red there would score the
// whole benchmark as a vacuous pass. AUTO mode must therefore take no baseline at
// all on an unscoped run: no extra verify, no report block, no behaviour change.
func TestNoBaselineOnAnUnscopedRun(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{
			"path": "frontend/src/app/pages/game/game.page.scss", "content": "x{}\n",
		}})},
		llmtest.Mock{Body: toolResp(t, 5, finishSpec)},
	)
	v := &countingVerifier{results: []VerifyResult{redResult(karmaRed)}}
	loop, _ := newScopedLoop(t, srv, incidentTree(), nil, nil, nil, false, v)

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Baseline != nil {
		t.Fatalf("an unscoped run must take no baseline, got %+v", rep.Baseline)
	}
	if rep.Reason == ReasonSuccess {
		t.Fatalf("an unscoped red verify must still fail the run:\n%s", rep.String())
	}
}

// TestVerifyBaselineForcedOnUnscoped: KLOO_VERIFY_BASELINE=1 overrides AUTO, for a
// field where a baseline is wanted on an unscoped tree. It records the baseline,
// and — because tolerance ALSO requires the failure to be out of reach — it still
// cannot hand out a success for an unfixed red.
func TestVerifyBaselineForcedOnUnscoped(t *testing.T) {
	t.Setenv("KLOO_VERIFY_BASELINE", "1")
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{
			"path": "frontend/src/app/pages/game/game.page.scss", "content": "x{}\n",
		}})},
		llmtest.Mock{Body: toolResp(t, 5, finishSpec)},
	)
	v := &countingVerifier{results: []VerifyResult{redResult(karmaRed)}}
	loop, _ := newScopedLoop(t, srv, incidentTree(), nil, nil, nil, false, v)

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Baseline == nil || !rep.Baseline.Taken {
		t.Fatalf("forced-on baseline was not taken: %+v", rep.Baseline)
	}
	if len(rep.Baseline.Unreachable) != 0 {
		t.Fatalf("nothing is unreachable without a scope, got %v", rep.Baseline.Unreachable)
	}
	if rep.Reason == ReasonSuccess {
		t.Fatalf("an unscoped red verify must still fail the run:\n%s", rep.String())
	}
}

// TestVerifyBaselineDisabled: KLOO_VERIFY_BASELINE=-1 restores the pre-baseline
// behaviour exactly, so the whole mechanism can be backed out in the field without
// a new build.
func TestVerifyBaselineDisabled(t *testing.T) {
	t.Setenv("KLOO_VERIFY_BASELINE", "-1")
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{
			"path": "frontend/src/app/pages/game/game.page.scss", "content": "x{}\n",
		}})},
		llmtest.Mock{Body: toolResp(t, 5, finishSpec)},
	)
	v := &countingVerifier{results: []VerifyResult{redResult(karmaRed)}}
	loop, _ := newScopedLoop(t, srv, incidentTree(), []string{gameScope}, nil, nil, false, v)

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Baseline != nil {
		t.Fatalf("disabled baseline still recorded %+v", rep.Baseline)
	}
	if rep.Reason == ReasonSuccess {
		t.Fatal("with the baseline disabled the red verify must sink the run, as before")
	}
}

// TestEnvTriSignConvention pins kloo's knob convention on the new switch: 0 (and
// unset, and garbage) means the COMPUTED default, negative disables, positive forces
// on. Getting this backwards would make an unparseable value a silent disable.
func TestEnvTriSignConvention(t *testing.T) {
	for in, want := range map[string]int{
		"": 0, "0": 0, "nonsense": 0, "auto": 0,
		"1": 1, "on": 1, "true": 1, "yes": 1, "always": 1,
		"-1": -1, "off": -1, "false": -1, "no": -1, "never": -1,
	} {
		t.Setenv("KLOO_TEST_TRI", in)
		if got := envTri("KLOO_TEST_TRI"); got != want {
			t.Errorf("envTri(%q) = %d, want %d", in, got, want)
		}
	}
}

// ─── Bug 2: the corrective must never order an impossible edit ───────────────

// TestOutOfScopeCorrectiveDoesNotDemandAnEdit is the decisive test for the
// off-scope chase. Twelve-read-opening shape: the model reads and reads, the
// explore rail fires, and the only failing test lives entirely outside the allowed
// edit scope.
//
// On HEAD the corrective at that moment was editCorrective, which says "the code
// must change for the failing test to pass … Do not read, do not search, do not
// run a command, do not call finish" — an order the scope would refuse. The model
// obeyed it 21 times. The corrective must instead say so plainly, demand nothing,
// and leave the read tools alone.
func TestOutOfScopeCorrectiveDoesNotDemandAnEdit(t *testing.T) {
	reads := []string{
		"frontend/src/app/pages/game/game.page.ts",
		"frontend/src/app/pages/game/game.page.scss",
		"frontend/src/app/pages/login/login.page.ts",
		"frontend/src/app/pages/login/login.page.scss",
		"frontend/src/app/app.component.ts",
		"frontend/src/app/services/auth.service.ts",
	}
	var mocks []llmtest.Mock
	for _, r := range reads {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": r}})})
	}
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, finishSpec)})
	srv := llmtest.Sequence(t, mocks...)

	v := &countingVerifier{results: []VerifyResult{redResult(karmaRed)}}
	loop, _ := newScopedLoop(t, srv, incidentTree(), []string{gameScope}, nil, nil, false, v)

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly at <=480px")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RailFires[string(RailVerifyOutOfScope)] == 0 {
		t.Fatalf("the out-of-scope verify rail never fired; rails=%v\n%s", rep.RailFires, rep.String())
	}
	corrective := ""
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "ALREADY FAILING before this run started") {
			corrective = m.Content
		}
	}
	if corrective == "" {
		t.Fatalf("no out-of-scope corrective in the transcript\n%s", rep.String())
	}
	// It must NOT demand an edit, and must not close the doors editCorrective closes.
	for _, forbidden := range []string{"make your best edit", "Do not read", "do not search", "do not call finish"} {
		if strings.Contains(corrective, forbidden) {
			t.Errorf("out-of-scope corrective still demands an edit (%q):\n%s", forbidden, corrective)
		}
	}
	// It must name the file the model is not allowed to touch, and the scope it may.
	if !strings.Contains(corrective, "login.page.spec.ts") {
		t.Errorf("corrective does not name the unreachable spec:\n%s", corrective)
	}
	if !strings.Contains(corrective, gameScope) {
		t.Errorf("corrective does not tell the model where it MAY work:\n%s", corrective)
	}
	// And the read tools must not have been withheld: a model that still has a task
	// to finish needs to be able to read.
	if loop.editOnlyLeft != 0 {
		t.Errorf("force-edit rail armed for an impossible edit (editOnlyLeft=%d)", loop.editOnlyLeft)
	}
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "That tool is unavailable on this turn") {
			t.Error("the read tools were withheld while no legal edit existed")
		}
	}
	if rep.ToolCounters.OffScopeEdits != 0 {
		t.Errorf("off_scope_edits = %d; the corrective should not have produced any", rep.ToolCounters.OffScopeEdits)
	}
}

// TestInScopeRedStillDemandsAnEdit is the no-regression twin: when the failing
// test's files ARE writable, kloo must behave exactly as before — the edit rail
// still fires and still demands an edit. The out-of-scope path must be a narrow
// exception, not a general softening of the rail that every other measurement in
// this campaign depends on.
func TestInScopeRedStillDemandsAnEdit(t *testing.T) {
	reads := []string{
		"frontend/src/app/pages/game/game.page.ts",
		"frontend/src/app/pages/game/game.page.scss",
		"frontend/src/app/pages/login/login.page.ts",
		"frontend/src/app/pages/login/login.page.scss",
		"frontend/src/app/app.component.ts",
		"frontend/src/app/services/auth.service.ts",
	}
	var mocks []llmtest.Mock
	for _, r := range reads {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": r}})})
	}
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, finishSpec)})
	srv := llmtest.Sequence(t, mocks...)

	v := &countingVerifier{results: []VerifyResult{redResult(karmaRed)}}
	// Everything under frontend/ is writable, so the login spec is reachable.
	loop, _ := newScopedLoop(t, srv, incidentTree(), []string{"frontend/**"}, nil, nil, false, v)

	rep, err := loop.Run(context.Background(), "fix the login form width")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RailFires[string(RailVerifyOutOfScope)] != 0 {
		t.Fatalf("the out-of-scope rail fired on a reachable failure: %v", rep.RailFires)
	}
	demanded := false
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "make your best edit") {
			demanded = true
		}
	}
	if !demanded {
		t.Fatalf("the edit rail stopped demanding an edit for a failure the agent CAN fix\n%s", rep.String())
	}
}

// ─── The parts that have to work for any of the above to be true ────────────

// TestFailingTestFilesFromKarmaStackFrame: the only place karma's output says WHICH
// file failed is a stack frame, and the test name it prints sits at the end of the
// line where failingAssertions (written for vitest) cannot see it. If the path
// extractor misses that frame, nothing downstream can tell an out-of-scope failure
// from any other — so this is the load-bearing step.
func TestFailingTestFilesFromKarmaStackFrame(t *testing.T) {
	loop, root := newScopedLoop(t, llmtest.Sequence(t), incidentTree(), []string{gameScope}, nil, nil, false, nil)
	_ = root
	files := loop.failingTestFiles(karmaRed)
	if len(files) == 0 {
		t.Fatal("no files extracted from karma output")
	}
	if files[0] != "frontend/src/app/pages/login/login.page.spec.ts" {
		t.Fatalf("files = %v, want the failing spec first", files)
	}
	// The spec's imports count too: a spec kloo may not edit can still be fixable
	// through the source it covers, and only when BOTH are denied is it unreachable.
	wantImport := "frontend/src/app/pages/login/login.page.ts"
	found := false
	for _, f := range files {
		if f == wantImport {
			found = true
		}
	}
	if !found {
		t.Fatalf("files = %v, want the source the spec imports (%s)", files, wantImport)
	}
	if denied := loop.scopeDenies(files); len(denied) != len(files) {
		t.Fatalf("scope denies %d of %d implicated files; the whole set must be denied for the failure to be unreachable",
			len(denied), len(files))
	}
}

// TestFailingTestFilesIgnoresProseAndMissingPaths: the decision this feeds is a
// scope decision, so a token that merely LOOKS like a path must never widen the
// set. Only files that really exist, and really look like tests, count.
func TestFailingTestFilesIgnoresProseAndMissingPaths(t *testing.T) {
	loop, _ := newScopedLoop(t, llmtest.Sequence(t), incidentTree(), []string{gameScope}, nil, nil, false, nil)
	out := "Error: cannot resolve ./nope.spec.ts (see docs/guide.ts) — e.g. version 1.2.ts\n" +
		"    at frontend/src/app/pages/game/game.page.ts:4:1\n"
	if got := loop.failingTestFiles(out); len(got) != 0 {
		t.Fatalf("failingTestFiles = %v, want none (no existing TEST file is named)", got)
	}
}

// TestResolveWorkspacePathShortenedByRunnerCwd: a runner invoked from a subdirectory
// prints paths relative to ITS cwd, which are shorter than the workspace-relative
// ones kloo matches scope globs against (`npm test` inside frontend/ prints
// src/app/…). Resolve those by suffix — but only when unambiguous, because a wrong
// answer here is a wrong scope decision.
func TestResolveWorkspacePathShortenedByRunnerCwd(t *testing.T) {
	loop, _ := newScopedLoop(t, llmtest.Sequence(t), incidentTree(), []string{gameScope}, nil, nil, false, nil)
	out := "FAILED at src/app/pages/login/login.page.spec.ts:53:5\n"
	got := loop.failingTestFiles(out)
	if len(got) == 0 || got[0] != "frontend/src/app/pages/login/login.page.spec.ts" {
		t.Fatalf("failingTestFiles = %v, want the spec resolved by suffix", got)
	}
}

// TestBaselineBuildBreakIsNotBlamedOnTheModel: the build-break corrective opens
// "YOUR LAST EDIT BROKE THE BUILD". When the build was ALREADY broken at the
// baseline that sentence is false, and a corrective that opens with a false
// accusation spends the model's turn on damage it did not do.
func TestBaselineBuildBreakIsNotBlamedOnTheModel(t *testing.T) {
	broken := "frontend/src/app/pages/login/login.page.ts:3:1 - error TS2304: Cannot find name 'Foo'.\n" +
		"    at frontend/src/app/pages/login/login.page.spec.ts:53:5\n"
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{
			"path": "frontend/src/app/pages/game/game.page.scss", "content": "x{}\n",
		}})},
		llmtest.Mock{Body: toolResp(t, 5, finishSpec)},
	)
	v := &countingVerifier{results: []VerifyResult{redResult(broken)}}
	loop, _ := newScopedLoop(t, srv, incidentTree(), []string{gameScope}, nil, nil, false, v)

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Baseline == nil || !rep.Baseline.Taken {
		t.Fatalf("baseline not taken: %+v", rep.Baseline)
	}
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "YOUR LAST EDIT BROKE THE BUILD") {
			t.Fatal("kloo blamed the model for a build that was already broken at baseline")
		}
	}
}

// TestNewFailuresFiltersOnlyBaselineRed is the attribution unit: the corrective may
// name a failure the agent could have caused, and must not name one it inherited.
func TestNewFailuresFiltersOnlyBaselineRed(t *testing.T) {
	b := VerifyBaseline{Taken: true, Failing: []string{"login width", "login a11y"}}
	got := b.newFailures([]string{"login width", "game grid", "login a11y"})
	if len(got) != 1 || got[0] != "game grid" {
		t.Fatalf("newFailures = %v, want only the new one", got)
	}
	// No usable baseline ⇒ the list is passed through untouched, which is exactly the
	// pre-baseline behaviour the fail-open contract promises.
	none := VerifyBaseline{}
	if got := none.newFailures([]string{"a", "b"}); len(got) != 2 {
		t.Fatalf("without a baseline newFailures must pass everything through, got %v", got)
	}
	green := VerifyBaseline{Taken: true, Passed: true}
	if got := green.newFailures([]string{"a"}); len(got) != 1 {
		t.Fatalf("a GREEN baseline excuses nothing, got %v", got)
	}
}

// TestUnchangedSinceRejectsAVerifyThatNeverRan: VerifyResult's zero value is "no
// verify has run", and it has Passed=false. Treating it as agreement with a red
// baseline would hand out a success for a tree nothing ever checked.
func TestUnchangedSinceRejectsAVerifyThatNeverRan(t *testing.T) {
	b := VerifyBaseline{Taken: true, Key: normalizeChurn("x"), Failing: []string{"a"}}
	if b.unchangedSince(VerifyResult{}) {
		t.Fatal("a verify that never ran must not count as the same red")
	}
	if b.unchangedSince(VerifyResult{Command: "t", Err: errors.New("boom")}) {
		t.Fatal("a verify that could not run must not count as the same red")
	}
}

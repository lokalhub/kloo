package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// scopedLoopAt is newScopedLoop with the workspace root supplied by the caller, so
// two runs can be driven over the SAME tree. Needed by the byte-equality test: the
// repo map and the pins carry paths, so a different temp root alone would make two
// prompts differ for a reason that has nothing to do with the thing under test.
func scopedLoopAt(t *testing.T, root string, srv *llmtest.Server, seed map[string]string, allow []string, v Verifier) *Loop {
	t.Helper()
	for rel, content := range seed {
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
		Client:        llm.New(srv.URL+"/v1", "test-model"),
		Adapter:       tools.NativeFCAdapter{},
		Registry:      tools.DefaultRegistry(ws),
		Verifier:      v,
		Budget:        &stubBudget{tripAt: 50},
		Churn:         &stubChurn{},
		Root:          canon,
		Scope:         ws.Scope(),
		Endpoint:      srv.URL + "/v1",
		Model:         "test-model",
		System:        "you are kloo",
		Memory:        NewWorkingMemory(),
		ContextTokens: 1_000_000, // nothing compacts: this measures prompt bytes, not folding
	}
}

// greenScript is a short scripted run: read, edit in scope, finish.
func greenScript(t *testing.T) []llmtest.Mock {
	t.Helper()
	return []llmtest.Mock{
		{Body: toolResp(t, 20, tcSpec{"read_file", map[string]any{"path": "frontend/src/app/pages/game/game.page.ts"}})},
		{Body: toolResp(t, 20, tcSpec{"write_file", map[string]any{
			"path": "frontend/src/app/pages/game/game.page.scss", "content": "ion-grid { --ion-grid-columns: 4 }\n",
		}})},
		{Body: toolResp(t, 20, finishSpec)},
	}
}

// TestGreenBaselineIsAStrictPromptNoOp: when the project's verify is GREEN at the
// start, the baseline must change NOTHING the model sees — not a note, not a pin,
// not a reworded anything. Asserted on the RAW request bodies, byte for byte.
//
// This codebase has a measured history of prompt additions destroying the prefix
// cache (a ~22k-token repo map re-prefilled every call cost ~40% of the runtime
// until it was pinned and frozen). A baseline note on a project with nothing wrong
// with it would be exactly that mistake again, paid by every user whose suite is
// green.
func TestGreenBaselineIsAStrictPromptNoOp(t *testing.T) {
	root := t.TempDir()

	capture := func(disabled bool) []string {
		if disabled {
			t.Setenv("KLOO_VERIFY_BASELINE", "-1")
		} else {
			t.Setenv("KLOO_VERIFY_BASELINE", "")
		}
		srv := llmtest.Sequence(t, greenScript(t)...)
		// Green from the first call, so a baseline probe (if one runs) sees green.
		v := &stubVerifier{results: []VerifyResult{passResult()}}
		loop := scopedLoopAt(t, root, srv, incidentTree(), []string{gameScope}, v)
		if _, err := loop.Run(context.Background(), "make the game board mobile friendly"); err != nil {
			t.Fatalf("run: %v", err)
		}
		var out []string
		for _, b := range srv.ModelCalls() {
			out = append(out, string(b))
		}
		return out
	}

	withFeature := capture(false)
	withoutFeature := capture(true)

	if len(withFeature) != len(withoutFeature) {
		t.Fatalf("different number of model calls: %d with the feature, %d without", len(withFeature), len(withoutFeature))
	}
	if len(withFeature) == 0 {
		t.Fatal("captured no model calls")
	}
	for i := range withFeature {
		if withFeature[i] != withoutFeature[i] {
			t.Fatalf("request %d differs with the baseline feature on a GREEN project.\nwith:\n%s\n\nwithout:\n%s",
				i+1, withFeature[i], withoutFeature[i])
		}
	}
}

// TestGreenBaselineDoesNotInflateVerifyAttempts: verify_attempts must keep meaning
// "checks run on the agent's work". The baseline probe is kloo's own, so it is
// counted separately — otherwise every green project silently gains one and every
// benchmark comparison that reads the field moves under its feet.
func TestGreenBaselineDoesNotInflateVerifyAttempts(t *testing.T) {
	srv := llmtest.Sequence(t, greenScript(t)...)
	v := &stubVerifier{results: []VerifyResult{passResult()}}
	loop := scopedLoopAt(t, t.TempDir(), srv, incidentTree(), []string{gameScope}, v)

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Two verifies of the AGENT's work: one for the edit, one on the finish path. (The
	// mid-loop green stop no longer ends the run on the spot — it spends a turn on the
	// completion probe, and the model's finish then triggers the final verify.) What
	// this test protects is that neither of them is kloo's own baseline probe, which
	// is counted on its own line below.
	if rep.ToolCounters.VerifyAttempts != 2 {
		t.Errorf("verify_attempts = %d, want 2 — the baseline probe must not be folded in",
			rep.ToolCounters.VerifyAttempts)
	}
	if rep.ToolCounters.BaselineVerifyAttempts != 1 {
		t.Errorf("baseline_verify_attempts = %d, want 1 — the probe must be reported, not hidden",
			rep.ToolCounters.BaselineVerifyAttempts)
	}
}

// TestVerifyPinCarriesTheReachVerdict: the pinned last-verify block is re-sent on
// EVERY turn after the first red verify, which makes it the first and most
// persistent thing pointing the model at a failing test — in the incident it named
// an out-of-scope spec on most turns, many turns before any corrective spoke. The
// pin itself has to say the failure is out of reach.
func TestVerifyPinCarriesTheReachVerdict(t *testing.T) {
	v := VerifyResult{Command: "npm test", Passed: false, Stdout: karmaRed}
	note := outOfScopeVerifyNote(
		[]string{"frontend/src/app/pages/login/login.page.spec.ts"},
		[]string{gameScope})
	msg, ok := verifyPin(v, false, nil, note)
	if !ok {
		t.Fatal("no verify pin produced")
	}
	for _, want := range []string{"already there before this run", "NOT allowed to edit", "login.page.spec.ts", gameScope} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("pin does not carry %q:\n%s", want, msg.Content)
		}
	}
	// And a pin with no note is byte-identical to the pre-change pin.
	plain, _ := verifyPin(v, false, nil, "")
	if strings.Contains(plain.Content, "NOT allowed to edit") {
		t.Errorf("an empty note still changed the pin:\n%s", plain.Content)
	}
}

// TestVerifyPinNoteReachesTheWire is the end-to-end half of the above: the note has
// to travel from the loop's reach check into the assembled prompt, not merely exist.
func TestVerifyPinNoteReachesTheWire(t *testing.T) {
	reads := []string{
		"frontend/src/app/pages/game/game.page.ts",
		"frontend/src/app/pages/game/game.page.scss",
		"frontend/src/app/pages/login/login.page.ts",
		"frontend/src/app/pages/login/login.page.scss",
		"frontend/src/app/app.component.ts",
		"frontend/src/app/services/auth.service.ts",
		"frontend/src/app/pages/game/game.page.ts",
	}
	// The pin only exists once a verify has produced a signal, and a verify only runs
	// after a mutation — so the run starts with the in-scope change the task is
	// actually about, and the reads follow.
	mocks := []llmtest.Mock{{Body: toolResp(t, 20, tcSpec{"write_file", map[string]any{
		"path": "frontend/src/app/pages/game/game.page.scss", "content": "@media (max-width: 480px) { ion-grid { --ion-grid-columns: 4 } }\n",
	}})}}
	for _, r := range reads {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 20, tcSpec{"read_file", map[string]any{"path": r}})})
	}
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 20, finishSpec)})
	srv := llmtest.Sequence(t, mocks...)

	v := &stubVerifier{results: []VerifyResult{redResult(karmaRed)}}
	loop := scopedLoopAt(t, t.TempDir(), srv, incidentTree(), []string{gameScope}, v)

	if _, err := loop.Run(context.Background(), "make the game board mobile friendly"); err != nil {
		t.Fatalf("run: %v", err)
	}
	found := false
	for _, b := range srv.ModelCalls() {
		if strings.Contains(string(b), "NOT allowed to edit") {
			found = true
		}
	}
	if !found {
		t.Fatal("the reach note never reached the wire; the pin kept presenting an out-of-scope failure as actionable")
	}
}

// TestDeniedEditDoesNotBecomeTheFileUnderEdit: a refused write must not entrench its
// own target in working memory.
//
// curEditPath was set BEFORE dispatch and unconditionally, so a denied edit became
// the pinned "Current file under edit (re-read fresh from disk)" for every later
// turn — a self-reinforcing loop in which the pin names a forbidden file, the model
// edits it, the write is refused, and the refusal re-pins it. Captured prompts from
// the incident run show exactly that.
func TestDeniedEditDoesNotBecomeTheFileUnderEdit(t *testing.T) {
	srv := llmtest.Sequence(t,
		// Denied: outside the allowed scope.
		llmtest.Mock{Body: toolResp(t, 20, tcSpec{"write_file", map[string]any{
			"path": "frontend/src/app/pages/login/login.page.ts", "content": "export class LoginPage { x = 1 }\n",
		}})},
		llmtest.Mock{Body: toolResp(t, 20, tcSpec{"read_file", map[string]any{"path": "frontend/src/app/pages/game/game.page.ts"}})},
		llmtest.Mock{Body: toolResp(t, 20, finishSpec)},
	)
	v := &stubVerifier{results: []VerifyResult{redResult(karmaRed)}}
	loop := scopedLoopAt(t, t.TempDir(), srv, incidentTree(), []string{gameScope}, v)

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.ToolCounters.OffScopeEdits != 1 {
		t.Fatalf("off_scope_edits = %d, want 1 (the fixture must actually be denied)", rep.ToolCounters.OffScopeEdits)
	}
	for i, b := range srv.ModelCalls() {
		body := string(b)
		if strings.Contains(body, "Current file under edit") && strings.Contains(body, "login.page.ts") {
			t.Fatalf("request %d pins a file kloo had just REFUSED to write:\n%s", i+1, body)
		}
	}
}

// TestEditCorrectiveReportsTheCounterTheNudgeFiredOn: the nudge fires on
// exploreTotal; the message quoted exploreStreak, which every new-ground read RESETS
// to zero. A run that read twelve distinct files was therefore told "You have
// inspected 0 files without changing a single line" — an incoherent number in the one
// message designed to be obeyed, and the exact wording the user's run received.
func TestEditCorrectiveReportsTheCounterTheNudgeFiredOn(t *testing.T) {
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
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 20, tcSpec{"read_file", map[string]any{"path": r}})})
	}
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 20, finishSpec)})
	srv := llmtest.Sequence(t, mocks...)

	// Everything writable, so the ordinary edit corrective is the one that fires.
	v := &stubVerifier{results: []VerifyResult{redResult("FAIL  frontend/src/app/pages/login/login.page.spec.ts\n")}}
	loop := scopedLoopAt(t, t.TempDir(), srv, incidentTree(), []string{"frontend/**"}, v)

	rep, err := loop.Run(context.Background(), "fix the login width")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	corrective := ""
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "without changing a single line") {
			corrective = m.Content
		}
	}
	if corrective == "" {
		t.Fatalf("the edit corrective never fired\n%s", rep.String())
	}
	if strings.Contains(corrective, "inspected 0 files") {
		t.Fatalf("corrective reports a counter that every distinct read resets:\n%s", corrective)
	}
	if !strings.Contains(corrective, "inspected 6 files") {
		t.Fatalf("corrective does not report the six read-only turns it fired on:\n%s", corrective)
	}
	// And the baseline must NOT have pruned the failing list here. The failure is
	// reachable, so it is the agent's to fix, and "Still failing: X" is the measured
	// value of this corrective (kloo-bench A33). A baseline that silently removed it
	// would trade one regression for another.
	if !strings.Contains(corrective, "login.page.spec.ts") {
		t.Fatalf("the corrective no longer names the reachable failure it must fix:\n%s", corrective)
	}
}

// TestFailingAssertionKeepsItsPath: vitest names a failure as
// "× <file> > <test>". Dropping the leading path turned that into
// "> builds the google auth url" — a failure named with the one piece of information
// that would let the model (and kloo's own scope check) see where it lives removed.
// It is also the baseline's identity, so two failures in different files with the
// same test name would compare equal.
func TestFailingAssertionKeepsItsPath(t *testing.T) {
	got := failingAssertions("× src/login/login.page.spec.ts > builds the google auth url\n")
	if len(got) != 1 {
		t.Fatalf("parsed %#v", got)
	}
	if !strings.Contains(got[0], "src/login/login.page.spec.ts") {
		t.Fatalf("the path was dropped: %q", got[0])
	}
	if !strings.Contains(got[0], "builds the google auth url") {
		t.Fatalf("the test name was dropped: %q", got[0])
	}
}

// TestBaselineRedTestThatPassesNeverReturnsToTheExcusedSet: a FLAKY red at baseline
// must not become a permanent amnesty. Once a baseline-red test has been seen
// passing it leaves the excused set for good, so a later failure of the same test is
// a regression and is treated as one.
func TestBaselineRedTestThatPassesNeverReturnsToTheExcusedSet(t *testing.T) {
	b := VerifyBaseline{Taken: true, Failing: []string{"flaky one", "always red"}, Key: normalizeChurn("x")}
	// A verify where "flaky one" passed: it is retired.
	b.retire([]string{"always red"}, false)
	if got := b.newFailures([]string{"flaky one", "always red"}); len(got) != 1 || got[0] != "flaky one" {
		t.Fatalf("a retired baseline failure is still excused: newFailures = %v", got)
	}
	// A whole-green verify retires everything.
	g := VerifyBaseline{Taken: true, Failing: []string{"a", "b"}}
	g.retire(nil, true)
	if len(g.Failing) != 0 {
		t.Fatalf("a green verify must retire the whole excused set, left %v", g.Failing)
	}
	// An unparseable red output (no identities at all) must retire NOTHING: kloo
	// cannot tell which tests now pass, and guessing would un-excuse real baseline red.
	u := VerifyBaseline{Taken: true, Failing: []string{"a", "b"}}
	u.retire(nil, false)
	if len(u.Failing) != 2 {
		t.Fatalf("retired on no evidence, left %v", u.Failing)
	}
}

// TestBaselineEqualRedDoesNotTripTheRepeatedVerifyStop: --stop-on repeated-verify
// counts consecutive identical verify failures. A failure set equal to the baseline
// is not new evidence, and halting on it would end a run that is making correct,
// in-scope progress against a failure it never caused and cannot fix.
func TestBaselineEqualRedDoesNotTripTheRepeatedVerifyStop(t *testing.T) {
	var mocks []llmtest.Mock
	for i := 0; i < 4; i++ {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 20, tcSpec{"write_file", map[string]any{
			"path":    "frontend/src/app/pages/game/game.page.scss",
			"content": strings.Repeat("a", i+1) + "{}\n",
		}})})
	}
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 20, finishSpec)})
	srv := llmtest.Sequence(t, mocks...)

	v := &stubVerifier{results: []VerifyResult{redResult(karmaRed)}}
	loop := scopedLoopAt(t, t.TempDir(), srv, incidentTree(), []string{gameScope}, v)
	loop.StopOn = StopPolicy{RepeatedVerify: 2}

	rep, err := loop.Run(context.Background(), "make the game board mobile friendly")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Reason == ReasonSafetyStop {
		t.Fatalf("the repeated-verify stop fired on the baseline's own red:\n%s", rep.String())
	}
}

// TestBaselineEqualRedIsNotFedToTheChurnRail: the repeated-failure churn rail reads
// the verify output. An unchanged baseline red would read as "same red build every
// step" and halt a run doing correct work — so that output is withheld, exactly as
// it is in unverified mode and on a skipped verify.
func TestBaselineEqualRedIsNotFedToTheChurnRail(t *testing.T) {
	var mocks []llmtest.Mock
	for i := 0; i < 3; i++ {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 20, tcSpec{"write_file", map[string]any{
			"path":    "frontend/src/app/pages/game/game.page.scss",
			"content": strings.Repeat("a", i+1) + "{}\n",
		}})})
	}
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 20, finishSpec)})
	srv := llmtest.Sequence(t, mocks...)

	rc := &recordingChurn{}
	v := &stubVerifier{results: []VerifyResult{redResult(karmaRed)}}
	loop := scopedLoopAt(t, t.TempDir(), srv, incidentTree(), []string{gameScope}, v)
	loop.Churn = rc

	if _, err := loop.Run(context.Background(), "make the game board mobile friendly"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rc.turns) == 0 {
		t.Fatal("the churn rail was never fed")
	}
	for i, turn := range rc.turns {
		if strings.Contains(turn.VerifyOutput, "FAILED") {
			t.Fatalf("turn %d fed the baseline's own red to the churn rail: %q", i+1, turn.VerifyOutput)
		}
	}
}

// TestPrecheckRedIsNotTheSameRedAsAVerifyFailure: a LayeredVerifier precheck failure
// reports the HOOK's command and output and sets FailedStage="precheck". Comparing
// that to a later verify-command failure would be comparing two different commands,
// and would excuse a real verify failure on the strength of a precheck's.
func TestPrecheckRedIsNotTheSameRedAsAVerifyFailure(t *testing.T) {
	b := VerifyBaseline{Taken: true, Key: normalizeChurn("boom"), FailedStage: "precheck"}
	v := VerifyResult{Command: "npm test", ExitCode: 1, Passed: false, Stdout: "boom"}
	if b.unchangedSince(v) {
		t.Fatal("a precheck's red was treated as the verify command's red")
	}
	v.FailedStage = "precheck"
	if !b.unchangedSince(v) {
		t.Fatal("the same precheck red was not recognised")
	}
}

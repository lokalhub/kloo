package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// The seeded fixture repo is generated in-test (git init + one commit) rather
// than committed under testdata/repos/, because a committed nested `.git` cannot
// be stored in the parent repo. The baseline commit gives the checkpoint test a
// git anchor; the suite stays fully deterministic and offline (decisions.md).

const verifyCmd = "grep -qx right answer.txt"

// seedRepo creates a git repo whose answer.txt is "wrong" (so verifyCmd is red)
// and returns the canonical root.
func seedRepo(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("integration suite uses /bin/sh + git")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	gitRun(t, root, "init")
	writeFile(t, filepath.Join(root, "answer.txt"), "wrong\n")
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-m", "seed")
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return canon
}

func gitRun(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeFileCall renders a native tool_calls response for a write_file call,
// optionally with assistant content (a model "claim" the loop must ignore).
func writeFileCall(t *testing.T, content, claim string) string {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"path": "answer.txt", "content": content})
	resp := llm.ChatResponse{
		Choices: []llm.Choice{{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: claim,
			ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{
				Name: "write_file", Arguments: string(args),
			}}},
		}}},
		Usage: llm.Usage{TotalTokens: 50},
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

func readFileCall(t *testing.T) string {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"path": "answer.txt"})
	resp := llm.ChatResponse{
		Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "read_file", Arguments: string(args)}}}}}},
		Usage:   llm.Usage{TotalTokens: 10},
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// buildLoop wires the REAL rails to the repo + mocked server.
func buildLoop(t *testing.T, root string, srv *llmtest.Server, cfg config.Config) *Loop {
	t.Helper()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Loop{
		Client:        llm.New(srv.URL+"/v1", "test-model"),
		Adapter:       tools.NativeFCAdapter{},
		Registry:      tools.DefaultRegistry(ws),
		Verifier:      NewCommandVerifier(ws, verifyCmd),
		Budget:        NewBudget(cfg, time.Now),
		Churn:         NewChurnDetector(cfg.ChurnRounds),
		Checkpoint:    NewGitCheckpointer(root),
		Root:          root,
		ContextTokens: 500,
		System:        "fix the failing check",
	}
}

// 1) drive-to-green: a scripted edit turns the red check green; success is
// decided on the REAL verify signal, not the model's claim.
func TestIntegrationDriveToGreen(t *testing.T) {
	root := seedRepo(t)
	srv := llmtest.Sequence(t,
		// Turn 1: the model CLAIMS success but writes a still-wrong answer.
		llmtest.Mock{Body: writeFileCall(t, "still-wrong\n", "Done! All tests pass. ✅")},
		// Turn 2: actually fixes it.
		llmtest.Mock{Body: writeFileCall(t, "right\n", "")},
	)
	cfg := config.Config{MaxSteps: 10, ChurnRounds: 10}
	loop := buildLoop(t, root, srv, cfg)

	rep, err := loop.Run(context.Background(), "make the check pass")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q (%s), want success", rep.Reason, rep.String())
	}
	// Decided on the real signal: it took TWO turns — the turn-1 claim of success
	// did NOT stop the loop; only the green verify (exit 0) did.
	if rep.Steps != 2 {
		t.Errorf("steps = %d, want 2 (claim must not short-circuit the real verify)", rep.Steps)
	}
	if !rep.FinalVerify.Passed || rep.FinalVerify.ExitCode != 0 {
		t.Errorf("success must rest on a green verify: %+v", rep.FinalVerify)
	}
	if got := readFile(t, filepath.Join(root, "answer.txt")); got != "right\n" {
		t.Errorf("success keeps the edit, answer.txt = %q", got)
	}
	if rep.RolledBack {
		t.Errorf("a successful run must not roll back")
	}
}

// 2) budget-exceeded: a script that never fixes the bug trips maxSteps and stops
// + reports instead of looping forever.
func TestIntegrationBudgetExceeded(t *testing.T) {
	root := seedRepo(t)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: readFileCall(t)}) // read-only, never fixes
	cfg := config.Config{MaxSteps: 2, ChurnRounds: 100}             // churn won't fire first
	loop := buildLoop(t, root, srv, cfg)

	rep, _ := loop.Run(context.Background(), "look around forever")
	if rep.Reason != ReasonBudgetExceeded {
		t.Fatalf("reason = %q, want budget-exceeded (%s)", rep.Reason, rep.String())
	}
	if rep.Budget == nil || rep.Budget.Kind != BudgetSteps {
		t.Errorf("budget evidence should name steps: %+v", rep.Budget)
	}
}

// 3) churn: the same failing edit repeated halts via churn within N rounds.
func TestIntegrationChurn(t *testing.T) {
	root := seedRepo(t)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "still-wrong\n", "")}) // same edit every turn
	cfg := config.Config{MaxSteps: 100, ChurnRounds: 2}
	loop := buildLoop(t, root, srv, cfg)

	rep, _ := loop.Run(context.Background(), "spin on the same edit")
	if rep.Reason != ReasonChurn {
		t.Fatalf("reason = %q, want churn (%s)", rep.Reason, rep.String())
	}
	if rep.Steps >= 100 {
		t.Errorf("churn should halt well before the step budget, steps=%d", rep.Steps)
	}
	// Churn is an abort after an edit → the working tree is rolled back.
	if got := readFile(t, filepath.Join(root, "answer.txt")); got != "wrong\n" {
		t.Errorf("churn abort should roll back the edit, answer.txt = %q", got)
	}
	if !rep.RolledBack {
		t.Errorf("churn abort should report a rollback")
	}
}

// 4a) rollback-on-abort, CLEAN repo: a budget abort after an edit restores the
// committed state.
func TestIntegrationRollbackCleanRepo(t *testing.T) {
	root := seedRepo(t)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "loop-edit\n", "")})
	cfg := config.Config{MaxSteps: 1, ChurnRounds: 100} // one edit, then budget trips
	loop := buildLoop(t, root, srv, cfg)

	rep, _ := loop.Run(context.Background(), "edit then get cut off")
	if rep.Reason != ReasonBudgetExceeded {
		t.Fatalf("reason = %q, want budget-exceeded", rep.Reason)
	}
	if !rep.RolledBack {
		t.Errorf("abort after an edit should roll back")
	}
	if got := readFile(t, filepath.Join(root, "answer.txt")); got != "wrong\n" {
		t.Errorf("clean rollback should restore committed state, got %q", got)
	}
}

// 4b) rollback-on-abort, DIRTY repo: a pre-existing uncommitted change survives
// the rollback; only the loop's edit is reverted.
func TestIntegrationRollbackDirtyRepo(t *testing.T) {
	root := seedRepo(t)
	// Pre-existing uncommitted user change.
	writeFile(t, filepath.Join(root, "answer.txt"), "user-dirty\n")

	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "loop-edit\n", "")})
	cfg := config.Config{MaxSteps: 1, ChurnRounds: 100}
	loop := buildLoop(t, root, srv, cfg)

	rep, _ := loop.Run(context.Background(), "edit a dirty tree then get cut off")
	if !rep.RolledBack {
		t.Errorf("abort after an edit should roll back")
	}
	if got := readFile(t, filepath.Join(root, "answer.txt")); got != "user-dirty\n" {
		t.Errorf("dirty rollback should preserve the user change + drop the loop edit, got %q", got)
	}
}

// 5) interrupt: a cancelled context ends the run as interrupted (and rolls back
// if an edit was already made).
func TestIntegrationInterrupt(t *testing.T) {
	root := seedRepo(t)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "loop-edit\n", "")})
	cfg := config.Config{MaxSteps: 100, ChurnRounds: 100}
	loop := buildLoop(t, root, srv, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // interrupted before the first turn
	rep, _ := loop.Run(ctx, "x")
	if rep.Reason != ReasonInterrupted {
		t.Errorf("reason = %q, want interrupted", rep.Reason)
	}
	// No edit happened (cancelled before acting) → committed state intact.
	if got := readFile(t, filepath.Join(root, "answer.txt")); got != "wrong\n" {
		t.Errorf("answer.txt = %q, want unchanged", got)
	}
}

// 6) repair-then-apply: a no-match edit_file gets a repair observation (actual
// content + fix instruction) fed back; the model's corrected 2nd edit applies and
// the real verify goes green → success. Exercises the REAL rails (DefaultRegistry,
// churn, budget, checkpoint) end-to-end.
func TestIntegrationRepairThenApply(t *testing.T) {
	root := seedRepo(t)
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: editFileCall(t, "answer.txt", "WRONG\n", "right\n", 50)}, // turn 1: no-match
		llmtest.Mock{Body: editFileCall(t, "answer.txt", "wrong\n", "right\n", 50)}, // turn 2: applies
	)
	cfg := config.Config{MaxSteps: 10, ChurnRounds: 10}
	loop := buildLoop(t, root, srv, cfg)

	rep, err := loop.Run(context.Background(), "make the check pass")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q (%s), want success", rep.Reason, rep.String())
	}
	if !msgWithAll(rep.Transcript, "Failing SEARCH block", "wrong", "Fix this edit") {
		t.Errorf("transcript missing the repair observation between the two edits")
	}
	if got := readFile(t, filepath.Join(root, "answer.txt")); got != "right\n" {
		t.Errorf("answer.txt = %q, want %q", got, "right\n")
	}
	if rep.RolledBack {
		t.Errorf("a successful run must not roll back")
	}
}

// 7) repair-bounded: a model that emits the SAME non-matching edit every turn is
// terminated by the real churn rail within the per-target enrichment cap — not an
// infinite loop. At most MaxRepairAttempts (2) enriched observations appear, the
// verify never goes green (file unchanged), and the abort rolls back.
func TestIntegrationRepairBounded(t *testing.T) {
	root := seedRepo(t)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: editFileCall(t, "answer.txt", "DOESNOTMATCH\n", "right\n", 50)})
	cfg := config.Config{MaxSteps: 100, ChurnRounds: 2}
	loop := buildLoop(t, root, srv, cfg)

	rep, err := loop.Run(context.Background(), "spin on a broken edit")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonChurn && rep.Reason != ReasonBudgetExceeded {
		t.Fatalf("reason = %q (%s), want churn or budget-exceeded (terminated, not infinite)", rep.Reason, rep.String())
	}
	if n := countMsgsContaining(rep.Transcript, "Failing SEARCH block"); n > DefaultMaxRepairAttempts {
		t.Errorf("enriched observations = %d, want <= %d", n, DefaultMaxRepairAttempts)
	}
	if got := readFile(t, filepath.Join(root, "answer.txt")); got != "wrong\n" {
		t.Errorf("a never-applied edit must leave answer.txt unchanged, got %q", got)
	}
	if !rep.RolledBack {
		t.Errorf("a non-success terminal after an edit attempt should roll back")
	}
}

// 8) malformed-edit recovery: a structurally malformed edit (gpt-oss's failure —
// duplicated SEARCH markers) gets a FORMAT-correction nudge, and the model uses it
// to retry with a valid block that applies and turns the check green. Proves kloo
// COACHES a weak/reasoner model through a bad edit format instead of letting it
// fail and give up.
func TestIntegrationMalformedEditNudgeRecovers(t *testing.T) {
	root := seedRepo(t)
	// Two SEARCH markers before any divider → ErrMalformedBlock (the gpt-oss shape).
	malformed := "<<<<<<< SEARCH\nwrong\n<<<<<<< SEARCH\n=======\nright\n>>>>>>> REPLACE\n"
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 50, tcSpec{"edit_file", map[string]any{"path": "answer.txt", "diff": malformed}})}, // malformed → nudge
		llmtest.Mock{Body: editFileCall(t, "answer.txt", "wrong\n", "right\n", 50)},                                       // corrected → applies → green
	)
	cfg := config.Config{MaxSteps: 50, ChurnRounds: 5}
	loop := buildLoop(t, root, srv, cfg)

	rep, err := loop.Run(context.Background(), "fix the check, recovering from a bad edit format")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q (%s), want success (recovered via the nudge)", rep.Reason, rep.String())
	}
	if n := countMsgsContaining(rep.Transcript, "MALFORMED"); n == 0 {
		t.Error("expected a malformed-format correction nudge in the transcript")
	}
	if got := readFile(t, filepath.Join(root, "answer.txt")); got != "right\n" {
		t.Errorf("after recovery, answer.txt = %q, want \"right\\n\"", got)
	}
}

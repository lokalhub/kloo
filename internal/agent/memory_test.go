package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/repomap"
	"github.com/lokalhub/kloo/internal/tools"
)

// ─── shared builders for the compaction (B) tests ─────────────────────────────

// editTurn is an (assistant edit_file call, observation) pair — the applied diff
// is kept verbatim in the summary; the result observation is small.
func memEditTurn(t *testing.T, i int) []llm.Message {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"path": fmt.Sprintf("f%d.go", i), "diff": fmt.Sprintf("- old line %d\n+ new line %d", i, i)})
	return []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: tools.NameEditFile, Arguments: string(args)}}}},
		userMsg(fmt.Sprintf("tool edit_file result:\nok %d", i)),
	}
}

// obsTurn is a single distinctive observation message of roughly tokensEach
// tokens (used to drive the transcript past the trigger / window).
func obsTurn(tag string, i, tokensEach int) llm.Message {
	return userMsg(fmt.Sprintf("OBS-%s-%d %s", tag, i, strings.Repeat("word ", tokensEach)))
}

func hasSummary(msgs []llm.Message) (string, bool) {
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, summaryPrefix) {
			return m.Content, true
		}
	}
	return "", false
}

// joinContents concatenates every message's content (for substring assertions).
func joinContents(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

func userMsg(s string) llm.Message      { return llm.Message{Role: llm.RoleUser, Content: s} }
func assistantMsg(s string) llm.Message { return llm.Message{Role: llm.RoleAssistant, Content: s} }

// ─── A1: short transcript ⇒ [task, …tail], exact PromptTokens ──────────────────

func TestMemoryA1ShortTranscript(t *testing.T) {
	task := "make the failing check pass"
	convo := []llm.Message{userMsg(task), assistantMsg("reading the file"), userMsg("tool read_file result:\nx := 1")}

	wm := NewWorkingMemory()
	out, err := wm.Assemble(MemoryInput{Task: task, Convo: convo, WindowTokens: 8000, SystemTokens: 0})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	// [task-pinned, …recent tail] — the whole transcript fits, so nothing trims.
	if len(out) != len(convo) {
		t.Fatalf("len(out) = %d, want %d (%v)", len(out), len(convo), out)
	}
	if out[0].Content != task {
		t.Errorf("out[0] = %q, want the task", out[0].Content)
	}
	for i := range convo {
		if out[i].Content != convo[i].Content {
			t.Errorf("out[%d] = %q, want %q", i, out[i].Content, convo[i].Content)
		}
	}
	// PromptTokens is exact and deterministic: system (0) + Σ ApproxTokens(content).
	want := tokensOf(out)
	if got := wm.Stats().PromptTokens; got != want {
		t.Errorf("PromptTokens = %d, want %d (exact)", got, want)
	}
	if wm.Stats().Compactions != 0 {
		t.Errorf("Compactions = %d, want 0 (T00.1 never compacts)", wm.Stats().Compactions)
	}
}

// TestMemoryHistorySeededAsTail: prior-session History is included (so a follow-up
// has context) and ordered before this run's turns, with the current task pinned.
func TestMemoryHistorySeededAsTail(t *testing.T) {
	task := "what's the issue?"
	history := []llm.Message{
		userMsg("rework the tabs"),
		assistantMsg("[Previous run ended: error after 3 steps. Last verify: go test ./... (exit 1, passed=false).\nno Go files]"),
	}
	convo := []llm.Message{userMsg(task)} // fresh run, no steps yet

	wm := NewWorkingMemory()
	out, err := wm.Assemble(MemoryInput{Task: task, Convo: convo, History: history, WindowTokens: 8000, SystemTokens: 0})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if out[0].Content != task {
		t.Errorf("out[0] = %q, want the current task pinned", out[0].Content)
	}
	joined := joinContents(out)
	if !strings.Contains(joined, "go test ./... (exit 1") || !strings.Contains(joined, "rework the tabs") {
		t.Errorf("history (prior task + outcome) not seeded into the prompt:\n%s", joined)
	}
}

// TestMemoryHistoryCompactedFirst: under window pressure the OLDEST history is the
// first thing folded into the summary, while the current task stays pinned and the
// recent run turns stay verbatim.
func TestMemoryHistoryCompactedFirst(t *testing.T) {
	task := "continue"
	// Bulky prior-session history that can't fit verbatim in a small window.
	var history []llm.Message
	for i := 0; i < 40; i++ {
		history = append(history, obsTurn("HIST", i, 30))
	}
	convo := []llm.Message{userMsg(task), assistantMsg("recent-step-marker")}

	wm := NewWorkingMemory()
	out, err := wm.Assemble(MemoryInput{Task: task, Convo: convo, History: history, WindowTokens: 600, SystemTokens: 50})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if out[0].Content != task {
		t.Errorf("task not pinned under pressure: out[0]=%q", out[0].Content)
	}
	if _, ok := hasSummary(out); !ok {
		t.Errorf("expected a running summary when history overflows the window")
	}
	if wm.Stats().Compactions == 0 {
		t.Errorf("expected a compaction (history overflowed the window)")
	}
	// The newest run turn survives verbatim (it's the freshest context).
	if !strings.Contains(joinContents(out), "recent-step-marker") {
		t.Errorf("most-recent run turn was dropped; it should survive verbatim")
	}
}

// ─── A2: task is always first, regardless of tail length ──────────────────────

func TestMemoryA2TaskAlwaysFirst(t *testing.T) {
	task := "the goal that must never be dropped"
	convo := []llm.Message{userMsg(task)}
	// A long tail of bulky turns, forced to trim against a tiny window.
	for i := 0; i < 200; i++ {
		convo = append(convo, userMsg(fmt.Sprintf("turn %d: %s", i, strings.Repeat("filler ", 40))))
	}

	wm := NewWorkingMemory()
	out, err := wm.Assemble(MemoryInput{Task: task, Convo: convo, WindowTokens: 400, SystemTokens: 10})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(out) == 0 || out[0].Content != task {
		t.Fatalf("task must be first regardless of tail length, got %v", out)
	}
	// The undroppable goal is retained and the hard ceiling holds even when a huge
	// tail forces compaction + shedding.
	if wm.Stats().PromptTokens > 400 {
		t.Errorf("PromptTokens = %d, want ≤ window (400)", wm.Stats().PromptTokens)
	}
}

// ─── A3: last-verify pin is synthesized, verbatim failing tail ────────────────

func TestMemoryA3VerifyPin(t *testing.T) {
	task := "fix it"
	failTail := "--- FAIL: TestThing (0.00s)\n    want 2, got 1"
	v := VerifyResult{Command: "go test ./...", ExitCode: 1, Passed: false, Stdout: failTail}

	wm := NewWorkingMemory()
	out, err := wm.Assemble(MemoryInput{Task: task, Convo: []llm.Message{userMsg(task)}, LastVerify: v, WindowTokens: 8000})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	joined := joinContents(out)
	for _, want := range []string{"go test ./...", "passed=false", failTail} {
		if !strings.Contains(joined, want) {
			t.Errorf("verify pin missing %q:\n%s", want, joined)
		}
	}
}

// ─── A4: current-file pin is FRESH; the stale read-dump is dropped ────────────

func TestMemoryA4FreshFileNoStaleDump(t *testing.T) {
	task := "edit foo.go"
	readArgs, _ := json.Marshal(map[string]any{"path": "foo.go"})
	convo := []llm.Message{
		userMsg(task),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: tools.NameReadFile, Arguments: string(readArgs)}}}},
		userMsg("tool read_file result:\nOLD-STALE-CONTENT"),
		assistantMsg("now editing"),
	}

	wm := NewWorkingMemory()
	out, err := wm.Assemble(MemoryInput{Task: task, Convo: convo, EditPath: "foo.go", FreshFile: "NEW-FRESH-CONTENT", WindowTokens: 8000})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	joined := joinContents(out)
	if !strings.Contains(joined, "NEW-FRESH-CONTENT") {
		t.Errorf("assembled set must contain the FRESH file content:\n%s", joined)
	}
	if strings.Contains(joined, "OLD-STALE-CONTENT") {
		t.Errorf("assembled set must NOT contain the stale read-dump of the edited file:\n%s", joined)
	}
}

// ─── A5 / A6 helpers: a temp repo whose map exceeds the shrunk budget ─────────

// genRepo writes nFiles Go files (nFuncs functions each) so the repo map is big
// enough that the full-window budget and the shrunk mapBudgetFrac budget produce
// DIFFERENT maps — otherwise A5/A6 could not tell the two budgets apart.
func genRepo(t *testing.T, nFiles, nFuncs int) string {
	t.Helper()
	root := t.TempDir()
	for f := 0; f < nFiles; f++ {
		var b strings.Builder
		fmt.Fprintf(&b, "package pkg%d\n\n", f)
		for fn := 0; fn < nFuncs; fn++ {
			fmt.Fprintf(&b, "func File%dFunc%dDoesSomething() int { return %d }\n", f, fn, fn)
		}
		path := filepath.Join(root, fmt.Sprintf("file%02d.go", f))
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return canon
}

// sentSystem parses the system message content from the first captured request.
func sentSystem(t *testing.T, srv *llmtest.Server) string {
	t.Helper()
	reqs := srv.Requests()
	if len(reqs) == 0 {
		t.Fatal("no request captured")
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqs[0], &body); err != nil {
		t.Fatalf("request not JSON: %v", err)
	}
	if len(body.Messages) == 0 || body.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("first message is not the system prompt: %+v", body.Messages)
	}
	return body.Messages[0].Content
}

func memLoop(t *testing.T, srv *llmtest.Server, root string, ctxTokens int, mem WorkingMemory) *Loop {
	t.Helper()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Loop{
		Client:        llm.New(srv.URL+"/v1", "test-model"),
		Adapter:       tools.NativeFCAdapter{},
		Registry:      tools.DefaultRegistry(ws),
		Verifier:      &stubVerifier{results: []VerifyResult{passResult()}},
		Budget:        NewBudget(config.Config{MaxSteps: 5}, time.Now),
		Churn:         &stubChurn{},
		Root:          root,
		ContextTokens: ctxTokens,
		System:        "you are kloo",
		Memory:        mem,
	}
}

// ─── A5: Memory==nil ⇒ full msgs byte-identical (full map budget + boundedHistory)

func TestMemoryA5LegacyPathByteIdentical(t *testing.T) {
	root := genRepo(t, 14, 12)
	const ctxTokens = 2000
	task := "look at File3Func2DoesSomething"

	// Guard: the full-window map and the shrunk map MUST differ, else the test
	// could not distinguish the legacy (full) budget from the memory (shrunk) one.
	probe := &Loop{Root: root, System: "you are kloo"}
	full := probe.systemWithContext(task, ctxTokens)
	shrunk := probe.systemWithContext(task, mapBudgetTokens(ctxTokens))
	if full == shrunk {
		t.Fatalf("repo map too small to exercise the budget split (full==shrunk); raise nFiles/nFuncs")
	}

	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "file00.go"}})})
	loop := memLoop(t, srv, root, ctxTokens, nil) // Memory == nil ⇒ legacy path

	if _, err := loop.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The legacy path must build the system prompt with the FULL window budget,
	// byte-identical to pre-P00 (the Lead-1 fix: do not shrink the map when off).
	if got := sentSystem(t, srv); got != full {
		t.Errorf("legacy system prompt is not the full-budget map (map silently shrank)\n got len=%d\nwant len=%d", len(got), len(full))
	}
}

// ─── A6: Memory!=nil ⇒ shrunk map budget + hot cap (guards the headline bug) ───

func TestMemoryA6MemoryPathBudgets(t *testing.T) {
	root := genRepo(t, 14, 12)
	const ctxTokens = 2000
	task := "look at File3Func2DoesSomething"

	probe := &Loop{Root: root, System: "you are kloo"}
	full := probe.systemWithContext(task, ctxTokens)
	shrunk := probe.systemWithContext(task, mapBudgetTokens(ctxTokens))
	if full == shrunk {
		t.Fatalf("repo map too small to exercise the budget split; raise nFiles/nFuncs")
	}

	wm := NewWorkingMemory()
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "file00.go"}})})
	loop := memLoop(t, srv, root, ctxTokens, wm)

	if _, err := loop.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The repo-map curator at the act() call site got mapBudgetTokens(window),
	// NOT the full window (guards the "map eats the whole window" bug directly).
	if got := sentSystem(t, srv); got != shrunk {
		t.Errorf("memory-path system prompt did not use mapBudgetTokens(window)")
	}
	st := wm.Stats()
	if st.MapBudget != mapBudgetTokens(ctxTokens) {
		t.Errorf("Stats().MapBudget = %d, want %d", st.MapBudget, mapBudgetTokens(ctxTokens))
	}
	if st.HotBudget != hotBudgetTokens(ctxTokens) {
		t.Errorf("Stats().HotBudget = %d, want %d", st.HotBudget, hotBudgetTokens(ctxTokens))
	}
	// And the assembled history confirms the map budget really is < the window.
	if mapBudgetTokens(ctxTokens) >= ctxTokens {
		t.Errorf("mapBudgetTokens(%d) = %d must be < the window", ctxTokens, mapBudgetTokens(ctxTokens))
	}
	_ = repomap.ApproxTokens // map math is the shared estimator (no tokenizer dep)
}

// ─── B1: under the trigger ⇒ no compaction, no summary slot ────────────────────

func TestMemoryB1NoCompactionUnderTrigger(t *testing.T) {
	task := "fix the failing check"
	convo := []llm.Message{userMsg(task), assistantMsg("looking"), userMsg("tool read_file result:\nshort")}

	wm := NewWorkingMemory()
	out, err := wm.Assemble(MemoryInput{Task: task, Convo: convo, WindowTokens: 8000, SystemTokens: 100})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if wm.Stats().Compactions != 0 {
		t.Errorf("Compactions = %d, want 0 (well under the 70%% trigger)", wm.Stats().Compactions)
	}
	if _, ok := hasSummary(out); ok {
		t.Errorf("no summary slot should be emitted under the trigger:\n%s", joinContents(out))
	}
}

// ─── B2: crossing the 70% trigger ⇒ one compaction, summary, strict ceiling ────

func TestMemoryB2CompactionTripsAndHoldsCeiling(t *testing.T) {
	const window = 2000
	task := "fix"
	convo := []llm.Message{userMsg(task)}
	for i := 0; i < 60; i++ {
		convo = append(convo, memEditTurn(t, i)...)
		convo = append(convo, obsTurn("pad", i, 20)) // heavy enough to push the transcript past the trigger
	}

	wm := NewWorkingMemory()
	out, err := wm.Assemble(MemoryInput{Task: task, Convo: convo, WindowTokens: window, SystemTokens: 100})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if wm.Stats().Compactions != 1 {
		t.Errorf("Compactions = %d, want 1", wm.Stats().Compactions)
	}
	if _, ok := hasSummary(out); !ok {
		t.Errorf("a compaction must emit a running-summary slot")
	}
	if got := wm.Stats().PromptTokens; got > window {
		t.Errorf("PromptTokens = %d, want ≤ window (%d) — the hard ceiling, strict", got, window)
	}
}

// ─── B3: keep diffs + verify tails verbatim; stub raw read dumps ───────────────

func TestMemoryB3ClassifyVerbatimVsStub(t *testing.T) {
	const window = 2000
	task := "fix"
	editArgs, _ := json.Marshal(map[string]any{"path": "a.go", "diff": "- bad\n+ good"})
	readArgs, _ := json.Marshal(map[string]any{"path": "big.txt"})
	bigDump := "tool read_file result:\n" + strings.Repeat("x", 5000)

	convo := []llm.Message{userMsg(task)}
	// The distinctive cold turns (oldest ⇒ folded into the summary):
	convo = append(convo,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: tools.NameEditFile, Arguments: string(editArgs)}}}},
		userMsg("tool edit_file result:\nok"),
		userMsg("--- FAIL: TestThing\n    want 1, got 2"), // small verify-fail tail (kept verbatim)
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: tools.NameReadFile, Arguments: string(readArgs)}}}},
		userMsg(bigDump),
	)
	// Padding AFTER, so the distinctive turns are cold and the trigger is crossed.
	for i := 0; i < 40; i++ {
		convo = append(convo, obsTurn("pad", i, 10))
	}

	wm := NewWorkingMemory()
	out, err := wm.Assemble(MemoryInput{Task: task, Convo: convo, WindowTokens: window, SystemTokens: 100})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	summary, ok := hasSummary(out)
	if !ok {
		t.Fatalf("expected a summary slot")
	}
	for _, want := range []string{"edit_file a.go", "- bad", "+ good", "FAIL: TestThing", "want 1, got 2"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary must keep %q verbatim:\n%s", want, summary)
		}
	}
	if strings.Contains(joinContents(out), strings.Repeat("x", 100)) {
		t.Errorf("the raw 5k read dump must be stubbed, not stored")
	}
	if !strings.Contains(summary, "[read big.txt:") {
		t.Errorf("the read dump must be replaced by a one-line stub:\n%s", summary)
	}
}

// ─── B4: bounded every turn while a no-memory control overflows ────────────────

func TestMemoryB4MonotonicBoundVsControl(t *testing.T) {
	const window = 2000
	const sys = 100
	task := "fix"
	convo := []llm.Message{userMsg(task)}

	wm := NewWorkingMemory()
	controlOverflowed := false
	for i := 0; i < 90; i++ {
		convo = append(convo, obsTurn("turn", i, 20))
		_, err := wm.Assemble(MemoryInput{Task: task, Convo: convo, WindowTokens: window, SystemTokens: sys})
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if got := wm.Stats().PromptTokens; got > window {
			t.Fatalf("turn %d: PromptTokens = %d exceeded window %d", i, got, window)
		}
		if control := sys + tokensOf(convo); control > window {
			controlOverflowed = true // a no-memory prompt (system + full transcript) would overflow
		}
	}
	if !controlOverflowed {
		t.Errorf("the no-memory control never overflowed — the run was too short to prove the bound")
	}
}

// ─── B5: idempotence ──────────────────────────────────────────────────────────

func TestMemoryB5Idempotent(t *testing.T) {
	const window = 1500
	task := "fix"
	convo := []llm.Message{userMsg(task)}
	for i := 0; i < 50; i++ {
		convo = append(convo, memEditTurn(t, i)...)
	}
	in := MemoryInput{Task: task, Convo: convo, WindowTokens: window, SystemTokens: 80}

	wm := NewWorkingMemory()
	out1, err1 := wm.Assemble(in)
	out2, err2 := wm.Assemble(in)
	if err1 != nil || err2 != nil {
		t.Fatalf("errs: %v %v", err1, err2)
	}
	if len(out1) != len(out2) {
		t.Fatalf("non-deterministic length: %d vs %d", len(out1), len(out2))
	}
	for i := range out1 {
		if out1[i].Content != out2[i].Content || out1[i].Role != out2[i].Role {
			t.Errorf("message %d differs across identical Assemble calls", i)
		}
	}
}

// ─── B6: one oversized cold item ⇒ truncated-with-marker, never dropped/whole ──

func TestMemoryB6OversizedItemTruncatedNotDropped(t *testing.T) {
	const window = 200
	task := "fix"
	head := "HEAD-FRAGMENT-KEEPME"
	tailFrag := "TAIL-FRAGMENT-KEEPME"
	big := head + strings.Repeat(" mid", 3000) + tailFrag // tokens ≫ window

	convo := []llm.Message{
		userMsg(task),
		userMsg(big),                // the single oversized cold item (oldest)
		userMsg("recent tiny turn"), // the recent tail
	}
	wm := NewWorkingMemory()
	out, err := wm.Assemble(MemoryInput{Task: task, Convo: convo, WindowTokens: window, SystemTokens: 0})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	joined := joinContents(out)
	if got := wm.Stats().PromptTokens; got > window {
		t.Errorf("PromptTokens = %d, want ≤ window %d", got, window)
	}
	if !strings.Contains(joined, head) || !strings.Contains(joined, tailFrag) {
		t.Errorf("head and tail fragments of the oversized item must survive:\n%s", joined)
	}
	if !strings.Contains(joined, "truncated") {
		t.Errorf("a truncation marker must be present (not dropped):\n%s", joined)
	}
	if strings.Contains(joined, strings.Repeat(" mid", 200)) {
		t.Errorf("the middle of the oversized item must be elided, not kept whole")
	}
}

// ─── B7: deterministic shed order summary → tail → file → verify; task retained ─

func TestMemoryB7ShedOrder(t *testing.T) {
	task := "GOAL-NEVER-DROP"
	verify := VerifyResult{Command: "go test", Passed: false, Stdout: "VERIFY-TAIL"}
	editPath, fresh := "cur.go", "FILE-PIN-BODY"

	// A big cold middle of heavy observations (folded → a large summary) and a
	// fixed, light recent tail of distinctive turns. A deliberately large system
	// prompt (300 tok) makes the post-summary space tight, so a small window
	// forces phase-3 tail shedding (otherwise 0.35×window tail always fits).
	build := func() []llm.Message {
		convo := []llm.Message{userMsg(task)}
		for i := 0; i < 40; i++ { // cold middle ⇒ a big summary
			convo = append(convo, obsTurn("COLD", i, 60))
		}
		for k := 0; k < 6; k++ { // distinctive recent tail turns (light)
			convo = append(convo, obsTurn("TAILTURN", k, 10))
		}
		return convo
	}
	mkInput := func(window int) MemoryInput {
		return MemoryInput{Task: task, Convo: build(), LastVerify: verify, EditPath: editPath, FreshFile: fresh, WindowTokens: window, SystemTokens: 300}
	}

	// (a) Mild overflow: shedding the summary alone suffices ⇒ tail + both pins intact.
	wmA := NewWorkingMemory()
	outA, err := wmA.Assemble(mkInput(1500))
	if err != nil {
		t.Fatalf("(a): %v", err)
	}
	jA := joinContents(outA)
	if wmA.Stats().PromptTokens > 1500 {
		t.Errorf("(a) ceiling: %d > 1500", wmA.Stats().PromptTokens)
	}
	if !strings.Contains(jA, task) {
		t.Errorf("(a) task must always be retained")
	}
	for k := 0; k < 6; k++ {
		if !strings.Contains(jA, fmt.Sprintf("OBS-TAILTURN-%d", k)) {
			t.Errorf("(a) summary must shed before the tail — tail turn %d was dropped:\n%s", k, jA)
		}
	}
	if !strings.Contains(jA, "FILE-PIN-BODY") || !strings.Contains(jA, "VERIFY-TAIL") {
		t.Errorf("(a) summary must shed before the file/verify pins — a pin was dropped")
	}

	// (b) Tighter window: the tail must shed too, but the file/verify pins survive.
	wmB := NewWorkingMemory()
	outB, err := wmB.Assemble(mkInput(400))
	if err != nil {
		t.Fatalf("(b): %v", err)
	}
	jB := joinContents(outB)
	if wmB.Stats().PromptTokens > 400 {
		t.Errorf("(b) ceiling: %d > 400", wmB.Stats().PromptTokens)
	}
	if !strings.Contains(jB, task) {
		t.Errorf("(b) task must always be retained")
	}
	tailPresent := 0
	for k := 0; k < 6; k++ {
		if strings.Contains(jB, fmt.Sprintf("OBS-TAILTURN-%d", k)) {
			tailPresent++
		}
	}
	if tailPresent == 6 {
		t.Errorf("(b) a tight window should have shed some tail turns")
	}
	if !strings.Contains(jB, "FILE-PIN-BODY") || !strings.Contains(jB, "VERIFY-TAIL") {
		t.Errorf("(b) tail must shed before the file/verify pins:\n%s", jB)
	}
}

// ─── B8 (unit): sub-floor window ⇒ ErrWindowTooSmall, task not dropped ─────────

func TestMemoryB8SubFloorWindow(t *testing.T) {
	task := strings.Repeat("important goal ", 40) // ~150 tokens
	wm := NewWorkingMemory()
	out, err := wm.Assemble(MemoryInput{Task: task, Convo: []llm.Message{userMsg(task)}, WindowTokens: 10, SystemTokens: 50})
	if !errors.Is(err, ErrWindowTooSmall) {
		t.Fatalf("err = %v, want ErrWindowTooSmall", err)
	}
	if out != nil {
		t.Errorf("a sub-floor window must not return an over-ceiling prompt, got %d msgs", len(out))
	}
}

// ─── B8 (loop): ErrWindowTooSmall becomes a ReasonError stop ──────────────────

func TestMemoryB8LoopReasonError(t *testing.T) {
	root := genRepo(t, 2, 2)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "file00.go"}})})
	loop := memLoop(t, srv, root, 10, NewWorkingMemory()) // window 10 < floor
	loop.System = strings.Repeat("you are kloo and you do many things ", 20)

	rep, err := loop.Run(context.Background(), strings.Repeat("big task goal ", 20))
	if err != nil {
		t.Fatalf("Run returned a setup error: %v", err)
	}
	if rep.Reason != ReasonError {
		t.Fatalf("reason = %q, want error (config: window below floor)", rep.Reason)
	}
	if !errors.Is(rep.Err, ErrWindowTooSmall) {
		t.Errorf("rep.Err = %v, want ErrWindowTooSmall", rep.Err)
	}
}

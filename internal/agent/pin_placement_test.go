package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/repomap"
	"github.com/lokalhub/kloo/internal/tools"
)

// ─── Phase 01 Task 03: pins below the tail, and an append-only tail ─────────

// shedPressureInput builds one fixed, window-pressured assembly. Both
// TestShedOrderUnchangedByPinPlacement and its baseline capture use it, so the
// golden numbers below describe exactly this input and nothing else.
func shedPressureInput() MemoryInput {
	task := "make the failing check pass"
	convo := []llm.Message{userMsg(task)}
	for i := range 40 {
		convo = append(convo,
			assistantMsg(fmt.Sprintf("turn %d: inspecting the tree and planning the next edit", i)),
			userMsg(fmt.Sprintf("tool read_file result:\n%s", strings.Repeat(fmt.Sprintf("line %d of a long file dump\n", i), 12))),
		)
	}
	return MemoryInput{
		Task:         task,
		Convo:        convo,
		LastVerify:   VerifyResult{Command: "go test ./...", ExitCode: 1, Passed: false, Stdout: strings.Repeat("FAIL: TestThing\n", 20)},
		EditPath:     "pkg/thing.go",
		FreshFile:    strings.Repeat("func Thing() int { return 0 }\n", 30),
		WindowTokens: 4000,
		SystemTokens: 300,
		MapBudget:    500,
	}
}

// TestShedOrderUnchangedByPinPlacement: moving the pins below the tail must not
// change the token arithmetic. Assemble's shed steps operate on totals, not
// indices, so the same input under the same window must shed the same amount and
// land on the same MemoryStats.
//
// The literals are the numbers this exact input produced at v0.16.7 (commit
// 60bf66a), captured 2026-09-11 by running this same function on a clean baseline
// worktree before the pin move landed.
func TestShedOrderUnchangedByPinPlacement(t *testing.T) {
	const (
		baselinePromptTokens  = 2071
		baselineWindowTokens  = 4000
		baselineSummaryTokens = 386
		baselineDroppedTurns  = 62
		baselineHotBudget     = 1400
		baselineTrimmedTail   = false
	)

	wm := NewWorkingMemory()
	out, err := wm.Assemble(shedPressureInput())
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("Assemble returned no messages")
	}
	got := wm.Stats()

	if got.PromptTokens != baselinePromptTokens {
		t.Errorf("PromptTokens = %d, want %d (v0.16.7 literal) — pin placement changed the token math",
			got.PromptTokens, baselinePromptTokens)
	}
	if got.WindowTokens != baselineWindowTokens {
		t.Errorf("WindowTokens = %d, want %d", got.WindowTokens, baselineWindowTokens)
	}
	if got.SummaryTokens != baselineSummaryTokens {
		t.Errorf("SummaryTokens = %d, want %d", got.SummaryTokens, baselineSummaryTokens)
	}
	if got.DroppedTurns != baselineDroppedTurns {
		t.Errorf("DroppedTurns = %d, want %d — the shed order changed", got.DroppedTurns, baselineDroppedTurns)
	}
	if got.HotBudget != baselineHotBudget {
		t.Errorf("HotBudget = %d, want %d", got.HotBudget, baselineHotBudget)
	}
	if got.TrimmedTail != baselineTrimmedTail {
		t.Errorf("TrimmedTail = %t, want %t", got.TrimmedTail, baselineTrimmedTail)
	}
	if got.PromptTokens > got.WindowTokens {
		t.Errorf("assembly broke the hard ceiling: %d > %d", got.PromptTokens, got.WindowTokens)
	}
}

// TestAssembleOrderPinsAfterTail: the direct unit assertion on assemble()'s output
// order — task, summary, tail, pins — including the no-pins and pins-only cases.
// This is the ordering the whole phase rests on: everything volatile last.
func TestAssembleOrderPinsAfterTail(t *testing.T) {
	task := userMsg("the task")
	pins := []llm.Message{userMsg("PIN-verify"), userMsg("PIN-file")}
	tail := []llm.Message{assistantMsg("TAIL-1"), userMsg("TAIL-2")}

	cases := []struct {
		name    string
		summary []string
		pins    []llm.Message
		tail    []llm.Message
		want    []string
	}{
		{
			name: "task, tail, pins",
			pins: pins, tail: tail,
			want: []string{"the task", "TAIL-1", "TAIL-2", "PIN-verify", "PIN-file"},
		},
		{
			name:    "summary sits directly after the task, above the tail",
			summary: []string{"older turns"}, pins: pins, tail: tail,
			want: []string{"the task", summaryPrefix + "older turns", "TAIL-1", "TAIL-2", "PIN-verify", "PIN-file"},
		},
		{
			name: "no pins",
			tail: tail,
			want: []string{"the task", "TAIL-1", "TAIL-2"},
		},
		{
			name: "pins only",
			pins: pins,
			want: []string{"the task", "PIN-verify", "PIN-file"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := assemble(task, tc.summary, tc.pins, tc.tail)
			got := make([]string, 0, len(out))
			for _, m := range out {
				got = append(got, m.Content)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("order = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("order = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// TestSentMessageNeverDisappears: with editPath changing across turns, every
// observation that was ever assembled is still assembled — the tail is a pure
// function of the transcript. Before Phase 01 the tail dropped read-dumps of the
// file currently under edit, so a message vanished from the middle of the prompt
// the moment the edit path moved to its file, and came back when it moved away.
func TestSentMessageNeverDisappears(t *testing.T) {
	task := "fix both files"
	readArgs := func(p string) string {
		b, _ := json.Marshal(map[string]any{"path": p})
		return string(b)
	}
	readCall := func(p string) llm.Message {
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{Function: llm.FunctionCall{Name: tools.NameReadFile, Arguments: readArgs(p)}},
		}}
	}
	convo := []llm.Message{
		userMsg(task),
		readCall("a.go"), userMsg("tool read_file result:\nDUMP-OF-A"),
		readCall("b.go"), userMsg("tool read_file result:\nDUMP-OF-B"),
		assistantMsg("editing now"),
	}

	// The edit path walks across both files, and then off them entirely.
	for _, editPath := range []string{"", "a.go", "b.go", "a.go", "c.go"} {
		wm := NewWorkingMemory()
		out, err := wm.Assemble(MemoryInput{
			Task: task, Convo: convo, EditPath: editPath,
			FreshFile: "FRESH", WindowTokens: 100000,
		})
		if err != nil {
			t.Fatalf("editPath=%q Assemble: %v", editPath, err)
		}
		joined := joinContents(out)
		for _, want := range []string{"DUMP-OF-A", "DUMP-OF-B"} {
			if !strings.Contains(joined, want) {
				t.Errorf("editPath=%q: %s was dropped from the tail — a sent message must never disappear:\n%s",
					editPath, want, joined)
			}
		}
	}
}

// TestCompactionStillFoldsColdMiddle: removing the retroactive deletion must not
// weaken the legitimate one. Under window pressure the cold middle is still folded
// into the running summary, which is the sanctioned way a message leaves the hot
// prompt.
func TestCompactionStillFoldsColdMiddle(t *testing.T) {
	in := shedPressureInput()
	wm := NewWorkingMemory()
	out, err := wm.Assemble(in)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	st := wm.Stats()

	if st.Compactions < 1 {
		t.Fatalf("Compactions = %d, want at least 1 — this input must compact", st.Compactions)
	}
	if st.DroppedTurns < 1 {
		t.Errorf("DroppedTurns = %d, want at least 1 — the cold middle was not folded", st.DroppedTurns)
	}
	if st.PromptTokens > in.WindowTokens {
		t.Errorf("assembly broke the hard ceiling: %d > %d", st.PromptTokens, in.WindowTokens)
	}
	if len(out) == 0 {
		t.Fatal("Assemble returned no messages")
	}
	if out[0].Content != in.Task {
		t.Errorf("the task must still lead the history, got %.60q", out[0].Content)
	}
}

// ─── the measured cost of keeping the duplicate read dumps ──────────────────

// v0167StaleReadDumps is v0.16.7's removed filter, kept here as the REFERENCE
// implementation so the cost of no longer applying it can be measured rather than
// argued. It is test-only; production has no such function any more.
func v0167StaleReadDumps(convo []llm.Message, editPath string) map[int]bool {
	stale := map[int]bool{}
	if editPath == "" {
		return stale
	}
	for i, m := range convo {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == tools.NameReadFile && argString(tc.Function.Arguments, "path") == editPath {
				if i+1 < len(convo) {
					stale[i+1] = true
				}
			}
		}
	}
	return stale
}

// v0167RecentTail reproduces v0.16.7's recentTail — the tail WITH the retroactive
// deletion applied — so the two tails can be assembled side by side.
func v0167RecentTail(convo []llm.Message, editPath string) []llm.Message {
	if len(convo) <= 1 {
		return nil
	}
	stale := v0167StaleReadDumps(convo, editPath)
	out := make([]llm.Message, 0, len(convo)-1)
	for i := 1; i < len(convo); i++ {
		if stale[i] {
			continue
		}
		out = append(out, convo[i])
	}
	return out
}

// TestTailGrowthFromKeptReadDumps bounds what §3's trade actually costs. Keeping
// the duplicate read dumps is the price of an append-only prefix; this measures
// that price per turn over the Task 02 harness fixture and fails if the worst turn
// grows by more than the threshold.
//
// The 5% threshold is the figure Phase 01 Task 03 fixes as the bound. If real
// growth ever exceeds it, the fix is the compactor fallback described there —
// never raising this number.
//
// The denominator is the WHOLE prompt: the messages plus the tool schemas. That is
// what (*Loop).act itself counts (lastPromptChars = messageChars + toolSchemaChars)
// and what the provider bills as prompt_tokens. Measuring the messages alone would
// put a ~1.4k-token schema block outside the denominator and inflate every ratio by
// an order of magnitude — on this fixture, 1.64% becomes 25%. See decisions.md.
func TestTailGrowthFromKeptReadDumps(t *testing.T) {
	const maxWorstTurnGrowthPct = 5.0

	prompts, schemaTokens := capturePromptsWithSchemas(t)
	convo, editPaths := harnessTranscript(t)
	if len(editPaths) < 6 {
		t.Fatalf("fixture yielded %d turns, want at least 6", len(editPaths))
	}
	est := repomap.ApproxTokens

	var report strings.Builder
	worst, worstTurn := 0.0, 0
	for turn := range prompts {
		if turn >= len(editPaths) {
			break
		}
		// after: the prompt kloo actually sent this turn, schemas included.
		after := schemaTokens
		for _, m := range prompts[turn] {
			after += est(m.content)
		}
		// before: the same prompt with the dumps v0.16.7 would have deleted removed.
		snapshot := convo[:min(1+2*turn, len(convo))]
		dropped := 0
		for i, stale := range v0167StaleReadDumps(snapshot, editPaths[turn]) {
			if stale {
				dropped += est(snapshot[i].Content)
			}
		}
		before := after - dropped

		growth := 0.0
		if before > 0 {
			growth = float64(dropped) / float64(before) * 100
		}
		if growth > worst {
			worst, worstTurn = growth, turn+1
		}
		fmt.Fprintf(&report, "turn %d: before=%d after=%d kept_dump_tokens=%d growth=%.2f%%\n",
			turn+1, before, after, dropped, growth)
	}
	fmt.Fprintf(&report, "worst-turn growth: %.2f%% (turn %d)\n", worst, worstTurn)

	// Printed to stdout (not t.Log) so the artifact capture stays line-anchored.
	if os.Getenv("KLOO_TAIL_GROWTH_REPORT") != "" {
		fmt.Print(report.String())
	}
	if worst > maxWorstTurnGrowthPct {
		t.Errorf("worst-turn prompt growth from keeping duplicate read dumps is %.2f%%, over the %.1f%% bound.\n"+
			"Do NOT raise the bound — take the compactor fallback (Task 03 §3) and re-measure.\n%s",
			worst, maxWorstTurnGrowthPct, report.String())
	}
}

// harnessTranscript runs the Task 02 fixture and returns the resulting transcript
// plus the edit path in force at the start of each turn, so the tail-growth
// measurement is taken over the same conversation the prefix metric uses.
func harnessTranscript(t *testing.T) ([]llm.Message, []string) {
	t.Helper()
	fixture := prefixFixture()
	mocks := make([]llmtest.Mock, 0, len(fixture))
	for _, turn := range fixture {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 20, turn.call)})
	}
	srv := llmtest.Sequence(t, mocks...)

	loop, _ := newRealEditLoop(t, srv, "a.go", "package main\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n",
		&stubVerifier{results: prefixVerifyResults()}, &stubBudget{tripAt: 50}, &stubChurn{})
	writeFixtureFile(t, loop.Root, "b.go", "package main\n\nfunc Zero() int {\n\treturn 0\n}\n")
	loop.Memory = NewWorkingMemory()
	loop.ContextTokens = 1_000_000

	rep, err := loop.Run(context.Background(), "make the tests pass")
	if err != nil {
		t.Fatalf("harness run: %v", err)
	}

	// The edit path in force at the START of each turn is the path of the most
	// recent edit_file call strictly before it.
	paths := make([]string, 0, len(fixture))
	cur := ""
	for _, turn := range fixture {
		paths = append(paths, cur)
		if turn.editPath != "" {
			cur = turn.editPath
		}
	}
	return rep.Transcript, paths
}

// TestBreakpointOnStablePrefixTail: with caching on, the marked message is the
// last one before the per-turn pins, and the trailing repo map is never marked.
// A breakpoint on the map would cache nothing (it changes every turn) and burn the
// slot; a breakpoint below the pins would cache the volatile content itself.
func TestBreakpointOnStablePrefixTail(t *testing.T) {
	base := []llm.Message{
		{Role: llm.RoleSystem, Content: "you are kloo"},
		userMsg("the task"),
		assistantMsg("TAIL-assistant"),
		userMsg("TAIL-observation"), // ← the last stable-prefix message
		userMsg("Last verify: go test ./...\npassed=false exit=1"),
		userMsg("Current file under edit (re-read fresh from disk): a.go\nFRESH"),
		userMsg(repoMapHeader + "a.go\n  function Add:3\n"),
	}
	const pins = 2

	cases := []struct {
		name    string
		enabled bool
		pins    int
		msgs    []llm.Message
		want    int // index expected to carry the marker (-1 ⇒ none)
	}{
		{name: "marks the last tail message, above the pins", enabled: true, pins: pins, msgs: base, want: 3},
		{name: "off marks nothing", enabled: false, pins: pins, msgs: base, want: -1},
		{name: "no map still skips the pins", enabled: true, pins: pins, msgs: base[:len(base)-1], want: 3},
		{name: "no pins marks the message above the map", enabled: true, pins: 0, msgs: base, want: 5},
		{name: "nothing above the system message ⇒ no marker", enabled: true, pins: 1, msgs: base[:2], want: -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := append([]llm.Message(nil), tc.msgs...)
			markCacheBreakpoint(msgs, tc.enabled, tc.pins)

			marked := -1
			for i, m := range msgs {
				if m.CacheControl == nil {
					continue
				}
				if marked >= 0 {
					t.Fatalf("more than one breakpoint: %d and %d", marked, i)
				}
				marked = i
			}
			if marked != tc.want {
				t.Fatalf("breakpoint at index %d, want %d", marked, tc.want)
			}
			for i, m := range msgs {
				if isMapMessage(m) && m.CacheControl != nil {
					t.Errorf("the repo map message[%d] must never be marked", i)
				}
			}
		})
	}
}

// TestBreakpointSurvivesTheRealAssembly: end-to-end through the loop's own
// assembly — the marker lands above the pins the working memory actually emitted,
// not at an index guessed by the test.
func TestBreakpointSurvivesTheRealAssembly(t *testing.T) {
	wm := NewWorkingMemory()
	hist, err := wm.Assemble(MemoryInput{
		Task:       "the task",
		Convo:      []llm.Message{userMsg("the task"), assistantMsg("looked"), userMsg("tool read_file result:\nX")},
		LastVerify: VerifyResult{Command: "go test ./...", ExitCode: 1, Passed: false, Stdout: "FAIL"},
		EditPath:   "a.go", FreshFile: "FRESH\n", WindowTokens: 1_000_000,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	pins := wm.Stats().PinnedMessages
	if pins != 2 {
		t.Fatalf("expected both pins in this assembly, got %d", pins)
	}

	msgs := append([]llm.Message{{Role: llm.RoleSystem, Content: "you are kloo"}}, hist...)
	msgs = append(msgs, userMsg(repoMapHeader+"a.go\n"))
	markCacheBreakpoint(msgs, true, pins)

	want := len(msgs) - 1 /*map*/ - pins - 1
	if msgs[want].CacheControl == nil {
		t.Fatalf("breakpoint missing at index %d (the last stable-prefix message)", want)
	}
	for i, m := range msgs {
		if i != want && m.CacheControl != nil {
			t.Errorf("unexpected breakpoint at index %d", i)
		}
	}
	if strings.HasPrefix(msgs[want].Content, "Last verify: ") ||
		strings.HasPrefix(msgs[want].Content, "Current file under edit") {
		t.Errorf("the breakpoint landed ON a pin: %.60q", msgs[want].Content)
	}
}

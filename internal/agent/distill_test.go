package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

// realisticRun builds a history shaped like an actual run: file reads, edits,
// commands and verify output, big enough to blow the summary budget.
func realisticRun(n int) []llm.Message {
	convo := []llm.Message{{Role: llm.RoleUser, Content: "make the tabs Home/Apps/Profile"}}
	for i := 0; i < n; i++ {
		switch i % 4 {
		case 0:
			convo = append(convo, llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				Type: "function", Function: llm.FunctionCall{Name: "read_file",
					Arguments: fmt.Sprintf(`{"path":"src/app/file%d.ts"}`, i)}}}})
			convo = append(convo, llm.Message{Role: llm.RoleUser,
				Content: fmt.Sprintf("     1\timport x from 'y';\n%s", strings.Repeat("     2\tconst a = 1;\n", 200))})
		case 1:
			convo = append(convo, llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				Type: "function", Function: llm.FunctionCall{Name: "run_command",
					Arguments: fmt.Sprintf(`{"command":"npm test -- file%d"}`, i)}}}})
			convo = append(convo, llm.Message{Role: llm.RoleUser,
				Content: fmt.Sprintf("exit 1\nFAIL src/app/file%d.spec.ts\n  expected 'Home' got 'Tab 1'", i)})
		default:
			convo = append(convo, llm.Message{Role: llm.RoleUser,
				Content: fmt.Sprintf("step %d observation: %s", i, strings.Repeat("detail ", 40))})
		}
	}
	return convo
}

// TestDistillationReclaimsWhatDroppingOnlyDeletes is the point of the pass.
//
// The deterministic fold can only shrink text: entries under maxKeepItemTokens are
// copied verbatim and the budget then DELETES the oldest. Measured before this,
// folding 241 messages returned the prompt 60714 -> 60865 tokens — it ran and gave
// back nothing, and what it did give back it gave back by losing the record.
//
// Distillation must do two things at once: come in UNDER the same budget (so it is
// not just a bigger prompt), and still carry the facts the dropped entries held.
func TestDistillationReclaimsWhatDroppingOnlyDeletes(t *testing.T) {
	in := MemoryInput{
		Task:         "make the tabs Home/Apps/Profile",
		Convo:        realisticRun(240),
		WindowTokens: 131072,
	}

	drop := &workingMemory{}
	dropped, err := drop.Assemble(in)
	if err != nil {
		t.Fatalf("drop arm: %v", err)
	}

	var got [][]string
	in.Distill = func(entries []string) (string, error) {
		got = append(got, entries)
		return "edited src/app/tabs.routes.ts (tab1->home); npm test FAILS: expected 'Home' got 'Tab 1'; read 40 files under src/app", nil
	}
	dist := &workingMemory{}
	distilled, err := dist.Assemble(in)
	if err != nil {
		t.Fatalf("distill arm: %v", err)
	}

	dropTok := tokensOfWith(dropped, in.estimate)
	distTok := tokensOfWith(distilled, in.estimate)
	t.Logf("drop arm:    %d tokens, %d entries dropped", dropTok, drop.droppedEntries)
	t.Logf("distill arm: %d tokens, %d entries distilled into %d brief(s)", distTok, dist.distilled, len(got))

	if len(got) == 0 {
		t.Fatal("the distiller was never called — the summary never overflowed, so this test proves nothing")
	}
	if dist.distilled == 0 {
		t.Error("entries were dropped rather than distilled")
	}
	// It must not buy its information with a bigger prompt. Some slack: the brief is
	// real content the drop arm simply does not have.
	if distTok > dropTok+2000 {
		t.Errorf("distillation blew the budget: %d tokens vs %d dropping", distTok, dropTok)
	}
	// And the facts have to actually be in the assembled prompt.
	joined := ""
	for _, m := range distilled {
		joined += m.Content + "\n"
	}
	for _, want := range []string{"tabs.routes.ts", "expected 'Home'"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the brief never reached the prompt: missing %q", want)
		}
	}
}

// TestDistillFailureKeepsTheOldBehaviour: the summariser is bookkeeping, so every
// way it can fail has to cost exactly what it cost before — the entries drop.
// A run must never die because its own compaction call did.
func TestDistillFailureKeepsTheOldBehaviour(t *testing.T) {
	base := MemoryInput{Task: "t", Convo: realisticRun(240), WindowTokens: 131072}

	plain := &workingMemory{}
	want, err := plain.Assemble(base)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	wantTok := tokensOfWith(want, base.estimate)

	for _, tc := range []struct {
		name string
		fn   func([]string) (string, error)
	}{
		{"error", func([]string) (string, error) { return "", fmt.Errorf("endpoint down") }},
		{"empty", func([]string) (string, error) { return "   ", nil }},
	} {
		in := base
		in.Distill = tc.fn
		w := &workingMemory{}
		out, err := w.Assemble(in)
		if err != nil {
			t.Fatalf("%s: Assemble returned an error: %v", tc.name, err)
		}
		if got := tokensOfWith(out, in.estimate); got != wantTok {
			t.Errorf("%s: prompt differs from the no-distiller baseline: %d vs %d", tc.name, got, wantTok)
		}
		if w.distillFailures == 0 {
			t.Errorf("%s: the failure was not recorded", tc.name)
		}
		if w.droppedEntries == 0 {
			t.Errorf("%s: entries should still have been dropped", tc.name)
		}
	}
}

// TestBriefSurvivesTheNextOverflow: a brief is the compacted form of everything
// before it, so the next overflow must fold it into the NEXT brief rather than
// delete it — a summary of summaries. Without this the early run is lost in one
// step the moment the summary fills a second time.
func TestBriefSurvivesTheNextOverflow(t *testing.T) {
	if !durableSummaryEntry(distilledEntry("ran the build, it failed")) {
		t.Fatal("a brief must be durable: dropping it loses the whole early run at once")
	}
	// A tight summary budget so the summary overflows repeatedly within one test,
	// which is what a long run does over its lifetime.
	t.Cleanup(func() { SetSummaryBudgetFrac(summaryBudgetFrac); SetWorkingSetTokens(0) })
	SetSummaryBudgetFrac(0.10)
	SetWorkingSetTokens(8192) // a small working set so the summary overflows repeatedly

	var seen []string
	in := MemoryInput{Task: "t", Convo: realisticRun(200), WindowTokens: 131072}
	in.Distill = func(entries []string) (string, error) {
		seen = append(seen, strings.Join(entries, "|"))
		return fmt.Sprintf("brief#%d covering %d entries", len(seen), len(entries)), nil
	}
	w := &workingMemory{}
	for turn := 0; turn < 8; turn++ {
		if _, err := w.Assemble(in); err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		in.Convo = append(in.Convo, llm.Message{Role: llm.RoleUser,
			Content: fmt.Sprintf("later step %d: %s", turn, strings.Repeat("more detail ", 2000))})
	}
	if len(seen) < 2 {
		t.Fatalf("the summary only overflowed once (%d) — recursion is untested", len(seen))
	}
	t.Logf("%d distillations; last input: %.200s", len(seen), seen[len(seen)-1])
	if !strings.Contains(seen[len(seen)-1], "brief#") {
		t.Errorf("the previous brief was not folded into the next one:\n%s", seen[len(seen)-1])
	}
}

// TestTheRecordIsNeverHandedToTheModel is a regression test for a real, measured
// hallucination, not a hypothetical.
//
// qwen3.8-next was given six summary entries — five read stubs and the exploration
// rail's own line, "You have inspected 3 files without changing a single line" —
// and wrote back:
//
//	"Edited frontend/src/app/pages/game/game.page.ts"
//
// Nothing had been edited. It inverted the one fact its input stated outright and
// filed it into the agent's own memory as history, which is precisely how a run
// talks itself into being finished before it has done anything.
//
// So the summariser never sees the record. It may compress narration; applied
// edits, failures and read stubs are dropped the old way if they must go, never
// rewritten.
func TestTheRecordIsNeverHandedToTheModel(t *testing.T) {
	record := []string{
		"edit_file src/app/tabs.routes.ts",
		"write_file src/app/new.ts",
		"[read frontend/src/app/pages/game/game.page.scss: 159 lines, re-read on demand]",
		"exit 1 FAIL src/app/file.spec.ts expected 'Home' got 'Tab 1'",
	}
	for _, entry := range record {
		if distillableSummaryEntry(entry) {
			t.Errorf("the record must never be rewritten by the model: %q", entry)
		}
	}

	narration := []string{
		"step 7 observation: looked at the themes folder",
		"$ npm run build",
		distilledEntry("read 12 files under src/app; build is green"),
	}
	for _, entry := range narration {
		if !distillableSummaryEntry(entry) {
			t.Errorf("narration should be compressible: %q", entry)
		}
	}

	// End to end: a summary made ONLY of record entries must call the model zero
	// times, however far over budget it is.
	w := &workingMemory{foldedEntries: append([]string{}, record...)}
	calls := 0
	w.collapseSummary(1, func(s string) int { return len(s) }, func([]string) (string, error) {
		calls++
		return "Edited frontend/src/app/pages/game/game.page.ts", nil
	})
	if calls != 0 {
		t.Errorf("the summariser was called %d time(s) on a summary that is pure record", calls)
	}
	for _, e := range w.summaryEntries() {
		if strings.Contains(e, "Edited frontend") {
			t.Fatalf("a fabricated edit reached the summary: %q", e)
		}
	}
}

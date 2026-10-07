package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

// The cap must actually cause a SHED, not merely report a lower trigger. A
// trigger that nothing consults would show up in `kloo doctor` and change
// nothing about what is sent — which is precisely the failure this whole change
// is correcting in the other direction.
func TestWorkingSetCapActuallyShedsOnALargeWindow(t *testing.T) {
	t.Cleanup(func() { SetWorkingSetTokens(0) })

	// The history has to sit between the two bounds that exist at this window: past
	// the CAP (now 65536 at ctx 131072 — the geometric-mean curve, not a flat
	// 32768) and under the 73399 the fraction alone allows. Then the cap is the only
	// thing that can trigger a compaction, which is what this test is about.
	//
	// The fixture was 240 messages when the cap was flat. That is now BELOW the cap
	// at this window and correctly causes no shed — which is the whole point of the
	// curve, so the fixture moves rather than the behaviour.
	convo := []llm.Message{{Role: llm.RoleUser, Content: "trace the prompt"}}
	for i := 0; i < 280; i++ {
		convo = append(convo, llm.Message{Role: llm.RoleUser,
			Content: fmt.Sprintf("read %d:\n%s", i, strings.Repeat("x ", 500))})
	}
	in := MemoryInput{
		Task:         "trace the prompt",
		Convo:        convo,
		WindowTokens: 131072,
	}

	SetWorkingSetTokens(-1)
	w1 := &workingMemory{}
	off, err := w1.Assemble(in)
	if err != nil {
		t.Fatalf("cap off: %v", err)
	}

	SetWorkingSetTokens(0)
	w2 := &workingMemory{}
	on, err := w2.Assemble(in)
	if err != nil {
		t.Fatalf("cap on: %v", err)
	}

	offTok := tokensOfWith(off, in.estimate)
	onTok := tokensOfWith(on, in.estimate)
	t.Logf("cap off: %d tokens, %d msgs, compactions=%d", offTok, len(off), w1.compactions)
	t.Logf("cap on:  %d tokens, %d msgs, compactions=%d", onTok, len(on), w2.compactions)

	if onTok >= offTok {
		t.Fatalf("cap did not shrink the assembled prompt: on=%d off=%d", onTok, offTok)
	}
	if want := WorkingSetTokensFor(in.WindowTokens); onTok > want {
		t.Fatalf("capped prompt %d exceeds the working-set cap %d at ctx %d", onTok, want, in.WindowTokens)
	}
	if w2.compactions == 0 {
		t.Fatal("cap reported a lower trigger but no compaction ran")
	}
	if w1.compactions != 0 {
		t.Fatalf("cap off: %d compactions, want none (the fraction alone must not fire here)", w1.compactions)
	}
	// The task must survive the shed — it is the one thing that is never dropped.
	if !strings.Contains(on[len(on)-1].Content+on[0].Content, "trace the prompt") {
		joined := ""
		for _, m := range on {
			joined += m.Content
		}
		if !strings.Contains(joined, "trace the prompt") {
			t.Fatal("the task did not survive the capped assembly")
		}
	}
}

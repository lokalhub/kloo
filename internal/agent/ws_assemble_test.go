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

	// ~60k tokens of history: well past the 32768 cap, well under the 73399 the
	// fraction alone allows at this window. So the cap is the ONLY thing that can
	// trigger a compaction here.
	convo := []llm.Message{{Role: llm.RoleUser, Content: "trace the prompt"}}
	for i := 0; i < 240; i++ {
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
	if onTok > defaultWorkingSetTokens {
		t.Fatalf("capped prompt %d exceeds the working-set cap %d", onTok, defaultWorkingSetTokens)
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

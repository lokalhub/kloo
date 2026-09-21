package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestIntegrationNoOpEditIsSurfaced drives the exact failure that costs kloo the
// A16 case, deterministically, because the benchmark only produces it about a
// third of the time.
//
// The model writes the file's EXISTING content — a write that applies cleanly and
// changes nothing. Before this fix kloo returned "wrote answer.txt (N bytes)",
// incremented an internal counter, and said nothing, so the model believed its
// change had landed and repeated it until the churn rail killed the run. Measured
// on kloo-bench A16 across 19 runs: every failure had repeated_edits=2 with
// no_op_edits 3-4 and died in churn; no passing run had either counter.
func TestIntegrationNoOpEditIsSurfaced(t *testing.T) {
	t.Setenv("KLOO_NOOP_EDIT_FEEDBACK", "1")
	root := seedRepo(t) // answer.txt contains "wrong\n"
	// Write back exactly what is already there, every turn.
	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "wrong\n", "")})
	loop := buildLoop(t, root, srv, config.Config{MaxSteps: 20, ChurnRounds: 2})

	rep, _ := loop.Run(context.Background(), "make the check pass")

	if rep.ToolCounters.NoOpEdits == 0 {
		t.Fatalf("a write of identical content was not detected as a no-op (%s)", rep.String())
	}
	var told bool
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "changed NOTHING") {
			told = true
		}
	}
	if !told {
		t.Fatal("kloo detected the no-op edit and never told the model — the model " +
			"cannot distinguish 'my edit landed' from 'my edit did nothing'")
	}
}

// TestIntegrationNoOpFeedbackOffByDefault: the released binary is unchanged until
// the bench says otherwise.
func TestIntegrationNoOpFeedbackOffByDefault(t *testing.T) {
	t.Setenv("KLOO_NOOP_EDIT_FEEDBACK", "")
	root := seedRepo(t)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "wrong\n", "")})
	loop := buildLoop(t, root, srv, config.Config{MaxSteps: 20, ChurnRounds: 2})

	rep, _ := loop.Run(context.Background(), "make the check pass")
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "changed NOTHING") {
			t.Fatal("no-op corrective fired with the flag off")
		}
	}
}

// TestIntegrationRealEditIsNotCalledANoOp: the guard must not fire on an edit that
// actually changed the file, or every legitimate edit gets contradicted.
func TestIntegrationRealEditIsNotCalledANoOp(t *testing.T) {
	t.Setenv("KLOO_NOOP_EDIT_FEEDBACK", "1")
	root := seedRepo(t)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "right\n", "")})
	loop := buildLoop(t, root, srv, config.Config{MaxSteps: 20, ChurnRounds: 2})

	rep, _ := loop.Run(context.Background(), "make the check pass")
	if rep.ToolCounters.NoOpEdits != 0 {
		t.Fatalf("a real edit was counted as a no-op (%s)", rep.String())
	}
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "changed NOTHING") {
			t.Fatal("no-op corrective fired on an edit that DID change the file")
		}
	}
}

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

// TestIntegrationNoOpEditIsRefused: the CONSTRAIN-shaped version. The informing
// version was measured and ignored — in the one kloo-bench run where the
// corrective fired, the model was told its edit changed nothing and repeated it
// anyway until churn killed the run. Refusing does not depend on the model heeding
// anything: the loop does not record a change, so the run cannot proceed as though
// one landed.
func TestIntegrationNoOpEditIsRefused(t *testing.T) {
	t.Setenv("KLOO_NOOP_EDIT_REFUSE", "1")
	root := seedRepo(t) // answer.txt contains "wrong\n"
	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "wrong\n", "")})
	loop := buildLoop(t, root, srv, config.Config{MaxSteps: 20, ChurnRounds: 2})

	rep, _ := loop.Run(context.Background(), "make the check pass")

	if rep.ToolCounters.FailedEdits == 0 {
		t.Fatalf("a no-op edit was not counted as a FAILED edit (%s)", rep.String())
	}
	var told bool
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "REJECTED") {
			told = true
		}
	}
	if !told {
		t.Fatal("the refusal was not surfaced to the model")
	}
	// The run must not end as a success on the strength of a change that never
	// happened.
	if rep.Reason == ReasonSuccess {
		t.Fatalf("run succeeded on a no-op edit (%s)", rep.String())
	}
}

// TestIntegrationRefuseBeatsInform: with refusal on, the weaker informing path
// must not also fire — two messages about one event is noise, and it was measured
// as ineffective.
func TestIntegrationRefuseBeatsInform(t *testing.T) {
	t.Setenv("KLOO_NOOP_EDIT_REFUSE", "1")
	t.Setenv("KLOO_NOOP_EDIT_FEEDBACK", "1")
	root := seedRepo(t)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "wrong\n", "")})
	loop := buildLoop(t, root, srv, config.Config{MaxSteps: 20, ChurnRounds: 2})

	rep, _ := loop.Run(context.Background(), "make the check pass")
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "changed NOTHING") {
			t.Fatal("the informing corrective fired alongside the refusal")
		}
	}
}

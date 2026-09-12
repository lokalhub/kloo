package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// ─── Phase 02 S9: the user-visible record of WHEN verify runs ────────────────
//
// renderingVerifier wraps a verifier and appends one line per REAL verify, in the
// shape the S9 mock agrees. It renders from the loop's actual verify events, so the
// transcript is a record of what happened rather than a re-simulation.
type renderingVerifier struct {
	inner Verifier
	out   *strings.Builder
}

func (r *renderingVerifier) Verify(ctx context.Context) VerifyResult {
	res := r.inner.Verify(ctx)
	glyph := "✓"
	if !res.Passed {
		glyph = "✗"
	}
	fmt.Fprintf(r.out, "  ⌘ %s    exit %d %s\n", res.Command, res.ExitCode, glyph)
	return res
}

// TestS9TranscriptVerifyLinesMatchAttempts drives the read-heavy run and asserts
// the transcript's verify lines and the counted attempts agree exactly — no verify
// line after a read-only step, one after the applied write, one after finish.
// With KLOO_S9_TRANSCRIPT set it also writes the transcript to that path, which is
// how the phase artifact is produced.
func TestS9TranscriptVerifyLinesMatchAttempts(t *testing.T) {
	var out strings.Builder
	read := tcSpec{"read_file", map[string]any{"path": "answer.txt"}}
	srv := llmtest.Sequence(t, mocksFor(t,
		read, read, read,
		tcSpec{"write_file", map[string]any{"path": "answer.txt", "content": "right\n"}},
		finishSpec,
	)...)

	// Red after the write so the mid-loop success gate does not end the run there —
	// the run reaches `finish`, whose verify is green. That exercises all three
	// cases the S9 mock names: no line after a read, one after the applied edit,
	// one after finish.
	inner := &countingVerifier{results: []VerifyResult{
		{Command: "npm test", ExitCode: 1, Passed: false, Stdout: "FAIL: one"},
		{Command: "npm test", ExitCode: 0, Passed: true},
	}}
	rv := &renderingVerifier{inner: inner, out: &out}
	loop := gateLoop(t, srv, rv, &stubChurn{},
		scriptTool{name: tools.NameWriteFile, res: tools.Result{Output: "wrote answer.txt"}})

	step := 0
	loop.OnProgress = func(s, maxSteps, tokens, maxTokens int) {
		step = s
		fmt.Fprintf(&out, "── step %d/%d  tokens %d\n", s, maxSteps, tokens)
	}
	loop.OnTool = func(call tools.Call, res tools.Result, err error) {
		target := str(call.Args["path"])
		if target == "" {
			target = str(call.Args["command"])
		}
		fmt.Fprintf(&out, "  ▸ %s %s\n", call.Name, target)
	}
	_ = step

	rep, err := loop.Run(context.Background(), "make the check pass")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	lines := strings.Count(out.String(), "npm test")
	if lines != rep.ToolCounters.VerifyAttempts {
		t.Errorf("transcript shows %d verify lines but %d attempts were counted:\n%s",
			lines, rep.ToolCounters.VerifyAttempts, out.String())
	}
	if rep.ToolCounters.VerifyAttempts != 2 {
		t.Errorf("VerifyAttempts = %d, want 2 (the applied write, then finish)", rep.ToolCounters.VerifyAttempts)
	}
	// No verify line may follow a read-only step: the three reads are steps 1-3.
	for _, block := range strings.Split(out.String(), "── step ")[1:4] {
		if strings.Contains(block, "npm test") {
			t.Errorf("a read-only step shows a verify line, which the gate removes:\n%s", block)
		}
	}

	if path := os.Getenv("KLOO_S9_TRANSCRIPT"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
	}
}

package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// loopLiteralKeys returns the field names assigned in the `agent.Loop{...}`
// composite literal of a cli source file. The TUI path takes over the terminal
// the moment it is entered, so its Loop cannot be built inside a test the way the
// headless one can — the literal's own shape is what stays checkable there.
func loopLiteralKeys(t *testing.T, file string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	keys := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Loop" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "agent" {
			return true
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok {
					keys[id.Name] = true
				}
			}
		}
		return true
	})
	if len(keys) == 0 {
		t.Fatalf("no agent.Loop composite literal found in %s", file)
	}
	return keys
}

// runRepeatingWrite drives a real headless run whose mocked model emits the SAME
// write_file call every turn, and returns the run's output. write_file (not
// edit_file) keeps every repeat APPLYING cleanly, so the failed-edit rail stays
// out of the way and only the repetition rail can stop the run.
func runRepeatingWrite(t *testing.T, cfg func(*config.Config)) string {
	t.Helper()
	t.Chdir(t.TempDir())

	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeAnswerStream(t, "right\n"), SSE: true})
	c := config.Config{
		Endpoint: srv.URL + "/v1", Model: "test-model", ToolFormat: config.DefaultToolFormat,
		// ChurnRounds well above the repetition thresholds so the churn rail cannot
		// halt the run first and mask which threshold actually fired.
		MaxSteps: 30, ChurnRounds: 20, MaxContextTokens: 8000,
	}
	cfg(&c)

	var out strings.Builder
	// "false" never passes, so the run can only end on a rail — never on success.
	_ = defaultRunHeadless(c, "write the answer", "false", lintOpts{}, &out)
	return out.String()
}

// TestRepeatRoundsReachLoop: the resolved config's repetition-rail knobs actually
// reach the loop the CLI builds. The headless path is driven for real — a tuned
// abort stops the run at the tuned round, an unset one at the package default —
// and both Loop construction sites are checked to carry the assignment.
func TestRepeatRoundsReachLoop(t *testing.T) {
	t.Run("headless tuned abort halts at the tuned round", func(t *testing.T) {
		got := runRepeatingWrite(t, func(c *config.Config) { c.RepeatAbortRounds = 3 })
		if !strings.Contains(got, "churn:   repeated-call") {
			t.Fatalf("run should end on the repetition rail:\n%s", got)
		}
		if !strings.Contains(got, "steps:   3") {
			t.Errorf("RepeatAbortRounds=3 must halt on the 3rd identical call:\n%s", got)
		}
	})

	t.Run("headless unset abort halts at the package default", func(t *testing.T) {
		got := runRepeatingWrite(t, func(c *config.Config) {})
		if !strings.Contains(got, "churn:   repeated-call") {
			t.Fatalf("run should end on the repetition rail:\n%s", got)
		}
		if !strings.Contains(got, "steps:   6") {
			t.Errorf("unset knobs must fall through to DefaultRepeatAbortRounds (6):\n%s", got)
		}
	})

	t.Run("both Loop construction sites assign the knobs", func(t *testing.T) {
		for _, file := range []string{"headless.go", "tui.go"} {
			keys := loopLiteralKeys(t, filepath.Join(".", file))
			for _, want := range []string{"RepeatNudgeRounds", "RepeatAbortRounds"} {
				if !keys[want] {
					t.Errorf("%s: agent.Loop literal does not set %s", file, want)
				}
			}
		}
	})
}

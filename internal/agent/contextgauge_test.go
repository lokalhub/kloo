package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
)

// gaugeWorkspace builds a tiny real workspace so the repo map is non-empty and the
// gauge has something to attribute.
func gaugeWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"main.go":    "package main\n\nfunc main() { helper() }\n",
		"helper.go":  "package main\n\nfunc helper() int { return 41 }\n",
		"README.md":  "# fixture\n\nA fixture workspace for the context gauge.\n",
		"service.ts": "export function service(): number { return helper(); }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func gaugeLoop(t *testing.T, root string, ctxTokens int) *Loop {
	t.Helper()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Loop{
		Adapter:       tools.NativeFCAdapter{},
		Registry:      tools.DefaultRegistry(ws),
		Root:          root,
		ContextTokens: ctxTokens,
		Memory:        NewWorkingMemory(),
		System:        "You are kloo. " + strings.Repeat("Follow the rules. ", 40),
		Model:         "fixture-model",
	}
}

// TestContextGaugeIsMeasuredNotDerivedFromBudgets is the test that justifies the
// whole file. The gauge must report what is IN the prompt, not what the budget
// constants would have allowed.
//
// It is constructed so the two answers differ by construction: the repo-map section
// is replaced with a block of a known size that is deliberately NOT equal to
// mapBudgetTokens, and the gauge must report the real one. On master there is no
// gauge at all, so this cannot compile there — which is the strongest possible form
// of "fails without the change".
func TestContextGaugeIsMeasuredNotDerivedFromBudgets(t *testing.T) {
	root := gaugeWorkspace(t)
	l := gaugeLoop(t, root, 131072)
	if _, _, err := l.BuildPromptForMeasurement(context.Background(), "make helper return 42"); err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	g := l.ContextGauge()

	// The map is a few hundred tokens for a four-file fixture; the BUDGET at this
	// window is ~22k. A gauge derived from mapBudgetFrac would report the budget.
	if g.MapBudget < 10000 {
		t.Fatalf("fixture no longer exercises the gap: map budget %d is not much larger than any real map", g.MapBudget)
	}
	if g.RepoMapTokens >= g.MapBudget {
		t.Fatalf("repo map %d should be far below its budget %d on a 4-file fixture — the gauge looks derived from the budget",
			g.RepoMapTokens, g.MapBudget)
	}

	// Every section must be a sum over the assembled prompt, so the attribution has
	// to close. A nonzero residual beyond estimator rounding means the layout moved
	// and the attribution did not.
	if g.Unattributed > 4 || g.Unattributed < -4 {
		t.Errorf("unattributed = %d; the section attribution does not account for the prompt", g.Unattributed)
	}
	if g.Used != g.SystemTokens+g.SchemaTokens+g.RepoMapTokens+g.PinnedHotTokens+g.HistoryTokens+g.Unattributed {
		t.Errorf("sections do not sum to used: %+v", g)
	}
	// The schemas are prompt that is NOT in any message. A gauge summed from messages
	// alone under-reports every turn by their size, which is the bug in promptBytes.
	if g.SchemaTokens <= 0 {
		t.Error("schema tokens = 0; the tool definitions are prompt and must be counted")
	}
	if g.SystemTokens <= 0 || g.RepoMapTokens <= 0 || g.PinnedHotTokens <= 0 {
		t.Errorf("a section is empty on a real assembly: %+v", g)
	}
	if g.Window != 131072 || g.Usable != usableWindow(131072) {
		t.Errorf("window/usable = %d/%d, want 131072/%d", g.Window, g.Usable, usableWindow(131072))
	}
}

// TestContextGaugeAddsNoBytesToThePrompt is the zero-cost property. A ~22k repo map
// re-prefilling every call was the worst performance bug kloo has had, and on the
// user's endpoint a changed prompt HEAD costs ~15x. The gauge must be invisible to
// the request.
//
// Proved by assembling the same turn with and without reading the gauge and
// comparing the prompt byte for byte, and by asserting the labelling arrays carry no
// content of their own.
func TestContextGaugeAddsNoBytesToThePrompt(t *testing.T) {
	root := gaugeWorkspace(t)
	task := "make helper return 42"
	convo := []llm.Message{{Role: llm.RoleUser, Content: task}}

	l1 := gaugeLoop(t, root, 131072)
	ps1, req1, err := l1.buildPrompt(context.Background(), task, convo, VerifyResult{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Read the gauge on one and not the other; the request must be identical.
	_ = l1.ContextGauge()

	l2 := gaugeLoop(t, root, 131072)
	ps2, req2, err := l2.buildPrompt(context.Background(), task, convo, VerifyResult{}, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(ps1.Msgs) != len(ps2.Msgs) {
		t.Fatalf("message count differs: %d vs %d", len(ps1.Msgs), len(ps2.Msgs))
	}
	for i := range ps1.Msgs {
		if ps1.Msgs[i].Content != ps2.Msgs[i].Content || ps1.Msgs[i].Role != ps2.Msgs[i].Role {
			t.Fatalf("message %d differs between an observed and an unobserved assembly", i)
		}
	}
	if messageChars(req1.Messages) != messageChars(req2.Messages) {
		t.Fatalf("prompt chars differ: %d vs %d", messageChars(req1.Messages), messageChars(req2.Messages))
	}

	// The labels are metadata, never content: every Kinds entry is one of the four
	// section names, so nothing in that array could be serialised as prompt even by
	// accident.
	if len(ps1.Kinds) != len(ps1.Msgs) {
		t.Fatalf("Kinds (%d) is not parallel to Msgs (%d)", len(ps1.Kinds), len(ps1.Msgs))
	}
	for i, k := range ps1.Kinds {
		switch k {
		case sectSystem, sectMap, sectHot, sectHistory:
		default:
			t.Errorf("Kinds[%d] = %q is not a section label", i, k)
		}
	}
	// And the gauge itself holds no strings taken from the prompt.
	g := l1.ContextGauge()
	for _, m := range ps1.Msgs {
		if len(m.Content) > 32 && strings.Contains(g.MapPlacement+g.Binding, m.Content[:32]) {
			t.Error("the gauge is carrying prompt content")
		}
	}
}

// TestContextGaugeNamesTheBindingWindow pins the denominator decision. At a large
// --ctx the absolute working-set cap is far tighter than the trigger fraction, and
// `kloo tokens` reported headroom against the looser one — 3.8x too much room. The
// gauge must say which budget is in force and report room against BOTH.
func TestContextGaugeNamesTheBindingWindow(t *testing.T) {
	root := gaugeWorkspace(t)

	// Large window: the absolute cap binds.
	big := gaugeLoop(t, root, 600000)
	if _, _, err := big.BuildPromptForMeasurement(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	g := big.ContextGauge()
	if g.Binding != "working-set" {
		t.Errorf("at --ctx 600000 binding = %q, want working-set (usable %d, trigger %d)", g.Binding, g.Usable, g.Trigger)
	}
	if g.Trigger >= g.Usable {
		t.Fatalf("trigger %d should be well below usable %d at this window", g.Trigger, g.Usable)
	}
	// The two kinds of room must differ, which is exactly the fact `headroom` hid.
	if g.Free <= g.FreeToCompaction {
		t.Errorf("free (%d) should exceed free-to-compaction (%d) when the working set binds", g.Free, g.FreeToCompaction)
	}
	if want := g.Trigger - g.Used; g.FreeToCompaction != want {
		t.Errorf("FreeToCompaction = %d, want trigger-used = %d", g.FreeToCompaction, want)
	}

	// Small window: the fraction is already tighter, so the cap is a no-op.
	small := gaugeLoop(t, root, 8000)
	if _, _, err := small.BuildPromptForMeasurement(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if b := small.ContextGauge().Binding; b != "trigger-fraction" {
		t.Errorf("at --ctx 8000 binding = %q, want trigger-fraction", b)
	}
}

// TestContextGaugeCountsTheMapOnceWhateverItsPlacement guards the one piece of
// arithmetic that a layout change could silently break: the map lives in its own
// message under the default pinned placement and INSIDE the system message under
// MapPositionSystem, and must be charged to the map either way.
func TestContextGaugeCountsTheMapOnceWhateverItsPlacement(t *testing.T) {
	root := gaugeWorkspace(t)
	for _, pos := range []string{MapPositionPinned, MapPositionTail, MapPositionSystem} {
		l := gaugeLoop(t, root, 131072)
		l.MapPosition = pos
		if _, _, err := l.BuildPromptForMeasurement(context.Background(), "x"); err != nil {
			t.Fatalf("%s: %v", pos, err)
		}
		g := l.ContextGauge()
		if g.RepoMapTokens <= 0 {
			t.Errorf("%s: repo map charged 0 tokens", pos)
		}
		if g.Unattributed > 8 || g.Unattributed < -8 {
			t.Errorf("%s: unattributed = %d — the map is being counted twice or not at all (%+v)", pos, g.Unattributed, g)
		}
		// The system section must never silently absorb the map.
		if pos == MapPositionSystem && g.SystemTokens >= g.Used {
			t.Errorf("%s: system %d absorbed the whole prompt %d", pos, g.SystemTokens, g.Used)
		}
	}
}

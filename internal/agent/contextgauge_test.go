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

// TestContextGaugeReportsTheEffectiveNotTheNominalRatio: the first cut announced
// "4.00 chars/token", which is the BASE constant and not what anything was charged.
// tokens.EstimateAt bills high-entropy words at ~1.8 (estimate.go: Go source measured
// at 3.75, go.sum hashes at 1.78), and a repo map is almost entirely long paths and
// identifiers, so the effective rate lands below the base. A gauge announcing a
// precision it does not have is this feature's own failure mode.
func TestContextGaugeReportsTheEffectiveNotTheNominalRatio(t *testing.T) {
	root := gaugeWorkspace(t)
	l := gaugeLoop(t, root, 131072)
	if _, _, err := l.BuildPromptForMeasurement(context.Background(), "make helper return 42"); err != nil {
		t.Fatal(err)
	}
	g := l.ContextGauge()

	if g.Chars <= 0 {
		t.Fatal("Chars = 0; the effective ratio cannot be derived without it")
	}
	if g.EffectiveRatio <= 0 {
		t.Fatal("EffectiveRatio = 0")
	}
	// Derived from THIS prompt, so it must reproduce exactly.
	if want := float64(g.Chars) / float64(g.Used); g.EffectiveRatio != want {
		t.Errorf("EffectiveRatio = %v, want Chars/Used = %v", g.EffectiveRatio, want)
	}
	// And it must actually differ from the nominal base, or the report is still just
	// quoting a constant.
	if g.EffectiveRatio >= g.Ratio {
		t.Errorf("effective %.3f is not below the base %.3f — the entropy-aware estimator should bill a code-heavy prompt harder",
			g.EffectiveRatio, g.Ratio)
	}
	if g.EffectiveRatio < 2.0 || g.EffectiveRatio > g.Ratio {
		t.Errorf("effective ratio %.3f is implausible for a source-code prompt", g.EffectiveRatio)
	}
}

// TestContextGaugeSurfacesTheMapOvershoot: the overshoot was computed and then never
// rendered, and as an int it truncated a real 0.47% to 0 — measure-then-hide, which is
// the thing this feature exists to correct.
func TestContextGaugeSurfacesTheMapOvershoot(t *testing.T) {
	ps := promptSections{MapBudget: 22019, Usable: 104857, Window: 131072}
	// A map 104 tokens over its budget: the real measured overshoot.
	ps.MapSection = strings.Repeat("x", 4*22123)
	ps.add(sectSystem, llm.Message{Role: llm.RoleSystem, Content: "sys"})
	g := ps.gauge(0, 0, func(s string) int { return len(s) / 4 })

	if g.MapOverBudget != 104 {
		t.Errorf("MapOverBudget = %d, want 104", g.MapOverBudget)
	}
	// A float, so a sub-1% overshoot is visible instead of truncated to zero.
	if g.MapOverBudgetPct <= 0 || g.MapOverBudgetPct >= 1 {
		t.Errorf("MapOverBudgetPct = %v, want a small positive fraction of a percent", g.MapOverBudgetPct)
	}
	// Within budget reports nothing, so the field cannot be read as noise.
	ps.MapSection = strings.Repeat("x", 4*1000)
	if under := ps.gauge(0, 0, func(s string) int { return len(s) / 4 }); under.MapOverBudget != 0 || under.MapOverBudgetPct != 0 {
		t.Errorf("a map within budget reported an overshoot: %d / %v", under.MapOverBudget, under.MapOverBudgetPct)
	}
}

// TestGaugeLabellingCarriesNoPromptContent is the zero-cost property, named for what
// it actually guards. A ~22k repo map re-prefilling every call was the worst
// performance bug kloo has had, and on the user's endpoint a changed prompt HEAD
// costs ~15x, so the gauge must be invisible to the request.
//
// IT WAS CALLED TestContextGaugeAddsNoBytesToThePrompt AND THAT NAME WAS A LIE.
// Comparing two assemblies of the same build cannot detect an addition: buildPrompt
// always computes the gauge, so both arms contain it and are identical by
// construction. Review proved it by injecting 19 bytes into the system prompt — this
// test passed. What catches that is a GOLDEN BYTE COUNT over a fixed fixture
// (asserted below) plus master's pre-existing TestMapPositionTailKeepsSystemStable,
// which did catch the mutation.
//
// What this test does guard, and what is worth guarding: the labels are metadata that
// can never become prompt text, and reading the gauge has no side effect on the
// request.
func TestGaugeLabellingCarriesNoPromptContent(t *testing.T) {
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

	// THE GOLDEN BYTE COUNT — the part that can actually catch an addition. The fixture
	// is fixed, so the assembled non-map prompt has an exact size; anything the gauge
	// (or anything else) appends to the system prompt or the hot set moves it. The repo
	// map is excluded because it legitimately varies with the tree and the estimator,
	// which is precisely why a whole-prompt golden would be unmaintainable here.
	nonMap := 0
	for i, m := range ps1.Msgs {
		if ps1.Kinds[i] != sectMap {
			nonMap += len(m.Content)
		}
	}
	const wantNonMapBytes = 755 // system (fixture) + task; update ONLY with an explanation
	if nonMap != wantNonMapBytes {
		t.Errorf("non-map prompt bytes = %d, want %d — something was added to or removed from the prompt. "+
			"If that was deliberate, say what in the commit; if not, the gauge is participating in the prompt.",
			nonMap, wantNonMapBytes)
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

package tui

import (
	"strings"
	"testing"
)

func TestStatusIdleDefaults(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, th)
	v := m.View()
	for _, want := range []string{"test-model", "step 0/40", "0/8k tok", "auto"} {
		if !contains(v, want) {
			t.Errorf("idle header missing %q:\n%s", want, v)
		}
	}
	requireGolden(t, "status-idle.golden", v)
}

func TestStatusUpdatesOnProgress(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, th)
	m = apply(m, progressMsg{Model: "test-model", Step: 3, MaxSteps: 40, Tokens: 1200, MaxTokens: 8000})
	v := m.View()
	for _, want := range []string{"test-model", "step 3/40", "1.2k/8k tok", "auto"} {
		if !contains(v, want) {
			t.Errorf("running header missing %q:\n%s", want, v)
		}
	}
	requireGolden(t, "status-running.golden", v)

	// A second snapshot advances the counters.
	m2 := apply(m, progressMsg{Model: "test-model", Step: 4, MaxSteps: 40, Tokens: 1800, MaxTokens: 8000})
	if !contains(m2.View(), "step 4/40") || !contains(m2.View(), "1.8k/8k") {
		t.Errorf("counters did not advance:\n%s", m2.View())
	}
}

func TestStatusNarrowElidesGracefully(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), 30, 12)
	// Should not panic and the header line should fit the width.
	header := strings.Split(m.View(), "\n")[1] // the content line (index 1, between borders)
	if len([]rune(header)) > 30 {
		t.Errorf("narrow header overflows: %q", header)
	}
}

// C2: the status line gains a working-memory compaction indicator that
// increments on a memoryMsg; with no compaction it renders exactly as today
// (the unchanged status-running.golden above is the regression for the 0 case).
func TestStatusCompactionIndicator(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, th)
	m = apply(m, progressMsg{Model: "test-model", Step: 3, MaxSteps: 40, Tokens: 1200, MaxTokens: 8000})

	// No compaction yet ⇒ no ⟲ marker (the live token total is still shown).
	if contains(m.View(), "⟲") {
		t.Errorf("no compaction should render no ⟲ marker:\n%s", m.View())
	}
	if !contains(m.View(), "1.2k/8k") {
		t.Errorf("the live token total must remain:\n%s", m.View())
	}

	// A compaction memoryMsg surfaces ⟲N and subsequent ones update it.
	m = apply(m, memoryMsg{Compactions: 2})
	if !contains(m.View(), "⟲2") {
		t.Errorf("compaction indicator should show ⟲2:\n%s", m.View())
	}
	if !contains(m.View(), "1.2k/8k") {
		t.Errorf("the token total must stay alongside the indicator:\n%s", m.View())
	}
	// Product-lens evidence: the delivered after-compaction status line (matches
	// the ASCII mock in the plan §6, bottom).
	requireGolden(t, "status-compaction.golden", m.View())
	m = apply(m, memoryMsg{Compactions: 5})
	if !contains(m.View(), "⟲5") {
		t.Errorf("indicator should advance to ⟲5:\n%s", m.View())
	}
}

// TestCtxSegmentLadderWidths pins the whole occupancy ladder: which form is chosen at
// which column budget, and what each one says.
//
// The ladder is the feature. `ctx 25%` answered neither question a user has mid-run —
// 25% of what, and how long until the agent starts throwing history away — and the
// reason it was only a percentage is that a status bar has no room. So the test that
// matters is not "does it render" but "does the richest form that FITS get chosen",
// at the budgets a real terminal actually leaves.
func TestCtxSegmentLadderWidths(t *testing.T) {
	// The gauge from `kloo context --ctx 131072 --curator-budget 104857` on kloo's own
	// tree, measured 2026-10-07: used 26,370 of usable 104,857, trigger 58,617,
	// free 78,487, 32,247 before compaction.
	live := statusData{ctxUsed: 26370, ctxUsable: 104857, ctxTrigger: 58617, ctxFree: 78487, ctxToCompact: 32247}
	for _, tc := range []struct {
		name  string
		avail int
		want  string
	}{
		{"everything: used, percentage, free, trigger and distance", 59,
			"ctx 26.4k used 25% · 78.5k free · compact at 58.6k in 32.2k"},
		{"a 120-column terminal: the trigger value is the first thing dropped", 50,
			"ctx 26.4k used 25% · 78.5k free · compact in 32.2k"},
		{"free goes next, used/usable stands in for it", 37,
			"ctx 26.4k/105k 25% · compact in 32.2k"},
		{"the distance outlives the absolutes — it is what is about to happen", 26,
			"ctx 25% · compact in 32.2k"},
		{"no room for the distance: the absolutes come back", 18, "ctx 26.4k/105k 25%"},
		{"the old bare percentage is the floor, not the default", 7, "ctx 25%"},
		{"narrower than the floor drops the field rather than truncate a number", 6, ""},
	} {
		if got := live.ctxSegment(tc.avail); got != tc.want {
			t.Errorf("%s: ctxSegment(%d) = %q, want %q", tc.name, tc.avail, got, tc.want)
		}
		// One column short of a form's width must step DOWN the ladder, never render a
		// clipped number: a truncated "32.2k" reads as a different, smaller number.
		if got := live.ctxSegment(tc.avail - 1); len([]rune(got)) > tc.avail-1 {
			t.Errorf("%s: ctxSegment(%d) = %q overflows its budget", tc.name, tc.avail-1, got)
		}
	}
}

// TestCtxSegmentPastTheTriggerNeverPrintsANegative. At --working-set-tokens 12000 the
// first turn on kloo's own tree already exceeds the trigger (`kloo context` reports
// "-14,370 before compaction"). "compact in -14.4k" is a puzzle, so the bar states the
// fact instead, and names the overshoot where there is room for it — that is the
// actionable number, being what the next compaction has to shed.
func TestCtxSegmentPastTheTriggerNeverPrintsANegative(t *testing.T) {
	over := statusData{ctxUsed: 26370, ctxUsable: 104857, ctxTrigger: 12000, ctxFree: 78487, ctxToCompact: -14370}
	for _, avail := range []int{200, 59, 50, 37, 26, 18, 7} {
		got := over.ctxSegment(avail)
		if strings.Contains(got, "-") {
			t.Errorf("ctxSegment(%d) = %q prints a negative distance", avail, got)
		}
		if got == "" {
			t.Errorf("ctxSegment(%d) rendered nothing with a measured gauge", avail)
		}
	}
	if got, want := over.ctxSegment(200), "ctx 26.4k used 25% · 78.5k free · past compact 12k by 14.4k"; got != want {
		t.Errorf("widest form = %q, want %q", got, want)
	}
	if got, want := over.ctxSegment(50), "ctx 26.4k used 25% · 78.5k free · compacting"; got != want {
		t.Errorf("mid form = %q, want %q", got, want)
	}
}

// TestCtxSegmentWithoutATriggerNamesNone. Compaction can be switched off
// (--working-set-tokens negative with the fraction disabled), and then there is no
// trigger to be near. Every form that would print one has to be dropped rather than
// invent a number — doctor inventing a working-set figure from the same constants the
// assembler used is the defect this whole area exists to correct.
func TestCtxSegmentWithoutATriggerNamesNone(t *testing.T) {
	off := statusData{ctxUsed: 26370, ctxUsable: 104857, ctxFree: 78487}
	for _, avail := range []int{200, 50, 37, 18, 7} {
		got := off.ctxSegment(avail)
		if strings.Contains(got, "compact") {
			t.Errorf("ctxSegment(%d) = %q claims a trigger when compaction is off", avail, got)
		}
	}
	if got, want := off.ctxSegment(200), "ctx 26.4k used 25% · 78.5k free"; got != want {
		t.Errorf("widest form = %q, want %q", got, want)
	}

	// And with no gauge at all the field is absent, so a run before its first
	// assembled prompt renders exactly as it did before this field existed — the
	// property status.go has committed to since the gauge was first plumbed in.
	for _, s := range []statusData{{}, {ctxUsable: 104857}, {ctxUsed: 26370}} {
		if got := s.ctxSegment(200); got != "" {
			t.Errorf("no gauge should render nothing, got %q", got)
		}
	}
}

// TestStatusContextFieldReachesTheHeader wires the whole path: a contextMsg lands on
// the status data and the rendered header carries the occupancy.
//
// It also pins what the fitting can and cannot promise about the permission mode,
// because the first version of this test and its comment OVERCLAIMED. The field is
// budgeted to the leftover columns MINUS ONE, because renderHeader falls back to
// whole-line truncation whenever the gap between the two clusters drops below one
// column — so a form that exactly filled the space still cost the line its last
// character. Measured before that fix: at 90 columns with a 10-character model name
// the bar rendered `· au…`.
//
// What it cannot promise: when the lead cluster and the counters ALONE overflow the
// line, the mode goes whatever this field does. A 37-character model name at 85
// columns is already over before any occupancy is placed, and that is identical on
// master.
func TestStatusContextFieldReachesTheHeader(t *testing.T) {
	line := func(model string, width int) string {
		m := sized(New(Config{Model: model, MaxSteps: 500, MaxTokens: 480000}), width, th)
		m = apply(m, progressMsg{Model: model, Step: 18, MaxSteps: 500, Tokens: 1553679, MaxTokens: 480000})
		m = apply(m, contextMsg{Used: 26370, Usable: 104857, Trigger: 58617, Free: 78487, FreeToCompaction: 32247})
		return strings.Split(m.View(), "\n")[1]
	}
	wide := line("test-model", 140)
	for _, want := range []string{"26.4k used", "25%", "78.5k free", "compact at 58.6k", "32.2k", "auto"} {
		if !contains(wide, want) {
			t.Errorf("wide header missing %q:\n%s", want, wide)
		}
	}
	// A short model name leaves room for the mode at every width where the line can
	// hold `lead + counters + mode` at all, which starts at 70. The ctx field must
	// never be the reason it is lost.
	for _, w := range []int{140, 125, 120, 110, 100, 95, 90, 85, 80, 70} {
		got := line("test-model", w)
		if n := len([]rune(got)); n > w {
			t.Errorf("width %d: header is %d columns: %q", w, n, got)
		}
		if !contains(got, "· auto") {
			t.Errorf("width %d: the permission mode was lost to the ctx field:\n%s", w, got)
		}
	}
	// And the honest limit: a long model name loses the mode below 90 columns, with
	// the ctx field already fully suppressed, so nothing this field does could save
	// it. Asserted so the comment above cannot drift back into overclaiming.
	const longModel = "lokalai/muse-glimmer-30b-a3b-instruct"
	if got := line(longModel, 85); contains(got, "ctx ") {
		t.Errorf("at 85 columns with a 37-char model name the ctx field should be suppressed, not competing:\n%s", got)
	}
	if got := line(longModel, 90); !contains(got, "· auto") {
		t.Errorf("at 90 columns the mode should still fit a 37-char model name:\n%s", got)
	}
}

// TestTokensShortStaysUnambiguous. The field competes for columns with the permission
// mode, so it does not reuse `human` (which spends six columns on "104.9k"). What it
// must not do is round a number into a different one or drop a sign.
func TestTokensShortStaysUnambiguous(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{0, "0"}, {7, "7"}, {999, "999"},
		{1000, "1k"}, {12000, "12k"}, {26370, "26.4k"}, {58617, "58.6k"},
		{104857, "105k"}, {480000, "480k"},
		{1553679, "1.6M"},
		{-14370, "-14.4k"}, {-500, "-500"},
	} {
		if got := tokensShort(tc.in); got != tc.want {
			t.Errorf("tokensShort(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

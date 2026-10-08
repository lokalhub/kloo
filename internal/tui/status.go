package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// statusData is the header status-line state: model, step N/max, tokens used/
// budget, and the current permission mode. Updated from loop progress snapshots.
type statusData struct {
	effort      string
	model       string
	provider    string
	step        int
	maxSteps    int
	tokens      int
	maxTokens   int
	compactions int // working-memory compactions this run (⟲ marker; 0 ⇒ hidden)
	// ctxUsed / ctxUsable are WINDOW OCCUPANCY from the last assembled prompt
	// (agent.ContextGauge) — not the token counter beside them, which is cumulative
	// spend against the RUN budget and routinely exceeds the window by 10x because
	// the whole prompt is counted every turn. Having only the second number on screen
	// is why "how full is my context" was unanswerable. 0 ⇒ hidden, so a run with no
	// gauge yet renders exactly as before.
	ctxUsed   int
	ctxUsable int
	// ctxTrigger / ctxFree / ctxToCompact are the rest of the gauge, READ rather than
	// re-derived. Free is Usable-Used and ToCompact is Trigger-Used, so the status bar
	// could compute both — and must not. The arithmetic that decides where compaction
	// fires lives in one place (agent/memory.go triggerTokens, bounded by the
	// working-set cap), and a second copy of it in the renderer is exactly how
	// `kloo doctor` came to print a confident number that the loop disagreed with for
	// three releases. The bar displays what buildPrompt measured and nothing else.
	//
	// ctxTrigger <= 0 means compaction is off; the bar then falls back to the bare
	// percentage, because there is no trigger to be near.
	ctxTrigger   int
	ctxFree      int
	ctxToCompact int
	mode         Mode
}

// progressMsg is a loop-progress snapshot pumped into the program each step.
type progressMsg struct {
	Model     string
	Step      int
	MaxSteps  int
	Tokens    int
	MaxTokens int
}

// contextMsg carries window occupancy to the status line. Like memoryMsg it rides
// the existing progress plumbing and is nil-safe: no message is sent when nothing has
// been assembled, so the field stays hidden and the header renders as before.
// Every field is a straight copy of an agent.ContextGauge field, never a
// recomputation of one.
type contextMsg struct {
	Used   int
	Usable int
	// Trigger is where compaction begins and FreeToCompaction the distance to it
	// (negative once the prompt is past it). Trigger <= 0 ⇒ compaction disabled.
	Trigger          int
	Free             int
	FreeToCompaction int
}

// handleContext updates the occupancy fields from a contextMsg.
func (m Model) handleContext(msg contextMsg) (tea.Model, tea.Cmd) {
	m.status.ctxUsed, m.status.ctxUsable = msg.Used, msg.Usable
	m.status.ctxTrigger, m.status.ctxFree, m.status.ctxToCompact = msg.Trigger, msg.Free, msg.FreeToCompaction
	return m, nil
}

// memoryMsg carries the working-memory compaction count for the status line. It
// rides the existing progress plumbing (loop_bridge) — nil-safe: when memory is
// off no memoryMsg is sent, so the indicator stays hidden and the header renders
// exactly as before.
type memoryMsg struct {
	Compactions int
}

// handleMemory updates the compaction indicator from a memoryMsg.
func (m Model) handleMemory(msg memoryMsg) (tea.Model, tea.Cmd) {
	m.status.compactions = msg.Compactions
	return m, nil
}

// handleProgress stores the latest snapshot and refreshes the header.
func (m Model) handleProgress(msg progressMsg) (tea.Model, tea.Cmd) {
	if msg.Model != "" {
		m.status.model = msg.Model
		m.modelName = msg.Model
	}
	m.status.step = msg.Step
	if msg.MaxSteps > 0 {
		m.status.maxSteps = msg.MaxSteps
	}
	m.status.tokens = msg.Tokens
	if msg.MaxTokens > 0 {
		m.status.maxTokens = msg.MaxTokens
	}
	// A new step begins with the model call — clear the previous tool's in-flight
	// phrase so the thinking line shows "thinking" (not a stale "running <cmd>" from
	// a command that already finished) while the model is being called.
	m.activity = ""
	return m, nil
}

// ctxSegment renders window occupancy for the header, at the most detailed form
// that fits in `avail` columns. "" when there is nothing measured yet.
//
// WHY THE FIELD IS MORE THAN A PERCENTAGE. It used to be `ctx 25%`, and a
// percentage cannot answer either question a user actually has mid-run: how much
// room is left (25% of WHAT — the declared window, the usable slice, or the budget
// compaction enforces? they differ by 3.8x at --ctx 600000), and how soon does the
// agent start throwing history away. `kloo context` has printed both since v0.26.0,
// but it is a separate command that assembles its own turn-one prompt, so during a
// run the one place the live numbers existed was off screen.
//
// So the forms, widest first, each dropping the least useful thing. The widths are
// what the ladder is FOR, and they are pinned by TestCtxSegmentLadderWidths:
//
//	59  ctx 26.4k used 25% · 78.5k free · compact at 58.6k in 32.2k
//	50  ctx 26.4k used 25% · 78.5k free · compact in 32.2k
//	37  ctx 26.4k/105k 25% · compact in 32.2k
//	26  ctx 25% · compact in 32.2k
//	18  ctx 26.4k/105k 25%
//	 7  ctx 25%
//
// 50 is not an arbitrary target. Measured with a 10-character model name and
// `step 18/500 · 1553.7k/480k tok` beside it, the field is handed 54 columns at a
// 120-column terminal, 59 at 125 and 14 at 80 — so the 50-wide form carrying used,
// free, percentage AND the distance to compaction is the widest one that fits a
// standard wide terminal, and everything is on screen from 125.
//
// Past the trigger the distance is not printed as a negative — a bar reading
// "-14.4k left" is a puzzle, and the fact worth stating is that the loop compacts on
// the next step.
//
// TRIGGER <= 0 collapses to the forms that name no trigger, because printing one
// would mean inventing it. That branch is DEFENSIVE, not a configuration: no flag
// reaches it. `--working-set-tokens -1` disables the absolute cap and leaves the
// fraction, which at --ctx 131072 is 73,399; `--compact-trigger-frac -1` falls
// outside (0,1] and SetContextFractions ignores it silently, leaving 58,617. Driving
// the trigger to zero takes a usable window of 1 or less, and Assemble refuses that
// with ErrWindowTooSmall before any prompt is built. So the branch is here so the bar
// cannot print `compact in 0` if the gauge ever arrives zeroed — an earlier version
// of this comment named two flags for it and neither produces it.
func (s statusData) ctxSegment(avail int) string {
	// No gauge yet ⇒ nothing, so a run before its first assembled prompt renders
	// exactly as it did before this field existed.
	if s.ctxUsable <= 0 || s.ctxUsed <= 0 {
		return ""
	}
	pct := fmt.Sprintf("%d%%", 100*s.ctxUsed/s.ctxUsable)
	wide := fmt.Sprintf("ctx %s used %s · %s free", tokensShort(s.ctxUsed), pct, tokensShort(s.ctxFree))
	mid := fmt.Sprintf("ctx %s/%s %s", tokensShort(s.ctxUsed), tokensShort(s.ctxUsable), pct)
	forms := []string{wide, mid, "ctx " + pct}
	if s.ctxTrigger > 0 {
		full, brief := fmt.Sprintf("compact at %s in %s", tokensShort(s.ctxTrigger), tokensShort(s.ctxToCompact)),
			"compact in "+tokensShort(s.ctxToCompact)
		if s.ctxToCompact <= 0 {
			// Over the line already. Name the overshoot in the wide form (it is the
			// actionable number — it says how much the next compaction has to shed) and
			// just state the fact in the narrower ones.
			full, brief = fmt.Sprintf("past compact %s by %s", tokensShort(s.ctxTrigger), tokensShort(-s.ctxToCompact)), "compacting"
		}
		// Note the order of the last three: the DISTANCE TO COMPACTION outlives the
		// absolute used/usable pair. A bar that has room for only one more field should
		// spend it on the thing that is about to happen, not on a number the percentage
		// already implies.
		forms = []string{
			wide + " · " + full,
			wide + " · " + brief,
			mid + " · " + brief,
			"ctx " + pct + " · " + brief,
			mid,
			"ctx " + pct,
		}
	}
	for _, f := range forms {
		if lipgloss.Width(f) <= avail {
			return f
		}
	}
	// Narrower than `ctx 25%`: drop the field rather than render a truncated
	// number, which would read as a different number.
	return ""
}

// tokensShort is a token count in the fewest columns that stay unambiguous.
//
// It is not `human` (transcript.go), which always prints one decimal: `human(104857)`
// is "104.9k", six columns for a tenth of a percent of precision on a budget figure,
// and this field is competing for the columns the permission mode needs. Above 100k
// the decimal is dropped, an exact multiple of 1000 loses its ".0", and the sign is
// kept because Free goes negative when the first turn alone overruns the usable
// window — a case `kloo context` already prints and the bar must not swallow.
func tokensShort(n int) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%s%.1fM", sign, float64(n)/1e6)
	case n%1000 == 0 && n >= 1000:
		return fmt.Sprintf("%s%dk", sign, n/1000)
	case n >= 100_000:
		return fmt.Sprintf("%s%.0fk", sign, float64(n)/1000)
	case n >= 1000:
		return fmt.Sprintf("%s%.1fk", sign, float64(n)/1000)
	default:
		return fmt.Sprintf("%s%d", sign, n)
	}
}

// displayVersion formats the build version for the header: "" or "dev" → "dev"
// (a local build), a bare semver like "0.2.0" gets a "v" prefix ("v0.2.0"), and
// anything already prefixed/odd is shown as-is.
func displayVersion(v string) string {
	if v == "" {
		return "dev"
	}
	if len(v) > 0 && v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}

// renderHeader renders the bordered status line, reframed (task 06) to LEAD with
// `kloo • model • effort` + the live token total, demoting `step N/max` to a
// dim/secondary field (header.html):
//
//	┌ kloo  qwen2.5-coder · medium     step 18/80 · 14.4k/200k tok · auto ┐
//
// The token total is the live, non-zero value from Phase 00's OnProgress.
func (m Model) renderHeader() string {
	s := m.status
	s.mode = m.mode // mode always reflects the live dial
	if s.model == "" {
		s.model = m.modelName
	}

	// Lead cluster: kloo <version> • model • effort.
	modelLabel := s.model
	if s.provider != "" {
		modelLabel = s.provider + "/" + modelLabel
	}
	lead := "kloo " + displayVersion(m.version) + "  " + modelLabel
	if s.effort != "" {
		lead += " · " + s.effort
	}

	// Trailing cluster: step (dim/secondary) · live token total · [⟲ compactions] · mode.
	step := muted.Render(fmt.Sprintf("step %d/%d", s.step, s.maxSteps))
	// maxTokens 0 ⇒ unbounded: show a plain counter, not "N/0".
	tok := human(s.tokens) + " tok"
	if s.maxTokens > 0 {
		tok = human(s.tokens) + "/" + human(s.maxTokens) + " tok"
	}
	right := fmt.Sprintf("%s · %s", step, tok)
	tail := ""
	if s.compactions > 0 {
		// Working memory folded the transcript this run — surfaced only when it
		// actually happened, so a no-compaction run renders identically to before.
		tail += fmt.Sprintf(" · ⟲%d", s.compactions)
	}
	tail += fmt.Sprintf(" · %s", s.mode)

	inner := m.width - 2 - 2 // border + padding
	// Window occupancy, beside the cumulative spend and deliberately labelled
	// differently: the ctx segment is how full the window is right now,
	// `14.4k/200k tok` is what the run has spent in total. They were previously
	// indistinguishable because only the second one existed.
	//
	// The segment is fitted to the columns left over once everything else on the line
	// is placed, so a narrow terminal loses DETAIL from this field rather than having
	// this field push the line into the whole-line truncation below, which cuts the
	// END — the permission mode, the field you must never misread.
	//
	// That is all the fitting can promise, and the original form of this comment
	// overclaimed. When the lead cluster and the counters ALONE overflow the line the
	// mode is lost whatever this field does: measured at 80 columns with the model
	// name `lokalai/muse-glimmer-30b-a3b-instruct`, the line truncates to
	// `… 1553.7k/480k …` with the ctx field already suppressed. Identical on master,
	// so not a regression, but not something this field can fix either.
	const sep = " · "
	// -1: renderHeader falls back to whole-line truncation when the gap between the
	// two clusters is below ONE column, so a ctx form that exactly fills the space
	// still costs the line its last character — the permission mode. Measured: at 90
	// columns with a 10-character model name the chosen form left gap 0 and the bar
	// rendered `· au…`. The field therefore budgets itself one column short of the
	// space it has.
	avail := inner - lipgloss.Width(lead) - lipgloss.Width(right) - lipgloss.Width(tail) - lipgloss.Width(sep) - 1
	if seg := s.ctxSegment(avail); seg != "" {
		right += sep + seg
	}
	right += tail
	gap := inner - lipgloss.Width(lead) - lipgloss.Width(right)
	var line string
	if gap < 1 {
		line = truncate(lead+" "+right, inner)
	} else {
		line = lead + strings.Repeat(" ", gap) + right
	}
	return lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Width(m.width-2).Padding(0, 1).Render(line)
}

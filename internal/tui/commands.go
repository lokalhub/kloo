package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
)

// isSlash reports whether a submitted line is a slash command.
func isSlash(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "/")
}

// ackReplies maps a bare acknowledgment / closing to a short conversational reply.
// A weak model, handed a no-op input like "thanks" on a RESUMED session (where the
// finished task is still in context), tends to re-interpret it as "do the task
// again" and launches a fresh tool-driven run — even though the system prompt tells
// it to just finish. So kloo short-circuits the obvious cases deterministically:
// reply in one line, launch NO run. Kept to UNMISTAKABLE whole-message acks (a
// real request like "thanks, now add a tab" is longer and won't match), so we never
// swallow actual work.
var ackReplies = map[string]string{
	"thanks":           "You're welcome! Let me know if you'd like any changes or another task.",
	"thank you":        "You're welcome! Let me know if you'd like any changes or another task.",
	"thank u":          "You're welcome! Let me know if you'd like any changes or another task.",
	"thanks so much":   "You're welcome! Let me know if you'd like any changes or another task.",
	"thanks a lot":     "You're welcome! Let me know if you'd like any changes or another task.",
	"ty":               "You're welcome! Let me know if you'd like any changes or another task.",
	"thx":              "You're welcome! Let me know if you'd like any changes or another task.",
	"tysm":             "You're welcome! Let me know if you'd like any changes or another task.",
	"much appreciated": "Anytime! Happy to help with the next thing.",
	"appreciate it":    "Anytime! Happy to help with the next thing.",
	"appreciated":      "Anytime! Happy to help with the next thing.",
	"cheers":           "Anytime! Happy to help with the next thing.",
	"great":            "Glad it helped! Happy to make tweaks or take on the next thing.",
	"great work":       "Glad it helped! Happy to make tweaks or take on the next thing.",
	"nice":             "Glad it helped! Happy to make tweaks or take on the next thing.",
	"nice work":        "Glad it helped! Happy to make tweaks or take on the next thing.",
	"perfect":          "Glad it helped! Happy to make tweaks or take on the next thing.",
	"awesome":          "Glad it helped! Happy to make tweaks or take on the next thing.",
	"excellent":        "Glad it helped! Happy to make tweaks or take on the next thing.",
	"good job":         "Glad it helped! Happy to make tweaks or take on the next thing.",
	"well done":        "Glad it helped! Happy to make tweaks or take on the next thing.",
	"looks good":       "Glad it helped! Happy to make tweaks or take on the next thing.",
	"lgtm":             "Glad it helped! Happy to make tweaks or take on the next thing.",
	"ok":               "👍 I'm here whenever you want to continue.",
	"okay":             "👍 I'm here whenever you want to continue.",
	"k":                "👍 I'm here whenever you want to continue.",
	"kk":               "👍 I'm here whenever you want to continue.",
	"alright":          "👍 I'm here whenever you want to continue.",
	"got it":           "👍 I'm here whenever you want to continue.",
	"sounds good":      "👍 I'm here whenever you want to continue.",
	"all good":         "👍 I'm here whenever you want to continue.",
	"no thanks":        "👍 I'm here whenever you want to continue.",
	"that's all":       "👍 I'm here whenever you want to continue.",
	"bye":              "👋 Bye for now — reopen the session anytime to continue.",
	"goodbye":          "👋 Bye for now — reopen the session anytime to continue.",
}

// ackReply returns a canned reply if the line is a bare acknowledgment / closing,
// else ("", false). Matching is on the message reduced to lowercase letters +
// single spaces (so "Thanks!", "thank you :)", "thanks 🙏" all match), guarding
// against catching a real request by only honouring the exact whole-message acks.
func ackReply(line string) (string, bool) {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(strings.TrimSpace(line)) {
		switch {
		case r >= 'a' && r <= 'z', r == '\'':
			b.WriteRune(r)
			prevSpace = false
		case r == ' ' || r == '\t':
			if !prevSpace && b.Len() > 0 {
				b.WriteRune(' ')
			}
			prevSpace = true
		}
		// everything else (punctuation, emoji, digits) is dropped
	}
	reply, ok := ackReplies[strings.TrimSpace(b.String())]
	return reply, ok
}

// runSlash parses and dispatches a slash command, taking effect on state. An
// unknown slash produces a clear visible message and submits nothing.
func (m Model) runSlash(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(strings.TrimSpace(line))
	cmd := fields[0]
	arg := ""
	if len(fields) > 1 {
		arg = strings.Join(fields[1:], " ")
	}

	switch cmd {
	case "/add":
		return m.slashAdd(arg), nil
	case "/models":
		return m.slashModels(), nil
	case "/model":
		return m.slashModel(arg), nil
	case "/mode":
		return m.slashMode(arg), nil
	case "/provider":
		return m.slashProvider(arg), nil
	case "/profile":
		return m.slashProfile(arg), nil
	case "/verbose":
		return m.slashVerbose(), nil
	case "/context":
		return m.slashContext(), nil
	default:
		return m.appendItem(infoItem{text: "unknown command: " + cmd}), nil
	}
}

// slashContext prints the window-occupancy breakdown for the last assembled prompt
// — the same measurement `kloo context` renders on the CLI, in the transcript where
// the user is already looking. It is a READ of agent.ContextGauge (what buildPrompt
// already measured), never a re-derivation: the section split, the trigger and the
// free-to-compaction figure are copied, not recomputed, because a second copy of the
// compaction arithmetic in the renderer is how `kloo doctor` came to print a number
// the loop disagreed with for three releases.
//
// Before the first run there is no assembled prompt, and the command says so rather
// than printing zeros as if they were data.
func (m Model) slashContext() Model {
	if m.runner == nil {
		return m.appendItem(infoItem{text: "/context: no runner attached — nothing has been assembled yet"})
	}
	g, ok := m.runner.ContextGauge()
	if !ok {
		// No run has assembled a prompt yet. On a RESUMED session the prior
		// conversation is already carried (SessionHistory), so measure what the next
		// turn would send rather than claiming nothing is loaded — that mismatch is
		// what made /context look empty on resume despite the context being there.
		if rg, rok := m.runner.MeasureResumed(context.Background()); rok {
			return m.appendItem(infoItem{text: renderContextGauge(rg) +
				"\n  (resumed session — measured from carried history; no run yet this launch)"})
		}
		return m.appendItem(infoItem{text: "/context: no prompt assembled yet — submit a task first, then /context shows what it cost"})
	}
	return m.appendItem(infoItem{text: renderContextGauge(g)})
}

// renderContextGauge formats a ContextGauge as the transcript's /context block: one
// line per consumer with its share of the usable window, then the totals. Percentages
// are of USABLE (the budget the prompt was built to) and are omitted when there is no
// window to divide by, so a --ctx of 0 prints counts instead of a nonsense percentage.
func renderContextGauge(g agent.ContextGauge) string {
	var b strings.Builder
	b.WriteString("context\n")
	fmt.Fprintf(&b, "  window %s  usable %s  compaction at %s (%s binding)\n",
		tokensShort(g.Window), tokensShort(g.Usable), tokensShort(g.Trigger), g.Binding)

	pct := func(n int) string {
		if g.Usable <= 0 {
			return ""
		}
		return fmt.Sprintf("%4.0f%%", 100*float64(n)/float64(g.Usable))
	}
	row := func(label string, n int, extra string) {
		fmt.Fprintf(&b, "  %-13s %8s  %s  %s\n", label, tokensShort(n), pct(n), extra)
	}
	row("system+schema", g.SystemTokens+g.SchemaTokens,
		fmt.Sprintf("(system %s + schemas %s)", tokensShort(g.SystemTokens), tokensShort(g.SchemaTokens)))
	mapNote := "(" + g.MapPlacement + ")"
	if g.MapBudget > 0 {
		mapNote = fmt.Sprintf("(%s; budget %s", g.MapPlacement, tokensShort(g.MapBudget))
		if g.MapOverBudget > 0 {
			mapNote += fmt.Sprintf(" — over by %s (%.2f%%)", tokensShort(g.MapOverBudget), g.MapOverBudgetPct)
		}
		mapNote += ")"
	}
	row("repo map", g.RepoMapTokens, mapNote)
	row("pinned/hot", g.PinnedHotTokens, "(task + pins + todos — re-sent every turn)")
	row("history", g.HistoryTokens, "(summary + recent tail — what compaction sheds)")
	if g.Unattributed != 0 {
		row("unattributed", g.Unattributed, "(estimator rounding; a large value here is a bug)")
	}
	if g.Usable <= 0 {
		row("used", g.Used, fmt.Sprintf("in %d messages", g.Messages))
		fmt.Fprintf(&b, "  %-13s %8s  (no window configured — set --ctx to measure occupancy)\n", "free", "n/a")
	} else {
		row("used", g.Used, fmt.Sprintf("of usable, in %d messages", g.Messages))
		row("free", g.Free, fmt.Sprintf("— %s before compaction", tokensShort(g.FreeToCompaction)))
	}
	if g.Usable > 0 && g.FreeToCompaction < 0 {
		b.WriteString("  note: already above the compaction trigger — the loop compacts on step one\n")
	}
	if g.Used > g.Usable {
		b.WriteString("  note: the first turn alone exceeds the usable window — the assembler will shed to fit\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// slashVerbose toggles kloo's own numbers on screen: the step/token counters in the
// header and the counter banner under each outcome. They are off by default because
// they are kloo's bookkeeping rather than the user's work; this is the switch for
// when the bookkeeping IS the question (a run that stopped early, a benchmark, a
// budget being tuned).
func (m Model) slashVerbose() Model {
	m.verbose = !m.verbose
	if m.verbose {
		return m.appendItem(infoItem{text: "verbose: on — step/token counters in the header and a full stop report per run"})
	}
	return m.appendItem(infoItem{text: "verbose: off — kloo shows the work, not its counters"})
}

func (m Model) slashAdd(path string) Model {
	if strings.TrimSpace(path) == "" {
		return m.appendItem(infoItem{text: "/add needs a path"})
	}
	m.contextFiles = append(append([]string{}, m.contextFiles...), path)
	return m.appendItem(infoItem{text: "added " + path + " to context"})
}

func (m Model) slashModel(name string) Model {
	name = strings.TrimSpace(name)
	if name == "" {
		return m.openModelPicker()
	}
	return m.selectModelName(name)
}

func (m Model) slashMode(value string) Model {
	value = strings.TrimSpace(value)
	switch Mode(value) {
	case ModeAuto, ModeAcceptEdits, ModeApproveEach:
		m.mode = Mode(value)
		m.status.mode = m.mode
		return m.appendItem(infoItem{text: "mode: " + value})
	default:
		return m.appendItem(infoItem{text: "invalid mode: " + value + " (valid: auto, accept-edits, approve-each)"})
	}
}

// slashProvider switches the active provider (endpoint+key) for the next run
// without restarting kloo. With no argument it lists available providers from
// the profile. With a name it applies that provider's endpoint+key immediately.
func (m Model) slashProvider(name string) Model {
	name = strings.TrimSpace(name)
	providers, err := config.ListProviders(m.profilePath, m.getenv)
	if err != nil {
		return m.appendItem(infoItem{text: "provider: could not load profile: " + err.Error()})
	}
	if len(providers) == 0 {
		return m.appendItem(infoItem{text: "provider: no providers defined in profile (add a \"providers\" block to your profiles.json)"})
	}
	if name == "" {
		// list available providers
		lines := []string{"providers:"}
		for _, p := range providers {
			active := ""
			if p.Name == m.runtime.Provider {
				active = " ← active"
			}
			lines = append(lines, fmt.Sprintf("  %s  %s%s", p.Name, p.Endpoint, active))
		}
		return m.appendItem(infoItem{text: strings.Join(lines, "\n")})
	}
	// find the named provider
	for _, p := range providers {
		if p.Name == name {
			m.runtime.Provider = p.Name
			m.runtime.Endpoint = p.Endpoint
			m.runtime.APIKey = p.APIKey
			m.status.provider = p.Name
			return m.appendItem(infoItem{text: fmt.Sprintf("provider: %s (%s)", p.Name, p.Endpoint)})
		}
	}
	// not found — show options
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Name)
	}
	return m.appendItem(infoItem{text: fmt.Sprintf("provider %q not found (available: %s)", name, strings.Join(names, ", "))})
}

// slashProfile reloads a DIFFERENT profiles.json for subsequent runs (C6). It
// re-resolves the runtime (provider/endpoint/key/model tuning/context/temperature/
// tool-format) preserving the launch CLI flags, and — on success — swaps the runtime,
// profile path, and model/provider status fields. `/provider` remains scoped to the
// loaded profile (it now lists the NEW profile's providers). A failed load leaves the
// current runtime fully intact and shows a clear info line. API keys are never printed.
func (m Model) slashProfile(path string) Model {
	path = strings.TrimSpace(path)
	if path == "" {
		return m.appendItem(infoItem{text: "/profile needs a path to a profiles.json"})
	}
	if m.reloadProfile == nil {
		return m.appendItem(infoItem{text: "/profile is not available in this session"})
	}
	rc, summary, err := m.reloadProfile(path)
	if err != nil {
		// Keep the current runtime/profile intact; surface a clear, secret-free error.
		return m.appendItem(infoItem{text: "profile: could not load " + path + " — " + oneLineErr(err)})
	}
	m.runtime = rc
	m.profilePath = path
	if rc.Model != "" {
		m.modelName = rc.Model
		m.status.model = rc.Model
	}
	m.status.provider = rc.Provider
	return m.appendItem(infoItem{text: fmt.Sprintf("profile: loaded %s (%s)", path, summary)})
}

// oneLineErr collapses an error message to a single bounded line for a TUI info
// line (config errors never contain secrets; this just keeps the line tidy).
func oneLineErr(err error) string {
	return oneLine(err.Error())
}

// submitTask appends the user line and, if a Runner is wired, launches an
// autonomous run under a cancelable context. Without a Runner (unit tests) it
// just records the submission.
//
// One run at a time: a non-slash submission is ignored (with a message) while a
// run is active — otherwise a second goroutine would race the first on the
// shared loop's hooks and overwrite the cancel func, leaving the first run
// uninterruptible (Esc interrupts the active run before a new one can start).
func (m Model) submitTask(line string) (tea.Model, tea.Cmd) {
	if m.running {
		return m.appendItem(infoItem{text: "a run is already active — press Esc to interrupt it first"}), nil
	}
	// A dropped run's goroutine is still live and still owns the shared Loop's hooks;
	// starting a second one here is the race the comment above describes.
	if m.abandoned {
		return m.appendItem(infoItem{text: "the run you dropped hasn't finished stopping yet — a moment, or Ctrl-C to quit kloo"}), nil
	}
	m = m.appendItem(userItem{text: line}) // transcript shows the line as typed (paste placeholders kept short)
	// A bare "thanks"/"ok"/"nice" is not a task — reply in one line and launch NO
	// run, so a weak model can't mistake it for "redo the work" (which it did even
	// with the system prompt telling it to just finish).
	if reply, ok := ackReply(line); ok {
		return m.appendItem(assistantItem{content: reply}), nil
	}
	if m.runner == nil {
		return m, nil
	}
	task := m.expandPastes(line) // the model receives the full pasted text
	// Attached images leave the prompt text and travel as content parts.
	task, images := m.takeImages(task)
	m.pastes = nil // consumed by this submission
	m.images = nil
	// C8: start a fresh compact activity log + summary for the run (the full
	// transcript still records everything, including the request itself).
	m.latestSummary = ""
	m.activityLog = nil
	m.frozenSummaryText = ""
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.running = true
	m.runStart = time.Now()
	m.verb = randomVerb()
	m.tickCount = 0
	files := append([]string{}, m.contextFiles...)
	mode := m.mode
	runtime := m.runtime
	runner := m.runner
	return m, tea.Batch(
		func() tea.Msg {
			go runner.Start(ctx, task, runtime, mode, files, images)
			return nil
		},
		tickCmd(),
	)
}

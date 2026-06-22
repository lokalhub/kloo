package tui

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// thinkingVerbs are the whimsical, rotating status words shown while a run is in
// flight (Claude-Code style). One is chosen at run start and rotated every few
// seconds so a long step doesn't feel frozen.
var thinkingVerbs = []string{
	"Cooking", "Percolating", "Noodling", "Conjuring", "Tinkering",
	"Marinating", "Scheming", "Brewing", "Spelunking", "Orchestrating",
	"Pondering", "Whirring", "Galloping", "Beaming", "Moonwalking",
	"Computing", "Wrangling", "Munging", "Hatching", "Riffing",
	"Synthesizing", "Finagling", "Plotting", "Simmering", "Tinkering",
}

// spinnerFrames is the braille spinner cycled each tick.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Thinking-line styles source their colour from the central palette (theme.go):
// the verb/spinner use the accent (pink), the metadata uses muted (dim grey).
var thinkingStyle = accent
var thinkingMeta = muted

// tickMsg drives the thinking-line animation (spinner + verb rotation).
type tickMsg struct{}

const (
	tickInterval  = 120 * time.Millisecond
	verbEveryTick = 25 // ~3s between verb changes (25 * 120ms)
)

// tickCmd schedules the next animation frame.
func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// randomVerb picks a thinking verb (global rand is auto-seeded in Go 1.20+).
func randomVerb() string { return thinkingVerbs[rand.Intn(len(thinkingVerbs))] }

// handleTick advances the spinner and periodically rotates the verb, rescheduling
// itself only while a run is active (so the animation self-terminates on report).
func (m Model) handleTick(tickMsg) (tea.Model, tea.Cmd) {
	if !m.running {
		return m, nil
	}
	m.spinFrame = (m.spinFrame + 1) % len(spinnerFrames)
	m.tickCount++
	if m.tickCount%verbEveryTick == 0 {
		m.verb = randomVerb()
	}
	return m, tickCmd()
}

// renderThinking is the animated status line shown in place of the hint while a
// run is active, reframed (task 06) to carry a compact activity phrase:
// "⠹ Moonwalking…  editing src/app/app.ts · 12s · 14.4k tok · esc to interrupt".
// The activity phrase comes from the current tool (display-only); the token
// count is Phase 00's live value.
func (m Model) renderThinking() string {
	verb := m.verb
	if verb == "" {
		verb = "Working"
	}
	elapsed := 0
	if !m.runStart.IsZero() {
		elapsed = int(time.Since(m.runStart).Seconds())
	}
	spin := spinnerFrames[m.spinFrame%len(spinnerFrames)]
	head := thinkingStyle.Render(fmt.Sprintf("%s %s…", spin, verb))

	parts := make([]string, 0, 4)
	if m.activity != "" {
		parts = append(parts, m.activity)
	}
	parts = append(parts,
		fmt.Sprintf("%ds", elapsed),
		fmt.Sprintf("%s tok", human(m.status.tokens)),
		"esc to interrupt",
	)
	meta := thinkingMeta.Render("  " + strings.Join(parts, " · "))
	return head + meta
}

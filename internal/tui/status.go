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
	mode        Mode
}

// progressMsg is a loop-progress snapshot pumped into the program each step.
type progressMsg struct {
	Model     string
	Step      int
	MaxSteps  int
	Tokens    int
	MaxTokens int
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
	if s.compactions > 0 {
		// Working memory folded the transcript this run — surfaced only when it
		// actually happened, so a no-compaction run renders identically to before.
		right += fmt.Sprintf(" · ⟲%d", s.compactions)
	}
	right += fmt.Sprintf(" · %s", s.mode)

	inner := m.width - 2 - 2 // border + padding
	gap := inner - lipgloss.Width(lead) - lipgloss.Width(right)
	var line string
	if gap < 1 {
		line = truncate(lead+" "+right, inner)
	} else {
		line = lead + strings.Repeat(" ", gap) + right
	}
	return lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Width(m.width-2).Padding(0, 1).Render(line)
}

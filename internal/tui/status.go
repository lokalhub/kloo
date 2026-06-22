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
	effort    string
	model     string
	step      int
	maxSteps  int
	tokens    int
	maxTokens int
	mode      Mode
}

// progressMsg is a loop-progress snapshot pumped into the program each step.
type progressMsg struct {
	Model     string
	Step      int
	MaxSteps  int
	Tokens    int
	MaxTokens int
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
	return m, nil
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

	// Lead cluster: kloo • model • effort.
	lead := "kloo  " + s.model
	if s.effort != "" {
		lead += " · " + s.effort
	}

	// Trailing cluster: step (dim/secondary) · live token total · mode.
	step := muted.Render(fmt.Sprintf("step %d/%d", s.step, s.maxSteps))
	right := fmt.Sprintf("%s · %s/%s tok · %s", step, human(s.tokens), human(s.maxTokens), s.mode)

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

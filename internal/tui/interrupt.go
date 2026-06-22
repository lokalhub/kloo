package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// handleInterruptKeys implements the idle-vs-running key distinction:
//   - Ctrl-C / Esc while a run is ACTIVE → interrupt the run (cancel ctx); does
//     NOT quit the program.
//   - Ctrl-C from IDLE → quit the program.
//   - `q` from IDLE with an empty input → quit (so typing a task containing 'q'
//     still works); Esc from idle is a no-op.
//
// Returns handled=false to let normal key handling proceed.
func (m Model) handleInterruptKeys(msg tea.KeyMsg) (handled bool, nm tea.Model, cmd tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		if m.running {
			nm, cmd = m.interrupt()
			return true, nm, cmd
		}
		return true, m, tea.Quit
	case tea.KeyEsc:
		if m.running {
			nm, cmd = m.interrupt()
			return true, nm, cmd
		}
		return true, m, nil // idle Esc: no-op
	case tea.KeyRunes:
		if string(msg.Runes) == "q" && !m.running && m.input.Value() == "" {
			return true, m, tea.Quit
		}
	}
	return false, m, nil
}

// interrupt cancels the active run's context. The run then returns a
// reportMsg with reason "interrupted", which handleReport renders and returns
// the UI to idle.
//
// If an edit is currently HELD for an approve-each confirm, the pending
// responder is resolved (rejected) and the confirm cleared — otherwise the loop
// goroutine, blocked waiting for y/n, would never unblock and the run would
// deadlock (loop.Run never returns, the program becomes unquittable). This
// complements the run ctx-cancel, which the OnBeforeEdit gate also selects on.
func (m Model) interrupt() (tea.Model, tea.Cmd) {
	if m.confirm != nil {
		if m.confirm.respond != nil {
			go m.confirm.respond(false) // unblock the held edit (rejected)
		}
		m.confirm = nil
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	return m, nil
}

// reportMsg is the terminal outcome of a run, translated from the Phase-04
// agent.Report by the runner (keeping the TUI decoupled from internal/agent).
type reportMsg struct {
	Reason     string // success | budget-exceeded | churn | error | interrupted
	Steps      int
	Tokens     int
	MaxTokens  int
	Elapsed    string
	Detail     string // e.g. "max-steps (40/40)" or "same failure ×3"
	VerifyCmd  string
	VerifyExit int
	RolledBack bool
}

// handleReport renders the terminal state and returns the UI to idle. A
// user-initiated interrupt renders a simple line; an autonomous stop (budget /
// churn / error / success) renders a distinct terminal banner.
func (m Model) handleReport(msg reportMsg) (tea.Model, tea.Cmd) {
	m.running = false
	m.cancel = nil
	m.streamIdx = -1
	m.activity = "" // an in-flight phrase must not outlive the run (task 06)
	if msg.Reason == "interrupted" {
		return m.appendItem(interruptItem{}), nil
	}
	return m.appendItem(reportItem{r: msg}), nil
}

// reportItem renders the autonomous stop-report banner per the mock, visibly
// distinct from the interrupt line.
type reportItem struct{ r reportMsg }

func (ri reportItem) render(width int) string {
	r := ri.r
	title := bannerTitle(r.Reason)

	var b strings.Builder
	fmt.Fprintf(&b, "■ run stopped — %s\n", title)
	if r.Detail != "" {
		fmt.Fprintf(&b, "reason:  %s\n", r.Detail)
	}
	fmt.Fprintf(&b, "steps:   %d · tokens: %s/%s · elapsed: %s\n", r.Steps, human(r.Tokens), human(r.MaxTokens), r.Elapsed)
	if r.VerifyCmd != "" {
		// Colour the verify outcome green pass / red fail (task 05), consistent
		// with the run-command exit result.
		result := verifyPass.Render(fmt.Sprintf("exit %d %s", r.VerifyExit, glyphPass))
		if r.VerifyExit != 0 {
			result = verifyFail.Render(fmt.Sprintf("exit %d %s", r.VerifyExit, glyphFail))
		}
		fmt.Fprintf(&b, "last verify: %s → %s\n", r.VerifyCmd, result)
	}
	if r.RolledBack {
		b.WriteString("workspace rolled back to checkpoint. Refine the task and retry.")
	}

	border := lipgloss.NormalBorder()
	style := cardStyle(width, border)
	if r.Reason != "success" {
		style = style.BorderForeground(warningColor) // attention tint (theme.go)
	}
	return style.Render(strings.TrimRight(b.String(), "\n"))
}

func bannerTitle(reason string) string {
	switch reason {
	case "budget-exceeded":
		return "BUDGET EXCEEDED"
	case "churn":
		return "NO PROGRESS (churn)"
	case "error":
		return "ERROR"
	case "success":
		return "COMPLETE"
	default:
		return strings.ToUpper(reason)
	}
}

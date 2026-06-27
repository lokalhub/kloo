package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Region heights (border lines included).
const (
	headerHeight   = 3 // bordered status line
	activityHeight = 2 // thinking/spinner line + a blank spacer below it, ABOVE the input (Claude-style)
	inputHeight    = 3 // bordered input line
	hintHeight     = 1 // interrupt / dial hint line
)

// View implements tea.Model: it composes header + transcript + input + hint.
func (m Model) View() string {
	if m.width == 0 {
		return "" // not sized yet
	}
	header := m.renderHeader()
	transcript := m.renderTranscriptRegion()
	activity := m.renderActivityLine()
	picker := m.renderModelPicker()
	input := m.renderInput()
	hint := m.renderHint()
	parts := []string{header, transcript, activity}
	if picker != "" {
		parts = append(parts, picker)
	}
	parts = append(parts, input, hint)
	return strings.Join(parts, "\n")
}

// renderTranscriptRegion renders the scroll region (viewport when ready).
func (m Model) renderTranscriptRegion() string {
	if m.vpReady {
		vp := m.vp
		if m.picker != nil {
			if h := vp.Height - lipgloss.Height(m.renderModelPicker()); h > 1 {
				vp.Height = h
			}
		}
		return vp.View()
	}
	h := max(m.height-headerHeight-activityHeight-inputHeight-hintHeight, 1)
	return strings.Repeat("\n", h-1)
}

// renderTranscript joins all transcript items into the viewport content. Blocks
// are separated by a single blank line ("\n\n") so cards and prose read airy
// (task 07); the separation is owned here — items stay pure formatters of their
// own content. Run cards are stamped with the model's expand flag (task 03)
// before rendering, so ctrl+o toggles all run-card output without widening the
// shared item.render interface.
func (m Model) renderTranscript() string {
	if len(m.transcript) == 0 {
		return ""
	}
	w := m.width - 2
	blocks := make([]string, 0, len(m.transcript))
	for _, it := range m.transcript {
		if rc, ok := it.(runCardItem); ok {
			rc.expanded = m.expanded
			it = rc
		}
		blocks = append(blocks, it.render(w))
	}
	return strings.Join(blocks, "\n\n")
}

// transcriptContent is the viewport body, BOTTOM-anchored: when the transcript is
// shorter than the viewport, blank lines are prepended so the newest content sits
// just ABOVE the input (chat-style), instead of stranded at the top with a big gap
// below. Once the content fills the viewport, the pad is zero and it scrolls
// normally. Use this everywhere the viewport content is set.
func (m Model) transcriptContent() string {
	body := m.renderTranscript()
	if !m.vpReady {
		return body
	}
	if pad := m.vp.Height - lipgloss.Height(body); pad > 0 {
		return strings.Repeat("\n", pad) + body
	}
	return body
}

// renderInput renders the bordered input region with slash hints to the right.
func (m Model) renderInput() string {
	box := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		Width(m.width - 2)

	left := m.input.View()
	right := slashHints
	gap := m.width - 2 - 2 - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		// Too narrow: drop the hints.
		return box.Render(left)
	}
	return box.Render(left + strings.Repeat(" ", gap) + right)
}

// renderHint renders the line under the input: the animated thinking line while
// a run is active (verbs.go), otherwise the interrupt + permission-dial hint.
// renderActivityLine is the Claude-style working line shown ABOVE the input while
// a run is active (verb + spinner + activity + tokens). It reserves activityHeight
// (2) lines: the thinking line plus a trailing blank spacer, so the thinking display
// isn't cramped against the input border. Idle returns two blank lines, keeping the
// region height stable so the viewport math (model.go) never shifts.
func (m Model) renderActivityLine() string {
	if m.running {
		return truncate(m.renderThinking(), m.width) + "\n" // thinking line + blank spacer
	}
	return "\n" // two blank lines (stable height) when idle
}

func (m Model) renderHint() string {
	dial := "permission dial: auto › accept-edits › approve-each"
	line := "Esc/Ctrl-C interrupt · " + dial
	return truncate(line, m.width)
}

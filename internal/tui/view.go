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
	menu := m.renderSlashMenu()
	input := m.renderInput()
	hint := m.renderHint()
	parts := []string{header, transcript}
	// C8: while a run is active, pin the task header, latest assistant summary, and
	// compact activity log between the transcript and the thinking line (mock order).
	if rr := m.renderRunningRegions(); rr != "" {
		parts = append(parts, rr)
	}
	parts = append(parts, activity)
	if picker != "" {
		parts = append(parts, picker)
	}
	if menu != "" {
		parts = append(parts, menu)
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
		if m.menu != nil {
			if h := vp.Height - lipgloss.Height(m.renderSlashMenu()); h > 1 {
				vp.Height = h
			}
		}
		// C8: the pinned running regions consume viewport rows while active — shrink a
		// COPY for rendering (like the picker/menu) so scroll math on the real vp is
		// untouched and header/input never overlap at small sizes. Because the copy is
		// shorter than the real vp, re-anchor it to the tail when the user is at the
		// bottom, so the newest content stays visible (not clipped off the bottom).
		if rr := m.renderRunningRegions(); rr != "" {
			atBottom := m.vp.AtBottom()
			if h := vp.Height - lipgloss.Height(rr); h > 1 {
				vp.Height = h
			}
			if atBottom {
				vp.GotoBottom()
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

// renderInput renders the bordered input region. The input spans the full width;
// the filterable slash-command menu (slash_menu.go) floats just above it.
func (m Model) renderInput() string {
	box := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		Width(m.width - 2)
	return box.Render(m.input.View())
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

// renderRunningRegions renders the C8 active-run panels shown between the transcript
// and the thinking line while a run is in flight: a task header (original request +
// current in-flight activity), the pinned latest assistant summary, and a compact
// activity log (the most recent few tool/step entries). Returns "" when idle so the
// idle layout (and its golden) is unchanged. Height is deterministic (line count),
// so the viewport math in renderTranscriptRegion can subtract it exactly.
func (m Model) renderRunningRegions() string {
	if !m.running {
		return ""
	}
	w := m.width
	var lines []string

	// Task header: "Task: <request>" + a dim " · <current activity>" when known.
	if t := strings.TrimSpace(m.activeTask); t != "" {
		head := muted.Render("Task: ") + t
		if a := strings.TrimSpace(m.activity); a != "" {
			head += muted.Render("  · " + a)
		}
		lines = append(lines, truncate(head, w))
	}

	// Latest assistant summary (amber label per the mock), pinned until run end.
	if s := strings.TrimSpace(m.latestSummary); s != "" {
		lines = append(lines, truncate(warning.Render("Latest: ")+s, w))
	}

	// Compact activity log: the most recent entries (oldest of the window first).
	if n := len(m.activityLog); n > 0 {
		start := max(n-activityLogVisible, 0)
		for _, e := range m.activityLog[start:] {
			lines = append(lines, truncate(e.styled(), w))
		}
	}

	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

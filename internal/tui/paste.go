package tui

import (
	"fmt"
	"strings"
)

// pasteInlineMax: pastes longer than this many bytes (or with any newline) collapse
// to a short placeholder in the input instead of flooding the line; the full text is
// kept and expanded on submit. Short single-line pastes insert normally.
const pasteInlineMax = 200

// pastedText pairs the short placeholder shown in the input with the full pasted
// text sent to the model on submit.
type pastedText struct{ placeholder, full string }

// handlePaste collapses a long/multi-line bracketed paste into a placeholder (like
// "[#1 pasted 320 lines, 9.1k chars]") appended to the input, stashing the full text.
// Returns (model, handled): handled=false means it's a short paste the input should
// insert as-is. Mirrors how Claude Code / Codex show "[Pasted text …]".
func (m Model) handlePaste(text string) (Model, bool) {
	if len(text) <= pasteInlineMax && !strings.Contains(text, "\n") {
		return m, false // short single-line paste → let the input insert it
	}
	lines := strings.Count(text, "\n") + 1
	ph := fmt.Sprintf("[#%d pasted %d lines, %s chars]", len(m.pastes)+1, lines, human(len(text)))
	m.pastes = append(m.pastes, pastedText{placeholder: ph, full: text})
	m.input.SetValue(m.input.Value() + ph)
	return m, true
}

// expandPastes replaces each paste placeholder in line with its full text (what the
// model receives); the transcript keeps the short placeholder.
func (m Model) expandPastes(line string) string {
	for _, p := range m.pastes {
		line = strings.ReplaceAll(line, p.placeholder, p.full)
	}
	return line
}
